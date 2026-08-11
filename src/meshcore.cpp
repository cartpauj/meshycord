#include "meshcore.h"
#include "settings.h"
#include "util.h"

#include <NimBLEDevice.h>

// MeshCore companion == Nordic UART Service
// src/helpers/esp32/SerialBLEInterface.cpp:7
static const char* NUS_SERVICE = "6E400001-B5A3-F393-E0A9-E50E24DCCA9E";
static const char* NUS_RX      = "6E400002-B5A3-F393-E0A9-E50E24DCCA9E"; // we write
static const char* NUS_TX      = "6E400003-B5A3-F393-E0A9-E50E24DCCA9E"; // we notify

static NimBLEClient*               g_client = nullptr;
static NimBLERemoteCharacteristic* g_rx = nullptr;
static NimBLERemoteCharacteristic* g_tx = nullptr;
static volatile bool g_connected = false;
static volatile bool g_msgs_waiting = false;

// Delivery confirmations, filled from the BLE callback.
struct Confirm { uint32_t ack; uint32_t trip_ms; };
static const size_t CQ = 8;
static Confirm g_cq[CQ];
static volatile size_t g_cq_head = 0, g_cq_tail = 0;
static volatile bool g_authenticated = false;   // MITM-protected link achieved?
static uint8_t       g_auth_failures = 0;       // consecutive unauthenticated links

// One BLE notification == one protocol frame (type in byte 0). The BLE
// callback must never block, so it only enqueues.
struct Frame { uint8_t data[256]; size_t len; };
// CMD_GET_CONTACTS streams one frame per contact back-to-back. With a queue of
// 8 a burst silently overflowed and contacts were dropped, so senders could
// not be classified. Sized for a full contact list instead.
static const size_t FQ = 24;
static Frame g_fq[FQ];
static volatile size_t g_head = 0, g_tail = 0;

static bool fq_push(const uint8_t* d, size_t n) {
  size_t next = (g_head + 1) % FQ;
  if (next == g_tail) return false;
  if (n > sizeof(g_fq[0].data)) n = sizeof(g_fq[0].data);
  memcpy(g_fq[g_head].data, d, n);
  g_fq[g_head].len = n;
  g_head = next;
  return true;
}
static bool fq_pop(Frame& out) {
  if (g_tail == g_head) return false;
  out = g_fq[g_tail];
  g_tail = (g_tail + 1) % FQ;
  return true;
}
static void fq_drain() { Frame f; while (fq_pop(f)) {} }

static void onNotify(NimBLERemoteCharacteristic*, uint8_t* data,
                     size_t len, bool) {
  if (!len) return;

  // Everything >= 0x80 is an asynchronous PUSH_CODE_*, never a command reply
  // (MyMesh.cpp:112). Keeping them out of the response queue entirely removes a
  // whole bug class: a push landing mid-command used to be mistaken for that
  // command's reply. It cost us a contact enumeration (0x88
  // PUSH_CODE_LOG_RX_DATA) and, before that, a channel lookup.
  if (data[0] >= 0x80) {
    if (data[0] == PUSH_MSG_WAITING) g_msgs_waiting = true;
    // Delivery confirmation for a DM we sent: [0x82][ack 4][trip_time 4].
    // Captured here rather than queued, so it can never be mistaken for a
    // command reply — that mix-up cost us two separate bugs already.
    else if (data[0] == PUSH_SEND_CONFIRMED && len >= 9) {
      uint32_t ack, trip;
      memcpy(&ack,  &data[1], 4);
      memcpy(&trip, &data[5], 4);
      size_t next = (g_cq_head + 1) % CQ;
      if (next != g_cq_tail) {
        g_cq[g_cq_head].ack = ack;
        g_cq[g_cq_head].trip_ms = trip;
        g_cq_head = next;
      }
    }
    return;
  }
  fq_push(data, len);
}

class Cbs : public NimBLEClientCallbacks {
  void onConnect(NimBLEClient* c) override {
    c->updateConnParams(24, 48, 0, 400);
  }
  void onDisconnect(NimBLEClient*) override {
    g_connected = false;
    Serial.println("[ble] disconnected");
  }
  uint32_t onPassKeyRequest() override {
    // The companion pairs with a STATIC PIN and demands MITM protection
    // (SerialBLEInterface.cpp:32 setStaticPIN / ESP_LE_AUTH_REQ_SC_MITM_BOND),
    // so we must perform passkey entry. MeshCore's default is 123456.
    uint32_t pin = g_settings.ble_pin.length()
                     ? (uint32_t)g_settings.ble_pin.toInt()
                     : 123456;
    Serial.printf("[ble] passkey request -> %u\n", pin);
    return pin;
  }
  bool onConfirmPIN(uint32_t) override { return true; }
  void onAuthenticationComplete(ble_gap_conn_desc* d) override {
    g_authenticated = d->sec_state.authenticated;
    Serial.printf("[ble] auth enc=%d authd=%d bonded=%d\n",
                  d->sec_state.encrypted, d->sec_state.authenticated,
                  d->sec_state.bonded);
  }
};
static Cbs g_cbs;

bool mesh_connected()        { return g_connected; }
bool mesh_messages_waiting() { return g_msgs_waiting; }

void mesh_begin() {
  NimBLEDevice::init("meshycord");
  NimBLEDevice::setPower(ESP_PWR_LVL_P9);
  // The companion's TX characteristic is ESP_GATT_PERM_READ_ENC_MITM, so an
  // encrypted, MITM-protected link is mandatory: we must bond.
  NimBLEDevice::setSecurityAuth(true, true, true);   // bond, MITM, secure conn
  // KEYBOARD_ONLY means "we can enter a passkey", which is what produces an
  // AUTHENTICATED link. NO_INPUT_OUTPUT forces Just Works pairing, which is
  // encrypted but never authenticated — and the companion rejects writes on an
  // unauthenticated link (ESP_GATT_PERM_WRITE_ENC_MITM).
  NimBLEDevice::setSecurityIOCap(BLE_HS_IO_KEYBOARD_ONLY);
  heap_log("nimble init");
}

void mesh_disconnect() {
  if (g_client && g_client->isConnected()) g_client->disconnect();
  g_connected = false;
}

// Send one command, wait for a matching response. Docs: one at a time, 5s.
static bool cmd(const uint8_t* payload, size_t len, uint8_t expect,
                Frame* reply, uint32_t timeout_ms = 5000) {
  if (!g_rx || !g_connected) return false;
  fq_drain();
  if (!g_rx->writeValue(payload, len, true)) {
    Serial.println("[mesh] write failed");
    return false;
  }
  uint32_t deadline = millis() + timeout_ms;
  while (millis() < deadline) {
    Frame f;
    if (fq_pop(f)) {
      if (expect == 0xFF || f.data[0] == expect) { if (reply) *reply = f; return true; }
      Serial.printf("[mesh] skip 0x%02X (want 0x%02X)\n", f.data[0], expect);
    }
    delay(5);
  }
  Serial.printf("[mesh] timeout want 0x%02X\n", expect);
  return false;
}

// Like cmd(), but accepts any frame for which `pred` returns true. Frames that
// do not match are asynchronous pushes and are skipped.
static bool cmd_until(const uint8_t* payload, size_t len,
                      bool (*pred)(uint8_t), Frame* reply,
                      uint32_t timeout_ms = 5000) {
  if (!g_rx || !g_connected) return false;
  fq_drain();
  if (!g_rx->writeValue(payload, len, true)) {
    Serial.println("[mesh] write failed");
    return false;
  }
  uint32_t deadline = millis() + timeout_ms;
  while (millis() < deadline) {
    Frame f;
    if (fq_pop(f)) {
      if (pred(f.data[0])) { if (reply) *reply = f; return true; }
      Serial.printf("[mesh] skip push 0x%02X\n", f.data[0]);
    }
    delay(5);
  }
  Serial.println("[mesh] timeout");
  return false;
}

static void hex12(const uint8_t* b, char* out13) {
  for (int i = 0; i < 6; i++) sprintf(&out13[i * 2], "%02x", b[i]);
  out13[12] = 0;
}

static size_t hex_to_bytes(const String& hex, uint8_t* out, size_t max) {
  size_t n = 0;
  for (size_t i = 0; i + 1 < hex.length() && n < max; i += 2) {
    auto nib = [](char c) -> int {
      if (c >= '0' && c <= '9') return c - '0';
      if (c >= 'a' && c <= 'f') return c - 'a' + 10;
      if (c >= 'A' && c <= 'F') return c - 'A' + 10;
      return -1;
    };
    int hi = nib(hex[i]), lo = nib(hex[i + 1]);
    if (hi < 0 || lo < 0) break;
    out[n++] = (uint8_t)((hi << 4) | lo);
  }
  return n;
}

bool mesh_connect() {
  heap_log("ble connect enter");
  NimBLEScan* scan = NimBLEDevice::getScan();
  scan->setActiveScan(true);
  scan->setInterval(100);
  scan->setWindow(99);
  Serial.println("[ble] scanning");
  NimBLEScanResults res = scan->start(8, false);

  bool found = false;
  NimBLEAddress addr;
  std::string name;
  for (uint32_t i = 0; i < res.getCount(); i++) {
    // NimBLE 1.4.x returns by value; don't hold pointers into scan results.
    NimBLEAdvertisedDevice d = res.getDevice(i);
    bool match;
    if (g_settings.ble_addr.length())
      match = (d.getAddress().toString() == std::string(g_settings.ble_addr.c_str()));
    else if (g_settings.ble_name.length())
      match = d.haveName() &&
              d.getName().find(g_settings.ble_name.c_str()) != std::string::npos;
    else
      match = d.isAdvertisingService(NimBLEUUID(NUS_SERVICE));
    if (match) {
      addr = d.getAddress();
      name = d.haveName() ? d.getName() : std::string("(unnamed)");
      found = true;
      break;
    }
  }
  scan->clearResults();
  if (!found) { Serial.println("[ble] no companion found"); return false; }
  Serial.printf("[ble] found %s (%s)\n", name.c_str(), addr.toString().c_str());

  if (!g_client) {
    g_client = NimBLEDevice::createClient();
    g_client->setClientCallbacks(&g_cbs, false);
    g_client->setConnectTimeout(10);
  }
  g_authenticated = false;
  if (!g_client->connect(addr)) { Serial.println("[ble] connect failed"); return false; }
  if (!g_client->secureConnection()) {
    Serial.println("[ble] bonding failed");
    g_client->disconnect();
    return false;
  }
  if (!g_authenticated) {
    // A stale bond is reused and stays unauthenticated forever, so drop it and
    // re-pair. Purging only ONCE per boot meant a PIN change on the node left
    // the bridge retrying the dead bond indefinitely, so re-purge on every
    // third consecutive failure instead.
    Serial.println("[ble] link not authenticated - writes would be rejected");
    g_client->disconnect();
    if (++g_auth_failures % 3 == 1) {
      Serial.println("[ble] deleting stale bonds, will re-pair");
      NimBLEDevice::deleteAllBonds();
    }
    return false;
  }
  g_auth_failures = 0;
  g_client->setDataLen(251);

  NimBLERemoteService* svc = g_client->getService(NimBLEUUID(NUS_SERVICE));
  if (!svc) { Serial.println("[ble] no NUS"); g_client->disconnect(); return false; }
  g_rx = svc->getCharacteristic(NimBLEUUID(NUS_RX));
  g_tx = svc->getCharacteristic(NimBLEUUID(NUS_TX));
  if (!g_rx || !g_tx) { Serial.println("[ble] no chars"); g_client->disconnect(); return false; }
  if (!g_tx->subscribe(true, onNotify)) {
    Serial.println("[ble] subscribe failed");
    g_client->disconnect();
    return false;
  }
  g_connected = true;

  // Handshake: 0x01, 7 reserved bytes, app name.
  // CMD_APP_START: [0x01][7 reserved][app name]. Derive the length from the
  // name instead of hardcoding it — a hardcoded 6 silently truncated the name
  // to "meshyc" after the app was renamed.
  static const char* APP_NAME = "meshycord";
  const size_t name_len = strlen(APP_NAME);
  uint8_t start[8 + 32];
  memset(start, 0, sizeof(start));
  start[0] = CMD_APP_START;
  memcpy(&start[8], APP_NAME, name_len);
  Frame self_info;
  if (!cmd(start, 8 + name_len, PKT_SELF_INFO, &self_info)) {
    Serial.println("[mesh] APP_START failed");
    mesh_disconnect();
    return false;
  }
  Serial.printf("[mesh] APP_START ok (%u bytes), mtu=%u\n",
                (unsigned)self_info.len, g_client->getMTU());

  // Channels first: only 8 quick queries. Contact enumeration can stream for
  // tens of seconds, and running it first left the channel replies stuck
  // behind that flood (they arrived late and were discarded).
  mesh_refresh_channels();
  mesh_refresh_contacts();  // so inbound messages can be classified
  g_msgs_waiting = true;   // drain anything queued while we were away
  heap_log("ble connect done");
  return true;
}

// Accept only the frames CMD_SYNC_NEXT_MESSAGE can legitimately answer with.
// Using the wildcard here let an asynchronous push (e.g. a new advert) be
// mis-parsed as a message.
static bool is_sync_reply(uint8_t t) {
  return t == PKT_NO_MORE_MSGS || t == PKT_CONTACT_MSG_RECV ||
         t == PKT_CONTACT_MSG_RECV_V3 || t == PKT_CHANNEL_MSG_RECV ||
         t == PKT_CHANNEL_MSG_RECV_V3 || t == PKT_ERROR;
}

int mesh_next_message(MeshMessage& out) {
  uint8_t c = CMD_SYNC_NEXT_MESSAGE;
  Frame f;
  if (!cmd_until(&c, 1, is_sync_reply, &f)) return -1;

  uint8_t t = f.data[0];
  if (t == PKT_ERROR) return -1;
  if (t == PKT_NO_MORE_MSGS) { g_msgs_waiting = false; return 0; }

  bool is_contact = (t == PKT_CONTACT_MSG_RECV || t == PKT_CONTACT_MSG_RECV_V3);
  bool is_channel = (t == PKT_CHANNEL_MSG_RECV || t == PKT_CHANNEL_MSG_RECV_V3);
  if (!is_contact && !is_channel) {
    Serial.printf("[mesh] ignoring 0x%02X\n", t);
    return -1;
  }

  const uint8_t* d = f.data;
  size_t off = 1;
  out = MeshMessage();
  bool v3 = (t == PKT_CONTACT_MSG_RECV_V3 || t == PKT_CHANNEL_MSG_RECV_V3);
  if (v3) {
    out.have_snr = true;
    out.snr = ((int8_t)d[off]) / 4.0f;
    off += 3;                      // SNR + 2 reserved
  }
  out.is_channel = is_channel;
  if (is_channel) {
    if (off + 1 > f.len) return -1;
    out.channel_idx = d[off++];
  } else {
    if (off + 6 > f.len) return -1;
    hex12(&d[off], out.pubkey_prefix);
    off += 6;
  }
  if (off + 2 > f.len) return -1;
  // Decode the packed path byte rather than printing it raw: it was showing
  // "71 hops" for what is actually 7.
  out.path_raw = d[off];
  if (out.path_raw == 0xFF) {
    out.have_hops = false;       // direct route, hop count not applicable
  } else {
    out.have_hops = true;
    out.hops = out.path_raw & 63;    // Packet.h:80 getPathHashCount()
  }
  uint8_t txt_type = d[off + 1];
  off += 2;
  if (off + 4 > f.len) return -1;
  out.timestamp = (uint32_t)d[off] | ((uint32_t)d[off+1] << 8) |
                  ((uint32_t)d[off+2] << 16) | ((uint32_t)d[off+3] << 24);
  off += 4;
  if (txt_type == 2) {
    // Not a signature: the original author's pubkey prefix (4 bytes).
    if (off + 4 <= f.len) {
      for (int i = 0; i < 4; i++)
        sprintf(&out.author_prefix[i * 2], "%02x", d[off + i]);
      out.author_prefix[8] = 0;
    }
    off += 4;
  }
  if (off > f.len) return -1;
  out.text = "";
  out.text.reserve(f.len - off + 1);
  for (size_t i = off; i < f.len; i++) out.text += (char)d[i];
  return 1;
}

// --- contact cache -------------------------------------------------------
//
// Contact frame layout (MyMesh.cpp:166 writeContactRespFrame):
//   [code][pub_key 32][type 1][flags 1][out_path_len 1][out_path 64]
//   [name 32][last_advert 4][gps_lat 4][gps_lon 4][lastmod 4]
// so name sits at offset 1+32+1+1+1+64 = 100.
static const size_t CONTACT_TYPE_OFF     = 1 + PUB_KEY_SZ;          // 33
static const size_t CONTACT_PATHLEN_OFF  = 1 + PUB_KEY_SZ + 1 + 1;  // 35
static const size_t CONTACT_NAME_OFF     = 1 + PUB_KEY_SZ + 1 + 1 + 1 + MAX_PATH_SZ; // 100
static const size_t CONTACT_ADVERT_OFF   = CONTACT_NAME_OFF + 32;   // 132

struct CacheEntry {
  char     prefix[13];
  uint8_t  type;
  char     name[36];   // 32 bytes of name + room for a NUL
  uint32_t last_advert;
  uint8_t  hops;
  // Full 32-byte key is kept because several commands REQUIRE it and cannot
  // work from the 6-byte prefix that messages carry:
  //   CMD_SEND_LOGIN         (MyMesh.cpp:1524, len >= 1 + PUB_KEY_SIZE)
  //   CMD_GET_CONTACT_BY_KEY (MyMesh.cpp:1322, lookupContactByPubKey(.., 32))
  uint8_t pubkey[PUB_KEY_SZ];
};
// Only contacts that can actually exchange messages are cached — ADV_TYPE_CHAT
// (companions) and ADV_TYPE_ROOM (room servers). Repeaters and sensors are
// skipped: a mesh of 350 contacts is mostly repeaters, and caching them would
// blow the cap and push real senders out. An unrecognised sender simply routes
// to the inbox, so nothing is lost.
static const size_t CACHE_MAX = 192;
static CacheEntry g_cache[CACHE_MAX];
static size_t     g_cache_n = 0;
static uint32_t   g_cache_last_refresh = 0;

size_t mesh_contact_cache_count() { return g_cache_n; }
size_t mesh_contact_count()       { return g_cache_n; }

bool mesh_contact_at(size_t i, MeshContact& out, char prefix_out[13]) {
  if (i >= g_cache_n) return false;
  out = MeshContact();
  out.found = true;
  out.type  = g_cache[i].type;
  out.name  = g_cache[i].name;
  out.last_advert = g_cache[i].last_advert;
  out.hops        = g_cache[i].hops;
  memcpy(out.pubkey, g_cache[i].pubkey, PUB_KEY_SZ);
  strncpy(prefix_out, g_cache[i].prefix, 13);
  return true;
}

// --- channel name cache ---
static char g_chan_names[MESH_MAX_CHANNELS][32];
static bool g_chan_valid[MESH_MAX_CHANNELS];

bool mesh_channel_at(uint8_t idx, String& name_out) {
  if (idx >= MESH_MAX_CHANNELS || !g_chan_valid[idx]) return false;
  name_out = g_chan_names[idx];
  return true;
}

static size_t g_cache_seen = 0;     // contacts observed, including skipped
static size_t g_cache_skipped = 0;  // repeaters/sensors ignored

static void cache_put_from_frame(const Frame& f) {
  if (f.len < CONTACT_NAME_OFF) return;
  g_cache_seen++;

  uint8_t type = f.data[CONTACT_TYPE_OFF];
  if (type != ADV_TYPE_CHAT && type != ADV_TYPE_ROOM) {
    g_cache_skipped++;
    return;                          // repeater / sensor / unknown: not routable
  }

  char prefix[13];
  hex12(&f.data[1], prefix);

  CacheEntry* e = nullptr;
  for (size_t i = 0; i < g_cache_n; i++)
    if (strcmp(g_cache[i].prefix, prefix) == 0) { e = &g_cache[i]; break; }
  if (!e) {
    if (g_cache_n >= CACHE_MAX) return;         // full; oldest wins, fine
    e = &g_cache[g_cache_n++];
  }
  strncpy(e->prefix, prefix, sizeof(e->prefix));
  memcpy(e->pubkey, &f.data[1], PUB_KEY_SZ);
  e->type = type;
  e->hops = (CONTACT_PATHLEN_OFF < f.len) ? f.data[CONTACT_PATHLEN_OFF] : 0xFF;
  e->last_advert = 0;
  if (CONTACT_ADVERT_OFF + 4 <= f.len) {
    memcpy(&e->last_advert, &f.data[CONTACT_ADVERT_OFF], 4);   // little-endian
  }
  // The node sends the name as a 32-byte NUL-padded field. Read all 32, then
  // trim to a UTF-8 boundary so an emoji is never cut in half.
  {
    char raw[33];
    memset(raw, 0, sizeof(raw));
    for (size_t i = 0; i < 32 && CONTACT_NAME_OFF + i < f.len; i++) {
      char c = (char)f.data[CONTACT_NAME_OFF + i];
      if (!c) break;
      raw[i] = c;
    }
    String clean = utf8_truncate(String(raw), 32);
    memset(e->name, 0, sizeof(e->name));
    strncpy(e->name, clean.c_str(), sizeof(e->name) - 1);
  }
}

bool mesh_refresh_contacts() {
  if (!g_connected) return false;

  // CMD_GET_CONTACTS with no 'since' -> full list.
  // Replies: CONTACTS_START(2), then N x CONTACT(3), then END_OF_CONTACTS(4).
  uint8_t c = CMD_GET_CONTACTS;
  Frame f;
  if (!cmd(&c, 1, PKT_CONTACTS_START, &f, 8000)) {
    Serial.println("[mesh] GET_CONTACTS: no CONTACTS_START");
    return false;
  }

  g_cache_n = 0;
  // Enumeration streams one frame per contact and can take a while on a node
  // with many contacts; too short a deadline left the iterator still running
  // and its frames leaking into the next command.
  uint32_t deadline = millis() + 30000;
  size_t got = 0;
  while (millis() < deadline) {
    Frame g;
    if (!fq_pop(g)) { watchdog_feed(); delay(5); continue; }
    if (g.data[0] == PKT_CONTACT)         { cache_put_from_frame(g); got++; continue; }
    if (g.data[0] == PKT_END_OF_CONTACTS) break;
    // anything else (pushes) is ignored during enumeration
  }
  g_cache_last_refresh = millis();
  Serial.printf("[mesh] contacts: %u seen, %u skipped (repeater/sensor), "
                "%u cached%s\n",
                (unsigned)g_cache_seen, (unsigned)g_cache_skipped,
                (unsigned)g_cache_n,
                g_cache_n >= CACHE_MAX ? "  *** CACHE FULL ***" : "");
  return true;
}

bool mesh_lookup_contact(const String& prefix_hex, MeshContact& out) {
  out = MeshContact();
  // Accept 8 hex chars (4-byte room-post author prefix) as well as the usual 12.
  if (prefix_hex.length() < 8) return false;
  size_t n = prefix_hex.length() < 12 ? prefix_hex.length() : 12;
  String want = prefix_hex.substring(0, n);
  want.toLowerCase();

  for (int attempt = 0; attempt < 2; attempt++) {
    for (size_t i = 0; i < g_cache_n; i++) {
      if (strncmp(want.c_str(), g_cache[i].prefix, want.length()) == 0) {
        out.found = true;
        out.type  = g_cache[i].type;
        out.name  = g_cache[i].name;
        out.last_advert = g_cache[i].last_advert;
        out.hops        = g_cache[i].hops;
        memcpy(out.pubkey, g_cache[i].pubkey, PUB_KEY_SZ);
        return true;
      }
    }
    // Miss: refresh at most once per 30s, then retry the lookup.
    if (attempt == 0) {
      if (g_cache_last_refresh != 0 && millis() - g_cache_last_refresh < 30000)
        break;
      if (!mesh_refresh_contacts()) break;
    }
  }
  return false;   // unknown sender -> caller routes to the inbox
}

// Accept only what CMD_GET_CHANNEL can answer with: the info frame, or an
// error for an empty slot. Without this a stale frame from the contact
// enumeration could be mistaken for the reply.
static bool is_channel_reply(uint8_t t) {
  return t == PKT_CHANNEL_INFO || t == PKT_ERROR;
}

String mesh_channel_name(uint8_t idx) {
  if (!g_connected) return "";
  uint8_t buf[2] = { CMD_GET_CHANNEL, idx };
  Frame f;

  // Two attempts, and CRITICALLY the reply's channel index (byte 1) must match
  // what we asked for. Without that check a timed-out query for slot N could be
  // satisfied by its own late reply arriving during the query for slot N+1,
  // giving slot N+1 the wrong name.
  for (int attempt = 0; attempt < 2; attempt++) {
    fq_drain();
    if (!cmd_until(buf, 2, is_channel_reply, &f, 3000)) { delay(250); continue; }
    if (f.data[0] != PKT_CHANNEL_INFO) return "";        // empty slot
    if (f.len >= 2 && f.data[1] == idx) break;           // correct slot
    Serial.printf("[mesh] channel reply for %u while asking %u; retrying\n",
                  (unsigned)f.data[1], (unsigned)idx);
    f.len = 0;
    delay(250);
  }
  if (f.len == 0 || f.data[0] != PKT_CHANNEL_INFO ||
      f.len < 2 || f.data[1] != idx) return "";
  // [code][idx][name 32][secret 16]
  String name;
  for (size_t i = 2; i < f.len && i < 2 + 32; i++) {
    char c = (char)f.data[i];
    if (!c) break;
    name += c;
  }
  return name;
}

void mesh_refresh_channels() {
  // Let the node settle and clear anything still queued from the contact
  // iterator, so channel replies are not confused with leftover frames.
  delay(400);
  fq_drain();
  for (uint8_t i = 0; i < MESH_MAX_CHANNELS; i++) {
    g_chan_valid[i] = false;
    g_chan_names[i][0] = 0;
    String n = mesh_channel_name(i);
    if (n.length()) {
      strncpy(g_chan_names[i], n.c_str(), sizeof(g_chan_names[i]) - 1);
      g_chan_names[i][sizeof(g_chan_names[i]) - 1] = 0;
      g_chan_valid[i] = true;
    }
  }
  int n = 0;
  for (uint8_t i = 0; i < MESH_MAX_CHANNELS; i++) if (g_chan_valid[i]) n++;
  Serial.printf("[mesh] channels cached: %d\n", n);
}

bool mesh_send_dm(const String& prefix_hex, const String& text,
                  uint32_t* ack_out, uint32_t* timeout_ms_out) {
  uint8_t key[32] = {0};
  size_t n = hex_to_bytes(prefix_hex, key, sizeof(key));
  if (n < 6) return false;

  // CMD_SEND_TXT_MSG: [0x02][txt_type][attempt][sender_timestamp u32]
  // [pubkey_prefix 6][text...]
  uint8_t buf[1 + 1 + 1 + 4 + 6 + MESH_MAX_MSG_LEN + 1];
  size_t o = 0;
  buf[o++] = CMD_SEND_TXT_MSG;
  buf[o++] = 0;                          // txt_type: plain
  buf[o++] = 0;                          // attempt
  uint32_t ts = (uint32_t)(millis() / 1000);
  buf[o++] = ts & 0xFF; buf[o++] = (ts >> 8) & 0xFF;
  buf[o++] = (ts >> 16) & 0xFF; buf[o++] = (ts >> 24) & 0xFF;
  memcpy(&buf[o], key, 6); o += 6;
  size_t tlen = text.length();
  if (tlen > MESH_MAX_MSG_LEN) tlen = MESH_MAX_MSG_LEN;
  memcpy(&buf[o], text.c_str(), tlen); o += tlen;

  Frame f;
  if (!cmd(buf, o, 0xFF, &f)) return false;
  if (f.data[0] != PKT_SENT) {          // RESP_CODE_SENT (MyMesh.cpp:77)
    Serial.printf("[mesh] send_dm rejected (0x%02X)\n", f.data[0]);
    return false;
  }
  // RESP_CODE_SENT: [0x06][flood flag][expected_ack 4][est_timeout 4]
  if (f.len >= 10) {
    uint32_t ack, est;
    memcpy(&ack, &f.data[2], 4);
    memcpy(&est, &f.data[6], 4);
    if (ack_out)        *ack_out = ack;
    if (timeout_ms_out) *timeout_ms_out = est;
  }
  return true;
}

bool mesh_send_channel(uint8_t channel_idx, const String& text) {
  // CMD_SEND_CHANNEL_TXT_MSG: [0x03][txt_type][channel_idx]
  // [sender_timestamp u32][text...]
  uint8_t buf[1 + 1 + 1 + 4 + MESH_MAX_MSG_LEN + 1];
  size_t o = 0;
  buf[o++] = CMD_SEND_CHANNEL_TXT;
  buf[o++] = 0;                          // txt_type: plain
  buf[o++] = channel_idx;
  uint32_t ts = (uint32_t)(millis() / 1000);
  buf[o++] = ts & 0xFF; buf[o++] = (ts >> 8) & 0xFF;
  buf[o++] = (ts >> 16) & 0xFF; buf[o++] = (ts >> 24) & 0xFF;
  size_t tlen = text.length();
  if (tlen > MESH_MAX_MSG_LEN) tlen = MESH_MAX_MSG_LEN;
  memcpy(&buf[o], text.c_str(), tlen); o += tlen;

  Frame f;
  if (!cmd(buf, o, 0xFF, &f)) return false;
  if (f.data[0] != PKT_OK) {            // writeOKFrame() (MyMesh.cpp:1141)
    Serial.printf("[mesh] send_channel rejected (0x%02X)\n", f.data[0]);
    return false;
  }
  return true;
}

bool mesh_room_login(const String& prefix_hex, const String& password) {
  // Needs the FULL public key, so resolve it from the contact cache first.
  MeshContact c;
  if (!mesh_lookup_contact(prefix_hex, c) || !c.found) {
    Serial.printf("[mesh] room login: %s not a known contact\n",
                  prefix_hex.c_str());
    return false;
  }

  // CMD_SEND_LOGIN: [0x1A][pubkey 32][password...]   (MyMesh.cpp:1524)
  uint8_t buf[1 + PUB_KEY_SZ + 64];
  size_t o = 0;
  buf[o++] = CMD_SEND_LOGIN;
  memcpy(&buf[o], c.pubkey, PUB_KEY_SZ); o += PUB_KEY_SZ;
  size_t plen = password.length();
  if (plen > 63) plen = 63;
  memcpy(&buf[o], password.c_str(), plen); o += plen;

  Frame f;
  if (!cmd(buf, o, 0xFF, &f)) return false;
  if (f.data[0] != PKT_SENT) {
    Serial.printf("[mesh] room login rejected (0x%02X)\n", f.data[0]);
    return false;
  }
  return true;
}

bool mesh_take_confirmation(uint32_t* ack_out, uint32_t* trip_ms_out) {
  if (g_cq_tail == g_cq_head) return false;
  if (ack_out)     *ack_out     = g_cq[g_cq_tail].ack;
  if (trip_ms_out) *trip_ms_out = g_cq[g_cq_tail].trip_ms;
  g_cq_tail = (g_cq_tail + 1) % CQ;
  return true;
}
