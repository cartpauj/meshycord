# MeshyCord

A MeshCore ↔ Discord bridge. Messages from a LoRa mesh appear in Discord
channels; replies typed in Discord go back out over the radio.

```
  MeshCore node  ──USB / BLE / TCP──  MeshyCord  ──websocket──  Discord
     (LoRa)                          (Pi, or any                (Gateway)
                                      Linux box)
```

One static binary. No runtime to install, no virtualenv, no npm, no separate
daemon. It owns the radio link, the Discord Gateway, the routing, the message
history and its own web console.

Runs on a **Raspberry Pi Zero W** (ARMv6) upward — Pi Zero 2 W, Pi 2/3/4/5, and
any amd64 or i386 Linux box.

> This is the successor to the ESP32-C3 firmware, which is preserved in full on
> the `esp32-c3` branch and still runs on hardware. That version polls Discord's
> REST API because an ESP32 cannot comfortably hold a websocket open, and almost
> every limitation it has traces back to that one constraint. See
> [What changed, and why](#what-changed-and-why).

---

## Install

### Debian, Ubuntu, Raspberry Pi OS

Download the `.deb` for your architecture from
[Releases](https://github.com/cartpauj/meshycord/releases) and install it:

```sh
sudo dpkg -i meshycord_*_arm64.deb        # Pi 3/4/5, 64-bit OS
sudo dpkg -i meshycord_*_armhf.deb        # Pi 2/3/4 on a 32-bit OS
sudo dpkg -i meshycord_*_armhf-armv6.deb  # Pi Zero W, Pi 1  ← note the suffix
sudo dpkg -i meshycord_*_amd64.deb        # any x86-64 Linux box
```

**Pi Zero W and Pi 1 need the `-armv6` package.** Both it and the ARMv7 build
are `armhf`, so the filename is the only thing telling them apart. The ARMv7
binary will not run on an ARMv6 chip.

Fedora and RHEL: use the matching `.rpm`. Anything else: take the `.tar.gz`,
drop the binary in `/usr/bin`, and copy `deploy/meshycord.service`.

The service starts automatically and listens on port **9150** — for the 915 MHz LoRa band.

### From source

```sh
git clone https://github.com/cartpauj/meshycord
cd meshycord
CGO_ENABLED=0 go build -o meshycord ./cmd/meshycord
```

Go 1.26 or newer. `CGO_ENABLED=0` is not a habit — it is what makes the binary
static and every dependency pure Go, which is what makes ARMv6 a build flag
rather than a cross-toolchain problem.

---

## Setup

### 1. Make a Discord bot

1. [Discord Developer Portal](https://discord.com/developers/applications) →
   **New Application**.
2. **Bot** → **Reset Token**, and copy it. This is the one secret involved.
3. Still under **Bot**, turn **Message Content Intent** ON.
   Without it every message arrives blank, which looks like a bug in the bridge
   and is not.
4. Turn **Public Bot** OFF, and set the Install Link to **None**. This bot is
   yours; nobody else should be able to add it anywhere.
5. **OAuth2 → URL Generator**: scope `bot`, permissions **View Channels**,
   **Send Messages**, **Read Message History**, **Manage Channels**,
   **Manage Messages**. Open the generated URL and add it to your server.

`Manage Channels` is what lets it create the channels and categories for you.
`Manage Messages` is only used to delete a typed room-server password out of
channel history — and with the `/mesh login` popup, it should never need to.

### 2. Point it at your server

Open `http://<the-machine>:9150` and fill in the bot token and your server ID.

To find the server ID: open any channel in your server; the first long number in
the browser URL is it. It is asked for rather than guessed because inferring it
from the bot's membership only works when the bot is in exactly one server —
which is a silent failure for anyone running it in two.

**Set a console password** on the same page. There is no default, because a
default password is worse than none: it looks secure and is not. Until one is
set, the console is open to anyone who can reach the machine, and every page
says so.

All of this also works from the shell, for a headless install:

```sh
sudo meshycord -db /var/lib/meshycord/db.sqlite -set-token 'YOUR.BOT.TOKEN'
sudo meshycord -db /var/lib/meshycord/db.sqlite -set-guild 123456789012345678
sudo meshycord -db /var/lib/meshycord/db.sqlite -set-password 'something long'
sudo systemctl restart meshycord
```

### 3. Connect the radio

**USB serial is the recommended way.** If the box sits next to the radio there
is no reason to use anything else: no pairing, no PIN, no bonding, no dropped
links, and no BlueZ. Plug the node in and it is found automatically.

```sh
meshycord -list-ports     # what it can see
```

Prefer a `/dev/serial/by-id/…` entry if you pin one manually — it survives
reboots and does not shuffle when another USB device appears.

**Bluetooth LE** is for when the bridge is not next to the radio. It works, and
it is the flakiest of the three. The node **must have a fixed pairing PIN set**
in the MeshCore phone app: without one it generates a random PIN each boot and
displays it on its own screen, which a headless bridge cannot read. Set the same
PIN in Settings.

**TCP** talks to MeshCore's WiFi companion firmware on port 5000. It needs you
to build and flash that firmware yourself, but afterwards the bridge can run
anywhere on the network.

> **One client at a time.** While MeshyCord holds the link, the MeshCore phone
> app cannot connect to the same node. That is MeshCore, not this bridge, and it
> applies to all three transports.

---

## Using it

Once it is running, your Discord server grows a shape:

```
MeshyCord
  #meshycord-admin     type commands here
  #global-inbox        traffic from senders with no channel yet
Channels
  #public              your mesh channels
Room Servers
  #ridge-room          one per linked room server
Companion DMs
  #alice               one per linked person
```

Type in a linked channel and it goes out over the radio. Each message gets a
marker saying what happened:

| | |
|---|---|
| ⏳ | on the air, waiting for the recipient's node to confirm |
| ✅ | the recipient's node acknowledged it |
| 📡 | transmitted — no acknowledgement is possible |
| ❌ | rejected, or no acknowledgement before the deadline |
| 🔄 | *you* add this to your own failed message to ask for a resend |

The difference between ✅ and 📡 is not decoration: **MeshCore cannot
acknowledge group messages at all.** A channel send can only ever be reported as
transmitted, and a tick there would claim a delivery the protocol is incapable of
proving.

**Room servers do acknowledge posts** — but only from an account with
READ_WRITE or better. A guest is admitted and then has every post discarded
with the acknowledgement deliberately withheld, which is indistinguishable from
a delivery failure. The bridge reads the ACL byte from the login and says so
outright rather than letting you wonder.

The bridge also logs in again whenever the session is older than the window in
Settings (five minutes by default; 0 logs in before every message, which costs
a round trip per post).

### Commands

Use `/mesh` in Discord, or type the same commands in `#meshycord-admin`. Both go
through the same code, so they cannot drift apart.

```
/mesh status            radio, Discord, links, traffic
/mesh list              room servers, companions, channels, links, repeaters
/mesh find <text>       search by name or key
/mesh link <target>     give something its own Discord channel
/mesh unlink <target>   remove a link (the channel is left alone)
/mesh login             set a room-server password in a private popup
/mesh tidy              drop links whose channel or mesh slot is gone
/mesh sync-rooms        link every known room server (asks first)
/mesh contact-add       add a node from its full public key
/mesh contact-find      search every contact on the radio
/mesh contact-remove    delete a contact from the radio
/mesh reset             delete everything the bridge created (asks first)
```

`list` and `find` **freeze their numbering for ten minutes**, so `add 7` always
means the row you saw — contacts adverting in afterwards cannot shift it.

### Resending

Add the 🔄 reaction **to the message you want resent** — your own message, the
one carrying the ❌ — and it goes out again. Reacting to one numbered `[2/3]`
piece of a split message resends exactly that piece.

The reaction is consumed as soon as it registers, so it vanishes and is
replaced by ⏳ while the bridge works. That is deliberate: it means a second
press works, where a reaction left in place would be ignored by Discord as
already present.

Replying `retry` used to do the same thing — that was the ESP32's only option,
since reactions cannot be polled for. It is commented out rather than deleted
while the reaction path is tried on its own; note that with it off, the words
"retry" and "resend" are ordinary text and get transmitted like anything else.

### Promoting a sender

A key prefix shown next to a message — in the inbox, or on a room post — can be
given its own channel without leaving the conversation:

```
!promote a1b2c3d4e5f6
```

Same thing as `/mesh link`, just closer to hand.

### Mentioning someone

Don't use Discord's `@` autocomplete — it puts a raw account id in the message,
which is 21 bytes of a 133-byte budget and means nothing on the radio. A Discord
account is not a mesh node, so there is nothing sensible to translate it into.

MeshCore's own convention works, because it is plain text and passes through
untouched:

```
@[Ridge Cabin] are you still up there?
```

### Routing overrides

Prefix a message in a linked channel:

```
path:flood <text>     clear the stored path first, so this floods
path:direct <text>    use the stored path (the default)
```

The prefix costs none of the 133-byte budget and the recipient never sees it.
Only meaningful for direct messages and room servers — channel messages are
always flooded.

There is no per-message route flag in MeshCore; the node picks flood when a
contact has no stored path and direct otherwise, so `path:flood` works by
clearing the path. It is relearned from the reply, so the effect is for that
message rather than permanent.

### Long messages

A mesh message is **160 bytes** (`MAX_TEXT_LEN` in the firmware). On a group
channel the node prepends its own name and silently truncates to fit, so the
usable text there is `160 - len(node_name) - 2` and the bridge splits against
that smaller budget. Longer text is split into up to three
transmissions, about two seconds apart, each echoed into Discord as its own
message and tracked separately — so you can see how much airtime you used and
which pieces actually landed.

Beyond that limit it is **refused, not truncated**. Silent truncation was the
worst of the available options: it looked like it sent.

### Room servers

A room server refuses posts from anyone not logged in, and it does so
**silently** — the send succeeds and the post simply never appears. MeshyCord
refuses up front instead, and offers a button. Press it, type the password into
the popup, and it never enters channel history at all.

The password is kept so the session can be re-established after a reconnect,
which happens automatically: the companion protocol does not forward the
server's keep-alive interval, so there is no expiry to schedule against.

---

## The web console

`http://<the-machine>:9150`

- **Dashboard** — both links, live, refreshing itself
- **Messages** — full searchable history, both directions, with hop count,
  signal and delivery state
- **Links** — what is bridged to what; link and unlink
- **Contacts** — everything the radio knows, including repeaters and sensors;
  add or remove contacts
- **Activity** — what the bridge has done to your server, so "why does this
  channel exist" has an answer months later
- **Settings** — everything above

The console is deliberately not the only way to drive this. It is not reachable
when you are away from home, and the radio is — which is why the Discord admin
console exists and is complete.

---

## Configuration

Everything lives in SQLite at `/var/lib/meshycord/db.sqlite`, mode `0600`,
in a root-owned `0700` directory. It holds the bot token and every room
password, so nothing else on the box has any business reading it.

**Nothing secret is ever compiled in.** The same binary is safe to hand to
anyone, and reinstalling does not leak credentials.

Command-line flags:

```
-listen 0.0.0.0:9150               console address (9150 = the 915 MHz LoRa band)
-db /var/lib/meshycord/db.sqlite   database path
-log-level info                    debug | info | warn | error
-list-ports                        list serial devices and exit
-version
```

### Auto-create policy

Off by default except channels, and the reason is a real abuse surface:

| | Default | Why |
|---|---|---|
| Mesh channels | **on** | Your node's channels are already yours. No risk. |
| Room servers | off | There are often dozens; it fills a server and hits Discord's 50-per-category limit. |
| Direct messages | off | Anyone who has heard your advert can DM you. |

Even with DM auto-create on, it only fires for senders already in the radio's
contact list. **An unknown sender always goes to the inbox** — an unknown sender
cannot be classified or named, and that is the guard the whole policy rests on.

---

## Operating

```sh
systemctl status meshycord     # both links at a glance
journalctl -u meshycord -f     # logs
systemctl restart meshycord
```

The unit is `Type=notify` with a 120-second watchdog. If the process wedges,
systemd restarts it — there is no watchdog scaffolding inside the application,
because that approach is precisely what failed on the ESP32.

The `systemctl status` line reports live state:

```
Status: "radio up, discord up, 6 links, 41 messages today"
```

### Things that go wrong

**"The bot sees messages but they are all blank."**
Message Content Intent is off. Developer Portal → Bot → Privileged Gateway
Intents. The console says so on every page when Discord tells us.

**"Connected to the radio over BLE, but nothing works."**
The link came up encrypted but *unauthenticated*, and the companion rejects
writes on such a link. Usually a stale bond after a PIN change — remove the
bond (`bluetoothctl remove <MAC>`) and let it re-pair.

**"A channel I deleted keeps coming back"** — the admin channel and the inbox
are rebuilt on purpose; without an inbox, unrouted messages are silently
dropped. Ordinary linked channels stay deleted, and their link is dropped with
them.

**"My node is on the mesh but I cannot message it."**
It has to be in the radio's contact list. If its adverts do not reach you, add
it from its full public key with `/mesh contact-add` — the 12-character prefix
shown next to a message is not enough.

### Backups

The database is the whole state. Stop the service, copy it, start it again:

```sh
sudo systemctl stop meshycord
sudo cp /var/lib/meshycord/db.sqlite ~/meshycord-backup.sqlite
sudo systemctl start meshycord
```

Separately, and more importantly: **back up your node's private key.**
`CMD_EXPORT_PRIVATE_KEY` means the identity can be exported over the protocol
with no flash dumping — `meshcore-cli` or the phone app will do it. A private
key cannot be derived from a public one, so a wiped identity with no backup is
gone permanently: the node becomes a different node to the entire mesh, and
every contact anyone saved for it stops working. Worth doing regardless of this
project.

---

## What changed, and why

The ESP32 version is not being abandoned because the code is bad. It ran on real
hardware against a live ~350-node mesh. It is being left behind because of one
hard ceiling: **an ESP32 cannot comfortably hold a websocket open, so it polls
Discord's REST API.** Almost everything else follows from that.

| | ESP32-C3 | This |
|---|---|---|
| Discord | REST polling | Gateway websocket |
| Reply latency | 17–22 s sweep of 5 channels | instant |
| Cost of more links | grows per link | flat |
| Reactions | **impossible** — Gateway-only, no endpoint to poll | events; 🔄 resends |
| Buttons, modals, ephemeral replies | **impossible** | yes |
| Room passwords | typed in a channel, then deleted | private popup |
| History | 256-message ring on the radio | SQLite, searchable |
| Contact cache | capped at 192, repeaters dropped | uncapped, everything |
| Web UI | cut to almost nothing for memory | full console |
| Transports | BLE only | serial, BLE, TCP |
| Recovery | hardware watchdog, fed by hand | systemd, `Restart=always` |

Two of those deserve more than a table row.

**Reactions cannot be received over REST.** Not "are slow to" — cannot. The
complete webhook event list is twelve types, all about app authorization,
entitlements, quests and Social-SDK lobbies. Discord's own wording is that
events for channels, guilds, roles and messages "are only available over
Gateway". There is no endpoint to poll for pending interactions.

**The ESP32 had an unfixed hang.** As of its last commit the device
watchdog-rebooted roughly 170 s after boot. The root cause is structural: one
`loop()` where every operation blocks every other one, so a slow Discord request
stalls the radio and a slow radio stalls Discord. This design makes that failure
mode impossible — four independent goroutines, none of which can block another,
and supervision moved outside the process entirely.

### What did *not* change

These are MeshCore, not the old hardware, and no amount of new hardware fixes
them:

- The companion serves **one client at a time**. The phone app cannot connect
  while the bridge holds the link.
- **133 bytes** per message.
- Group messages **cannot be acknowledged**, so delivery is never confirmable
  for those.

---

## Design notes

**Five dependencies, all pure Go.** `golang.org/x/sys` (termios),
`golang.org/x/crypto` (bcrypt), `modernc.org/sqlite`, `github.com/coder/websocket`,
`tinygo.org/x/bluetooth`. No Discord library: REST is hand-written on
`net/http` and the Gateway protocol implemented directly. The ESP32 version
hand-wrote REST and that part was never the problem — the standard library gives
TLS, keep-alive, connection pooling and JSON for free, so what is left is small
enough to read in one sitting.

`modernc.org/sqlite` rather than `mattn/go-sqlite3` specifically because the
latter needs cgo, which would make the ARMv6 build a cross-toolchain problem
instead of a build flag.

**Layers, so the protocol logic is testable without hardware.**

```
internal/meshcore   transports + protocol codec (pure, unit-tested)
internal/discord    REST client + Gateway client
internal/bridge     routing, chunking, receipts, room sessions, admin console
internal/store      SQLite
internal/config     typed settings
internal/server     web console
internal/web        embedded templates and assets
```

`internal/meshcore/frames.go` has no I/O in it at all, and every hard-won
protocol gotcha has a test pinning it down. `client_test.go` exercises the whole
session — command sequencing, push filtering, contact enumeration, reply
matching — against a fake radio that lives in memory.

**Routing is by public-key prefix or channel slot, never by channel name.**
Renaming a Discord channel can never break anything. Categories are the one
exception, matched by name, because Discord offers no other way to find one.

**Nothing is adopted, only created.** The bridge never claims or moves a channel
it did not make. An earlier version moved one unconditionally and dragged a
repurposed `#general` into its own category.

**SD-card longevity is taken seriously.** WAL mode, `synchronous=NORMAL`, writes
only when something actually changed, logs to journald rather than a file, and
none of the ESP32's per-poll persistence.

See [PROTOCOL-NOTES.md](PROTOCOL-NOTES.md) for the companion protocol itself —
byte layouts with firmware line references, and every gotcha that cost real
debugging.

---

## Licence

GPL v3. See [LICENSE](LICENSE).
