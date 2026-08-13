package meshcore

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// Each test below pins down one of the gotchas recorded in PROTOCOL-NOTES.md.
// They cost real debugging on the ESP32; a unit test is a cheaper way to keep
// them fixed.

func TestDecodeContactMessage(t *testing.T) {
	// [0x07][prefix 6][path][txt_type][ts u32][text]
	f := []byte{RespContactMsgRecv,
		0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, // prefix
		0x07,       // path byte: 7 hops
		0x00,       // txt_type plain
		0, 0, 0, 0} // timestamp
	binary.LittleEndian.PutUint32(f[9:13], 1700000000)
	f = append(f, []byte("hello mesh")...)

	m, err := DecodeMessage(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.IsChannel {
		t.Error("contact message decoded as a channel message")
	}
	if m.PubKeyPrefix != "deadbeef0011" {
		t.Errorf("prefix = %q", m.PubKeyPrefix)
	}
	if m.Text != "hello mesh" {
		t.Errorf("text = %q", m.Text)
	}
	if !m.HaveHops || m.Hops != 7 {
		t.Errorf("hops = %d have=%v; the path byte is packed, not a count", m.Hops, m.HaveHops)
	}
	if m.HaveSNR {
		t.Error("a non-V3 frame carries no SNR")
	}
	if !m.Timestamp.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("timestamp = %v", m.Timestamp)
	}
}

// The path byte is packed: bits 0-5 are the hop count, bits 6-7 the per-hop
// hash size. Printing it raw showed "71 hops" for what was actually 7.
func TestPathByteIsPackedNotACount(t *testing.T) {
	for _, tc := range []struct {
		raw      byte
		wantHops byte
		haveHops bool
	}{
		{0x07, 7, true},
		{0x47, 7, true},  // hash-size bits set; still 7 hops
		{0x87, 7, true},  // ditto
		{0x00, 0, true},  // heard direct: a flood with no repeaters
		{0xFF, 0, false}, // direct route: hop count does not apply
	} {
		f := []byte{RespContactMsgRecv, 1, 2, 3, 4, 5, 6, tc.raw, 0, 0, 0, 0, 0}
		m, err := DecodeMessage(f)
		if err != nil {
			t.Fatalf("raw=0x%02X: %v", tc.raw, err)
		}
		if m.HaveHops != tc.haveHops {
			t.Errorf("raw=0x%02X HaveHops = %v, want %v", tc.raw, m.HaveHops, tc.haveHops)
		}
		if tc.haveHops && m.Hops != tc.wantHops {
			t.Errorf("raw=0x%02X hops = %d, want %d", tc.raw, m.Hops, tc.wantHops)
		}
	}
}

func TestDecodeChannelMessageV3(t *testing.T) {
	// [0x11][snr][2 reserved][idx][path][txt_type][ts u32][text]
	f := []byte{RespChannelMsgRecvV3,
		0xEC, // SNR as a signed byte: -20, which is -5.0 dB
		0, 0,
		3,    // channel index
		0xFF, // direct route
		0x00,
		0, 0, 0, 0}
	binary.LittleEndian.PutUint32(f[7:11], 1700000000)
	f = append(f, []byte("Alice: hi all")...)

	m, err := DecodeMessage(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !m.IsChannel || m.ChannelIdx != 3 {
		t.Errorf("channel idx = %d, isChannel = %v", m.ChannelIdx, m.IsChannel)
	}
	if !m.HaveSNR || m.SNR != -5.0 {
		t.Errorf("snr = %v have = %v", m.SNR, m.HaveSNR)
	}
	if m.HaveHops {
		t.Error("0xFF means a direct route; a hop count must not be reported")
	}
	if m.Text != "Alice: hi all" {
		t.Errorf("text = %q", m.Text)
	}
}

// txt_type == 2 does NOT carry a signature. Those 4 bytes are the original
// author's public-key prefix — how a room server says who wrote a post.
func TestTxtType2CarriesAuthorPrefixNotASignature(t *testing.T) {
	f := []byte{RespContactMsgRecv,
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		0x02, // 2 hops
		0x02, // txt_type 2
		0, 0, 0, 0,
		0xca, 0xfe, 0xba, 0xbe} // author prefix
	binary.LittleEndian.PutUint32(f[9:13], 1700000000)
	f = append(f, []byte("posted to the room")...)

	m, err := DecodeMessage(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.AuthorPrefix != "cafebabe" {
		t.Errorf("author prefix = %q, want cafebabe", m.AuthorPrefix)
	}
	if m.Text != "posted to the room" {
		t.Errorf("text = %q — the 4 author bytes were probably read as text", m.Text)
	}
}

func TestDecodeMessageRejectsShortFrames(t *testing.T) {
	// Every truncation must be an error, never a panic and never a
	// half-populated message that reads as real.
	full := []byte{RespContactMsgRecvV3, 0, 0, 0, 1, 2, 3, 4, 5, 6, 0x01, 0x00, 0, 0, 0, 0}
	for n := 0; n < len(full); n++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked on a %d-byte frame: %v", n, r)
				}
			}()
			if _, err := DecodeMessage(full[:n]); err == nil && n < 12 {
				t.Errorf("%d-byte frame decoded without error", n)
			}
		}()
	}
}

func TestDecodeMessageRejectsWrongCode(t *testing.T) {
	if _, err := DecodeMessage([]byte{PushAdvert, 1, 2, 3}); err == nil {
		t.Error("a push was accepted as a message")
	}
}

func TestIsPush(t *testing.T) {
	for _, c := range []byte{0x80, 0x83, 0x8F, 0x90, 0xFF} {
		if !IsPush(c) {
			t.Errorf("0x%02X should be a push", c)
		}
	}
	for _, c := range []byte{0x00, 0x07, 0x12, 0x7F} {
		if IsPush(c) {
			t.Errorf("0x%02X should be a reply", c)
		}
	}
}

func TestDecodeContact(t *testing.T) {
	f := make([]byte, contactLatOff+8)
	f[0] = RespContact
	for i := 0; i < PubKeySize; i++ {
		f[contactKeyOff+i] = byte(i)
	}
	f[contactTypeOff] = AdvTypeRoom
	f[contactPathLenOff] = 3
	copy(f[contactNameOff:], "Ridge Room")
	binary.LittleEndian.PutUint32(f[contactAdvertOff:], 1700000000)
	// Coordinates are signed micro-degrees, so a western longitude is negative.
	lat, lon := int32(45123456), int32(-122987654)
	binary.LittleEndian.PutUint32(f[contactLatOff:], uint32(lat))
	binary.LittleEndian.PutUint32(f[contactLonOff:], uint32(lon))

	c, err := DecodeContact(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Name != "Ridge Room" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Type != AdvTypeRoom {
		t.Errorf("type = %d", c.Type)
	}
	if c.OutPathLen != 3 {
		t.Errorf("path len = %d", c.OutPathLen)
	}
	if c.Prefix() != "000102030405" {
		t.Errorf("prefix = %q", c.Prefix())
	}
	if len(c.PubKeyHex()) != 64 {
		t.Errorf("full key is %d chars, want 64", len(c.PubKeyHex()))
	}
	if c.Lat < 45.12 || c.Lat > 45.13 {
		t.Errorf("lat = %v", c.Lat)
	}
	if c.Lon > -122.98 || c.Lon < -122.99 {
		t.Errorf("lon = %v", c.Lon)
	}
}

// A name arrives in a fixed 32-byte NUL-padded field. Reading all 32 and
// trimming naively would cut a multi-byte character in half.
func TestContactNameStopsAtNulAndStaysValidUTF8(t *testing.T) {
	f := make([]byte, contactAdvertOff+4)
	f[0] = RespContact
	f[contactTypeOff] = AdvTypeChat
	copy(f[contactNameOff:], "Café 🏔 Ridge\x00garbage-after-nul")

	c, err := DecodeContact(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Name != "Café 🏔 Ridge" {
		t.Errorf("name = %q", c.Name)
	}
}

func TestDecodeChannelInfo(t *testing.T) {
	f := make([]byte, 2+32+ChannelSecretSize)
	f[0] = RespChannelInfo
	f[1] = 5
	copy(f[2:], "Public")
	for i := range f[34:] {
		f[34+i] = byte(i)
	}
	c, err := DecodeChannelInfo(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Index != 5 || c.Name != "Public" {
		t.Errorf("idx = %d name = %q", c.Index, c.Name)
	}
	if c.Secret[15] != 15 {
		t.Errorf("secret not copied: %v", c.Secret)
	}
}

func TestDecodeSendResult(t *testing.T) {
	f := make([]byte, 10)
	f[0] = RespSent
	f[1] = 1 // flooded
	binary.LittleEndian.PutUint32(f[2:6], 0xAABBCCDD)
	binary.LittleEndian.PutUint32(f[6:10], 45000)

	r, err := DecodeSendResult(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.Flooded {
		t.Error("flood flag not read")
	}
	if r.ExpectedAck != 0xAABBCCDD {
		t.Errorf("ack = %08X", r.ExpectedAck)
	}
	if r.EstTimeout != 45*time.Second {
		t.Errorf("timeout = %v", r.EstTimeout)
	}

	// Short form: older firmware answers with the code alone.
	if _, err := DecodeSendResult([]byte{RespSent}); err != nil {
		t.Errorf("a bare RespSent should be tolerated: %v", err)
	}
	if _, err := DecodeSendResult([]byte{RespOK}); err == nil {
		t.Error("RespOK was accepted as a send result")
	}
}

// Both login pushes carry the room's prefix at the same offset, which is what
// identifies which room answered.
func TestDecodeLoginResult(t *testing.T) {
	ok := []byte{PushLoginSuccess, 0x01, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0, 0, 0, 0}
	r, err := DecodeLoginResult(ok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.OK || r.Prefix != "aabbccddeeff" {
		t.Errorf("ok=%v prefix=%q", r.OK, r.Prefix)
	}

	fail := []byte{PushLoginFail, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	r, err = DecodeLoginResult(fail)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Prefix != "112233445566" {
		t.Errorf("ok=%v prefix=%q", r.OK, r.Prefix)
	}
}

func TestDecodeConfirmation(t *testing.T) {
	f := make([]byte, 9)
	f[0] = PushSendConfirmed
	binary.LittleEndian.PutUint32(f[1:5], 12345)
	binary.LittleEndian.PutUint32(f[5:9], 8500)
	c, err := DecodeConfirmation(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Ack != 12345 || c.RoundTrip != 8500*time.Millisecond {
		t.Errorf("ack=%d trip=%v", c.Ack, c.RoundTrip)
	}
	if _, err := DecodeConfirmation([]byte{PushSendConfirmed, 1}); err == nil {
		t.Error("short confirmation accepted")
	}
}

// CMD_APP_START is [0x01][7 reserved][app name]. A hardcoded length silently
// truncated the name to "meshyc" after the app was renamed.
func TestEncodeAppStartDerivesLengthFromTheName(t *testing.T) {
	f := EncodeAppStart(AppName)
	if f[0] != CmdAppStart {
		t.Fatalf("opcode = 0x%02X", f[0])
	}
	if len(f) != 8+len(AppName) {
		t.Fatalf("length = %d, want %d", len(f), 8+len(AppName))
	}
	if !bytes.Equal(f[1:8], make([]byte, 7)) {
		t.Error("reserved bytes are not zero")
	}
	if string(f[8:]) != AppName {
		t.Errorf("name = %q", string(f[8:]))
	}
}

func TestEncodeSendTxtMsg(t *testing.T) {
	prefix := []byte{1, 2, 3, 4, 5, 6}
	f, err := EncodeSendTxtMsg(prefix, "hi", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := []byte{CmdSendTxtMsg, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 'h', 'i'}
	binary.LittleEndian.PutUint32(want[3:7], 1700000000)
	if !bytes.Equal(f, want) {
		t.Errorf("got  %v\nwant %v", f, want)
	}

	if _, err := EncodeSendTxtMsg([]byte{1, 2, 3}, "hi", time.Now()); err == nil {
		t.Error("a 3-byte prefix was accepted; a message needs 6")
	}
}

// The message ceiling is enforced at the encoder as a last line of defence,
// and must never split a UTF-8 sequence while doing it.
func TestEncodeClampsToTheMessageLimitOnAUTF8Boundary(t *testing.T) {
	long := ""
	for len(long) < 200 {
		long += "é" // 2 bytes each
	}
	f, err := EncodeSendTxtMsg([]byte{1, 2, 3, 4, 5, 6}, long, time.Now())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body := f[13:]
	if len(body) > MaxMsgLen {
		t.Errorf("body is %d bytes, over the %d limit", len(body), MaxMsgLen)
	}
	if len(body)%2 != 0 {
		t.Errorf("body is %d bytes: a 2-byte character was split in half", len(body))
	}
}

func TestCommandsNeedingFullKeysRejectPrefixes(t *testing.T) {
	short := make([]byte, 6)
	full := make([]byte, PubKeySize)

	for name, fn := range map[string]func([]byte) ([]byte, error){
		"remove contact": EncodeRemoveContact,
		"reset path":     EncodeResetPath,
		"logout":         EncodeLogout,
		"get by key":     EncodeGetContactByKey,
		"login":          func(k []byte) ([]byte, error) { return EncodeSendLogin(k, "pw") },
		"add contact":    func(k []byte) ([]byte, error) { return EncodeAddUpdateContact(k, "n", AdvTypeChat) },
	} {
		if _, err := fn(short); err == nil {
			t.Errorf("%s accepted a 6-byte prefix; it needs the full 32-byte key", name)
		}
		if _, err := fn(full); err != nil {
			t.Errorf("%s rejected a full key: %v", name, err)
		}
	}
}

func TestEncodeAddUpdateContactLayout(t *testing.T) {
	key := make([]byte, PubKeySize)
	f, err := EncodeAddUpdateContact(key, "Ridge", AdvTypeRoom)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if f[0] != CmdAddUpdateContact {
		t.Fatalf("opcode = 0x%02X", f[0])
	}
	if f[1+PubKeySize] != AdvTypeRoom {
		t.Errorf("type byte = %d", f[1+PubKeySize])
	}
	// out_path_len must be 0xFF: no known path, so the node floods to find one.
	if f[1+PubKeySize+2] != 0xFF {
		t.Errorf("out_path_len = 0x%02X, want 0xFF", f[1+PubKeySize+2])
	}
	nameOff := 1 + PubKeySize + 3 + MaxPathSize
	if string(bytes.TrimRight(f[nameOff:nameOff+32], "\x00")) != "Ridge" {
		t.Errorf("name field = %q", f[nameOff:nameOff+32])
	}
}

func TestEncodeSetChannel(t *testing.T) {
	secret := make([]byte, ChannelSecretSize)
	f, err := EncodeSetChannel(2, "Private", secret)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if f[0] != CmdSetChannel || f[1] != 2 {
		t.Errorf("opcode=0x%02X idx=%d", f[0], f[1])
	}
	if len(f) != 2+32+ChannelSecretSize {
		t.Errorf("length = %d", len(f))
	}
	if _, err := EncodeSetChannel(2, "x", []byte{1, 2, 3}); err == nil {
		t.Error("a 3-byte channel secret was accepted")
	}
	// An empty name with a zero secret deletes the slot; that must encode.
	if _, err := EncodeSetChannel(2, "", nil); err != nil {
		t.Errorf("delete form rejected: %v", err)
	}
}

func TestParseKeys(t *testing.T) {
	if _, err := ParsePrefix("aabbccddeeff"); err != nil {
		t.Errorf("12-char prefix rejected: %v", err)
	}
	if _, err := ParsePrefix("aabbccdd"); err != nil {
		t.Errorf("8-char author prefix rejected: %v", err)
	}
	if _, err := ParsePrefix("aabb"); err == nil {
		t.Error("4-char prefix accepted")
	}
	if _, err := ParsePubKey("aabbccddeeff"); err == nil {
		t.Error("a prefix was accepted as a full key")
	}
	if _, err := ParsePubKey(strings.Repeat("z", 64)); err == nil {
		t.Error("non-hex accepted as a key")
	}
}

func TestTruncateUTF8NeverSplitsACharacter(t *testing.T) {
	s := "🏔🏔🏔" // 4 bytes each
	for n := 0; n <= len(s); n++ {
		got := TruncateUTF8(s, n)
		if len(got) > n {
			t.Errorf("n=%d produced %d bytes", n, len(got))
		}
		if len(got)%4 != 0 {
			t.Errorf("n=%d produced %q, splitting a 4-byte character", n, got)
		}
	}
}

// A contact's out_path_len is the SAME packed byte as a packet's path byte
// (Packet.cpp:20), not a hop count and not a length. Showing it raw produced
// "67 hops" for a contact three hops away — the ESP32 decoded it for messages
// and then printed it raw for contacts.
func TestPathByteIsPackedForContactsToo(t *testing.T) {
	for _, tc := range []struct {
		raw     byte
		hops    byte
		known   bool
		pathLen int
		what    string
	}{
		{0xFF, 0, false, 0, "no known path"},
		{0x00, 0, true, 0, "direct, zero hops"},
		{0x03, 3, true, 3, "3 hops, 1-byte hashes"},
		{67, 3, true, 6, "3 hops, 2-byte hashes — the value seen in the field"},
		{0x41, 1, true, 2, "1 hop, 2-byte hashes"},
		{0x85, 5, true, 15, "5 hops, 3-byte hashes"},
	} {
		hops, known := DecodePathByte(tc.raw)
		if known != tc.known || (known && hops != tc.hops) {
			t.Errorf("%s: DecodePathByte(%d) = %d,%v; want %d,%v",
				tc.what, tc.raw, hops, known, tc.hops, tc.known)
		}
		if got := PathByteLen(tc.raw); got != tc.pathLen {
			t.Errorf("%s: PathByteLen(%d) = %d, want %d", tc.what, tc.raw, got, tc.pathLen)
		}
	}
}

// The message ceiling is MAX_TEXT_LEN from the firmware, not the 133 the
// carried-over notes claimed.
func TestMessageCeilingMatchesFirmware(t *testing.T) {
	// src/helpers/BaseChatMesh.h: MAX_TEXT_LEN = 10 * CIPHER_BLOCK_SIZE, and
	// src/MeshCore.h: CIPHER_BLOCK_SIZE = 16.
	if MaxMsgLen != 10*16 {
		t.Errorf("MaxMsgLen = %d, want 160 (10 * CIPHER_BLOCK_SIZE)", MaxMsgLen)
	}
	// And it must stay under what a packet can carry, which is the constraint
	// the firmware comment cites.
	if MaxMsgLen >= 184-4-2-1 {
		t.Errorf("MaxMsgLen = %d exceeds the packet payload budget", MaxMsgLen)
	}
}

// A CLI command is a direct message with one byte changed. That byte is the
// whole difference between chat and remote administration, so pin it down.
func TestEncodeSendCLICmdSetsTheCLITextType(t *testing.T) {
	prefix := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11}
	now := time.Unix(1786000000, 0)

	cli, err := EncodeSendCLICmd(prefix, "clock", now)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	plain, err := EncodeSendTxtMsg(prefix, "clock", now)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if cli[0] != CmdSendTxtMsg {
		t.Errorf("command byte = 0x%02X, want 0x%02X", cli[0], CmdSendTxtMsg)
	}
	if cli[1] != TxtTypeCLIData {
		t.Errorf("txt_type = %d, want %d (TxtTypeCLIData)", cli[1], TxtTypeCLIData)
	}
	if plain[1] != TxtTypePlain {
		t.Errorf("plain txt_type = %d, want %d", plain[1], TxtTypePlain)
	}
	// Everything else must be byte-identical, or the far node reads a
	// different recipient or timestamp than a normal message would carry.
	if !bytes.Equal(cli[2:], plain[2:]) {
		t.Errorf("frames differ beyond txt_type:\n cli   = %x\n plain = %x", cli[2:], plain[2:])
	}
}

func TestEncodeSendCLICmdCarriesRecipientAndText(t *testing.T) {
	prefix := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	f, err := EncodeSendCLICmd(prefix, "clock sync", time.Unix(1786000000, 0))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := binary.LittleEndian.Uint32(f[3:7]); got != 1786000000 {
		t.Errorf("timestamp = %d, want 1786000000", got)
	}
	if !bytes.Equal(f[7:13], prefix) {
		t.Errorf("recipient = %x, want %x", f[7:13], prefix)
	}
	if got := string(f[13:]); got != "clock sync" {
		t.Errorf("text = %q, want %q", got, "clock sync")
	}
}

// Seconds since the epoch, so a node is set to UTC no matter what zone the
// bridge machine is standing in. MeshCore has no timezone concept at all.
func TestEncodeSetDeviceTimeIsEpochSecondsNotLocalTime(t *testing.T) {
	mdt := time.FixedZone("MDT", -6*3600)
	instant := time.Unix(1786000000, 0)

	utc := EncodeSetDeviceTime(instant.UTC())
	local := EncodeSetDeviceTime(instant.In(mdt))

	if !bytes.Equal(utc, local) {
		t.Errorf("the same instant encoded differently by zone:\n utc   = %x\n local = %x", utc, local)
	}
	if got := binary.LittleEndian.Uint32(utc[1:5]); got != 1786000000 {
		t.Errorf("epoch = %d, want 1786000000", got)
	}
}

func TestDecodeCurrTime(t *testing.T) {
	f := []byte{RespCurrTime, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(f[1:], 1786000000)

	got, err := DecodeCurrTime(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Equal(time.Unix(1786000000, 0)) {
		t.Errorf("time = %v, want %v", got, time.Unix(1786000000, 0))
	}
	if _, err := DecodeCurrTime([]byte{RespCurrTime}); err != ErrShortFrame {
		t.Errorf("short frame err = %v, want ErrShortFrame", err)
	}
}

// Admin is the only role that may run CLI commands, and repeaters report it in
// two places at once: the legacy is_admin byte and the newer ACL byte. Older
// firmware fills in only the first, so both must count.
func TestLoginResultIsAdmin(t *testing.T) {
	cases := []struct {
		name string
		r    LoginResult
		want bool
	}{
		{"legacy is_admin only", LoginResult{Perms: 1}, true},
		{"acl admin", LoginResult{HasExtra: true, ACL: ACLAdmin}, true},
		{"both agree", LoginResult{Perms: 1, HasExtra: true, ACL: ACLAdmin}, true},
		{"guest", LoginResult{HasExtra: true, ACL: ACLGuest}, false},
		// May post, but may NOT administer. This is the case that matters:
		// the repeater accepts the login and then ignores every command.
		{"read-write is not admin", LoginResult{HasExtra: true, ACL: ACLReadWrite}, false},
		{"no role reported at all", LoginResult{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.r.IsAdmin(); got != c.want {
				t.Errorf("IsAdmin() = %v, want %v", got, c.want)
			}
		})
	}
}
