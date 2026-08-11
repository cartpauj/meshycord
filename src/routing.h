// Mesh source  <->  Discord channel mapping, persisted in NVS.
//
// This is the only state the bridge genuinely owns. Contacts, names and
// identities all live on the V3 and are queried on demand — the bridge never
// keeps its own copy. Losing this table costs you the channel mapping and
// nothing else.
#pragma once

#include <Arduino.h>

enum RouteKind : uint8_t {
  ROUTE_DM      = 0,   // key = 12-char pubkey prefix (6 bytes hex)
  ROUTE_CHANNEL = 1,   // key = channel index as decimal string
  ROUTE_ROOM    = 2,   // key = 12-char pubkey prefix of the room server
};

struct Route {
  RouteKind kind;
  String    key;            // pubkey prefix or channel index
  String    channel_id;     // Discord channel snowflake
  String    label;          // last known display name, for channel naming
  uint32_t  last_seen;      // epoch-ish (millis/1000 since boot; best effort)
  String    last_discord_id;// last Discord message id we processed (poll cursor)
  uint32_t  hot_until;      // millis() before which this channel is polled fast
};

// Max routes held in RAM. 500 channels is Discord's per-guild ceiling; this is
// deliberately far below it, since each route also costs a poll request.
static const size_t MAX_ROUTES = 24;

void   routes_load();
void   routes_save();

// Persist a single route. routes_save() rewrites every slot, which is a flash
// write per route just to record one moved poll cursor — and cursors move as
// often as every five seconds on a hot channel. Use this when exactly one route
// changed; `r` must be a pointer from routes_at()/route_find().
void   route_save_one(const Route* r);

size_t routes_count();
Route* routes_at(size_t i);

// Find by kind+key, or nullptr.
Route* route_find(RouteKind kind, const String& key);

// Insert or update. Returns nullptr if the table is full.
Route* route_put(RouteKind kind, const String& key,
                 const String& channel_id, const String& label);

bool route_remove(RouteKind kind, const String& key);

// Inbox poll cursor, persisted. Without this a reboot re-reads recent Discord
// messages and re-sends them to the mesh.
String inbox_cursor_get();
void   inbox_cursor_set(const String& id);
void routes_clear();
