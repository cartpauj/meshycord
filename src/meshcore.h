// MeshCore companion-protocol client over BLE (Nordic UART Service).
//
// Byte layouts and command codes are reproduced in ../PROTOCOL-NOTES.md,
// read from MeshCore v1.17.0 (727fc05).
#pragma once

#include <Arduino.h>

// --- command codes (MyMesh.cpp:6)
static const uint8_t CMD_APP_START           = 0x01;
static const uint8_t CMD_SEND_TXT_MSG        = 0x02;
static const uint8_t CMD_SEND_CHANNEL_TXT    = 0x03;
static const uint8_t CMD_GET_CONTACTS        = 0x04;
static const uint8_t CMD_ADD_UPDATE_CONTACT  = 0x09;
static const uint8_t CMD_SET_DEVICE_TIME     = 0x06;
static const uint8_t CMD_SYNC_NEXT_MESSAGE   = 0x0A;
static const uint8_t CMD_DEVICE_QUERY        = 0x16;
static const uint8_t CMD_SEND_LOGIN          = 0x1A;
static const uint8_t CMD_GET_CONTACT_BY_KEY  = 0x1E;
static const uint8_t CMD_GET_CHANNEL         = 0x1F;

// --- response / push codes
static const uint8_t PKT_OK                  = 0x00;
static const uint8_t PKT_ERROR               = 0x01;
static const uint8_t PKT_CONTACTS_START     = 0x02;
static const uint8_t PKT_CONTACT            = 0x03;
static const uint8_t PKT_END_OF_CONTACTS    = 0x04;
static const uint8_t PKT_SENT               = 0x06;  // reply to CMD_SEND_TXT_MSG
static const uint8_t PKT_SELF_INFO           = 0x05;
static const uint8_t PKT_CONTACT_MSG_RECV    = 0x07;
static const uint8_t PKT_CHANNEL_MSG_RECV    = 0x08;
static const uint8_t PKT_NO_MORE_MSGS        = 0x0A;
static const uint8_t PKT_DEVICE_INFO         = 0x0D;
static const uint8_t PKT_CONTACT_MSG_RECV_V3 = 0x10;
static const uint8_t PKT_CHANNEL_MSG_RECV_V3 = 0x11;
static const uint8_t PKT_CHANNEL_INFO       = 0x12;  // reply to CMD_GET_CHANNEL
static const uint8_t PUSH_SEND_CONFIRMED     = 0x82;  // [ack 4][trip_time 4]
static const uint8_t PUSH_MSG_WAITING        = 0x83;

// --- advert types (AdvertDataHelpers.h:7)
static const uint8_t ADV_TYPE_NONE     = 0;
static const uint8_t ADV_TYPE_CHAT     = 1;
static const uint8_t ADV_TYPE_REPEATER = 2;
static const uint8_t ADV_TYPE_ROOM     = 3;
static const uint8_t ADV_TYPE_SENSOR   = 4;

static const size_t MESH_MAX_MSG_LEN = 133;   // MeshCore spec
static const size_t PUB_KEY_SZ       = 32;    // MeshCore.h:8
static const size_t MAX_PATH_SZ      = 64;    // MeshCore.h:22

struct MeshMessage {
  bool     is_channel = false;
  uint8_t  channel_idx = 0;
  char     pubkey_prefix[13] = {0};   // 6 bytes as lowercase hex
  // For txt_type==2 ("signed") messages the 4 bytes the companion docs call a
  // signature are in fact the ORIGINAL AUTHOR's pubkey prefix — that is how a
  // room server identifies who wrote a post (simple_room_server/MyMesh.cpp:80).
  char     author_prefix[9] = {0};    // 4 bytes as lowercase hex, or empty
  bool     have_snr = false;
  float    snr = 0;
  // Raw path byte from the node. NOT a plain hop count: bits 0-5 are the hop
  // count and bits 6-7 the per-hop hash size (Packet.h:79). The whole byte is
  // 0xFF when the packet came by a direct route, where hop count does not
  // apply (MyMesh.cpp queueMessage).
  uint8_t  path_raw = 0xFF;
  bool     have_hops = false;   // false for direct routes
  uint8_t  hops = 0;            // decoded hop count, valid when have_hops
  uint32_t timestamp = 0;
  String   text;
};

struct MeshContact {
  bool     found = false;
  uint8_t  type = ADV_TYPE_NONE;
  String   name;
  uint8_t  pubkey[32] = {0};
  // From the contact record (MyMesh.cpp:166). Used for sorting.
  uint32_t last_advert = 0;   // when we last heard them
  uint8_t  hops = 0xFF;       // out_path_len; 0xFF == unknown path
};

void mesh_begin();                       // NimBLE init
bool mesh_connected();
bool mesh_connect();                     // scan, bond, subscribe, handshake
void mesh_disconnect();

// True when the device has told us messages are queued.
bool mesh_messages_waiting();

// Pull the next queued message. Returns:
//   1  a message was parsed into `out`
//   0  no more messages
//  -1  error / timeout
int  mesh_next_message(MeshMessage& out);

// Resolve a contact from the 6-byte prefix that arrives with a message.
//
// NOTE: CMD_GET_CONTACT_BY_KEY cannot do this — it requires the FULL 32-byte
// public key (MyMesh.cpp:1322 calls lookupContactByPubKey(pub_key,
// PUB_KEY_SIZE)), and messages only carry 6 bytes. So prefixes are resolved
// against a cache built by enumerating CMD_GET_CONTACTS. The V3 remains the
// source of truth; this is a lookup index, not a second database.
bool mesh_lookup_contact(const String& prefix_hex, MeshContact& out);

// Re-enumerate contacts from the node into the cache. Called automatically on
// a cache miss (rate-limited) and after connecting.
bool mesh_refresh_contacts();
size_t mesh_contact_cache_count();

// Human-readable name of a mesh channel slot, e.g. "Public" for index 0.
// Returns "" if the slot is empty or the node refuses. Response layout:
//   [0x12][channel_idx][name 32][secret 16]   (MyMesh.cpp:1704)
String mesh_channel_name(uint8_t idx);

// --- read-only views for the web UI -------------------------------------
// Contacts come from the cache built by CMD_GET_CONTACTS; the V3 stays the
// source of truth. prefix_out receives the 12-hex key prefix used for routing.
size_t mesh_contact_count();
bool   mesh_contact_at(size_t i, MeshContact& out, char prefix_out[13]);

// Channel slots 0..7, cached at connect time so rendering a page does not
// fire eight BLE round-trips.
static const uint8_t MESH_MAX_CHANNELS = 8;
void   mesh_refresh_channels();
bool   mesh_channel_at(uint8_t idx, String& name_out);

// Send. Text longer than 133 bytes is chunked by the caller.
// Sends a DM. On success `ack_out` receives the expected-ACK handle and
// `timeout_ms_out` the node's own estimate of how long delivery should take —
// both from RESP_CODE_SENT. Pass nullptr if you do not care.
bool mesh_send_dm(const String& prefix_hex, const String& text,
                  uint32_t* ack_out = nullptr, uint32_t* timeout_ms_out = nullptr);

// Pops a delivery confirmation pushed by the node (PUSH_CODE_SEND_CONFIRMED).
// Returns false when none is pending.
bool mesh_take_confirmation(uint32_t* ack_out, uint32_t* trip_ms_out);
bool mesh_send_channel(uint8_t channel_idx, const String& text);

// Log into a room server (needed before it will relay messages).
bool mesh_room_login(const String& prefix_hex, const String& password);

// Add (or update) a contact on the node from a full 32-byte public key.
//
// Layout per updateContactFromFrame (MyMesh.cpp:189):
//   [0x09][pubkey 32][type][flags][out_path_len][out_path 64][name 32]
//   [last_advert 4]   then optional [lat 4][lon 4][lastmod 4]
//
// out_path_len is set to 0xFF meaning "no known path", so the node floods until
// it learns one. Intended for adding a node seen on the public map, which
// adverts cannot reach.
bool mesh_add_contact(const String& pubkey_hex, const String& name, uint8_t type);
