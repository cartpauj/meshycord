# MeshyCord

A small ESP32 device that bridges a [MeshCore](https://meshcore.io) companion
radio to Discord. Messages from the mesh appear in Discord channels within a
second or two. Replies you type in Discord go back out over LoRa.

It connects to your existing companion node over Bluetooth, so you do not have
to reflash the radio or give up its identity. Everything on the Discord side
(categories, channels, the admin console) is created automatically.

Status: working, running on hardware daily. Rough edges are listed under
[Known limitations](#known-limitations).

## What you need

* An ESP32 board. Developed and tested on an **ESP32-C3** (single core,
  400 KB SRAM, no PSRAM, 4 MB flash). Anything with more headroom also works.
* A MeshCore **companion** node running BLE firmware. Developed against a
  Heltec V3.
* A Discord server you control.
* USB cable and PlatformIO for the initial flash.

The bridge and the companion are separate devices. The bridge does not join the
mesh itself. It borrows your node's identity by talking to it over Bluetooth.

## Quick start

### 1. Set a fixed pairing PIN on the companion

This is the step people get stuck on. If no PIN is stored, MeshCore generates a
new random one on every boot and shows it on the node's screen. A headless
bridge cannot read a screen, so pairing will never succeed.

In the MeshCore phone app, connect to your companion and set a device PIN. It
must be exactly six digits, between `100000` and `999999`.

### 2. Create a Discord bot

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications)
   and click **Create App**.
2. Open the **Bot** page. Click **Reset Token** and save the token somewhere
   safe. Discord shows it once.
3. On the same page, turn on **Message Content Intent**. Without it the bot can
   see that messages exist but not read them, which breaks replies.
4. Turn **Public Bot** off so nobody else can invite it.
5. Go to **Installation** and set **Install Link** to **None**. A private app
   cannot use Discord's provided link.
6. Go to **OAuth2 > URL Generator**. Tick the `bot` scope and these four
   permissions:

   | Permission | Why |
   | --- | --- |
   | View Channels | Nothing else works without it |
   | Send Messages | Post mesh traffic |
   | Read Message History | Poll for your replies |
   | Manage Channels | Create channels and categories |
   | Manage Messages | Delete a room-server password the moment it is read |

   Without **Manage Messages** everything still works except that a password
   you type stays in the channel history; the bridge will tell you so and ask
   you to delete it yourself.

7. Open the generated URL and authorise the bot into your server.

Optionally rename the bot on the **Bot** page. That name appears as the author
of every relayed message.

### 3. Find your server ID

Open any channel in your server. The first long number in the browser URL is
the server (guild) ID.

```
https://discord.com/channels/000000000000000000/111111111111111111
                             ^^^^^^^^^^^^^^^^^^ this one
```

### 4. Flash the bridge

```sh
git clone <this repo>
cd meshycord
pio run -t upload
```

No configuration file to edit. Nothing secret is compiled in.

### 5. Provision it

On first boot the device has no settings and raises an open access point called
`meshycord-setup`.

1. Join that network from a phone or laptop.
2. Open `http://meshycord.local`, or `http://192.168.4.1` if mDNS does not
   resolve.
3. Fill in your WiFi, the bot token, and the server ID.
4. Set a password for the settings page. It holds your bot token.
5. Save.

The device reboots, joins your network, and builds the Discord side. Within a
minute you should have four categories and two channels.

To get back into setup later, hold the BOOT button while powering on. It also
falls back to setup mode automatically if WiFi becomes unusable, so a wrong
password cannot lock you out.

## How it works

### What appears in Discord

| Category | Contents |
| --- | --- |
| MeshyCord | `#meshycord-admin`, `#global-inbox` |
| Channels | one per linked mesh channel |
| Room Servers | one per linked room server |
| Companion DMs | one per linked contact |

Discord's own default channels are ignored. Delete them or keep them, the
bridge does not care and will never touch them.

#### Renaming and rearranging

**Renaming a channel is safe.** Routing uses the channel's Discord ID together
with the mesh public key prefix or channel slot, both stored in NVS. The name is
never consulted, so rename channels however you like, add emoji, whatever. Moving
a channel to a different category is fine too.

Two things behave differently:

* **Deleting a channel removes the link.** The next poll gets a 404, the route
  is dropped, and the contact goes back to `#global-inbox`. Link it again from
  the admin console if that was not what you wanted.
* **Renaming a category does not carry over.** Categories are matched by exact
  name, so if you rename `Room Servers` the bridge will create a fresh one next
  time it needs it and leave yours behind. Channels already inside stay put.
  Rename them if you prefer, just expect a new one to appear alongside.

### Reading messages

Mesh traffic reaches Discord in about a second. The companion pushes a
notification over Bluetooth the moment a message arrives, so nothing is polled
in this direction.

A channel message looks like this:

```
North Ridge Relay    2 hops, snr 8.5
repeater is back up after the power cut
```

The Discord channel already says which mesh channel it came from, so that is
not repeated. Direct messages also show the sender's key, which is what you use
to link them:

```
Alex  a3f19c000000    heard direct, snr 11.2
on my way
```

#### About the hop count

The number is the hops taken by the **first copy of the message that arrived**,
not the only path it travelled.

On a flood-routed mesh the same message usually reaches you several times by
different routes, each with its own hop count. MeshCore deduplicates by packet
hash (`wasSeen` / `markSeen` in `Mesh.cpp`), so the message layer hands the app
exactly one copy, the first to land. That is usually the fastest route, which is
not necessarily the shortest. The other copies were received by the radio but
are dropped before anything above sees them.

There are three forms:

* **`N hops`** is a flood-routed packet, where each repeater appended itself to
  the path. The number is real.
* **`heard direct`** is a flood-routed packet with nothing appended, so you
  received it straight from the sender with no repeater in between.
* **`via known path`** means the packet used MeshCore's direct routing, where
  the sender already knew a route and supplied it. That path can be many hops
  long, so this says nothing about distance. The node reports `0xFF` rather than
  a count for these, because the hop count is not carried.

That last one is worth being clear about. `ROUTE_TYPE_DIRECT` in MeshCore means
"a path was supplied", not "the sender was nearby". A station hundreds of miles
away with an established route sends direct-routed packets.

This is also why the phone app can show a repeat count and this bridge cannot.
The app watches raw radio traffic below the deduplication and counts copies of
the same packet. Doing the same here means matching raw packets against your own
traffic, which is the same work described under
[Known limitations](#known-limitations).

### Sending messages

Type in any linked channel. The message goes out over LoRa and gets a reaction
telling you what happened:

| Reaction | Meaning |
| --- | --- |
| check mark | Delivered. The recipient acknowledged it |
| cross | Failed, or no acknowledgement before the node's timeout |
| satellite antenna | Transmitted. Group messages cannot be acknowledged in MeshCore, so delivery is unknown |
| counterclockwise arrows | A resend is in flight; replaced by one of the above |

A direct message gets its check or cross only when the recipient acknowledges,
which can take up to two minutes. A channel message is marked transmitted
immediately, because there is nothing to wait for.

A cross is not the end of it — reply to the message with `retry` to send it
again. See [Resending a message](#resending-a-message).

MeshCore caps a message at 133 bytes. Longer text is split into up to three
transmissions, two seconds apart. When that happens the bridge posts a notice
and then each transmission as its own Discord message, tracked separately, so a
partial delivery is visible rather than hidden:

```
MeshyCord   Splitting into 3 transmissions (340 characters, 133 per
            transmission, ~2.0s apart)
MeshyCord   [1/3] ...     check mark
MeshyCord   [2/3] ...     check mark
MeshyCord   [3/3] ...     cross
```

Anything over 375 characters of plain text is refused outright rather than
truncated. Emoji cost four bytes each, so the practical limit is lower for
those.

### Resending a message

**Reply** to the message and say `retry` (or `resend`). On mobile that is a
swipe on the message; on desktop, hover it and pick Reply. The old marker is
cleared and the new result lands on the original message, where you are already
looking. The word `retry` itself never goes to the mesh.

A counterclockwise-arrows marker appears while the resend is in flight and is
replaced by the result. That matters for direct messages: those are only ticked
once the recipient acknowledges, which can be up to two minutes, so without it
the cross would simply vanish and nothing would seem to happen.

Reply to your own message to send the whole thing again. Reply to a numbered
`[2/3]` transmission from a split message to resend **only that piece** — which
is usually what you want, since the others already landed.

It is a reply rather than a reaction for a concrete reason. Reactions are only
delivered over Discord's Gateway and cannot be polled for at all, so noticing
one would mean re-fetching every failed message on every sweep. A reply is an
ordinary message: it arrives on the poll that was happening anyway, it costs
nothing extra, and it says exactly which message it means.

### Message history and catching up

The companion holds a queue of messages that arrived while no client was
connected, and the bridge drains it on reconnect. On a Heltec V3 that queue is
**256 messages** (`OFFLINE_QUEUE_SIZE` in the board's `platformio.ini`), which
is hours of traffic on a busy mesh. Some boards use the MeshCore default of 16
instead, so check your variant if catching up seems short.

Room servers keep their own history separately, up to 32 posts, and push what
you missed when you log in. That backfill arrives over the air at LoRa speed.
It follows every successful login, including the automatic one after a
reconnect, so it is the normal way a room catches up — see
[Room servers need a login](#room-servers-need-a-login).

Two things this does not do:

* **There is no backfill for a newly linked channel.** The queue is drained,
  not replayed, and MeshCore has no "give me the last N messages" command. Link
  a channel today and you get everything from now on, nothing from before.
* **Mesh channels keep no history anywhere.** If nothing was connected and the
  queue overflowed, that traffic is gone.

After a long outage, expect a visible replay rather than an instant catch-up.
Each message is a separate Discord API call at roughly two to three seconds, so
a few hundred queued messages take several minutes to appear.

### Discord replaces the app, not augments it

This is a deliberate position rather than a limitation to work around.

The companion serves one client at a time, and the bridge holds that link
continuously. While it is running, your phone cannot connect to the same node.
Both consume the same offline queue, so whichever syncs first empties it.

The intent is that Discord becomes where you read and reply, and the phone app
becomes the fallback for when the bridge is off or being worked on. Your message
history then lives in Discord, which is a better place for it than a queue on a
microcontroller. Switching back to the app is always possible, you just leave
the history behind.

### Forcing the path

MeshCore decides how to send a message on its own: it floods when it has no
stored path for a contact, and follows the path when it has one. That is usually
what you want, but a stale path keeps being used until something proves it wrong.

Prefix a message in a linked channel to override it:

```
path:flood are you still on the ridge?
path:direct are you still on the ridge?
```

The prefix is case insensitive and can be followed by a space or a tab. It and
any whitespace after it are stripped before anything else happens, so they use
none of the 133 byte transmission budget and do not count toward the 375
character limit. The recipient sees only your text.

A message that merely starts with those letters is left alone, so
`path:flooding is a problem` sends as written.

`path:flood` clears the contact's stored path first, so the message floods and
the path is relearned from the reply. It affects that message rather than being
a permanent setting. `path:direct` is the default and is there for symmetry.

The bridge replies with whether the message flooded or followed the stored path,
taken from the node's own report rather than assumed.

There is no per-message path flag in the companion protocol, so this works by
clearing the path (`CMD_RESET_PATH`) before sending. Two consequences: it only
applies to direct messages and room servers, since channel messages are not
addressed to a contact and are always flooded; and it needs the recipient to be
in the node's contact list, because clearing a path requires their full public
key.

### Room servers need a login

A room server accepts posts only from someone logged in, so each one needs its
password once. Everything else on the mesh does not: direct messages need no
login at all, and mesh channels use a shared secret that lives on your node, so
any channel your companion is already configured for just works.

Link the room, then paste the command it gives you back into
`#meshycord-admin` with the password on the end:

```
login 7 hunter2
login a3f19c000000 hunter2
```

**Your message is deleted as soon as it has been read**, so the password does
not stay in the channel history. That needs the bot to have **Manage Messages**;
if it does not, the bridge says so and leaves the message for you to delete by
hand. The password itself is kept on the device, so the session can be
re-established without asking you again.

`login <n>` with no password forgets the stored one. Unlinking a room forgets it
too, and so does `reset`.

The room replies over the air, so the verdict takes a few seconds and arrives in
that room's own channel — either a confirmation, or a note that the password was
rejected with the command ready to paste again.

Two things worth knowing:

* **A session does not last forever, and it does not survive a reconnect.** The
  bridge logs in again whenever the Bluetooth link comes back, and again if a
  post fails. Because the companion protocol does not pass the server's
  keep-alive interval on, this is driven by what actually happens rather than by
  a timer.
* **Posting without a session fails silently on the mesh.** The send itself
  succeeds and the post simply never appears. Rather than show that as a
  delivered message, the bridge refuses it up front and tells you what to paste.

Since the bridge borrows your node's identity, a session your phone established
is one the bridge inherits — which is why a room can appear to work and then
stop once that session lapses.

### The inbox

`#global-inbox` collects traffic from senders that do not have a channel yet.
In practice that means direct messages from people you have not linked.

It is informational. You do not reply there. The channel aggregates many
senders, so an unprefixed reply would be ambiguous. To talk to someone, copy
the key shown next to their name and link them from the admin console.

### The admin console

Type commands in `#meshycord-admin`.

```
help                                 list the commands
status                               link state, contacts, heap, uptime

list rooms                           room servers, numbered
list companions                      people
list channels                        mesh channels
list links                           what is currently bridged
find ridge                           search by name

  modifiers: unlinked, recent, name, hops, desc
  example:   list rooms unlinked hops
             find ridge name

add 7                                link item 7 from the last listing
add a3f19c000000                     link by key, even for a stranger
add 7 as control-net                 choose the Discord channel name
remove 3                             unlink

login 7 hunter2                      log in to a room server
login 7                              forget its stored password

contact find ridge                   search EVERY contact on the node
contact list                         ...all of them
contact add <64-hex-key> <name>      add a node seen on the public map
contact remove 3                     delete a contact from the node

tidy                                 move channels into their categories
sync rooms                           link every room server, asks first
reset                                delete everything the bridge made
```

Listings are sorted by most recently heard by default. Rows show whether they
are already linked and how many hops away they are.

Numbering is stable. A listing freezes the mapping from number to contact for
ten minutes, so `add 7` always means the row you were looking at even if new
contacts advert in meanwhile. After that it asks you to list again rather than
acting on something stale. Every action names what it acted on, so a mistake is
obvious and reversible.

### Contacts on the node

`list` and `find` show what the bridge has cached, which is only companions and
room servers — on a 350-contact mesh that is about 95 of them. The rest are
repeaters and sensors, deliberately skipped so they cannot push real senders out
of the cache.

The `contact` commands work on the node's own list instead, all of it:

```
contact find ridge      search every contact by name or key prefix
contact list            all of them
contact remove 3        delete one from the node
```

`contact find` scans the node live rather than reading the cache, so it takes a
few seconds and it does see repeaters and sensors. It matches on the name or on
the key prefix, so a key from a message is enough to find who it belongs to.

Removal needs the **full 32-byte public key**, which no listing displays and
nobody would retype — so a scan remembers the full keys of what it found, and a
row number is enough. That numbering holds for ten minutes, like every other
listing. `contact remove <64-hex-key>` also works if you have the key to hand.

Removing a contact also drops its bridge link and forgets any stored room
password. The Discord channel is left alone, as everywhere else. **A removed
contact comes back the next time that node adverts** — this clears clutter, it
does not block anyone.

Adding requires a key and a name:

```
contact add f40e2949...4a91 North Ridge Relay
contact add f40e2949...4a91 Ridge Room room
```

The name is not optional. Without one the contact is unnamed on the node, cannot
be found by name afterwards, and any Discord channel created for it would be
named after its key.

### Settings

`http://meshycord.local` has WiFi, Discord credentials, the BLE target, poll
interval, and two policy switches:

* **Auto-create channels for mesh channels.** There are at most eight, so this
  is usually safe to leave on.
* **Auto-create channels for room servers.** Off by default. A busy mesh can
  have dozens of rooms, and Discord caps a category at 50.
* **Auto-create channels for direct messages.** Off by default, and the riskiest
  of the three. Anyone who has heard your advert can send you a DM, so this is
  the switch that lets a stranger cause a channel to be created.

All three only ever fire for senders the node already knows. A message from
something not in the contact list cannot be classified as a person or a room,
and there is nothing to name a channel after, so it always goes to the inbox
regardless of these settings.

It also shows how many links exist, and has buttons to clear them all or re-run
the Discord setup.

Linking is done from `#meshycord-admin`, not here — it works from anywhere and
reads better. The page is deliberately a fixed size whatever the mesh is doing:
its cost does not grow with the number of contacts, so opening it on a busy mesh
is never the thing that runs the device out of memory.

## Design notes

A few decisions that are easy to second-guess later.

**Auto-create defaults to off.** The admin console makes linking easy, and
automatic creation surprises people by filling their server with channels they
did not ask for.

**The inbox is read-only in practice.** An earlier version let you reply there
by typing the sender's hex key first. That was a bad idea. The channel
aggregates many people, so an unprefixed reply is genuinely ambiguous, and a
prefixed one means copying hex by hand.

**Nothing is adopted, only created.** The bridge never claims a channel it did
not make, and never moves one. Your `#general` is safe.

**Channel names are cosmetic.** Routing is by public key prefix or channel slot,
stored in NVS, so renaming a channel by hand never breaks anything. Categories
are the exception: they are matched by name, because there is no other way to
find one that already exists.

**Room servers are never bulk-created automatically.** `sync rooms` exists for
when you really want all of them, and it asks first.

**Unknown senders always go to the inbox.** Auto-create needs the contact record
to tell a person from a room server, and needs a name for the channel. Neither
exists for a stranger, so the inbox is the only sensible destination. This is
also what stops an unknown node from filling the server with channels.

## Known limitations

**Polling does not scale.** Replies from Discord are found by polling, because
Discord's REST API has no push. Each request costs this chip roughly two to
three seconds, so a sweep of two channels takes about eleven. Channels you are
actively talking in are polled every five seconds and the rest every thirty,
which works well up to a couple of dozen links. Beyond that the right fix is
the Discord Gateway, which replaces polling with a push socket and makes the
cost independent of how many channels you have. Not built yet.

**Group messages cannot be acknowledged.** MeshCore returns a plain OK for
channel sends with no acknowledgement handle, so those get a transmitted marker
rather than a delivery confirmation. The phone app infers delivery by watching
for repeaters echoing the packet, which requires reimplementing MeshCore's
group cipher to recognise your own traffic. Possible, not done.

**Only one route per received message is visible.** Duplicate copies arriving by
other routes are discarded by MeshCore's deduplication before the companion
protocol sees them, so the reported hop count describes one path rather than
all of them. Counting repeats or reporting per-route hops needs the same
packet-level matching as the item above. Both would be solved by the same piece
of work.

**One client at a time.** While the bridge holds the Bluetooth link, your phone
cannot connect to that same node.

**Discord mentions do not translate.** Typing `@someone` in Discord sends a raw
user ID over the mesh, which means nothing there and wastes bytes. MeshCore's
own convention is `@[Node Name]`, which passes through correctly if you type it
by hand.

**Contact cache is capped at 192.** Only contacts that can exchange messages are
cached. On a 350 contact mesh roughly 250 are repeaters and sensors and are
skipped, leaving about 100.

## Developers

### Building

```sh
pio run                  # compile
pio run -t upload        # flash over USB
pio device monitor       # serial log at 115200
```

The platform is pinned to `espressif32@7.0.1` (Arduino core 2.0.17), and that
pin matters. `platform = espressif32` on its own resolves to whatever is
installed under that name, and the pioarduino/tasmota fork ships under the same
name with Arduino core 3.x. On core 3.x `WiFiClientSecure.h` no longer exists —
it was renamed `NetworkClientSecure.h` — and NimBLE-Arduino 1.4.3 does not
compile at all. The result is errors in `discord.cpp` and deep inside NimBLE
that look like the project is broken when only the framework is wrong.

If you see `fatal error: WiFiClientSecure.h: No such file or directory`, that is
what happened. Check which platform actually got used:

```sh
pio system info          # confirm the core directory
pio pkg list             # confirm espressif32 7.0.1, not a 202x.xx.xx fork
```

The ESP32-C3 needs specific flash settings, already in `platformio.ini`:

```ini
board_build.flash_mode = dio
board_build.f_flash    = 40000000L
```

At the 80 MHz default the chip boot loops forever with `rst:0x3
(RTC_SW_SYS_RST)` and prints nothing useful, because the ESP-IDF bootloader
console goes to UART0 rather than USB. A blank sketch fails the same way. This
is a known issue on several C3 boards.

`huge_app.csv` gives a single 3 MB app partition. The stock 4 MB scheme reserves
two 1.25 MB OTA slots, which the firmware outgrew.

### Layout

| File | Responsibility |
| --- | --- |
| `main.cpp` | State machine, routing, message formatting, delivery receipts |
| `meshcore.cpp` | BLE client, companion protocol, contact and channel caches |
| `discord.cpp` | REST client, TLS, connection pooling, rate limits |
| `admin.cpp` | Admin console commands, Discord bootstrap, categories |
| `webui.cpp` | Setup access point, captive portal suppression, settings page |
| `routing.cpp` | Mesh source to Discord channel mapping, persisted |
| `settings.cpp` | NVS-backed configuration |
| `util.cpp` | Chunking, UTF-8 handling, heap instrumentation, watchdog |

`PROTOCOL-NOTES.md` documents the MeshCore companion protocol as used here,
with byte layouts and firmware line references. It is worth reading before
touching `meshcore.cpp`.

### Things worth knowing before changing anything

**Push codes are not command replies.** Anything with a first byte of `0x80` or
above is an asynchronous push and is dropped at the BLE callback rather than
entering the response queue. Letting them mix caused three separate bugs, each
of which presented as something unrelated being subtly wrong.

**Command replies must be matched to their request.** `CMD_GET_CHANNEL` returns
the channel index in its reply and it has to be checked. A timed-out query for
slot 1, answered during the query for slot 2, produced a duplicate Discord
channel with the wrong name.

**Some commands need the full 32 byte public key.** `CMD_GET_CONTACT_BY_KEY` and
`CMD_SEND_LOGIN` will not accept the six byte prefix that arrives with a
message. The contact cache keeps full keys for this reason.

**The path byte is packed, not a count.** Bits 0 to 5 hold the hop count and
bits 6 to 7 the per-hop hash size (`Packet.h:79`), and the whole byte is `0xFF`
for a direct route. Printing it raw showed "71 hops" for what was really 7. The
protocol documentation calls the field "Path Length", which reads like a plain
integer; the firmware accessors `getPathHashCount()` and `getPathHashSize()` are
what give it away.

**The length check and the splitter share one function.** They used to compute
capacity separately and disagreed by eight bytes per chunk, which silently
truncated messages that passed the check.

**NVS namespaces are `meshy` and `meshy_rt`.** Renaming them orphans every
deployed device's settings.

**Reactions cannot be received over REST.** They are Gateway-only, and there is
no endpoint to poll for them — Discord's HTTP webhook events cover app
authorization, entitlements and Social-SDK lobbies, nothing about messages. This
is why resending is driven by a reply: a reply is an ordinary message that
arrives on the poll already being made, and `message_reference` names its target.
The same wall rules out modals, ephemeral replies and buttons, all of which need
an interaction.

**A login is answered by a push, not by the command reply.** `CMD_SEND_LOGIN`
replies `RESP_CODE_SENT`, which only means the request went out over the air.
The room's verdict arrives later as `PUSH_CODE_LOGIN_SUCCESS` (0x85) or
`PUSH_CODE_LOGIN_FAIL` (0x86), both carrying the room's 6-byte key prefix.
Treating the immediate reply as success reports every login as working.

**The server's keep-alive interval is not passed on.** The node parses it from
the room's response but the push frame does not carry it, so there is no expiry
to schedule against. Sessions are re-established on events instead: whenever the
BLE link returns, and whenever a post to that room finds no session.

**Room passwords live under their own NVS key.** The route record is packed with
`|` separators and a password is free text, so one pipe would silently shift
every field after it. The key is `p` plus the 12-character prefix — 13
characters, against an NVS limit of 15.

**Delete the password message before anything else can fail.** `do_login()`
deletes it first and only then parses, stores and transmits. Deleting a message
the bot did not write needs `MANAGE_MESSAGES`, so the false return is meaningful
and has to be surfaced rather than assumed.

**Nothing may build a response proportional to the contact list.** The C3 has no
PSRAM, and a single contiguous allocation of a few tens of KB is already most of
the free heap once WiFi, TLS and NimBLE are up. Anything that renders per-contact
or per-channel output has to be bounded, streamed, or paged. This is the
constraint the web UI is shaped around.

**`GET /guilds/{id}/channels` is parsed off the socket, not into a String.** The
response carries every channel with its full permission overwrites, which is tens
of KB on a large server — holding it and then parsing it needs the body in memory
twice. `http_get_json()` streams it through an ArduinoJson filter that keeps only
`id`, `name` and `type`.

**That path forces HTTP/1.0, and it is load-bearing.** `useHTTP10(true)` is what
stops the server replying with chunked transfer encoding, which `getStream()`
does not decode — the parser would be handed chunk-length lines as if they were
JSON. It costs connection reuse, which is why this only runs during bootstrap,
rediscover and reset, never in the polling sweep.

**Free heap alone does not predict an allocation failure.** Fragmentation does:
a request fails because no single block is big enough while the total still looks
healthy. `heap_guard_check()` watches the largest free block as well, and spaces
its strikes over seconds — `loop()` runs every 20ms, so counting three
consecutive calls measured 60 milliseconds, not a sustained condition.

**Persist one route, not the table.** `routes_save()` rewrites every slot, so
recording a single moved poll cursor cost a flash write per route — and hot
channels poll every five seconds. `route_save_one()` exists for that case. NVS is
20KB and shared with the bot token.

**Every NVS write is checked.** `Preferences` reports failure by returning 0 and
nothing else. Unchecked, a full or worn-out NVS lets settings appear to save,
keep working for the rest of the session because they are held in RAM, and simply
vanish at the next boot.

**Dropping a route compacts the table.** Any loop holding a `Route*` across a
call that can remove one is holding a pointer to a different route afterwards,
and must not advance its index. `poll_discord_once()` and `admin_bootstrap()`
both handle this; the failure is silent, and corrupts another channel's poll
cursor rather than crashing.

**An Arduino String releases its buffer only when assigned a null `const char*`.**
Assigning `""` keeps it, and so does move-assigning an empty String. Strings of
14 characters or fewer are stored inline and own no heap at all. This matters for
the static scratch arrays in `admin.cpp`, which would otherwise hold a listing's
worth of names until something overwrote them.

### Serial log

The device narrates what it does. Useful markers:

```
[ble] auth enc=1 authd=1 bonded=1        Bluetooth link authenticated
[mesh] contacts: 350 seen, 253 skipped   contact enumeration
[poll] full sweep: 2 channel(s) in ...   Discord polling, watch as links grow
[ack] delivered in 2410ms                delivery confirmation
[heap] free=..  largest=..  floor=..     watch largest, not just free
[room] login a3f19c000000: OK        room server session established
[wdt] watchdog armed, 90s timeout
```

Three worth reacting to:

```
[heap] LOW (fragmented): ...             free heap is fine, no usable block
[route] NVS WRITE FAILED for 'r3'        links will not survive a reboot
[discord] json stream parse: ...         the channel listing did not parse
```

## Credits

Built by [cartpauj](https://github.com/cartpauj) with Claude (Opus 5) doing most
of the typing.

Worth being straight about what that means. The model wrote the code, but
getting from a working demo to something that survives contact with a real mesh
took a lot of hours, a fair amount of money in tokens, and a lot of decisions
that were not the model's to make. Most of the interesting problems were found
by running it against a live 350 node network and watching it misbehave: a
duplicate channel here, a silently truncated message there, a boot loop that
turned out to be flash timing rather than anything in the firmware. Several
designs were built and then thrown away, including two different schemes for
replying from a shared inbox.

If you are evaluating whether an LLM can build something like this, it can write
the code, and it will confidently get protocol details wrong until you make it
read the firmware. Reading the MeshCore source rather than its documentation
caught two bugs that would have failed silently at runtime.

## License

GNU General Public License v3.0. See [LICENSE](LICENSE).

Worth noting for anyone building on this: MeshCore itself is GPL v3, and this
reads its source closely enough that GPL is the right fit regardless.
