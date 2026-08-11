// Package config is the typed view over the settings table.
//
// Nothing secret is ever compiled in. Everything below is entered through the
// web UI or the command line and lives in the database, so the same binary is
// safe to hand to anyone and a reinstall does not leak credentials — the same
// rule the ESP32 firmware followed, for the same reason.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"meshycord/internal/store"
)

// Setting keys. Strings rather than a struct because the settings table is
// key/value, and a typo in one place should not silently create a second
// setting nobody reads.
const (
	KeyBotToken     = "discord.bot_token"
	KeyGuildID      = "discord.guild_id"
	KeyAdminChannel = "discord.admin_channel"
	KeyInboxChannel = "discord.inbox_channel"
	KeyAppID        = "discord.application_id"

	KeyTransport  = "mesh.transport"
	KeySerialDev  = "mesh.serial.device"
	KeySerialBaud = "mesh.serial.baud"
	KeyBLEName    = "mesh.ble.name"
	KeyBLEAddr    = "mesh.ble.address"
	KeyBLEPin     = "mesh.ble.pin"
	KeyTCPAddr    = "mesh.tcp.address"

	KeyAutoChannels = "policy.autocreate_channels"
	KeyAutoRooms    = "policy.autocreate_rooms"
	KeyAutoDMs      = "policy.autocreate_dms"
	KeyMaxChunks    = "policy.max_chunks"
	KeyChunkGapMS   = "policy.chunk_gap_ms"
	KeyRetentionDay = "policy.retention_days"
	KeyRoomTTL      = "policy.room_session_seconds"

	KeyWebUser       = "web.username"
	KeyWebPassHash   = "web.password_hash"
	KeyWebSessionKey = "web.session_key"
	KeyWebPassGen    = "web.password_generation"
)

// Transport names.
const (
	TransportSerial = "serial"
	TransportBLE    = "ble"
	TransportTCP    = "tcp"
)

// Defaults that are policy decisions rather than arbitrary numbers.
const (
	// DefaultMaxChunks caps how many transmissions one Discord message may
	// become. Kept low deliberately: each one is airtime everybody on the
	// channel pays for, and a wall of chunks is unpleasant to read on a
	// handheld. Beyond this the message is refused rather than truncated —
	// silent truncation was the worst of the options, because it looked like
	// it sent.
	DefaultMaxChunks = 3
	// DefaultChunkGapMS spaces transmissions out as a courtesy to the mesh.
	DefaultChunkGapMS = 2000
	// DefaultRetentionDays bounds history growth on an SD card. Zero keeps
	// everything.
	DefaultRetentionDays = 90
	// DefaultRoomSessionSeconds is how long a room-server login is assumed to
	// remain good.
	//
	// The protocol never tells us the room's real keep-alive interval, so this
	// is a guess. It matters because nothing can detect a stale session: room
	// servers do not acknowledge posts, so a post into a lapsed session is
	// discarded in complete silence.
	//
	// Five minutes is a compromise, not a measurement. Set it to 0 to log in
	// before every message, which is the only way to be certain.
	DefaultRoomSessionSeconds = 300
)

// Store is the typed settings accessor.
type Store struct {
	db *store.Store

	mu      sync.RWMutex
	sessKey []byte
}

// New wraps a database. It generates a web session key on first use.
func New(db *store.Store) (*Store, error) {
	c := &Store{db: db}
	if err := c.ensureSessionKey(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Store) ensureSessionKey() error {
	if v := c.db.Get(KeyWebSessionKey, ""); v != "" {
		k, err := base64.StdEncoding.DecodeString(v)
		if err == nil && len(k) >= 32 {
			c.mu.Lock()
			c.sessKey = k
			c.mu.Unlock()
			return nil
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return err
	}
	if err := c.db.Set(KeyWebSessionKey, base64.StdEncoding.EncodeToString(k)); err != nil {
		return err
	}
	c.mu.Lock()
	c.sessKey = k
	c.mu.Unlock()
	return nil
}

// SessionKey is the HMAC key for web session cookies.
func (c *Store) SessionKey() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessKey
}

// --- Discord ---------------------------------------------------------------

// BotToken is the Discord bot token. Never render this back to a browser.
func (c *Store) BotToken() string { return c.db.Get(KeyBotToken, "") }

// SetBotToken stores the token.
func (c *Store) SetBotToken(v string) error { return c.db.Set(KeyBotToken, strings.TrimSpace(v)) }

// GuildID is the Discord server the bridge builds its channels in.
//
// Required, and deliberately not auto-detected: inferring it from the bot's
// membership only works when the bot is in exactly one server, which is a
// silent failure mode for anyone who runs it in two.
func (c *Store) GuildID() string { return c.db.Get(KeyGuildID, "") }

// SetGuildID stores the server id.
func (c *Store) SetGuildID(v string) error { return c.db.Set(KeyGuildID, strings.TrimSpace(v)) }

// ApplicationID is the bot's application id, needed to register slash
// commands. It is derivable from the token, so the bridge fills it in itself.
func (c *Store) ApplicationID() string { return c.db.Get(KeyAppID, "") }

// SetApplicationID stores the application id.
func (c *Store) SetApplicationID(v string) error { return c.db.Set(KeyAppID, v) }

// AdminChannel is #meshycord-admin. Never bridged to the mesh.
func (c *Store) AdminChannel() string { return c.db.Get(KeyAdminChannel, "") }

// SetAdminChannel stores the admin channel id.
func (c *Store) SetAdminChannel(v string) error { return c.db.Set(KeyAdminChannel, v) }

// InboxChannel is where traffic from unlinked senders goes.
func (c *Store) InboxChannel() string { return c.db.Get(KeyInboxChannel, "") }

// SetInboxChannel stores the inbox channel id.
func (c *Store) SetInboxChannel(v string) error { return c.db.Set(KeyInboxChannel, v) }

// --- Mesh transport --------------------------------------------------------

// Transport is which link to the node to use: serial, ble or tcp.
func (c *Store) Transport() string {
	switch v := c.db.Get(KeyTransport, TransportSerial); v {
	case TransportSerial, TransportBLE, TransportTCP:
		return v
	default:
		return TransportSerial
	}
}

// SetTransport picks the link type.
func (c *Store) SetTransport(v string) error {
	switch v {
	case TransportSerial, TransportBLE, TransportTCP:
		return c.db.Set(KeyTransport, v)
	}
	return errors.New("transport must be serial, ble or tcp")
}

// SerialDevice is the tty path; empty means auto-detect.
func (c *Store) SerialDevice() string { return c.db.Get(KeySerialDev, "") }

// SetSerialDevice sets the tty path.
func (c *Store) SetSerialDevice(v string) error { return c.db.Set(KeySerialDev, strings.TrimSpace(v)) }

// SerialBaud is the line rate, ignored by USB CDC-ACM devices.
func (c *Store) SerialBaud() int { return c.db.GetInt(KeySerialBaud, 115200) }

// SetSerialBaud sets the line rate.
func (c *Store) SetSerialBaud(v int) error { return c.db.SetInt(KeySerialBaud, v) }

// BLEName matches the node's advertised name as a substring.
func (c *Store) BLEName() string { return c.db.Get(KeyBLEName, "") }

// SetBLEName sets the name filter.
func (c *Store) SetBLEName(v string) error { return c.db.Set(KeyBLEName, strings.TrimSpace(v)) }

// BLEAddress is the node's MAC, which wins over the name when set.
func (c *Store) BLEAddress() string { return c.db.Get(KeyBLEAddr, "") }

// SetBLEAddress sets the MAC.
func (c *Store) SetBLEAddress(v string) error {
	return c.db.Set(KeyBLEAddr, strings.ToUpper(strings.TrimSpace(v)))
}

// BLEPin is the node's fixed six-digit pairing PIN.
func (c *Store) BLEPin() string { return c.db.Get(KeyBLEPin, "") }

// SetBLEPin sets the pairing PIN.
func (c *Store) SetBLEPin(v string) error { return c.db.Set(KeyBLEPin, strings.TrimSpace(v)) }

// TCPAddress is host:port of a WiFi companion. Port 5000 is assumed.
func (c *Store) TCPAddress() string { return c.db.Get(KeyTCPAddr, "") }

// SetTCPAddress sets the companion address.
func (c *Store) SetTCPAddress(v string) error { return c.db.Set(KeyTCPAddr, strings.TrimSpace(v)) }

// --- Policy ----------------------------------------------------------------

// AutoCreateChannels links mesh channels automatically. On by default: a
// node's channels are yours already, so there is no abuse surface, and a
// bridge with no channels visible looks broken on first run.
func (c *Store) AutoCreateChannels() bool { return c.db.GetBool(KeyAutoChannels, true) }

// SetAutoCreateChannels sets the channel policy.
func (c *Store) SetAutoCreateChannels(v bool) error { return c.db.SetBool(KeyAutoChannels, v) }

// AutoCreateRooms links room servers automatically. Off by default: there are
// often dozens, which fills a server and hits Discord's per-category limit.
func (c *Store) AutoCreateRooms() bool { return c.db.GetBool(KeyAutoRooms, false) }

// SetAutoCreateRooms sets the room policy.
func (c *Store) SetAutoCreateRooms(v bool) error { return c.db.SetBool(KeyAutoRooms, v) }

// AutoCreateDMs links direct messages automatically.
//
// The riskiest of the three and off by default: anyone who has heard your
// advert can send you a DM, so this is the switch that lets a stranger create
// a channel in your server. Even when on it only fires for senders already in
// the node's contact list — an unknown sender always goes to the inbox, which
// is the abuse guard the whole policy exists for.
func (c *Store) AutoCreateDMs() bool { return c.db.GetBool(KeyAutoDMs, false) }

// SetAutoCreateDMs sets the DM policy.
func (c *Store) SetAutoCreateDMs(v bool) error { return c.db.SetBool(KeyAutoDMs, v) }

// MaxChunks caps transmissions per Discord message.
func (c *Store) MaxChunks() int {
	n := c.db.GetInt(KeyMaxChunks, DefaultMaxChunks)
	if n < 1 {
		return 1
	}
	if n > 10 {
		return 10
	}
	return n
}

// SetMaxChunks sets the transmission cap.
func (c *Store) SetMaxChunks(v int) error { return c.db.SetInt(KeyMaxChunks, v) }

// ChunkGap is the pause between transmissions of a split message.
func (c *Store) ChunkGap() time.Duration {
	return time.Duration(c.db.GetInt(KeyChunkGapMS, DefaultChunkGapMS)) * time.Millisecond
}

// SetChunkGapMS sets the inter-transmission pause.
func (c *Store) SetChunkGapMS(v int) error { return c.db.SetInt(KeyChunkGapMS, v) }

// Retention is how long message history is kept. Zero means forever.
func (c *Store) Retention() time.Duration {
	d := c.db.GetInt(KeyRetentionDay, DefaultRetentionDays)
	if d <= 0 {
		return 0
	}
	return time.Duration(d) * 24 * time.Hour
}

// SetRetentionDays sets the history window in days.
func (c *Store) SetRetentionDays(v int) error { return c.db.SetInt(KeyRetentionDay, v) }

// RoomSessionTTL is how long a room-server login is trusted before the bridge
// logs in again.
//
// Zero means log in before every single message. That is the simplest thing to
// reason about and the most reliable, and it costs a full over-the-air round
// trip per post — on a shared medium where every hop repeats the packet, that
// roughly doubles the airtime a room conversation uses. A short non-zero
// window gets almost all of the reliability for a fraction of the cost.
func (c *Store) RoomSessionTTL() time.Duration {
	n := c.db.GetInt(KeyRoomTTL, DefaultRoomSessionSeconds)
	if n < 0 {
		n = 0
	}
	return time.Duration(n) * time.Second
}

// SetRoomSessionSeconds sets the room-session freshness window. Zero means log
// in before every message.
func (c *Store) SetRoomSessionSeconds(v int) error { return c.db.SetInt(KeyRoomTTL, v) }

// --- Web UI login ----------------------------------------------------------

// Username is the web console account name.
func (c *Store) Username() string {
	if u := c.db.Get(KeyWebUser, ""); u != "" {
		return u
	}
	return "admin"
}

// SetUsername renames the account.
func (c *Store) SetUsername(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("username cannot be empty")
	}
	if err := c.db.Set(KeyWebUser, v); err != nil {
		return err
	}
	// A rename invalidates outstanding sessions, same as a password change.
	return c.bumpPasswordGeneration()
}

// HasPassword reports whether a web password has been set.
//
// It has NOT been, on a fresh install. The console is then reachable by anyone
// on the network and it holds the bot token, so the UI says so loudly rather
// than quietly defaulting to something guessable.
func (c *Store) HasPassword() bool { return c.db.Get(KeyWebPassHash, "") != "" }

// SetPassword hashes and stores a new password.
func (c *Store) SetPassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	// bcrypt's default cost is 10, which is several seconds on an ARMv6 Pi
	// Zero. 10 is still the right choice — a login is rare and a slow hash is
	// the entire point — but it is worth knowing why the login page pauses.
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := c.db.Set(KeyWebPassHash, string(h)); err != nil {
		return err
	}
	return c.bumpPasswordGeneration()
}

// CheckPassword verifies a password against the stored hash.
func (c *Store) CheckPassword(pw string) bool {
	h := c.db.Get(KeyWebPassHash, "")
	if h == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)) == nil
}

// PasswordGeneration is embedded in session cookies so that changing the
// password invalidates every outstanding session.
func (c *Store) PasswordGeneration() uint64 {
	return uint64(c.db.GetInt(KeyWebPassGen, 1))
}

func (c *Store) bumpPasswordGeneration() error {
	return c.db.SetInt(KeyWebPassGen, c.db.GetInt(KeyWebPassGen, 1)+1)
}

// --- Readiness -------------------------------------------------------------

// Configured reports whether the bridge has what it needs to run.
//
// The inbox is not in this list because the bridge always creates it. The
// guild is, because it cannot be guessed.
func (c *Store) Configured() bool {
	return c.BotToken() != "" && c.GuildID() != ""
}

// MissingSettings lists what setup still needs, for the UI to show.
func (c *Store) MissingSettings() []string {
	var out []string
	if c.BotToken() == "" {
		out = append(out, "Discord bot token")
	}
	if c.GuildID() == "" {
		out = append(out, "Discord server (guild) ID")
	}
	if c.Transport() == TransportTCP && c.TCPAddress() == "" {
		out = append(out, "companion TCP address")
	}
	return out
}
