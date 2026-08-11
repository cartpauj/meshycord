package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"

	"meshycord/internal/meshcore"
)

const limit = meshcore.MaxMsgLen

func TestShortTextIsOneUnmarkedChunk(t *testing.T) {
	got := Chunk("hello", limit, 3)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got %q; a single transmission must carry no [i/n] marker", got)
	}
	if ChunkCount("hello", limit) != 1 {
		t.Error("count disagrees with the splitter")
	}
}

func TestExactlyAtTheLimitStaysOneChunk(t *testing.T) {
	text := strings.Repeat("a", limit)
	got := Chunk(text, limit, 3)
	if len(got) != 1 {
		t.Fatalf("%d bytes split into %d chunks; the limit is inclusive", len(text), len(got))
	}
	if got[0] != text {
		t.Error("text was altered")
	}
}

// The bug this pins down: estimating the chunk count as len/133 ignores the 8
// bytes each chunk reserves for its marker, so a ~390-character message passed
// a "fits in 3" check and was then silently truncated.
func TestCountMatchesWhatTheSplitterActuallyProduces(t *testing.T) {
	for _, n := range []int{1, 100, 133, 134, 250, 375, 376, 390, 500, 1000} {
		text := strings.Repeat("a", n)
		want := ChunkCount(text, limit)
		got := Chunk(text, limit, 64)
		if len(got) != want {
			t.Errorf("%d bytes: ChunkCount said %d, splitter produced %d", n, want, len(got))
		}
	}
}

func TestNoChunkExceedsTheWireLimit(t *testing.T) {
	text := strings.Repeat("abcdefghij", 100)
	for _, c := range Chunk(text, limit, 64) {
		if len(c) > limit {
			t.Errorf("chunk of %d bytes exceeds the %d-byte limit: %q", len(c), limit, c[:40])
		}
	}
}

func TestSplitNeverBreaksAUTF8Character(t *testing.T) {
	// Emoji are 4 bytes each, so a naive byte split lands mid-character
	// constantly.
	text := strings.Repeat("🏔", 200)
	chunks := Chunk(text, limit, 64)
	if len(chunks) < 2 {
		t.Fatal("expected a split")
	}
	rejoined := ""
	for _, c := range chunks {
		body, ok := StripChunkMarker(c)
		if !ok {
			t.Fatalf("chunk has no marker: %q", c)
		}
		if !utf8.ValidString(body) {
			t.Errorf("chunk is not valid UTF-8: %q", body)
		}
		rejoined += body
	}
	if rejoined != text {
		t.Error("rejoined chunks do not reproduce the original text")
	}
}

func TestSplitIsLosslessForASCII(t *testing.T) {
	text := strings.Repeat("the quick brown fox ", 40)
	rejoined := ""
	for _, c := range Chunk(text, limit, 64) {
		body, ok := StripChunkMarker(c)
		if !ok {
			t.Fatalf("no marker on %q", c)
		}
		rejoined += body
	}
	if rejoined != text {
		t.Errorf("lost data: %d bytes in, %d out", len(text), len(rejoined))
	}
}

func TestChunkMarkersAreNumberedAgainstTheRealTotal(t *testing.T) {
	// Even when maxChunks stops us early, the marker must report the true
	// total — "[1/5]" tells the reader four more were needed, where "[1/3]"
	// would claim the message was complete.
	text := strings.Repeat("a", 1000)
	total := ChunkCount(text, limit)
	got := Chunk(text, limit, 2)
	if len(got) != 2 {
		t.Fatalf("maxChunks not honoured: got %d", len(got))
	}
	wantSuffix := "/" + itoa(total) + "]"
	if !strings.Contains(got[0], wantSuffix) {
		t.Errorf("marker %q does not report the real total of %d", got[0][:10], total)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestChunkCapacityMatchesWhatActuallyFits(t *testing.T) {
	for _, max := range []int{1, 2, 3, 5} {
		cap := ChunkCapacity(limit, max)
		text := strings.Repeat("a", cap)
		if got := ChunkCount(text, limit); got > max {
			t.Errorf("maxChunks=%d: capacity says %d bytes fit, but that needs %d chunks",
				max, cap, got)
		}
		// One byte more should not fit.
		if got := ChunkCount(text+"a", limit); got <= max && max > 1 {
			t.Errorf("maxChunks=%d: capacity of %d is understated; %d bytes still fit in %d",
				max, cap, cap+1, got)
		}
	}
}

func TestStripChunkMarker(t *testing.T) {
	for _, tc := range []struct {
		in       string
		want     string
		stripped bool
	}{
		{"[1/3] hello", "hello", true},
		{"[2/3] more", "more", true},
		{"[10/10] last", "last", true},
		{"plain text", "plain text", false},
		{"[not a marker] text", "[not a marker] text", false},
		{"[1/] text", "[1/] text", false},
		{"[/3] text", "[/3] text", false},
		{"[13] text", "[13] text", false},
		{"[1/2/3] text", "[1/2/3] text", false},
		{"", "", false},
		{"[1/3]nospace", "[1/3]nospace", false},
	} {
		got, ok := StripChunkMarker(tc.in)
		if ok != tc.stripped || got != tc.want {
			t.Errorf("StripChunkMarker(%q) = %q,%v; want %q,%v", tc.in, got, ok, tc.want, tc.stripped)
		}
	}
}

func TestChunkAndStripRoundTrip(t *testing.T) {
	text := strings.Repeat("mesh ", 100)
	for _, c := range Chunk(text, limit, 64) {
		body, ok := StripChunkMarker(c)
		if !ok {
			t.Fatalf("could not strip the marker the splitter just added: %q", c)
		}
		if strings.HasPrefix(body, "[") {
			t.Errorf("marker survived stripping: %q", body)
		}
	}
}

func TestEmptyTextProducesNothing(t *testing.T) {
	if got := Chunk("", limit, 3); len(got) != 0 {
		t.Errorf("empty text produced %d chunks", len(got))
	}
	if ChunkCount("", limit) != 0 {
		t.Error("empty text counted as a transmission")
	}
}
