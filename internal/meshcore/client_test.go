package meshcore

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeNode is a MeshCore companion that lives entirely in memory.
//
// The whole point of the transport interface is that the protocol logic can be
// exercised with no radio in the room, so this is where the sequencing rules
// get proved: one command at a time, pushes never satisfying a reply, replies
// matched to their request.
type fakeNode struct {
	mu sync.Mutex

	// respond turns one command into zero or more reply frames.
	respond func(cmd []byte) [][]byte

	out    chan []byte
	closed chan struct{}
	once   sync.Once

	// received records every command the client sent, in order.
	received [][]byte
}

func newFakeNode(respond func(cmd []byte) [][]byte) *fakeNode {
	return &fakeNode{
		respond: respond,
		out:     make(chan []byte, 256),
		closed:  make(chan struct{}),
	}
}

func (f *fakeNode) Describe() string { return "fake" }

func (f *fakeNode) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeNode) WriteFrame(ctx context.Context, cmd []byte) error {
	select {
	case <-f.closed:
		return io.ErrClosedPipe
	default:
	}
	f.mu.Lock()
	c := append([]byte(nil), cmd...)
	f.received = append(f.received, c)
	f.mu.Unlock()

	for _, r := range f.respond(c) {
		select {
		case f.out <- r:
		case <-f.closed:
			return io.ErrClosedPipe
		}
	}
	return nil
}

func (f *fakeNode) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case b := <-f.out:
		return b, nil
	case <-f.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// push injects an asynchronous frame, exactly as a real node would at any
// moment — including in the middle of a command exchange.
func (f *fakeNode) push(frame []byte) {
	select {
	case f.out <- frame:
	case <-f.closed:
	}
}

func (f *fakeNode) commands() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.received...)
}

// selfInfoFrame builds a plausible RespSelfInfo with a name.
func selfInfoFrame(name string) []byte {
	f := make([]byte, 58)
	f[0] = RespSelfInfo
	f[1] = AdvTypeChat
	for i := 0; i < PubKeySize; i++ {
		f[4+i] = byte(i + 1)
	}
	binary.LittleEndian.PutUint32(f[48:52], 906875) // kHz
	return append(f, name...)
}

func contactFrame(key byte, advType byte, name string, pathLen byte) []byte {
	f := make([]byte, contactAdvertOff+4)
	f[0] = RespContact
	for i := 0; i < PubKeySize; i++ {
		f[contactKeyOff+i] = key
	}
	f[contactTypeOff] = advType
	f[contactPathLenOff] = pathLen
	copy(f[contactNameOff:], name)
	binary.LittleEndian.PutUint32(f[contactAdvertOff:], uint32(time.Now().Unix()))
	return f
}

func channelInfoFrame(idx byte, name string) []byte {
	f := make([]byte, 2+32+ChannelSecretSize)
	f[0] = RespChannelInfo
	f[1] = idx
	copy(f[2:], name)
	return f
}

func currTimeFrame(t time.Time) []byte {
	f := make([]byte, 5)
	f[0] = RespCurrTime
	binary.LittleEndian.PutUint32(f[1:], uint32(t.Unix()))
	return f
}

// defaultResponder answers the handshake and nothing else.
func defaultResponder(extra func(cmd []byte) [][]byte) func([]byte) [][]byte {
	return func(cmd []byte) [][]byte {
		switch cmd[0] {
		case CmdAppStart:
			return [][]byte{selfInfoFrame("Ridge Node")}
		case CmdDeviceQuery:
			return [][]byte{{RespDeviceInfo, 3}}
		case CmdSetDeviceTime:
			return [][]byte{{RespOK}}
		case CmdGetDeviceTime:
			return [][]byte{currTimeFrame(time.Now())}
		}
		if extra != nil {
			return extra(cmd)
		}
		return nil
	}
}

func newTestSession(t *testing.T, node *fakeNode) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sess, err := NewSession(ctx, node, nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestHandshakeReadsSelfInfo(t *testing.T) {
	node := newFakeNode(defaultResponder(nil))
	sess := newTestSession(t, node)

	if got := sess.SelfInfo().Name; got != "Ridge Node" {
		t.Errorf("node name = %q", got)
	}
	if sess.SelfInfo().FreqKHz != 906875 {
		t.Errorf("frequency = %d", sess.SelfInfo().FreqKHz)
	}

	// CMD_APP_START must be first — nothing else is valid before the
	// handshake completes.
	cmds := node.commands()
	if len(cmds) == 0 || cmds[0][0] != CmdAppStart {
		t.Fatalf("first command was 0x%02X, want CMD_APP_START", cmds[0][0])
	}
}

// The rule that cost the ESP32 three separate bugs: a push landing mid-command
// must never be mistaken for that command's reply.
func TestPushesNeverSatisfyACommand(t *testing.T) {
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		if cmd[0] != CmdSyncNextMessage {
			return nil
		}
		// Answer with a burst of pushes BEFORE the real reply. Every one of
		// these has a first byte >= 0x80 and must be routed away from the
		// response path.
		msg := []byte{RespContactMsgRecv, 1, 2, 3, 4, 5, 6, 0x02, 0x00, 0, 0, 0, 0}
		msg = append(msg, "the real reply"...)
		return [][]byte{
			{PushLogRxData, 1, 2, 3},
			{PushAdvert, 9, 9},
			{PushNewAdvert, 1},
			msg,
		}
	}))
	sess := newTestSession(t, node)

	ctx := context.Background()
	m, got, err := sess.NextMessage(ctx)
	if err != nil {
		t.Fatalf("NextMessage: %v", err)
	}
	if !got {
		t.Fatal("no message returned; a push was probably read as the reply")
	}
	if m.Text != "the real reply" {
		t.Errorf("text = %q", m.Text)
	}
}

func TestMsgWaitingPushWakesTheDrain(t *testing.T) {
	node := newFakeNode(defaultResponder(nil))
	sess := newTestSession(t, node)

	node.push([]byte{PushMsgWaiting})
	select {
	case <-sess.MsgWaiting():
	case <-time.After(2 * time.Second):
		t.Fatal("PUSH_CODE_MSG_WAITING did not wake anything")
	}
}

func TestConfirmationAndLoginPushesAreDelivered(t *testing.T) {
	node := newFakeNode(defaultResponder(nil))
	sess := newTestSession(t, node)

	conf := make([]byte, 9)
	conf[0] = PushSendConfirmed
	binary.LittleEndian.PutUint32(conf[1:5], 4242)
	binary.LittleEndian.PutUint32(conf[5:9], 7000)
	node.push(conf)

	select {
	case c := <-sess.Confirmations():
		if c.Ack != 4242 || c.RoundTrip != 7*time.Second {
			t.Errorf("confirmation = %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery confirmation arrived")
	}

	node.push([]byte{PushLoginSuccess, 0x01, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0, 0, 0, 0})
	select {
	case r := <-sess.LoginResults():
		if !r.OK || r.Prefix != "aabbccddeeff" {
			t.Errorf("login result = %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no login result arrived")
	}
}

func TestContactEnumeration(t *testing.T) {
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		if cmd[0] != CmdGetContacts {
			return nil
		}
		return [][]byte{
			{RespContactsStart, 3},
			contactFrame(0xA1, AdvTypeChat, "Alice", 2),
			contactFrame(0xB2, AdvTypeRoom, "Ridge Room", 0xFF),
			// A repeater. The ESP32 threw these away to save RAM; a Pi keeps
			// them, which is what makes `contact find` instant.
			contactFrame(0xC3, AdvTypeRepeater, "Hilltop Repeater", 1),
			{RespEndOfContacts},
		}
	}))
	sess := newTestSession(t, node)

	n, complete, err := sess.RefreshContacts(context.Background())
	if err != nil {
		t.Fatalf("RefreshContacts: %v", err)
	}
	if !complete {
		t.Error("a stream ending in END_OF_CONTACTS was not reported as complete")
	}
	if n != 3 {
		t.Fatalf("cached %d contacts, want 3 — repeaters must be kept too", n)
	}

	c, ok := sess.LookupContact("a1a1a1a1a1a1")
	if !ok {
		t.Fatal("could not resolve a contact by key prefix")
	}
	if c.Name != "Alice" || c.Type != AdvTypeChat {
		t.Errorf("contact = %+v", c)
	}
	// The full key must be cached: several commands require it and cannot work
	// from the 6-byte prefix a message carries.
	if len(c.PubKeyHex()) != 64 {
		t.Errorf("full key not cached: %q", c.PubKeyHex())
	}

	if got := sess.FindContacts("repeater"); len(got) != 1 {
		t.Errorf("search for repeaters found %d", len(got))
	}
	if got := sess.FindContacts(""); len(got) != 3 {
		t.Errorf("empty search found %d, want all 3", len(got))
	}
}

// Clearing a path must clear the cached copy too.
//
// UpdateContact resends whatever path the cache holds, because there is no
// dedicated rename command. So a cache left stale after a reset would hand the
// just-forgotten route straight back to the node on the next rename, and the
// flood that was asked for would silently never happen.
func TestResetPathClearsTheCachedPath(t *testing.T) {
	var sentRecordPathLen = -1
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		switch cmd[0] {
		case CmdGetContacts:
			return [][]byte{
				{RespContactsStart, 1},
				contactFrame(0xA1, AdvTypeChat, "Alice", 3),
				{RespEndOfContacts},
			}
		case CmdResetPath:
			return [][]byte{{RespOK}}
		case CmdAddUpdateContact:
			// [cmd][pubkey 32][type][flags][out_path_len]... — mind the flags
			// byte, which is easy to miscount and always reads as 0.
			sentRecordPathLen = int(cmd[1+PubKeySize+2])
			return [][]byte{{RespOK}}
		}
		return nil
	}))
	sess := newTestSession(t, node)

	if _, _, err := sess.RefreshContacts(context.Background()); err != nil {
		t.Fatalf("RefreshContacts: %v", err)
	}
	if c, _ := sess.LookupContact("a1a1a1a1a1a1"); c.OutPathLen != 3 {
		t.Fatalf("setup: cached path len = %d, want 3", c.OutPathLen)
	}

	if err := sess.ResetPath(context.Background(), "a1a1a1a1a1a1"); err != nil {
		t.Fatalf("ResetPath: %v", err)
	}
	c, ok := sess.LookupContact("a1a1a1a1a1a1")
	if !ok {
		t.Fatal("the contact vanished from the cache; only its path should have gone")
	}
	if c.OutPathLen != NoPath {
		t.Errorf("cached path len = %d after a reset, want %d (NoPath)", c.OutPathLen, NoPath)
	}
	if c.OutPath != ([MaxPathSize]byte{}) {
		t.Error("the stored hop list survived a reset")
	}

	// And prove the consequence, which is the reason this matters at all.
	if err := sess.UpdateContact(context.Background(), "a1a1a1a1a1a1", "Alicia", AdvTypeNone); err != nil {
		t.Fatalf("UpdateContact: %v", err)
	}
	if sentRecordPathLen != NoPath {
		t.Errorf("a rename after a reset sent path len %d, want %d — it restored the "+
			"forgotten route, so the next message would not flood",
			sentRecordPathLen, NoPath)
	}
}

// A timed-out query for slot N answered during the query for slot N+1 gave
// N+1 the wrong name, and produced a duplicate Discord channel for a slot that
// was never real. The index echo is what stops that.
func TestChannelQueryRejectsAReplyForTheWrongSlot(t *testing.T) {
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		if cmd[0] != CmdGetChannel {
			return nil
		}
		switch cmd[1] {
		case 0:
			return [][]byte{channelInfoFrame(0, "Public")}
		case 1:
			// The node answers slot 1's query with slot 0's data — the exact
			// mis-attribution that caused the bug. It must be discarded, and
			// the retry answered correctly.
			return [][]byte{channelInfoFrame(0, "Public"), channelInfoFrame(1, "Emergency")}
		default:
			return [][]byte{{RespError}} // an empty slot
		}
	}))
	sess := newTestSession(t, node)

	if err := sess.RefreshChannels(context.Background()); err != nil {
		t.Fatalf("RefreshChannels: %v", err)
	}

	ch0, ok := sess.Channel(0)
	if !ok || ch0.Name != "Public" {
		t.Errorf("slot 0 = %+v ok=%v", ch0, ok)
	}
	ch1, ok := sess.Channel(1)
	if !ok {
		t.Fatal("slot 1 was not read")
	}
	if ch1.Name != "Emergency" {
		t.Errorf("slot 1 named %q — a reply for the wrong slot was accepted", ch1.Name)
	}
	if ch1.Index != 1 {
		t.Errorf("slot 1 has index %d", ch1.Index)
	}
	// Empty slots must not be reported as channels.
	if got := len(sess.Channels()); got != 2 {
		t.Errorf("%d channels reported, want 2 — empty slots leaked in", got)
	}
}

func TestSendDMReturnsTheAckHandle(t *testing.T) {
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		if cmd[0] != CmdSendTxtMsg {
			return nil
		}
		f := make([]byte, 10)
		f[0] = RespSent
		f[1] = 1 // flooded
		binary.LittleEndian.PutUint32(f[2:6], 999)
		binary.LittleEndian.PutUint32(f[6:10], 30000)
		return [][]byte{f}
	}))
	sess := newTestSession(t, node)

	res, err := sess.SendDM(context.Background(), "aabbccddeeff", "hello")
	if err != nil {
		t.Fatalf("SendDM: %v", err)
	}
	if res.ExpectedAck != 999 || !res.Flooded || res.EstTimeout != 30*time.Second {
		t.Errorf("send result = %+v", res)
	}

	// The wire format must carry the 6-byte prefix at the documented offset.
	var sent []byte
	for _, c := range node.commands() {
		if c[0] == CmdSendTxtMsg {
			sent = c
		}
	}
	if sent == nil {
		t.Fatal("no send command reached the node")
	}
	if string(sent[13:]) != "hello" {
		t.Errorf("body at the wrong offset: %q", sent[7:])
	}
}

// Group messages cannot be acknowledged: the node answers with a plain OK and
// no handle, so delivery is never confirmable for these.
func TestSendChannelExpectsAPlainOK(t *testing.T) {
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		if cmd[0] == CmdSendChannelTxtMsg {
			return [][]byte{{RespOK}}
		}
		return nil
	}))
	sess := newTestSession(t, node)

	if err := sess.SendChannel(context.Background(), 2, "hi all"); err != nil {
		t.Fatalf("SendChannel: %v", err)
	}
}

func TestRoomLoginNeedsTheFullKeyFromTheCache(t *testing.T) {
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		switch cmd[0] {
		case CmdGetContacts:
			return [][]byte{
				{RespContactsStart, 1},
				contactFrame(0xB2, AdvTypeRoom, "Ridge Room", 0xFF),
				{RespEndOfContacts},
			}
		case CmdSendLogin:
			return [][]byte{{RespSent, 0, 0, 0, 0, 0, 0, 0, 0, 0}}
		}
		return nil
	}))
	sess := newTestSession(t, node)
	ctx := context.Background()

	// Before the cache is populated the prefix cannot be resolved to a full
	// key, so the login must be refused rather than sent with a short key.
	if err := sess.RoomLogin(ctx, "b2b2b2b2b2b2", "pw"); !errors.Is(err, ErrNotContact) {
		t.Errorf("login with an unknown contact returned %v, want ErrNotContact", err)
	}

	if _, _, err := sess.RefreshContacts(ctx); err != nil {
		t.Fatalf("RefreshContacts: %v", err)
	}
	if err := sess.RoomLogin(ctx, "b2b2b2b2b2b2", "pw"); err != nil {
		t.Fatalf("RoomLogin: %v", err)
	}

	var login []byte
	for _, c := range node.commands() {
		if c[0] == CmdSendLogin {
			login = c
		}
	}
	if login == nil {
		t.Fatal("no login command reached the node")
	}
	if len(login) != 1+PubKeySize+2 {
		t.Fatalf("login is %d bytes, want %d", len(login), 1+PubKeySize+2)
	}
	if string(login[1+PubKeySize:]) != "pw" {
		t.Errorf("password at the wrong offset: %q", login[1+PubKeySize:])
	}
}

func TestCommandsTimeOutRatherThanHang(t *testing.T) {
	node := newFakeNode(defaultResponder(nil)) // answers the handshake, then nothing
	sess := newTestSession(t, node)

	start := time.Now()
	_, _, err := sess.NextMessage(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 3*DefaultCommandTimeout {
		t.Errorf("took %v to time out", d)
	}
}

func TestLinkDeathUnblocksCommands(t *testing.T) {
	node := newFakeNode(defaultResponder(nil))
	sess := newTestSession(t, node)

	go func() {
		time.Sleep(100 * time.Millisecond)
		node.Close()
	}()

	start := time.Now()
	_, _, err := sess.NextMessage(context.Background())
	if err == nil {
		t.Fatal("a command succeeded after the link died")
	}
	// It must not sit out the full command timeout when the link is gone.
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v to notice the link was gone", d)
	}
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Error("Done() was not closed when the link died")
	}
}

// The stream framing used by serial and TCP, round-tripped.
func TestStreamTransportFraming(t *testing.T) {
	a, b := newPipePair()
	client := NewStreamTransport(a, "test-client")
	node := NewStreamTransport(b, "test-node")
	defer client.Close()
	defer node.Close()

	ctx := context.Background()
	frames := [][]byte{
		{CmdAppStart},
		[]byte("a longer frame with binary \x00\x01\x02\xff inside it"),
		make([]byte, 300),
	}

	// The node writes with the device marker; the client reads it. Written by
	// hand from the node side, since a Transport always writes '>'.
	//
	// In a goroutine because io.Pipe is unbuffered: writing before anyone
	// reads would deadlock the test rather than the code.
	writeErr := make(chan error, 1)
	go func() {
		for _, f := range frames {
			hdr := []byte{frameFromDevice, 0, 0}
			binary.LittleEndian.PutUint16(hdr[1:3], uint16(len(f)))
			if _, err := b.Write(append(hdr, f...)); err != nil {
				writeErr <- err
				return
			}
		}
		close(writeErr)
	}()
	for i, want := range frames {
		got, err := client.ReadFrame(ctx)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(got) != len(want) {
			t.Errorf("frame %d: got %d bytes, want %d", i, len(got), len(want))
		}
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Serial ports carry boot banners and the occasional lost byte. A desynced
// stream must resynchronise rather than die.
func TestStreamTransportResynchronises(t *testing.T) {
	a, b := newPipePair()
	client := NewStreamTransport(a, "test")
	defer client.Close()
	defer b.Close()

	go func() {
		// Plain-text noise, then a truncated header, then a real frame.
		_, _ = b.Write([]byte("MeshCore booting...\r\nready\r\n"))
		payload := []byte("real frame")
		hdr := []byte{frameFromDevice, 0, 0}
		binary.LittleEndian.PutUint16(hdr[1:3], uint16(len(payload)))
		_, _ = b.Write(append(hdr, payload...))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := client.ReadFrame(ctx)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(got) != "real frame" {
		t.Errorf("got %q — the reader did not resynchronise past the banner", got)
	}
}

// newPipePair returns two connected in-memory streams.
func newPipePair() (io.ReadWriteCloser, io.ReadWriteCloser) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	return &rwPipe{r: ar, w: aw}, &rwPipe{r: br, w: bw}
}

type rwPipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *rwPipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *rwPipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *rwPipe) Close() error {
	_ = p.r.Close()
	return p.w.Close()
}

// A contact enumeration is a stream the node can abandon part-way — it is a
// single-threaded radio and can simply stop answering for a while. Treating
// that partial answer as the whole truth deletes contacts that are still real.
//
// This is exactly what happened in the field: a room server dropped out of the
// cache mid-session, so the bridge could no longer resolve its key and refused
// to log in to it, reporting it as "not in the node's contact list".
func TestPartialEnumerationDoesNotForgetKnownContacts(t *testing.T) {
	full := true
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		if cmd[0] != CmdGetContacts {
			return nil
		}
		if full {
			return [][]byte{
				{RespContactsStart, 3},
				contactFrame(0xA1, AdvTypeChat, "Alice", 2),
				contactFrame(0xB2, AdvTypeRoom, "Ridge Room", 0xFF),
				contactFrame(0xF4, AdvTypeRoom, "Tina", 0xFF),
				{RespEndOfContacts},
			}
		}
		// Second time: the node stops after the first contact and never sends
		// END_OF_CONTACTS.
		return [][]byte{
			{RespContactsStart, 3},
			contactFrame(0xA1, AdvTypeChat, "Alice", 2),
		}
	}))
	sess := newTestSession(t, node)
	// The real patience is 25s; the point here is the behaviour, not the wait.
	sess.enumIdle = 250 * time.Millisecond
	ctx := context.Background()

	if n, complete, err := sess.RefreshContacts(ctx); err != nil || !complete || n != 3 {
		t.Fatalf("first refresh: n=%d complete=%v err=%v", n, complete, err)
	}

	// Now the truncated one. It must be reported as incomplete, and must not
	// evict the two contacts it did not mention.
	full = false
	n, complete, err := sess.RefreshContacts(ctx)
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if complete {
		t.Error("a stream with no END_OF_CONTACTS was reported as complete")
	}
	if n != 3 {
		t.Errorf("cache holds %d contacts after a partial read, want 3 — the tail was forgotten", n)
	}
	if _, ok := sess.LookupContact("f4f4f4f4f4f4"); !ok {
		t.Error("the room server vanished from the cache; this is the bug that broke room login")
	}

	// And the room login path must still find its key.
	if err := sess.RoomLogin(ctx, "f4f4f4f4f4f4", "pw"); errors.Is(err, ErrNotContact) {
		t.Error("room login could not resolve a contact it had already seen")
	}
}

func TestSendCLIMarksTheMessageAsCLIData(t *testing.T) {
	node := newFakeNode(defaultResponder(func(cmd []byte) [][]byte {
		if cmd[0] != CmdSendTxtMsg {
			return nil
		}
		// expected_ack stays 0: the companion firmware never asks for an ack on
		// a CLI message, so the reply coming back is the only proof of anything.
		f := make([]byte, 10)
		f[0] = RespSent
		return [][]byte{f}
	}))
	sess := newTestSession(t, node)

	res, err := sess.SendCLI(context.Background(), "aabbccddeeff", "clock")
	if err != nil {
		t.Fatalf("SendCLI: %v", err)
	}
	if res.ExpectedAck != 0 {
		t.Errorf("ExpectedAck = %d, want 0 — CLI messages are never acked", res.ExpectedAck)
	}

	var sent []byte
	for _, c := range node.commands() {
		if c[0] == CmdSendTxtMsg {
			sent = c
		}
	}
	if sent == nil {
		t.Fatal("no CmdSendTxtMsg reached the node")
	}
	if sent[1] != TxtTypeCLIData {
		t.Fatalf("txt_type = %d, want %d — the far node would treat this as chat, not a command",
			sent[1], TxtTypeCLIData)
	}
	if got := string(sent[13:]); got != "clock" {
		t.Errorf("command text = %q, want %q", got, "clock")
	}
}

// A node whose clock is ahead refuses to be corrected, and that has to be
// distinguishable from every other rejection: it is the one case that needs a
// USB cable rather than a retry.
func TestSetDeviceTimeReportsARefusalToGoBackwards(t *testing.T) {
	// Not defaultResponder: it answers the clock commands itself, so a refusal
	// has to be wired in ahead of it.
	node := newFakeNode(func(cmd []byte) [][]byte {
		switch cmd[0] {
		case CmdAppStart:
			return [][]byte{selfInfoFrame("Ridge Node")}
		case CmdSetDeviceTime:
			return [][]byte{{RespError, ErrCodeIllegalArg}}
		}
		return nil
	})
	sess := newTestSession(t, node)

	err := sess.SetDeviceTime(context.Background(), time.Now())
	if !errors.Is(err, ErrClockBackwards) {
		t.Errorf("err = %v, want ErrClockBackwards", err)
	}
}

func TestDeviceTimeReadsTheNodeClock(t *testing.T) {
	node := newFakeNode(func(cmd []byte) [][]byte {
		switch cmd[0] {
		case CmdAppStart:
			return [][]byte{selfInfoFrame("Ridge Node")}
		case CmdSetDeviceTime:
			return [][]byte{{RespOK}}
		case CmdGetDeviceTime:
			return [][]byte{currTimeFrame(time.Unix(1786000000, 0))}
		}
		return nil
	})
	sess := newTestSession(t, node)

	got, err := sess.DeviceTime(context.Background())
	if err != nil {
		t.Fatalf("DeviceTime: %v", err)
	}
	if !got.Equal(time.Unix(1786000000, 0)) {
		t.Errorf("time = %v, want %v", got, time.Unix(1786000000, 0))
	}
}
