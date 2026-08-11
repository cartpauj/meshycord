// Runtime settings, persisted in NVS.
//
// Nothing secret is compiled into the firmware image. Everything below is
// entered through the web UI and stored in NVS, so the same binary is safe to
// share and reflashing does not leak credentials.
#pragma once

#include <Arduino.h>

struct Settings {
  // --- WiFi (station mode)
  String wifi_ssid;
  String wifi_pass;

  // --- Discord
  String bot_token;        // SECRET. Never rendered back to the browser.
  String guild_id;
  String inbox_channel;    // default destination for strangers / unrouted
  String admin_channel;    // #meshycord-admin, auto-created; NEVER bridged
  uint32_t poll_interval_ms = 30000;   // Discord->mesh reply latency

  // --- Web UI
  String ui_user = "admin";
  String ui_pass;          // blank disables auth (logged as a warning)

  // --- BLE target (blank = first device advertising the MeshCore NUS service)
  String ble_name;
  String ble_addr;
  String ble_pin;

  // --- Policy
  // Both default OFF. #meshycord-admin can list/search/add/remove, so explicit
  // linking is easy and automatic creation would just surprise people by
  // filling their server.
  bool autocreate_channels = false;
  bool autocreate_rooms    = false;

  // The inbox channel is always created by the bridge, so it is never asked
  // for. The guild IS required: auto-detecting it from the bot's membership
  // only worked when the bot was in exactly one server, which is a silent
  // failure mode for anyone who runs it in two.
  bool configured() const {
    return wifi_ssid.length() > 0 && bot_token.length() > 0 &&
           guild_id.length() > 0;
  }
};

extern Settings g_settings;

void settings_load();
void settings_save();
void settings_factory_reset();
