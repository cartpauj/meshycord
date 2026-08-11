// Chat-ops: a #meshycord-admin Discord channel you type commands into.
//
// Commands
//   help
//   status
//   list rooms|companions|channels|links   [unlinked] [recent|name|hops] [desc]
//   find <text>                            [unlinked] [recent|name|hops] [desc]
//   add <n>                                link item n from the last listing
//   remove <n>                             unlink item n from the last listing
//
// Index stability: a listing FREEZES an ordered snapshot of (index -> key) in
// RAM. `add 7` resolves against that frozen snapshot, so contacts adverting in
// afterwards can never shift what 7 refers to. The snapshot expires after
// SNAPSHOT_TTL_MS, and every action echoes the resolved name so a mistake is
// visible immediately.
#pragma once

#include <Arduino.h>
#include "routing.h"

// Ensure #meshycord-admin exists (find, else create) and remember its id.
// Safe to call repeatedly; does nothing once a valid id is stored.
// Runs after WiFi comes up: discovers the guild, creates all four categories,
// creates/adopts #meshycord-admin and #mesh-inbox, and on the very first run moves
// any pre-existing linked channels into their categories. Idempotent.
bool admin_bootstrap();

// Runs once after the mesh link is first established, when channel names are
// known: auto-links every mesh channel if that policy is enabled. Room servers
// are deliberately NOT bulk-created — there can be dozens, which would blow
// Discord's per-category limit and its channel-creation rate limit.
void admin_sync_after_mesh();

bool admin_ensure_channel();

// True if this channel id is the admin channel — it must never be bridged.
bool admin_is_admin_channel(const String& channel_id);

// Handle one message typed by a human in the admin channel.
void admin_handle(const String& content);

// Discord category a route kind belongs in, creating it if needed. Returns ""
// on failure, in which case the caller should create the channel uncategorised
// rather than not at all.
String admin_category_for(RouteKind kind);

// Post the greeting/help once after boot, so the channel shows it is alive.
void admin_announce_ready();

// Forget every Discord-side id (admin channel, inbox, links, first-run marker)
// and run the bootstrap again from scratch. WiFi and the bot token are kept, so
// this reproduces exactly what a new user sees the first time the bridge
// reaches Discord. Returns a human-readable summary.
String admin_rediscover();
