package meshcore

// Raw packet sniffing, and what it is good for.
//
// A companion node reports EVERY packet its radio hears to whatever client is
// attached, as PushLogRxData — no flag, no subscription, no way to ask it to
// stop (`Dispatcher.cpp:199` calls `logRxRaw` before the packet is even parsed,
// and `companion_radio/MyMesh.cpp:286` writes the frame whenever a client is
// connected). Those frames were arriving here already and being dropped; this
// file is what makes them useful.
//
// The one thing they can tell us that nothing else can: whether a channel
// message we sent was picked up and rebroadcast. MeshCore cannot acknowledge a
// group message, so a repeater's rebroadcast is the ONLY evidence a channel
// send ever reached anybody. Hearing our own packet come back is proof that a
// repeater took it; hearing nothing means the transmission went into the dark.
//
// This is exactly what the phone app's "Heard N repeats" line is, and it is
// computed the same way — by recognising our own packet in the firehose, not by
// asking the node anything.

import (
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"time"
)

// Payload types, from src/Packet.h:20. Only the ones this file reasons about.
const (
	PayloadTypeTxtMsg  = 0x02
	PayloadTypeAck     = 0x03
	PayloadTypeAdvert  = 0x04
	PayloadTypeGrpTxt  = 0x05 // channel hash, MAC, then enc(timestamp, "name: msg")
	PayloadTypeGrpData = 0x06
)

// Route types, from src/Packet.h:14. The two "transport" variants carry four
// extra header bytes, which is the only reason this matters here: get it wrong
// and the payload is read from four bytes into itself.
const (
	RouteTypeTransportFlood  = 0x00
	RouteTypeFlood           = 0x01
	RouteTypeDirect          = 0x02
	RouteTypeTransportDirect = 0x03
)

// GroupPrefixLen is what sits in front of the ciphertext of a group message:
// the channel hash (PATH_HASH_SIZE = 1) and the MAC (CIPHER_MAC_SIZE = 2).
// Both are from src/MeshCore.h.
const GroupPrefixLen = 1 + 2

// RawPacket is one packet the node's radio heard, exactly as it came off the
// air.
//
//	[0x88][snr x4 i8][rssi i8][header][transport codes 4?][path_len][path...][payload...]
//
// Path is the interesting field beyond the payload: on a flood, every repeater
// that passes the packet on appends its own hash, so the path is both the hop
// count and the identity of the route this copy took. Two rebroadcasts of the
// same message differ only there.
type RawPacket struct {
	SNR  float64
	RSSI int
	// Header is the raw first byte: route type, payload type, payload version.
	Header byte
	// Path is the accumulated repeater hashes, HashSize bytes each.
	Path     []byte
	HashSize int
	Payload  []byte
}

// RouteType is the low two bits of the header.
func (p RawPacket) RouteType() byte { return p.Header & 0x03 }

// PayloadType says what the packet IS: PayloadType* above.
func (p RawPacket) PayloadType() byte { return (p.Header >> 2) & 0x0F }

// IsFlood reports whether this copy is being flooded rather than routed down a
// stored path. Channel messages are always flooded.
func (p RawPacket) IsFlood() bool {
	return p.RouteType() == RouteTypeFlood || p.RouteType() == RouteTypeTransportFlood
}

// Hops is how many repeaters have handled this copy of the packet.
//
// Zero means it came straight from the sender. Anything above zero means we are
// hearing a rebroadcast — which, for a packet we sent ourselves, is the whole
// point of looking.
func (p RawPacket) Hops() int {
	if p.HashSize <= 0 {
		return 0
	}
	return len(p.Path) / p.HashSize
}

// GroupCipher is the encrypted part of a group message, with the channel hash
// and MAC stripped off. Empty for anything that is not a group message.
//
// This is the field that identifies a message: MeshCore encrypts group traffic
// with AES-128 in ECB mode and no IV (`Utils.cpp` encrypt, called from
// `Mesh.cpp:553`), so the same plaintext under the same channel key always
// produces the same bytes. Deterministic encryption is a real weakness of the
// protocol and it is what makes this feature possible at all.
func (p RawPacket) GroupCipher() []byte {
	if p.PayloadType() != PayloadTypeGrpTxt || len(p.Payload) <= GroupPrefixLen {
		return nil
	}
	return p.Payload[GroupPrefixLen:]
}

// PathKey identifies the route this copy took, for counting distinct
// rebroadcasts rather than raw frames.
func (p RawPacket) PathKey() string { return string(p.Path) }

// DecodeLogRxData parses PushLogRxData into the packet it carries.
//
// A parse failure here is ordinary, not alarming: the node reports what the
// radio handed it, including corrupt frames and future payload versions it
// cannot read itself, and every one of those arrives on this path.
func DecodeLogRxData(f []byte) (RawPacket, error) {
	var p RawPacket
	if len(f) < 4 || f[0] != PushLogRxData {
		return p, ErrShortFrame
	}
	// SNR is scaled by 4 and both fields are signed (MyMesh.cpp:290).
	p.SNR = float64(int8(f[1])) / 4
	p.RSSI = int(int8(f[2]))

	raw := f[3:]
	i := 0
	p.Header = raw[i]
	i++
	if ver := (p.Header >> 6) & 0x03; ver != 0 {
		// PAYLOAD_VER_1 is the only version defined. The firmware refuses
		// anything higher (Dispatcher.cpp:152) and so must we: a later version
		// may lay the header out differently, and guessing would mis-read the
		// payload rather than fail.
		return p, fmt.Errorf("meshcore: unsupported packet version %d", ver)
	}
	if rt := p.RouteType(); rt == RouteTypeTransportFlood || rt == RouteTypeTransportDirect {
		if i+4 > len(raw) {
			return p, ErrShortFrame
		}
		i += 4 // two uint16 transport codes, not needed here
	}
	if i >= len(raw) {
		return p, ErrShortFrame
	}
	pathLen := raw[i]
	i++
	// The upper two bits are the hash SIZE minus one, not part of the count.
	// Legacy firmware sends 00 here, so this is one byte per hop in practice.
	p.HashSize = int(pathLen>>6) + 1
	if mode := pathLen >> 6; mode == 3 {
		return p, fmt.Errorf("meshcore: unsupported path mode 3")
	}
	pathBytes := int(pathLen&63) * p.HashSize
	if pathBytes > MaxPathSize || i+pathBytes > len(raw) {
		return p, ErrShortFrame
	}
	p.Path = raw[i : i+pathBytes]
	i += pathBytes
	p.Payload = raw[i:]
	return p, nil
}

// GroupTextCipher recomputes the ciphertext the node will have put on the air
// for a group message, so we can recognise it coming back.
//
// Every input is known locally, which is the only reason this works: the
// timestamp is the one we chose, the name is the node's own, the key is the
// channel secret the node gave us, and the cipher has no IV or nonce to guess.
//
// The plaintext layout is from BaseChatMesh.cpp:487 and has to be reproduced to
// the byte:
//
//	[timestamp u32 LE][0x00 = TXT_TYPE_PLAIN]["<name>: <text>"]
//
// including the firmware's truncation, which cuts at MAX_TEXT_LEN with no
// regard for UTF-8 boundaries and without telling anyone. Matching the padding
// matters too: encrypt() zero-pads the last block to 16 bytes.
func GroupTextCipher(secret []byte, ts time.Time, nodeName, text string) ([]byte, error) {
	if len(secret) != ChannelSecretSize {
		return nil, fmt.Errorf("meshcore: a channel secret is %d bytes, got %d", ChannelSecretSize, len(secret))
	}
	body := GroupTextBody(nodeName, text)
	plain := make([]byte, 0, 5+len(body))
	plain = binary.LittleEndian.AppendUint32(plain, uint32(ts.Unix()))
	plain = append(plain, TxtTypePlain)
	plain = append(plain, body...)

	// Zero-pad the tail block, exactly as the firmware does.
	if n := len(plain) % aes.BlockSize; n != 0 {
		plain = append(plain, make([]byte, aes.BlockSize-n)...)
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(plain))
	// ECB: every block independently, which is the whole weakness being
	// exploited here. Go's standard library deliberately offers no ECB mode.
	for off := 0; off < len(plain); off += aes.BlockSize {
		block.Encrypt(out[off:off+aes.BlockSize], plain[off:off+aes.BlockSize])
	}
	return out, nil
}

// GroupTextBody is the "<name>: <text>" the node builds and then truncates.
//
// The truncation is a byte cut at MAX_TEXT_LEN (BaseChatMesh.cpp:496) applied
// to the text only, after the prefix is accounted for. It can split a
// multi-byte character in half; reproducing that faithfully is the point, since
// a byte of difference means the ciphertext will not match.
func GroupTextBody(nodeName, text string) string {
	prefix := nodeName + ": "
	if len(prefix) >= MaxMsgLen {
		// A name this long leaves no room for text at all. The firmware would
		// compute a negative length here; there is nothing sensible to
		// reproduce, so give up rather than guess.
		return prefix
	}
	if len(text)+len(prefix) > MaxMsgLen {
		text = text[:MaxMsgLen-len(prefix)]
	}
	return prefix + text
}
