#pragma once

#include <Arduino.h>

// Heap instrumentation. The C3 has no PSRAM, so the free-heap floor across a
// bonded BLE link plus a TLS session is the number that decides whether this
// board is viable. Logged at every phase transition and around each request.
void     heap_log(const char* where);
uint32_t heap_floor();

// Escape a string for embedding in a JSON string literal.
String json_escape(const String& in);

// Split text into <=limit-byte chunks on UTF-8 boundaries, prefixing
// "[i/n] " when more than one chunk is needed. Returns the number of chunks
// written to out[] (at most max_chunks).
size_t chunk_text(const String& text, size_t limit,
                  String* out, size_t max_chunks);

// How many chunks chunk_text() would actually produce, with no cap. Uses the
// SAME splitting rule, so a caller's "will this fit?" check cannot disagree with
// what the splitter then does — estimating it separately let a message pass the
// check and still be truncated.
size_t chunk_count(const String& text, size_t limit);

// Largest plain-ASCII message that fits in `max_chunks` transmissions.
size_t chunk_capacity(size_t limit, size_t max_chunks);

// Truncate to at most max_bytes WITHOUT splitting a UTF-8 sequence. Cutting
// mid-character produces invalid bytes that render as mojibake regardless of
// the declared charset.
String utf8_truncate(const String& in, size_t max_bytes);

// Watchdog. Recovery so far only handled failures we DETECT; a hang (wedged TLS
// socket, deadlocked BLE stack) left the device silently dead. The watchdog
// reboots it instead. feed() must be called from any loop that can run longer
// than the timeout — contact enumeration and a poll sweep both can.
// Breadcrumb for hangs. The watchdog reboots the device but says nothing about
// WHERE it was stuck, and a RISC-V panic gives no usable call trace — so record
// the last place we got to, and print it when a stage takes suspiciously long.
void stage_set(const char* where, int line);
#define STAGE(w) stage_set((w), __LINE__)

void watchdog_begin(uint32_t timeout_s);
void watchdog_feed();

// Deliberate restart when the heap stays critically low: a controlled reboot
// beats a random allocation failure somewhere unpredictable.
//
// Checks the largest free BLOCK as well as the total, because fragmentation is
// what actually kills these allocations — a 20KB request fails when no single
// block is that big, however healthy the total looks. `min_block` defaults to
// half of `min_free`; pass it explicitly to be stricter.
void heap_guard_check(uint32_t min_free, uint32_t min_block = 0);
