package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// groupPacket builds a raw packet as the radio would report it: a flooded group
// text message whose payload carries `cipher` behind the channel hash and MAC,
// having passed through `hops` repeaters.
func groupPacket(cipher []byte, hops int) meshcore.RawPacket {
	path := make([]byte, hops)
	for i := range path {
		path[i] = byte(0xA0 + i)
	}
	payload := append([]byte{0x7F, 0x11, 0x22}, cipher...) // channel hash, MAC
	return meshcore.RawPacket{
		Header:   meshcore.RouteTypeFlood | (meshcore.PayloadTypeGrpTxt << 2),
		Path:     path,
		HashSize: 1,
		Payload:  payload,
	}
}

func heardTestBridge(t *testing.T) (*Bridge, *store.Store, *restLog) {
	t.Helper()
	b, db := newTestBridge(t)
	calls := recordREST(t, b)
	// The floor the config clamps to, so the "nothing was heard" case does not
	// hold the suite up for the real eight seconds.
	if err := b.cfg.SetHeardWindowMS(1000); err != nil {
		t.Fatalf("window: %v", err)
	}
	return b, db, calls
}

// The whole point of the feature: a channel message that the mesh is heard
// passing on gets a tick, not a satellite.
func TestChannelMessageTicksWhenHeardRepeated(t *testing.T) {
	b, db, calls := heardTestBridge(t)
	ctx := context.Background()

	rowID, err := db.InsertMessage(store.Message{
		Direction: "out", Kind: store.KindChannel, MeshKey: "ch:0",
		Body: "hello mesh", ChannelID: "c1", MessageID: "m1",
		Delivery: store.DeliveryTransmitted, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	cipher := []byte("0123456789abcdef")
	b.watchForRepeats(ctx, meshcore.ChannelSend{Timestamp: time.Now(), Cipher: cipher},
		"ch:0", "c1", "m1", rowID, nil)

	b.noteRawPacket(ctx, groupPacket(cipher, 1))

	if !anyContains(calls.all(), "PUT") || !anyContains(calls.all(), EmojiOK) {
		t.Fatalf("a repeated message was not ticked: %v", calls.all())
	}
	// And the history says what actually happened — repeated by the mesh, which
	// is not the same claim as delivered.
	m, ok := db.MessageByDiscordID("m1")
	if !ok {
		t.Fatal("the message went missing from the history")
	}
	if m.Delivery != store.DeliveryHeard {
		t.Fatalf("delivery state = %q, want %q", m.Delivery, store.DeliveryHeard)
	}

	// Further copies are counted, not re-decided: the tick is already on, and a
	// second verdict would only mean a second round of Discord calls.
	before := len(calls.all())
	b.noteRawPacket(ctx, groupPacket(cipher, 2))
	if len(calls.all()) != before {
		t.Errorf("a second repeat caused more Discord traffic: %v", calls.all()[before:])
	}

	// Once the window closes the watch is forgotten, so nothing accumulates.
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.heardMu.Lock()
		n := len(b.heard)
		b.heardMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d watches still outstanding after the window closed", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Silence is not failure. The radio did transmit; nothing in earshot passed it
// on. That is the satellite's real meaning and it must never be a cross.
func TestChannelMessageFallsBackToSatelliteWhenNothingIsHeard(t *testing.T) {
	b, _, calls := heardTestBridge(t)
	ctx := context.Background()

	b.watchForRepeats(ctx, meshcore.ChannelSend{Timestamp: time.Now(), Cipher: []byte("fingerprint-abcd")},
		"ch:0", "c1", "m1", 0, nil)

	deadline := time.Now().Add(5 * time.Second)
	for !anyContains(calls.all(), EmojiSent) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !anyContains(calls.all(), EmojiSent) {
		t.Fatalf("an unheard message got no satellite: %v", calls.all())
	}
	for _, c := range calls.all() {
		if strings.HasPrefix(c, "PUT") && strings.Contains(c, EmojiFail) {
			t.Fatalf("an unheard message was marked failed: %v", calls.all())
		}
	}
}

// Without the channel secret there is no fingerprint, so no evidence can ever
// arrive. Say so at once instead of leaving the message on an hourglass waiting
// for a verdict that cannot come.
func TestChannelMessageWithNoFingerprintAnswersImmediately(t *testing.T) {
	b, _, calls := heardTestBridge(t)

	b.watchForRepeats(context.Background(), meshcore.ChannelSend{Timestamp: time.Now()},
		"ch:0", "c1", "m1", 0, nil)

	if !anyContains(calls.all(), EmojiSent) {
		t.Fatalf("no immediate satellite without a fingerprint: %v", calls.all())
	}
	b.heardMu.Lock()
	n := len(b.heard)
	b.heardMu.Unlock()
	if n != 0 {
		t.Errorf("a watch was left open with nothing to match: %d", n)
	}
}

// A copy that has not been through a repeater is somebody's original
// transmission, not a rebroadcast, and proves nothing about ours being passed
// on.
func TestZeroHopCopyIsNotARepeat(t *testing.T) {
	b, _, calls := heardTestBridge(t)
	ctx := context.Background()
	cipher := []byte("0123456789abcdef")

	b.watchForRepeats(ctx, meshcore.ChannelSend{Timestamp: time.Now(), Cipher: cipher},
		"ch:0", "c1", "m1", 0, nil)
	b.noteRawPacket(ctx, groupPacket(cipher, 0))

	for _, c := range calls.all() {
		if strings.HasPrefix(c, "PUT") && strings.Contains(c, EmojiOK) {
			t.Fatalf("a zero-hop copy was treated as a repeat: %v", calls.all())
		}
	}
}

// A resend reuses the same Discord message. The previous attempt's watch must
// not still be listening: it would time out later and stamp the old attempt's
// verdict over the new one.
func TestResendSupersedesTheEarlierWatch(t *testing.T) {
	b, _, _ := heardTestBridge(t)
	ctx := context.Background()

	first := []byte("first-attempt-16")
	b.watchForRepeats(ctx, meshcore.ChannelSend{Timestamp: time.Now(), Cipher: first},
		"ch:0", "c1", "m1", 0, nil)
	second := []byte("second-attempt!!")
	b.watchForRepeats(ctx, meshcore.ChannelSend{Timestamp: time.Now(), Cipher: second},
		"ch:0", "c1", "m1", 0, nil)

	b.heardMu.Lock()
	n := len(b.heard)
	var only []byte
	if n == 1 {
		only = b.heard[0].cipher
	}
	b.heardMu.Unlock()
	if n != 1 {
		t.Fatalf("%d watches for one message, want 1", n)
	}
	if string(only) != string(second) {
		t.Errorf("the surviving watch is the old attempt's")
	}

	// The superseded watch is closed, so a late repeat of the first attempt
	// cannot answer the message either.
	b.noteRawPacket(ctx, groupPacket(first, 1))
}

// A split channel message is only fully accounted for when every piece has been
// heard; one unheard piece leaves the original wearing the satellite.
func TestSplitChannelParentNeedsEveryPieceHeard(t *testing.T) {
	g := &chunkGroup{channelID: "c", messageID: "m", total: 2}
	g.settle(true, true) // heard
	if v := g.settle(false, false); v != EmojiSent {
		t.Errorf("verdict = %q, want %q for a split with one unheard piece", v, EmojiSent)
	}

	g2 := &chunkGroup{channelID: "c", messageID: "m", total: 2}
	g2.settle(true, true)
	if v := g2.settle(true, true); v != EmojiOK {
		t.Errorf("verdict = %q, want %q when every piece was heard", v, EmojiOK)
	}
}
