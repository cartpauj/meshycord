# MeshCore companion protocol — notes for the bridge

Everything here was read out of the MeshCore source at commit `727fc05`
(v1.17.0). Authoritative reference is `docs/companion_protocol.md` in
<https://github.com/meshcore-dev/MeshCore> (990 lines).

Official client libraries exist and are worth reading as reference
implementations: `meshcore_py`, `meshcore.js`, `meshcore-cli`
(all under the `meshcore-dev` org).

**Where this lives in the code.** The codec is `internal/meshcore/frames.go` —
pure functions, no I/O, unit-tested in `frames_test.go`. Every gotcha below has
a test pinning it down, which is a cheaper way to keep them fixed than
rediscovering them. The three transports are behind one interface in
`transport.go`, so the protocol logic can be exercised against a fake radio
(`client_test.go`) with no hardware in the room.

## Transports

There are three, and they differ in exactly one respect: **framing**.

| Transport | Framing |
| --- | --- |
| BLE | One notification is one frame. Free. |
| USB serial | Length-prefixed, see below |
| TCP :5000 | Length-prefixed, identical to serial |

### Stream framing (serial and TCP)

A byte stream has no frame boundaries, so MeshCore length-prefixes everything:

```
to the node:    '>' [length u16 LE] [payload]
from the node:  '<' [length u16 LE] [payload]
```

This is what the official `meshcore_py` and `meshcore.js` clients speak, and it
is the same for USB serial and for the TCP companion on port 5000.

Two practical notes for a serial reader:

- **Resynchronise, do not die.** The node may have been mid-frame when the
  bridge attached, some firmware prints a plain-text boot banner on the same
  port, and USB occasionally eats a byte. Scan forward for the next `<` rather
  than treating a surprise as fatal.
- **Sanity-check the length.** A bogus length means payload bytes were mistaken
  for a header. Drop it and keep scanning, or the reader blocks forever on a
  read that will never be satisfied.

### Serial is the better default

The ESP32 version had no choice about BLE. A Linux box next to the radio does,
and serial wins on every axis that matters: no pairing, no PIN, no bonding, no
dropped links, no BlueZ, and raw termios is the only OS-specific part.

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

### Authenticated bonding on BlueZ

This was the largest unknown in the plan, and it resolves cleanly. On the ESP32,
NimBLE took a passkey callback directly. On Linux, pairing is `bluetoothd`'s
job: it asks a registered **agent** over D-Bus for whatever the pairing needs.

The capability string matters more than it looks:

| Capability | Result |
| --- | --- |
| `KeyboardOnly` | passkey entry → an **authenticated** link ✅ |
| `NoInputNoOutput` | Just Works → encrypted but **never authenticated** ❌ |

`NoInputNoOutput` is the trap. The link comes up, everything looks connected,
and the companion then rejects every write — which presents as "connected fine,
nothing works". This is the exact same distinction as the ESP32's
`BLE_HS_IO_KEYBOARD_ONLY` versus `NO_INPUT_OUTPUT`.

So the bridge registers an `org.bluez.Agent1` at `/org/meshycord/agent` with
capability `KeyboardOnly`, calls `RequestDefaultAgent` (otherwise `bluetoothd`
may hand the request to a desktop agent that is not there on a headless Pi),
and answers `RequestPasskey` with the configured PIN. Every Agent1 method must
be exported even the ones a `KeyboardOnly` agent never sees, or `bluetoothd`
fails mid-pairing with `UnknownMethod`.

Two more things worth not rediscovering:

- **Mark the device `Trusted` after pairing**, or `bluetoothd` asks for
  authorisation on every later reconnect and nobody is there to answer.
- **A stale bond is reused forever.** Change the PIN on the node and the bridge
  keeps retrying the dead bond indefinitely. `RemoveDevice` on the adapter is
  the only fix; the ESP32 hit this too and solved it by purging bonds after
  repeated auth failures.

The library situation is worth stating plainly: of the four Go BLE libraries,
two are dead (`muka/go-bluetooth` archived, `go-ble/ble` untouched since Mar
2024). Only `tinygo.org/x/bluetooth` is maintained, and it has no pairing API at
all — hence the raw D-Bus agent above. It is pure Go on Linux via `godbus`; its
cgo dependencies are macOS and Windows only, so `CGO_ENABLED=0` still holds.

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

**The single most important rule in this protocol: anything whose first byte is
≥ `0x80` is an asynchronous push, and is NEVER a reply to anything.**

Letting pushes into the response queue caused three separate bugs on the ESP32,
each presenting as something entirely unrelated being subtly wrong — a lost
contact enumeration (`0x88 PUSH_CODE_LOG_RX_DATA`) and, before that, a channel
lookup answered by the wrong frame. The Go port enforces this structurally: the
read loop routes on the top bit before anything else can see the frame, so the
bug class cannot recur.

The full set, read from `main` during the port:

```
0x80 ADVERT               0x88 LOG_RX_DATA
0x81 PATH_UPDATED         0x89 TRACE_DATA
0x82 SEND_CONFIRMED       0x8A NEW_ADVERT
0x83 MSG_WAITING          0x8B TELEMETRY_RESPONSE
0x84 RAW_DATA             0x8C BINARY_RESPONSE
0x85 LOGIN_SUCCESS        0x8D PATH_DISCOVERY_RESPONSE
0x86 LOGIN_FAIL           0x8E CONTROL_DATA
0x87 STATUS_RESPONSE      0x8F CONTACT_DELETED
                          0x90 CONTACTS_FULL
```

The four the bridge acts on:

- `0x83 MSG_WAITING` → send `CMD_SYNC_NEXT_MESSAGE` to fetch it. It is a
  coalescing signal, not a count: the node keeps the flag set until it answers
  `NO_MORE_MESSAGES`, so one wake-up drains a whole backlog.
- `0x82 SEND_CONFIRMED` → `[ack u32][round-trip ms u32]`. The only actual proof
  a direct message was delivered.
- `0x85` / `0x86` → a room-server login verdict. See below.
- `0x80` / `0x8A` ADVERT → a contact was heard; a good moment to refresh a
  cache, and nothing more than that.

`0x90 CONTACTS_FULL` is worth surfacing loudly: the node has stopped learning
new contacts, which silently degrades routing.

Response codes (`RESP_CODE_*`) are direct replies to commands. `PACKET_OK` =
`0x00`, `PACKET_ERROR` = `0x01`. Byte values are authoritative; names are
aliases.

### A room login can succeed and still refuse to let you post

The login verdict carries two permission fields, and the obvious one is the
wrong one. From the companion (`MyMesh.cpp:696`):

```c
out_frame[1]  = data[6];   // legacy "is_admin" — 0 for every non-admin
out_frame[12] = data[7];   // NEW (v7): ACL permissions  ← the real role
```

and the room fills them in (`simple_room_server/MyMesh.cpp:388`):

```cpp
reply_data[6] = (client->isAdmin() ? 1 : (client->permissions == 0 ? 2 : 0));
reply_data[7] = client->permissions;   // NEW
```

Roles are `src/helpers/ClientACL.h`: `GUEST 0`, `READ_ONLY 1`, `READ_WRITE 2`,
`ADMIN 3`. The room's admin password grants ADMIN, its room password grants
READ_WRITE, and anything else grants GUEST when `allow_read_only` is set —
otherwise the room **does not reply at all** and the client times out.

The part that matters: a GUEST is admitted, and then every post is discarded
**and the acknowledgement deliberately withheld**
(`simple_room_server/MyMesh.cpp:481`):

```cpp
} else { // TXT_TYPE_PLAIN
  if ((client->permissions & PERM_ACL_ROLE_MASK) == PERM_ACL_GUEST) {
    temp[5] = 0;      // no reply
    send_ack = false; // no ACK
  } else {
    if (!is_retry) addPost(client, (const char *)&data[5]);
    send_ack = true;
  }
}
```

So "logged in" and "may post" are different questions, and a guest looks
identical to a delivery failure unless byte 12 is read. Room servers **do**
acknowledge posts — from anyone with READ_WRITE or better.

### A login is answered by a push, not by the command reply

`CMD_SEND_LOGIN` replies `RESP_CODE_SENT`, which means only "the request went
out over the air". The verdict arrives seconds later as `0x85` or `0x86`, both
carrying the room's 6-byte key prefix at the same offset:

```
0x85  [perms 1][pubkey prefix 6][server time 4][acl 1][fw level 1]
0x86  [reserved 1][pubkey prefix 6]
```

Treating the immediate reply as success reports **every** login as working,
including the failed ones.

## More gotchas, each of which cost real debugging

- **Match a reply to its request.** `CMD_GET_CHANNEL` echoes the channel index
  in byte 1 — check it. A timed-out query for slot 1 answered during the query
  for slot 2 gave slot 2 the wrong name, and produced a duplicate Discord
  channel for a slot that was never real.
- **Some commands need the full 32-byte public key**, not the 6-byte prefix
  that arrives with a message: `GET_CONTACT_BY_KEY`, `SEND_LOGIN`,
  `REMOVE_CONTACT`, `RESET_PATH`, `LOGOUT`. Note the circularity —
  `GET_CONTACT_BY_KEY` cannot resolve a prefix, because it needs the full key
  itself. Prefixes are only resolvable against a cache built by enumerating
  `CMD_GET_CONTACTS`.
- **The path byte is packed, not a count.** Bits 0–5 hold the hop count, bits
  6–7 the per-hop hash size (`Packet.h:79`); the whole byte is `0xFF` for a
  direct route. Printing it raw showed "71 hops" for what was actually 7. The
  docs call it "Path Length", which reads like an integer.
- **`txt_type == 2` does not carry a signature.** Those 4 bytes are the
  original author's public-key prefix — how a room server identifies who wrote
  a post.
- **`ROUTE_TYPE_DIRECT` means "a path was supplied", not "the sender was
  nearby".** A station hundreds of miles away with a stored route sends
  direct-routed packets. Wording matters in any UI: genuine adjacency is a
  *flooded* packet with zero hops accumulated.
- **Posting to a room server without a session fails silently** — the send
  succeeds and the post simply never appears. Refuse up front instead.
- **The room server's keep-alive interval is not forwarded** to the client, so
  there is no expiry to schedule against. Re-establish sessions on events:
  whenever the link returns, and whenever a post finds no session.
- **There is no per-message route flag.** The node picks flood when a contact
  has no stored path and direct otherwise (`BaseChatMesh.cpp:449`), so forcing
  a flood means clearing the stored path with `CMD_RESET_PATH`. It is relearned
  from the reply, so the effect is for that message rather than permanent.
- **Only one copy of each received message is visible.** MeshCore deduplicates
  by packet hash below the app layer, so the reported hop count describes one
  path, not all of them.
- **MeshCore terminology**: *route* is the strategy (`ROUTE_TYPE_FLOOD` /
  `ROUTE_TYPE_DIRECT`); *path* is the stored hop list (`path_len`, `path[]`).
  The companion protocol uses "path" throughout and never names a field
  "route".

## `CMD_SET_CHANNEL` (0x20)

Not used by the ESP32 version, and worth having: it creates or updates a
channel on the node.

```
[0x20][channel_idx][name 32][secret 16]
```

An empty name with an all-zero secret deletes the slot. This means a private
channel can be added without the phone app.

## Sending

- **160 bytes per message**, not 133. `MAX_TEXT_LEN` in
  `src/helpers/BaseChatMesh.h` is `10 * CIPHER_BLOCK_SIZE` = 160, and
  `BaseChatMesh.cpp:463` refuses anything longer. The 133 in earlier notes was
  simply wrong and cost about 20% of every transmission.
- **A group message pays for the node's own name.** `sendGroupMessage`
  (`BaseChatMesh.cpp:487`) builds `"<name>: <text>"` and then silently
  truncates the result to `MAX_TEXT_LEN`:

  ```cpp
  if (text_len + prefix_len > MAX_TEXT_LEN) text_len = MAX_TEXT_LEN - prefix_len;
  ```

  So the usable text on a channel is `160 - len(node_name) - 2`, and exceeding
  it loses the tail with no error anywhere. Direct messages carry no prefix and
  get the full 160.
- Commands must be sequenced — don't pipeline; wait for a response or timeout.
  Docs suggest a **5 second** timeout per command and a command queue.
- Dedupe received messages. The docs warn explicitly that resync can deliver
  the same message twice.

## WiFi companion (supported, not the default)

`Heltec_v3_companion_radio_wifi` serves the same protocol over **TCP port 5000**
(`examples/companion_radio/main.cpp:37`), which lets the bridge run anywhere on
the network — a NAS, a spare x86 box — with no BLE and no serial cable.

The cost is that `WIFI_SSID`/`WIFI_PWD` are **compile-time** build flags
(FAQ §7.6), so it means building firmware yourself and reflashing the node.
Reflashing does *not* lose data — firmware lives in the app partition, data in
SPIFFS.

The ESP32 version rejected this because BLE worked without touching the node.
It is supported here because the framing is identical to serial, so it cost
almost nothing to add, and it is the only way to run the bridge somewhere other
than beside the radio without depending on BLE.

## One client at a time

Not a protocol detail so much as a fact of life: **the MeshCore companion
serves one client at a time.** While the bridge holds the link, the phone app
cannot connect to the same node. This is MeshCore, not the transport and not
the chip — it is the same over serial, BLE and TCP, and there is no working
around it.

This is also the whole reason `meshycord-cli` is a client of the daemon rather
than a program that opens the radio itself. There is no second slot to take.

## Remote CLI (`TXT_TYPE_CLI_DATA`)

**There is no command for running a CLI command.** Reading the full command
list in `examples/companion_radio/MyMesh.cpp` (1–65 on `main`) turns up nothing
of the sort, and the companion protocol documentation has no such section
either. What exists instead is a text message with one byte changed.

`CMD_SEND_TXT_MSG` carries a `txt_type`, and the values are:

```
0  TXT_TYPE_PLAIN         chat
1  TXT_TYPE_CLI_DATA      a command line
2  TXT_TYPE_SIGNED_PLAIN  a room post, with the author's key prefix
```

Send `txt_type = 1` to a repeater and it does not store the text as chat: it
runs it through `CommonCLI::handleCommand` and replies with the output as
another `TXT_TYPE_CLI_DATA` message (`simple_repeater/MyMesh.cpp:683`, and
`companion_radio/MyMesh.cpp:534` on the way back in). That is the entire remote
administration mechanism.

Four things about it are not obvious and all four cost real work to establish:

**It needs an ADMIN login, and says nothing when it does not have one.** The
check is `client->isAdmin()` in the same condition that matches the message
type, so a guest session — which logs in perfectly happily — falls through to
nothing at all. No error, no reply, no acknowledgement. Identical, from the
sending end, to a repeater that is out of range. A repeater reports admin in
two places (`reply_data[6]` legacy `is_admin`, `reply_data[7]` ACL), so both
are worth consulting; older firmware fills in only the first.

**Nothing correlates a reply with its command.** No request id, no sequence
number. The output arrives as an inbound message from that node some seconds
later, and the only thing tying it to what was asked is that it came from the
node that was asked. Hence one command in flight per target, enforced by the
bridge — two at once and the answers could be matched up the wrong way round.

**The sender's timestamp is overwritten, which changes what `clock sync`
means.** For a CLI message the companion substitutes its own RTC
(`MyMesh.cpp:1098`) so a wrong client clock cannot trip the far end's replay
protection. The repeater's `clock sync` then sets itself from `sender_timestamp`
— which is the *companion node's* clock, never the bridge machine's. A node
running fast propagates its error to every repeater it syncs, and since no
MeshCore clock will move backwards (`MyMesh.cpp:1240`, and the same rule in
`CommonCLI.cpp:216`), undoing that needs `clkreboot` over USB on each one.
`time <epoch>` takes an explicit value and bypasses both RTCs, which is why
that is the safer command for setting a repeater from a machine with real time.

**Silence is a legitimate outcome.** `reboot` and `poweroff` are gone before a
reply could leave. The firmware also sends nothing when it decides a message is
a retry (`is_retry` → empty reply). So a timeout cannot be reported as failure;
it means "no answer", which is a different thing.

### There is no CLI for the locally attached node

Worth stating plainly, because it is the obvious thing to want. The node on the
end of the USB cable cannot be given CLI commands over the companion protocol —
no firmware version has a command for it. Its text CLI exists only in **CLI
Rescue** mode (`MyMesh.cpp:2020`), which reads lines straight off `Serial`,
which is the same port the binary protocol uses. The two cannot coexist. Setting
that node's name, radio parameters or clock is done with the real commands
(`CMD_SET_ADVERT_NAME`, `CMD_SET_RADIO_PARAMS`, `CMD_SET_DEVICE_TIME`, …).
