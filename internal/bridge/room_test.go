package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"meshycord/internal/discord"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// recordREST points the bridge's Discord client at a test server and collects
// every request path, in order.
func recordREST(t *testing.T, b *Bridge) *[]string {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(srv.Close)
	b.rest.BaseURL = srv.URL
	b.rest.Token = func() string { return "test-token" }
	return &calls
}

func seedRoom(t *testing.T, b *Bridge, db *store.Store) store.Route {
	t.Helper()
	seedBridge(t, db)
	route, err := db.PutRoute(store.KindRoom, "b2b2b2b2b2b2", "chan-room", "Ridge Room")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if err := db.SetRoomPassword("b2b2b2b2b2b2", "hunter2"); err != nil {
		t.Fatalf("password: %v", err)
	}
	return route
}

// A post to a room whose session has lapsed is HELD, not failed. It used to get
// a cross first and an hourglass immediately after — the message looked
// rejected, then looked busy, and the cross stayed put through the tick at the
// end because only the hourglass is ever removed.
func TestLapsedRoomSessionHoldsWithoutACross(t *testing.T) {
	b, db := newTestBridge(t)
	route := seedRoom(t, b, db)
	calls := recordREST(t, b)

	b.refuseRoomPost(context.Background(), route, &discord.Message{
		ID: "msg-1", ChannelID: route.ChannelID, Content: "hello room",
	})

	for _, c := range *calls {
		if strings.Contains(c, EmojiFail) {
			t.Fatalf("held post was marked failed: %v", *calls)
		}
	}
	if !anyContains(*calls, EmojiWaiting) {
		t.Fatalf("held post got no in-progress marker: %v", *calls)
	}

	// And it is actually being held, so the login sweep can pick it up.
	b.roomMu.Lock()
	held := len(b.roomState(route.MeshKey).pending)
	b.roomMu.Unlock()
	if held != 1 {
		t.Fatalf("held %d posts, want 1", held)
	}
}

// A room with no password on file has genuinely failed, and says so.
func TestRoomWithNoPasswordFailsAndOffersTheButton(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)
	route, err := db.PutRoute(store.KindRoom, "b2b2b2b2b2b2", "chan-room", "Ridge Room")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	calls := recordREST(t, b)

	b.refuseRoomPost(context.Background(), route, &discord.Message{
		ID: "msg-1", ChannelID: route.ChannelID, Content: "hello room",
	})

	if !anyContains(*calls, EmojiFail) {
		t.Fatalf("a post with no password should be marked failed: %v", *calls)
	}
}

// A post held while no login is in flight — which is what happens when the
// anti-storm window refuses the login, or the link is down — must be picked up
// again by the sweep. Without that it sat under an hourglass indefinitely.
func TestStrandedRoomPostsRestartTheLogin(t *testing.T) {
	b, db := newTestBridge(t)
	route := seedRoom(t, b, db)
	recordREST(t, b)

	ctx := context.Background()
	// No mesh session, so no login can go out and nothing is in flight.
	b.holdRoomPost(ctx, queuedPost{
		route: route, text: "hello", uiChannel: route.ChannelID, uiMessage: "msg-1",
	})

	b.roomMu.Lock()
	rs := b.roomState(route.MeshKey)
	inFlight, held := rs.loginInFlight, len(rs.pending)
	b.roomMu.Unlock()
	if inFlight {
		t.Fatal("no session, so no login can be in flight")
	}
	if held != 1 {
		t.Fatalf("held %d posts, want 1", held)
	}

	// The sweep must notice the stranded queue rather than skipping the room
	// for not being mid attempt. It cannot get further without a session, so
	// what is checked is that the post survives to be retried and is not
	// silently abandoned.
	b.expireRoomLogins(ctx)

	b.roomMu.Lock()
	held = len(b.roomState(route.MeshKey).pending)
	b.roomMu.Unlock()
	if held != 1 {
		t.Fatalf("the sweep dropped a held post: held %d, want 1", held)
	}
}

// A room login lives in the room's own client table, saved to the room's flash.
// It is not ours to lose when a USB cable is nudged, and the bridge restores it
// from the recorded login rather than spending a burst of airtime re-proving it.
func TestRoomSessionSurvivesARestart(t *testing.T) {
	b, db := newTestBridge(t)
	route := seedRoom(t, b, db)
	if err := db.RecordLogin(route.MeshKey, "ok"); err != nil {
		t.Fatalf("record: %v", err)
	}

	// No mesh session: loginAllRooms cannot send anything, so what is being
	// checked is purely what it believes afterwards.
	b.loginAllRooms(context.Background(), nil)

	if !b.roomLoggedIn(route.MeshKey) {
		t.Fatal("a recorded login was not restored")
	}
}

// A login the room REJECTED is not a session, however recently it happened.
func TestRejectedLoginIsNotRestored(t *testing.T) {
	b, db := newTestBridge(t)
	route := seedRoom(t, b, db)
	if err := db.RecordLogin(route.MeshKey, "rejected"); err != nil {
		t.Fatalf("record: %v", err)
	}

	b.loginAllRooms(context.Background(), nil)

	if b.roomLoggedIn(route.MeshKey) {
		t.Fatal("a rejected login was treated as a session")
	}
}

// A room that cannot be reached must not be retried every tick.
//
// The keep-alive gives up after three attempts and resets the counter, which
// left a never-answering room looking due again a minute later — a room out of
// range being called every few minutes forever, on a medium every repeater on
// the mesh pays to relay.
func TestKeepAliveBacksOffAfterAFailedCycle(t *testing.T) {
	const every = 4 * time.Hour

	// Exactly the state giveUpOnRoom leaves behind for a room that never
	// answered: never logged in, counter reset, attempt moments ago.
	justGaveUp := &roomSession{lastAttempt: time.Now()}
	if roomRefreshDue(justGaveUp, every) {
		t.Error("a room that just gave up was due again immediately")
	}

	// Once the interval has genuinely passed, try again — an out-of-range room
	// coming back should not need a human.
	stale := &roomSession{lastAttempt: time.Now().Add(-every - time.Minute)}
	if !roomRefreshDue(stale, every) {
		t.Error("a room untried for longer than the interval was not due")
	}

	// A room we have never touched at all is due at once: that is a room we are
	// not in, and the whole point is to be in it before somebody types.
	if !roomRefreshDue(&roomSession{}, every) {
		t.Error("a room with no login on record was not due")
	}

	// A fresh session is left alone.
	fresh := &roomSession{loggedIn: true, loggedInAt: time.Now(), lastAttempt: time.Now()}
	if roomRefreshDue(fresh, every) {
		t.Error("a session refreshed moments ago was refreshed again")
	}

	// And nothing is refreshed out from under a login or a held post.
	busy := &roomSession{loginInFlight: true}
	if roomRefreshDue(busy, every) {
		t.Error("refreshed a room with a login already in flight")
	}
	held := &roomSession{pending: []queuedPost{{text: "hi"}}}
	if roomRefreshDue(held, every) {
		t.Error("refreshed a room that has posts waiting on a login")
	}
}

func anyContains(calls []string, sub string) bool {
	for _, c := range calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// The node's send timeout does not clear its expected-ack entry, so a
// confirmation can genuinely arrive after the bridge gave up. When it does, the
// message DID land, and the cross must come off — leaving it invites a resend
// of something that already arrived, which costs the whole mesh airtime.
func TestALateAckClearsTheCross(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)
	route, err := db.Route(store.KindDM, "a1a1a1a1a1a1")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	calls := recordREST(t, b)
	ctx := context.Background()

	// A send that has already blown its deadline.
	b.pendMu.Lock()
	b.pending[4242] = &pendingSend{
		ack: 4242, deadline: time.Now().Add(-time.Second), window: minAckWait,
		channelID: route.ChannelID, messageID: "msg-1", route: route,
	}
	b.pendMu.Unlock()

	b.expirePending(ctx)
	if !anyContains(*calls, EmojiFail) {
		t.Fatalf("an expired send was not crossed: %v", *calls)
	}

	// ...and the ack turns up anyway.
	b.handleConfirmation(ctx, meshcore.Confirmation{Ack: 4242, RoundTrip: 9 * time.Second})

	var removedCross, addedTick bool
	for _, c := range *calls {
		if strings.HasPrefix(c, "DELETE") && strings.Contains(c, EmojiFail) {
			removedCross = true
		}
		if strings.HasPrefix(c, "PUT") && strings.Contains(c, EmojiOK) {
			addedTick = true
		}
	}
	if !removedCross || !addedTick {
		t.Errorf("a late ack did not correct the verdict (cross removed=%v, tick added=%v):\n%v",
			removedCross, addedTick, *calls)
	}

	// And it is not left in the map to be corrected twice.
	b.pendMu.Lock()
	n := len(b.failed)
	b.pendMu.Unlock()
	if n != 0 {
		t.Errorf("corrected send still held: %d", n)
	}
}

// A split message must not claim to have been sent when its pieces failed.
//
// The original used to get a satellite the moment the radio accepted the last
// piece, and was never revisited — so a message whose every transmission went
// unacknowledged sat wearing "transmitted" directly above its own pieces
// wearing crosses.
func TestSplitMessageVerdictFollowsItsPieces(t *testing.T) {
	cases := []struct {
		name   string
		settle func(g *chunkGroup) string
		want   string
	}{
		{"all delivered", func(g *chunkGroup) string {
			g.settle(true, true)
			return g.settle(true, true)
		}, EmojiOK},
		{"all failed", func(g *chunkGroup) string {
			g.settle(false, true)
			return g.settle(false, true)
		}, EmojiFail},
		{"one of two failed", func(g *chunkGroup) string {
			g.settle(true, true)
			return g.settle(false, true)
		}, EmojiFail},
		{"unconfirmable", func(g *chunkGroup) string {
			g.settle(true, true)
			return g.settle(false, false)
		}, EmojiSent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &chunkGroup{channelID: "c", messageID: "m", total: 2}
			if got := tc.settle(g); got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}

	// Nothing is decided while a piece is still outstanding.
	g := &chunkGroup{channelID: "c", messageID: "m", total: 3}
	if v := g.settle(true, true); v != "" {
		t.Errorf("decided early with 1 of 3 settled: %q", v)
	}
	if v := g.settle(false, true); v != "" {
		t.Errorf("decided early with 2 of 3 settled: %q", v)
	}
	if v := g.settle(true, true); v != EmojiFail {
		t.Errorf("final verdict = %q, want %q", v, EmojiFail)
	}
	// And having spoken once, it does not speak again.
	if v := g.settle(true, true); v != "" {
		t.Errorf("reported a second time: %q", v)
	}

	// A late ack for the failed piece makes the whole message a success.
	if v := g.correct(); v != EmojiOK {
		t.Errorf("after a late ack, verdict = %q, want %q", v, EmojiOK)
	}
}

// Resending a message that was split reuses the transmissions already on
// screen. Posting a second identical set underneath the first — while the first
// sits there wearing crosses — makes it impossible to tell what is being
// retried from what already failed.
func TestResendReusesThePiecesItAlreadyPosted(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	const parent = "parent-msg"
	chunks := []string{"[1/2] hello", "[2/2] world"}
	for i, body := range chunks {
		if _, err := db.InsertMessage(store.Message{
			Direction: "out", Kind: store.KindRoom, MeshKey: "b2b2b2b2b2b2", Body: body,
			ChannelID: "chan", MessageID: "echo-" + string(rune('a'+i)),
			ChunkIndex: i + 1, ChunkTotal: 2, ParentMsgID: parent,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	req := sendRequest{UIChannel: "chan", UIMessage: parent}

	got := b.previousChunks(req, chunks)
	if len(got) != 2 {
		t.Fatalf("found %d previous transmissions, want 2", len(got))
	}
	if got[0].MessageID != "echo-a" || got[1].MessageID != "echo-b" {
		t.Errorf("wrong messages or wrong order: %+v", got)
	}

	// Edited text must not be reused: the markers would land on messages whose
	// text is no longer what went out.
	if r := b.previousChunks(req, []string{"[1/2] hello", "[2/2] there"}); r != nil {
		t.Error("reused transmissions whose text had changed")
	}
	// Nor a different number of pieces.
	if r := b.previousChunks(req, []string{"[1/1] hello"}); r != nil {
		t.Error("reused transmissions when the split changed shape")
	}
	// And a message that was never split has nothing to reuse.
	if r := b.previousChunks(sendRequest{UIChannel: "chan", UIMessage: "other"}, chunks); r != nil {
		t.Error("found previous transmissions for an unrelated message")
	}
}

// Only the latest attempt's transmissions are offered for reuse.
func TestResendReusesTheMostRecentPieces(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)

	const parent = "parent-msg"
	chunks := []string{"[1/2] hello", "[2/2] world"}
	for _, attempt := range []string{"old", "new"} {
		for i, body := range chunks {
			if _, err := db.InsertMessage(store.Message{
				Direction: "out", Kind: store.KindRoom, MeshKey: "b2b2b2b2b2b2", Body: body,
				ChannelID: "chan", MessageID: attempt + "-" + string(rune('a'+i)),
				ChunkIndex: i + 1, ChunkTotal: 2, ParentMsgID: parent,
			}); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	got := b.previousChunks(sendRequest{UIChannel: "chan", UIMessage: parent}, chunks)
	if len(got) != 2 || got[0].MessageID != "new-a" || got[1].MessageID != "new-b" {
		t.Errorf("did not pick the most recent attempt: %+v", got)
	}
}

// A piece of a split cannot be resent on its own: the pieces are one message
// torn up and go out in order, each waiting on the last, so putting one back on
// the air by itself either duplicates what the far end has or drops into a gap.
func TestASinglePieceCannotBeResent(t *testing.T) {
	b, db := newTestBridge(t)
	route := seedRoom(t, b, db)
	calls := recordREST(t, b)

	b.resendMessage(context.Background(), route, route.ChannelID, "piece-1", &discord.Message{
		ID: "piece-1", ChannelID: route.ChannelID, Content: "[2/3] the middle bit",
		Author: discord.User{ID: "bot-1", Bot: true},
	})

	if !anyContains(*calls, EmojiFail) {
		t.Errorf("resending one piece was not refused: %v", *calls)
	}
	// And it must not have gone anywhere near the radio.
	b.pendMu.Lock()
	n := len(b.pending)
	b.pendMu.Unlock()
	if n != 0 {
		t.Errorf("a piece resend reached the send path: %d pending", n)
	}
}

// A restart orphans every acknowledgement in flight, because the handle only
// means anything to the node that issued it. Those messages must be answered on
// the way back up rather than keeping an hourglass forever.
func TestStrandedSendsAreAnsweredAfterARestart(t *testing.T) {
	b, db := newTestBridge(t)
	seedBridge(t, db)
	calls := recordREST(t, b)

	id, err := db.InsertMessage(store.Message{
		Direction: "out", Kind: store.KindDM, MeshKey: "a1a1a1a1a1a1", Body: "hello",
		ChannelID: "chan", MessageID: "msg-1", Delivery: store.DeliveryPending, Ack: 99,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	b.resolveStrandedSends(context.Background())

	if !anyContains(*calls, EmojiFail) {
		t.Errorf("a stranded send kept its hourglass: %v", *calls)
	}
	var delivery string
	if err := db.DB().QueryRow(`SELECT delivery FROM messages WHERE id = ?`, id).
		Scan(&delivery); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if delivery != store.DeliveryFailed {
		t.Errorf("delivery = %q, want %q", delivery, store.DeliveryFailed)
	}
}
