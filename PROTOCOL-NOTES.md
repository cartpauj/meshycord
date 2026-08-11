# MeshCore companion protocol — notes for the bridge

Everything here was read out of the MeshCore source at commit `727fc05`
(v1.17.0). Authoritative reference is `docs/companion_protocol.md` in
<https://github.com/meshcore-dev/MeshCore> (990 lines).

Official client libraries exist and are worth reading as reference
implementations: `meshcore_py`, `meshcore.js`, `meshcore-cli`
(all under the `meshcore-dev` org).

## BLE transport

Plain **Nordic UART Service**. From `src/helpers/esp32/SerialBLEInterface.cpp:7`:

```
SERVICE_UUID           6E400001-B5A3-F393-E0A9-E50E24DCCA9E
CHARACTERISTIC_UUID_RX 6E400002-B5A3-F393-E0A9-E50E24DCCA9E   (write commands here)
CHARACTERISTIC_UUID_TX 6E400003-B5A3-F393-E0A9-E50E24DCCA9E   (notify — subscribe)
```

TX is created with `PROPERTY_READ | PROPERTY_NOTIFY` and
`setAccessPermissions(ESP_GATT_PERM_READ_ENC_MITM)` — so **an encrypted,
MITM-protected link is mandatory**. The client must bond, not merely connect.

Devices may disconnect after inactivity; the docs explicitly say implement
auto-reconnect with exponential backoff.

## Command codes

From `examples/companion_radio/MyMesh.cpp:6`:

| # | Command | Used by bridge |
|---|---|---|
| 1 | `CMD_APP_START` | yes — handshake |
| 2 | `CMD_SEND_TXT_MSG` | yes — send a DM |
| 3 | `CMD_SEND_CHANNEL_TXT_MSG` | yes — send to a channel |
| 4 | `CMD_GET_CONTACTS` | optional (takes a `since` param for incremental sync) |
| 5 | `CMD_GET_DEVICE_TIME` | |
| 6 | `CMD_SET_DEVICE_TIME` | |
| 7 | `CMD_SEND_SELF_ADVERT` | |
| 8 | `CMD_SET_ADVERT_NAME` | |
| 9 | `CMD_ADD_UPDATE_CONTACT` | |
| 10 | `CMD_SYNC_NEXT_MESSAGE` | yes — pull a waiting message |
| 11 | `CMD_SET_RADIO_PARAMS` | |
| 12 | `CMD_SET_RADIO_TX_POWER` | |
| 13 | `CMD_RESET_PATH` | |
| 14 | `CMD_SET_ADVERT_LATLON` | |
| 15 | `CMD_REMOVE_CONTACT` | |
| 16 | `CMD_SHARE_CONTACT` | |
| 17 | `CMD_EXPORT_CONTACT` | |
| 18 | `CMD_IMPORT_CONTACT` | |
| 19 | `CMD_REBOOT` | |
| 20 | `CMD_GET_BATT_AND_STORAGE` | maybe — health reporting |
| 21 | `CMD_SET_TUNING_PARAMS` | |
| 22 | `CMD_DEVICE_QUERY` | yes — capability/version check |
| 23 | `CMD_EXPORT_PRIVATE_KEY` | **see below** |
| 24 | `CMD_IMPORT_PRIVATE_KEY` | |
| 25 | `CMD_SEND_RAW_DATA` | |
| 26 | `CMD_SEND_LOGIN` | yes — room server auth |
| 27 | `CMD_SEND_STATUS_REQ` | |
| 28 | `CMD_HAS_CONNECTION` | |
| 29 | `CMD_LOGOUT` | |
| 30 | `CMD_GET_CONTACT_BY_KEY` | yes — classify a sender on demand |

### Back up the companion's identity

`CMD_EXPORT_PRIVATE_KEY` (23) means the companion's identity can be exported
over the protocol, with no flash dumping needed. `meshcore-cli` or the phone app
can do it.

Worth doing regardless of this project. A private key cannot be derived from a
public key, so a wiped identity with no backup is gone permanently: the node
becomes a different node to the entire mesh, and every contact anyone saved for
it stops working.

## Classifying an incoming message

Two steps. First, **byte 0** separates DMs from channel messages:

| Byte 0 | Meaning | Source id |
|---|---|---|
| `0x07` | contact message | 6-byte pubkey prefix |
| `0x10` | contact message, V3 format | 6-byte pubkey prefix |
| `0x08` | channel message | channel index 0–7 |
| `0x11` | channel message, V3 format | channel index 0–7 |

The V3 formats (`0x10`/`0x11`) are the same payload with a leading signed SNR
byte plus 2 reserved bytes. **Parse both.**

```
PACKET_CONTACT_MSG_RECV (0x07)
  0      0x07
  1-6    pubkey prefix (6 bytes)
  7      path length
  8      text type
  9-12   timestamp (uint32 LE)
  13-16  signature (only if txt_type == 2)
  17+    UTF-8 text

PACKET_CONTACT_MSG_RECV_V3 (0x10)
  0      0x10
  1      SNR (signed, ×4)
  2-3    reserved
  4-9    pubkey prefix
  10     path length
  11     text type
  12-15  timestamp (uint32 LE)
  16-19  signature (only if txt_type == 2)
  20+    UTF-8 text

PACKET_CHANNEL_MSG_RECV (0x08)
  0      0x08
  1      channel index (0-7)
  2      path length
  3      text type
  4-7    timestamp (uint32 LE)
  8+     UTF-8 text

PACKET_CHANNEL_MSG_RECV_V3 (0x11)
  0      0x11
  1      SNR (signed, ×4)
  2-3    reserved
  4      channel index
  5      path length
  6      text type
  7-10   timestamp (uint32 LE)
  11+    UTF-8 text
```

Second, for contact messages, a **person vs a room server** is decided by the
contact's `type` field (`ContactInfo.h:11`), fetched with
`CMD_GET_CONTACT_BY_KEY`. From `src/helpers/AdvertDataHelpers.h:7`:

```
ADV_TYPE_NONE      0
ADV_TYPE_CHAT      1   ← a person
ADV_TYPE_REPEATER  2
ADV_TYPE_ROOM      3   ← room server
ADV_TYPE_SENSOR    4
```

Routing decision:

```
byte0 in {0x08,0x11}                    → channel index N  → #channel-N
byte0 in {0x07,0x10} → lookup prefix:
    type == ADV_TYPE_ROOM               → room server      → auto-create
    type == ADV_TYPE_CHAT               → person           → inbox until replied
    unknown / not a contact             → stranger         → #global-inbox
```

The 6-byte prefix is 48 bits — fine for routing, **not** proof of identity.
Resolve the full key with `CMD_GET_CONTACT_BY_KEY` before acting on content.

## Push codes

Asynchronous, not tied to a command:

- `PUSH_CODE_MSG_WAITING` = `0x83` → send `CMD_SYNC_NEXT_MESSAGE` to fetch it
- `PUSH_CODE_ADVERT` → a contact was seen; good moment to refresh a cache

Response codes (`RESP_CODE_*`) are direct replies to commands. `PACKET_OK` =
`0x00`, `PACKET_ERROR` = `0x01`. Byte values are authoritative; names are
aliases.

## Sending

- **133 character limit** per message (MeshCore spec). Chunk longer text and
  include an indicator like `[1/3]`.
- Commands must be sequenced — don't pipeline; wait for a response or timeout.
  Docs suggest a **5 second** timeout per command and a command queue.
- Dedupe received messages. The docs warn explicitly that resync can deliver
  the same message twice.

## WiFi companion alternative (not chosen)

`Heltec_v3_companion_radio_wifi` serves the same protocol over **TCP port 5000**
(`examples/companion_radio/main.cpp:37`), which would let any machine bridge with
`MeshCore.create_tcp(ip, 5000)` and no BLE at all. Rejected because
`WIFI_SSID`/`WIFI_PWD` are **compile-time** build flags (FAQ §7.6) — it requires
building firmware yourself, and reflashing the V3.

Note reflashing would *not* lose data — firmware lives in the app partition,
data in SPIFFS — but it's moot since BLE works without touching the V3.
