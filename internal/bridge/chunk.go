package bridge

import (
	"fmt"
	"unicode/utf8"
)

// Message splitting.
//
// A mesh message is meshcore.MaxMsgLen bytes, and less than that on a group
// channel. Longer text becomes several transmissions, and rather than hiding
// that, each one is echoed into Discord as its own message and tracked
// separately — so you can see exactly how much airtime you used and which
// transmissions actually landed.
//
// Two rules here are not obvious and both were learned the hard way:
//
//   - Never split mid-character. Cutting a UTF-8 sequence in half produces
//     invalid bytes that render as mojibake whatever charset is declared.
//   - Ask the splitter how many chunks it needs; never estimate. Computing
//     len/limit under-counted, because splitting reserves 8 bytes per chunk for
//     the "[i/n] " prefix — so a ~390 character message passed the "will this
//     fit in 3?" check and was then silently truncated. Silent truncation is
//     the worst possible outcome: it looks like it sent.

// chunkPrefixBudget is the worst-case width of the "[10/10] " marker. Single
// transmissions carry no marker, so this only applies once text is split.
const chunkPrefixBudget = 8

func bodyLimit(limit int) int {
	if limit > chunkPrefixBudget {
		return limit - chunkPrefixBudget
	}
	return limit
}

// backOffToUTF8Boundary walks back off continuation bytes (10xxxxxx).
func backOffToUTF8Boundary(s string, end int) int {
	for end > 0 && end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	return end
}

// sliceOffsets computes where each chunk starts and ends, using exactly the
// rule the splitter itself uses. Everything else is derived from this, so a
// "will it fit" check can never disagree with what actually happens.
func sliceOffsets(text string, limit int) [][2]int {
	if limit <= 0 || text == "" {
		return nil
	}
	if len(text) <= limit {
		return [][2]int{{0, len(text)}}
	}
	body := bodyLimit(limit)
	var out [][2]int
	pos := 0
	// 64 is a runaway guard, far above any message that would be accepted.
	for pos < len(text) && len(out) < 64 {
		take := body
		if pos+take >= len(text) {
			take = len(text) - pos
		} else {
			end := backOffToUTF8Boundary(text, pos+take)
			if end <= pos {
				end = pos + take // pathological: a single rune longer than a chunk
			}
			take = end - pos
		}
		out = append(out, [2]int{pos, pos + take})
		pos += take
	}
	return out
}

// ChunkCount is how many transmissions this text needs. Same rule as Chunk, so
// the two can never disagree.
func ChunkCount(text string, limit int) int { return len(sliceOffsets(text, limit)) }

// ChunkCapacity is the largest plain-ASCII message that fits in maxChunks
// transmissions. Used to tell the user what "shorter" actually means.
func ChunkCapacity(limit, maxChunks int) int {
	if maxChunks <= 1 {
		return limit // one transmission carries no marker
	}
	return bodyLimit(limit) * maxChunks
}

// Chunk splits text into transmissions, prefixing "[i/n] " when there is more
// than one. It never returns more than maxChunks pieces; callers must check
// ChunkCount first and refuse rather than accept a truncated send.
func Chunk(text string, limit, maxChunks int) []string {
	offsets := sliceOffsets(text, limit)
	if len(offsets) == 0 {
		return nil
	}
	n := len(offsets)
	if n == 1 {
		return []string{text[offsets[0][0]:offsets[0][1]]}
	}
	if n > maxChunks {
		n = maxChunks
	}
	out := make([]string, 0, n)
	total := len(offsets)
	for i := 0; i < n; i++ {
		piece := text[offsets[i][0]:offsets[i][1]]
		out = append(out, fmt.Sprintf("[%d/%d] %s", i+1, total, piece))
	}
	return out
}

// StripChunkMarker removes a "[2/3] " prefix.
//
// This is what makes replying `retry` to one failed piece of a split message
// resend exactly that piece, rather than the whole thing over again.
func StripChunkMarker(s string) (string, bool) {
	if len(s) < 6 || s[0] != '[' {
		return s, false
	}
	close := -1
	for i := 1; i < len(s)-1 && i <= 8; i++ {
		if s[i] == ']' && s[i+1] == ' ' {
			close = i
			break
		}
	}
	if close < 3 {
		return s, false
	}
	slash := -1
	for i := 1; i < close; i++ {
		switch {
		case s[i] == '/':
			if slash >= 0 {
				return s, false
			}
			slash = i
		case s[i] < '0' || s[i] > '9':
			return s, false
		}
	}
	// There must be at least one digit on each side of the slash, so "[/3] "
	// and "[1/] " are not mistaken for markers.
	if slash < 2 || slash == close-1 {
		return s, false
	}
	return s[close+2:], true
}
