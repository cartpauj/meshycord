#pragma once

#include <Arduino.h>

// Start the provisioning access point ("meshycord-setup") with a captive portal.
// Used on first boot, when settings are incomplete, or when BOOT is held.
void webui_start_ap();

// Start the settings server in station mode (normal operation).
void webui_start_sta();

// Must be called from loop(); services HTTP and, in AP mode, DNS.
void webui_loop();

bool webui_in_ap_mode();

// Set true by the web handler after a successful save, so main can reboot
// outside of a request handler.
bool webui_reboot_requested();
