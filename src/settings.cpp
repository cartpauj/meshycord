#include "settings.h"
#include <Preferences.h>

Settings g_settings;

// NVS namespace. Renamed from "meshie" before first release: it is compiled in,
// so every device shares it, and changing it later would drop every deployed
// device back into setup mode.
static const char* NS = "meshy";

void settings_load() {
  Preferences p;
  p.begin(NS, /*readOnly=*/true);
  g_settings.wifi_ssid      = p.getString("wifi_ssid", "");
  g_settings.wifi_pass      = p.getString("wifi_pass", "");
  g_settings.bot_token      = p.getString("bot_token", "");
  g_settings.guild_id       = p.getString("guild_id", "");
  g_settings.inbox_channel  = p.getString("inbox", "");
  g_settings.admin_channel  = p.getString("admin", "");
  g_settings.poll_interval_ms = p.getUInt("poll_ms", 30000);
  g_settings.ui_user        = p.getString("ui_user", "admin");
  g_settings.ui_pass        = p.getString("ui_pass", "");
  g_settings.ble_name       = p.getString("ble_name", "");
  g_settings.ble_addr       = p.getString("ble_addr", "");
  g_settings.ble_pin        = p.getString("ble_pin", "");
  g_settings.autocreate_channels = p.getBool("ac_chan", false);
  g_settings.autocreate_rooms    = p.getBool("ac_room", false);
  g_settings.autocreate_dms      = p.getBool("ac_dm", false);
  p.end();

  Serial.printf("[cfg] loaded: configured=%d ssid='%s' guild='%s' inbox='%s'\n",
                g_settings.configured() ? 1 : 0,
                g_settings.wifi_ssid.c_str(),
                g_settings.guild_id.c_str(),
                g_settings.inbox_channel.c_str());
  if (g_settings.ui_pass.length() == 0)
    Serial.println("[cfg] WARNING: web UI has no password set");
}

void settings_save() {
  Preferences p;
  p.begin(NS, /*readOnly=*/false);
  p.putString("wifi_ssid", g_settings.wifi_ssid);
  p.putString("wifi_pass", g_settings.wifi_pass);
  p.putString("bot_token", g_settings.bot_token);
  p.putString("guild_id",  g_settings.guild_id);
  p.putString("inbox",     g_settings.inbox_channel);
  p.putString("admin",     g_settings.admin_channel);
  p.putUInt  ("poll_ms",   g_settings.poll_interval_ms);
  p.putString("ui_user",   g_settings.ui_user);
  p.putString("ui_pass",   g_settings.ui_pass);
  p.putString("ble_name",  g_settings.ble_name);
  p.putString("ble_addr",  g_settings.ble_addr);
  p.putString("ble_pin",   g_settings.ble_pin);
  p.putBool  ("ac_chan",    g_settings.autocreate_channels);
  p.putBool  ("ac_room",    g_settings.autocreate_rooms);
  p.putBool  ("ac_dm",      g_settings.autocreate_dms);
  p.end();
  Serial.println("[cfg] saved");
}

void settings_factory_reset() {
  Preferences p;
  p.begin(NS, false);
  p.clear();
  p.end();
  Serial.println("[cfg] factory reset");
}
