package meshcore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors a caller may want to distinguish.
var (
	// ErrTimeout means the node did not answer a command in time. The docs
	// suggest 5 seconds per command; the node is single-threaded behind a
	// radio, so this is normal under load rather than a fault.
	ErrTimeout = errors.New("meshcore: the node did not answer in time")
	// ErrLinkDown means the transport went away mid-command.
	ErrLinkDown = errors.New("meshcore: the link to the node is down")
	// ErrRejected means the node answered, and said no.
	ErrRejected = errors.New("meshcore: the node rejected the command")
	// ErrNotContact means a command needed a full public key and the contact
	// is not in the node's list, so the prefix could not be resolved.
	ErrNotContact = errors.New("meshcore: not a known contact on the node")
)

// DefaultCommandTimeout is the per-command ceiling suggested by the MeshCore
// docs. Commands are strictly sequenced — never pipelined — so this bounds how
// long any one operation can hold the link.
const DefaultCommandTimeout = 5 * time.Second

// AppName identifies the bridge to the node in the CmdAppStart handshake.
const AppName = "meshycord"

// Session owns one live link to a companion node.
//
// The ESP32 version was a single loop() where every mesh operation blocked
// every other operation, which is precisely why it watchdog-rebooted. Here the
// read path is a goroutine that never blocks on anything slow, commands are
// serialised through one mutex, and asynchronous pushes go out on channels
// that drop rather than block. A stuck Discord request cannot stall the radio
// and a slow radio cannot stall Discord.
type Session struct {
	tr  Transport
	log *slog.Logger

	// replies carries non-push frames from the read loop to whichever command
	// is currently outstanding. Buffered because a contact enumeration streams
	// one frame per contact back to back — the ESP32's queue of 8 silently
	// overflowed and dropped contacts, so senders could not be classified.
	replies chan []byte

	// cmdMu serialises commands. The protocol requires one at a time: the node
	// has a single response path and pipelining scrambles which reply belongs
	// to which request.
	cmdMu sync.Mutex

	done     chan struct{}
	doneOnce sync.Once
	closeErr error

	// --- asynchronous pushes ---------------------------------------------
	//
	// Every one of these is delivered on a buffered channel with a
	// drop-when-full send. A subscriber that stops reading must never be able
	// to block the read loop and therefore the whole link.
	msgWaiting    chan struct{}
	confirmations chan Confirmation
	loginResults  chan LoginResult
	adverts       chan struct{}

	// --- caches ------------------------------------------------------------
	mu          sync.RWMutex
	self        SelfInfo
	device      DeviceInfo
	contacts    map[string]Contact // by 12-char prefix
	contactsAt  time.Time
	channels    [MaxChannels]ChannelInfo
	channelsOK  [MaxChannels]bool
	connectedAt time.Time

	// enumIdle is the per-frame patience during a contact enumeration.
	// Overridden by tests so the suite does not sit out a real timeout.
	enumIdle time.Duration
}

// EnumerationIdleTimeout is how long to wait between contact frames before
// concluding the node has stopped answering.
//
// Generous: the node interleaves this stream with actual radio work, so a
// pause of several seconds mid-enumeration is normal rather than a fault. At
// 10s a busy mesh truncated the list, which then looked like contacts had been
// deleted.
const EnumerationIdleTimeout = 25 * time.Second

// NewSession runs the handshake and returns a live session. The caller owns
// closing it.
func NewSession(ctx context.Context, tr Transport, log *slog.Logger) (*Session, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Session{
		tr:            tr,
		log:           log.With("link", tr.Describe()),
		replies:       make(chan []byte, 64),
		done:          make(chan struct{}),
		msgWaiting:    make(chan struct{}, 1),
		confirmations: make(chan Confirmation, 32),
		loginResults:  make(chan LoginResult, 16),
		adverts:       make(chan struct{}, 1),
		contacts:      make(map[string]Contact),
		connectedAt:   time.Now(),
		enumIdle:      EnumerationIdleTimeout,
	}
	go s.readLoop()

	if err := s.handshake(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Describe names the underlying link.
func (s *Session) Describe() string { return s.tr.Describe() }

// ConnectedAt is when this session came up, for the status page.
func (s *Session) ConnectedAt() time.Time { return s.connectedAt }

// Done is closed when the link dies. Everything above should watch this rather
// than trying to detect a dead radio for itself.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close tears the session down. Safe to call more than once.
func (s *Session) Close() error {
	s.doneOnce.Do(func() {
		close(s.done)
		s.closeErr = s.tr.Close()
	})
	return s.closeErr
}

// --- push subscriptions ----------------------------------------------------

// MsgWaiting fires when the node says it has queued messages. It is a
// coalescing signal, not a count: the flag stays set on the node until it
// answers "no more", so one wake-up is enough to drain a whole backlog.
func (s *Session) MsgWaiting() <-chan struct{} { return s.msgWaiting }

// Confirmations delivers PushSendConfirmed: a DM we sent was acknowledged.
func (s *Session) Confirmations() <-chan Confirmation { return s.confirmations }

// LoginResults delivers the verdict on a room-server login.
//
// This is the push that catches people out. CmdSendLogin replies RespSent,
// which means only "the request went out over the air" — treating that as
// success reports every login as working. The real answer arrives here,
// seconds later.
func (s *Session) LoginResults() <-chan LoginResult { return s.loginResults }

// Adverts fires when a contact is heard. A good moment to refresh the cache,
// and nothing more than that.
func (s *Session) Adverts() <-chan struct{} { return s.adverts }

// ---------------------------------------------------------------------------
// Read loop
// ---------------------------------------------------------------------------

func (s *Session) readLoop() {
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()

	for {
		f, err := s.tr.ReadFrame(ctx)
		if err != nil {
			select {
			case <-s.done:
			default:
				s.log.Info("link closed", "err", err)
			}
			return
		}
		if len(f) == 0 {
			continue
		}

		// THE rule of this protocol: anything with a first byte >= 0x80 is an
		// asynchronous push and is never a reply to anything. Letting pushes
		// into the response queue cost the ESP32 three separate bugs, each
		// presenting as something unrelated being subtly wrong.
		if IsPush(f[0]) {
			s.handlePush(f)
			continue
		}

		select {
		case s.replies <- f:
		default:
			// No command is waiting, or the buffer is full. Either way this
			// frame is stale; dropping it is better than letting it satisfy
			// some later command.
			s.log.Debug("dropped an unsolicited reply frame", "code", fmt.Sprintf("0x%02X", f[0]))
		}
	}
}

func (s *Session) handlePush(f []byte) {
	switch f[0] {
	case PushMsgWaiting:
		select {
		case s.msgWaiting <- struct{}{}:
		default: // already signalled; one is enough
		}

	case PushSendConfirmed:
		c, err := DecodeConfirmation(f)
		if err != nil {
			s.log.Debug("malformed send confirmation", "err", err)
			return
		}
		select {
		case s.confirmations <- c:
		default:
			s.log.Warn("dropped a delivery confirmation: nothing is reading them")
		}

	case PushLoginSuccess, PushLoginFail:
		r, err := DecodeLoginResult(f)
		if err != nil {
			s.log.Debug("malformed login result", "err", err)
			return
		}
		select {
		case s.loginResults <- r:
		default:
			s.log.Warn("dropped a room login result")
		}

	case PushAdvert, PushNewAdvert, PushPathUpdated:
		select {
		case s.adverts <- struct{}{}:
		default:
		}

	case PushContactsFull:
		s.log.Warn("the node's contact list is FULL; it will stop learning new contacts")

	default:
		s.log.Debug("ignoring push", "code", fmt.Sprintf("0x%02X", f[0]))
	}
}

// ---------------------------------------------------------------------------
// Command plumbing
// ---------------------------------------------------------------------------

// collector decides what a command does with each non-push frame it receives.
// Returning done ends the command; returning an error aborts it.
type collector func(frame []byte) (done bool, err error)

// do writes one command and feeds replies to collect until it says it is
// finished, the idle timeout expires, or the link dies.
//
// The timeout is per frame, not for the whole exchange: a contact enumeration
// streams for tens of seconds on a busy mesh, and a single overall deadline
// either cut it short or had to be set so long that a genuinely wedged
// command took forever to notice.
func (s *Session) do(ctx context.Context, payload []byte, idle time.Duration, collect collector) error {
	if idle <= 0 {
		idle = DefaultCommandTimeout
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	select {
	case <-s.done:
		return ErrLinkDown
	default:
	}

	// Discard anything left over from a previous command that timed out. Its
	// late reply arriving now would be mistaken for this command's answer —
	// exactly how a stale channel query gave the wrong slot the wrong name.
	s.drainReplies()

	if err := s.tr.WriteFrame(ctx, payload); err != nil {
		return fmt.Errorf("write command: %w", err)
	}

	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case f := <-s.replies:
			done, err := collect(f)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			return ErrTimeout
		case <-s.done:
			return ErrLinkDown
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Session) drainReplies() {
	for {
		select {
		case <-s.replies:
		default:
			return
		}
	}
}

// expect is the common shape: one command, one reply of a known code.
// Frames with any other code are skipped, not accepted — the caller's matcher
// is what stops an unrelated late reply being read as this one's answer.
func (s *Session) expect(ctx context.Context, payload []byte, want byte, timeout time.Duration) ([]byte, error) {
	var got []byte
	err := s.do(ctx, payload, timeout, func(f []byte) (bool, error) {
		if f[0] == want {
			got = f
			return true, nil
		}
		if f[0] == RespError {
			return true, ErrRejected
		}
		s.log.Debug("skipping unexpected reply", "got", fmt.Sprintf("0x%02X", f[0]), "want", fmt.Sprintf("0x%02X", want))
		return false, nil
	})
	return got, err
}

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

func (s *Session) handshake(ctx context.Context) error {
	f, err := s.expect(ctx, EncodeAppStart(AppName), RespSelfInfo, 8*time.Second)
	if err != nil {
		return fmt.Errorf("handshake (CMD_APP_START): %w", err)
	}
	self, err := DecodeSelfInfo(f)
	if err != nil {
		s.log.Warn("could not parse the node's self-info", "err", err)
	} else {
		s.mu.Lock()
		s.self = self
		s.mu.Unlock()
		s.log.Info("connected to node", "name", self.Name, "key", self.PubKeyHex()[:12])
	}

	// Best-effort extras. Neither is required to bridge messages, so a node
	// that does not answer them is not a failure.
	if d, err := s.expect(ctx, EncodeDeviceQuery(), RespDeviceInfo, 3*time.Second); err == nil {
		if di, err := DecodeDeviceInfo(d); err == nil {
			s.mu.Lock()
			s.device = di
			s.mu.Unlock()
		}
	}
	// A node with no RTC boots at the epoch, which makes every message
	// timestamp meaningless. The bridge has a real clock; hand it over.
	if err := s.do(ctx, EncodeSetDeviceTime(time.Now()), 3*time.Second, anyReply); err != nil {
		s.log.Debug("could not set the node's clock", "err", err)
	}
	return nil
}

// anyReply accepts whatever comes back. Used for commands where the reply
// carries nothing we act on.
func anyReply(f []byte) (bool, error) {
	if f[0] == RespError {
		return true, ErrRejected
	}
	return true, nil
}

// MaxChannelTextLen is how much text actually fits in a group message.
//
// The node prepends its own name — "<name>: <text>" — and then silently
// truncates the whole thing to MAX_TEXT_LEN. Sending the full ceiling to a
// channel therefore loses the tail of every message with no error anywhere.
func (s *Session) MaxChannelTextLen() int {
	name := s.SelfInfo().Name
	limit := MaxMsgLen - len(name) - ChannelNamePrefixOverhead
	if limit < 32 {
		// A pathological name should not make sending impossible.
		limit = 32
	}
	return limit
}

// SelfInfo returns the node's own identity, as learned at handshake.
func (s *Session) SelfInfo() SelfInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.self
}

// DeviceInfo returns the node's firmware details.
func (s *Session) DeviceInfo() DeviceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.device
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// syncReplies are the only frames CmdSyncNextMessage can legitimately answer
// with. Accepting anything would let an unrelated reply be mis-parsed as a
// message.
func isSyncReply(t byte) bool {
	switch t {
	case RespNoMoreMessages, RespContactMsgRecv, RespContactMsgRecvV3,
		RespChannelMsgRecv, RespChannelMsgRecvV3, RespError:
		return true
	}
	return false
}

// NextMessage pulls one queued message off the node.
//
// The second return is false when the queue is empty, which is the normal way
// a drain ends.
func (s *Session) NextMessage(ctx context.Context) (Message, bool, error) {
	var (
		msg   Message
		empty bool
	)
	err := s.do(ctx, EncodeSyncNextMessage(), DefaultCommandTimeout, func(f []byte) (bool, error) {
		if !isSyncReply(f[0]) {
			s.log.Debug("skipping non-message reply during sync", "code", fmt.Sprintf("0x%02X", f[0]))
			return false, nil
		}
		switch f[0] {
		case RespNoMoreMessages:
			empty = true
			return true, nil
		case RespError:
			return true, ErrRejected
		}
		m, err := DecodeMessage(f)
		if err != nil {
			return true, err
		}
		msg = m
		return true, nil
	})
	if err != nil {
		return Message{}, false, err
	}
	return msg, !empty, nil
}

// SendDM sends a direct message.
//
// The returned SendResult carries the expected-ack handle: a later
// PushSendConfirmed with that handle is what actually proves delivery.
func (s *Session) SendDM(ctx context.Context, prefixHex, text string) (SendResult, error) {
	prefix, err := ParsePrefix(prefixHex)
	if err != nil {
		return SendResult{}, err
	}
	payload, err := EncodeSendTxtMsg(prefix, text, time.Now())
	if err != nil {
		return SendResult{}, err
	}
	f, err := s.expect(ctx, payload, RespSent, DefaultCommandTimeout)
	if err != nil {
		return SendResult{}, err
	}
	return DecodeSendResult(f)
}

// SendChannel sends a group message to one of the node's channel slots.
//
// There is no acknowledgement to be had: MeshCore cannot acknowledge group
// messages, so the node answers with a plain OK and delivery is never
// confirmable for these.
func (s *Session) SendChannel(ctx context.Context, idx byte, text string) error {
	_, err := s.expect(ctx, EncodeSendChannelTxtMsg(idx, text, time.Now()), RespOK, DefaultCommandTimeout)
	return err
}

// ---------------------------------------------------------------------------
// Contacts
// ---------------------------------------------------------------------------

// RefreshContacts re-enumerates the node's contacts into the cache.
//
// Unlike the ESP32 version the cache is uncapped and keeps every contact type.
// That limit was RAM, and repeaters were dropped to make room for people; a Pi
// has no such problem, and holding the whole list means `contact find` is
// instant instead of costing a 30-second enumeration.
// The second return says whether the node reached the end of its list. A false
// there means the answer is a subset, not the truth, and callers must not
// treat anything missing from it as gone.
func (s *Session) RefreshContacts(ctx context.Context) (int, bool, error) {
	started := time.Now()
	list, complete, err := s.enumerateContacts(ctx)
	if err != nil && len(list) == 0 {
		return 0, false, err
	}

	s.mu.Lock()
	if complete {
		// The node reached END_OF_CONTACTS, so this really is the whole list
		// and anything missing has genuinely been removed.
		m := make(map[string]Contact, len(list))
		for _, c := range list {
			m[c.Prefix()] = c
		}
		s.contacts = m
	} else {
		// Partial: merge. Dropping what did not arrive would forget contacts
		// that are still on the node, and the only symptom is some later
		// command mysteriously failing to find one.
		for _, c := range list {
			s.contacts[c.Prefix()] = c
		}
	}
	s.contactsAt = time.Now()
	m := s.contacts
	s.mu.Unlock()
	// The duration is logged because this is the single slowest thing the
	// bridge asks of the radio, and the number is what tells you whether a
	// command that felt slow was actually doing an enumeration it did not need.
	s.log.Info("contacts refreshed", "count", len(m), "complete", complete,
		"took", time.Since(started).Round(time.Millisecond))
	return len(m), complete, nil
}

// enumerateContacts runs CmdGetContacts and collects the stream.
//
// Replies are RespContactsStart, then one RespContact per contact, then
// RespEndOfContacts. On a 350-node mesh that is several seconds of frames
// arriving back to back.
func (s *Session) enumerateContacts(ctx context.Context) ([]Contact, bool, error) {
	var (
		out      []Contact
		started  bool
		complete bool
	)
	err := s.do(ctx, EncodeGetContacts(time.Time{}), s.enumIdle, func(f []byte) (bool, error) {
		switch f[0] {
		case RespContactsStart:
			started = true
			return false, nil
		case RespContact:
			c, err := DecodeContact(f)
			if err != nil {
				s.log.Debug("skipping malformed contact record", "err", err)
				return false, nil
			}
			out = append(out, c)
			return false, nil
		case RespEndOfContacts:
			complete = true
			return true, nil
		case RespError:
			if !started {
				return true, ErrRejected
			}
			return true, nil
		default:
			return false, nil
		}
	})
	if err != nil && len(out) == 0 {
		return nil, false, err
	}
	// A timeout after some contacts arrived is worth keeping: a partial list
	// still classifies most senders, and the alternative is none at all. What
	// it must NOT do is look authoritative — see RefreshContacts.
	if err != nil {
		s.log.Warn("contact enumeration ended early; merging what arrived rather than treating it "+
			"as the whole list", "count", len(out), "err", err)
	}
	return out, complete, nil
}

// Contacts returns a snapshot of the cache, sorted by most recently heard.
func (s *Session) Contacts() []Contact {
	s.mu.RLock()
	out := make([]Contact, 0, len(s.contacts))
	for _, c := range s.contacts {
		out = append(out, c)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastAdvert.After(out[j].LastAdvert) })
	return out
}

// ContactCount is the number of cached contacts.
func (s *Session) ContactCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.contacts)
}

// LookupContact resolves a key prefix against the cache.
//
// Note what this cannot be: CmdGetContactByKey requires the FULL 32-byte key
// (MyMesh.cpp:1322 calls lookupContactByPubKey with PUB_KEY_SIZE) and a
// message only carries 6 bytes. So prefixes are resolved against a cache built
// by enumeration. The node remains the source of truth; this is an index.
//
// An 8-character prefix is accepted too — that is the 4-byte author prefix a
// room-server post carries.
func (s *Session) LookupContact(prefixHex string) (Contact, bool) {
	want := strings.ToLower(strings.TrimSpace(prefixHex))
	if len(want) < 8 {
		return Contact{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.contacts[want]; ok {
		return c, true
	}
	for p, c := range s.contacts {
		if strings.HasPrefix(p, want) {
			return c, true
		}
	}
	return Contact{}, false
}

// LookupOrRefresh resolves a prefix, re-enumerating once on a miss. The
// refresh is rate limited: a mesh full of strangers would otherwise trigger a
// full enumeration per message.
func (s *Session) LookupOrRefresh(ctx context.Context, prefixHex string) (Contact, bool) {
	if c, ok := s.LookupContact(prefixHex); ok {
		return c, true
	}
	s.mu.RLock()
	age := time.Since(s.contactsAt)
	s.mu.RUnlock()
	if age < 30*time.Second {
		return Contact{}, false
	}
	if _, _, err := s.RefreshContacts(ctx); err != nil {
		return Contact{}, false
	}
	return s.LookupContact(prefixHex)
}

// FindContacts searches the node's ENTIRE contact list.
//
// On the ESP32 this had to stream a live enumeration because the cache
// deliberately held only companions and rooms, hiding some 255 repeaters and
// sensors — which are exactly what you are looking for when clearing out
// clutter. Here the cache holds everything, so this is a filter over memory
// and returns instantly. An empty needle matches everything.
func (s *Session) FindContacts(needle string) []Contact {
	needle = strings.ToLower(strings.TrimSpace(needle))
	all := s.Contacts()
	if needle == "" {
		return all
	}
	var out []Contact
	for _, c := range all {
		if strings.Contains(strings.ToLower(c.Name), needle) ||
			strings.HasPrefix(c.Prefix(), needle) ||
			strings.HasPrefix(c.PubKeyHex(), needle) {
			out = append(out, c)
		}
	}
	return out
}

// AddContact adds or updates a contact from a full public key. This is how a
// node seen on the public map — whose adverts never reach you — gets added.
func (s *Session) AddContact(ctx context.Context, pubKeyHex, name string, advType byte) error {
	key, err := ParsePubKey(pubKeyHex)
	if err != nil {
		return err
	}
	payload, err := EncodeAddUpdateContact(key, name, advType)
	if err != nil {
		return err
	}
	if _, err := s.expect(ctx, payload, RespOK, DefaultCommandTimeout); err != nil {
		return err
	}
	// Put it in the cache immediately so it shows up in listings without
	// waiting for the next enumeration.
	var c Contact
	copy(c.PubKey[:], key)
	c.Type, c.Name, c.OutPathLen = advType, name, NoPath
	s.mu.Lock()
	s.contacts[c.Prefix()] = c
	s.mu.Unlock()
	return nil
}

// UpdateContact renames or retypes an existing contact, keeping its stored
// path intact.
//
// There is no dedicated rename command — CmdAddUpdateContact does both — so
// this resends the whole record with the path it already had. Sending it
// without the path would quietly force the next message to that contact to
// flood the mesh.
func (s *Session) UpdateContact(ctx context.Context, prefixHex, newName string, advType byte) error {
	c, ok := s.LookupContact(prefixHex)
	if !ok {
		return ErrNotContact
	}
	if advType == AdvTypeNone {
		advType = c.Type
	}
	payload, err := EncodeContactRecord(c.PubKey[:], newName, advType, c.OutPathLen, c.OutPath[:])
	if err != nil {
		return err
	}
	if _, err := s.expect(ctx, payload, RespOK, DefaultCommandTimeout); err != nil {
		return err
	}
	s.mu.Lock()
	c.Name, c.Type = newName, advType
	s.contacts[c.Prefix()] = c
	s.mu.Unlock()
	return nil
}

// RemoveContact deletes a contact from the node. Needs the full key: the
// 12-character prefix shown next to a message is not enough.
func (s *Session) RemoveContact(ctx context.Context, pubKeyHex string) error {
	key, err := ParsePubKey(pubKeyHex)
	if err != nil {
		return err
	}
	payload, err := EncodeRemoveContact(key)
	if err != nil {
		return err
	}
	if _, err := s.expect(ctx, payload, RespOK, DefaultCommandTimeout); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.contacts, hex.EncodeToString(key[:6]))
	s.mu.Unlock()
	return nil
}

// ResetPath forgets a contact's stored route, so the next message floods.
//
// The cached record is cleared to match. UpdateContact resends whatever path
// the cache holds, so a stale entry here would quietly hand the just-forgotten
// route back to the node on the next rename — and the flood that was asked for
// would never happen.
func (s *Session) ResetPath(ctx context.Context, prefixHex string) error {
	c, ok := s.LookupContact(prefixHex)
	if !ok {
		return ErrNotContact
	}
	payload, err := EncodeResetPath(c.PubKey[:])
	if err != nil {
		return err
	}
	if _, err := s.expect(ctx, payload, RespOK, DefaultCommandTimeout); err != nil {
		return err
	}
	s.mu.Lock()
	if cur, ok := s.contacts[c.Prefix()]; ok {
		cur.OutPathLen = NoPath
		cur.OutPath = [MaxPathSize]byte{}
		s.contacts[c.Prefix()] = cur
	}
	s.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Room servers
// ---------------------------------------------------------------------------

// RoomLogin sends a login to a room server.
//
// A nil error means only that the request went out over the air. The verdict
// arrives later on LoginResults(). Treating this return as success reports
// every login as working, including the failed ones.
func (s *Session) RoomLogin(ctx context.Context, prefixHex, password string) error {
	c, ok := s.LookupContact(prefixHex)
	if !ok {
		return ErrNotContact
	}
	return s.RoomLoginKey(ctx, c.PubKey[:], password)
}

// RoomLoginKey logs in using a key the caller already holds.
//
// The cache is a convenience, not the only source of truth: the bridge mirrors
// full keys to its database, so a login should not fail merely because an
// enumeration was incomplete when it happened to run.
func (s *Session) RoomLoginKey(ctx context.Context, pubKey []byte, password string) error {
	payload, err := EncodeSendLogin(pubKey, password)
	if err != nil {
		return err
	}
	_, err = s.expect(ctx, payload, RespSent, DefaultCommandTimeout)
	return err
}

// RoomLogout ends a room-server session.
func (s *Session) RoomLogout(ctx context.Context, prefixHex string) error {
	c, ok := s.LookupContact(prefixHex)
	if !ok {
		return ErrNotContact
	}
	payload, err := EncodeLogout(c.PubKey[:])
	if err != nil {
		return err
	}
	return s.do(ctx, payload, DefaultCommandTimeout, anyReply)
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

// RefreshChannels reads all 8 channel slots into the cache.
func (s *Session) RefreshChannels(ctx context.Context) error {
	for i := byte(0); i < MaxChannels; i++ {
		info, ok, err := s.readChannel(ctx, i)
		s.mu.Lock()
		s.channelsOK[i] = ok && err == nil
		if ok && err == nil {
			s.channels[i] = info
		}
		s.mu.Unlock()
	}
	n := 0
	s.mu.RLock()
	for _, ok := range s.channelsOK {
		if ok {
			n++
		}
	}
	s.mu.RUnlock()
	s.log.Info("channels cached", "count", n)
	return nil
}

// readChannel queries one slot, twice if needed.
//
// The index echo check is load-bearing. A timed-out query for slot N answered
// during the query for slot N+1 gave N+1 the wrong name, which produced a
// duplicate Discord channel for a slot that was never real.
func (s *Session) readChannel(ctx context.Context, idx byte) (ChannelInfo, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var (
			info  ChannelInfo
			empty bool
		)
		err := s.do(ctx, EncodeGetChannel(idx), 3*time.Second, func(f []byte) (bool, error) {
			switch f[0] {
			case RespChannelInfo:
				ci, err := DecodeChannelInfo(f)
				if err != nil {
					return true, err
				}
				if ci.Index != idx {
					s.log.Debug("channel reply for the wrong slot; ignoring",
						"got", ci.Index, "want", idx)
					return false, nil
				}
				info = ci
				return true, nil
			case RespError, RespDisabled:
				empty = true // an empty slot, which is not a failure
				return true, nil
			default:
				return false, nil
			}
		})
		if err == nil {
			if empty || info.Name == "" {
				return ChannelInfo{}, false, nil
			}
			return info, true, nil
		}
		if errors.Is(err, ErrLinkDown) || ctx.Err() != nil {
			return ChannelInfo{}, false, err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return ChannelInfo{}, false, nil
}

// Channel returns a cached channel slot.
func (s *Session) Channel(idx byte) (ChannelInfo, bool) {
	if idx >= MaxChannels {
		return ChannelInfo{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channels[idx], s.channelsOK[idx]
}

// Channels returns every populated slot.
func (s *Session) Channels() []ChannelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ChannelInfo
	for i := range s.channels {
		if s.channelsOK[i] {
			out = append(out, s.channels[i])
		}
	}
	return out
}

// SetChannel creates or updates a channel on the node.
//
// The ESP32 version never used this command. It means a private channel can be
// added straight from the web UI, with no phone app involved. An empty name
// and a zero secret delete the slot.
func (s *Session) SetChannel(ctx context.Context, idx byte, name string, secret []byte) error {
	payload, err := EncodeSetChannel(idx, name, secret)
	if err != nil {
		return err
	}
	if _, err := s.expect(ctx, payload, RespOK, DefaultCommandTimeout); err != nil {
		return err
	}
	return s.RefreshChannels(ctx)
}
