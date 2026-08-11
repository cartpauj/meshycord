package bridge

import (
	"strings"
	"testing"

	"meshycord/internal/meshcore"
)

// The wording here is not cosmetic. MeshCore's "direct route" means the sender
// used a STORED PATH, which can be many hops and says nothing about distance —
// calling that "direct" read as "nearby" and was actively misleading for a
// station hundreds of miles away.
func TestInboundMetaWordsHopsHonestly(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  meshcore.Message
		want string
		bad  string
	}{
		{
			name: "stored path: hop count does not apply",
			msg:  meshcore.Message{PathRaw: 0xFF, HaveHops: false, PubKeyPrefix: "aabbccddeeff"},
			want: "via known path",
			bad:  "direct",
		},
		{
			name: "flooded with no repeaters: genuinely adjacent",
			msg:  meshcore.Message{HaveHops: true, Hops: 0, PubKeyPrefix: "aabbccddeeff"},
			want: "heard direct",
		},
		{
			name: "one hop is singular",
			msg:  meshcore.Message{HaveHops: true, Hops: 1, PubKeyPrefix: "aabbccddeeff"},
			want: "1 hop_",
		},
		{
			name: "several hops",
			msg:  meshcore.Message{HaveHops: true, Hops: 7, PubKeyPrefix: "aabbccddeeff"},
			want: "7 hops",
		},
	} {
		got := FormatInbound(tc.msg, "Alice")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not contain %q", tc.name, got, tc.want)
		}
		if tc.bad != "" && strings.Contains(got, tc.bad) {
			t.Errorf("%s: %q contains the misleading word %q", tc.name, got, tc.bad)
		}
	}
}

func TestInboundShowsSNRAndTheKey(t *testing.T) {
	m := meshcore.Message{
		PubKeyPrefix: "aabbccddeeff", HaveHops: true, Hops: 2, HaveSNR: true, SNR: -7.25,
	}
	got := FormatInbound(m, "Alice")
	if !strings.Contains(got, "snr -7.2") {
		t.Errorf("SNR missing from %q", got)
	}
	// The key is shown so it can be linked later without retyping hex.
	if !strings.Contains(got, "`aabbccddeeff`") {
		t.Errorf("key prefix missing from %q", got)
	}
	if !strings.Contains(got, "**Alice**") {
		t.Errorf("name missing from %q", got)
	}
}

// MeshCore embeds the sender of a group message in the text as "Name: body".
// The Discord channel already says which mesh channel it is, so repeating that
// on every line is noise — what matters is who sent it.
func TestChannelMessageSplitsTheEmbeddedSender(t *testing.T) {
	m := meshcore.Message{IsChannel: true, ChannelIdx: 0, HaveHops: true, Hops: 1,
		Text: "Alice: heading up the ridge"}
	got := FormatInbound(m, "Public")
	if !strings.Contains(got, "**Alice**") {
		t.Errorf("sender not pulled out: %q", got)
	}
	if !strings.Contains(got, "heading up the ridge") {
		t.Errorf("body lost: %q", got)
	}

	// A colon deep inside the body is not a sender.
	long := "this is a long message that happens to contain a colon: right here"
	m.Text = long
	got = FormatInbound(m, "Public")
	if strings.Contains(got, "**this is a long message") {
		t.Errorf("a mid-body colon was mistaken for a sender: %q", got)
	}
	if !strings.Contains(got, long) {
		t.Errorf("body altered: %q", got)
	}

	// No embedded sender at all.
	m.Text = "just some text"
	if got := FormatInbound(m, "Public"); !strings.HasPrefix(got, "just some text") {
		t.Errorf("plain channel text was reformatted: %q", got)
	}
}

// A room post carries its own author, separate from the room itself.
func TestRoomPostAttributesItsAuthor(t *testing.T) {
	m := meshcore.Message{PubKeyPrefix: "b2b2b2b2b2b2", AuthorPrefix: "cafebabe",
		TxtType: 2, Text: "the meeting is at seven"}
	body := FormatInboundBody(m, "Bob")
	if !strings.Contains(body, "**Bob**: the meeting is at seven") {
		t.Errorf("author not attributed: %q", body)
	}
	// With no resolvable author, the text stands alone rather than gaining an
	// empty name.
	if body := FormatInboundBody(m, ""); body != "the meeting is at seven" {
		t.Errorf("unattributed body = %q", body)
	}
}

func TestTakeRoutePrefix(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantText string
		wantWish RouteWish
	}{
		{"path:flood hello", "hello", RouteForceFlood},
		{"path:direct hello", "hello", RouteForceDirect},
		{"PATH:FLOOD hello", "hello", RouteForceFlood},
		{"path:flood\thello", "hello", RouteForceFlood},
		{"path:flood    lots of space", "lots of space", RouteForceFlood},
		{"path:flood", "", RouteForceFlood},
		{"hello", "hello", RouteAuto},
		// A message that merely begins with those letters must not be
		// swallowed.
		{"path:flooding the area", "path:flooding the area", RouteAuto},
		{"path:directions to the trailhead", "path:directions to the trailhead", RouteAuto},
		{"pathological", "pathological", RouteAuto},
		{"", "", RouteAuto},
	} {
		text, wish := TakeRoutePrefix(tc.in)
		if text != tc.wantText || wish != tc.wantWish {
			t.Errorf("TakeRoutePrefix(%q) = %q,%v; want %q,%v",
				tc.in, text, wish, tc.wantText, tc.wantWish)
		}
	}
}

// A 🔄 resend asks for a flood without a prefix to type, because repeating a
// message down the path that just failed it is the retry least likely to work.
// The table also pins the two ways that must NOT happen: an explicit prefix
// overriding it, and a channel where there is no path to clear.
func TestResolveRouteWish(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           string
		forceFlood   bool
		isChannel    bool
		wantText     string
		wantWish     RouteWish
		wantComplain bool
	}{
		{"plain message, no force", "hello", false, false, "hello", RouteAuto, false},
		{"resend forces a flood", "hello", true, false, "hello", RouteForceFlood, false},
		{"prefix still works without force", "path:flood hi", false, false, "hi", RouteForceFlood, false},
		// The point of the precedence rule: typing path:direct and then hitting
		// 🔄 means "try the known path again", not "flood it after all".
		{"explicit direct beats a forced flood", "path:direct hi", true, false, "hi", RouteForceDirect, false},
		{"explicit flood plus force is still a flood", "path:flood hi", true, false, "hi", RouteForceFlood, false},
		// A channel message is not addressed to a contact, so there is no
		// stored path either way.
		{"channel resend floods silently", "hello", true, true, "hello", RouteAuto, false},
		{"channel with a prefix is told why", "path:flood hi", false, true, "hi", RouteAuto, true},
		{"channel with a prefix and force is still told", "path:direct hi", true, true, "hi", RouteAuto, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, wish, complain := ResolveRouteWish(tc.in, tc.forceFlood, tc.isChannel)
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if wish != tc.wantWish {
				t.Errorf("wish = %v, want %v", wish, tc.wantWish)
			}
			if complain != tc.wantComplain {
				t.Errorf("complain = %v, want %v", complain, tc.wantComplain)
			}
		})
	}
}

// The prefix must not cost any of the 133-byte transmission budget.
func TestRoutePrefixIsFreeOfTheByteBudget(t *testing.T) {
	body := strings.Repeat("a", meshcore.MaxMsgLen)
	text, wish := TakeRoutePrefix("path:flood " + body)
	if wish != RouteForceFlood {
		t.Fatal("prefix not recognised")
	}
	if len(text) != meshcore.MaxMsgLen {
		t.Errorf("text is %d bytes after stripping, want %d", len(text), meshcore.MaxMsgLen)
	}
	if ChunkCount(text, meshcore.MaxMsgLen) != 1 {
		t.Error("a full-length message with a prefix no longer fits in one transmission")
	}
}

// Discord allows unicode and emoji in channel names. An earlier version
// stripped every non-ASCII byte, which was stricter than Discord for no
// reason: it turned "Russet🥔 Room" into "russet-room" and an emoji-only name
// into nothing at all.
func TestSanitizeChannelName(t *testing.T) {
	for _, tc := range []struct {
		in       string
		fallback string
		want     string
	}{
		{"Public", "", "public"},
		{"Ridge Room", "", "ridge-room"},
		{"UPPER CASE", "", "upper-case"},
		{"lots   of   spaces", "", "lots-of-spaces"},
		{"trailing---", "", "trailing"},
		{"---leading", "", "leading"},
		{"Russet🥔 Room", "", "russet🥔-room"},
		{"Café", "", "café"},
		{"🏔", "", "🏔"},
		{"...", "node-abc123", "node-abc123"},
		{"", "node-abc123", "node-abc123"},
		{"", "", "mesh"},
		{"!!!", "", "mesh"},
	} {
		if got := SanitizeChannelName(tc.in, tc.fallback); got != tc.want {
			t.Errorf("SanitizeChannelName(%q, %q) = %q, want %q", tc.in, tc.fallback, got, tc.want)
		}
	}
}

func TestSanitizeChannelNameStaysInsideDiscordsLimit(t *testing.T) {
	// Discord caps a channel name at 100 characters.
	long := strings.Repeat("mountain", 40)
	got := SanitizeChannelName(long, "")
	if len(got) > 100 {
		t.Errorf("name is %d bytes, over Discord's 100 limit", len(got))
	}

	// And it must not cut a multi-byte character in half while doing it.
	emoji := strings.Repeat("🏔", 60)
	got = SanitizeChannelName(emoji, "")
	if len(got)%4 != 0 {
		t.Errorf("truncation split a 4-byte character: %q", got)
	}
}

func TestHopsLabel(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{0xFF, "?"}, // no known path
		{0, "direct"},
		{1, "1h"},
		{7, "7h"},
	} {
		if got := HopsLabel(tc.in); got != tc.want {
			t.Errorf("HopsLabel(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Listings are rendered in a code block, so columns must line up by display
// width rather than by byte count — otherwise a name with an emoji in it
// shifts everything after it.
func TestPadDisplayCountsCharactersNotBytes(t *testing.T) {
	if got := PadDisplay("🏔🏔", 5); len([]rune(got)) != 5 {
		t.Errorf("PadDisplay(%q, 5) has %d runes, want 5", got, len([]rune(got)))
	}
	if got := PadDisplay("abc", 5); got != "abc  " {
		t.Errorf("PadDisplay = %q", got)
	}
	// Already at or over the width: left alone rather than truncated.
	if got := PadDisplay("abcdefgh", 5); got != "abcdefgh" {
		t.Errorf("PadDisplay truncated: %q", got)
	}
}

// The markers have to tell a coherent story, and each has exactly one job.
func TestMarkersAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for name, e := range map[string]string{
		"ok": EmojiOK, "fail": EmojiFail, "sent": EmojiSent,
		"retry-trigger": EmojiRetry, "waiting": EmojiWaiting,
	} {
		if prev, dup := seen[e]; dup {
			t.Errorf("%s and %s are the same emoji (%s); the display would be ambiguous", name, prev, e)
		}
		seen[e] = name
	}
	// The trigger must differ from the in-progress marker, or the bridge's own
	// marker would look like a pending request and a second press would be
	// swallowed by Discord as an already-present reaction.
	if EmojiRetry == EmojiWaiting {
		t.Error("the resend trigger and the in-progress marker must not be the same emoji")
	}
	// Everything the bridge might need to clear off a message before a fresh
	// verdict must be listed.
	for _, e := range []string{EmojiOK, EmojiFail, EmojiSent, EmojiWaiting} {
		found := false
		for _, v := range AllVerdicts {
			if v == e {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is applied but not in AllVerdicts, so a resend would leave it behind", e)
		}
	}
}

// Any instruction to react has to say WHICH message. "React to it" inside a
// bot message reads as "react to this one", which does nothing at all — the
// bridge only watches reactions on messages it can resend.
func TestRetryInstructionsNameTheTargetMessage(t *testing.T) {
	b, _ := newTestBridge(t)
	help := exec(t, b, "help")

	if !strings.Contains(help, EmojiRetry) {
		t.Fatal("help does not mention the resend reaction at all")
	}
	// It must point at the failed message, not leave "it" dangling.
	if !strings.Contains(help, "failed message") {
		t.Errorf("help does not say which message to react to:\n%s", help)
	}
}
