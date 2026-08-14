// Package bridge is the routing core: mesh in, Discord out, and back again.
//
// The layering is deliberate. Nothing here knows how a serial port works or
// how a websocket frames a heartbeat; it knows about links, chunking, delivery
// receipts and room sessions. That is what makes the protocol logic testable
// without hardware or a network.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"meshycord/internal/config"
	"meshycord/internal/discord"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// Bridge wires the mesh link to the Discord adapter.
type Bridge struct {
	cfg  *config.Store
	db   *store.Store
	rest *discord.Client
	gw   *discord.Gateway
	link *meshcore.Link
	log  *slog.Logger

	// catMu guards the category id cache.
	catMu sync.Mutex
	cats  map[string]string

	// pending holds outbound DMs waiting for a delivery confirmation.
	pendMu  sync.Mutex
	pending map[uint32]*pendingSend
	// failed holds sends whose deadline passed, briefly. The node can still
	// push an ack for one — its send timeout does not clear the expected-ack
	// entry — and a message that actually landed should not keep a cross.
	failed map[uint32]*pendingSend

	// rooms holds room-server session state.
	roomMu sync.Mutex
	rooms  map[string]*roomSession

	// Remote CLI state. Kept apart from the room machinery above because a
	// repeater is not a room: nothing here queues posts, announces itself in a
	// Discord channel, or survives the link dropping. See nodecli.go.
	cliMu      sync.Mutex
	cliWaiters map[string]*cliPending
	cliLogins  map[string]chan meshcore.LoginResult
	cliAdminAt map[string]time.Time

	// snapshots freeze listing numbering, so `add 7` always means the row you
	// saw. Keyed by whoever asked, so two people listing at once do not
	// renumber each other's rows.
	snapMu    sync.Mutex
	snapshots map[string]*listSnapshot
	// lastInboxHint rate-limits the inbox's "this channel is read-only"
	// answer, so casual chatter is not met with a wall of text every line.
	lastInboxHint time.Time

	// txMu serialises transmissions. Two goroutines interleaving chunks would
	// deliver a split message out of order, and the mesh is a shared medium
	// where that is everyone's problem.
	txMu sync.Mutex

	// events carries Discord events off the Gateway's read loop.
	//
	// This is load-bearing, not tidiness. Gateway handlers run on the socket
	// reader, and the work they trigger — creating a channel, transmitting a
	// split message with two-second gaps — takes far longer than the ~41s
	// heartbeat interval allows. Doing it inline stops HEARTBEAT_ACK being
	// read, the connection is declared a zombie, and the bridge reconnects in
	// a loop while apparently working fine.
	//
	// One worker, so messages are still handled in the order they arrived.
	events chan gatewayEvent
	// interactionSlots caps concurrent interaction handling. Interactions get
	// a goroutine each rather than a queue, because Discord gives only three
	// seconds to acknowledge one and a queue behind a slow send would blow
	// that.
	interactionSlots chan struct{}
	// bootstrapping stops two Discord setups overlapping after a reconnect.
	bootstrapping atomic.Bool
	// syncing guards the post-connect channel sync, which both the radio side
	// and the Discord side try to run — whichever comes up second.
	syncing atomic.Bool

	// seen holds recently handled Discord message ids, so a Gateway RESUME
	// that replays events cannot put the same text on the air twice.
	seenMu sync.Mutex
	seen   map[string]struct{}

	ready atomic.Bool
	// announcedOnline keeps "Bridge online" to once per run, rather than once
	// per Gateway reconnect.
	announcedOnline atomic.Bool
	botUserID       atomic.Value // string
	startedAt       time.Time
	lastInbound     atomic.Int64
}

// gatewayEvent is one thing that happened in Discord, queued for the worker.
type gatewayEvent struct {
	message       *discord.Message
	reaction      *discord.ReactionEvent
	channelDelete *discord.Channel
}

// New builds a bridge. Nothing connects until Run.
func New(cfg *config.Store, db *store.Store, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	b := &Bridge{
		cfg:              cfg,
		db:               db,
		log:              log,
		cats:             map[string]string{},
		pending:          map[uint32]*pendingSend{},
		failed:           map[uint32]*pendingSend{},
		rooms:            map[string]*roomSession{},
		cliWaiters:       map[string]*cliPending{},
		cliLogins:        map[string]chan meshcore.LoginResult{},
		cliAdminAt:       map[string]time.Time{},
		snapshots:        map[string]*listSnapshot{},
		events:           make(chan gatewayEvent, 256),
		interactionSlots: make(chan struct{}, 8),
		startedAt:        time.Now(),
	}
	b.rest = discord.NewClient(cfg.BotToken, log)
	b.gw = discord.NewGateway(cfg.BotToken, b.rest, log)

	// Every handler below must return promptly: they run on the Gateway's
	// socket reader. Each one only queues.
	b.gw.Handlers = discord.Handlers{
		Ready:             b.onReady,
		Resumed:           func() { b.log.Info("Discord session resumed") },
		Disconnected:      func(error) { b.ready.Store(false) },
		MessageCreate:     func(m *discord.Message) { b.queue(gatewayEvent{message: m}) },
		ReactionAdd:       func(e *discord.ReactionEvent) { b.queue(gatewayEvent{reaction: e}) },
		ChannelDelete:     func(c *discord.Channel) { b.queue(gatewayEvent{channelDelete: c}) },
		InteractionCreate: b.onInteraction,
	}
	b.link = meshcore.NewLink(b.dialer(), log)
	b.link.OnConnect = b.onMeshConnect
	b.link.OnDisconnect = b.onMeshDisconnect
	return b
}

// REST exposes the Discord client, for the web UI's status page.
func (b *Bridge) REST() *discord.Client { return b.rest }

// Gateway exposes the Gateway client, for status.
func (b *Bridge) Gateway() *discord.Gateway { return b.gw }

// Link exposes the mesh link, for status.
func (b *Bridge) Link() *meshcore.Link { return b.link }

// dialer builds the transport the settings ask for.
func (b *Bridge) dialer() meshcore.Dialer {
	switch b.cfg.Transport() {
	case config.TransportBLE:
		return meshcore.BLEDialer{
			Address: b.cfg.BLEAddress(),
			Name:    b.cfg.BLEName(),
			PIN:     b.cfg.BLEPin(),
			Log:     b.log,
		}
	case config.TransportTCP:
		return meshcore.TCPDialer{Addr: b.cfg.TCPAddress()}
	default:
		return meshcore.SerialDialer{Device: b.cfg.SerialDevice(), Baud: b.cfg.SerialBaud()}
	}
}

// Run holds everything open until ctx is cancelled.
//
// Four independent goroutines, none of which can block another. This is the
// structural reason the ESP32's hang cannot happen here: there, one loop() did
// every operation in sequence, so a slow Discord request stalled the radio and
// a slow radio stalled Discord.
func (b *Bridge) Run(ctx context.Context) {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { defer wg.Done(); b.gw.Run(ctx) }()

	wg.Add(1)
	go func() { defer wg.Done(); b.link.Run(ctx) }()

	wg.Add(1)
	go func() { defer wg.Done(); b.meshLoop(ctx) }()

	wg.Add(1)
	go func() { defer wg.Done(); b.housekeeping(ctx) }()

	wg.Add(1)
	go func() { defer wg.Done(); b.discordLoop(ctx) }()

	// Keeps a negotiated connection to Discord in the pool. An interaction has
	// three seconds to be answered and a TLS handshake costs about one of them
	// on ARMv6, so the handshake must not happen on the click.
	wg.Add(1)
	go func() { defer wg.Done(); b.rest.KeepWarm(ctx) }()

	wg.Wait()
}

// queue hands an event to the worker. Called from the Gateway's read loop, so
// it must never block.
//
// A full queue drops the event rather than stalling the socket. That is the
// right trade: the alternative is the whole Gateway connection backing up
// behind one slow send, which loses far more than a single message.
func (b *Bridge) queue(ev gatewayEvent) {
	select {
	case b.events <- ev:
	default:
		b.log.Warn("dropped a Discord event: the bridge is not keeping up")
		b.db.LogEvent("warn", "discord", "dropped an event under load")
	}
}

// discordLoop handles queued Discord events, one at a time so that messages
// are processed in the order they were sent.
func (b *Bridge) discordLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-b.events:
			switch {
			case ev.message != nil:
				b.onMessageCreate(ctx, ev.message)
			case ev.reaction != nil:
				b.onReactionAdd(ctx, ev.reaction)
			case ev.channelDelete != nil:
				b.onChannelDelete(ctx, ev.channelDelete)
			}
		}
	}
}

// RebuildLink applies changed radio settings without a restart.
//
// Both halves are needed. Closing the session alone made the supervisor redial
// with the ORIGINAL dialer, so switching from serial to BLE in the web UI
// silently did nothing until the process was restarted — while the page
// cheerfully said "reconnecting to the radio".
func (b *Bridge) RebuildLink() {
	b.link.SetDialer(b.dialer())
	if sess := b.link.Session(); sess != nil {
		sess.Close()
	}
}

// ---------------------------------------------------------------------------
// Discord lifecycle
// ---------------------------------------------------------------------------

// onReady runs on the Gateway's read loop, so it only starts the real work.
//
// Bootstrap creates categories and channels, which on a slow link takes longer
// than the heartbeat interval — doing it inline here is what would make the
// connection look like a zombie and reconnect in a loop.
func (b *Bridge) onReady(bot discord.User, appID string) {
	b.botUserID.Store(bot.ID)
	if appID != "" && b.cfg.ApplicationID() != appID {
		_ = b.cfg.SetApplicationID(appID)
	}
	if b.bootstrapping.Swap(true) {
		return // a reconnect arrived while the first setup is still running
	}
	go func() {
		defer b.bootstrapping.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		b.setup(ctx)
	}()
}

func (b *Bridge) setup(ctx context.Context) {
	if err := b.Bootstrap(ctx); err != nil {
		b.log.Error("Discord setup failed", "err", err)
		b.db.LogEvent("error", "discord", "setup failed: "+err.Error())
		return
	}
	b.ready.Store(true)
	b.resolveStrandedSends(ctx)

	// Once per run, not once per Gateway READY. A reconnect is routine — a
	// network blip, a Discord-side restart — and announcing each one turns the
	// admin channel into a connectivity log for something that fixed itself.
	// The web console and `status` both show the link, which is where you look
	// when you actually want to know.
	if !b.announcedOnline.Swap(true) {
		b.adminSay(ctx, "Bridge online. `help` for commands, or use `/mesh`.")
	}

	// Channel names are only known once the node is reachable, so if the mesh
	// link came up first, catch up on the auto-linking now.
	if b.link.Session() != nil {
		b.syncAfterMesh(ctx)
	}
}

func (b *Bridge) onChannelDelete(ctx context.Context, ch *discord.Channel) {
	if ch == nil {
		return
	}
	if gone, _ := b.db.DeleteRouteByChannel(ch.ID); gone {
		b.log.Info("a linked channel was deleted; the link is gone too", "channel", ch.ID)
		b.db.LogEvent("info", "discord", "link removed: #"+ch.Name+" was deleted")
		return
	}
	// The admin or inbox channel being deleted has to be repaired immediately.
	// Leaving the inbox empty means unrouted messages are silently dropped.
	switch ch.ID {
	case b.cfg.AdminChannel():
		b.log.Info("the admin channel was deleted; recreating")
		_ = b.cfg.SetAdminChannel("")
		_ = b.Bootstrap(ctx)
	case b.cfg.InboxChannel():
		b.log.Info("the inbox channel was deleted; recreating")
		_ = b.cfg.SetInboxChannel("")
		_ = b.Bootstrap(ctx)
	}
}

// ---------------------------------------------------------------------------
// Mesh lifecycle
// ---------------------------------------------------------------------------

// onMeshConnect runs on EVERY connection, not just the first.
//
// None of this state survives the link going away: channel names, the contact
// cache and room-server sessions all have to be re-established, which is why
// the ESP32 did the same thing on every reconnect rather than once at boot.
func (b *Bridge) onMeshConnect(ctx context.Context, sess *meshcore.Session) error {
	b.log.Info("mesh link up", "transport", sess.Describe())

	// Channels first. Contact enumeration can stream for tens of seconds, and
	// running it first left the channel replies stuck behind that flood — they
	// arrived late and were discarded.
	if err := sess.RefreshChannels(ctx); err != nil {
		b.log.Warn("could not read the node's channels", "err", err)
	}
	if _, complete, err := sess.RefreshContacts(ctx); err != nil {
		b.log.Warn("could not read the node's contacts", "err", err)
	} else {
		b.syncContacts(sess, complete)
	}
	b.persistContacts(sess)
	b.db.LogEvent("info", "mesh", "connected via "+sess.Describe())
	return nil
}

func (b *Bridge) onMeshDisconnect() {
	b.db.LogEvent("warn", "mesh", "link to the node went down")

	// Room sessions are deliberately NOT forgotten here. A room login is a
	// permission byte in the room's own client table, written to the room's
	// flash — our USB cable coming loose has nothing to do with it, and
	// discarding the knowledge only bought a burst of logins on reconnect.
	// Anything mid-attempt is abandoned, since its answer was coming back over
	// a link that no longer exists.
	b.roomMu.Lock()
	for _, s := range b.rooms {
		s.loginInFlight = false
	}
	b.roomMu.Unlock()

	// Remote-CLI admin sessions are dropped, though the far node's ACL outlives
	// the link just as a room's does. They are not worth the same care: nothing
	// records them across a restart, they are used interactively a few times a
	// week, and the cost of being wrong is a command discarded in silence
	// against the cost of one extra login round trip.
	b.forgetCLIAdmins()
}

// syncContacts mirrors the cache, deleting stragglers ONLY when the caller
// knows the enumeration reached the end of the node's list.
//
// Treating a truncated stream as authoritative deletes contacts that are still
// real, and the symptom shows up much later as some command failing to find
// one — which is exactly how a room server went missing and could no longer be
// logged in to.
func (b *Bridge) syncContacts(sess *meshcore.Session, complete bool) {
	if complete {
		b.persistContactsAuthoritative(sess)
		return
	}
	b.persistContacts(sess)
}

// persistContacts mirrors the node's contacts into the database, so the web UI
// still has names when the radio is unplugged. Never removes anything.
func (b *Bridge) persistContacts(sess *meshcore.Session) {
	if err := b.db.UpsertContacts(contactRows(sess)); err != nil {
		b.log.Warn("could not save the contact cache", "err", err)
	}
}

// contactRows converts the live cache into database rows.
func contactRows(sess *meshcore.Session) []store.Contact {
	cs := sess.Contacts()
	out := make([]store.Contact, 0, len(cs))
	for _, c := range cs {
		out = append(out, store.Contact{
			PubKey:     c.PubKeyHex(),
			Prefix:     c.Prefix(),
			Type:       int(c.Type),
			Name:       c.Name,
			OutPathLen: int(c.OutPathLen),
			LastAdvert: c.LastAdvert,
			Lat:        c.Lat,
			Lon:        c.Lon,
		})
	}
	return out
}

// persistContactsAuthoritative mirrors the cache AND drops anything absent
// from it. Only correct after a complete enumeration.
func (b *Bridge) persistContactsAuthoritative(sess *meshcore.Session) {
	if err := b.db.ReplaceContacts(contactRows(sess)); err != nil {
		b.log.Warn("could not save the contact cache", "err", err)
	}
}

// meshLoop drives everything that comes off the radio.
func (b *Bridge) meshLoop(ctx context.Context) {
	// A slow tick catches anything the push may have missed — a MSG_WAITING
	// that arrived while the link was down, say. The node keeps the flag set
	// until it answers "no more", so an extra poll is cheap and never
	// duplicates.
	sweep := time.NewTicker(30 * time.Second)
	defer sweep.Stop()

	for {
		sess := b.link.Session()
		if sess == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		// The link supervisor publishes nil slightly after a session dies, so
		// a dead one can still be visible here for a moment. Without this
		// check the loop would spin through a doomed drain until the
		// supervisor caught up.
		select {
		case <-sess.Done():
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		default:
		}

		// A fresh session: drain whatever queued up while we were away,
		// re-establish every room session, and link any mesh channels that
		// appeared while the radio was gone.
		//
		// All three run on every reconnect, not just the first: none of this
		// state survives the link going away.
		b.drainMesh(ctx, sess)
		go b.loginAllRooms(ctx, sess)
		go b.syncAfterMesh(ctx)

		for sess == b.link.Session() {
			select {
			case <-ctx.Done():
				return
			case <-sess.Done():
			case <-sess.MsgWaiting():
				b.drainMesh(ctx, sess)
				continue
			case c := <-sess.Confirmations():
				b.handleConfirmation(ctx, c)
				continue
			case r := <-sess.LoginResults():
				b.handleLoginResult(ctx, r)
				continue
			case <-sess.Adverts():
				continue
			case <-sweep.C:
				b.drainMesh(ctx, sess)
				b.expirePending(ctx)
				continue
			}
			break
		}
	}
}

// drainMesh pulls every queued message off the node and relays it.
func (b *Bridge) drainMesh(ctx context.Context, sess *meshcore.Session) {
	for i := 0; i < 256; i++ { // a runaway guard, not a budget
		if ctx.Err() != nil {
			return
		}
		m, got, err := sess.NextMessage(ctx)
		if err != nil {
			if !errors.Is(err, meshcore.ErrLinkDown) {
				b.log.Debug("could not read the next message", "err", err)
			}
			return
		}
		if !got {
			return
		}
		b.relayInbound(ctx, sess, m)
	}
	b.log.Warn("stopped draining after 256 messages; will continue on the next pass")
}

// relayInbound routes one mesh message into Discord and records it.
func (b *Bridge) relayInbound(ctx context.Context, sess *meshcore.Session, m meshcore.Message) {
	b.lastInbound.Store(time.Now().Unix())

	// The output of a remote CLI command comes back as an ordinary inbound
	// message, distinguished only by its text type. Hand it to whoever is
	// waiting for it rather than posting a repeater's console output into a
	// chat channel.
	//
	// If nobody is waiting it still must not be relayed as chat: an unsolicited
	// one means a command timed out and its answer arrived late, or somebody
	// ran a command from another client. Log it and stop.
	if !m.IsChannel && m.TxtType == meshcore.TxtTypeCLIData {
		if b.deliverCLIReply(m.PubKeyPrefix, m.Text) {
			return
		}
		b.log.Info("a CLI reply arrived with nobody waiting for it",
			"from", m.PubKeyPrefix, "text", meshcore.TruncateUTF8(m.Text, 120))
		b.db.LogEvent("info", "cli", "late CLI reply from "+m.PubKeyPrefix+": "+
			meshcore.TruncateUTF8(m.Text, 200))
		return
	}

	dest, kind, key, label := b.destinationFor(ctx, sess, m)

	// Would turn the command channel into a chat feed and echo commands back
	// onto the mesh. This should be impossible; refuse loudly if it happens.
	if dest != "" && dest == b.cfg.AdminChannel() {
		b.log.Error("refusing to post mesh traffic into the admin channel")
		dest = b.cfg.InboxChannel()
	}
	if dest == "" {
		b.log.Warn("no destination for an inbound message; dropping", "channel_msg", m.IsChannel)
		return
	}

	author := ""
	if m.AuthorPrefix != "" {
		author = m.AuthorPrefix
		if c, ok := sess.LookupContact(m.AuthorPrefix); ok && c.Name != "" {
			author = c.Name
		}
	}

	text := FormatInbound(m, label)
	if body := FormatInboundBody(m, author); body != "" {
		text += body
	}

	rec := store.Message{
		Direction: "in",
		Kind:      kind,
		MeshKey:   key,
		PeerLabel: label,
		Author:    author,
		Body:      m.Text,
		ChannelID: dest,
		HaveHops:  m.HaveHops,
		Hops:      int(m.Hops),
		PathRaw:   int(m.PathRaw),
		HaveSNR:   m.HaveSNR,
		SNR:       m.SNR,
		Delivery:  store.DeliveryReceived,
		CreatedAt: time.Now(),
	}
	id, err := b.db.InsertMessage(rec)
	if err != nil {
		b.log.Warn("could not record an inbound message", "err", err)
	}

	sent, err := b.rest.SendMessage(ctx, dest, clampReply(text))
	if err != nil {
		if discord.IsNotFound(err) {
			b.handleDeadChannel(dest)
			return
		}
		b.log.Warn("could not relay a mesh message to Discord", "err", err)
		return
	}
	if id > 0 && sent != nil {
		_ = b.db.SetDiscordMessageID(id, dest, sent.ID)
	}
	if r, err := b.db.RouteByChannel(dest); err == nil {
		_ = b.db.TouchRoute(r.ID)
	}
	b.log.Info("mesh -> discord", "from", label, "channel", dest, "bytes", len(m.Text))
}

// handleDeadChannel reacts to Discord reporting that a channel we just posted
// to does not exist.
//
// CHANNEL_DELETE over the Gateway is the normal way this is noticed, and this
// is the backstop for when that event was missed — the bridge was offline when
// the channel was deleted, or the event was dropped under load.
//
// The inbox case matters most and is why this exists at all: with no inbox,
// destinationFor has nowhere to send traffic from unlinked senders and every
// one of those messages is silently dropped. Rebuild it immediately rather
// than waiting for a restart.
func (b *Bridge) handleDeadChannel(channelID string) {
	if gone, _ := b.db.DeleteRouteByChannel(channelID); gone {
		b.log.Info("destination channel is gone; removed the link", "channel", channelID)
		return
	}
	if channelID != b.cfg.InboxChannel() && channelID != b.cfg.AdminChannel() {
		return
	}
	which := "inbox"
	if channelID == b.cfg.AdminChannel() {
		which = "admin"
		_ = b.cfg.SetAdminChannel("")
	} else {
		_ = b.cfg.SetInboxChannel("")
	}
	b.log.Info("the "+which+" channel is gone; rebuilding", "channel", channelID)

	// In the background: this runs on the mesh drain loop, and rebuilding
	// involves several Discord round trips that must not stall the radio.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := b.Bootstrap(ctx); err != nil {
			b.log.Error("could not rebuild after a channel was deleted", "err", err)
		}
	}()
}

// destinationFor decides where an inbound message goes.
//
// Channels and room servers auto-create when you have opted into those. A DM
// from a person goes to the inbox unless you opted in AND they are already a
// contact — that is what stops a stranger creating Discord channels at will,
// and it is the reason unknown senders ALWAYS go to the inbox: an unknown
// sender cannot be classified or named.
func (b *Bridge) destinationFor(ctx context.Context, sess *meshcore.Session, m meshcore.Message) (channelID string, kind store.RouteKind, key, label string) {
	if m.IsChannel {
		key = fmt.Sprintf("%d", m.ChannelIdx)
		kind = store.KindChannel
		if r, err := b.db.Route(store.KindChannel, key); err == nil {
			return r.ChannelID, kind, key, r.Label
		}
		if !b.cfg.AutoCreateChannels() {
			return b.cfg.InboxChannel(), kind, key, "channel " + key
		}
		// Ask the node for the channel's real name ("Public" for slot 0)
		// rather than naming it after the index.
		name := "channel-" + key
		if ci, ok := sess.Channel(m.ChannelIdx); ok && ci.Name != "" {
			name = ci.Name
		}
		ch, err := b.createLinkedChannel(ctx, store.KindChannel, name,
			"MeshCore channel "+key+" ("+name+")", "mesh-channel-"+key)
		if err != nil {
			b.log.Warn("could not auto-create a channel", "slot", key, "err", err)
			return b.cfg.InboxChannel(), kind, key, name
		}
		if _, err := b.db.PutRoute(store.KindChannel, key, ch.ID, name); err != nil {
			b.log.Warn("could not save an auto-created link", "err", err)
		}
		return ch.ID, kind, key, name
	}

	// A contact message: a person or a room server.
	key = m.PubKeyPrefix
	if r, err := b.db.RouteByPrefix(key); err == nil {
		// Refresh the stored label: it was captured when the link was created
		// and goes stale if the contact renames itself.
		if c, ok := sess.LookupContact(key); ok && c.Name != "" && c.Name != r.Label {
			_ = b.db.UpdateRouteLabel(r.Kind, key, c.Name)
			r.Label = c.Name
		}
		return r.ChannelID, r.Kind, key, r.Label
	}

	c, known := sess.LookupOrRefresh(ctx, key)
	if known {
		label = c.Name
		b.persistContacts(sess)
	}
	if label == "" {
		label = key
	}

	switch {
	case known && c.Type == meshcore.AdvTypeRoom && b.cfg.AutoCreateRooms():
		kind = store.KindRoom
		ch, err := b.createLinkedChannel(ctx, kind, label, "MeshCore room server "+key, "node-"+key[:6])
		if err == nil {
			_, _ = b.db.PutRoute(kind, key, ch.ID, c.Name)
			return ch.ID, kind, key, label
		}
		b.log.Warn("could not auto-create a room channel", "key", key, "err", err)

	case known && c.Type == meshcore.AdvTypeChat && b.cfg.AutoCreateDMs():
		kind = store.KindDM
		ch, err := b.createLinkedChannel(ctx, kind, label, "MeshCore DM "+key, "node-"+key[:6])
		if err == nil {
			_, _ = b.db.PutRoute(kind, key, ch.ID, c.Name)
			return ch.ID, kind, key, label
		}
		b.log.Warn("could not auto-create a DM channel", "key", key, "err", err)
	}

	// The inbox: an unknown sender, or a person with auto-create off.
	kind = store.KindDM
	if known && c.Type == meshcore.AdvTypeRoom {
		kind = store.KindRoom
	}
	return b.cfg.InboxChannel(), kind, key, label
}

// ---------------------------------------------------------------------------
// Housekeeping
// ---------------------------------------------------------------------------

func (b *Bridge) housekeeping(ctx context.Context) {
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	expire := time.NewTicker(5 * time.Second)
	defer expire.Stop()
	contacts := time.NewTicker(15 * time.Minute)
	defer contacts.Stop()
	// Room keep-alive. Checked every minute, acted on only when a room is
	// actually due — the interval itself is hours, and lives in settings.
	rooms := time.NewTicker(time.Minute)
	defer rooms.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-expire.C:
			b.expirePending(ctx)
			b.expireRoomLogins(ctx)
		case <-rooms.C:
			b.refreshRoomSessions(ctx)
		case <-contacts.C:
			if sess := b.link.Session(); sess != nil {
				if _, complete, err := sess.RefreshContacts(ctx); err == nil {
					b.syncContacts(sess, complete)
				}
			}
		case <-prune.C:
			if n, err := b.db.Prune(b.cfg.Retention()); err != nil {
				b.log.Warn("could not prune history", "err", err)
			} else if n > 0 {
				b.log.Info("pruned old history", "messages", n)
			}
		}
	}
}

// Status is the whole bridge in one struct, for the web UI and `status`.
type Status struct {
	Mesh            meshcore.Status
	DiscordUp       bool
	DiscordSince    time.Time
	DiscordError    string
	DiscordFatal    string
	AuthFailed      bool
	Ready           bool
	Uptime          time.Duration
	Links           int
	RoomsLoggedIn   int
	RoomsKnown      int
	PendingDelivery int
	LastInbound     time.Time
	Stats           store.Stats
}

// Status snapshots everything worth reporting.
func (b *Bridge) Status(dbPath string) Status {
	st := Status{
		Mesh:         b.link.Status(),
		DiscordUp:    b.gw.Connected(),
		DiscordSince: b.gw.UpSince(),
		DiscordError: b.gw.LastError(),
		DiscordFatal: b.gw.FatalError(),
		AuthFailed:   b.rest.AuthFailed(),
		Ready:        b.ready.Load(),
		Uptime:       time.Since(b.startedAt),
		Stats:        b.db.Stats(dbPath),
	}
	if routes, err := b.db.Routes(); err == nil {
		st.Links = len(routes)
	}
	b.roomMu.Lock()
	st.RoomsKnown = len(b.rooms)
	for _, s := range b.rooms {
		if s.loggedIn {
			st.RoomsLoggedIn++
		}
	}
	b.roomMu.Unlock()
	b.pendMu.Lock()
	st.PendingDelivery = len(b.pending)
	b.pendMu.Unlock()
	if ts := b.lastInbound.Load(); ts > 0 {
		st.LastInbound = time.Unix(ts, 0)
	}
	return st
}

// RoomLoggedIn reports whether the bridge currently holds a session with a
// room server. Used by the console to show why a post might be refused.
func (b *Bridge) RoomLoggedIn(prefix string) bool { return b.roomLoggedIn(prefix) }

// SyncContacts mirrors the radio's contact list into the database. Called by
// the console after it changes something on the node, so the page it redirects
// to shows the new state rather than the old one.
func (b *Bridge) SyncContacts(ctx context.Context) {
	if sess := b.link.Session(); sess != nil {
		b.persistContacts(sess)
	}
}

// isOwnBot reports whether a Discord user is us.
func (b *Bridge) isOwnBot(id string) bool {
	s, _ := b.botUserID.Load().(string)
	return s != "" && s == id
}

// mentionChannel renders a channel link, or a plain name when there is no id.
func mentionChannel(id string) string {
	if id == "" {
		return "the admin channel"
	}
	return "<#" + id + ">"
}

// trimLower is the common "normalise a typed command" step.
func trimLower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
