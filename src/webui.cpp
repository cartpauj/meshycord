#include "webui.h"
#include "settings.h"
#include "routing.h"
#include "admin.h"
#include "util.h"

#include <WiFi.h>
#include <WebServer.h>
#include <DNSServer.h>
#include <ESPmDNS.h>

// Sync WebServer, not ESPAsyncWebServer: it allocates per-request rather than
// holding buffers open, which matters on a C3 that also has NimBLE and a TLS
// session to fit.
static WebServer  g_http(80);
static DNSServer  g_dns;
static bool       g_ap_mode = false;
static bool       g_reboot  = false;
static bool       g_started = false;

bool webui_in_ap_mode()        { return g_ap_mode; }
bool webui_reboot_requested()  { return g_reboot; }

// HTML lives in flash, not RAM.
static const char PAGE_HEAD[] PROGMEM =
  // charset MUST come first: without it browsers assume windows-1252 and every
  // emoji or accented character in a contact name renders as mojibake.
  "<!doctype html><meta charset=utf-8>"
  "<meta name=viewport content='width=device-width,initial-scale=1'>"
  "<title>MeshyCord</title><style>"
  "body{font:16px system-ui,sans-serif;margin:0;background:#111;color:#eee}"
  "main{max-width:34rem;margin:0 auto;padding:1.5rem 1rem 4rem}"
  "h1{font-size:1.3rem;margin:.2rem 0 1.2rem}h2{font-size:1rem;margin:1.6rem 0 .5rem;color:#9ad}"
  "label{display:block;margin:.7rem 0 .2rem;font-size:.85rem;color:#bbb}"
  "input[type=text],input[type=password],input[type=number]{width:100%;box-sizing:border-box;"
  "padding:.55rem;border:1px solid #444;border-radius:6px;background:#1c1c1c;color:#eee;font:inherit}"
  "input[type=checkbox]{margin-right:.4rem}"
  ".row{display:flex;gap:.5rem}.row>*{flex:1}"
  "button{margin-top:1.4rem;padding:.7rem 1.2rem;border:0;border-radius:6px;"
  "background:#2b7;color:#022;font:inherit;font-weight:600;cursor:pointer}"
  "small{color:#888;display:block;margin-top:.2rem;font-size:.78rem}"
  ".ok{color:#7d7}.warn{color:#fc6}"
  "</style>"
  "<main>";

static const char PAGE_FOOT[] PROGMEM = "</main>";

static bool require_auth() {
  if (g_settings.ui_pass.length() == 0) return true;   // no password configured
  if (g_http.authenticate(g_settings.ui_user.c_str(), g_settings.ui_pass.c_str()))
    return true;
  g_http.requestAuthentication();
  return false;
}

static String esc(const String& s) {
  String o;
  for (size_t i = 0; i < s.length(); i++) {
    char c = s[i];
    if (c == '&') o += "&amp;";
    else if (c == '<') o += "&lt;";
    else if (c == '>') o += "&gt;";
    else if (c == '"') o += "&quot;";
    else o += c;
  }
  return o;
}

static void handle_root() {
  if (!g_ap_mode && !require_auth()) return;

  String p;
  p.reserve(4096);
  p += FPSTR(PAGE_HEAD);
  p += "<h1>MeshyCord</h1>";

  if (g_ap_mode) {
    p += "<p class=warn>Setup. You need your <b>WiFi</b>, a <b>Discord bot "
         "token</b> and your <b>server ID</b> — all channels and categories are "
         "created for you.</p>";
  } else {
    p += "<p>";
    p += "WiFi <span class=ok>" + esc(WiFi.SSID()) + "</span> &middot; ";
    p += WiFi.localIP().toString();
    p += " &middot; heap floor " + String(heap_floor());
    p += "</p>";
  }

  p += "<form method=POST action=/save>";

  p += "<h2>WiFi</h2>";
  p += "<label>SSID</label><input name=ssid value='" + esc(g_settings.wifi_ssid) + "'>";
  p += "<label>Password</label><input type=password name=wpass placeholder='";
  p += g_settings.wifi_pass.length() ? "unchanged" : "";
  p += "'><small>Leave blank to keep the current password.</small>";

  p += "<h2>Discord</h2>";
  p += "<label>Bot token</label><input type=password name=token placeholder='";
  p += g_settings.bot_token.length() ? "unchanged" : "required";
  p += "'><small>Never displayed back. Leave blank to keep the stored one.</small>";
  p += "<label>Server (guild) ID</label>"
       "<input name=guild value='" + esc(g_settings.guild_id) + "'>";
  p += "<small>Open any channel in your server; the first long number in the "
       "browser URL is the server ID. Every channel and category is created "
       "for you.</small>";
  p += "<label>Poll interval (ms)</label><input type=number name=poll min=2000 max=300000 value=";
  p += String(g_settings.poll_interval_ms);
  p += ">";
  p += "<small>How often Discord is checked for your replies. Lower means "
       "faster replies and more requests; 30000 is a good balance.</small>";

  p += "<h2>MeshCore node</h2>";
  p += "<label>BLE name contains</label><input name=blename value='" + esc(g_settings.ble_name) + "'>";
  p += "<small>Blank = first device advertising the MeshCore service.</small>";
  p += "<label>BLE address (optional)</label><input name=bleaddr value='" + esc(g_settings.ble_addr) + "'>";
  p += "<label>BLE pairing PIN (if set)</label><input name=blepin value='" + esc(g_settings.ble_pin) + "'>";

  p += "<h2>Policy</h2>";
  p += "<small>Leave these off and link things deliberately from "
       "<b>#meshycord-admin</b>. Unlinked traffic still shows up in the inbox.</small>";
  p += "<label><input type=checkbox name=acchan";
  p += g_settings.autocreate_channels ? " checked" : "";
  p += "> Auto-create channels for mesh channels</label>";
  p += "<label><input type=checkbox name=acroom";
  p += g_settings.autocreate_rooms ? " checked" : "";
  p += "> Auto-create channels for room servers</label>";
  p += "<label><input type=checkbox name=acdm";
  p += g_settings.autocreate_dms ? " checked" : "";
  p += "> Auto-create channels for direct messages</label>";
  p += "<small>Direct messages are the riskiest to automate: anyone who has "
       "heard your advert can send you one. Only known contacts trigger it; "
       "strangers always go to the inbox.</small>";

  p += "<h2>Web UI login</h2>";
  p += "<label>Username</label><input name=uiuser value='" + esc(g_settings.ui_user) + "'>";
  p += "<label>Password</label><input type=password name=uipass placeholder='";
  p += g_settings.ui_pass.length() ? "unchanged" : "STRONGLY recommended";
  p += "'><small>This page holds your bot token. Set a password.</small>";

  p += "<button type=submit>Save</button></form>";

  if (!g_ap_mode) {
    // Listing contacts, channels and links used to live here. It is all in
    // #meshycord-admin now (`list`, `find`, `add`, `remove`), which works from
    // anywhere and reads better. It also has to be: rendering ~190 contacts
    // built a ~60KB HTML string in one allocation, which is most of the free
    // heap on a C3 and could fail mid-page.
    p += "<h2>Links</h2>";
    p += "<p><b>" + String((int)routes_count()) + "</b> linked. "
         "Manage them from <b>#meshycord-admin</b>: `list links`, `list "
         "companions`, `add &lt;n&gt;`, `remove &lt;n&gt;`. `help` for the "
         "rest.</p>";
    p += "<form method=POST action=/forget><button>Clear all links</button></form>";
    p += "<h2>Discord setup</h2>";
    p += "<small>Forgets the admin channel, inbox and all links, then builds "
         "them again from scratch — exactly what a new user sees on first "
         "connect. Delete the old channels in Discord first.</small>";
    p += "<form method=POST action=/rediscover>"
         "<button>Re-run Discord setup</button></form>";
  }

  p += FPSTR(PAGE_FOOT);
  g_http.send(200, "text/html; charset=utf-8", p);
}

static void handle_save() {
  if (!g_ap_mode && !require_auth()) return;

  if (g_http.hasArg("ssid"))  g_settings.wifi_ssid = g_http.arg("ssid");
  // Blank password fields mean "keep existing" so the UI never has to echo them.
  if (g_http.arg("wpass").length()) g_settings.wifi_pass = g_http.arg("wpass");
  if (g_http.arg("token").length()) g_settings.bot_token = g_http.arg("token");
  if (g_http.hasArg("guild")) g_settings.guild_id      = g_http.arg("guild");
  if (g_http.hasArg("inbox")) g_settings.inbox_channel = g_http.arg("inbox");
  if (g_http.hasArg("poll")) {
    uint32_t v = (uint32_t)g_http.arg("poll").toInt();
    if (v >= 2000 && v <= 300000) g_settings.poll_interval_ms = v;
  }
  if (g_http.hasArg("blename")) g_settings.ble_name = g_http.arg("blename");
  if (g_http.hasArg("bleaddr")) g_settings.ble_addr = g_http.arg("bleaddr");
  if (g_http.hasArg("blepin"))  g_settings.ble_pin  = g_http.arg("blepin");

  g_settings.autocreate_channels = g_http.hasArg("acchan");
  g_settings.autocreate_rooms    = g_http.hasArg("acroom");
  g_settings.autocreate_dms      = g_http.hasArg("acdm");

  if (g_http.hasArg("uiuser") && g_http.arg("uiuser").length())
    g_settings.ui_user = g_http.arg("uiuser");
  if (g_http.arg("uipass").length()) g_settings.ui_pass = g_http.arg("uipass");

  settings_save();

  String p;
  p += FPSTR(PAGE_HEAD);
  p += "<h1>Saved</h1>";
  if (g_ap_mode && g_settings.configured()) {
    p += "<p>Rebooting to join <b>" + esc(g_settings.wifi_ssid) +
         "</b>. This access point will disappear.</p>"
         "<p>Afterwards the bridge is at <b>http://meshycord.local</b> "
         "(or find its IP on your router).</p>";
    g_reboot = true;
  } else if (g_ap_mode) {
    p += "<p class=warn>Still incomplete — WiFi SSID, bot token and server ID "
         "are all required.</p><p><a href=/>Back</a></p>";
  } else {
    p += "<p><a href=/>Back</a></p>";
  }
  p += FPSTR(PAGE_FOOT);
  g_http.send(200, "text/html; charset=utf-8", p);
}

static void handle_forget() {
  if (!require_auth()) return;
  routes_clear();
  g_http.sendHeader("Location", "/");
  g_http.send(303);
}

static void handle_rediscover() {
  if (!require_auth()) return;
  String result = admin_rediscover();
  String p;
  p += FPSTR(PAGE_HEAD);
  p += "<h1>Discord setup</h1><pre>" + esc(result) + "</pre>";
  p += "<p><a href=/>Back</a></p>";
  p += FPSTR(PAGE_FOOT);
  g_http.send(200, "text/html; charset=utf-8", p);
}

// Answer each OS's connectivity probe the way it expects, so it concludes the
// network is working and does NOT open a captive-portal webview.
//
// That webview is a cut-down browser with the clipboard disabled, which makes
// pasting a Discord bot token impossible. DNS is still hijacked, so typing any
// address reaches the settings page — we just stop the OS from forcing its own
// restricted browser on the user.
static void register_probe_handlers() {
  // Android / Chrome OS
  g_http.on("/generate_204", HTTP_GET, []() { g_http.send(204); });
  g_http.on("/gen_204",      HTTP_GET, []() { g_http.send(204); });
  // Windows
  g_http.on("/connecttest.txt", HTTP_GET, []() {
    g_http.send(200, "text/plain", "Microsoft Connect Test");
  });
  g_http.on("/ncsi.txt", HTTP_GET, []() {
    g_http.send(200, "text/plain", "Microsoft NCSI");
  });
  // Apple
  g_http.on("/hotspot-detect.html", HTTP_GET, []() {
    g_http.send(200, "text/html",
      "<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>");
  });
  g_http.on("/library/test/success.html", HTTP_GET, []() {
    g_http.send(200, "text/html",
      "<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>");
  });
  // Firefox
  g_http.on("/canonical.html", HTTP_GET, []() {
    g_http.send(200, "text/html", "<meta http-equiv=refresh content=0>");
  });
  g_http.on("/success.txt", HTTP_GET, []() { g_http.send(200, "text/plain", "success"); });
}

static void handle_notfound() {
  if (g_ap_mode) {
    // Anything else in setup mode lands on the settings page.
    g_http.sendHeader("Location", String("http://") +
                      WiFi.softAPIP().toString() + "/");
    g_http.send(302, "text/plain; charset=utf-8", "");
    return;
  }
  g_http.send(404, "text/plain; charset=utf-8", "not found");
}

static void routes_common() {
  register_probe_handlers();
  g_http.on("/", HTTP_GET, handle_root);
  g_http.on("/save", HTTP_POST, handle_save);
  g_http.on("/forget", HTTP_POST, handle_forget);
  g_http.on("/rediscover", HTTP_POST, handle_rediscover);
  g_http.onNotFound(handle_notfound);
}

void webui_start_ap() {
  g_ap_mode = true;
  WiFi.mode(WIFI_AP);
  // Open AP. Your WiFi password and bot token cross this link, so it is
  // deliberately short-lived — the device leaves AP mode as soon as it is
  // configured.
  WiFi.softAP("meshycord-setup");
  delay(300);
  Serial.printf("[web] AP 'meshycord-setup' -> open http://%s in a normal "
                "browser (no captive-portal popup, so paste works)\n",
                WiFi.softAPIP().toString().c_str());
  g_dns.setErrorReplyCode(DNSReplyCode::NoError);
  g_dns.start(53, "*", WiFi.softAPIP());
  routes_common();
  g_http.begin();

  // mDNS on the softAP too, so the same name works in setup mode as in normal
  // operation. Not every client resolves mDNS on a network with no internet,
  // so the IP stays the documented fallback.
  if (MDNS.begin("meshycord")) {
    MDNS.addService("http", "tcp", 80);
    Serial.println("[web] http://meshycord.local (or http://192.168.4.1)");
  }
  g_started = true;
  heap_log("webui ap started");
}

void webui_start_sta() {
  g_ap_mode = false;
  routes_common();
  g_http.begin();
  if (MDNS.begin("meshycord")) {
    MDNS.addService("http", "tcp", 80);
    Serial.println("[web] http://meshycord.local");
  }
  g_started = true;
  heap_log("webui sta started");
}

void webui_loop() {
  if (!g_started) return;
  if (g_ap_mode) g_dns.processNextRequest();
  g_http.handleClient();
}
