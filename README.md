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

(A second small binary, `meshycord-cli`, ships alongside it for
[administering repeaters from the shell](#administering-a-repeater-from-the-shell).
It is a client of the one above, not another daemon — it holds nothing and
does nothing on its own.)

Runs on a **Raspberry Pi Zero W** (ARMv6) upward — Pi Zero 2 W, Pi 2/3/4/5, and
any amd64 or i386 Linux box.

```sh
curl -fsSL https://raw.githubusercontent.com/cartpauj/meshycord/main/install.sh | sudo sh
```

That installs the latest release and starts it. **Run the same line again to
update** — it always fetches the latest, and your settings, links and message
history stay where they are.

Then open `http://<the-machine>:9150` and sign in:

| | |
|---|---|
| username | `admin` |
| password | `admin` |

> **Change that password before anything else.** It is the same on every
> install and it is written above, so until you change it, anyone who can reach
> the machine can read your message history and your Discord bot token. The
> console warns on every page until you do. Settings → Console login, or
> `sudo meshycord -db /var/lib/meshycord/db.sqlite -set-password 'something long'`

<details>
<summary>What that command does, and how to pin or undo it</summary>

It works out what the machine is, downloads the matching package from
[Releases](https://github.com/cartpauj/meshycord/releases), checks it against the
release's `SHA256SUMS`, and installs it with `dpkg` or `rpm` — falling back to the
plain binary plus a systemd unit on distributions with neither.

Reading a script before piping it into root is a good habit:

```sh
curl -fsSL https://raw.githubusercontent.com/cartpauj/meshycord/main/install.sh -o install.sh
less install.sh
sudo sh install.sh
```

```sh
MESHYCORD_DRY_RUN=1 sh install.sh      # say what would happen, change nothing
MESHYCORD_VERSION=v0.0.1 sudo sh install.sh   # pin a version, or go back to one
MESHYCORD_METHOD=tar sudo sh install.sh       # force deb | rpm | tar
```

To remove it, with the settings and history kept:

```sh
sudo apt-get remove meshycord     # or: sudo rpm -e meshycord
```

Add `sudo rm -rf /var/lib/meshycord` to delete the database too. Neither touches
anything in Discord — for that, `/mesh reset` first.

</details>

---

## Install by hand

### Debian, Ubuntu, Raspberry Pi OS

Download the `.deb` for your architecture from
[Releases](https://github.com/cartpauj/meshycord/releases) and install it:

```sh
sudo dpkg -i meshycord_*_arm64.deb        # Pi 3/4/5, 64-bit OS
sudo dpkg -i meshycord_*_armhf.deb        # Pi 2/3/4 on a 32-bit OS
sudo dpkg -i meshycord_*_armhf-armv6.deb  # Pi Zero W, Pi 1  ← note the suffix
sudo dpkg -i meshycord_*_amd64.deb        # any x86-64 Linux box
sudo dpkg -i meshycord_*_i386.deb         # 32-bit x86
```

**Pi Zero W and Pi 1 need the `-armv6` package.** Both it and the ARMv7 build
are `armhf`, so the filename is the only thing telling them apart. The ARMv7
binary will not run on an ARMv6 chip.

Fedora and RHEL: use the matching `.rpm` — `x86_64`, `aarch64`, `armv7hl` or
`i686`. There is no ARMv6 `.rpm`, so on a Pi Zero W use the `.deb` or the
tarball. Anything else: take the `.tar.gz`, drop the binary in `/usr/bin`, and
copy `deploy/meshycord.service`.

The service starts automatically and listens on port **9150** — for the 915 MHz LoRa band.

### From source

```sh
git clone https://github.com/cartpauj/meshycord
cd meshycord
CGO_ENABLED=0 go build -o meshycord ./cmd/meshycord
CGO_ENABLED=0 go build -o meshycord-cli ./cmd/meshycord-cli
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

`Manage Messages` is needed for two things. It deletes a room-server password out
of channel history if you type one there, which the `/mesh login` popup avoids.
More importantly it is how the bridge clears your 🔄 off a message: Discord only
lets an app remove somebody else's reaction with this permission, and it sends no
event for a reaction that is already present — so without it, a 🔄 works once and
every press after that is silently ignored.

### 2. Point it at your server

Open `http://<the-machine>:9150`, sign in with `admin` / `admin`, and fill in the
bot token and your server ID.

To find the server ID: open any channel in your server; the first long number in
the browser URL is it. It is asked for rather than guessed because inferring it
from the bot's membership only works when the bot is in exactly one server —
which is a silent failure for anyone running it in two.

**Change the console password** on the same page, under Console login. The
shipped `admin` / `admin` is a lock on the door, not security: it is identical on
every install and published in this file. It exists so the console asks for
something from the very first request instead of serving the bot token to anyone
who can reach the port — not so it can be left alone.

Every page carries a warning while the default is still in use, and it does not go
away until the password actually changes. A default that quietly looked like a
configured password would be the worse outcome: nobody fixes what nothing is
complaining about.

A password must be at least 8 characters. Changing it, or the username, signs out
every existing session.

The account name is also editable, in the same place.

Expect the login page to pause for a second or two on a Pi Zero. That is bcrypt at
its default cost, which is the point of it; repeated attempts from one address are
rate limited so the slow hash cannot be used against you.

Saving in the console takes effect without a restart: the Discord connection
retries on a backoff and picks up a corrected token or server id within about a
minute, then builds the categories and channels. Switching transport reconnects
the radio straight away.

All of this also works from the shell, for a headless install:

```sh
sudo meshycord -db /var/lib/meshycord/db.sqlite -set-token 'YOUR.BOT.TOKEN'
sudo meshycord -db /var/lib/meshycord/db.sqlite -set-guild 123456789012345678
sudo meshycord -db /var/lib/meshycord/db.sqlite -set-password 'something long'
sudo meshycord -db /var/lib/meshycord/db.sqlite -set-serial /dev/ttyACM0
sudo systemctl restart meshycord
```

Each of those stores the value and exits, so the running service needs a restart
to pick it up.

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
| 🔄 | *you* add this to your own failed message to ask for a resend, which also clears the stored route so the retry floods |

⏳ does not hang around indefinitely. The node estimates how long the round trip
should take, and the bridge waits that long, floored at a minute and capped at
two. The floor is there because a four-second estimate would turn a perfectly
good delivery into a ❌; the ceiling is there because a lost acknowledgement
should not leave a message pending forever.

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

Use `/mesh` anywhere, or type commands in `#meshycord-admin`. Both reach the same
implementations, so behaviour cannot drift apart — but the typed forms are shorter,
since a channel you are already in does not need the ceremony. `link` is `add`,
`sync-rooms` is `sync rooms`, `contact-reset-path` is `contact reset-path`, and
`help` lists them all.

```
/mesh status              radio, Discord, links, traffic
/mesh help                list every command
/mesh list <what>         room servers, companions, channels, links, repeaters, sensors
/mesh find <text>         search contacts and channels by name or key
/mesh link <target>       give a mesh source its own Discord channel
/mesh unlink <target>     remove a link (the channel is left alone)
/mesh login [target]      set a room-server password in a private popup
/mesh tidy                drop links whose channel or mesh slot is gone
/mesh sync-rooms          give every known room server a channel (asks first)
/mesh reset               delete everything the bridge created (asks first)
```

The radio's own contact list is managed separately, because these change the node
rather than anything in Discord:

```
/mesh contact-add         add a node from its full 64-character public key
/mesh contact-find        search every contact, repeaters and sensors included
/mesh contact-info        everything about one contact, including its full key
/mesh contact-rename      rename a contact on the radio
/mesh contact-type        correct what a contact is (room server, companion, …)
/mesh contact-reset-path  forget a contact's stored route, so the next message floods
/mesh contact-refresh     re-read the contact list from the radio
/mesh contact-remove      delete a contact from the radio
```

`contact-info` is the one that matters more than it sounds: it is where a **full
public key** comes from, which is what you need to add the same node on another
radio, or to hand to somebody whose adverts do not reach you.

The rest take a name, a 12-character key prefix, or a row number from the last
listing — the bridge keeps a mirror of the contact list and resolves the full key
itself, even for the commands whose protocol command requires one.

`list` takes what to list, and optionally `unlinked` to hide what already has a
channel and `sort` (most recently heard, name, or hops).

`rediscover`, in `#meshycord-admin` or on the Links page, is the repair command:
it drops every link and forgets the admin channel, the inbox and the categories,
then finds those three again by name. Use it when the bridge and your server have
got out of step — a restored database pointed at a server whose channels have
moved on, say.

**It deletes nothing in Discord, and it does not re-adopt previously linked
channels either.** Only the admin channel and the categories are matched by name;
a link is always a freshly created channel. So with auto-create on you can end up
with a second `#public` beside the first, and the original left orphaned. Delete
the strays by hand — `tidy` will not, since it drops links whose channel is
missing rather than channels whose link is.

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

**A resend clears the stored route first, so it floods.** A message usually
needs sending again because the recorded route went stale — somebody moved, or a
repeater in the middle went away — and the node keeps using a stored path until
something proves it wrong. Repeating the send down that same path is the retry
least likely to work. The route is relearned from the reply, so this is not
permanent.

Three things follow from that, and they are the cases worth knowing:

- **Direct messages and room servers only.** A mesh channel is not addressed to
  a contact, so there is no stored route to clear; those resends simply go out
  again, and the bridge does not editorialise about it.
- **A `path:direct` prefix wins.** If you typed it and then reacted 🔄, you
  meant the route you have, so the resend stays direct.
- **A resend held for a room login keeps the behaviour.** Otherwise 🔄 would
  work differently depending on whether the room happened to be logged in, which
  is not something you should have to think about.

Often the route will already have been cleared before you press anything. A
direct message sent along a stored path that gets no acknowledgement has that
path cleared automatically when the deadline passes, on the same reasoning — the
path is the prime suspect. The bridge does not resend on its own, though: the
message may well have arrived with only the acknowledgement lost, and an
automatic retry would post it twice. Clearing costs nothing and cannot duplicate
anything; sending again is your call.

The reaction is the only way to ask for a resend. A linked channel intercepts
only the `path:` prefixes and `!promote`; everything else you type goes out over
the radio, so "retry" is just a message like any other.

### Promoting a sender

A key prefix shown next to a message — in the inbox, or on a room post — can be
given its own channel without leaving the conversation:

```
!promote a1b2c3d4e5f6
```

Same thing as `/mesh link`, just closer to hand.

### Mentioning someone

Don't use Discord's `@` autocomplete — it puts a raw account id in the message,
which is 21 bytes of a 160-byte budget and means nothing on the radio. A Discord
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

The prefix costs none of the 160-byte budget and the recipient never sees it.
Only meaningful for direct messages and room servers — channel messages are
always flooded.

There is no per-message route flag in MeshCore; the node picks flood when a
contact has no stored path and direct otherwise, so `path:flood` works by
clearing the path. It is relearned from the reply, so the effect is for that
message rather than permanent.

A 🔄 resend does the same thing without a prefix — see
[Resending](#resending). `path:direct` is how you override that and retry down
the route you already have.

### Long messages

A mesh message is **160 bytes** (`MAX_TEXT_LEN` in the firmware). On a group
channel the node prepends its own name and silently truncates to fit, so the
usable text there is `160 - len(node_name) - 2` and the bridge splits against
that smaller budget. Longer text is split into up to three transmissions, about
two seconds apart — both numbers are in Settings — each echoed into Discord as its
own message and tracked separately, so you can see how much airtime you used and
which pieces actually landed.

Beyond that limit it is **refused, not truncated**, and the refusal says how much
would have to go. Truncating silently looks identical to a successful send.

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
- **Settings** — the bot token and server, the console account, the transport and
  its device, BLE name/address/PIN, the auto-create policy, history retention,
  the room-session window, and the split limits

`/healthz` answers `ok` without a login, for an uptime check or a monitoring
probe. It reports that the process is serving, not that either link is up — the
dashboard and `systemctl status` are where you look for that. The login page and
`/static/` are necessarily unauthenticated too; every other page needs the
password once one is set.

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

And four that write a setting and exit, for setting the bridge up without a
browser — see [Setup](#2-point-it-at-your-server):

```
-set-token <token>       -set-guild <id>
-set-password <pass>     -set-serial <device>
```

**History is kept for 90 days by default**, then pruned; Settings takes any
number of days, and 0 keeps everything. On an SD card, unbounded history is a
slow leak rather than a feature.

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
systemd restarts it. Supervision is deliberately outside the process: a watchdog
a program feeds itself is only as trustworthy as the program, and the case you
need it for is exactly the case where the program has stopped being trustworthy.

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

**"🔄 worked once and now does nothing."**
The bridge could not take your reaction off the message, so Discord considers it
already present and sends no event for the second press. Give the bot **Manage
Messages**, or remove the 🔄 by hand before adding it again.

**"My node is on the mesh but I cannot message it."**
It has to be in the radio's contact list. If its adverts do not reach you, add
it from its full public key with `/mesh contact-add` — the 12-character prefix
shown next to a message is not enough.

### Administering a repeater from the shell

`meshycord-cli` runs MeshCore CLI commands on a repeater or room server over
the air, from the machine running the bridge:

```sh
sudo meshycord-cli -list                                  # what can I talk to?
sudo meshycord-cli -login ridge-repeater <admin-password> # store it once, verified
sudo meshycord-cli -c "clock" ridge-repeater
sudo meshycord-cli -c "neighbors" ridge-repeater
sudo meshycord-cli -c "advert" ridge-repeater
```

It is a client of the running bridge, not a second one. The radio serves
[one program at a time](#limitations), and the bridge holds that slot — so the
CLI hands the command to the daemon over a local socket (`/run/meshycord/`,
root-only) and prints what comes back. **If meshycord is not running, this does
not work.** There is no radio to borrow.

Three things to know before relying on it:

- **It needs the repeater's ADMIN password**, not the guest one. A guest login
  is accepted and then every command is discarded in silence — no error, no
  reply, indistinguishable from a repeater that is out of range. `-login`
  verifies the password immediately rather than letting you find out later.
- **The repeater must be a contact on your node**, because a login needs its
  full 64-hex key. Add it with `contact add <key> repeater <name>`.
- **Silence is a normal outcome for some commands.** `reboot` and `poweroff`
  are gone before a reply could leave. That exits `3`, not `1`, so a script can
  tell "no answer" from "it failed".

`sudo` because the socket is root-only: anything that can open it can reboot
every repeater you have a password for.

### Clocks, and why `clock sync` may not do what you expect

MeshCore keeps every clock in UTC and has no concept of a timezone. A repeater
in Denver reporting `21:04 UTC` at three in the afternoon is correct.

The catch is which clock a repeater copies. `clock sync` sets it from the
**USB node's** clock — not the machine running meshycord. The node stamps the
message with its own RTC and there is no way to override that. So a node
running fast quietly spreads its error to every repeater you sync, and MeshCore
clocks never move backwards, so undoing it means `clkreboot` over USB on each
one.

meshycord sets the attached node's clock on every connect, and warns loudly if
it cannot. To check and correct it by hand:

```sh
sudo meshycord-cli -clock        # node vs this machine, in UTC and local time
sudo meshycord-cli -clock-sync   # set the node from this machine
```

Or bypass both clocks and send an explicit value, which is the safer habit when
this machine has real time and the node may not:

```sh
sudo meshycord-cli -c "time $(date +%s)" ridge-repeater
```

There is **no CLI for the node on the end of your USB cable** — MeshCore has no
such command in any firmware version. Its own CLI exists only in USB "CLI
Rescue" mode, which replaces the binary protocol and so cannot run while the
bridge is connected. Its name, radio settings and clock are set through the
real protocol commands instead.

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

## Limitations

These are MeshCore and Discord, not this bridge. Nothing here is a to-do list —
they are the shape of what a bridge like this can and cannot do:

- The companion serves **one client at a time**. The phone app cannot connect
  while the bridge holds the link, on any of the three transports.
- **160 bytes** per message, less the node's own name on a group channel.
- Group messages **cannot be acknowledged**, so delivery is never confirmable
  for those. A ✅ there would claim something the protocol cannot prove.
- **Only one route per received message is visible.** MeshCore deduplicates by
  packet hash below the app layer, so copies arriving by other routes are gone
  before the companion protocol sees them. The hop count shown describes one
  path, not all of them.
- **Discord mentions do not translate.** A Discord account is not a mesh node, so
  there is nothing sensible to turn one into. Use MeshCore's `@[Node Name]`.

---

## Design notes

**Discord is spoken over the Gateway websocket.** Reactions, buttons, modals and
ephemeral replies are delivered there and nowhere else — Discord's webhook events
cover app authorization, entitlements, quests and Social-SDK lobbies, and its own
documentation says events for channels, guilds, roles and messages "are only
available over Gateway". So 🔄 needs an open socket, and the socket is what makes
replies arrive as fast as the radio can carry them.

**Six dependencies, all pure Go.** `golang.org/x/sys` (termios),
`golang.org/x/crypto` (bcrypt), `modernc.org/sqlite`, `github.com/coder/websocket`,
`tinygo.org/x/bluetooth`, and `github.com/godbus/dbus/v5`, which is how BlueZ is
driven — Linux exposes Bluetooth over D-Bus and there is no way around that.
No Discord library: REST is hand-written on
`net/http` and the Gateway protocol implemented directly. The standard library
already gives TLS, keep-alive, connection pooling and JSON, so what is left is
small enough to read in one sitting — and this uses a narrow enough slice of
Discord that a general-purpose library would mostly be surface area to learn.

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

**Nothing is moved, and almost nothing is adopted.** The bridge never relocates a
channel it did not create. Matching a channel by name and then moving it is how a
repurposed `#general` ends up dragged into someone else's category, and a bridge
that rearranges a server it was invited into has overstepped.

One deliberate exception, besides the categories above: an existing
`#meshycord-admin` is adopted by name, so recreating one by hand works. It is
adopted where it already sits rather than moved into the category.

**SD-card longevity is taken seriously.** WAL mode, `synchronous=NORMAL`, writes
only when something actually changed, and logs to journald rather than a file. A
bridge that idles for weeks should not be quietly wearing out the card it boots
from.

See [PROTOCOL-NOTES.md](PROTOCOL-NOTES.md) for the companion protocol itself —
byte layouts with firmware line references, and every gotcha that cost real
debugging.

---

## Credits

Built by [cartpauj](https://github.com/cartpauj), with Claude (Opus 5) doing most
of the typing.

Protocol details here come from reading the MeshCore firmware rather than its
documentation. [PROTOCOL-NOTES.md](PROTOCOL-NOTES.md) records the byte layouts and
the gotchas, citing the firmware file and line wherever a claim rests on one, so
anything you doubt can be checked at the source.

## Licence

GPL v3. See [LICENSE](LICENSE).

MeshCore itself is GPL v3, and this reads its source closely enough that GPL is
the right fit regardless.
