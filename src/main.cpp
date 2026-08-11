// MeshCore <-> Discord bridge, ESP32-C3
//
//   Heltec V3 (MeshCore companion)  <--BLE-->  ESP32-C3  <--WiFi-->  Discord
//
// Mesh -> Discord : PUSH_CODE_MSG_WAITING -> CMD_SYNC_NEXT_MESSAGE -> route
//                   to a Discord channel (inbox for strangers).
// Discord -> Mesh : poll each mapped channel -> CMD_SEND_TXT_MSG /
//                   CMD_SEND_CHANNEL_TXT_MSG, chunked to 133 bytes.
//
// Settings live in NVS and are edited from a web page; nothing secret is
// compiled in. First boot (or BOOT held) raises a "meshycord-setup" access point.
//
// Target: ESP32-C3, single core, 400KB SRAM, NO PSRAM. Heap is the binding
// constraint, so: NimBLE (not Bluedroid), sync WebServer (not Async), and TLS
// sessions are opened per-request and closed rather than held open.

#include <Arduino.h>
#include <WiFi.h>

#include "settings.h"
#include "routing.h"
#include "meshcore.h"
#include "discord.h"
#include "webui.h"
#include "admin.h"
#include "util.h"

#ifndef PIN_BOOT_BUTTON
#define PIN_BOOT_BUTTON 9        // GPIO9 is BOOT on most C3 boards
#endif

enum Mode { MODE_PROVISION, MODE_RUN };
static Mode g_mode = MODE_RUN;

static uint32_t g_ble_backoff = 2000;

// ---------------------------------------------------------------------------
// WiFi
// ---------------------------------------------------------------------------
static bool wifi_connect(uint32_t timeout_ms = 25000) {
  if (g_settings.wifi_ssid.length() == 0) return false;
  WiFi.mode(WIFI_STA);
  WiFi.setSleep(true);              // eases BLE/WiFi coexistence on one radio
  WiFi.begin(g_settings.wifi_ssid.c_str(), g_settings.wifi_pass.c_str());
  Serial.printf("[wifi] joining %s", g_settings.wifi_ssid.c_str());
  uint32_t deadline = millis() + timeout_ms;
  while (WiFi.status() != WL_CONNECTED && millis() < deadline) {
    watchdog_feed();
    webui_loop();          // keep the settings page reachable while connecting
    delay(400);
    Serial.print(".");
  }
  Serial.println();
  if (WiFi.status() == WL_CONNECTED) {
    Serial.printf("[wifi] ok ip=%s rssi=%d\n",
                  WiFi.localIP().toString().c_str(), WiFi.RSSI());
    return true;
  }
  Serial.println("[wifi] failed");
  return false;
}

// Marks a Discord channel as actively-used so it is polled every few seconds
// instead of on the slow interval. Defined with the polling code below.
void mark_channel_hot(Route* r);

// ---------------------------------------------------------------------------
// Routing helpers
// ---------------------------------------------------------------------------

// Where should this incoming mesh message go?
// Channels and room servers auto-create (you opted into those). A DM from a
// person goes to the inbox until you reply to it — that is what prevents a
// stranger from creating Discord channels at will.
static String destination_for(const MeshMessage& m, String& label_out) {
  if (m.is_channel) {
    String key = String((int)m.channel_idx);
    Route* r = route_find(ROUTE_CHANNEL, key);
    if (r) { label_out = r->label; return r->channel_id; }
    if (!g_settings.autocreate_channels) return g_settings.inbox_channel;

    // Ask the node for the real channel name ("Public" for slot 0) rather than
    // naming it after the index.
    String cname = mesh_channel_name(m.channel_idx);
    // No prefix: the "Channels" category already conveys that.
    String name  = cname.length() ? cname : ("channel-" + key);
    String id = admin_create_channel(ROUTE_CHANNEL, name,
                                     "MeshCore channel " + key +
                                     (cname.length() ? " (" + cname + ")" : ""));
    if (id.length()) {
      route_put(ROUTE_CHANNEL, key, id, cname.length() ? cname : name);
      label_out = cname.length() ? cname : name;
      return id;
    }
    return g_settings.inbox_channel;
  }

  // Contact message: person or room server?
  String prefix = String(m.pubkey_prefix);
  Route* r = route_find(ROUTE_DM, prefix);
  if (!r) r = route_find(ROUTE_ROOM, prefix);
  if (r) {
    // Refresh the stored label: it was captured when the route was created and
    // goes stale if the contact renames itself.
    MeshContact rc;
    if (mesh_lookup_contact(prefix, rc) && rc.name.length() && rc.name != r->label) {
      r->label = rc.name;
      routes_save();
    }
    label_out = r->label;
    return r->channel_id;
  }

  MeshContact c;
  bool known = mesh_lookup_contact(prefix, c);
  if (known) label_out = c.name;

  if (known && c.type == ADV_TYPE_ROOM && g_settings.autocreate_rooms) {
    String name = c.name.length() ? c.name : prefix;   // no prefix; the
                                                       // category says it
    String id = admin_create_channel(ROUTE_ROOM, name, "MeshCore room server",
                                     "node-" + prefix.substring(0, 6));
    if (id.length()) {
      route_put(ROUTE_ROOM, prefix, id, c.name);
      return id;
    }
  }

  // A known person, if that policy is enabled. Deliberately requires the sender
  // to be in the node's contact list: an unknown sender cannot be classified or
  // named, and letting strangers create channels is the abuse vector this whole
  // policy exists to avoid.
  if (known && c.type == ADV_TYPE_CHAT && g_settings.autocreate_dms) {
    String name = c.name.length() ? c.name : prefix;
    String id = admin_create_channel(ROUTE_DM, name, "MeshCore DM " + prefix,
                                     "node-" + prefix.substring(0, 6));
    if (id.length()) {
      route_put(ROUTE_DM, prefix, id, c.name);
      return id;
    }
  }

  // Otherwise the inbox: an unknown sender, or a person with auto-create off.
  return g_settings.inbox_channel;
}

// Resolve the author of a room-server post, if the message carried one.
static String author_name(const MeshMessage& m) {
  if (m.author_prefix[0] == 0) return "";
  MeshContact c;
  if (mesh_lookup_contact(String(m.author_prefix), c) && c.name.length())
    return c.name;
  return String(m.author_prefix);   // fall back to the raw prefix
}

static String format_inbound(const MeshMessage& m, const String& label) {
  // Hops and signal, shown for every message.
  // Wording matters here. MeshCore's ROUTE_TYPE_DIRECT means "the sender used a
  // stored path", which can be many hops; it does NOT mean the sender was heard
  // first-hand. Calling that "direct" read as "nearby" and was misleading for a
  // node hundreds of miles away. Genuine adjacency is a FLOOD packet with zero
  // hops accumulated.
  String meta = "  _";
  if (!m.have_hops)      meta += "via known path";  // 0xFF: routed, hops n/a
  else if (m.hops == 0)  meta += "heard direct";    // flood, no repeaters
  else { meta += String((int)m.hops); meta += (m.hops == 1 ? " hop" : " hops"); }
  if (m.have_snr) { meta += ", snr "; meta += String(m.snr, 1); }
  meta += "_";

  if (m.is_channel) {
    // The Discord channel already says WHICH mesh channel this is, so naming it
    // again on every line is noise. What matters is who sent it — and MeshCore
    // embeds that in the text as "Name: body" (sendGroupMessage passes
    // _prefs.node_name).
    int c = m.text.indexOf(": ");
    if (c > 0 && c <= 32) {
      return "**" + m.text.substring(0, c) + "**" + meta + "\n" +
             m.text.substring(c + 2);
    }
    return m.text + meta;          // no sender embedded; just the text
  }

  // A DM or room post. The key is shown so it can be linked with `add <key>`.
  String s2 = "**";
  s2 += (label.length() ? label : String("unknown"));
  s2 += "** `";
  s2 += m.pubkey_prefix;
  s2 += "`";
  s2 += meta;
  s2 += "\n";

  // Room posts carry their original author separately from the room itself.
  String who = author_name(m);
  if (who.length()) {
    s2 += "**" + who + "**: " + m.text;
    return s2;
  }
  s2 += m.text;
  return s2;
}

// ---------------------------------------------------------------------------
// Mesh -> Discord
// ---------------------------------------------------------------------------
static void drain_mesh() {
  // Bounded so the loop still gets to service WiFi and the web UI; the waiting
  // flag stays set until the node reports NO_MORE_MESSAGES, so a large backlog
  // drains across several passes.
  for (int guard = 0; guard < 24; guard++) {
    // Each message is a Discord POST at roughly two to three seconds. A full
    // batch is over a minute, which is uncomfortably close to the 90s watchdog
    // and would have rebooted mid-drain on a slow network — leaving the backlog
    // undrained and doing it again.
    watchdog_feed();
    MeshMessage m;
    int r = mesh_next_message(m);
    if (r == 0) return;                 // no more
    if (r < 0)  return;                 // error; try again next loop

    String label;
    String dest = destination_for(m, label);
    if (admin_is_admin_channel(dest)) {
      // Would turn the command channel into a chat feed and echo commands back
      // onto the mesh. Should never happen; refuse loudly if it does.
      Serial.println("[bridge] refusing to post mesh traffic to the admin channel");
      dest = g_settings.inbox_channel;
    }
    if (dest.length() == 0) {
      Serial.println("[bridge] no destination, dropping");
      continue;
    }
    Serial.printf("[bridge] mesh->discord %s: %s\n",
                  m.is_channel ? "chan" : m.pubkey_prefix, m.text.c_str());
    discord_send(dest, format_inbound(m, label));

    // You are most likely to reply to what just came in, so make that channel
    // fast-polled for the next few minutes.
    for (size_t i = 0; i < routes_count(); i++) {
      Route* rr = routes_at(i);
      if (rr && rr->channel_id == dest) { mark_channel_hot(rr); break; }
    }
  }
}

// ---------------------------------------------------------------------------
// Delivery receipts
//
// A DM send returns an expected-ACK handle; the node later pushes
// PUSH_CODE_SEND_CONFIRMED with that handle and the round trip time. We hold the
// Discord message id alongside it so the right message gets the tick.
//
// Group/channel sends cannot be acknowledged at all — MeshCore returns a plain
// OK with no handle — so those get a "transmitted" marker instead of a tick that
// would imply delivery it cannot prove.
// ---------------------------------------------------------------------------
struct Pending {
  uint32_t ack;
  uint32_t deadline;
  String   channel_id;
  String   message_id;
};
// A message is refused above this many transmissions. Kept low deliberately:
// each one is airtime everybody on the channel pays for, and a wall of chunks
// is unpleasant to read on a handheld.
static const size_t MAX_CHUNKS   = 3;
static const uint32_t CHUNK_GAP_MS = 2000; // airtime courtesy between TXs
static const size_t MAX_PENDING = 8;
static Pending g_pending[MAX_PENDING];
static size_t  g_pending_n = 0;

static const char* EMOJI_OK    = "\xE2\x9C\x85";   // white_check_mark
static const char* EMOJI_FAIL  = "\xE2\x9D\x8C";   // x
static const char* EMOJI_SENT  = "\xF0\x9F\x93\xA1"; // satellite antenna
// Shown while a resend is in flight. A direct message is not ticked until the
// node confirms delivery, which can be up to two minutes, so something has to
// say "working on it" in the meantime.
static const char* EMOJI_RETRY = "\xF0\x9F\x94\x84"; // arrows_counterclockwise

// Messages currently showing the in-progress marker. Tracked rather than always
// attempting a clear, because clearing is a REST call and adding one to every
// normal send would cost two to three seconds a message for nothing.
static const size_t MAX_RETRY_MARKS = 4;
static String g_retry_marks[MAX_RETRY_MARKS];

static void retry_mark_add(const String& id) {
  for (size_t i = 0; i < MAX_RETRY_MARKS; i++)
    if (g_retry_marks[i].length() == 0) { g_retry_marks[i] = id; return; }
  g_retry_marks[0] = id;         // full: overwrite the oldest slot
}

static bool retry_mark_take(const String& id) {
  for (size_t i = 0; i < MAX_RETRY_MARKS; i++) {
    if (g_retry_marks[i] != id) continue;
    g_retry_marks[i] = (const char*)nullptr;
    return true;
  }
  return false;
}

// Apply a final verdict, taking down the in-progress marker first if this
// message was a resend.
static void react_verdict(const String& ch, const String& msg,
                          const char* emoji) {
  if (retry_mark_take(msg)) discord_unreact(ch, msg, EMOJI_RETRY);
  discord_react(ch, msg, emoji);
}

static void pending_add(uint32_t ack, uint32_t timeout_ms,
                        const String& ch, const String& msg) {
  if (ack == 0 || g_pending_n >= MAX_PENDING) return;
  if (timeout_ms < 5000)  timeout_ms = 5000;
  if (timeout_ms > 120000) timeout_ms = 120000;
  g_pending[g_pending_n++] = { ack, millis() + timeout_ms, ch, msg };
}

// Match confirmations to pending sends, and time out the rest.
static void pending_service() {
  uint32_t ack, trip;
  while (mesh_take_confirmation(&ack, &trip)) {
    for (size_t i = 0; i < g_pending_n; i++) {
      if (g_pending[i].ack != ack) continue;
      String label = String(EMOJI_OK);
      react_verdict(g_pending[i].channel_id, g_pending[i].message_id, EMOJI_OK);
      Serial.printf("[ack] delivered in %ums\n", trip);
      for (size_t j = i; j + 1 < g_pending_n; j++) g_pending[j] = g_pending[j + 1];
      g_pending_n--;
      break;
    }
  }
  uint32_t now = millis();
  for (size_t i = 0; i < g_pending_n; ) {
    if ((int32_t)(g_pending[i].deadline - now) <= 0) {
      Serial.println("[ack] no confirmation before timeout");
      react_verdict(g_pending[i].channel_id, g_pending[i].message_id, EMOJI_FAIL);
      for (size_t j = i; j + 1 < g_pending_n; j++) g_pending[j] = g_pending[j + 1];
      g_pending_n--;
    } else i++;
  }
}

// ---------------------------------------------------------------------------
// Room server sessions
//
// A room server refuses posts from anyone who has not logged in, and the
// session does not last forever. The companion protocol does not pass the
// server's keep-alive interval on to us, so rather than guessing an expiry we
// re-login whenever the BLE link comes back and whenever a post to that room
// goes unacknowledged — the two things that actually indicate a dead session.
// ---------------------------------------------------------------------------
// One per linked room at most, so it can never be the thing that runs out:
// a smaller cap would silently report the extra rooms as permanently logged
// out. A prefix is 12 characters, so it lives inline in the String.
static const size_t MAX_SESSIONS = MAX_ROUTES;
struct RoomSession {
  String   prefix;
  bool     logged_in;
  uint32_t last_attempt;
};
static RoomSession g_sessions[MAX_SESSIONS];
static size_t      g_sessions_n = 0;

static RoomSession* session_for(const String& prefix, bool create) {
  for (size_t i = 0; i < g_sessions_n; i++)
    if (g_sessions[i].prefix == prefix) return &g_sessions[i];
  if (!create || g_sessions_n >= MAX_SESSIONS) return nullptr;
  g_sessions[g_sessions_n] = { prefix, false, 0 };
  return &g_sessions[g_sessions_n++];
}

bool room_is_logged_in(const String& prefix) {
  RoomSession* s = session_for(prefix, false);
  return s && s->logged_in;
}

// Try to log in, if we have a password and are not hammering the room.
static bool room_try_login(const String& prefix) {
  String pw = room_password_get(prefix);
  if (pw.length() == 0) return false;
  RoomSession* s = session_for(prefix, true);
  if (!s) return false;
  // Logging in costs airtime and the reply takes seconds to come back. Do not
  // retry faster than that or a busy channel turns into a login storm.
  if (s->last_attempt && millis() - s->last_attempt < 30000) return false;
  s->last_attempt = millis();
  Serial.printf("[room] logging in to %s\n", prefix.c_str());
  return mesh_room_login(prefix, pw);
}

// Log in to every room we hold a password for. Called once the mesh link is up.
static void rooms_login_all() {
  for (size_t i = 0; i < routes_count(); i++) {
    Route* r = routes_at(i);
    if (!r || r->kind != ROUTE_ROOM) continue;
    RoomSession* s = session_for(r->key, true);
    if (s) { s->logged_in = false; s->last_attempt = 0; }   // link is new
    if (!room_password_known(r->key)) continue;
    // Each login is a command with a 5 second timeout, so a handful of rooms
    // can add up to a meaningful fraction of the 90s watchdog.
    watchdog_feed();
    room_try_login(r->key);
    delay(300);                       // stagger them; this is airtime
  }
}

// Drain login verdicts and say what happened, in the room's own channel.
static void rooms_service() {
  String prefix;
  bool ok = false;
  while (mesh_take_login_result(prefix, &ok)) {
    RoomSession* s = session_for(prefix, true);
    if (s) s->logged_in = ok;
    Serial.printf("[room] login %s: %s\n", prefix.c_str(), ok ? "OK" : "FAILED");

    Route* r = route_find(ROUTE_ROOM, prefix);
    if (!r) continue;
    if (ok) {
      discord_send(r->channel_id,
        "Logged in to this room server. Anything you missed should arrive "
        "shortly, over the air.");
    } else {
      discord_send(r->channel_id,
        "**The room server rejected that password.**\n"
        "Paste this into <#" + g_settings.admin_channel + ">, with the right "
        "password on the end:\n"
        "```\nlogin " + prefix + " \n```"
        "Your message is deleted the moment it is read.");
    }
  }
}

// ---------------------------------------------------------------------------
// Discord -> Mesh
// ---------------------------------------------------------------------------

// Pull a 12-hex-char pubkey prefix out of a message we posted earlier, so a
// reply in the inbox can be routed without extra stored state.
static bool extract_prefix(const String& s, String& out) {
  int a = s.indexOf('`');
  if (a < 0) return false;
  int b = s.indexOf('`', a + 1);
  if (b < 0) return false;
  String cand = s.substring(a + 1, b);
  if (cand.length() != 12) return false;
  for (size_t i = 0; i < cand.length(); i++) {
    char c = cand[i];
    bool hex = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
    if (!hex) return false;
  }
  out = cand;
  return true;
}

// Send text to the mesh, splitting it if needed.
//
// A message longer than 133 bytes becomes several transmissions. Rather than
// hiding that, each chunk is echoed back into Discord as its own message and
// tracked separately, so you can see exactly how much airtime you used and
// which transmissions actually landed. Silently truncating (the old behaviour
// past 6 chunks) was the worst option: it looked like it sent.
// Message prefixes that override how the node routes a message.
//
//   path:flood <text>    forget the stored path first, so this floods
//   path:direct <text>   use the stored path (the default; here for symmetry)
//
// The prefix and every space or tab after it are removed before anything else,
// so they cost none of the 133 byte transmission budget and do not count toward
// the length limit. The recipient sees only the text.
//
// There is no per-message route flag in the companion protocol. The node picks
// flood when a contact has no stored path and direct otherwise
// (BaseChatMesh.cpp:449), so path:flood works by clearing the path. It is
// relearned from the reply, so the effect is for this message rather than
// permanent.
enum RouteWish { ROUTE_AUTO, ROUTE_FORCE_FLOOD, ROUTE_FORCE_DIRECT };

static RouteWish take_route_prefix(String& text) {
  struct { const char* tag; RouteWish wish; } forms[] = {
    { "path:flood",  ROUTE_FORCE_FLOOD  },
    { "path:direct", ROUTE_FORCE_DIRECT },
  };
  String l = text; l.toLowerCase();

  for (auto& f : forms) {
    size_t n = strlen(f.tag);
    if (!l.startsWith(f.tag)) continue;
    // Must be the whole message or followed by whitespace, so a message that
    // merely begins with these letters is not swallowed.
    if (text.length() > n && text[n] != ' ' && text[n] != '\t') continue;

    size_t i = n;
    while (i < text.length() && (text[i] == ' ' || text[i] == '\t')) i++;
    text = text.substring(i);
    return f.wish;
  }
  return ROUTE_AUTO;
}

static void send_to_mesh_chunked(bool is_channel, uint8_t chan_idx,
                                 const String& prefix, const String& text_in,
                                 const String& react_channel = "",
                                 const String& react_message = "") {
  String text = text_in;
  RouteWish wish = take_route_prefix(text);

  const bool ui = react_channel.length() && react_message.length();

  if (wish != ROUTE_AUTO && is_channel) {
    // Group messages are not addressed to a contact, so there is no stored path
    // to clear or follow.
    if (ui) discord_send(react_channel,
      "`path:` prefixes only apply to direct messages and room servers. "
      "Channel messages are always flooded.");
  } else if (wish == ROUTE_FORCE_FLOOD) {
    if (mesh_reset_path(prefix)) {
      Serial.println("[bridge] path cleared, forcing flood");
    } else if (ui) {
      discord_send(react_channel,
        "Could not clear the stored path, sending normally. "
        "That needs the sender to be in the node's contact list.");
    }
  }
  if (text.length() == 0) {
    if (ui) react_verdict(react_channel, react_message, EMOJI_FAIL);
    return;
  }
  String chunks[MAX_CHUNKS];
  size_t n = chunk_text(text, MESH_MAX_MSG_LEN, chunks, MAX_CHUNKS);
  if (n == 0) return;

  const bool have_ui = ui;

  // Ask the splitter itself how many transmissions this needs. Estimating it
  // separately (text.length()/133) under-counted, because splitting reserves 8
  // bytes per chunk for the "[i/n] " prefix — so a ~390 character message passed
  // the check as 3 chunks, then got silently truncated to fit.
  size_t needed = chunk_count(text, MESH_MAX_MSG_LEN);
  if (needed > MAX_CHUNKS) {
    if (have_ui) {
      react_verdict(react_channel, react_message, EMOJI_FAIL);
      discord_send(react_channel,
        "**Not sent — too long.** " + String((int)text.length()) +
        " characters would need " + String((int)needed) + " transmissions; "
        "the limit is " + String((int)MAX_CHUNKS) + ", which is " +
        String((int)chunk_capacity(MESH_MAX_MSG_LEN, MAX_CHUNKS)) +
        " characters of plain text (fewer with emoji). "
        "Please send something shorter.");
    }
    return;
  }

  // Single transmission: the common case, no extra noise.
  if (n == 1) {
    uint32_t ack = 0, est = 0;
    bool flooded = false;
    bool ok = is_channel ? mesh_send_channel(chan_idx, chunks[0])
                         : mesh_send_dm(prefix, chunks[0], &ack, &est, &flooded);
    Serial.printf("[bridge] discord->mesh 1/1 %s (%s)\n", ok ? "ok" : "FAILED",
                  is_channel ? "channel" : (flooded ? "flood" : "direct"));
    if (ok && !is_channel && wish != ROUTE_AUTO && ui)
      discord_send(react_channel, flooded ? "Sent by flood."
                                          : "Sent via the stored path.");
    if (!have_ui) return;
    if (!ok)                 react_verdict(react_channel, react_message, EMOJI_FAIL);
    else if (is_channel)     react_verdict(react_channel, react_message, EMOJI_SENT);
    else if (ack)            pending_add(ack, est, react_channel, react_message);
    else                     react_verdict(react_channel, react_message, EMOJI_SENT);
    return;
  }

  // Split. Announce it, then post and track each transmission separately.
  if (have_ui) {
    discord_send(react_channel,
      "**Splitting into " + String((int)n) + " transmissions** (" +
      String((int)text.length()) + " characters, " +
      String(MESH_MAX_MSG_LEN) + " per transmission, ~" +
      String(CHUNK_GAP_MS / 1000.0f, 1) + "s apart)");
  }

  for (size_t i = 0; i < n; i++) {
    String echo_id;
    if (have_ui) echo_id = discord_send_id(react_channel, chunks[i]);

    uint32_t ack = 0, est = 0;
    bool ok = is_channel ? mesh_send_channel(chan_idx, chunks[i])
                         : mesh_send_dm(prefix, chunks[i], &ack, &est);
    Serial.printf("[bridge] discord->mesh %u/%u %s\n",
                  (unsigned)i + 1, (unsigned)n, ok ? "ok" : "FAILED");

    if (echo_id.length()) {
      if (!ok)              react_verdict(react_channel, echo_id, EMOJI_FAIL);
      else if (is_channel)  react_verdict(react_channel, echo_id, EMOJI_SENT);
      else if (ack)         pending_add(ack, est, react_channel, echo_id);
      else                  react_verdict(react_channel, echo_id, EMOJI_SENT);
    }

    if (!ok) {
      if (have_ui) react_verdict(react_channel, react_message, EMOJI_FAIL);
      return;                       // stop; do not send the rest out of order
    }
    if (i + 1 < n) delay(CHUNK_GAP_MS);   // give the mesh room to breathe
  }
}

// Strip the "[2/3] " marker the splitter puts on each transmission, so replying
// to one failed piece of a split message resends exactly that piece rather than
// the whole thing again.
static bool strip_chunk_marker(const String& in, String& out) {
  if (!in.startsWith("[")) return false;
  int close = in.indexOf("] ");
  if (close < 2 || close > 8) return false;
  int slash = in.indexOf('/');
  if (slash < 0 || slash > close) return false;
  for (int i = 1; i < close; i++) {
    if (i == slash) continue;
    if (!isdigit((unsigned char)in[i])) return false;
  }
  out = in.substring(close + 2);
  return true;
}

// "retry", sent as a REPLY to a message that failed, sends it again.
//
// A reply rather than a reaction because a reply is an ordinary message: it
// arrives on the poll that was already happening, and message_reference names
// exactly which message it refers to. Reactions cannot be polled for at all —
// they are only delivered over the Gateway — and watching for one would mean a
// request per failed message per sweep.
static void handle_retry(Route* r, const DiscordMessage& dm) {
  if (dm.ref_id.length() == 0) {
    discord_send(r->channel_id,
      "To resend something, **reply** to the message that failed and say "
      "`retry` (or `resend`). On mobile that is a swipe on the message; on "
      "desktop, hover it and pick Reply.");
    return;
  }

  String text = dm.ref_content;
  bool from_bot = dm.ref_is_bot;
  if (!dm.ref_present) {
    // Discord did not inline the original, so go and get it.
    if (!discord_get_message(r->channel_id, dm.ref_id, text, from_bot)) {
      discord_send(r->channel_id, "Could not read that message to resend it.");
      return;
    }
  }

  // A bot message is only resendable if it is one of our own transmissions;
  // anything else is a status line, and echoing that onto the mesh is nonsense.
  String chunk;
  bool is_chunk = strip_chunk_marker(text, chunk);
  if (from_bot) {
    if (!is_chunk) {
      discord_send(r->channel_id,
        "That is one of my own status messages, not something that was sent to "
        "the mesh. Reply to your own message, or to a numbered `[1/3]` "
        "transmission.");
      return;
    }
    text = chunk;             // resend just this transmission
  }

  text.trim();
  if (text.length() == 0) {
    discord_send(r->channel_id, "That message has no text to resend.");
    return;
  }

  // Clear our old verdict so the new one is not read alongside a stale cross.
  // Only our own reactions — removing anyone else's would need MANAGE_MESSAGES.
  discord_unreact(r->channel_id, dm.ref_id, EMOJI_FAIL);
  discord_unreact(r->channel_id, dm.ref_id, EMOJI_SENT);
  discord_unreact(r->channel_id, dm.ref_id, EMOJI_OK);

  // A room server drops posts from anyone without a session, exactly as it does
  // for a first attempt — resending into that would look like a retry that
  // quietly achieved nothing.
  if (r->kind == ROUTE_ROOM && !room_is_logged_in(r->key)) {
    discord_react(r->channel_id, dm.ref_id, EMOJI_FAIL);
    discord_send(r->channel_id,
      "**Not resent — not logged in to this room server.**" +
      String(room_password_known(r->key)
        ? (room_try_login(r->key)
             ? " Logging in now; reply `retry` again in a few seconds."
             : " A login is already in progress; try again in a moment.")
        : ("\nPaste this into <#" + g_settings.admin_channel +
           "> with the room's password on the end:\n```\nlogin " + r->key +
           " \n```")));
    return;
  }

  // Mark it in progress straight away. A direct message is only ticked when the
  // node confirms delivery, which can be two minutes later — so without this the
  // cross simply vanishes and nothing appears to happen for a long time. Cleared
  // by react_verdict once there is a real answer.
  retry_mark_add(dm.ref_id);
  discord_react(r->channel_id, dm.ref_id, EMOJI_RETRY);

  Serial.printf("[retry] resending %s in %s\n", dm.ref_id.c_str(),
                r->channel_id.c_str());

  // The result lands back on the ORIGINAL message, which is where you are
  // looking, rather than on the word "retry".
  if (r->kind == ROUTE_CHANNEL) {
    send_to_mesh_chunked(true, (uint8_t)r->key.toInt(), "", text,
                         r->channel_id, dm.ref_id);
  } else {
    send_to_mesh_chunked(false, 0, r->key, text, r->channel_id, dm.ref_id);
  }
}

// Handle one Discord message that a human wrote in a mapped channel.
static void handle_discord_message(Route* r, const DiscordMessage& dm) {
  if (dm.content.length() == 0) return;
  mark_channel_hot(r);        // you are mid-conversation here

  // Checked before anything is sent anywhere: these are commands, and must
  // never themselves go out over the mesh.
  {
    String t = dm.content; t.trim(); t.toLowerCase();
    if (t == "retry" || t == "resend") { handle_retry(r, dm); return; }
  }

  // Manual promotion: "!promote <prefix>"
  if (dm.content.startsWith("!promote ")) {
    String prefix = dm.content.substring(9);
    prefix.trim();
    if (prefix.length() == 12) {
      MeshContact c;
      mesh_lookup_contact(prefix, c);
      String name = c.name.length() ? c.name : prefix;
      String id = admin_create_channel(ROUTE_DM, name, "MeshCore DM " + prefix,
                                       "node-" + prefix.substring(0, 6));
      if (id.length()) {
        route_put(ROUTE_DM, prefix, id, c.name);
        discord_send(r->channel_id, "Promoted `" + prefix + "` to <#" + id + ">");
      } else {
        discord_send(r->channel_id, "Could not create a channel for `" + prefix + "`");
      }
    } else {
      discord_send(r->channel_id, "Usage: `!promote <12-hex-prefix>`");
    }
    return;
  }

  if (r->kind == ROUTE_CHANNEL) {
    send_to_mesh_chunked(true, (uint8_t)r->key.toInt(), "", dm.content,
                         r->channel_id, dm.id);
    return;
  }
  if (r->kind == ROUTE_DM || r->kind == ROUTE_ROOM) {
    // A room server drops posts from anyone without a session, and it does so
    // silently — the send itself succeeds, the post simply never appears. Say
    // so up front rather than letting it look like a delivery that worked.
    if (r->kind == ROUTE_ROOM && !room_is_logged_in(r->key)) {
      if (!room_password_known(r->key)) {
        discord_react(r->channel_id, dm.id, EMOJI_FAIL);
        // The command goes on its own line in a code block: Discord gives that
        // a copy button on desktop and makes it one tap to select on mobile.
        // Nobody should have to retype a key prefix from memory.
        discord_send(r->channel_id,
          "**Not sent — no password for this room server.**\n"
          "Room servers only accept posts from someone logged in. Paste this "
          "into <#" + g_settings.admin_channel + ">, with the room's password "
          "on the end:\n"
          "```\nlogin " + r->key + " \n```"
          "Your message is deleted the moment it is read, so the password does "
          "not stay in the channel.");
        return;
      }
      // We have a password but no live session — most likely it lapsed while
      // the bridge was away. Log in and let the user retry rather than sending
      // into the void.
      discord_react(r->channel_id, dm.id, EMOJI_FAIL);
      discord_send(r->channel_id,
        room_try_login(r->key)
          ? "**Not sent — not logged in to this room server.** Logging in now; "
            "try again in a few seconds."
          : "**Not sent — not logged in to this room server.** A login is "
            "already in progress; try again in a moment.");
      return;
    }
    send_to_mesh_chunked(false, 0, r->key, dm.content, r->channel_id, dm.id);
    return;
  }
}

// The inbox is informational. It shows traffic from senders that are not
// linked to a channel yet, so you can see what is out there — but you do not
// converse in it.
//
// Replying here was previously supported by typing the sender's hex prefix, and
// it was a bad idea: the channel aggregates many people, so an unprefixed reply
// is genuinely ambiguous and a prefixed one means copying hex by hand. To talk
// to someone, link them a channel from #meshycord-admin and talk there.
static void handle_inbox_message(const DiscordMessage& dm) {
  if (dm.content.length() == 0) return;

  // Answer at most once a minute so casual chatter is not met with a wall of
  // text every line.
  static uint32_t last_hint = 0;
  if (millis() - last_hint < 60000) return;
  last_hint = millis();

  discord_send(g_settings.inbox_channel,
      "This channel is read-only in practice — it shows mesh traffic from "
      "senders that don't have their own channel yet.\n"
      "To talk to one of them, copy the `key` shown next to their name and run "
      "`add <key>` in <#" + g_settings.admin_channel + ">. "
      "You'll get a channel for them and can reply there.");
}

// Poll one channel and dispatch whatever a human wrote in it.
static void poll_channel(const String& channel_id, String& cursor_io,
                         Route* route_or_null) {
  DiscordMessage msgs[8];
  size_t n = 0;
  String newest;
  bool gone = false;
  if (!discord_poll(channel_id, cursor_io, msgs, 8, n, newest, &gone)) {
    if (gone && route_or_null) {
      // Channel deleted: drop the route rather than retrying it every poll.
      Serial.printf("[bridge] channel %s is gone, removing route %s\n",
                    channel_id.c_str(), route_or_null->key.c_str());
      route_remove(route_or_null->kind, route_or_null->key);
    } else if (gone) {
      // Rebuild immediately. Leaving inbox_channel empty until the next reboot
      // means destination_for() returns nothing and unrouted messages are
      // silently dropped.
      Serial.println("[bridge] inbox channel deleted, recreating now");
      g_settings.inbox_channel = "";
      settings_save();
      admin_bootstrap();
    }
    return;
  }

  // Advance past filtered (bot) messages too, or the cursor sticks forever.
  if (newest.length()) cursor_io = newest;

  for (size_t i = 0; i < n; i++) {
    if (route_or_null) handle_discord_message(route_or_null, msgs[i]);
    else               handle_inbox_message(msgs[i]);
  }
}

// The admin poll cursor is deliberately NOT persisted, so after a reboot the
// bridge has no idea what it has already acted on. Discord is then asked for the
// most recent messages with no "since" marker, which is every recent command
// again — a reboot replayed the last handful of commands, and `help`, `find` and
// `contact add` all ran a second time. A watchdog reboot did it three times over.
//
// The first poll after boot therefore only ARMS the cursor: it records where the
// channel is and acts on nothing. Anything typed while the device was down is
// ignored, which is the right way round — replaying `reset confirm` or
// `sync rooms confirm` unprompted is far worse than missing a command.
static String g_admin_cursor;
static bool   g_admin_primed = false;

static void poll_admin() {
  if (g_settings.admin_channel.length() == 0) return;
  DiscordMessage msgs[6];
  size_t n = 0;
  String newest;
  bool gone = false;
  if (!discord_poll(g_settings.admin_channel, g_admin_cursor,
                    msgs, 6, n, newest, &gone)) {
    if (gone) {
      Serial.println("[admin] channel deleted, recreating now");
      g_settings.admin_channel = "";
      settings_save();
      g_admin_cursor = "";
      g_admin_primed = false;      // re-arm against the replacement channel
      admin_bootstrap();          // rebuild immediately, do not wait for a reboot
    }
    return;
  }
  if (newest.length()) g_admin_cursor = newest;
  if (!g_admin_primed) {
    g_admin_primed = true;
    if (n) Serial.printf("[admin] armed at boot; ignoring %u message(s) already "
                         "in the channel\n", (unsigned)n);
    return;
  }
  for (size_t i = 0; i < n; i++) admin_handle(msgs[i].content, msgs[i].id);
}

// A channel is "hot" for a while after mesh traffic is relayed into it, or
// after you post in it — that is when you are most likely to be typing. Hot
// channels are checked every few seconds; everything else on the slow interval.
static const uint32_t HOT_WINDOW_MS   = 5UL * 60UL * 1000UL;   // stays hot 5 min
static const uint32_t HOT_INTERVAL_MS = 5000;                  // checked every 5s

void mark_channel_hot(Route* r) {
  if (r) r->hot_until = millis() + HOT_WINDOW_MS;
}

static uint32_t g_last_slow_poll = 0;
static uint32_t g_last_fast_poll = 0;

static void poll_discord_once() {
  uint32_t now = millis();
  bool do_slow = (now - g_last_slow_poll >= g_settings.poll_interval_ms);
  bool do_fast = (now - g_last_fast_poll >= HOT_INTERVAL_MS);
  if (!do_slow && !do_fast) return;

  // One TLS handshake for the whole sweep. Without this, every channel needed
  // its own 1-2s handshake, which is why only one could be checked per cycle —
  // and with many links a typed reply could wait many minutes to be sent.
  STAGE("poll:session");
  discord_session_begin();

  if (do_slow) {
    g_last_slow_poll = now;
    STAGE("poll:admin");
    poll_admin();                       // commands
    // The inbox is informational: nothing typed there does anything except
    // trigger a hint, so it does not need checking often.
    String cursor = inbox_cursor_get();
    STAGE("poll:inbox");
    poll_channel(g_settings.inbox_channel, cursor, nullptr);
    inbox_cursor_set(cursor);
  }
  if (do_fast) g_last_fast_poll = now;

  int checked = 0, hot_checked = 0;
  for (size_t i = 0; i < routes_count(); ) {
    Route* r = routes_at(i);
    if (!r) break;
    bool hot = (int32_t)(r->hot_until - now) > 0;
    // Every channel on every slow tick; hot ones also on fast ticks.
    if (!(do_slow || (hot && do_fast))) { i++; continue; }
    watchdog_feed();
    STAGE("poll:route");

    size_t before = routes_count();
    String rc = r->last_discord_id;
    poll_channel(r->channel_id, rc, r);

    // poll_channel drops the route when Discord reports the channel deleted,
    // and that compacts the table underneath us: `r` now points at a DIFFERENT
    // route, so writing the cursor through it would corrupt that route's
    // bookmark, and i++ would skip the entry that just slid into this slot.
    if (routes_count() < before) continue;      // same index, next route

    if (rc != r->last_discord_id) {
      r->last_discord_id = rc;
      // One slot, not the whole table: this fires as often as every five
      // seconds on a hot channel.
      route_save_one(r);
    }
    checked++;
    if (hot) hot_checked++;
    i++;
  }

  discord_session_end();

  // Pooled requests are not logged individually, so report the sweep. This is
  // the number to watch as links are added: it must stay well under the poll
  // interval.
  Serial.printf("[poll] %s sweep: %d channel(s) (%d hot) in %lums, heap %u\n",
                do_slow ? "full" : "hot", checked, hot_checked,
                (unsigned long)(millis() - now), ESP.getFreeHeap());
}

// ---------------------------------------------------------------------------
void setup() {
  Serial.begin(115200);
  delay(1500);                       // native USB CDC needs a moment
  Serial.println("\n=== MeshyCord: MeshCore <-> Discord bridge (ESP32-C3) ===");
  heap_log("boot");

  pinMode(PIN_BOOT_BUTTON, INPUT_PULLUP);
  bool boot_held = (digitalRead(PIN_BOOT_BUTTON) == LOW);

  settings_load();
  routes_load();

  if (boot_held) Serial.println("[cfg] BOOT held -> provisioning mode");

  if (boot_held || !g_settings.configured()) {
    g_mode = MODE_PROVISION;
    webui_start_ap();
    return;
  }

  if (!wifi_connect()) {
    // Can't reach the network: fall back to provisioning so the user can fix
    // the credentials instead of being locked out.
    Serial.println("[cfg] WiFi unusable -> provisioning mode");
    g_mode = MODE_PROVISION;
    webui_start_ap();
    return;
  }

  g_mode = MODE_RUN;
  // 90s: long enough for a contact enumeration (up to 30s) plus a full poll
  // sweep, short enough that a hang is noticed quickly.
  watchdog_begin(90);
  webui_start_sta();

  // Discover the guild and create #meshycord-admin + #mesh-inbox before the mesh
  // comes up, so the first thing in the channel is a greeting, not a backlog.
  if (admin_bootstrap()) admin_announce_ready();

  mesh_begin();
  heap_log("setup done");
}

void loop() {
  watchdog_feed();
  heap_guard_check(20000);
  webui_loop();

  if (webui_reboot_requested()) {
    Serial.println("[cfg] rebooting to apply settings");
    delay(400);
    ESP.restart();
  }

  if (g_mode == MODE_PROVISION) {
    delay(5);
    return;
  }

  if (WiFi.status() != WL_CONNECTED) {
    static uint32_t wifi_backoff = 2000;
    Serial.println("[wifi] dropped, retrying");
    if (!wifi_connect(15000)) {
      uint32_t until = millis() + wifi_backoff;
      while ((int32_t)(until - millis()) > 0) { watchdog_feed(); delay(50); }
      wifi_backoff = min<uint32_t>(wifi_backoff * 2, 60000);
      return;
    }
    wifi_backoff = 2000;
  }

  if (!mesh_connected()) {
    if (!mesh_connect()) {
      Serial.printf("[ble] retry in %ums\n", g_ble_backoff);
      uint32_t until = millis() + g_ble_backoff;
      while (millis() < until) { watchdog_feed(); webui_loop(); delay(10); }
      g_ble_backoff = min<uint32_t>(g_ble_backoff * 2, 60000);
      return;
    }
    g_ble_backoff = 2000;
    // Channel names are only known once the mesh link is up, so the automatic
    // channel linking has to happen here rather than in setup().
    static bool synced_once = false;
    if (!synced_once) { synced_once = true; STAGE("sync_after_mesh"); admin_sync_after_mesh(); }
    // Room sessions do not survive the link going away, so re-establish them
    // on every reconnect rather than only on the first one.
    STAGE("rooms_login_all");
    rooms_login_all();
  }

  if (mesh_messages_waiting()) { STAGE("drain_mesh"); drain_mesh(); }
  STAGE("pending_service"); pending_service();
  STAGE("rooms_service");   rooms_service();

  STAGE("poll_discord");    poll_discord_once();
  STAGE("idle");

  static uint32_t last_report = 0;
  if (millis() - last_report > 60000) {
    last_report = millis();
    heap_log("idle");
  }

  delay(20);
}
