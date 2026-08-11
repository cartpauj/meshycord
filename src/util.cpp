#include "util.h"
#include <esp_heap_caps.h>
#include <esp_task_wdt.h>

static uint32_t g_floor = UINT32_MAX;

void heap_log(const char* where) {
  uint32_t free_now = ESP.getFreeHeap();
  uint32_t largest  = heap_caps_get_largest_free_block(MALLOC_CAP_8BIT);
  if (free_now < g_floor) g_floor = free_now;
  Serial.printf("[heap] %-24s free=%6u largest=%6u floor=%6u\n",
                where, free_now, largest, g_floor);
}

uint32_t heap_floor() { return g_floor; }

String json_escape(const String& in) {
  String o;
  o.reserve(in.length() + 8);
  for (size_t i = 0; i < in.length(); i++) {
    uint8_t c = (uint8_t)in[i];
    switch (c) {
      case '"':  o += "\\\""; break;
      case '\\': o += "\\\\"; break;
      case '\n': o += "\\n";  break;
      case '\r': o += "\\r";  break;
      case '\t': o += "\\t";  break;
      default:
        if (c < 0x20) {        // other control chars -> \u00XX
          char buf[7];
          snprintf(buf, sizeof(buf), "\\u%04X", c);
          o += buf;
        } else {
          o += (char)c;        // pass UTF-8 bytes through untouched
        }
    }
  }
  return o;
}

// Don't split in the middle of a UTF-8 sequence: continuation bytes are 10xxxxxx.
static size_t back_off_to_utf8_boundary(const String& s, size_t end) {
  while (end > 0 && ((uint8_t)s[end] & 0xC0) == 0x80) end--;
  return end;
}

String utf8_truncate(const String& in, size_t max_bytes) {
  if (in.length() <= max_bytes) return in;
  size_t end = max_bytes;
  // step back off continuation bytes (10xxxxxx)
  while (end > 0 && ((uint8_t)in[end] & 0xC0) == 0x80) end--;
  return in.substring(0, end);
}

// Bytes of payload per chunk once the "[i/n] " prefix is accounted for. Single
// chunks carry no prefix, so this only applies when splitting.
static size_t chunk_body_limit(size_t limit) {
  const size_t prefix_budget = 8;             // "[10/10] " worst case
  return (limit > prefix_budget) ? (limit - prefix_budget) : limit;
}

size_t chunk_count(const String& text, size_t limit) {
  if (limit == 0) return 0;
  if (text.length() <= limit) return 1;
  size_t body = chunk_body_limit(limit);
  size_t pos = 0, n = 0;
  while (pos < text.length() && n < 64) {
    size_t take = body;
    if (pos + take >= text.length()) {
      take = text.length() - pos;
    } else {
      size_t end = back_off_to_utf8_boundary(text, pos + take);
      if (end <= pos) end = pos + take;
      take = end - pos;
    }
    pos += take;
    n++;
  }
  return n;
}

size_t chunk_capacity(size_t limit, size_t max_chunks) {
  if (max_chunks <= 1) return limit;          // one chunk needs no prefix
  return chunk_body_limit(limit) * max_chunks;
}

size_t chunk_text(const String& text, size_t limit,
                  String* out, size_t max_chunks) {
  if (limit == 0 || max_chunks == 0) return 0;

  if (text.length() <= limit) {
    out[0] = text;
    return 1;
  }

  // Two passes. The first slices on UTF-8 boundaries, which makes real chunk
  // sizes smaller than the nominal limit; computing the count arithmetically
  // (as an earlier version did) under-counted and silently dropped the tail.
  size_t body_limit = chunk_body_limit(limit);

  size_t starts[16], lens[16], n = 0;
  size_t pos = 0;
  while (pos < text.length() && n < max_chunks && n < 16) {
    size_t take = body_limit;
    if (pos + take >= text.length()) {
      take = text.length() - pos;
    } else {
      size_t end = back_off_to_utf8_boundary(text, pos + take);
      if (end <= pos) end = pos + take;           // pathological, take raw
      take = end - pos;
    }
    starts[n] = pos;
    lens[n]   = take;
    pos += take;
    n++;
  }

  bool truncated = (pos < text.length());
  for (size_t i = 0; i < n; i++) {
    String piece = text.substring(starts[i], starts[i] + lens[i]);
    if (n > 1) {
      out[i] = "[" + String((int)i + 1) + "/" + String((int)n) + "] " + piece;
    } else {
      out[i] = piece;
    }
  }
  if (truncated) {
    Serial.printf("[chunk] message truncated: %u of %u bytes sent in %u chunks\n",
                  (unsigned)pos, (unsigned)text.length(), (unsigned)n);
  }
  return n;
}

void watchdog_begin(uint32_t timeout_s) {
#if ESP_ARDUINO_VERSION_MAJOR >= 3
  esp_task_wdt_config_t cfg = { .timeout_ms = timeout_s * 1000,
                                .idle_core_mask = 0, .trigger_panic = true };
  esp_task_wdt_init(&cfg);
#else
  esp_task_wdt_init(timeout_s, true);
#endif
  esp_task_wdt_add(NULL);
  Serial.printf("[wdt] watchdog armed, %us timeout\n", timeout_s);
}

void watchdog_feed() {
  esp_task_wdt_reset();
}

void heap_guard_check(uint32_t min_free) {
  static int strikes = 0;
  uint32_t f = ESP.getFreeHeap();
  if (f < min_free) {
    strikes++;
    Serial.printf("[heap] LOW: %u free (< %u), strike %d/3\n", f, min_free, strikes);
    if (strikes >= 3) {
      Serial.println("[heap] exhausted - restarting deliberately");
      delay(200);
      ESP.restart();
    }
  } else if (strikes) {
    strikes = 0;
  }
}
