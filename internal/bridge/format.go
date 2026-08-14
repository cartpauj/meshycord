package bridge

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"meshycord/internal/meshcore"
)

// Delivery markers.
//
// The distinction between a tick and a satellite is not decoration, and what
// separates them differs by route.
//
// For a direct message or a room post it is an acknowledgement: the far node
// says it has the message, or it does not.
//
// For a channel there is no acknowledgement to be had — MeshCore cannot
// acknowledge group traffic at all — so the tick means something weaker and
// still worth having: the radio HEARD a repeater pass the message on. The
// satellite is what is left when it did not. Both are honest; neither claims a
// delivery the protocol cannot prove. See heard.go.
const (
	EmojiOK   = "✅" // acknowledged by the recipient's node, or heard being repeated
	EmojiFail = "❌" // rejected, or no acknowledgement before the deadline
	EmojiSent = "📡" // transmitted, and nothing was heard of it afterwards
	// EmojiRetry is what YOU add to ask for a resend. The bridge never displays
	// it — echoing the request back reads as though it is still unhandled — and
	// consumes it on receipt so a second press registers.
	EmojiRetry = "🔄"
	// EmojiWaiting means the message is on the air and no answer has come back
	// yet — an acknowledgement for a direct message or room post, a repeat for
	// a channel message. Distinct from a verdict: it says the mesh is still
	// working on it.
	EmojiWaiting = "⏳"
	// EmojiSplit marks a message that did not fit in one transmission and was
	// sent as several. It is a fact about the message, not a verdict on it: it
	// goes on once, at the moment of splitting, and is never taken off — not by
	// a verdict, not by a resend. The numbered pieces posted underneath are
	// what the marker is pointing at.
	//
	// A puzzle piece, and a single code point (U+1F9E9) like every marker
	// above. Scissors would read better and are a trap: ✂️ is U+2702 followed
	// by a variation selector, which has to survive being percent-encoded into
	// a reaction URL, and an emoji that fails there fails silently.
	EmojiSplit = "🧩"
)

// AllVerdicts is every marker the bridge applies, so a resend can clear the
// previous one before adding a new verdict.
//
// EmojiSplit is deliberately absent. It records what happened to the message
// rather than how it ended, so clearing it on a resend would erase something
// still true — and a resend of a long message splits it again anyway.
var AllVerdicts = []string{EmojiOK, EmojiFail, EmojiSent, EmojiRetry, EmojiWaiting}

// FormatInbound renders a mesh message for Discord.
//
// Every message carries its hop count and signal, because on a mesh that is
// the interesting part. The wording matters: MeshCore's "direct route" means
// the sender used a STORED PATH, which can be many hops and says nothing about
// distance. Calling that "direct" read as "nearby" and was actively misleading
// for a station hundreds of miles away. Genuine adjacency is a flooded packet
// with zero hops accumulated.
func FormatInbound(m meshcore.Message, label string) string {
	meta := "  _"
	switch {
	case !m.HaveHops:
		meta += "via known path" // 0xFF: routed by a stored path, hops not applicable
	case m.Hops == 0:
		meta += "heard direct" // flooded, no repeaters in between
	case m.Hops == 1:
		meta += "1 hop"
	default:
		meta += fmt.Sprintf("%d hops", m.Hops)
	}
	if m.HaveSNR {
		meta += fmt.Sprintf(", snr %.1f", m.SNR)
	}
	meta += "_"

	if m.IsChannel {
		// The Discord channel already says WHICH mesh channel this is, so
		// naming it again on every line is noise. What matters is who sent it,
		// and MeshCore embeds that in the text as "Name: body"
		// (sendGroupMessage passes _prefs.node_name).
		if name, body, ok := splitSenderPrefix(m.Text); ok {
			return "**" + name + "**" + meta + "\n" + body
		}
		return m.Text + meta
	}

	// A DM or a room post. The key is shown so it can be linked later with
	// `/mesh link` or `add <key>` without anyone retyping hex from a screen.
	who := label
	if who == "" {
		who = "unknown"
	}
	s := "**" + who + "** `" + m.PubKeyPrefix + "`" + meta + "\n"
	return s
}

// FormatInboundBody is the message text as it should appear under the header,
// with a room post's own author attributed.
func FormatInboundBody(m meshcore.Message, authorName string) string {
	if m.IsChannel {
		return ""
	}
	if authorName != "" {
		return "**" + authorName + "**: " + m.Text
	}
	return m.Text
}

// splitSenderPrefix pulls "Name: body" apart, which is how MeshCore embeds the
// sender of a group message. The 32-byte ceiling stops a colon deep inside the
// message body being mistaken for a name.
func splitSenderPrefix(text string) (name, body string, ok bool) {
	i := strings.Index(text, ": ")
	if i <= 0 || i > 32 {
		return "", "", false
	}
	return text[:i], text[i+2:], true
}

// SanitizeChannelName turns a mesh contact or channel name into a Discord
// channel name.
//
// Discord allows unicode and emoji in channel names; it lowercases them and
// turns spaces into hyphens, with a 100-character cap. An earlier version
// stripped every non-ASCII byte, which was stricter than Discord for no
// reason: it turned "Russet🥔 Room" into "russet-room" and an emoji-only name
// into nothing at all.
//
// fallback is used when nothing usable survives — pass the contact's key, so
// the result is still unique rather than a generic word every such contact
// would share.
func SanitizeChannelName(raw, fallback string) string {
	var b strings.Builder
	lastDash := false

	for _, r := range raw {
		if b.Len() >= 90 {
			break
		}
		switch {
		case r >= 0x80:
			// Pass multi-byte characters straight through, so emoji and
			// accented letters survive.
			b.WriteRune(r)
			lastDash = false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		default:
			// Space, punctuation, control: one hyphen, never a run of them.
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}

	s := meshcore.TruncateUTF8(b.String(), 90)
	s = strings.TrimRight(s, "-")
	if s != "" {
		return s
	}
	if fallback != "" {
		return SanitizeChannelName(fallback, "")
	}
	return "mesh"
}

// HopsLabel renders a contact's stored path for a listing.
//
// The stored byte is packed, not a count — see meshcore.DecodePathByte. Showing
// it raw produced "67h" for a contact three hops away.
func HopsLabel(outPathLen int) string {
	hops, known := meshcore.DecodePathByte(byte(outPathLen))
	switch {
	case !known:
		return "?"
	case hops == 0:
		return "direct"
	default:
		return fmt.Sprintf("%dh", hops)
	}
}

// HopCount is the decoded hop count, for sorting. Unknown paths sort last.
func HopCount(outPathLen int) int {
	hops, known := meshcore.DecodePathByte(byte(outPathLen))
	if !known {
		return 255
	}
	return int(hops)
}

// PadDisplay pads s to width, counting display characters rather than bytes so
// a column of names with emoji in them still lines up in a code block.
func PadDisplay(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// RouteWish is a per-message override of how the node routes a message.
type RouteWish int

// The three routing wishes a message prefix can express.
const (
	RouteAuto RouteWish = iota
	RouteForceFlood
	RouteForceDirect
)

// TakeRoutePrefix strips a `path:flood` / `path:direct` prefix off a message
// and reports which was asked for.
//
// The prefix and all following whitespace are removed before anything else, so
// they cost none of the transmission budget and the recipient sees only the text.
//
// There is no per-message route flag in the companion protocol. The node picks
// flood when a contact has no stored path and direct otherwise
// (BaseChatMesh.cpp:449), so `path:flood` works by clearing the stored path.
// It is relearned from the reply, so the effect is for this message rather
// than permanent.
func TakeRoutePrefix(text string) (string, RouteWish) {
	lower := strings.ToLower(text)
	for _, f := range []struct {
		tag  string
		wish RouteWish
	}{
		{"path:flood", RouteForceFlood},
		{"path:direct", RouteForceDirect},
	} {
		if !strings.HasPrefix(lower, f.tag) {
			continue
		}
		rest := text[len(f.tag):]
		// It must be the whole message or be followed by whitespace, so a
		// message that merely begins with these letters is not swallowed.
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			continue
		}
		return strings.TrimLeft(rest, " \t"), f.wish
	}
	return text, RouteAuto
}

// ResolveRouteWish decides how one message should be routed, combining what the
// text asked for with what the caller asked for.
//
// forceFlood is the implicit request a resend makes: clear the path, because the
// recorded one is the most likely reason the message needs sending again. An
// explicit prefix always wins over it — somebody who typed `path:direct` and
// then reacted 🔄 means the direct attempt.
//
// complain reports that a `path:` prefix was typed on a channel message, where
// it cannot mean anything. It is deliberately false for a forced flood with no
// prefix: resending a channel message needs no apology, it floods either way.
func ResolveRouteWish(text string, forceFlood, isChannel bool) (body string, wish RouteWish, complain bool) {
	body, wish = TakeRoutePrefix(text)
	explicit := wish != RouteAuto
	if !explicit && forceFlood {
		wish = RouteForceFlood
	}
	if isChannel {
		// Nothing to clear and nothing to follow: a channel message is not
		// addressed to a contact, so it has no stored path either way.
		return body, RouteAuto, explicit
	}
	return body, wish, false
}
