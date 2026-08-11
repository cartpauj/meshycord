#include "discord.h"
#include "settings.h"
#include "root_ca.h"
#include "util.h"

#include <WiFiClientSecure.h>
#include <HTTPClient.h>
#include <ArduinoJson.h>

static const char* API_HOST = "discord.com";
static const char* API_BASE = "/api/v10";

// When a session is open, every request rides the same TLS connection.
static WiFiClientSecure* g_session = nullptr;

void discord_session_begin() {
  if (g_session) return;
  g_session = new WiFiClientSecure();
  g_session->setCACert(DISCORD_ROOT_CAS);
  g_session->setTimeout(15);
  g_session->setHandshakeTimeout(20);
}

void discord_session_end() {
  if (!g_session) return;
  g_session->stop();
  delete g_session;
  g_session = nullptr;
}

static bool g_auth_failed = false;
bool discord_auth_failed() { return g_auth_failed; }

static int http_request(const char* method, const String& path,
                        const String& body, String* response_out) {
  const bool pooled = (g_session != nullptr);
  if (!pooled) heap_log("discord req enter");

  WiFiClientSecure  own;
  WiFiClientSecure& client = pooled ? *g_session : own;
  if (!pooled) {
    client.setCACert(DISCORD_ROOT_CAS);   // real validation, not setInsecure()
    client.setTimeout(15);
    client.setHandshakeTimeout(20);
  }

  HTTPClient http;
  String url = String("https://") + API_HOST + path;
  if (!http.begin(client, url)) {
    Serial.println("[discord] http.begin failed");
    return -1000;
  }
  http.addHeader("Authorization", String("Bot ") + g_settings.bot_token);
  http.addHeader("Content-Type", "application/json");
  http.addHeader("User-Agent", "MeshyCord (esp32c3, 1.0)");
  http.setTimeout(15000);
  http.setReuse(pooled);      // keep the socket open for the rest of the sweep

  int code;
  if (strcmp(method, "GET") == 0)      code = http.GET();
  else if (strcmp(method, "POST") == 0) code = http.POST(body);
  else                                  code = http.sendRequest(method, body);

  if (code > 0 && response_out) *response_out = http.getString();
  else if (code > 0)            http.getString();   // drain

  if (code == 429) {
    // Honour the rate limit instead of hammering: the body carries retry_after
    // in seconds. Ignoring it earns longer bans.
    float retry = 1.0f;
    if (response_out) {
      JsonDocument d;
      if (!deserializeJson(d, *response_out)) retry = d["retry_after"] | 1.0f;
    }
    if (retry > 30) retry = 30;
    Serial.printf("[discord] 429 rate limited, waiting %.1fs\n", retry);
    uint32_t until = millis() + (uint32_t)(retry * 1000);
    while ((int32_t)(until - millis()) > 0) { watchdog_feed(); delay(50); }
  } else if (code == 401 || code == 403) {
    // Bad or revoked token, or missing permissions. Latch it so the web UI can
    // say so rather than everything failing silently.
    if (!g_auth_failed)
      Serial.println("[discord] AUTH FAILED (401/403) - check the bot token and "
                     "that the bot is still in the server");
    g_auth_failed = true;
  } else if (code < 200 || code >= 300) {
    Serial.printf("[discord] %s %s -> %d\n", method, path.c_str(), code);
    if (code > 0 && response_out)
      Serial.printf("[discord] body: %s\n", response_out->c_str());
    else if (code < 0)
      Serial.printf("[discord] transport: %s\n",
                    http.errorToString(code).c_str());
  }

  if (code >= 200 && code < 300) g_auth_failed = false;

  http.end();
  if (!pooled) {
    client.stop();
    heap_log("discord req exit");
  } else if (code < 0) {
    // Transport error on the shared connection: drop it so the next request
    // reconnects instead of every remaining request in the sweep failing too.
    Serial.println("[discord] pooled connection failed, resetting session");
    discord_session_end();
    discord_session_begin();
  }
  return code;
}

String discord_send_id(const String& channel_id, const String& content) {
  if (channel_id.length() == 0) return "";
  String body = String("{\"content\":\"") + json_escape(content) + "\"}";
  String resp;
  int code = http_request("POST", String(API_BASE) + "/channels/" +
                          channel_id + "/messages", body, &resp);
  if (code < 200 || code >= 300) return "";
  JsonDocument doc;
  if (deserializeJson(doc, resp)) return "";
  return doc["id"].as<String>();
}

bool discord_send(const String& channel_id, const String& content) {
  return discord_send_id(channel_id, content).length() > 0;
}

bool discord_poll(const String& channel_id, const String& after_id,
                  DiscordMessage* out, size_t max_out, size_t& n_out,
                  String& newest_seen_out, bool* channel_gone_out) {
  n_out = 0;
  if (channel_gone_out) *channel_gone_out = false;
  if (channel_id.length() == 0) return false;

  String path = String(API_BASE) + "/channels/" + channel_id + "/messages?limit=";
  path += String((int)(max_out > 20 ? 20 : max_out));
  if (after_id.length()) { path += "&after="; path += after_id; }

  String resp;
  int code = http_request("GET", path, "", &resp);
  if (code == 404) {
    if (channel_gone_out) *channel_gone_out = true;
    return false;
  }
  if (code < 200 || code >= 300) return false;

  // Discord returns newest-first. Filter, then reverse into `out`.
  JsonDocument doc;
  DeserializationError err = deserializeJson(doc, resp);
  if (err) {
    Serial.printf("[discord] json parse: %s\n", err.c_str());
    return false;
  }
  JsonArray arr = doc.as<JsonArray>();
  if (arr.isNull()) return false;

  DiscordMessage tmp[20];
  size_t n = 0;
  bool first = true;
  for (JsonObject m : arr) {
    // Discord returns newest-first, so the first element is the newest id in
    // this window. Record it before any filtering so the cursor always moves.
    if (first) { newest_seen_out = m["id"].as<String>(); first = false; }
    if (n >= max_out || n >= 20) break;
    JsonObject author = m["author"];
    bool is_bot = author["bot"] | false;
    // Echo-loop guard: never surface our own posts (or any bot/webhook) as
    // something to send to the mesh. Getting this wrong floods the mesh.
    if (is_bot) continue;
    if (!m["webhook_id"].isNull()) continue;

    tmp[n].id          = m["id"].as<String>();
    tmp[n].content     = m["content"].as<String>();
    tmp[n].author_id   = author["id"].as<String>();
    tmp[n].author_name = author["username"].as<String>();
    tmp[n].is_bot      = is_bot;
    n++;
  }
  // reverse to oldest-first so the cursor advances monotonically
  for (size_t i = 0; i < n; i++) out[i] = tmp[n - 1 - i];
  n_out = n;
  return true;
}

String discord_sanitize_name(const String& raw, const String& fallback) {
  String s;
  bool last_dash = false;

  for (size_t i = 0; i < raw.length() && s.length() < 90; i++) {
    uint8_t c = (uint8_t)raw[i];

    if (c >= 0x80) {
      s += (char)c;                 // UTF-8 byte: pass through untouched so
      last_dash = false;            // emoji and accented letters survive
      continue;
    }
    if (c >= 'A' && c <= 'Z') { s += (char)(c - 'A' + 'a'); last_dash = false; continue; }
    if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) { s += (char)c; last_dash = false; continue; }
    // Anything else (space, punctuation, control) becomes a single hyphen.
    if (!last_dash && s.length() > 0) { s += '-'; last_dash = true; }
  }

  // Never end mid-character, and never leave a trailing hyphen.
  s = utf8_truncate(s, 90);
  while (s.length() && s[s.length() - 1] == '-') s.remove(s.length() - 1);

  if (s.length() == 0) {
    if (fallback.length()) return discord_sanitize_name(fallback, "");
    return "mesh";
  }
  return s;
}

String discord_create_channel(const String& name, const String& topic,
                              const String& parent_id,
                              const String& name_fallback) {
  if (g_settings.guild_id.length() == 0) return "";

  String clean = discord_sanitize_name(name, name_fallback);
  String body = String("{\"name\":\"") + json_escape(clean) +
                "\",\"type\":0";
  if (topic.length()) {
    body += ",\"topic\":\"";
    body += json_escape(topic);
    body += "\"";
  }
  if (parent_id.length()) {
    body += ",\"parent_id\":\"";
    body += parent_id;
    body += "\"";
  }
  body += "}";

  String resp;
  int code = http_request("POST", String(API_BASE) + "/guilds/" +
                          g_settings.guild_id + "/channels", body, &resp);
  if (code < 200 || code >= 300) return "";

  JsonDocument doc;
  if (deserializeJson(doc, resp)) return "";
  String id = doc["id"].as<String>();
  Serial.printf("[discord] created #%s -> %s\n", clean.c_str(), id.c_str());
  return id;
}

String discord_find_channel(const String& name) {
  if (g_settings.guild_id.length() == 0) return "";
  String want = discord_sanitize_name(name);

  String resp;
  int code = http_request("GET", String(API_BASE) + "/guilds/" +
                          g_settings.guild_id + "/channels", "", &resp);
  if (code < 200 || code >= 300) return "";

  JsonDocument doc;
  if (deserializeJson(doc, resp)) return "";
  for (JsonObject ch : doc.as<JsonArray>()) {
    if ((int)(ch["type"] | -1) != 0) continue;          // text channels only
    if (want == ch["name"].as<String>()) return ch["id"].as<String>();
  }
  return "";
}

String discord_find_or_create_channel(const String& name, const String& topic) {
  String id = discord_find_channel(name);
  if (id.length()) return id;
  return discord_create_channel(name, topic);
}

String discord_find_or_create_category(const String& name) {
  if (g_settings.guild_id.length() == 0) return "";

  String resp;
  int code = http_request("GET", String(API_BASE) + "/guilds/" +
                          g_settings.guild_id + "/channels", "", &resp);
  if (code >= 200 && code < 300) {
    JsonDocument doc;
    if (!deserializeJson(doc, resp)) {
      for (JsonObject ch : doc.as<JsonArray>()) {
        if ((int)(ch["type"] | -1) != 4) continue;      // 4 == category
        if (ch["name"].as<String>() == name) return ch["id"].as<String>();
      }
    }
  }

  // Categories keep their given name verbatim — no slug sanitising.
  String body = String("{\"name\":\"") + json_escape(name) + "\",\"type\":4}";
  String cresp;
  code = http_request("POST", String(API_BASE) + "/guilds/" +
                      g_settings.guild_id + "/channels", body, &cresp);
  if (code < 200 || code >= 300) return "";
  JsonDocument doc2;
  if (deserializeJson(doc2, cresp)) return "";
  String id = doc2["id"].as<String>();
  Serial.printf("[discord] created category '%s' -> %s\n", name.c_str(), id.c_str());
  return id;
}

bool discord_move_channel(const String& channel_id, const String& parent_id) {
  if (channel_id.length() == 0 || parent_id.length() == 0) return false;
  String body = String("{\"parent_id\":\"") + parent_id + "\"}";
  String resp;
  int code = http_request("PATCH", String(API_BASE) + "/channels/" + channel_id,
                          body, &resp);
  return code >= 200 && code < 300;
}

bool discord_delete_channel(const String& channel_id) {
  if (channel_id.length() == 0) return false;
  String resp;
  int code = http_request("DELETE", String(API_BASE) + "/channels/" + channel_id,
                          "", &resp);
  return (code >= 200 && code < 300) || code == 404;   // already gone is fine
}

bool discord_channel_exists(const String& channel_id) {
  if (channel_id.length() == 0) return false;
  String resp;
  int code = http_request("GET", String(API_BASE) + "/channels/" + channel_id,
                          "", &resp);
  return code >= 200 && code < 300;
}

bool discord_react(const String& channel_id, const String& message_id,
                   const String& emoji) {
  if (channel_id.length() == 0 || message_id.length() == 0) return false;
  // The emoji must be percent-encoded in the path.
  String enc;
  for (size_t i = 0; i < emoji.length(); i++) {
    uint8_t c = (uint8_t)emoji[i];
    char buf[4];
    snprintf(buf, sizeof(buf), "%%%02X", c);
    enc += buf;
  }
  String resp;
  int code = http_request("PUT", String(API_BASE) + "/channels/" + channel_id +
                          "/messages/" + message_id + "/reactions/" + enc + "/@me",
                          "", &resp);
  return code >= 200 && code < 300;
}
