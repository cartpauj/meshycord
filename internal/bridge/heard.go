package bridge

// Hearing a channel message come back.
//
// MeshCore cannot acknowledge a group message, so for years the honest marker
// on a channel send was the satellite: "the radio accepted this, and nothing
// further can ever be known". That was true of the protocol's own machinery and
// false of the radio, which hears the mesh rebroadcast our packet a second or
// two later. A repeat is not a delivery receipt — nobody can say a human read
// it — but it is proof that the message left the building and that at least one
// repeater took responsibility for passing it on. That is worth a tick, and its
// absence is worth knowing about.
//
// The mechanics are in meshcore/rawpkt.go: the node reports every packet it
// hears, group traffic is encrypted deterministically, so our own message is
// recognisable by its exact ciphertext. This file is the bookkeeping — which
// Discord message is listening for which fingerprint, and for how long.

import (
	"bytes"
	"context"
	"sync"
	"time"

	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// heardWatch is one transmitted channel message listening for its own echo.
type heardWatch struct {
	cipher    []byte
	channelID string
	messageID string
	rowID     int64
	slot      string
	group     *chunkGroup

	mu sync.Mutex
	// paths counts DISTINCT rebroadcasts, keyed by the route each copy took.
	// Counting frames instead would count the same repeater twice when its
	// transmission is heard on two different sidebands of the same mesh, and
	// would miss that two separate repeaters both took the message.
	paths  map[string]struct{}
	best   int // the fewest hops any copy came back in
	done   bool
	closed bool
}

// note records one heard copy and reports whether this is the first.
func (w *heardWatch) note(p meshcore.RawPacket) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	first := len(w.paths) == 0
	if w.paths == nil {
		w.paths = map[string]struct{}{}
	}
	w.paths[p.PathKey()] = struct{}{}
	if hops := p.Hops(); first || hops < w.best {
		w.best = hops
	}
	return first
}

// watchForRepeats starts listening for the mesh rebroadcasting a channel
// message we just sent, and answers the Discord message when it does — or when
// it is clear that nothing did.
//
// The verdict is deliberately not applied here. Until the window closes the
// message wears the hourglass, which is the truth: the radio has it, the mesh
// has not yet spoken.
func (b *Bridge) watchForRepeats(ctx context.Context, send meshcore.ChannelSend,
	slot, channelID, messageID string, rowID int64, group *chunkGroup) {

	if len(send.Cipher) == 0 {
		// No fingerprint, so no evidence is obtainable. Fall back to the old
		// honest answer immediately rather than making the message sit on an
		// hourglass for a verdict that can never come.
		b.log.Debug("no fingerprint for a channel message; reporting it as transmitted",
			"slot", slot)
		b.reactVerdict(ctx, channelID, messageID, EmojiSent)
		b.settleChunk(ctx, group, false, false)
		return
	}

	w := &heardWatch{
		cipher: send.Cipher, channelID: channelID, messageID: messageID,
		rowID: rowID, slot: slot, group: group,
	}

	b.heardMu.Lock()
	// A resend reuses the same Discord message, and the previous attempt's
	// watch may still be open. Left alone it would resolve later and overwrite
	// the new attempt's verdict with the old one's.
	kept := b.heard[:0]
	for _, old := range b.heard {
		if old.messageID != "" && old.messageID == messageID {
			old.mu.Lock()
			old.closed, old.done = true, true
			old.mu.Unlock()
			continue
		}
		kept = append(kept, old)
	}
	b.heard = append(kept, w)
	b.heardMu.Unlock()

	window := b.cfg.HeardWindow()
	go func() {
		select {
		case <-ctx.Done():
			// Shutting down. Leave the marker as it is rather than claiming
			// silence we never finished listening for.
			b.forgetWatch(w)
			return
		case <-time.After(window):
		}
		b.closeWatch(ctx, w, window)
	}()
}

// noteRawPacket checks one heard packet against every message waiting for its
// echo.
//
// Called for every packet the radio hears, so it does as little as possible:
// non-group traffic is out after one byte, and the comparison itself is a byte
// compare against a handful of outstanding sends.
func (b *Bridge) noteRawPacket(ctx context.Context, p meshcore.RawPacket) {
	cipher := p.GroupCipher()
	if len(cipher) == 0 {
		return
	}
	// A copy with no repeater in its path is the original transmission as
	// somebody else's radio would hear it, not a rebroadcast. Our own node
	// cannot hear itself transmit, so in practice this never matches ours —
	// but if a second node on the same mesh sent an identical message, only a
	// repeated copy is evidence that OURS was passed on.
	if p.Hops() == 0 {
		return
	}

	b.heardMu.Lock()
	var match *heardWatch
	for _, w := range b.heard {
		if bytes.Equal(w.cipher, cipher) {
			match = w
			break
		}
	}
	b.heardMu.Unlock()
	if match == nil {
		return
	}

	if !match.note(p) {
		return // already ticked; the rest of the window is only for the count
	}
	b.log.Info("a channel message was repeated by the mesh",
		"slot", match.slot, "hops", p.Hops(), "snr", p.SNR)
	if match.rowID > 0 {
		_ = b.db.SetDelivery(match.rowID, store.DeliveryHeard, 0)
	}
	// Answer now rather than at the end of the window. The first repeat is the
	// whole verdict; waiting to count the rest would leave the message on an
	// hourglass for several more seconds with nothing left to decide.
	b.setVerdict(ctx, match.channelID, match.messageID, EmojiOK)
	b.settleChunk(ctx, match.group, true, true)
}

// closeWatch ends the listening window and reports silence if that is what it
// was.
func (b *Bridge) closeWatch(ctx context.Context, w *heardWatch, window time.Duration) {
	w.mu.Lock()
	already, repeats, hops := w.done, len(w.paths), w.best
	w.closed = true
	w.done = true
	w.mu.Unlock()
	b.forgetWatch(w)

	if repeats > 0 {
		// The tick went on when the first repeat arrived. This is only the
		// tally, which is the part worth having in the log: one repeat means
		// one repeater in earshot, several means the message is genuinely
		// spreading.
		b.log.Info("finished listening for repeats", "slot", w.slot,
			"repeats", repeats, "closest", hops, "window", window)
		return
	}
	if already {
		return // resolved some other way, or superseded by a resend
	}

	// Nothing came back. The radio did transmit — the node accepted the
	// message — so this is not a failure and must not wear a cross. It is the
	// satellite's real meaning: on the air, and nobody heard it happen.
	b.log.Info("a channel message was not repeated by anything in earshot",
		"slot", w.slot, "window", window)
	b.setVerdict(ctx, w.channelID, w.messageID, EmojiSent)
	b.settleChunk(ctx, w.group, false, false)
}

// forgetWatch drops a watch from the outstanding list.
func (b *Bridge) forgetWatch(w *heardWatch) {
	b.heardMu.Lock()
	defer b.heardMu.Unlock()
	kept := b.heard[:0]
	for _, o := range b.heard {
		if o != w {
			kept = append(kept, o)
		}
	}
	b.heard = kept
}

// watchRawPackets consumes the node's raw-packet firehose for one session.
//
// It has a goroutine of its own for a reason. The node pushes one frame per
// packet its radio hears — unconditionally, whether or not anything is
// listening — so on a busy mesh this is the highest-volume thing on the link.
// Handling it in the main event loop would put that volume in front of incoming
// messages and delivery confirmations.
func (b *Bridge) watchRawPackets(ctx context.Context, sess *meshcore.Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Done():
			return
		case p := <-sess.RawPackets():
			b.noteRawPacket(ctx, p)
		}
	}
}
