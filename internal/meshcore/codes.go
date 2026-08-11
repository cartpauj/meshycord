// Package meshcore implements the MeshCore companion protocol.
//
// The codec (this file, frames.go) is pure: bytes in, structs out, no I/O and
// no knowledge of Discord. That is deliberate — it is the part carried over
// from the ESP32 firmware, where every one of the gotchas documented in
// PROTOCOL-NOTES.md cost real debugging, and it is the part worth having
// unit tests for.
//
// Byte layouts were read from the MeshCore source at commit 727fc05 (v1.17.0);
// the additional command codes were read from main. See PROTOCOL-NOTES.md.
package meshcore

// Command codes, sent to the node. From examples/companion_radio/MyMesh.cpp:6.
const (
	CmdAppStart          = 0x01
	CmdSendTxtMsg        = 0x02
	CmdSendChannelTxtMsg = 0x03
	CmdGetContacts       = 0x04
	CmdGetDeviceTime     = 0x05
	CmdSetDeviceTime     = 0x06
	CmdSendSelfAdvert    = 0x07
	CmdSetAdvertName     = 0x08
	CmdAddUpdateContact  = 0x09
	CmdSyncNextMessage   = 0x0A
	CmdSetRadioParams    = 0x0B
	CmdSetRadioTxPower   = 0x0C
	CmdResetPath         = 0x0D
	CmdSetAdvertLatLon   = 0x0E
	CmdRemoveContact     = 0x0F // needs the FULL 32-byte key
	CmdShareContact      = 0x10
	CmdExportContact     = 0x11
	CmdImportContact     = 0x12
	CmdReboot            = 0x13
	CmdGetBattAndStorage = 0x14
	CmdSetTuningParams   = 0x15
	CmdDeviceQuery       = 0x16
	CmdExportPrivateKey  = 0x17
	CmdImportPrivateKey  = 0x18
	CmdSendRawData       = 0x19
	CmdSendLogin         = 0x1A // needs the FULL 32-byte key
	CmdSendStatusReq     = 0x1B
	CmdHasConnection     = 0x1C
	CmdLogout            = 0x1D
	CmdGetContactByKey   = 0x1E // needs the FULL 32-byte key
	CmdGetChannel        = 0x1F
	CmdSetChannel        = 0x20 // create/update a channel on the node
)

// Response codes. These are direct replies to commands: anything below 0x80.
const (
	RespOK               = 0x00
	RespError            = 0x01
	RespContactsStart    = 0x02
	RespContact          = 0x03
	RespEndOfContacts    = 0x04
	RespSelfInfo         = 0x05
	RespSent             = 0x06 // reply to CmdSendTxtMsg / CmdSendLogin
	RespContactMsgRecv   = 0x07
	RespChannelMsgRecv   = 0x08
	RespCurrTime         = 0x09
	RespNoMoreMessages   = 0x0A
	RespExportContact    = 0x0B
	RespBattAndStorage   = 0x0C
	RespDeviceInfo       = 0x0D
	RespPrivateKey       = 0x0E
	RespDisabled         = 0x0F
	RespContactMsgRecvV3 = 0x10
	RespChannelMsgRecvV3 = 0x11
	RespChannelInfo      = 0x12 // reply to CmdGetChannel
)

// Push codes — asynchronous, never a command reply.
//
// This is the single most important rule in the protocol. Anything with a
// first byte >= 0x80 arrives whenever the node feels like it, and letting one
// into the response queue caused three separate bugs on the ESP32, each
// presenting as something entirely unrelated being subtly wrong.
const (
	PushAdvert                = 0x80
	PushPathUpdated           = 0x81
	PushSendConfirmed         = 0x82 // [ack u32][round-trip ms u32]
	PushMsgWaiting            = 0x83
	PushRawData               = 0x84
	PushLoginSuccess          = 0x85
	PushLoginFail             = 0x86
	PushStatusResponse        = 0x87
	PushLogRxData             = 0x88
	PushTraceData             = 0x89
	PushNewAdvert             = 0x8A
	PushTelemetryResponse     = 0x8B
	PushBinaryResponse        = 0x8C
	PushPathDiscoveryResponse = 0x8D
	PushControlData           = 0x8E
	PushContactDeleted        = 0x8F
	PushContactsFull          = 0x90
)

// IsPush reports whether a frame is an asynchronous push rather than a reply
// to whatever command happens to be outstanding.
func IsPush(code byte) bool { return code >= 0x80 }

// Advert types — what a contact IS. From src/helpers/AdvertDataHelpers.h:7.
const (
	AdvTypeNone     = 0
	AdvTypeChat     = 1 // a person, running the companion app
	AdvTypeRepeater = 2
	AdvTypeRoom     = 3 // a room server
	AdvTypeSensor   = 4
)

// AdvTypeName renders a contact type for humans.
func AdvTypeName(t byte) string {
	switch t {
	case AdvTypeChat:
		return "companion"
	case AdvTypeRoom:
		return "room"
	case AdvTypeRepeater:
		return "repeater"
	case AdvTypeSensor:
		return "sensor"
	default:
		return "unknown"
	}
}

// Protocol constants.
const (
	// MaxMsgLen is the MeshCore per-message ceiling: MAX_TEXT_LEN in
	// src/helpers/BaseChatMesh.h, defined as 10 * CIPHER_BLOCK_SIZE = 160.
	// BaseChatMesh.cpp:463 refuses anything longer outright.
	//
	// The carried-over notes said 133, and that was simply wrong — it cost
	// about 20% of every transmission and split messages that would have fitted
	// in one. Read from the firmware at 727fc05 rather than from the docs.
	MaxMsgLen = 160

	// ChannelNamePrefixOverhead is the ": " the node inserts between its own
	// name and a group message.
	//
	// sendGroupMessage builds "<name>: <text>" and then TRUNCATES to fit
	// MAX_TEXT_LEN (BaseChatMesh.cpp:496) — silently, with no error. So the
	// usable text on a channel is the ceiling minus the node's own name and
	// this separator.
	ChannelNamePrefixOverhead = 2
	// PubKeySize is a full MeshCore public key. Messages carry only the first
	// 6 bytes of it, which is enough to route by and NOT enough to prove
	// identity with.
	PubKeySize = 32
	// MaxPathSize is the stored hop list in a contact record.
	MaxPathSize = 64
	// NoPath is the OutPathLen value meaning "no route known". The node floods
	// the next message to a contact in this state, which is what makes clearing
	// the path the way to force a flood — there is no per-message flag for it.
	NoPath = 0xFF
	// MaxChannels is how many channel slots a node has.
	MaxChannels = 8
	// ChannelSecretSize is the shared secret of a mesh channel.
	ChannelSecretSize = 16
)
