#include "routing.h"
#include <Preferences.h>

static Route  g_routes[MAX_ROUTES];
static size_t g_count = 0;

static const char* NS = "meshy_rt";

size_t routes_count()       { return g_count; }
Route* routes_at(size_t i)  { return (i < g_count) ? &g_routes[i] : nullptr; }

Route* route_find(RouteKind kind, const String& key) {
  for (size_t i = 0; i < g_count; i++)
    if (g_routes[i].kind == kind && g_routes[i].key == key) return &g_routes[i];
  return nullptr;
}

Route* route_put(RouteKind kind, const String& key,
                 const String& channel_id, const String& label) {
  Route* r = route_find(kind, key);
  if (!r) {
    if (g_count >= MAX_ROUTES) {
      Serial.println("[route] table full, refusing new route");
      return nullptr;
    }
    r = &g_routes[g_count++];
    r->kind = kind;
    r->key  = key;
    r->last_discord_id = "";
    r->hot_until = 0;
  }
  if (channel_id.length()) r->channel_id = channel_id;
  if (label.length())      r->label      = label;
  r->last_seen = millis() / 1000;
  routes_save();
  return r;
}

bool route_remove(RouteKind kind, const String& key) {
  for (size_t i = 0; i < g_count; i++) {
    if (g_routes[i].kind == kind && g_routes[i].key == key) {
      for (size_t j = i; j + 1 < g_count; j++) g_routes[j] = g_routes[j + 1];
      g_count--;
      routes_save();
      return true;
    }
  }
  return false;
}

static String g_inbox_cursor;
static bool   g_inbox_cursor_loaded = false;

String inbox_cursor_get() {
  if (!g_inbox_cursor_loaded) {
    Preferences p;
    p.begin(NS, true);
    g_inbox_cursor = p.getString("inbox_cur", "");
    p.end();
    g_inbox_cursor_loaded = true;
  }
  return g_inbox_cursor;
}

void inbox_cursor_set(const String& id) {
  if (id.length() == 0 || id == g_inbox_cursor) return;
  g_inbox_cursor = id;
  g_inbox_cursor_loaded = true;
  Preferences p;
  p.begin(NS, false);
  p.putString("inbox_cur", id);
  p.end();
}

void routes_clear() {
  g_count = 0;
  routes_save();
}

// Stored as one packed string per slot: kind|key|channel_id|label|last_discord_id
void routes_save() {
  Preferences p;
  p.begin(NS, false);
  p.putUInt("n", g_count);
  for (size_t i = 0; i < g_count; i++) {
    char k[8];
    snprintf(k, sizeof(k), "r%u", (unsigned)i);
    String v = String((int)g_routes[i].kind) + "|" + g_routes[i].key + "|" +
               g_routes[i].channel_id + "|" + g_routes[i].label + "|" +
               g_routes[i].last_discord_id;
    p.putString(k, v);
  }
  p.end();
}

static String field(const String& s, int idx, int& pos_io) {
  int start = pos_io;
  int bar = s.indexOf('|', start);
  if (bar < 0) { pos_io = s.length(); return s.substring(start); }
  pos_io = bar + 1;
  return s.substring(start, bar);
}

void routes_load() {
  Preferences p;
  p.begin(NS, true);
  uint32_t n = p.getUInt("n", 0);
  if (n > MAX_ROUTES) n = MAX_ROUTES;
  g_count = 0;
  for (uint32_t i = 0; i < n; i++) {
    char k[8];
    snprintf(k, sizeof(k), "r%u", (unsigned)i);
    String v = p.getString(k, "");
    if (v.length() == 0) continue;
    int pos = 0;
    String kind_s = field(v, 0, pos);
    Route& r = g_routes[g_count];
    r.kind            = (RouteKind)kind_s.toInt();
    r.key             = field(v, 1, pos);
    r.channel_id      = field(v, 2, pos);
    r.label           = field(v, 3, pos);
    r.last_discord_id = field(v, 4, pos);
    r.last_seen       = 0;
    r.hot_until       = 0;
    if (r.key.length() && r.channel_id.length()) g_count++;
  }
  p.end();
  Serial.printf("[route] loaded %u routes\n", (unsigned)g_count);
}
