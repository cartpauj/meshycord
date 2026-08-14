package meshcore

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// A flooded group text message, as the node reports one it heard.
func TestDecodeLogRxDataFlood(t *testing.T) {
	cipher := bytes.Repeat([]byte{0x5A}, 16)
	raw := []byte{RouteTypeFlood | (PayloadTypeGrpTxt << 2), 0x02, 0xAA, 0xBB, 0x7F, 0x11, 0x22}
	raw = append(raw, cipher...)
	// path_len 0x02 above is two one-byte hashes: 0xAA and 0xBB.

	snr, rssi := int8(-24), int8(-110)
	frame := append([]byte{PushLogRxData, byte(snr), byte(rssi)}, raw...)
	p, err := DecodeLogRxData(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.PayloadType() != PayloadTypeGrpTxt {
		t.Errorf("payload type = 0x%02X", p.PayloadType())
	}
	if !p.IsFlood() {
		t.Error("a flooded packet did not report as flooded")
	}
	if p.Hops() != 2 {
		t.Errorf("hops = %d, want 2", p.Hops())
	}
	if got := p.SNR; got != -6 {
		t.Errorf("snr = %v, want -6 (the wire value is scaled by 4)", got)
	}
	if p.RSSI != -110 {
		t.Errorf("rssi = %d, want -110", p.RSSI)
	}
	if !bytes.Equal(p.GroupCipher(), cipher) {
		t.Errorf("cipher = % x, want % x", p.GroupCipher(), cipher)
	}
}

// The two transport route types carry four extra header bytes. Reading past
// them is not optional: get it wrong and every field after is shifted, which
// would silently compare the wrong bytes rather than fail.
func TestDecodeLogRxDataSkipsTransportCodes(t *testing.T) {
	cipher := bytes.Repeat([]byte{0x3C}, 16)
	raw := []byte{RouteTypeTransportFlood | (PayloadTypeGrpTxt << 2),
		0x01, 0x00, 0x02, 0x00, // two uint16 transport codes
		0x01, 0xC1, // one hop
		0x7F, 0x11, 0x22, // channel hash, MAC
	}
	raw = append(raw, cipher...)

	p, err := DecodeLogRxData(append([]byte{PushLogRxData, 0, 0}, raw...))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Hops() != 1 {
		t.Fatalf("hops = %d, want 1", p.Hops())
	}
	if !bytes.Equal(p.GroupCipher(), cipher) {
		t.Errorf("cipher = % x, want % x", p.GroupCipher(), cipher)
	}
}

// The radio hands the node corrupt and unreadable frames, and every one of them
// arrives on this path. None may panic.
func TestDecodeLogRxDataRejectsRubbish(t *testing.T) {
	cases := map[string][]byte{
		"empty":              {},
		"wrong code":         {PushAdvert, 1, 2, 3},
		"header only":        {PushLogRxData, 0, 0},
		"future version":     {PushLogRxData, 0, 0, 0xC0 | RouteTypeFlood, 0x00},
		"path mode 3":        {PushLogRxData, 0, 0, RouteTypeFlood, 0xC1, 0x01},
		"path runs off end":  {PushLogRxData, 0, 0, RouteTypeFlood, 0x08, 0x01},
		"truncated codes":    {PushLogRxData, 0, 0, RouteTypeTransportFlood, 0x01},
		"no path length":     {PushLogRxData, 0, 0, RouteTypeFlood},
		"non-group is fine":  {PushLogRxData, 0, 0, RouteTypeFlood | (PayloadTypeAck << 2), 0x00, 0x01},
		"group with no body": {PushLogRxData, 0, 0, RouteTypeFlood | (PayloadTypeGrpTxt << 2), 0x00, 0x7F},
	}
	for name, f := range cases {
		p, err := DecodeLogRxData(f)
		if err == nil && len(p.GroupCipher()) != 0 {
			t.Errorf("%s: produced a cipher from %d bytes", name, len(f))
		}
	}
}

// The fingerprint has to be byte-identical to what the firmware transmits, so
// the plaintext layout is asserted explicitly rather than trusted.
//
// Layout from BaseChatMesh.cpp:487 — timestamp, TXT_TYPE_PLAIN, "name: text" —
// encrypted with AES-128 in ECB mode and zero-padded to the block size.
func TestGroupTextCipherMatchesTheFirmwareLayout(t *testing.T) {
	secret := bytes.Repeat([]byte{0x11}, ChannelSecretSize)
	ts := time.Unix(1750000000, 0)

	got, err := GroupTextCipher(secret, ts, "Base", "hello mesh")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	if len(got)%aes.BlockSize != 0 {
		t.Fatalf("cipher length %d is not a whole number of blocks", len(got))
	}

	// Decrypt it back and check the bytes the node would have encrypted.
	block, err := aes.NewCipher(secret)
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, len(got))
	for off := 0; off < len(got); off += aes.BlockSize {
		block.Decrypt(plain[off:off+aes.BlockSize], got[off:off+aes.BlockSize])
	}
	if ts := binary.LittleEndian.Uint32(plain[0:4]); ts != 1750000000 {
		t.Errorf("timestamp = %d", ts)
	}
	if plain[4] != TxtTypePlain {
		t.Errorf("text type = %d, want %d", plain[4], TxtTypePlain)
	}
	if body := string(plain[5 : 5+len("Base: hello mesh")]); body != "Base: hello mesh" {
		t.Errorf("body = %q", body)
	}
	for i, b := range plain[5+len("Base: hello mesh"):] {
		if b != 0 {
			t.Errorf("padding byte %d is 0x%02X, want zero", i, b)
		}
	}
}

// Same message, same second, same key: the same bytes on the air. That is what
// makes a repeat recognisable, and it is why a different timestamp must produce
// a different fingerprint.
func TestGroupTextCipherIsDeterministicButTimestampSpecific(t *testing.T) {
	secret := bytes.Repeat([]byte{0x22}, ChannelSecretSize)
	ts := time.Unix(1750000000, 0)

	a, _ := GroupTextCipher(secret, ts, "Base", "same")
	b, _ := GroupTextCipher(secret, ts, "Base", "same")
	if !bytes.Equal(a, b) {
		t.Fatal("the same message encrypted differently twice; no repeat could ever be matched")
	}
	c, _ := GroupTextCipher(secret, ts.Add(time.Second), "Base", "same")
	if bytes.Equal(a, c) {
		t.Error("a different second produced the same fingerprint")
	}
	d, _ := GroupTextCipher(secret, ts, "Other", "same")
	if bytes.Equal(a, d) {
		t.Error("a different node name produced the same fingerprint")
	}
	if _, err := GroupTextCipher(secret[:8], ts, "Base", "x"); err == nil {
		t.Error("a short key was accepted")
	}
}

// The node truncates "name: text" at MAX_TEXT_LEN with a byte cut, splitting
// multi-byte characters if it comes to that. Reproducing the cut exactly is the
// difference between matching the packet and not.
func TestGroupTextBodyTruncatesLikeTheFirmware(t *testing.T) {
	body := GroupTextBody("Base", strings.Repeat("x", 400))
	if len(body) != MaxMsgLen {
		t.Errorf("body length = %d, want %d", len(body), MaxMsgLen)
	}
	if !strings.HasPrefix(body, "Base: ") {
		t.Errorf("body lost its prefix: %q", body[:10])
	}
	if got := GroupTextBody("Base", "short"); got != "Base: short" {
		t.Errorf("body = %q", got)
	}
	// A name long enough to leave no room is a firmware bug, not something to
	// reproduce; it just must not panic or produce a negative length.
	if got := GroupTextBody(strings.Repeat("n", 200), "text"); !strings.HasPrefix(got, "nnn") {
		t.Errorf("pathological name mishandled: %q", got)
	}
}
