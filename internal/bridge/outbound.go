package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"meshycord/internal/discord"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// How long a direct message may wait for its delivery confirmation.
//
// The node's own estimate does the real work here, and it is not a guess: the
// firmware computes it from the packet's airtime and the route it chose.
//
//	flood:  500ms + 16 x airtime
//	direct: 500ms + (6 x airtime + 250ms) x (hops + 1)
//
// (`companion_radio/MyMesh.cpp:851`, constants at :102.) The 16x on a flood is
// the firmware's allowance for the whole mesh relaying it, including the random
// 0..5x-airtime delay each repeater adds before retransmitting
// (`simple_repeater/MyMesh.cpp:540`). Doubling that is a generous margin on top
// of a number that already assumes the worst.
//
// The floor used to be 60 seconds, which is where "it took a minute to fail"
// came from: it overrode the estimate entirely on every ordinary send. Nothing
// in the firmware asks for it. It is now only a guard against an implausibly
// small estimate — every round trip this bridge has actually recorded came back
// under 4.1 seconds, and the largest estimate the node has ever reported was
// 10.25 seconds.
//
// The ceiling is worth keeping for a different reason. When the node's own
// timer expires it calls an EMPTY onSendTimeout() (`BaseChatMesh.cpp:957`) — it
// never tells us it gave up, so this timer is the only one there is. A late ACK
// can still be matched and pushed, because the timeout does not clear the
// expected-ack entry; what removes it is the table being circular and only 8
// deep (`MyMesh.h:253`). So waiting minutes buys nothing that eight more
// messages would not erase anyway — and a late one is now handled properly, see
// correctLateDelivery.
const (
	minAckWait = 15 * time.Second
	maxAckWait = 2 * time.Minute
)

// chunkGroup ties the transmissions of one split message back to the Discord
// message that was typed.
//
// Without it the original message was marked "transmitted" the moment the radio
// accepted the last piece, and never looked at again — so a message whose
// every transmission then went unacknowledged sat there wearing a satellite,
// above its own pieces wearing crosses. The satellite is honest only where an
// acknowledgement is impossible, which is channels; for a DM or a room the
// pieces are each acknowledged, so the whole is answerable and the original
// should say what actually happened to it.
type chunkGroup struct {
	channelID string
	messageID string
	total     int

	mu        sync.Mutex
	delivered int
	failed    int
	unknown   int // transmitted, but no acknowledgement was ever possible
	done      bool
}

// verdict is the marker for the parent, or "" while pieces are outstanding.
// Caller must hold mu.
func (g *chunkGroup) verdict() string {
	if g.delivered+g.failed+g.unknown < g.total {
		return ""
	}
	switch {
	case g.failed > 0:
		// One missing piece makes the whole message wrong at the far end, so
		// the original is a failure even if the rest landed. The pieces keep
		// their own markers, which is where you look to see which ones.
		return EmojiFail
	case g.unknown > 0:
		return EmojiSent
	default:
		return EmojiOK
	}
}

// settle records one transmission's outcome.
func (g *chunkGroup) settle(delivered, knowable bool) string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case !knowable:
		g.unknown++
	case delivered:
		g.delivered++
	default:
		g.failed++
	}
	if g.done {
		return ""
	}
	v := g.verdict()
	if v != "" {
		g.done = true
	}
	return v
}

// correct moves one transmission from failed to delivered, for an
// acknowledgement that turned up after the deadline.
func (g *chunkGroup) correct() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failed > 0 {
		g.failed--
		g.delivered++
	}
	return g.verdict()
}

// abandon stops a group reporting anything further, for a send that gave up
// part way and has already had its say.
func (g *chunkGroup) abandon() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.done = true
	g.mu.Unlock()
}

// pendingSend is an outbound DM waiting for its delivery confirmation.
//
// A DM send returns an expected-ack handle; the node later pushes
// PUSH_CODE_SEND_CONFIRMED with that handle and the round-trip time. The
// Discord message id is held alongside it so the right message gets the tick.
type pendingSend struct {
	ack      uint32
	deadline time.Time
	// window is how long we agreed to wait, kept so an expiry can say whether
	// the mesh was given a fair chance.
	window    time.Duration
	channelID string
	messageID string
	rowID     int64

	// route is kept so an expiry can name what failed, and flooded says whether
	// a stored route was involved — a stale one is the likeliest reason an
	// acknowledgement never arrives.
	route   store.Route
	flooded bool

	// failedAt is when the deadline passed, set only once the send has moved
	// into the failed map. It bounds how long a late ack can still correct the
	// verdict.
	failedAt time.Time

	// group is the split message this transmission is one piece of, nil for a
	// message that fitted in one. It carries the verdict back to the Discord
	// message somebody actually typed.
	group *chunkGroup

	// settled carries this send's outcome to whoever is waiting to send the
	// next piece: true acknowledged, false timed out. Buffered and written
	// once, so resolving a send never blocks even when nobody is listening.
	settled chan bool
}

// resolve reports the outcome to a waiting sender, at most once.
func (p *pendingSend) resolve(delivered bool) {
	if p.settled == nil {
		return
	}
	select {
	case p.settled <- delivered:
	default: // already resolved, or nobody waiting
	}
}

// ---------------------------------------------------------------------------
// Discord -> mesh
// ---------------------------------------------------------------------------

// onMessageCreate runs on the bridge's event worker, not on the Gateway's
// read loop, so it is free to take as long as the radio needs.
func (b *Bridge) onMessageCreate(ctx context.Context, m *discord.Message) {
	if m == nil {
		return
	}
	// The echo-loop guard. Never treat our own posts, or any other bot's, as
	// something to send to the mesh — getting this wrong floods the radio.
	if m.IsFromBot() || b.isOwnBot(m.Author.ID) {
		return
	}
	if strings.TrimSpace(m.Content) == "" {
		return
	}

	// Never act on the same Discord message twice.
	//
	// The ESP32 got this from its poll cursor. The Gateway normally delivers
	// each event once, but a RESUME replays everything after the last
	// acknowledged sequence number, and a redelivery here would put the same
	// text on the air a second time — airtime everybody on the mesh pays for.
	if !b.firstSighting(m.ID) {
		b.log.Debug("ignoring a Discord message we have already handled", "id", m.ID)
		return
	}

	switch m.ChannelID {
	case b.cfg.AdminChannel():
		b.handleAdminMessage(ctx, m)
		return
	case b.cfg.InboxChannel():
		b.handleInboxMessage(ctx, m)
		return
	}

	route, err := b.db.RouteByChannel(m.ChannelID)
	if err != nil {
		return // not a channel we bridge; none of our business
	}
	b.handleRoutedMessage(ctx, route, m)
}

// handleRoutedMessage deals with a human typing in a linked channel.
func (b *Bridge) handleRoutedMessage(ctx context.Context, route store.Route, m *discord.Message) {
	_ = b.db.TouchRoute(route.ID)

	// Reply-triggered resend is switched off while the reaction path is being
	// tried on its own. Reacting 🔄 to a message resends it — see onReactionAdd.
	//
	// Left in place rather than deleted: handleRetry below still compiles, and
	// uncommenting these four lines brings `retry` / `resend` back. Note the
	// trade — with this off, the words "retry" and "resend" are ordinary text
	// and WILL be transmitted over the radio like anything else.
	//
	// switch trimLower(m.Content) {
	// case "retry", "resend":
	// 	b.handleRetry(ctx, route, m)
	// 	return
	// }

	// `!promote <keyprefix>` gives a sender their own channel without leaving
	// the conversation you are already in — handy when a key shows up in the
	// inbox or in a room post and you want to talk to them directly.
	if rest, ok := cutPrefix(strings.TrimSpace(m.Content), "!promote"); ok {
		b.handlePromote(ctx, m, rest)
		return
	}

	sess := b.link.Session()
	if sess == nil {
		// Two separate facts, and the distinction matters. The LINK keeps
		// retrying on its own — 2 seconds backing off to 60 — so a node that is
		// merely still booting will be picked up without anyone doing anything.
		// THIS MESSAGE will not: it is dropped here, and nothing requeues it when
		// the link returns.
		//
		// So the wording promises only what happens. It used to end "I will keep
		// trying", which conflated the two and left people waiting for a send
		// that was never going to happen. Holding the message instead would be
		// worse than it sounds — the radio can be away for hours, and a message
		// that quietly arrives long after it stopped being relevant is not one
		// anybody wanted sent.
		b.react(ctx, m.ChannelID, m.ID, EmojiFail)
		b.say(ctx, m.ChannelID, "**Not sent — no link to the node right now.** "+
			"If the device restarted recently, give it a few minutes: reconnecting "+
			"is automatic, and over Bluetooth it can take a while to pair and read "+
			"the contact list. Otherwise check the radio is connected and powered.\n"+
			"This message was not queued. Once the link is back, react "+EmojiRetry+
			" on it to send it.")
		return
	}

	// A room server drops posts from anyone without a session, and it does so
	// SILENTLY: the send succeeds and the post simply never appears. Refuse up
	// front rather than letting that look like a delivery that worked.
	if route.Kind == store.KindRoom && !b.roomLoggedIn(route.MeshKey) {
		b.refuseRoomPost(ctx, route, m)
		return
	}

	// Say "received, working on it" straight away, for every kind of message.
	// Talking to the radio takes a moment, a split message takes several
	// seconds, and a direct message then waits on an acknowledgement — so
	// without this the first visible sign of life can be a minute away, which
	// is indistinguishable from being ignored.
	b.react(ctx, m.ChannelID, m.ID, EmojiWaiting)

	b.sendToMesh(ctx, sess, sendRequest{
		Route:     route,
		Text:      m.Content,
		UIChannel: m.ChannelID,
		UIMessage: m.ID,
		Author:    m.Author.Display(),
	})
}

// seenMessagesMax bounds the duplicate guard. Discord only ever replays events
// from the current session, so this needs to cover a reconnect's worth of
// traffic, not history — and it is deliberately not persisted: after a restart
// the Gateway identifies fresh and replays nothing at all.
const seenMessagesMax = 512

// firstSighting reports whether this Discord message id is new, and records
// it. Oldest entries are evicted wholesale once the set is full, which is
// crude but exactly right here: the risk window is seconds long.
func (b *Bridge) firstSighting(id string) bool {
	if id == "" {
		return true
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if b.seen == nil {
		b.seen = make(map[string]struct{}, seenMessagesMax)
	}
	if _, dup := b.seen[id]; dup {
		return false
	}
	if len(b.seen) >= seenMessagesMax {
		b.seen = make(map[string]struct{}, seenMessagesMax)
	}
	b.seen[id] = struct{}{}
	return true
}

// handlePromote links a key prefix to its own Discord channel.
//
// The same work as `add <keyprefix>` in the admin console, and it goes through
// the same executor so the two cannot behave differently — this is only a
// shortcut that saves switching channels.
func (b *Bridge) handlePromote(ctx context.Context, m *discord.Message, arg string) {
	prefix, ok := isKeyPrefix(strings.TrimSpace(arg))
	if !ok {
		b.say(ctx, m.ChannelID,
			"Usage: `!promote <keyprefix>` — the 12-character key shown next to a sender's name.\n"+
				"This gives them their own channel; `/mesh link` does the same thing.")
		return
	}
	b.say(ctx, m.ChannelID, b.Exec(ctx, m.Author.ID, "add "+prefix, m.ChannelID, ""))
}

// refuseRoomPost explains why a room post was not sent, and offers the fix.
//
// Nothing is marked failed until it has actually failed. A cross used to go on
// here first, before the two cases were told apart — so an ordinary post to a
// room with a lapsed session got a cross, then an hourglass a moment later when
// it was held for the login, and the cross stayed there through the tick at the
// end because reactVerdict only ever takes off the hourglass.
func (b *Bridge) refuseRoomPost(ctx context.Context, route store.Route, m *discord.Message) {
	if !b.db.HasRoomPassword(route.MeshKey) {
		b.react(ctx, m.ChannelID, m.ID, EmojiFail)
		// The modal is the whole reason the Gateway is worth having here: on
		// the ESP32 the password had to be typed as a channel message and
		// deleted afterwards, which was a compromise rather than a design.
		b.sayWithComponents(ctx, m.ChannelID,
			"**Not sent — no password for this room server.**\n"+
				"Room servers only accept posts from someone logged in. Press the button below and "+
				"type the password into the popup; it never enters channel history.",
			[]discord.Component{loginButtonRow(route.MeshKey, route.Label)})
		return
	}

	// A password is on file, so the session has simply lapsed — which happens
	// routinely, since the room never tells us its keep-alive interval. Hold
	// the message, log in, and send it when the session comes back. Asking the
	// user to retype something the bridge is perfectly capable of holding was
	// the wrong answer, and a cross would claim a failure that has not
	// happened yet.
	b.holdRoomPost(ctx, queuedPost{
		route: route, text: m.Content,
		uiChannel: m.ChannelID, uiMessage: m.ID, author: m.Author.Display(),
	})
}

// handleInboxMessage answers in the inbox.
//
// The inbox is read-only in practice. It aggregates traffic from senders that
// have no channel yet, so you can see what is out there — but an unprefixed
// reply in it is genuinely ambiguous, and a prefixed one means copying hex by
// hand. Link a channel and talk there instead.
func (b *Bridge) handleInboxMessage(ctx context.Context, m *discord.Message) {
	// Answer at most once a minute, so casual chatter is not met with a wall
	// of text on every line.
	b.snapMu.Lock()
	last := b.lastInboxHint
	now := time.Now()
	if now.Sub(last) < time.Minute {
		b.snapMu.Unlock()
		return
	}
	b.lastInboxHint = now
	b.snapMu.Unlock()

	b.say(ctx, m.ChannelID,
		"This channel is read-only in practice — it shows mesh traffic from senders that don't "+
			"have their own channel yet.\n"+
			"To talk to one of them, copy the `key` shown next to their name and run "+
			"`/mesh link` (or `add <key>` in "+mentionChannel(b.cfg.AdminChannel())+"). "+
			"You'll get a channel for them and can reply there.")
}

// sendRequest is one Discord message on its way to the radio.
type sendRequest struct {
	Route store.Route
	Text  string
	// UIChannel and UIMessage are where progress is reported and which message
	// carries the delivery marker. Empty for sends with no Discord origin.
	UIChannel string
	UIMessage string
	Author    string
	// Retry says this text has been on the air before, so each transmission
	// must repeat the timestamp it used and raise its attempt number rather
	// than going out as something new. Without it a resend is a duplicate at
	// the far end; see meshcore.EncodeSendTxtMsgRetry.
	Retry bool
	// ForceFlood asks for the stored path to be cleared first, the same thing a
	// `path:flood` prefix does, but without a prefix to type. A resend sets it:
	// the usual reason a message needs sending again is that the recorded route
	// no longer works, and repeating the send down the same dead path just fails
	// the same way. An explicit prefix in the text still wins over this.
	ForceFlood bool
}

func (r sendRequest) hasUI() bool { return r.UIChannel != "" && r.UIMessage != "" }

// sendToMesh transmits text, splitting it if it does not fit.
//
// A message longer than one transmission becomes several. Rather than
// hiding that, each chunk is echoed back into Discord as its own message and
// tracked separately, so you can see exactly how much airtime you used and
// which transmissions actually landed.
func (b *Bridge) sendToMesh(ctx context.Context, sess *meshcore.Session, req sendRequest) {
	// One transmission at a time. Interleaving two split messages would put
	// them on the air out of order, and the mesh is shared.
	b.txMu.Lock()
	defer b.txMu.Unlock()

	isChannel := req.Route.Kind == store.KindChannel
	text, wish, complain := ResolveRouteWish(req.Text, req.ForceFlood, isChannel)

	if complain {
		if req.hasUI() {
			b.say(ctx, req.UIChannel, "`path:` prefixes only apply to direct messages and room "+
				"servers. Channel messages are always flooded.")
		}
	} else if wish == RouteForceFlood {
		if err := sess.ResetPath(ctx, req.Route.MeshKey); err != nil {
			// Quietly. The message still goes out — down the stored path
			// instead of a fresh flood — so this is a lost optimisation, not a
			// failure, and the marker on the message reports what happened to
			// it either way.
			b.log.Info("could not clear the stored path; sending normally",
				"key", req.Route.MeshKey, "err", err)
		} else {
			b.log.Info("cleared the stored path to force a flood", "key", req.Route.MeshKey)
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		if req.hasUI() {
			b.reactVerdict(ctx, req.UIChannel, req.UIMessage, EmojiFail)
		}
		return
	}

	maxChunks := b.cfg.MaxChunks()

	// A group message costs the node's own name: it sends "<name>: <text>" and
	// truncates to fit, without telling anyone. Direct messages carry no such
	// prefix and get the full ceiling.
	limit := meshcore.MaxMsgLen
	if isChannel {
		limit = sess.MaxChannelTextLen()
	}

	// Ask the splitter itself how many transmissions this needs. Estimating it
	// separately under-counted, because splitting reserves 8 bytes per chunk
	// for the "[i/n] " prefix — so a ~390-character message passed the check
	// and was then silently truncated.
	needed := ChunkCount(text, limit)
	if needed > maxChunks {
		if req.hasUI() {
			b.reactVerdict(ctx, req.UIChannel, req.UIMessage, EmojiFail)
			b.say(ctx, req.UIChannel, fmt.Sprintf(
				"**Not sent — too long.** %d characters would need %d transmissions; the limit is "+
					"%d, which is %d characters of plain text (fewer with emoji). "+
					"Please send something shorter.",
				len(text), needed, maxChunks, ChunkCapacity(limit, maxChunks)))
		}
		b.recordRefusal(req, text, "too long")
		return
	}

	chunks := Chunk(text, limit, maxChunks)
	if len(chunks) == 0 {
		return
	}

	// The common case: one transmission, no extra noise.
	if len(chunks) == 1 {
		b.transmit(ctx, sess, req, chunks[0], text, 0, 0, req.UIChannel, req.UIMessage, wish)
		return
	}

	// No announcement that this is being split — a marker on the message says
	// it without a paragraph, and the numbered `[1/3]` transmissions posted
	// below carry the verdicts you would want to read afterwards.
	//
	// This one is never removed. It is not a verdict competing with the tick or
	// the cross; it explains why there are several messages underneath, and
	// that stays true forever.
	b.log.Info("splitting a message", "transmissions", len(chunks), "chars", len(text))
	if req.hasUI() {
		b.react(ctx, req.UIChannel, req.UIMessage, EmojiSplit)
	}

	// The original message waits for its pieces rather than claiming anything
	// now. Channels are the exception: nothing there is ever acknowledged, so
	// there is nothing to wait for and the satellite is the whole truth.
	var group *chunkGroup
	if req.hasUI() && !isChannel {
		group = &chunkGroup{
			channelID: req.UIChannel, messageID: req.UIMessage, total: len(chunks),
		}
	}

	// A resend of a message that was split before already has its pieces on
	// screen. Reuse them: posting a second identical set underneath, while the
	// first set sits there wearing crosses, makes it impossible to tell what is
	// being retried from what already failed.
	reuse := b.previousChunks(req, chunks)

	// One piece at a time, each waiting for the last to be acknowledged.
	//
	// The pieces are one message torn up, so their order is the message. Firing
	// them off back to back and sorting out the verdicts later meant a mesh that
	// dropped piece 2 still spent airtime on 3 and 4, and delivered a message
	// with a hole in the middle that read as though it were complete. Waiting
	// costs latency on a long message and nothing else — the radio can only send
	// one at a time regardless.
	//
	// Channels are exempt for the usual reason: nothing there is acknowledged,
	// so there is no outcome to wait for. Those keep the courtesy gap instead.
	for i, chunk := range chunks {
		// Already delivered on a previous attempt. Do not send it again: the far
		// end has it, and a duplicate is worse than a gap.
		if i < len(reuse) && reuse[i].Delivery == store.DeliveryDelivered {
			b.log.Info("skipping a transmission that already landed",
				"piece", i+1, "of", len(chunks))
			b.settleChunk(ctx, group, true, true)
			continue
		}

		// Each transmission gets its own Discord message, so partial delivery
		// is visible rather than hidden behind one ambiguous marker.
		echoChannel, echoMessage := "", ""
		if req.hasUI() {
			if i < len(reuse) && b.reclaimEcho(ctx, req.UIChannel, reuse[i].MessageID) {
				echoChannel, echoMessage = req.UIChannel, reuse[i].MessageID
			} else if sent, err := b.rest.SendMessage(ctx, req.UIChannel, chunk); err == nil {
				echoChannel, echoMessage = req.UIChannel, sent.ID
				b.react(ctx, echoChannel, echoMessage, EmojiWaiting)
			}
		}

		ok, settled := b.transmitChunk(ctx, sess, req, chunk, chunk, i+1, len(chunks),
			echoChannel, echoMessage, wish, group)
		if !ok {
			// The radio would not take it. Nothing after this is attempted.
			b.abandonRest(ctx, req, group, chunks, reuse, i+1)
			return
		}

		if isChannel || settled == nil {
			// Nothing to wait for. Space the transmissions out as a courtesy to
			// everybody else sharing the air.
			if i+1 < len(chunks) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(b.cfg.ChunkGap()):
				}
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case delivered := <-settled:
			if !delivered {
				// This piece did not arrive, so the rest are pointless: the
				// message is already incomplete at the far end, and sending the
				// remainder spends airtime to make a hole rather than fill one.
				b.log.Info("a transmission was not acknowledged; abandoning the rest",
					"piece", i+1, "of", len(chunks))
				b.abandonRest(ctx, req, group, chunks, reuse, i+1)
				return
			}
		}
	}

	// Every piece went out and, where it could be, was acknowledged. A channel
	// gets the satellite here because nothing there is ever acknowledged;
	// anywhere else the group has already answered the original as its last
	// piece settled.
	if req.hasUI() && group == nil {
		b.reactVerdict(ctx, req.UIChannel, req.UIMessage, EmojiSent)
	}
}

// previousChunks finds the messages posted for this message's pieces last time,
// if they still line up with what is about to be sent.
//
// Lining up means the same number of pieces carrying the same text. Anything
// else — the message was edited, the split landed differently, the history was
// pruned — and this returns nothing, so fresh ones are posted. Being wrong here
// would put markers on messages whose text no longer matches what went out,
// which is worse than a duplicate.
func (b *Bridge) previousChunks(req sendRequest, chunks []string) []store.ChunkEcho {
	if !req.hasUI() {
		return nil
	}
	prev, err := b.db.ChunkEchoes(req.UIMessage)
	if err != nil || len(prev) != len(chunks) {
		return nil
	}
	for i, c := range prev {
		if c.Body != chunks[i] || c.MessageID == "" {
			return nil
		}
	}
	return prev
}

// reclaimEcho puts a piece of a split back into the in-progress state, and
// reports whether the message is still there to be reused.
//
// Discord answers a reaction on a deleted message with 404, which is the only
// way to find out that somebody tidied the channel up.
func (b *Bridge) reclaimEcho(ctx context.Context, channelID, messageID string) bool {
	if err := b.rest.React(ctx, channelID, messageID, EmojiWaiting); err != nil {
		if discord.IsNotFound(err) {
			b.log.Info("a transmission from last time is gone; posting a fresh one",
				"message", messageID)
		} else {
			b.log.Warn("could not reuse a previous transmission", "message", messageID, "err", err)
		}
		return false
	}
	// Clear last time's verdict, or the piece would wear both.
	for _, e := range []string{EmojiOK, EmojiFail, EmojiSent} {
		_ = b.rest.Unreact(ctx, channelID, messageID, e)
	}
	return true
}

// settleChunk applies one transmission's outcome to the message it came from.
func (b *Bridge) settleChunk(ctx context.Context, g *chunkGroup, delivered, knowable bool) {
	v := g.settle(delivered, knowable)
	if v == "" {
		return
	}
	b.log.Info("a split message is fully accounted for",
		"message", g.messageID, "transmissions", g.total, "verdict", v)
	b.setVerdict(ctx, g.channelID, g.messageID, v)
}

// setVerdict puts one marker on a message and clears any that contradict it.
//
// reactVerdict only ever removes the hourglass, which is right for a message
// getting its first answer. This one is for a message whose answer can change —
// a split whose last piece decides it, or a cross corrected by a late
// acknowledgement — so every other marker we might have put there has to go.
func (b *Bridge) setVerdict(ctx context.Context, channelID, messageID, emoji string) {
	for _, e := range []string{EmojiWaiting, EmojiOK, EmojiFail, EmojiSent} {
		if e != emoji {
			_ = b.rest.Unreact(ctx, channelID, messageID, e)
		}
	}
	b.react(ctx, channelID, messageID, emoji)
}

// transmit puts a message on the air and applies the right marker.
func (b *Bridge) transmit(ctx context.Context, sess *meshcore.Session, req sendRequest,
	wire, recordBody string, chunkIdx, chunkTotal int,
	uiChannel, uiMessage string, wish RouteWish) bool {
	ok, _ := b.transmitChunk(ctx, sess, req, wire, recordBody, chunkIdx, chunkTotal,
		uiChannel, uiMessage, wish, nil)
	return ok
}

// transmitChunk is transmit with the group a split message belongs to, so the
// original can be answered once every piece has been.
//
// The second return is closed-over by the acknowledgement machinery: it carries
// true when this transmission is confirmed and false when it times out, and is
// nil when there is nothing to wait for (a channel, or a send the node gave no
// acknowledgement handle for). The split loop uses it to hold the next piece
// back until this one has landed.
func (b *Bridge) transmitChunk(ctx context.Context, sess *meshcore.Session, req sendRequest,
	wire, recordBody string, chunkIdx, chunkTotal int,
	uiChannel, uiMessage string, wish RouteWish, group *chunkGroup) (bool, <-chan bool) {

	rec := store.Message{
		Direction:  "out",
		Kind:       req.Route.Kind,
		MeshKey:    req.Route.MeshKey,
		PeerLabel:  req.Route.Label,
		Body:       recordBody,
		ChannelID:  uiChannel,
		MessageID:  uiMessage,
		DiscordUsr: req.Author,
		ChunkIndex: chunkIdx,
		ChunkTotal: chunkTotal,
		CreatedAt:  time.Now(),
	}
	if group != nil {
		// Only the pieces of a split carry this; it is how a resend finds them
		// again.
		rec.ParentMsgID = group.messageID
	}

	isChannel := req.Route.Kind == store.KindChannel
	if isChannel {
		err := sess.SendChannel(ctx, mustSlot(req.Route.MeshKey), wire)
		if err != nil {
			b.log.Warn("channel send rejected", "slot", req.Route.MeshKey, "err", err)
			rec.Delivery = store.DeliveryFailed
			_, _ = b.db.InsertMessage(rec)
			if uiMessage != "" {
				b.reactVerdict(ctx, uiChannel, uiMessage, EmojiFail)
			}
			return false, nil
		}
		// MeshCore cannot acknowledge group messages, so the honest marker is
		// "transmitted", never a tick.
		rec.Delivery = store.DeliveryTransmitted
		_, _ = b.db.InsertMessage(rec)
		if uiMessage != "" {
			b.reactVerdict(ctx, uiChannel, uiMessage, EmojiSent)
		}
		b.log.Info("discord -> mesh", "kind", "channel", "slot", req.Route.MeshKey, "bytes", len(wire))
		return true, nil
	}

	sent, attempt := time.Now(), uint8(0)
	if req.Retry && uiMessage != "" {
		// Repeat what this exact message went out as last time. The far end
		// identifies a message by its timestamp, so reusing it is what makes
		// the retry a retry instead of a second message; the attempt number
		// keeps the packet distinguishable so the mesh does not drop it as a
		// duplicate of the first try.
		if prev, ok := b.db.LastSend(uiMessage); ok {
			sent, attempt = prev.SentTS, prev.Attempt+1
			b.log.Info("resending a message the far end may already have",
				"attempt", attempt, "sent", sent.Unix())
		}
	}
	rec.SentTS, rec.Attempt = sent, attempt

	res, err := sess.SendDMAttempt(ctx, req.Route.MeshKey, wire, sent, attempt)
	if err != nil {
		b.log.Warn("direct message rejected", "key", req.Route.MeshKey, "err", err)
		rec.Delivery = store.DeliveryFailed
		_, _ = b.db.InsertMessage(rec)
		if uiMessage != "" {
			b.reactVerdict(ctx, uiChannel, uiMessage, EmojiFail)
		}
		// The caller abandons the group and reports the failure itself, so
		// nothing is settled here.
		return false, nil
	}
	rec.Flooded = res.Flooded
	rec.Ack = res.ExpectedAck

	if res.ExpectedAck == 0 {
		// No ack handle: the node cannot tell us about this one either.
		rec.Delivery = store.DeliveryTransmitted
		_, _ = b.db.InsertMessage(rec)
		if uiMessage != "" {
			b.reactVerdict(ctx, uiChannel, uiMessage, EmojiSent)
		}
		b.settleChunk(ctx, group, false, false)
		return true, nil
	}

	rec.Delivery = store.DeliveryPending
	rowID, _ := b.db.InsertMessage(rec)
	settled := b.addPending(res, req, uiChannel, uiMessage, rowID, group)
	b.log.Info("discord -> mesh", "kind", string(req.Route.Kind), "key", req.Route.MeshKey,
		"bytes", len(wire), "flood", res.Flooded, "ack", res.ExpectedAck)
	return true, settled
}

// abandonRest fails every piece from `from` onward without transmitting any of
// them, and answers the original.
//
// Sending the remainder of a message whose earlier piece never arrived spends
// airtime to deliver something already broken — everybody on the mesh pays to
// relay a message that cannot be read at the far end. The pieces that were
// never attempted say so plainly rather than sitting on an hourglass forever.
func (b *Bridge) abandonRest(ctx context.Context, req sendRequest, group *chunkGroup,
	chunks []string, reuse []store.ChunkEcho, from int) {

	group.abandon()
	for i := from; i < len(chunks); i++ {
		if i < len(reuse) && reuse[i].MessageID != "" {
			b.setVerdict(ctx, req.UIChannel, reuse[i].MessageID, EmojiFail)
		}
	}
	if req.hasUI() {
		b.setVerdict(ctx, req.UIChannel, req.UIMessage, EmojiFail)
		if skipped := len(chunks) - from; skipped > 0 {
			b.say(ctx, req.UIChannel, fmt.Sprintf(
				"**Transmission %d of %d was not acknowledged, so the remaining %d were not sent.** "+
					"A message missing a piece cannot be read at the other end, and sending the rest "+
					"would spend airtime making that worse. React "+EmojiRetry+
					" on the original to try again — anything that already landed is not sent twice.",
				from, len(chunks), skipped))
		}
	}
}

func (b *Bridge) recordRefusal(req sendRequest, text, why string) {
	_, _ = b.db.InsertMessage(store.Message{
		Direction:  "out",
		Kind:       req.Route.Kind,
		MeshKey:    req.Route.MeshKey,
		PeerLabel:  req.Route.Label,
		Body:       text,
		ChannelID:  req.UIChannel,
		MessageID:  req.UIMessage,
		DiscordUsr: req.Author,
		Delivery:   store.DeliveryRefused,
		CreatedAt:  time.Now(),
	})
	b.db.LogEvent("info", "bridge", "refused an outbound message: "+why)
}

func mustSlot(key string) byte {
	idx, _ := parseChannelSlot(key)
	return idx
}

// ---------------------------------------------------------------------------
// Delivery receipts
// ---------------------------------------------------------------------------

func (b *Bridge) addPending(res meshcore.SendResult, req sendRequest, channelID, messageID string,
	rowID int64, group *chunkGroup) <-chan bool {
	// How long to wait for the recipient's node to acknowledge.
	//
	// The estimate is the node's own and it already accounts for the round
	// trip, not just the outbound airtime — see minAckWait. The claim that used
	// to be here, that the official app waits up to two minutes, was wrong:
	// meshcore_py's send_msg_with_retry waits `suggested_timeout * 1.2` per
	// attempt and spends its patience on retrying instead.
	//
	// The asymmetry decides the numbers. Giving up too early marks a message
	// that WAS delivered with a cross, and the natural response to a cross is
	// to send it again — so impatience costs airtime that everybody on the
	// channel pays for. Waiting too long costs only a tick that arrives late.
	// So: be generous, and take the node's estimate as a lower bound rather
	// than an answer.
	timeout := res.EstTimeout * 2
	if timeout < minAckWait {
		timeout = minAckWait
	}
	if timeout > maxAckWait {
		timeout = maxAckWait
	}
	b.pendMu.Lock()
	b.pending[res.ExpectedAck] = &pendingSend{
		ack:       res.ExpectedAck,
		deadline:  time.Now().Add(timeout),
		window:    timeout,
		channelID: channelID,
		messageID: messageID,
		rowID:     rowID,
		route:     req.Route,
		flooded:   res.Flooded,
		group:     group,
		// Buffered, and written exactly once: whoever resolves this send must
		// never block on a reader that may have gone away.
		settled: make(chan bool, 1),
	}
	settled := b.pending[res.ExpectedAck].settled
	n := len(b.pending)
	b.pendMu.Unlock()

	// The window matters when a message is reported unconfirmed: it says
	// whether we gave the mesh a fair chance or gave up early.
	b.log.Info("awaiting delivery confirmation", "ack", res.ExpectedAck,
		"node_estimate", res.EstTimeout, "waiting", timeout, "outstanding", n)
	return settled
}

func (b *Bridge) handleConfirmation(ctx context.Context, c meshcore.Confirmation) {
	b.pendMu.Lock()
	p, ok := b.pending[c.Ack]
	if ok {
		delete(b.pending, c.Ack)
	}
	b.pendMu.Unlock()
	if !ok {
		// An ack for something already given up on. This is not hypothetical
		// and it is not the node being odd: its send timeout does not clear the
		// expected-ack entry, so a confirmation genuinely can arrive after we
		// stopped waiting, and the message DID get through.
		//
		// Correcting the cross is the honest thing to do. Leaving it says a
		// message failed when it landed, and the natural response to a cross is
		// to send it again — a duplicate at the far end, paid for in airtime by
		// everybody on the mesh.
		if b.correctLateDelivery(ctx, c) {
			return
		}
		// Otherwise the handles are not matching at all, which would mean
		// nothing ever gets ticked. Worth seeing.
		b.log.Warn("delivery confirmation matched no outstanding message",
			"ack", c.Ack, "round_trip", c.RoundTrip)
		return
	}
	b.log.Info("message delivered", "round_trip", c.RoundTrip, "kind", p.route.Kind)
	if p.rowID > 0 {
		_ = b.db.SetDelivery(p.rowID, store.DeliveryDelivered, c.RoundTrip)
	}
	p.resolve(true)
	b.settleChunk(ctx, p.group, true, true)
	if p.messageID != "" {
		// For a room this upgrades the satellite to a tick, which is a real
		// improvement in knowledge: some firmware may acknowledge where this
		// one does not.
		b.reactVerdict(ctx, p.channelID, p.messageID, EmojiOK)
	}
}

// lateAckGrace is how long an expired send is kept around so a confirmation
// that arrives after the deadline can still correct the verdict.
//
// The node imposes no time limit of its own — an unmatched expected-ack entry
// survives until eight further sends overwrite it — so this is a bound on
// bookkeeping, not on the protocol. Long enough to cover a slow mesh, short
// enough that a tick never lands on a message nobody remembers sending.
const lateAckGrace = 10 * time.Minute

// correctLateDelivery upgrades a message that was crossed and then acknowledged.
// It reports whether the ack belonged to one.
func (b *Bridge) correctLateDelivery(ctx context.Context, c meshcore.Confirmation) bool {
	b.pendMu.Lock()
	p, ok := b.failed[c.Ack]
	if ok {
		delete(b.failed, c.Ack)
	}
	b.pendMu.Unlock()
	if !ok {
		return false
	}

	b.log.Info("a message crossed as undelivered was acknowledged after all",
		"ack", c.Ack, "round_trip", c.RoundTrip, "waited", p.window, "kind", p.route.Kind)
	if p.rowID > 0 {
		_ = b.db.SetDelivery(p.rowID, store.DeliveryDelivered, c.RoundTrip)
	}
	if p.messageID != "" {
		// Take the cross off explicitly: reactVerdict only ever removes the
		// hourglass, and that is long gone by now.
		b.setVerdict(ctx, p.channelID, p.messageID, EmojiOK)
	}
	// If this was one piece of a split message, the original may have been
	// crossed on the strength of it. Re-decide.
	if v := p.group.correct(); v != "" {
		b.setVerdict(ctx, p.group.channelID, p.group.messageID, v)
	}
	// A room post that was acknowledged proves the login is fine, so undo the
	// pessimism from when it expired.
	if p.route.Kind == store.KindRoom {
		b.noteRoomAlive(p.route.MeshKey)
	}
	return true
}

// noteRoomAlive records that a room accepted a post, which is proof of a
// working session — better proof than a login, since it is the thing we
// actually wanted to do.
func (b *Bridge) noteRoomAlive(prefix string) {
	b.roomMu.Lock()
	defer b.roomMu.Unlock()
	if s, ok := b.rooms[prefix]; ok && !s.loggedIn {
		s.loggedIn, s.loggedInAt = true, time.Now()
	}
}

// expirePending marks messages failed once their deadline passes.
func (b *Bridge) expirePending(ctx context.Context) {
	now := time.Now()
	var expired []*pendingSend

	b.pendMu.Lock()
	for ack, p := range b.pending {
		if now.After(p.deadline) {
			expired = append(expired, p)
			delete(b.pending, ack)
			// Remember it briefly: the node can still push this ack, and a
			// message that actually landed should not keep a cross.
			p.failedAt = now
			b.failed[ack] = p
		}
	}
	for ack, p := range b.failed {
		if now.Sub(p.failedAt) > lateAckGrace {
			delete(b.failed, ack)
		}
	}
	b.pendMu.Unlock()

	for _, p := range expired {
		b.log.Info("no delivery confirmation before the deadline",
			"ack", p.ack, "window", p.window, "kind", p.route.Kind)

		if p.rowID > 0 {
			_ = b.db.SetDelivery(p.rowID, store.DeliveryFailed, 0)
		}
		if p.messageID != "" {
			b.reactVerdict(ctx, p.channelID, p.messageID, EmojiFail)
		}
		// Release whoever is holding the next piece back, then answer the
		// message this was split from.
		p.resolve(false)
		b.settleChunk(ctx, p.group, false, true)

		// A room post that goes unacknowledged is the one signal that our login
		// is gone: rooms DO acknowledge posts from anyone with READ_WRITE or
		// better, so silence means the packet was lost or the room no longer has
		// us in its client table — which happens when a 21st client logs in and
		// displaces the least recently active one. Either way, dropping the
		// session is right: the next post logs in first, which costs a round
		// trip and fixes the case where it matters.
		if p.route.Kind == store.KindRoom {
			b.forgetRoomSession(p.route.MeshKey)
			b.log.Info("a room post was not acknowledged; assuming the login is gone",
				"key", p.route.MeshKey)
		}

		// Sent along a stored path and never acknowledged. That path is the
		// prime suspect — a route the contact has since moved off is followed
		// faithfully into nowhere, and this is exactly how a room server looked
		// unreachable while answering logins fine.
		//
		// Clear it rather than resending: the message may well have arrived
		// and only the acknowledgement was lost, so an automatic retry could
		// post it twice. Clearing is free, cannot duplicate anything, and makes
		// the next attempt flood and relearn the route from the reply.
		if !p.flooded && p.route.MeshKey != "" {
			if sess := b.link.Session(); sess != nil {
				if err := sess.ResetPath(ctx, p.route.MeshKey); err == nil {
					// Silently. The cross already says it failed, and a
					// paragraph about internal route bookkeeping on every
					// unacknowledged message is noise in a channel meant for
					// conversation.
					b.log.Info("cleared a stored path that produced no acknowledgement",
						"key", p.route.MeshKey)
				}
			}
		}
	}
}

// react adds a marker, quietly.
func (b *Bridge) react(ctx context.Context, channelID, messageID, emoji string) {
	if channelID == "" || messageID == "" {
		return
	}
	if err := b.rest.React(ctx, channelID, messageID, emoji); err != nil && !discord.IsNotFound(err) {
		b.log.Debug("could not add a reaction", "err", err)
	}
}

// reactVerdict applies a final marker, taking down the in-progress one first
// if this message was a resend.
// reactVerdict replaces the in-progress marker with a final answer.
//
// The hourglass always comes off first: leaving it beside a verdict reads as
// "still working" when it is not. This used to be tracked so the extra REST
// call could be skipped for messages that had no marker — but every message
// gets one now, so the bookkeeping was only a way to get it wrong.
func (b *Bridge) reactVerdict(ctx context.Context, channelID, messageID, emoji string) {
	_ = b.rest.Unreact(ctx, channelID, messageID, EmojiWaiting)
	b.react(ctx, channelID, messageID, emoji)
}

// say posts a plain message, ignoring the id.
func (b *Bridge) say(ctx context.Context, channelID, text string) {
	if channelID == "" {
		return
	}
	// Warn, not Debug. A message the bridge could not deliver is usually the
	// one explaining why something else did not work, so silence here turns a
	// clear refusal into an unexplained failure.
	if _, err := b.rest.SendMessage(ctx, channelID, clampReply(text)); err != nil {
		b.log.Warn("could not post to Discord", "channel", channelID, "err", err)
	}
}

// sayWithComponents posts a message with buttons, falling back to plain text.
//
// The explanation matters more than the button. A malformed component makes
// Discord reject the ENTIRE message, so without this fallback a refused send
// showed a bare ❌ and nothing else — the user is told their message failed and
// given no way to find out why. Losing a button is a nuisance; losing the
// sentence explaining what to do is the actual failure.
func (b *Bridge) sayWithComponents(ctx context.Context, channelID, text string, comps []discord.Component) {
	if channelID == "" {
		return
	}
	_, err := b.rest.SendMessageWith(ctx, channelID, discord.CreateMessage{
		Content: clampReply(text), Components: comps,
	})
	if err == nil {
		return
	}
	b.log.Warn("could not post a message with components; retrying as plain text",
		"channel", channelID, "err", err)
	b.db.LogEvent("warn", "discord", "a message with buttons was rejected: "+err.Error())
	b.say(ctx, channelID, text)
}

// ---------------------------------------------------------------------------
// Resend
// ---------------------------------------------------------------------------

// handleRetry resends a message when someone replies `retry` to it.
//
// CURRENTLY UNREACHABLE: the dispatch in handleRoutedMessage is commented out
// while the reaction path is tried on its own. Kept compiled so it does not
// rot, and so restoring it is uncommenting four lines.
//
// A reply rather than a reaction was the ESP32's only option, since reactions
// cannot be polled for; the Gateway makes both possible.
func (b *Bridge) handleRetry(ctx context.Context, route store.Route, m *discord.Message) {
	if m.MessageReference == nil || m.MessageReference.MessageID == "" {
		b.say(ctx, m.ChannelID,
			"To resend a message, add the "+EmojiRetry+" reaction **to that message** — the one "+
				"that failed, not this one.")
		return
	}
	b.resendMessage(ctx, route, m.ChannelID, m.MessageReference.MessageID, m.ReferencedMessage)
}

// onReactionAdd turns a 🔄 reaction into a resend.
//
// This is the capability that was flatly impossible before: reactions are
// delivered only over the Gateway, and there is no REST endpoint to poll for
// them.
//
// The resend forces a flood where it can — see resendMessage.
func (b *Bridge) onReactionAdd(ctx context.Context, e *discord.ReactionEvent) {
	if e == nil || e.Emoji.Name != EmojiRetry {
		return
	}
	if b.isOwnBot(e.UserID) {
		return // our own in-progress marker, not a request
	}
	route, err := b.db.RouteByChannel(e.ChannelID)
	if err != nil {
		return
	}

	// Consume the reaction: it was a button press, not a state to display.
	//
	// Two reasons this matters. The bridge shows progress and then a verdict
	// using its own reactions, and it can only ever remove its own — so a
	// reaction left in place ends up sitting next to the tick, having outlived
	// what it meant. And Discord will not send another event for a reaction
	// that is already there, so leaving it makes a second press do nothing.
	if err := b.rest.UnreactUser(ctx, e.ChannelID, e.MessageID, EmojiRetry, e.UserID); err != nil {
		// Not fatal — the hourglass that follows is a different emoji, so
		// nothing looks doubled. It does mean pressing it again will do
		// nothing, because Discord sends no event for a reaction that is
		// already present.
		b.log.Warn("could not clear the retry reaction; the bot needs the Manage Messages "+
			"permission, and without it a second press will not register", "channel", e.ChannelID)
	}
	b.resendMessage(ctx, route, e.ChannelID, e.MessageID, nil)
}

// resendMessage puts a previously sent message back on the air, forcing a flood
// where the route has one to clear.
//
// Sending the same text down the same recorded path is the one retry least
// likely to work: a message usually needs resending because the path is stale —
// somebody moved, or a repeater in the middle went away — and the node keeps
// using a stored path until something proves it wrong. Clearing it first makes
// the retry flood, which is how the path gets relearned.
//
// "Where possible" means direct messages and room servers. A mesh CHANNEL has
// no stored path at all — group messages are not addressed to a contact — so
// there is nothing to reset and those resends go out unchanged.
func (b *Bridge) resendMessage(ctx context.Context, route store.Route, channelID, messageID string, inlined *discord.Message) {
	text, fromBot := "", false
	if inlined != nil {
		// Discord usually inlines the message a reply refers to, which saves a
		// fetch — but it does not promise to.
		text, fromBot = inlined.Content, inlined.IsFromBot()
	} else {
		orig, err := b.rest.GetMessage(ctx, channelID, messageID)
		if err != nil {
			b.say(ctx, channelID, "Could not read that message to resend it.")
			return
		}
		text, fromBot = orig.Content, orig.IsFromBot()
	}

	// A bot message is only resendable if it is one of our own transmissions.
	// Anything else is a status line, and echoing that onto the mesh is
	// nonsense.
	_, isChunk := StripChunkMarker(text)
	if fromBot {
		// A single piece of a split is not resendable on its own.
		//
		// It used to be, and it was a trap: the pieces are one message torn up
		// and they are sent in order, each waiting on the last, so putting one
		// back on the air by itself either duplicates something the far end
		// already has or arrives out of order into a gap. The original knows
		// which pieces still need sending; a piece does not.
		if isChunk {
			b.setVerdict(ctx, channelID, messageID, EmojiFail)
			b.say(ctx, channelID,
				"**That is one transmission of a longer message, and cannot be resent on its own.** "+
					"React "+EmojiRetry+" on the original message instead — the one marked "+
					EmojiSplit+". Anything that already landed will not be sent twice.")
			return
		}
		b.say(ctx, channelID,
			"That is one of my own status messages, not something that was sent to the mesh. "+
				"React "+EmojiRetry+" on your own message instead.")
		return
	}

	text = strings.TrimSpace(text)
	if text == "" {
		b.say(ctx, channelID, "That message has no text to resend.")
		return
	}

	// Clear the old verdict so the new one is not read alongside a stale
	// cross. Only our OWN reactions — removing anyone else's would need
	// MANAGE_MESSAGES.
	for _, e := range []string{EmojiFail, EmojiSent, EmojiOK, EmojiWaiting} {
		_ = b.rest.Unreact(ctx, channelID, messageID, e)
	}

	sess := b.link.Session()
	if sess == nil {
		b.react(ctx, channelID, messageID, EmojiFail)
		b.say(ctx, channelID, "**Not resent — no link to the node right now.** "+
			"If the device restarted recently, give it a few minutes; reconnecting is "+
			"automatic. Then react "+EmojiRetry+" again.")
		return
	}

	// A room with no live session gets the same treatment as a first attempt:
	// hold the message, log in, send it when the session comes back. Telling
	// the user to press the button again is not a design, it is the bridge
	// refusing to do the one job it is here for.
	if route.Kind == store.KindRoom && !b.roomLoggedIn(route.MeshKey) {
		if !b.db.HasRoomPassword(route.MeshKey) {
			b.reactVerdict(ctx, channelID, messageID, EmojiFail)
			b.sayWithComponents(ctx, channelID,
				"**Not resent — no password for this room server.**\nSet one and it will go out:",
				[]discord.Component{loginButtonRow(route.MeshKey, route.Label)})
			return
		}
		b.holdRoomPost(ctx, queuedPost{
			route: route, text: text,
			uiChannel: channelID, uiMessage: messageID, author: "resend",
			forceFlood: true, retry: true,
		})
		return
	}

	// Mark it in progress. The hourglass, not the arrows: the arrows are what
	// YOU add to ask for a resend, and echoing them back reads as though the
	// request is still sitting there unhandled.
	b.react(ctx, channelID, messageID, EmojiWaiting)
	b.log.Info("resending", "message", messageID, "channel", channelID)

	// The verdict lands back on the ORIGINAL message, which is where the
	// person is looking, rather than on the word "retry".
	b.sendToMesh(ctx, sess, sendRequest{
		Route:      route,
		Text:       text,
		UIChannel:  channelID,
		UIMessage:  messageID,
		Author:     "resend",
		Retry:      true,
		ForceFlood: true,
	})
}

// ---------------------------------------------------------------------------
// Room-server sessions
// ---------------------------------------------------------------------------
//
// NOTE, because the two are easy to confuse: this is only about ROOM SERVERS.
// A private mesh CHANNEL is not password-protected in this sense — it is
// encrypted with a 16-byte shared secret held on the node itself, so if your
// radio has the channel, it can already read and post to it and there is
// nothing to log in to. Room servers are the only thing with an account.

// roomSession is what the bridge believes about one room server.
//
// The companion protocol does not forward the server's keep-alive interval, so
// there is no expiry to schedule against. Sessions are re-established on
// events instead: whenever the link returns, and whenever a post finds no
// session — the two things that actually indicate a dead one.
type roomSession struct {
	loggedIn    bool
	lastAttempt time.Time
	lastResult  string

	// loginInFlight and loginStartedAt bound the wait for a verdict. The
	// answer comes back over the air and the air is lossy; without a deadline
	// a lost answer left the room permanently "not logged in", so every post
	// was refused with the same advice and nothing ever followed up.
	loginInFlight  bool
	loginStartedAt time.Time
	attempts       int
	// announce marks a login somebody actually asked for. Routine ones — on
	// reconnect, or when a session has simply aged out — happen constantly and
	// saying so every time is noise in a channel meant for messages.
	announce bool
	// loggedInAt is when the room last confirmed a login, and doubles as when
	// the keep-alive last refreshed it.
	loggedInAt time.Time
	// lastReported throttles unattended failures. The keep-alive rediscovers a
	// rejected password every few hours forever, and saying so every time is
	// the noise this whole design is trying to avoid.
	lastReported time.Time

	// pending holds posts typed while a login is in flight. Discarding them
	// and asking the user to retype was the wrong answer: the bridge knows a
	// login is coming, so it can wait and then send.
	pending []queuedPost
}

// queuedPost is a message waiting for a room-server session.
type queuedPost struct {
	route     store.Route
	text      string
	uiChannel string
	uiMessage string
	author    string
	// forceFlood survives the wait. A resend held for a room login still wants
	// its path cleared when it finally goes out — dropping the flag here would
	// make 🔄 behave differently purely because the room happened to be logged
	// out at the time.
	forceFlood bool
	// retry survives the wait for the same reason forceFlood does: a resend
	// held for a room login is still a resend when it finally goes out, and
	// losing the flag here would duplicate it at the far end.
	retry bool
}

const (
	// roomLoginAttemptTimeout is how long one login attempt may go unanswered.
	roomLoginAttemptTimeout = 45 * time.Second
	// maxRoomLoginAttempts is how many times to try before giving up and
	// saying so. Retries are silent: a room that answers on the second attempt
	// is a working room, and narrating each attempt is noise.
	maxRoomLoginAttempts = 3
	// roomQueueMax caps held posts per room — enough for a few lines typed
	// while a login completes, small enough that a wedged room cannot build a
	// backlog nobody wants sent later.
	roomQueueMax = 5
)

// roomState returns the session record, creating it. Caller must hold roomMu.
func (b *Bridge) roomState(prefix string) *roomSession {
	s, ok := b.rooms[prefix]
	if !ok {
		s = &roomSession{}
		b.rooms[prefix] = s
	}
	return s
}

// roomLoggedIn reports whether the bridge has a session it still trusts.
//
// The window is a re-check interval, not a lifetime: a room login does not
// expire (see config.DefaultRoomSessionSeconds). What ends one is eviction from
// the room's 20-slot client table, and that is not on a clock — so this only
// bounds how long we go without proving the login is still there, and the
// keep-alive normally does that proving before anybody notices.
func (b *Bridge) roomLoggedIn(prefix string) bool {
	ttl := b.cfg.RoomTrustWindow()
	b.roomMu.Lock()
	defer b.roomMu.Unlock()
	s, ok := b.rooms[prefix]
	if !ok || !s.loggedIn {
		return false
	}
	if ttl == 0 {
		return false // log in before every message
	}
	return time.Since(s.loggedInAt) < ttl
}

// holdRoomPost queues a message and starts a login, saying nothing.
//
// The hourglass on the message is the whole notification. A cross here would
// be a lie — nothing has failed yet — and a sentence explaining that a login
// is under way is noise on every single post.
func (b *Bridge) holdRoomPost(ctx context.Context, p queuedPost) {
	b.roomMu.Lock()
	rs := b.roomState(p.route.MeshKey)
	if len(rs.pending) >= roomQueueMax {
		b.roomMu.Unlock()
		b.reactVerdict(ctx, p.uiChannel, p.uiMessage, EmojiFail)
		return
	}
	// A fresh cycle: this is a new problem, so give it a full set of attempts.
	if len(rs.pending) == 0 && !rs.loginInFlight {
		rs.attempts = 0
	}
	rs.pending = append(rs.pending, p)
	held := len(rs.pending)
	b.roomMu.Unlock()

	b.react(ctx, p.uiChannel, p.uiMessage, EmojiWaiting)
	b.log.Info("holding a room post until login completes", "key", p.route.MeshKey, "held", held)
	b.tryRoomLogin(ctx, p.route.MeshKey)
}

// tryRoomLogin sends a login if we hold a password and are not already mid
// attempt. Reports whether one actually went out.
func (b *Bridge) tryRoomLogin(ctx context.Context, prefix string) bool {
	pw := b.db.RoomPassword(prefix)
	if pw == "" {
		return false
	}
	sess := b.link.Session()
	if sess == nil {
		return false
	}

	b.roomMu.Lock()
	s := b.roomState(prefix)
	// Logging in costs airtime and the reply takes seconds to come back. Do
	// not retry faster than that, or a busy channel turns into a login storm.
	if s.loginInFlight || (!s.lastAttempt.IsZero() && time.Since(s.lastAttempt) < 30*time.Second) {
		b.roomMu.Unlock()
		return false
	}
	s.lastAttempt = time.Now()
	s.loginStartedAt = time.Now()
	s.loginInFlight = true
	s.attempts++
	attempt := s.attempts
	b.roomMu.Unlock()

	b.log.Info("logging in to a room server", "key", prefix, "attempt", attempt)
	if err := b.sendRoomLogin(ctx, sess, prefix, pw); err != nil {
		b.roomMu.Lock()
		b.roomState(prefix).loginInFlight = false
		b.roomMu.Unlock()
		b.log.Warn("the node would not send the login", "key", prefix, "err", err)
		if errors.Is(err, meshcore.ErrNotContact) {
			b.giveUpOnRoom(ctx, prefix, "The radio does not have this room server as a contact. "+
				"Add it with `contact add <64-hex-key> room <name>`, then try again.", false)
		}
		return false
	}
	return true
}

// sendRoomLogin resolves the full public key and sends the login.
//
// The live cache is tried first, then the database mirror. A login needs the
// full 32-byte key and the cache can be missing an entry — a contact
// enumeration that ended early used to leave one out — but the mirror still
// holds the key from when it was last seen. Falling back to it means a
// transient gap in the cache no longer locks you out of a room you have a
// password for.
func (b *Bridge) sendRoomLogin(ctx context.Context, sess *meshcore.Session, prefix, password string) error {
	if err := sess.RoomLogin(ctx, prefix, password); !errors.Is(err, meshcore.ErrNotContact) {
		return err
	}
	c, ok := b.db.ContactByPrefix(prefix)
	if !ok || c.PubKey == "" {
		return meshcore.ErrNotContact
	}
	key, err := meshcore.ParsePubKey(c.PubKey)
	if err != nil {
		return meshcore.ErrNotContact
	}
	b.log.Info("room key was missing from the radio's cache; using the stored one", "key", prefix)
	return sess.RoomLoginKey(ctx, key, password)
}

// loginAllRooms re-establishes every room session we hold a password for.
//
// It used to wipe every session on reconnect, on the grounds that a session
// does not survive the link going away. That was wrong, and expensively so: the
// session is not ours to lose. Logging in writes a permission byte into the
// ROOM's client table, which the room saves to its own flash — so it survives
// our radio disconnecting, our process restarting, and the room rebooting.
//
// So the state is restored rather than discarded, from the last login the
// database recorded, and only rooms with nothing usable on file are logged in
// here. That turns a reconnect from a burst of airtime — one login per room,
// every time a USB cable is nudged — into nothing at all.
func (b *Bridge) loginAllRooms(ctx context.Context, sess *meshcore.Session) {
	routes, err := b.db.Routes()
	if err != nil {
		return
	}
	window := b.cfg.RoomTrustWindow()

	b.roomMu.Lock()
	for _, r := range routes {
		if r.Kind != store.KindRoom {
			continue
		}
		s := b.roomState(r.MeshKey)
		// Anything mid-flight belonged to the old link and is not coming back.
		s.loginInFlight = false
		s.attempts = 0
		if s.loggedIn {
			continue // already known good in this process
		}
		if at, result := b.db.LastLogin(r.MeshKey); result == "ok" && window > 0 &&
			time.Since(at) < window {
			s.loggedIn, s.loggedInAt = true, at
			b.log.Info("restored a room session from the last recorded login",
				"key", r.MeshKey, "at", at)
		}
	}
	b.roomMu.Unlock()

	for _, r := range routes {
		if r.Kind != store.KindRoom || !b.db.HasRoomPassword(r.MeshKey) {
			continue
		}
		if b.roomLoggedIn(r.MeshKey) {
			continue // still good; the keep-alive will refresh it in its own time
		}
		if ctx.Err() != nil || b.link.Session() != sess {
			return
		}
		b.tryRoomLogin(ctx, r.MeshKey)
		// Stagger them; this is airtime, and a dozen rooms logging in at once
		// is a burst everybody on the mesh pays for.
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// refreshRoomSessions logs in to each room server again, in the background and
// without saying anything.
//
// It is not fighting a session timer — there isn't one. It is doing two things
// a room's client table makes worth doing:
//
//   - keeping our entry recently active. The table holds 20 clients and a new
//     login evicts the least recently active non-admin, so a bridge that has
//     been quiet all week is first in line to be thrown out of a busy room.
//   - noticing an eviction that has already happened, and undoing it before
//     somebody types into a room that no longer knows us.
//
// A room that is out of range simply fails to answer, three times, quietly, and
// is tried again at the next interval. Nothing is reported: no message failed,
// because no message was involved.
func (b *Bridge) refreshRoomSessions(ctx context.Context) {
	every := b.cfg.RoomKeepAlive()
	if every == 0 {
		return
	}
	sess := b.link.Session()
	if sess == nil {
		return
	}
	routes, err := b.db.Routes()
	if err != nil {
		return
	}

	for _, r := range routes {
		if r.Kind != store.KindRoom || !b.db.HasRoomPassword(r.MeshKey) {
			continue
		}

		b.roomMu.Lock()
		s := b.roomState(r.MeshKey)
		due := roomRefreshDue(s, every)
		if due {
			// A fresh cycle of attempts. Whatever the last one did, this is a
			// new one, hours later.
			s.attempts = 0
		}
		b.roomMu.Unlock()
		if !due {
			continue
		}

		if ctx.Err() != nil || b.link.Session() != sess {
			return
		}
		b.log.Debug("refreshing a room session", "key", r.MeshKey)
		b.tryRoomLogin(ctx, r.MeshKey)

		// Stagger them, exactly as the reconnect sweep does: this is airtime,
		// and every hop on the mesh repeats it.
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// roomRefreshDue decides whether the keep-alive should log in to a room again.
// Caller must hold roomMu.
//
// Not while a login or a queue is already dealing with the room, and not before
// the interval is up. A room that has not logged in since the bridge started
// has a zero loggedInAt and is due immediately — exactly right, since that is a
// room we are not in.
//
// lastAttempt matters as much as loggedInAt, and leaving it out was a bug worth
// naming: an unreachable room fails its three attempts, gives up, and is left
// with the counter reset and loggedInAt still zero — so it looked due again on
// the very next tick. That is a room out of range being called every few
// minutes, forever, on a medium every repeater on the mesh pays to relay. A
// failed cycle now waits out the full interval, exactly like a successful one.
func roomRefreshDue(s *roomSession, every time.Duration) bool {
	if s.loginInFlight || len(s.pending) > 0 {
		return false
	}
	return time.Since(s.loggedInAt) >= every && time.Since(s.lastAttempt) >= every
}

// forgetRoomSession drops a session we have reason to believe is gone, so the
// next post logs in before sending rather than being discarded.
func (b *Bridge) forgetRoomSession(prefix string) {
	b.roomMu.Lock()
	defer b.roomMu.Unlock()
	if s, ok := b.rooms[prefix]; ok {
		s.loggedIn = false
		s.loggedInAt = time.Time{}
	}
}

// handleLoginResult applies a room's verdict.
//
// Success is quiet when messages were waiting: they simply go, and their own
// markers say what happened. A rejection is final — a wrong password will not
// fix itself on a retry — so it reports immediately and offers the popup.
func (b *Bridge) handleLoginResult(ctx context.Context, r meshcore.LoginResult) {
	// A login the remote-CLI code started belongs to it alone. Letting it fall
	// through would drive the room state machine for a repeater that has no
	// room channel, no queued posts and nothing to announce.
	if b.deliverCLILogin(r) {
		b.log.Info("admin login result", "key", r.Prefix, "ok", r.OK,
			"admin", r.IsAdmin(), "role", meshcore.RoleName(r.Role()))
		return
	}

	b.roomMu.Lock()
	s := b.roomState(r.Prefix)
	s.loggedIn = r.OK
	s.loginInFlight = false
	queued := s.pending
	s.pending = nil
	announce := s.announce
	s.announce = false
	if r.OK {
		s.lastResult, s.attempts, s.loggedInAt = "ok", 0, time.Now()
	} else {
		s.lastResult = "rejected"
	}
	result := s.lastResult
	b.roomMu.Unlock()

	// Log what the room actually granted. "Logged in" and "allowed to post"
	// are not the same thing, and the difference is only visible here.
	b.log.Info("room login result", "key", r.Prefix, "ok", r.OK,
		"role", meshcore.RoleName(r.Role()), "may_post", r.MayPost(),
		"acl", fmt.Sprintf("0x%02X", r.ACL), "fw_level", r.FwLevel,
		"held_messages", len(queued))
	_ = b.db.RecordLogin(r.Prefix, result)

	if !r.OK {
		// Definitive: the room heard us and said no. Retrying the same
		// password achieves nothing.
		b.giveUpOnRoom(ctx, r.Prefix,
			"The room server rejected the stored password.", true)
		return
	}

	// Logged in, but as an account the room will not accept posts from. It
	// says nothing about this: posts are dropped and the acknowledgement is
	// withheld, so without this the only symptom is every message failing for
	// no visible reason.
	if !r.MayPost() {
		b.failRoomQueueGuest(ctx, r, queued)
		return
	}

	if len(queued) > 0 {
		go b.flushRoomQueue(queued)
		return
	}

	// Only confirm a login somebody actually asked for. Sessions are
	// re-established on reconnect and whenever one ages out, so announcing
	// every success turns the channel into a log of routine housekeeping.
	if !announce {
		return
	}
	if route, err := b.db.Route(store.KindRoom, r.Prefix); err == nil {
		b.say(ctx, route.ChannelID,
			"Logged in to this room server. Anything you missed should arrive shortly, over the air.")
	}
}

// expireRoomLogins retries a login that went unanswered, and gives up after
// maxRoomLoginAttempts.
//
// Retries are silent. Only the final failure is worth a message, and it says
// which of the two things went wrong — a password the room refused, or a room
// that never answered at all — because the fixes are completely different.
func (b *Bridge) expireRoomLogins(ctx context.Context) {
	var retry, exhausted, stranded []string

	b.roomMu.Lock()
	for prefix, rs := range b.rooms {
		if !rs.loginInFlight {
			// Held posts with no login working on their behalf. tryRoomLogin
			// refuses to send one within 30 seconds of the last attempt, so a
			// message typed just after a failed login was queued and then left
			// there: nothing in this sweep looked at rooms that were not mid
			// attempt, and the hourglass sat on it until the next message
			// happened to arrive outside the window. Restart the login here —
			// by now the window has passed.
			if len(rs.pending) > 0 {
				if rs.attempts < maxRoomLoginAttempts {
					stranded = append(stranded, prefix)
				} else {
					rs.lastResult = "no answer"
					exhausted = append(exhausted, prefix)
				}
			}
			continue
		}
		if time.Since(rs.loginStartedAt) < roomLoginAttemptTimeout {
			continue
		}
		rs.loginInFlight = false
		if rs.attempts < maxRoomLoginAttempts {
			retry = append(retry, prefix)
			continue
		}
		rs.lastResult = "no answer"
		exhausted = append(exhausted, prefix)
	}
	b.roomMu.Unlock()

	for _, prefix := range stranded {
		b.log.Info("restarting a login for held room posts", "key", prefix)
		b.tryRoomLogin(ctx, prefix)
	}
	for _, prefix := range retry {
		b.log.Info("room login went unanswered; trying again", "key", prefix)
		// lastAttempt is older than the anti-storm window by now, so this goes
		// straight out.
		b.tryRoomLogin(ctx, prefix)
	}
	for _, prefix := range exhausted {
		b.log.Warn("giving up on a room login", "key", prefix, "attempts", maxRoomLoginAttempts)
		_ = b.db.RecordLogin(prefix, "no answer")
		b.giveUpOnRoom(ctx, prefix,
			fmt.Sprintf("Cannot reach the room server right now — it did not answer after %d "+
				"attempts. It is probably out of range or asleep; the password has not been "+
				"forgotten, so try again later.", maxRoomLoginAttempts), false)
	}
}

// giveUpOnRoom reports a final login failure once, and fails anything held.
//
// badPassword decides the advice: a rejected password needs a new one, and a
// room that never answered needs nothing but patience — offering the popup
// there would send people looking for a problem that is not theirs.
//
// Nothing held means nothing failed. Logins also happen on their own, on
// reconnect and on the keep-alive, and a room that is asleep or out of range
// will not answer those — which is normal, not news. Those give up in the log,
// and only a rejected PASSWORD is worth interrupting for, since that one will
// not fix itself; even then, at most once a day, because the keep-alive would
// otherwise rediscover it forever.
func (b *Bridge) giveUpOnRoom(ctx context.Context, prefix, why string, badPassword bool) {
	b.roomMu.Lock()
	rs := b.roomState(prefix)
	queued := rs.pending
	rs.pending = nil
	rs.attempts = 0
	unattended := len(queued) == 0
	if unattended && badPassword {
		if time.Since(rs.lastReported) < 24*time.Hour {
			badPassword = false // already said so today
		} else {
			rs.lastReported = time.Now()
		}
	}
	b.roomMu.Unlock()

	if unattended && !badPassword {
		b.log.Info("a background room login gave up", "key", prefix, "why", why)
		return
	}

	channelID, label := "", prefix
	if route, err := b.db.Route(store.KindRoom, prefix); err == nil {
		channelID, label = route.ChannelID, route.Label
	}
	for _, p := range queued {
		b.reactVerdict(ctx, p.uiChannel, p.uiMessage, EmojiFail)
		if channelID == "" {
			channelID = p.uiChannel
		}
	}
	if channelID == "" {
		b.adminSay(ctx, "**"+label+"**: "+why)
		return
	}

	msg := "**Not sent.** " + why
	switch {
	case unattended:
		// Nobody was sending anything: this is the keep-alive reporting that the
		// stored password no longer works, so "not sent" would be a lie.
		msg = "**" + label + ":** " + why
	case len(queued) > 1:
		msg = fmt.Sprintf("**%d messages not sent.** %s", len(queued), why)
	}

	if badPassword {
		b.sayWithComponents(ctx, channelID, msg,
			[]discord.Component{loginButtonRow(prefix, label)})
		return
	}
	b.say(ctx, channelID, msg)
}

// tryRoomLoginNow sends a login immediately, bypassing the anti-storm delay.
//
// The user has just typed a password, so this is not a retry loop — and it
// starts a fresh cycle, because a new password deserves a full set of attempts
// however badly the previous one went.
func (b *Bridge) tryRoomLoginNow(ctx context.Context, prefix, password string) bool {
	sess := b.link.Session()
	if sess == nil {
		return false
	}
	b.roomMu.Lock()
	s := b.roomState(prefix)
	s.lastAttempt = time.Now()
	s.loginStartedAt = time.Now()
	s.loginInFlight = true
	s.attempts = 1
	s.announce = true // somebody asked for this one
	b.roomMu.Unlock()

	if err := b.sendRoomLogin(ctx, sess, prefix, password); err != nil {
		b.roomMu.Lock()
		b.roomState(prefix).loginInFlight = false
		b.roomMu.Unlock()
		b.log.Warn("the node would not send the login", "key", prefix, "err", err)
		return false
	}
	return true
}

// failRoomQueueGuest explains a login that succeeded but grants no posting.
func (b *Bridge) failRoomQueueGuest(ctx context.Context, r meshcore.LoginResult, queued []queuedPost) {
	channelID, label := "", r.Prefix
	if route, err := b.db.Route(store.KindRoom, r.Prefix); err == nil {
		channelID, label = route.ChannelID, route.Label
	}
	for _, p := range queued {
		b.reactVerdict(ctx, p.uiChannel, p.uiMessage, EmojiFail)
		if channelID == "" {
			channelID = p.uiChannel
		}
	}
	msg := "**Logged in to " + label + ", but only as a " + meshcore.RoleName(r.Role()) + ".**\n" +
		"The room accepted the password but will not accept posts from this account — it discards " +
		"them without saying so. You need the room's posting password, which is a different one " +
		"from any read-only or guest password."
	if channelID == "" {
		b.adminSay(ctx, msg)
		return
	}
	b.sayWithComponents(ctx, channelID, msg, []discord.Component{loginButtonRow(r.Prefix, label)})
}

// flushRoomQueue sends everything that was waiting on a login.
//
// Runs in its own goroutine: transmitting takes the radio for seconds at a
// time, and this is reached from the loop that also services delivery
// confirmations — blocking there would stall acknowledgements for every other
// message in flight.
func (b *Bridge) flushRoomQueue(posts []queuedPost) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sess := b.link.Session()
	if sess == nil {
		// The posts were taken off the queue by the caller, so returning here
		// dropped them without a word: the hourglass stayed on every one of
		// them for good. Say what happened instead.
		b.log.Warn("the link went away before held room posts could be sent", "held", len(posts))
		for _, p := range posts {
			b.reactVerdict(ctx, p.uiChannel, p.uiMessage, EmojiFail)
			b.say(ctx, p.uiChannel, "**Not sent — the link to the node went away while this was "+
				"waiting for the room login.** React "+EmojiRetry+" on it once the radio is back.")
		}
		return
	}

	for _, p := range posts {
		b.log.Info("sending a held room post", "key", p.route.MeshKey)
		b.sendToMesh(ctx, sess, sendRequest{
			Route:      p.route,
			Text:       p.text,
			UIChannel:  p.uiChannel,
			UIMessage:  p.uiMessage,
			Author:     p.author,
			ForceFlood: p.forceFlood,
			Retry:      p.retry,
		})
	}
}

// resolveStrandedSends answers messages that were waiting for an
// acknowledgement when the bridge last stopped.
//
// Nothing can ever resolve them. The acknowledgement handle is issued by the
// node and matched in memory on both sides, so a restart at either end orphans
// whatever was in flight — and the hourglass on those messages would otherwise
// stay there for good, which reads as "still trying" long after anything is.
//
// They are marked failed rather than delivered. The message may well have
// arrived; what is certain is that we cannot say it did, and a cross that
// prompts a resend is a better lie than a tick that ends the matter.
func (b *Bridge) resolveStrandedSends(ctx context.Context) {
	stranded, err := b.db.StrandedSends()
	if err != nil || len(stranded) == 0 {
		return
	}
	b.log.Info("answering messages left waiting by a restart", "count", len(stranded))

	// A split's pieces are answered individually; the original is answered once,
	// after them, so its marker is the last word.
	parents := map[string]string{}
	for _, m := range stranded {
		_ = b.db.SetDelivery(m.ID, store.DeliveryFailed, 0)
		b.setVerdict(ctx, m.ChannelID, m.MessageID, EmojiFail)
		if m.ParentMsgID != "" {
			parents[m.ParentMsgID] = m.ChannelID
		}
	}
	for messageID, channelID := range parents {
		b.setVerdict(ctx, channelID, messageID, EmojiFail)
	}
}
