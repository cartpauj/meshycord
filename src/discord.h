#pragma once

#include <Arduino.h>

// Largest batch discord_poll() will fetch or buffer in one call. Callers ask for
// 8 and 6; the scratch array lives on the loop task's 8KB stack alongside
// HTTPClient and mbedTLS, so this is not free space to round up.
static const size_t POLL_BATCH_MAX = 8;

struct DiscordMessage {
  String id;          // needed to react to it later
  String content;
  String author_id;
  String author_name;
  bool   is_bot;        // MUST be checked — see echo-loop hazard

  // Reply target, when this message is a reply. A reply is an ordinary message,
  // so it arrives on the poll that was happening anyway — which is what makes
  // "reply with retry" free, where watching for a reaction would cost a request
  // per message. Reactions are Gateway-only and cannot be polled for at all.
  String ref_id;        // message_reference.message_id; empty if not a reply
  String ref_content;   // referenced_message.content, when Discord inlined it
  bool   ref_is_bot = false;
  bool   ref_present = false;   // referenced_message was inlined, not just cited
};

// Group several requests onto ONE TLS connection.
//
// Each TLS handshake costs this chip 1-2 seconds, which is why polling used to
// check only one channel per cycle (round-robin) — and with 50 links that meant
// a typed reply could wait 25 minutes to be transmitted. Reusing the connection
// makes a sweep of every channel take seconds instead, so all of them can be
// checked every cycle.
//
// The session is deliberately short-lived: a TLS session held open permanently
// alongside a bonded BLE link is the memory profile we are avoiding.
void discord_session_begin();
void discord_session_end();

// POST a message. Returns the new message's id, or "" on failure — the id is
// needed to react to it later.
String discord_send_id(const String& channel_id, const String& content);

// Convenience wrapper for callers that only care whether it worked.
bool discord_send(const String& channel_id, const String& content);

// Fetch messages newer than after_id (oldest-first in `out`).
// Returns false on transport/HTTP failure. Bot-authored messages are skipped
// so the bridge can never re-send its own output to the mesh.
//
// newest_seen_out receives the newest message id observed INCLUDING filtered
// ones. The caller must advance its cursor with that, not with the last
// returned message: otherwise our own bot posts never advance the cursor, we
// re-fetch them forever, and a human message beyond the `limit` window is
// never seen.
// channel_gone_out is set true if Discord reports the channel no longer exists
// (404 / code 10003), e.g. you deleted it. The caller should drop the route
// rather than retrying a dead channel forever.
bool discord_poll(const String& channel_id, const String& after_id,
                  DiscordMessage* out, size_t max_out, size_t& n_out,
                  String& newest_seen_out, bool* channel_gone_out = nullptr);

// Create a text channel in the configured guild. `name` is sanitized to
// Discord's rules internally. parent_id places it inside a category; without
// one the channel sits above every category, which looks broken.
// Returns the new channel id, or "" on failure.
String discord_create_channel(const String& name, const String& topic,
                              const String& parent_id = "",
                              const String& name_fallback = "");

// Find (or create) a CATEGORY by name. Categories are channels of type 4.
String discord_find_or_create_category(const String& name);

// Look for an existing text channel with this (sanitized) name in the guild and
// return its id, or "" if absent. Used so the admin channel survives a reboot
// or being recreated by hand.
String discord_find_channel(const String& name);

// find, else create.
String discord_find_or_create_channel(const String& name, const String& topic);

// Turn a mesh contact or channel name into a Discord channel name.
//
// Discord allows unicode and emoji in channel names; it lowercases them and
// turns spaces into hyphens, with a 100 character cap. An earlier version here
// stripped every non-ASCII byte, which was stricter than Discord for no reason:
// it turned "Russet<potato> Room" into "russet-room" and an emoji-only name
// into nothing at all.
//
// `fallback` is used when nothing usable survives (a name of pure punctuation,
// say). Pass the contact's key so the result is still unique.
String discord_sanitize_name(const String& raw, const String& fallback = "");

// DELETE a channel or category.
bool discord_delete_channel(const String& channel_id);

// Does this channel id still exist? Stored ids must be verified, not trusted:
// a channel deleted by hand would otherwise never be recreated.
bool discord_channel_exists(const String& channel_id);

// Delete a message. Used to get a typed room-server password out of the channel
// history immediately. Deleting a message the bot did not write requires the
// MANAGE_MESSAGES permission, so this returns false when the bot lacks it and
// the caller must tell the user the password is still sitting there.
bool discord_delete_message(const String& channel_id, const String& message_id);

// True if Discord last rejected our credentials (401/403). Surfaced in the web
// UI: without this a revoked token just makes everything fail silently forever.
bool discord_auth_failed();

// Add a reaction to a message, as the bot. Used to mark whether a message you
// typed in Discord was actually delivered over the mesh.
// Fetch a single message's content, for when a reply did not inline the message
// it replied to.
bool discord_get_message(const String& channel_id, const String& message_id,
                         String& content_out, bool& is_bot_out);

// Remove the bot's OWN reaction. Needs no special permission, unlike removing
// someone else's.
bool discord_unreact(const String& channel_id, const String& message_id,
                     const String& emoji);

bool discord_react(const String& channel_id, const String& message_id,
                   const String& emoji);
