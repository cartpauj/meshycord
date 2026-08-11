package meshcore

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrShortFrame means the node sent fewer bytes than the layout requires.
// Every decoder here is length-guarded: a truncated or unexpected frame
// produces an error, never a panic and never a half-filled struct.
var ErrShortFrame = errors.New("meshcore: frame too short")

// ---------------------------------------------------------------------------
// Received messages
// ---------------------------------------------------------------------------

// Message is one text message pulled off the node with CmdSyncNextMessage.
type Message struct {
	// IsChannel separates a group/channel message from a direct one.
	IsChannel  bool
	ChannelIdx byte // valid when IsChannel

	// PubKeyPrefix is the sender's first 6 key bytes as 12 lowercase hex
	// characters. 48 bits: fine for routing, NOT proof of identity.
	PubKeyPrefix string

	// AuthorPrefix is set only for txt_type == 2 messages. The companion docs
	// call those 4 bytes a signature; they are not. They are the ORIGINAL
	// author's public-key prefix — how a room server says who wrote a post.
	AuthorPrefix string

	TxtType byte

	HaveSNR bool
	SNR     float64 // dB; the wire carries it as a signed byte times 4

	// PathRaw is the node's path byte verbatim. It is NOT a hop count:
	// bits 0-5 hold the hop count and bits 6-7 the per-hop hash size
	// (Packet.h:79), and the whole byte is 0xFF for a direct route. Printing
	// it raw showed "71 hops" for what was actually 7.
	PathRaw  byte
	HaveHops bool // false when the packet came by a stored path
	Hops     byte // decoded hop count, meaningful when HaveHops

	Timestamp time.Time
	Text      string
}

// DecodeMessage parses one of the four message frames. 0x10/0x11 are the V3
// forms: identical payloads with a leading signed SNR byte and 2 reserved
// bytes. Both must be parsed — a node can emit either.
func DecodeMessage(f []byte) (Message, error) {
	var m Message
	if len(f) < 1 {
		return m, ErrShortFrame
	}
	code := f[0]

	var isContact, isChannel, v3 bool
	switch code {
	case RespContactMsgRecv:
		isContact = true
	case RespContactMsgRecvV3:
		isContact, v3 = true, true
	case RespChannelMsgRecv:
		isChannel = true
	case RespChannelMsgRecvV3:
		isChannel, v3 = true, true
	default:
		return m, fmt.Errorf("meshcore: 0x%02X is not a message frame", code)
	}

	off := 1
	if v3 {
		if len(f) < off+3 {
			return m, ErrShortFrame
		}
		m.HaveSNR = true
		m.SNR = float64(int8(f[off])) / 4.0
		off += 3 // SNR + 2 reserved
	}

	m.IsChannel = isChannel
	if isChannel {
		if len(f) < off+1 {
			return m, ErrShortFrame
		}
		m.ChannelIdx = f[off]
		off++
	} else if isContact {
		if len(f) < off+6 {
			return m, ErrShortFrame
		}
		m.PubKeyPrefix = hex.EncodeToString(f[off : off+6])
		off += 6
	}

	if len(f) < off+2 {
		return m, ErrShortFrame
	}
	m.PathRaw = f[off]
	if m.PathRaw == 0xFF {
		m.HaveHops = false // direct route; a hop count does not apply
	} else {
		m.HaveHops = true
		m.Hops = m.PathRaw & 63 // Packet.h:80 getPathHashCount()
	}
	m.TxtType = f[off+1]
	off += 2

	if len(f) < off+4 {
		return m, ErrShortFrame
	}
	secs := binary.LittleEndian.Uint32(f[off : off+4])
	m.Timestamp = time.Unix(int64(secs), 0)
	off += 4

	if m.TxtType == 2 {
		// Not a signature: the original author's 4-byte key prefix.
		if len(f) >= off+4 {
			m.AuthorPrefix = hex.EncodeToString(f[off : off+4])
		}
		off += 4
	}

	if off > len(f) {
		return m, ErrShortFrame
	}
	m.Text = strings.TrimRight(string(f[off:]), "\x00")
	return m, nil
}

// ---------------------------------------------------------------------------
// Contacts
// ---------------------------------------------------------------------------

// Contact is one entry in the node's contact list.
type Contact struct {
	PubKey [PubKeySize]byte
	Type   byte
	Flags  byte
	// OutPathLen is the stored hop list length, 0xFF when no path is known.
	// Treated as "hops" for display and sorting.
	OutPathLen byte
	// OutPath is the stored hop list itself. Kept because updating a contact
	// means resending the whole record: without it, renaming someone would
	// silently wipe their path and force the next message to flood.
	OutPath    [MaxPathSize]byte
	Name       string
	LastAdvert time.Time
	Lat, Lon   float64
}

// DecodePathByte unpacks a MeshCore path byte into a hop count.
//
// It is NOT a length. Packet.cpp:20 is the authority:
//
//	hash_count = path_len & 63
//	hash_size  = (path_len >> 6) + 1
//
// so the stored byte 67 means three hops recorded with two-byte hashes, not
// "67". 0xFF is the sentinel for no known path (OUT_PATH_UNKNOWN). The same
// packing applies to a contact's out_path_len and to a received packet's path
// byte — the ESP32 decoded it for messages and then printed it raw for
// contacts, which is where "67 hops" came from.
func DecodePathByte(b byte) (hops byte, known bool) {
	if b == 0xFF {
		return 0, false
	}
	return b & 63, true
}

// PathByteLen is how many bytes of stored path the byte describes.
func PathByteLen(b byte) int {
	if b == 0xFF {
		return 0
	}
	return int(b&63) * int((b>>6)+1)
}

// Prefix is the 12-hex-character routing key for this contact.
func (c Contact) Prefix() string { return hex.EncodeToString(c.PubKey[:6]) }

// PubKeyHex is the full 64-character key. Several commands require it and
// cannot work from the 6-byte prefix a message arrives with.
func (c Contact) PubKeyHex() string { return hex.EncodeToString(c.PubKey[:]) }

// Contact record layout (MyMesh.cpp:166 writeContactRespFrame):
//
//	[code][pub_key 32][type 1][flags 1][out_path_len 1][out_path 64]
//	[name 32][last_advert 4][gps_lat 4][gps_lon 4][lastmod 4]
const (
	contactKeyOff     = 1
	contactTypeOff    = 1 + PubKeySize                      // 33
	contactFlagsOff   = contactTypeOff + 1                  // 34
	contactPathLenOff = contactFlagsOff + 1                 // 35
	contactNameOff    = contactPathLenOff + 1 + MaxPathSize // 100
	contactAdvertOff  = contactNameOff + 32                 // 132
	contactLatOff     = contactAdvertOff + 4                // 136
	contactLonOff     = contactLatOff + 4                   // 140
	contactMinLen     = contactNameOff                      // enough to be useful
)

// DecodeContact parses a RespContact frame. Fields past the name are optional:
// older firmware stops after last_advert, so everything beyond contactNameOff
// is read only when the frame is long enough to hold it.
func DecodeContact(f []byte) (Contact, error) {
	var c Contact
	if len(f) < contactMinLen {
		return c, ErrShortFrame
	}
	copy(c.PubKey[:], f[contactKeyOff:contactKeyOff+PubKeySize])
	c.Type = f[contactTypeOff]
	c.Flags = f[contactFlagsOff]
	c.OutPathLen = f[contactPathLenOff]
	copy(c.OutPath[:], f[contactPathLenOff+1:contactNameOff])

	// The name is a 32-byte NUL-padded field. Read all 32, then trim to a
	// UTF-8 boundary so a truncated emoji never becomes invalid bytes.
	end := contactNameOff + 32
	if end > len(f) {
		end = len(f)
	}
	c.Name = cleanString(f[contactNameOff:end])

	if len(f) >= contactAdvertOff+4 {
		if secs := binary.LittleEndian.Uint32(f[contactAdvertOff : contactAdvertOff+4]); secs != 0 {
			c.LastAdvert = time.Unix(int64(secs), 0)
		}
	}
	if len(f) >= contactLonOff+4 {
		// MeshCore stores coordinates as signed micro-degrees.
		c.Lat = float64(int32(binary.LittleEndian.Uint32(f[contactLatOff:contactLatOff+4]))) / 1e6
		c.Lon = float64(int32(binary.LittleEndian.Uint32(f[contactLonOff:contactLonOff+4]))) / 1e6
	}
	return c, nil
}

// EncodeAddUpdateContact builds CmdAddUpdateContact.
//
//	[0x09][pubkey 32][type][flags][out_path_len][out_path 64][name 32][last_advert 4]
//
// out_path_len is 0xFF — "no known path" — so the node floods until it learns
// one. This is how a node seen on the public map, whose adverts never reach
// you, gets added.
func EncodeAddUpdateContact(pubKey []byte, name string, advType byte) ([]byte, error) {
	return EncodeContactRecord(pubKey, name, advType, 0xFF, nil)
}

// EncodeContactRecord builds CmdAddUpdateContact with an explicit stored path.
//
// The same command both adds and updates, so an update has to resend the whole
// record — including the path. Passing 0xFF and no path means "no known route,
// flood until you learn one", which is right for a brand new contact and wrong
// for a rename: it would throw away a working path and cost the next message a
// flood across the whole mesh.
func EncodeContactRecord(pubKey []byte, name string, advType, outPathLen byte, outPath []byte) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, fmt.Errorf("meshcore: contact needs a full %d-byte key, got %d", PubKeySize, len(pubKey))
	}
	if len(outPath) > MaxPathSize {
		return nil, fmt.Errorf("meshcore: a stored path is at most %d bytes, got %d", MaxPathSize, len(outPath))
	}
	buf := make([]byte, 1+PubKeySize+1+1+1+MaxPathSize+32+4)
	buf[0] = CmdAddUpdateContact
	copy(buf[1:], pubKey)
	o := 1 + PubKeySize
	buf[o] = advType
	buf[o+1] = 0 // flags
	buf[o+2] = outPathLen
	copy(buf[o+3:o+3+MaxPathSize], outPath)
	o += 3 + MaxPathSize
	copy(buf[o:o+32], truncateUTF8(name, 31))
	return buf, nil
}

// EncodeRemoveContact builds CmdRemoveContact, which needs the full key.
func EncodeRemoveContact(pubKey []byte) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, fmt.Errorf("meshcore: remove needs a full %d-byte key, got %d", PubKeySize, len(pubKey))
	}
	return append([]byte{CmdRemoveContact}, pubKey...), nil
}

// EncodeGetContacts builds CmdGetContacts. A non-zero `since` asks for an
// incremental sync; zero asks for the whole list.
func EncodeGetContacts(since time.Time) []byte {
	if since.IsZero() {
		return []byte{CmdGetContacts}
	}
	buf := make([]byte, 5)
	buf[0] = CmdGetContacts
	binary.LittleEndian.PutUint32(buf[1:], uint32(since.Unix()))
	return buf
}

// EncodeGetContactByKey builds CmdGetContactByKey. Needs the full 32-byte key
// — it cannot resolve the 6-byte prefix a message arrives with, which is why
// the client keeps a contact cache instead.
func EncodeGetContactByKey(pubKey []byte) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, fmt.Errorf("meshcore: lookup needs a full %d-byte key, got %d", PubKeySize, len(pubKey))
	}
	return append([]byte{CmdGetContactByKey}, pubKey...), nil
}

// EncodeResetPath builds CmdResetPath, forgetting a contact's stored route.
//
// There is no per-message route flag in the companion protocol. The node picks
// flood when a contact has no stored path and direct otherwise
// (BaseChatMesh.cpp:449), so clearing the path is the only way to force a
// flood. It is relearned from the reply, so the effect is for this message
// rather than permanent.
func EncodeResetPath(pubKey []byte) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, fmt.Errorf("meshcore: reset path needs a full %d-byte key, got %d", PubKeySize, len(pubKey))
	}
	return append([]byte{CmdResetPath}, pubKey...), nil
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// SendResult is the RespSent reply to a DM or a login.
//
//	[0x06][flood flag][expected_ack u32][est_timeout_ms u32]
//
// For a login, "sent" means only that the request went out over the air. The
// verdict arrives later as a PushLoginSuccess / PushLoginFail.
type SendResult struct {
	Flooded     bool          // the node used a flood, not a stored path
	ExpectedAck uint32        // handle a later PushSendConfirmed will carry
	EstTimeout  time.Duration // the node's own estimate of delivery time
}

// DecodeSendResult parses RespSent. A short frame is tolerated: older
// firmware answers with the code alone, which simply means no ack handle.
func DecodeSendResult(f []byte) (SendResult, error) {
	var r SendResult
	if len(f) < 1 || f[0] != RespSent {
		return r, fmt.Errorf("meshcore: expected RespSent, got 0x%02X", firstByte(f))
	}
	if len(f) >= 2 {
		r.Flooded = f[1] == 1
	}
	if len(f) >= 10 {
		r.ExpectedAck = binary.LittleEndian.Uint32(f[2:6])
		r.EstTimeout = time.Duration(binary.LittleEndian.Uint32(f[6:10])) * time.Millisecond
	}
	return r, nil
}

// EncodeSendTxtMsg builds CmdSendTxtMsg (a direct message).
//
//	[0x02][txt_type][attempt][sender_timestamp u32][pubkey_prefix 6][text...]
//
// Only the first 6 key bytes go on the wire, which is why a DM can be
// addressed to someone who is not in the contact list at all.
func EncodeSendTxtMsg(prefix []byte, text string, now time.Time) ([]byte, error) {
	if len(prefix) < 6 {
		return nil, fmt.Errorf("meshcore: need at least 6 key bytes to address a message, got %d", len(prefix))
	}
	body := clampToLimit(text)
	buf := make([]byte, 0, 1+1+1+4+6+len(body))
	buf = append(buf, CmdSendTxtMsg, 0 /*txt_type: plain*/, 0 /*attempt*/)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(now.Unix()))
	buf = append(buf, prefix[:6]...)
	buf = append(buf, body...)
	return buf, nil
}

// EncodeSendChannelTxtMsg builds CmdSendChannelTxtMsg (a group message).
//
//	[0x03][txt_type][channel_idx][sender_timestamp u32][text...]
//
// Group messages cannot be acknowledged: the node answers with a plain OK and
// no ack handle, so delivery is never confirmable for these.
func EncodeSendChannelTxtMsg(idx byte, text string, now time.Time) []byte {
	body := clampToLimit(text)
	buf := make([]byte, 0, 1+1+1+4+len(body))
	buf = append(buf, CmdSendChannelTxtMsg, 0 /*txt_type: plain*/, idx)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(now.Unix()))
	buf = append(buf, body...)
	return buf
}

// EncodeSendLogin builds CmdSendLogin.
//
//	[0x1A][pubkey 32][password...]
//
// Needs the FULL key (MyMesh.cpp:1524 checks len >= 1 + PUB_KEY_SIZE), so the
// caller must resolve the prefix through the contact cache first.
func EncodeSendLogin(pubKey []byte, password string) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, fmt.Errorf("meshcore: login needs a full %d-byte key, got %d", PubKeySize, len(pubKey))
	}
	if len(password) > 63 {
		password = password[:63]
	}
	buf := append([]byte{CmdSendLogin}, pubKey...)
	return append(buf, password...), nil
}

// EncodeLogout builds CmdLogout for a room server.
func EncodeLogout(pubKey []byte) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, fmt.Errorf("meshcore: logout needs a full %d-byte key, got %d", PubKeySize, len(pubKey))
	}
	return append([]byte{CmdLogout}, pubKey...), nil
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

// ChannelInfo is one of the node's 8 channel slots.
//
//	[0x12][channel_idx][name 32][secret 16]
type ChannelInfo struct {
	Index  byte
	Name   string
	Secret [ChannelSecretSize]byte
}

// DecodeChannelInfo parses RespChannelInfo.
//
// The caller MUST check that Index matches what it asked for. A timed-out
// query for slot N answered during the query for slot N+1 gave slot N+1 the
// wrong name, and produced a duplicate Discord channel for a slot that was
// never real.
func DecodeChannelInfo(f []byte) (ChannelInfo, error) {
	var c ChannelInfo
	if len(f) < 2 || f[0] != RespChannelInfo {
		return c, fmt.Errorf("meshcore: expected RespChannelInfo, got 0x%02X", firstByte(f))
	}
	c.Index = f[1]
	end := 2 + 32
	if end > len(f) {
		end = len(f)
	}
	c.Name = cleanString(f[2:end])
	if len(f) >= 2+32+ChannelSecretSize {
		copy(c.Secret[:], f[2+32:2+32+ChannelSecretSize])
	}
	return c, nil
}

// EncodeGetChannel builds CmdGetChannel for one slot.
func EncodeGetChannel(idx byte) []byte { return []byte{CmdGetChannel, idx} }

// EncodeSetChannel builds CmdSetChannel, which creates or updates a channel on
// the node.
//
//	[0x20][channel_idx][name 32][secret 16]
//
// An empty name with an all-zero secret deletes the slot. The ESP32 version
// never used this; it means private channels can be added without the phone
// app.
func EncodeSetChannel(idx byte, name string, secret []byte) ([]byte, error) {
	if len(secret) != 0 && len(secret) != ChannelSecretSize {
		return nil, fmt.Errorf("meshcore: a channel secret is %d bytes, got %d", ChannelSecretSize, len(secret))
	}
	buf := make([]byte, 2+32+ChannelSecretSize)
	buf[0] = CmdSetChannel
	buf[1] = idx
	copy(buf[2:2+32], truncateUTF8(name, 31))
	copy(buf[2+32:], secret)
	return buf, nil
}

// ---------------------------------------------------------------------------
// Pushes
// ---------------------------------------------------------------------------

// Confirmation is PushSendConfirmed: a DM we sent was delivered.
//
//	[0x82][ack u32][round-trip ms u32]
type Confirmation struct {
	Ack       uint32
	RoundTrip time.Duration
}

// DecodeConfirmation parses PushSendConfirmed.
func DecodeConfirmation(f []byte) (Confirmation, error) {
	var c Confirmation
	if len(f) < 9 {
		return c, ErrShortFrame
	}
	c.Ack = binary.LittleEndian.Uint32(f[1:5])
	c.RoundTrip = time.Duration(binary.LittleEndian.Uint32(f[5:9])) * time.Millisecond
	return c, nil
}

// LoginResult is the verdict on a room-server login, which arrives as a push
// rather than as the CmdSendLogin reply.
//
//	0x85 [perms 1][pubkey prefix 6][server time 4][acl 1][fw level 1]
//	0x86 [reserved 1][pubkey prefix 6]
//
// Both carry the prefix at the same offset, which is what names the room.
type LoginResult struct {
	Prefix string
	OK     bool
	// Perms is the LEGACY is_admin flag, and is 0 for any non-admin account —
	// it says nothing about whether you may post.
	Perms byte
	// ACL is the field that decides everything (companion MyMesh.cpp:701,
	// "NEW (v7): ACL permissions"). A guest is admitted and then has every
	// post silently discarded, with the acknowledgement deliberately withheld
	// (simple_room_server/MyMesh.cpp:481), which is indistinguishable from a
	// delivery failure unless you read this byte.
	ACL      byte
	FwLevel  byte
	HasExtra bool
	// ServerTime is the room's own clock at login.
	ServerTime time.Time
}

// Room-server ACL roles, from src/helpers/ClientACL.h.
const (
	ACLGuest     = 0 // admitted, but posts are discarded and never acknowledged
	ACLReadOnly  = 1
	ACLReadWrite = 2 // the room password; may post
	ACLAdmin     = 3 // the admin password
	ACLRoleMask  = 3 // lower 2 bits
)

// Role is the ACL role this login was granted.
func (r LoginResult) Role() byte { return r.ACL & ACLRoleMask }

// MayPost reports whether the room will accept posts from this session.
//
// A guest may not, and is told nothing about it — the room simply drops the
// post and withholds the acknowledgement.
func (r LoginResult) MayPost() bool {
	if !r.HasExtra {
		return true // older firmware does not report a role; assume the best
	}
	return r.Role() >= ACLReadWrite
}

// RoleName renders the granted role for humans.
func RoleName(role byte) string {
	switch role & ACLRoleMask {
	case ACLGuest:
		return "guest (read-only, cannot post)"
	case ACLReadOnly:
		return "read-only"
	case ACLReadWrite:
		return "read/write"
	case ACLAdmin:
		return "admin"
	}
	return "unknown"
}

// DecodeLoginResult parses PushLoginSuccess / PushLoginFail.
func DecodeLoginResult(f []byte) (LoginResult, error) {
	var r LoginResult
	if len(f) < 8 {
		return r, ErrShortFrame
	}
	r.OK = f[0] == PushLoginSuccess
	r.Perms = f[1]
	r.Prefix = hex.EncodeToString(f[2:8])
	// The success frame carries more: [server time u32][acl][fw level].
	if r.OK && len(f) >= 14 {
		if secs := binary.LittleEndian.Uint32(f[8:12]); secs != 0 {
			r.ServerTime = time.Unix(int64(secs), 0)
		}
		r.ACL, r.FwLevel, r.HasExtra = f[12], f[13], true
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Handshake and device info
// ---------------------------------------------------------------------------

// EncodeAppStart builds CmdAppStart: [0x01][7 reserved][app name].
//
// The length is derived from the name. Hardcoding it silently truncated the
// name to "meshyc" after the app was renamed.
func EncodeAppStart(appName string) []byte {
	buf := make([]byte, 8, 8+len(appName))
	buf[0] = CmdAppStart
	return append(buf, appName...)
}

// EncodeDeviceQuery builds CmdDeviceQuery.
func EncodeDeviceQuery() []byte { return []byte{CmdDeviceQuery} }

// EncodeSetDeviceTime builds CmdSetDeviceTime. A node with no RTC boots at the
// epoch, which makes every message timestamp useless until something sets it.
func EncodeSetDeviceTime(t time.Time) []byte {
	buf := make([]byte, 5)
	buf[0] = CmdSetDeviceTime
	binary.LittleEndian.PutUint32(buf[1:], uint32(t.Unix()))
	return buf
}

// EncodeSyncNextMessage builds CmdSyncNextMessage.
func EncodeSyncNextMessage() []byte { return []byte{CmdSyncNextMessage} }

// SelfInfo is the node's own identity, from the RespSelfInfo answer to
// CmdAppStart.
//
// Layout below matches the official meshcore_py / meshcore.js clients:
//
//	[0x05][adv_type][tx_power][max_tx_power][public_key 32][adv_lat i32]
//	[adv_lon i32][reserved 4][radio_freq u32][radio_bw u32][radio_sf][radio_cr][name...]
//
// Every field is length-guarded, so firmware that lays this out differently
// costs empty fields on a status page rather than a bad parse.
type SelfInfo struct {
	AdvType    byte
	TxPower    byte
	MaxTxPower byte
	PubKey     [PubKeySize]byte
	Lat, Lon   float64
	FreqKHz    uint32
	BandwidthK uint32
	SpreadFact byte
	CodingRate byte
	Name       string
}

// PubKeyHex is the node's own full public key.
func (s SelfInfo) PubKeyHex() string { return hex.EncodeToString(s.PubKey[:]) }

// DecodeSelfInfo parses RespSelfInfo.
func DecodeSelfInfo(f []byte) (SelfInfo, error) {
	var s SelfInfo
	if len(f) < 4+PubKeySize {
		return s, ErrShortFrame
	}
	s.AdvType, s.TxPower, s.MaxTxPower = f[1], f[2], f[3]
	copy(s.PubKey[:], f[4:4+PubKeySize])
	o := 4 + PubKeySize // 36
	if len(f) >= o+8 {
		s.Lat = float64(int32(binary.LittleEndian.Uint32(f[o:o+4]))) / 1e6
		s.Lon = float64(int32(binary.LittleEndian.Uint32(f[o+4:o+8]))) / 1e6
	}
	if len(f) >= 58 {
		s.FreqKHz = binary.LittleEndian.Uint32(f[48:52])
		s.BandwidthK = binary.LittleEndian.Uint32(f[52:56])
		s.SpreadFact = f[56]
		s.CodingRate = f[57]
	}
	if len(f) > 58 {
		s.Name = cleanString(f[58:])
	}
	return s, nil
}

// DeviceInfo is the answer to CmdDeviceQuery.
//
//	[0x0D][firmware_ver][reserved 6][fw_build_date 12][firmware name...]
type DeviceInfo struct {
	FirmwareVer byte
	BuildDate   string
	Firmware    string
}

// DecodeDeviceInfo parses RespDeviceInfo.
func DecodeDeviceInfo(f []byte) (DeviceInfo, error) {
	var d DeviceInfo
	if len(f) < 2 {
		return d, ErrShortFrame
	}
	d.FirmwareVer = f[1]
	if len(f) >= 20 {
		d.BuildDate = cleanString(f[8:20])
	}
	if len(f) > 20 {
		d.Firmware = cleanString(f[20:])
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

func firstByte(f []byte) byte {
	if len(f) == 0 {
		return 0
	}
	return f[0]
}

// cleanString reads a NUL-padded fixed field, stops at the first NUL, drops
// anything that is not valid UTF-8, and trims surrounding space.
func cleanString(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	s := strings.TrimSpace(string(b))
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "")
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// truncateUTF8 cuts to at most n bytes without splitting a UTF-8 sequence.
// Cutting mid-character emits invalid bytes that render as mojibake whatever
// charset is declared.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// TruncateUTF8 is truncateUTF8, exported for callers that format node-supplied
// names for Discord.
func TruncateUTF8(s string, n int) string { return truncateUTF8(s, n) }

// clampToLimit enforces the 133-byte ceiling as a last line of defence. The
// bridge splits before it gets here; this makes an over-long message
// impossible to put on the wire even if a caller forgets.
func clampToLimit(s string) string { return truncateUTF8(s, MaxMsgLen) }

// ParsePrefix accepts a 12- or 8-character hex key prefix and returns its
// bytes. 8 characters is the 4-byte author prefix a room post carries.
func ParsePrefix(s string) ([]byte, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 12 && len(s) != 8 {
		return nil, fmt.Errorf("meshcore: %q is not a 12- or 8-character key prefix", s)
	}
	return hex.DecodeString(s)
}

// ParsePubKey accepts a full 64-character hex key.
func ParsePubKey(s string) ([]byte, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != PubKeySize*2 {
		return nil, fmt.Errorf("meshcore: a public key is %d hex characters, got %d", PubKeySize*2, len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("meshcore: key is not hexadecimal")
	}
	return b, nil
}
