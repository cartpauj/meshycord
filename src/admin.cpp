#include "admin.h"
#include "settings.h"
#include "routing.h"
#include "meshcore.h"
#include "discord.h"
#include "util.h"
#include <Preferences.h>

// A Discord message caps at 2000 characters, so listings are capped and the
// remainder is reported rather than silently dropped.
static const size_t MAX_ROWS      = 25;
static const size_t MAX_REPLY_LEN = 1900;

// --- frozen listing snapshot ------------------------------------------------
static const size_t   SNAPSHOT_MAX    = 64;
static const uint32_t SNAPSHOT_TTL_MS = 10UL * 60UL * 1000UL;

struct SnapItem {
  RouteKind kind;
  String    key;      // pubkey prefix, or channel slot as decimal
  String    label;
};
static SnapItem g_snap[SNAPSHOT_MAX];
static size_t   g_snap_n  = 0;
static uint32_t g_snap_at = 0;

static void snap_reset() { g_snap_n = 0; g_snap_at = millis(); }
static void snap_add(RouteKind k, const String& key, const String& label) {
  if (g_snap_n >= SNAPSHOT_MAX) return;
  g_snap[g_snap_n].kind  = k;
  g_snap[g_snap_n].key   = key;
  g_snap[g_snap_n].label = label;
  g_snap_n++;
}
static bool snap_valid() {
  return g_snap_n > 0 && (millis() - g_snap_at) < SNAPSHOT_TTL_MS;
}

// --- categories ------------------------------------------------------------
// Without a parent_id a channel sits above every category, which looks broken.
// Separate categories per kind also respects Discord's 50-channels-per-category
// limit — with 46 rooms on this mesh a single category would fill up.
static String g_cat_admin, g_cat_chan, g_cat_room, g_cat_dm;

static String cat_cached(String& slot, const char* name) {
  if (slot.length()) return slot;
  slot = discord_find_or_create_category(name);
  return slot;
}

void admin_forget_categories() {
  g_cat_admin = ""; g_cat_chan = ""; g_cat_room = ""; g_cat_dm = "";
}

String admin_category_for(RouteKind kind) {
  switch (kind) {
    case ROUTE_CHANNEL: return cat_cached(g_cat_chan, "Channels");
    case ROUTE_ROOM:    return cat_cached(g_cat_room, "Room Servers");
    default:            return cat_cached(g_cat_dm,   "Companion DMs");
  }
}

String admin_create_channel(RouteKind kind, const String& name,
                            const String& topic, const String& name_fallback) {
  String id = discord_create_channel(name, topic, admin_category_for(kind),
                                     name_fallback);
  if (id.length()) return id;

  // Most likely the cached category was deleted in Discord, leaving a dead
  // parent id. Forget them and try once more; the category gets recreated as a
  // side effect of asking for it again.
  Serial.println("[admin] create failed; refreshing categories and retrying");
  admin_forget_categories();
  return discord_create_channel(name, topic, admin_category_for(kind),
                                name_fallback);
}

// --- helpers ---------------------------------------------------------------
static void reply(const String& text) {
  if (g_settings.admin_channel.length() == 0) return;
  String t = text;
  if (t.length() > MAX_REPLY_LEN) t = t.substring(0, MAX_REPLY_LEN) + "\n…truncated";
  discord_send(g_settings.admin_channel, t);
}

static bool linked_for(RouteKind k, const String& key) {
  return route_find(k, key) != nullptr;
}

static String ago(uint32_t last_advert) {
  // last_advert is an epoch second from the node; the bridge has no wall clock,
  // so show the raw value only when it is obviously unset.
  if (last_advert == 0) return "never";
  return String(last_advert);
}

// --- sorting ---------------------------------------------------------------
enum SortKey { SORT_RECENT, SORT_NAME, SORT_HOPS };

struct Row {
  RouteKind kind;
  String    key;
  String    label;
  uint32_t  last_advert;
  uint8_t   hops;
  bool      linked;
};

static void sort_rows(Row* rows, size_t n, SortKey key, bool desc) {
  for (size_t i = 1; i < n; i++) {              // insertion sort; n <= ~200
    Row v = rows[i];
    size_t j = i;
    while (j > 0) {
      const Row& a = rows[j - 1];
      bool a_first;
      switch (key) {
        case SORT_NAME: {
          String x = a.label; x.toLowerCase();
          String y = v.label; y.toLowerCase();
          a_first = (x <= y);
          break;
        }
        case SORT_HOPS:
          // 0xFF means "no known path" — always last.
          a_first = (a.hops == v.hops) ? true : (a.hops < v.hops);
          break;
        case SORT_RECENT:
        default:
          a_first = (a.last_advert >= v.last_advert);   // newest first
          break;
      }
      if (desc) a_first = !a_first;
      if (a_first) break;
      rows[j] = rows[j - 1];
      j--;
    }
    rows[j] = v;
  }
}

static String hops_str(uint8_t h) {
  if (h == 0xFF) return "?";
  if (h == 0)    return "direct";
  return String((int)h) + "h";
}

// --- listing ---------------------------------------------------------------
static void render(Row* rows, size_t n, const String& title) {
  snap_reset();
  String out = "**" + title + "** — " + String((int)n) + " item(s)\n```\n";
  size_t shown = 0;
  for (size_t i = 0; i < n && shown < MAX_ROWS; i++, shown++) {
    snap_add(rows[i].kind, rows[i].key, rows[i].label);
    // Trim on a UTF-8 boundary: "%.24s" would cut an emoji in half and emit
    // invalid bytes into the Discord message.
    String nm = utf8_truncate(rows[i].label, 24);
    char line[128];
    snprintf(line, sizeof(line), "%2u %-3s %s",
             (unsigned)shown + 1,
             rows[i].linked ? "[x]" : "[ ]",
             nm.c_str());
    out += line;
    for (int pad = nm.length(); pad < 26; pad++) out += ' ';
    if (rows[i].kind != ROUTE_CHANNEL) out += hops_str(rows[i].hops);
    out += "\n";
  }
  out += "```";
  if (n > shown) {
    out += "\n_" + String((int)(n - shown)) + " more — narrow it with `find <text>`_";
  }
  out += "\n`[x]` = already linked. Use `add <n>` / `remove <n>`.";
  reply(out);
}

static size_t collect(Row* rows, size_t max, uint8_t want_type,
                      bool channels, bool links_only,
                      bool unlinked_only, const String& filter) {
  size_t n = 0;

  if (links_only) {
    for (size_t i = 0; i < routes_count() && n < max; i++) {
      Route* r = routes_at(i);
      String lbl = r->label.length() ? r->label : r->key;
      if (filter.length()) {
        String l = lbl; l.toLowerCase();
        if (l.indexOf(filter) < 0) continue;
      }
      rows[n++] = { r->kind, r->key, lbl, 0, 0xFF, true };
    }
    return n;
  }

  if (channels) {
    for (uint8_t i = 0; i < MESH_MAX_CHANNELS && n < max; i++) {
      String cname;
      if (!mesh_channel_at(i, cname)) continue;
      String key = String((int)i);
      bool lk = linked_for(ROUTE_CHANNEL, key);
      if (unlinked_only && lk) continue;
      if (filter.length()) {
        String l = cname; l.toLowerCase();
        if (l.indexOf(filter) < 0) continue;
      }
      rows[n++] = { ROUTE_CHANNEL, key, cname, 0, 0xFF, lk };
    }
    return n;
  }

  for (size_t i = 0; i < mesh_contact_count() && n < max; i++) {
    MeshContact c; char pref[13];
    if (!mesh_contact_at(i, c, pref)) continue;
    if (want_type != 0xFF && c.type != want_type) continue;
    RouteKind rk = (c.type == ADV_TYPE_ROOM) ? ROUTE_ROOM : ROUTE_DM;
    bool lk = linked_for(rk, String(pref));
    if (unlinked_only && lk) continue;
    String lbl = c.name.length() ? c.name : String(pref);
    if (filter.length()) {
      String l = lbl; l.toLowerCase();
      if (l.indexOf(filter) < 0) continue;
    }
    rows[n++] = { rk, String(pref), lbl, c.last_advert, c.hops, lk };
  }
  return n;
}

// --- actions ---------------------------------------------------------------

// Is this a plausible pubkey prefix? 8 or 12 lowercase hex chars.
static bool is_prefix(const String& t, String& out) {
  String x = t; x.trim(); x.toLowerCase();
  if (x.length() != 8 && x.length() != 12) return false;
  for (size_t i = 0; i < x.length(); i++) {
    char c = x[i];
    if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) return false;
  }
  out = x;
  return true;
}

// Resolve a token to a target. Accepts a listing index or a pubkey prefix.
// A prefix works even for someone NOT in the node's contact list — e.g. a
// stranger whose DM landed in the inbox — because routing only needs the key.
static bool resolve_target(const String& token, RouteKind& kind_out,
                           String& key_out, String& label_out, String& err) {
  String pref;
  if (is_prefix(token, pref)) {
    MeshContact c;
    bool known = mesh_lookup_contact(pref, c);
    kind_out  = (known && c.type == ADV_TYPE_ROOM) ? ROUTE_ROOM : ROUTE_DM;
    key_out   = pref;
    label_out = (known && c.name.length()) ? c.name : pref;
    if (!known) {
      Serial.printf("[admin] %s not in contacts; linking as DM anyway\n",
                    pref.c_str());
    }
    return true;
  }

  int n = token.toInt();
  if (n <= 0) { err = "Give a listing number or a 12-character key prefix."; return false; }
  if (!snap_valid()) { err = "That listing has expired. Run `list` or `find` again."; return false; }
  if ((size_t)n > g_snap_n) {
    err = "No item " + String(n) + " in the last listing (1-" +
          String((int)g_snap_n) + ").";
    return false;
  }
  kind_out  = g_snap[n - 1].kind;
  key_out   = g_snap[n - 1].key;
  label_out = g_snap[n - 1].label;
  return true;
}

static void do_add(const String& token, const String& custom_name) {
  RouteKind kind; String key, label, err;
  if (!resolve_target(token, kind, key, label, err)) { reply(err); return; }

  if (route_find(kind, key)) {
    reply("**" + label + "** is already linked.");
    return;
  }

  String name, topic;
  if (custom_name.length()) {
    name = custom_name;                       // cosmetic only; routing uses key
  } else if (kind == ROUTE_CHANNEL) {
    name = "mesh-" + label;
  } else if (kind == ROUTE_ROOM) {
    name = label;               // the "Room Servers" category says the rest
  } else {
    name = label;               // the "Companion DMs" category says the rest
  }
  topic = (kind == ROUTE_CHANNEL) ? ("MeshCore channel " + key)
        : (kind == ROUTE_ROOM)    ? ("MeshCore room server " + key)
                                  : ("MeshCore DM " + key);

  // If the label sanitises to nothing (an emoji-only name that somehow still
  // yields nothing, or pure punctuation), fall back to something unique rather
  // than a generic word that every such contact would share.
  String id = admin_create_channel(kind, name, topic,
                                   "node-" + key.substring(0, 6));
  if (id.length()) {
    route_put(kind, key, id, label);
    reply("Linked **" + label + "** `" + key + "` -> <#" + id + ">");
  } else {
    reply("Could not create a channel for **" + label +
          "**. Check the bot has Manage Channels, and the 500-channel guild limit.");
  }
}

static void do_remove(const String& token) {
  RouteKind kind; String key, label, err;
  if (!resolve_target(token, kind, key, label, err)) { reply(err); return; }

  // A prefix could be linked as either kind; try both.
  bool gone = route_remove(kind, key);
  if (!gone && kind == ROUTE_DM)   gone = route_remove(ROUTE_ROOM, key);
  if (!gone && kind == ROUTE_ROOM) gone = route_remove(ROUTE_DM, key);

  if (gone) {
    reply("Unlinked **" + label + "** `" + key +
          "`. The Discord channel is left in place - delete it yourself if you "
          "want it gone.");
  } else {
    reply("**" + label + "** was not linked.");
  }
}

// Explicit, never automatic: linking every room server can mean dozens of
// channels, Discord's 50-per-category ceiling, and its strict creation limit.
static void do_sync_rooms(bool confirmed) {
  size_t pending = 0;
  for (size_t i = 0; i < mesh_contact_count(); i++) {
    MeshContact c; char pref[13];
    if (!mesh_contact_at(i, c, pref)) continue;
    if (c.type != ADV_TYPE_ROOM) continue;
    if (!route_find(ROUTE_ROOM, String(pref))) pending++;
  }
  if (pending == 0) { reply("Every known room server is already linked."); return; }

  if (!confirmed) {
    reply("This would create **" + String((int)pending) + "** channels in "
          "**Room Servers** (Discord caps a category at 50) and takes about " +
          String((int)pending * 2) + "s because of rate limits.\n"
          "Send `sync rooms confirm` to proceed.");
    return;
  }

  int created = 0;
  for (size_t i = 0; i < mesh_contact_count() && created < 45; i++) {
    MeshContact c; char pref[13];
    if (!mesh_contact_at(i, c, pref)) continue;
    if (c.type != ADV_TYPE_ROOM) continue;
    String key(pref);
    if (route_find(ROUTE_ROOM, key)) continue;
    String label = c.name.length() ? c.name : key;
    String id = admin_create_channel(ROUTE_ROOM, label, "MeshCore room " + key,
                                     "node-" + key.substring(0, 6));
    if (id.length()) { route_put(ROUTE_ROOM, key, id, label); created++; }
    delay(1500);
  }
  reply("Linked " + String(created) + " room server(s).");
}

// Add a contact from a full public key, for a node seen on the public map that
// adverts cannot reach. Auto-add only ever fires for nodes heard over the air.
static void do_contact_add(const String& rest) {
  String args = rest; args.trim();
  if (args.length() == 0) {
    reply("Usage: `contact add <64-hex-key> [name]`, or add `room` at the end "
          "for a room server.\nThe key is the node's full public key, as shown "
          "on the public map.");
    return;
  }

  int sp = args.indexOf(' ');
  String key  = (sp < 0) ? args : args.substring(0, sp);
  String name = (sp < 0) ? ""   : args.substring(sp + 1);
  name.trim();

  uint8_t type = ADV_TYPE_CHAT;
  String lower_name = name; lower_name.toLowerCase();
  if (lower_name.endsWith(" room") || lower_name == "room") {
    type = ADV_TYPE_ROOM;
    name = name.substring(0, name.length() - 4);
    name.trim();
  }

  key.trim(); key.toLowerCase();
  if (key.length() != 64) {
    reply("That key is " + String((int)key.length()) + " characters. A full "
          "public key is 64 hex characters. The 12-character prefix shown next "
          "to a message is not enough to add a contact, though `add <prefix>` "
          "can still link a channel for them.");
    return;
  }
  for (size_t i = 0; i < key.length(); i++) {
    char c = key[i];
    if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) {
      reply("That key is not hexadecimal.");
      return;
    }
  }

  if (!mesh_connected()) { reply("Not connected to the node right now."); return; }

  if (!mesh_add_contact(key, name, type)) {
    reply("The node rejected that contact.");
    return;
  }
  mesh_refresh_contacts();      // so it shows up in listings straight away

  String shown = name.length() ? name : key.substring(0, 12);
  reply("Added **" + shown + "** as a " +
        String(type == ADV_TYPE_ROOM ? "room server" : "contact") +
        ".\nKey prefix `" + key.substring(0, 12) + "`. "
        "Link a channel for it with `add " + key.substring(0, 12) + "`.");
}

static void do_status() {
  String s = "**Status**\n```\n";
  s += "mesh link   : " + String(mesh_connected() ? "connected" : "DOWN") + "\n";
  s += "contacts    : " + String((int)mesh_contact_count()) + " cached\n";
  int nch = 0;
  for (uint8_t i = 0; i < MESH_MAX_CHANNELS; i++) { String t; if (mesh_channel_at(i,t)) nch++; }
  s += "mesh chans  : " + String(nch) + "\n";
  s += "links       : " + String((int)routes_count()) + "\n";
  s += "free heap   : " + String(ESP.getFreeHeap()) + " (floor " + String(heap_floor()) + ")\n";
  s += "uptime      : " + String(millis() / 60000) + " min\n";
  s += "```";
  reply(s);
}

static void do_help() {
  reply(
    "**MeshyCord commands**\n```\n"
    "list rooms | companions | channels | links\n"
    "find <text>\n"
    "   modifiers: unlinked, recent, name, hops, desc\n"
    "   e.g. list rooms unlinked hops\n"
    "        find ridge name\n"
    "add <n>              link item n from the last listing\n"
    "add <keyprefix>      link by 12-char key, even if not a contact\n"
    "add <n> as <name>    choose the Discord channel name\n"
    "remove <n|keyprefix> unlink\n"
    "\n"
    "contact add <64-hex-key> [name]   add a node seen on the public map\n"
    "contact add <key> <name> room     ...as a room server\n"
    "status\n"
    "sync rooms           link every known room server (asks first)\n"
    "reset                delete everything the bridge created\n"
    "help\n"
    "```\n"
    "_Listings freeze their numbering for 10 minutes, so `add 7` always means "
    "the 7 you saw. Sorted by most-recently-heard by default._\n"
    "_Channel names are cosmetic - routing is by key, so renaming a Discord "
    "channel never breaks anything._");
}

// Defined below, next to admin_bootstrap() which it calls to rebuild the inbox.
static void do_reset(bool confirmed);

// --- entry point -----------------------------------------------------------
void admin_handle(const String& raw) {
  String c = raw;
  c.trim();
  if (c.length() == 0) return;
  String lower = c;
  lower.toLowerCase();

  if (lower == "help" || lower == "?")   { do_help();   return; }
  if (lower == "status")                 { do_status(); return; }
  if (lower == "reset")                  { do_reset(false); return; }
  if (lower == "reset confirm")          { do_reset(true);  return; }
  if (lower == "sync rooms")             { do_sync_rooms(false); return; }
  if (lower == "sync rooms confirm")     { do_sync_rooms(true);  return; }
  if (lower.startsWith("contact add ")) { do_contact_add(c.substring(12)); return; }
  if (lower == "contact add")           { do_contact_add(""); return; }

  if (lower.startsWith("add ")) {
    String rest = c.substring(4); rest.trim();
    String custom;
    // Arduino's String::toLowerCase() mutates and returns void, so it cannot
    // be chained — take a copy to search in.
    String rest_l = rest; rest_l.toLowerCase();
    int as_at = rest_l.indexOf(" as ");
    if (as_at > 0) {
      custom = rest.substring(as_at + 4); custom.trim();
      rest   = rest.substring(0, as_at);  rest.trim();
    }
    do_add(rest, custom);
    return;
  }
  if (lower.startsWith("remove ") || lower.startsWith("rm ")) {
    int sp = c.indexOf(' ');
    String rest = c.substring(sp + 1); rest.trim();
    do_remove(rest);
    return;
  }

  bool is_find = lower.startsWith("find ") || lower.startsWith("search ");
  bool is_list = lower.startsWith("list");
  if (!is_find && !is_list) {
    reply("Unknown command. Try `help`.");
    return;
  }

  // modifiers can appear anywhere in the line
  bool unlinked = lower.indexOf("unlinked") >= 0;
  bool desc     = lower.indexOf("desc") >= 0;
  SortKey sk = SORT_RECENT;
  if (lower.indexOf("name") >= 0) sk = SORT_NAME;
  if (lower.indexOf("hops") >= 0) sk = SORT_HOPS;

  uint8_t want_type = 0xFF;
  bool channels = false, links_only = false;
  String filter, title;

  if (is_list) {
    if (lower.indexOf("room") >= 0)        { want_type = ADV_TYPE_ROOM; title = "Room servers"; }
    else if (lower.indexOf("compan") >= 0) { want_type = ADV_TYPE_CHAT; title = "Companions"; }
    else if (lower.indexOf("chan") >= 0)   { channels = true;           title = "Mesh channels"; }
    else if (lower.indexOf("link") >= 0)   { links_only = true;         title = "Links"; }
    else { reply("`list rooms`, `list companions`, `list channels` or `list links`."); return; }
  } else {
    int sp = c.indexOf(' ');
    filter = c.substring(sp + 1);
    filter.trim();
    // strip trailing modifiers from the search text
    const char* mods[] = { "unlinked", "recent", "name", "hops", "desc" };
    for (auto m : mods) {
      String f = filter; f.toLowerCase();
      int at = f.lastIndexOf(m);
      if (at >= 0 && at + (int)strlen(m) == (int)filter.length())
        filter = filter.substring(0, at);
      filter.trim();
    }
    if (filter.length() == 0) { reply("Usage: `find <text>`"); return; }
    filter.toLowerCase();
    title = "Matching \"" + filter + "\"";
  }

  static Row rows[SNAPSHOT_MAX * 4];
  size_t n = collect(rows, sizeof(rows) / sizeof(rows[0]),
                     want_type, channels, links_only, unlinked, filter);
  if (n == 0) { reply("Nothing matched."); return; }
  if (!channels && !links_only) sort_rows(rows, n, sk, desc);
  render(rows, n, title);
}

bool admin_is_admin_channel(const String& channel_id) {
  return g_settings.admin_channel.length() &&
         channel_id == g_settings.admin_channel;
}

// Reset only what the bridge itself created: linked channels, the per-kind
// categories, and the inbox. The admin channel and the MeshyCord category are kept
// so there is somewhere to report back to, and nothing the user made by hand is
// ever touched.
static void do_reset(bool confirmed) {
  if (!confirmed) {
    reply("This deletes every channel the bridge created — all linked "
          "channels, the **Mesh Channels / Mesh Rooms / Mesh DMs** categories, "
          "and **global-inbox** — and clears all links.\n"
          "Channels you made yourself are untouched.\n\n"
          "Send `reset confirm` to go ahead.");
    return;
  }

  int deleted = 0;
  for (size_t i = 0; i < routes_count(); i++) {
    Route* r = routes_at(i);
    if (discord_delete_channel(r->channel_id)) deleted++;
  }
  routes_clear();

  if (g_settings.inbox_channel.length() &&
      discord_delete_channel(g_settings.inbox_channel)) deleted++;
  g_settings.inbox_channel = "";

  const char* cats[] = { "Channels", "Room Servers", "Companion DMs" };
  for (auto cname : cats) {
    String id = discord_find_or_create_category(cname);
    if (id.length() && discord_delete_channel(id)) deleted++;
  }
  g_cat_chan = ""; g_cat_room = ""; g_cat_dm = "";

  settings_save();
  snap_reset(); g_snap_n = 0;

  // Recreate a clean inbox so the bridge stays usable.
  admin_bootstrap();

  reply("Reset done — deleted " + String(deleted) +
        " channel(s)/category(ies) and cleared all links. "
        "A fresh **global-inbox** has been created.");
}

bool admin_bootstrap() {
  // Re-resolve categories from scratch. Bootstrap also runs at runtime when a
  // channel is found deleted, and by then a cached category id may be stale
  // because that was deleted too. Four extra lookups on a rare path is a fair
  // price for not creating channels with a dead parent.
  admin_forget_categories();

  // Verify the ids we have before trusting them. Channels deleted by hand would
  // otherwise never come back, because the create step is skipped whenever an
  // id is stored.
  if (g_settings.admin_channel.length() &&
      !discord_channel_exists(g_settings.admin_channel)) {
    Serial.println("[admin] stored admin channel is gone; will recreate");
    g_settings.admin_channel = "";
  }
  if (g_settings.inbox_channel.length() &&
      !discord_channel_exists(g_settings.inbox_channel)) {
    Serial.println("[admin] stored inbox channel is gone; will recreate");
    g_settings.inbox_channel = "";
  }

  if (g_settings.guild_id.length() == 0) {
    Serial.println("[admin] no server (guild) id set - enter it in the web UI");
    return false;
  }

  // Categories up front, so the server has its shape before any channel lands.
  String cat = cat_cached(g_cat_admin, "MeshyCord");
  admin_category_for(ROUTE_CHANNEL);
  admin_category_for(ROUTE_ROOM);
  admin_category_for(ROUTE_DM);

  if (!admin_ensure_channel()) return false;

  // Inbox: create our own rather than asking the user to repurpose #general.
  //
  // NOTE: only ever move a channel WE created. Moving one that was already
  // configured relocates a channel the user owns — an earlier version did this
  // unconditionally and dragged a repurposed #general into its own category.
  if (g_settings.inbox_channel.length() == 0) {
    String id = discord_create_channel(
        "global-inbox",
        "Unrouted mesh traffic. Reply as: <12-char-key> your message", cat);
    if (id.length() == 0) return false;
    g_settings.inbox_channel = id;
    settings_save();
    Serial.printf("[admin] created #global-inbox -> %s\n", id.c_str());
  }

  // Drop links whose Discord channel no longer exists. Otherwise a deleted
  // channel keeps its route, the auto-link step skips it as "already linked",
  // and it only recovers after a poll returns 404.
  for (size_t i = 0; i < routes_count(); ) {
    Route* r = routes_at(i);
    if (!r) break;
    if (!discord_channel_exists(r->channel_id)) {
      Serial.printf("[admin] link %s -> %s is dead, removing\n",
                    r->key.c_str(), r->channel_id.c_str());
      route_remove(r->kind, r->key);       // shifts the table; do not advance
      continue;
    }
    i++;
  }

  return true;
}

void admin_sync_after_mesh() {
  // Drop channel links whose slot does not exist on the node. A phantom route
  // can be left behind by a mis-attributed CMD_GET_CHANNEL reply (a timed-out
  // query for slot N answered during the query for slot N+1), which produced a
  // duplicate Discord channel for a slot that was never real.
  for (size_t i = 0; i < routes_count(); ) {
    Route* r = routes_at(i);
    if (!r) break;
    if (r->kind == ROUTE_CHANNEL) {
      String nm;
      uint8_t slot = (uint8_t)r->key.toInt();
      if (!mesh_channel_at(slot, nm)) {
        Serial.printf("[admin] channel slot %u is not a real channel; "
                      "removing phantom link (Discord channel %s left orphaned)\n",
                      (unsigned)slot, r->channel_id.c_str());
        route_remove(ROUTE_CHANNEL, r->key);
        continue;                      // table shifted
      }
    }
    i++;
  }

  if (!g_settings.autocreate_channels) return;

  int created = 0;
  for (uint8_t i = 0; i < MESH_MAX_CHANNELS; i++) {
    String cname;
    if (!mesh_channel_at(i, cname)) continue;
    String key = String((int)i);
    if (route_find(ROUTE_CHANNEL, key)) continue;

    String id = admin_create_channel(ROUTE_CHANNEL, cname,
                                     "MeshCore channel " + key + " (" + cname + ")");
    if (id.length()) {
      route_put(ROUTE_CHANNEL, key, id, cname);
      created++;
      Serial.printf("[admin] auto-linked channel %u '%s'\n",
                    (unsigned)i, cname.c_str());
      delay(1200);        // be gentle with Discord's channel-creation limit
    }
  }
  if (created) {
    reply("Auto-linked " + String(created) +
          " mesh channel(s). Room servers are not bulk-created — there are "
          "often dozens. Use `list rooms unlinked` then `add <n>`, or "
          "`sync rooms` to link them all.");
  }
}

bool admin_ensure_channel() {
  if (g_settings.admin_channel.length()) return true;
  String cat = cat_cached(g_cat_admin, "MeshyCord");
  // Adopt an existing #meshycord-admin by name if present, but leave it where the
  // user put it — only channels we create get placed in a category.
  String id = discord_find_channel("meshycord-admin");
  if (id.length() == 0) {
    id = discord_create_channel("meshycord-admin",
                                "Type commands here. `help` for the list.", cat);
  }
  if (id.length() == 0) {
    Serial.println("[admin] could not find or create #meshycord-admin");
    return false;
  }
  g_settings.admin_channel = id;
  settings_save();
  Serial.printf("[admin] #meshycord-admin -> %s\n", id.c_str());
  return true;
}

void admin_announce_ready() {
  reply("Bridge online. `help` for commands.");
}

String admin_rediscover() {
  Serial.println("[admin] rediscover: clearing Discord-side state");

  g_settings.admin_channel = "";
  g_settings.inbox_channel = "";
  settings_save();
  routes_clear();

  // Drop cached category ids: the categories may have been deleted by hand.
  g_cat_admin = ""; g_cat_chan = ""; g_cat_room = ""; g_cat_dm = "";

  if (!admin_bootstrap()) {
    return "Bootstrap failed. Check the bot token, and that the bot is in "
           "exactly one server (or set the guild ID manually).";
  }
  admin_announce_ready();
  admin_sync_after_mesh();

  String s = "Rediscovered.\n";
  s += "guild   : " + g_settings.guild_id + "\n";
  s += "admin   : " + g_settings.admin_channel + "\n";
  s += "inbox   : " + g_settings.inbox_channel + "\n";
  s += "links   : " + String((int)routes_count());
  return s;
}
