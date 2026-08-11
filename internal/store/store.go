// Package store is MeshyCord's persistence: settings, links, contacts, room
// credentials, and full message history.
//
// The ESP32 had 20 KB of NVS shared with the bot token, so it kept a packed
// route table and nothing else — history lived in a 256-message ring on the
// radio and was gone the moment it wrapped. A Pi has a disk, so history is
// real here, and that is what makes the web UI worth having.
//
// SD-card longevity is a genuine concern on a Pi, so this deliberately avoids
// the ESP32's per-poll persistence pattern: WAL mode, synchronous=NORMAL, and
// writes only when something actually changed.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: cgo would make ARMv6 builds painful
)

// Store wraps the database. Safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	}

	// _txlock=immediate avoids SQLITE_BUSY upgrade deadlocks when two
	// goroutines start a transaction that both later want to write.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer. SQLite serialises writes anyway, and a Pi Zero's single core
	// gains nothing from contention.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The bot token and every room password live in here. Nothing else on the
	// box has any business reading it.
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("lock down %s: %w", path, err)
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for callers that need one-off queries.
func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- The mesh source <-> Discord channel mapping. This is the only state the
-- bridge genuinely owns; contacts and names all live on the node.
--
-- Routing is by kind+mesh_key, NEVER by channel name, so renaming a Discord
-- channel can never break anything.
CREATE TABLE IF NOT EXISTS routes (
  id            INTEGER PRIMARY KEY,
  kind          TEXT    NOT NULL,          -- dm | channel | room
  mesh_key      TEXT    NOT NULL,          -- 12-hex key prefix, or channel slot
  channel_id    TEXT    NOT NULL,          -- Discord snowflake
  label         TEXT    NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  last_activity INTEGER NOT NULL DEFAULT 0,
  UNIQUE(kind, mesh_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS routes_channel ON routes(channel_id);

-- A cache of the node's contacts. The node stays the source of truth; this
-- exists so the web UI still has names when the radio is unplugged, and so a
-- 6-byte prefix off a message can be resolved to a full 32-byte key.
CREATE TABLE IF NOT EXISTS contacts (
  pubkey       TEXT PRIMARY KEY,           -- 64 hex characters
  prefix       TEXT NOT NULL,              -- first 12 of the above
  type         INTEGER NOT NULL,
  name         TEXT NOT NULL DEFAULT '',
  out_path_len INTEGER NOT NULL DEFAULT 255,
  last_advert  INTEGER NOT NULL DEFAULT 0,
  lat          REAL NOT NULL DEFAULT 0,
  lon          REAL NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS contacts_prefix ON contacts(prefix);
CREATE INDEX IF NOT EXISTS contacts_name   ON contacts(name);

-- Full history, both directions.
CREATE TABLE IF NOT EXISTS messages (
  id            INTEGER PRIMARY KEY,
  created_at    INTEGER NOT NULL,
  direction     TEXT    NOT NULL,          -- in | out
  kind          TEXT    NOT NULL,          -- dm | channel | room
  mesh_key      TEXT    NOT NULL,
  peer_label    TEXT    NOT NULL DEFAULT '',
  author        TEXT    NOT NULL DEFAULT '', -- room posts carry their own author
  body          TEXT    NOT NULL,
  discord_channel_id TEXT NOT NULL DEFAULT '',
  discord_message_id TEXT NOT NULL DEFAULT '',
  discord_user       TEXT NOT NULL DEFAULT '',
  have_hops     INTEGER NOT NULL DEFAULT 0,
  hops          INTEGER NOT NULL DEFAULT 0,
  path_raw      INTEGER NOT NULL DEFAULT 255,
  have_snr      INTEGER NOT NULL DEFAULT 0,
  snr           REAL    NOT NULL DEFAULT 0,
  flooded       INTEGER NOT NULL DEFAULT 0,
  ack           INTEGER NOT NULL DEFAULT 0, -- expected-ack handle for an outbound DM
  delivery      TEXT    NOT NULL DEFAULT '',
  round_trip_ms INTEGER NOT NULL DEFAULT 0,
  chunk_index   INTEGER NOT NULL DEFAULT 0,
  chunk_total   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS messages_time  ON messages(created_at DESC);
CREATE INDEX IF NOT EXISTS messages_route ON messages(kind, mesh_key, created_at DESC);
CREATE INDEX IF NOT EXISTS messages_ack   ON messages(ack) WHERE ack != 0;
CREATE INDEX IF NOT EXISTS messages_dmid  ON messages(discord_message_id);

-- Room-server credentials and session state.
--
-- A room server refuses posts from anyone not logged in, and the session does
-- not last forever, so the password is kept in order to re-establish it after
-- a reconnect rather than asking again.
CREATE TABLE IF NOT EXISTS rooms (
  prefix      TEXT PRIMARY KEY,
  password    TEXT NOT NULL DEFAULT '',
  last_login  INTEGER NOT NULL DEFAULT 0,
  last_result TEXT NOT NULL DEFAULT ''
);

-- An audit trail of admin actions, so "why does this channel exist" has an
-- answer months later.
CREATE TABLE IF NOT EXISTS events (
  id         INTEGER PRIMARY KEY,
  created_at INTEGER NOT NULL,
  level      TEXT NOT NULL,
  source     TEXT NOT NULL,
  message    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_time ON events(created_at DESC);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// Get reads a setting, returning def when unset.
func (s *Store) Get(key, def string) string {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

// Set writes a setting. A no-op write is skipped, because on a Pi every
// avoidable write is an avoidable SD-card erase cycle.
func (s *Store) Set(key, value string) error {
	if s.Get(key, "\x00missing") == value {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// GetBool reads a boolean setting.
func (s *Store) GetBool(key string, def bool) bool {
	v := s.Get(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// SetBool writes a boolean setting.
func (s *Store) SetBool(key string, v bool) error { return s.Set(key, strconv.FormatBool(v)) }

// GetInt reads an integer setting.
func (s *Store) GetInt(key string, def int) int {
	n, err := strconv.Atoi(s.Get(key, ""))
	if err != nil {
		return def
	}
	return n
}

// SetInt writes an integer setting.
func (s *Store) SetInt(key string, v int) error { return s.Set(key, strconv.Itoa(v)) }

// AllSettings returns every stored setting, for diagnostics. Secrets are the
// caller's problem to redact.
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

// RouteKind names the three things a Discord channel can be bridged to.
type RouteKind string

const (
	KindDM      RouteKind = "dm"
	KindChannel RouteKind = "channel"
	KindRoom    RouteKind = "room"
)

// Valid reports whether k is one of the three known kinds.
func (k RouteKind) Valid() bool {
	return k == KindDM || k == KindChannel || k == KindRoom
}

// Route links one mesh source to one Discord channel.
type Route struct {
	ID           int64
	Kind         RouteKind
	MeshKey      string // 12-hex key prefix, or a channel slot as decimal
	ChannelID    string
	Label        string
	CreatedAt    time.Time
	LastActivity time.Time
}

// ErrNoRoute is returned when a lookup finds nothing.
var ErrNoRoute = errors.New("store: no such link")

func scanRoute(sc interface{ Scan(...any) error }) (Route, error) {
	var (
		r          Route
		kind       string
		created    int64
		lastActive int64
	)
	err := sc.Scan(&r.ID, &kind, &r.MeshKey, &r.ChannelID, &r.Label, &created, &lastActive)
	if err != nil {
		return r, err
	}
	r.Kind = RouteKind(kind)
	r.CreatedAt = time.Unix(created, 0)
	if lastActive > 0 {
		r.LastActivity = time.Unix(lastActive, 0)
	}
	return r, nil
}

const routeCols = `id, kind, mesh_key, channel_id, label, created_at, last_activity`

// Routes returns every link, channels first then rooms then DMs, each by label.
func (s *Store) Routes() ([]Route, error) {
	rows, err := s.db.Query(`SELECT ` + routeCols + ` FROM routes
		ORDER BY CASE kind WHEN 'channel' THEN 0 WHEN 'room' THEN 1 ELSE 2 END,
		         label COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Route looks a link up by what it bridges.
func (s *Store) Route(kind RouteKind, meshKey string) (Route, error) {
	row := s.db.QueryRow(`SELECT `+routeCols+` FROM routes WHERE kind = ? AND mesh_key = ?`,
		string(kind), meshKey)
	r, err := scanRoute(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoRoute
	}
	return r, err
}

// RouteByChannel looks a link up by its Discord channel — the direction used
// on every inbound Discord message.
func (s *Store) RouteByChannel(channelID string) (Route, error) {
	row := s.db.QueryRow(`SELECT `+routeCols+` FROM routes WHERE channel_id = ?`, channelID)
	r, err := scanRoute(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoRoute
	}
	return r, err
}

// RouteByPrefix finds a DM or room link for a key prefix, whichever exists.
// A prefix can only be one or the other, but which one is not known up front.
func (s *Store) RouteByPrefix(prefix string) (Route, error) {
	row := s.db.QueryRow(`SELECT `+routeCols+` FROM routes
		WHERE mesh_key = ? AND kind IN ('dm','room') LIMIT 1`, prefix)
	r, err := scanRoute(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoRoute
	}
	return r, err
}

// PutRoute creates or updates a link.
func (s *Store) PutRoute(kind RouteKind, meshKey, channelID, label string) (Route, error) {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO routes(kind, mesh_key, channel_id, label, created_at, last_activity)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, mesh_key) DO UPDATE SET
		  channel_id = excluded.channel_id,
		  label      = CASE WHEN excluded.label != '' THEN excluded.label ELSE routes.label END`,
		string(kind), meshKey, channelID, label, now, now)
	if err != nil {
		return Route{}, err
	}
	return s.Route(kind, meshKey)
}

// UpdateRouteLabel refreshes the stored display name.
//
// Labels go stale: they are captured when the link is created, and a contact
// that renames itself would otherwise be shown under its old name forever.
func (s *Store) UpdateRouteLabel(kind RouteKind, meshKey, label string) error {
	if label == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE routes SET label = ? WHERE kind = ? AND mesh_key = ? AND label != ?`,
		label, string(kind), meshKey, label)
	return err
}

// TouchRoute records activity, for sorting the links page by liveliness.
func (s *Store) TouchRoute(id int64) error {
	_, err := s.db.Exec(`UPDATE routes SET last_activity = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// DeleteRoute removes a link. Reports whether anything was removed.
func (s *Store) DeleteRoute(kind RouteKind, meshKey string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM routes WHERE kind = ? AND mesh_key = ?`, string(kind), meshKey)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteRouteByChannel removes whatever link points at a Discord channel. Used
// when Discord reports the channel gone.
func (s *Store) DeleteRouteByChannel(channelID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM routes WHERE channel_id = ?`, channelID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearRoutes removes every link.
func (s *Store) ClearRoutes() error {
	_, err := s.db.Exec(`DELETE FROM routes`)
	return err
}

// ---------------------------------------------------------------------------
// Contacts
// ---------------------------------------------------------------------------

// Contact mirrors the node's contact record.
type Contact struct {
	PubKey     string
	Prefix     string
	Type       int
	Name       string
	OutPathLen int
	LastAdvert time.Time
	Lat, Lon   float64
	UpdatedAt  time.Time
}

// UpsertContacts adds or updates without removing anything.
//
// This is the safe form, and the right one whenever the list might be
// incomplete. A contact enumeration is a stream that can end early — the node
// is a single-threaded radio and can simply stop answering for a while — and
// treating a partial stream as authoritative deletes contacts that are still
// perfectly real. That happened: a room server vanished from the mirror
// mid-session, so the bridge could no longer resolve its key and refused to
// log in to it.
func (s *Store) UpsertContacts(cs []Contact) error {
	return s.writeContacts(cs, false)
}

// ReplaceContacts swaps the whole cache in one transaction.
//
// Only safe after a COMPLETE enumeration. Records absent from the new list are
// dropped, which is what makes a contact removed on the node disappear here
// too — but which destroys good data if the list was truncated.
func (s *Store) ReplaceContacts(cs []Contact) error {
	return s.writeContacts(cs, true)
}

func (s *Store) writeContacts(cs []Contact, authoritative bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if authoritative {
		if _, err := tx.Exec(`DELETE FROM contacts`); err != nil {
			return err
		}
	}
	stmt, err := tx.Prepare(`INSERT INTO contacts
		(pubkey, prefix, type, name, out_path_len, last_advert, lat, lon, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pubkey) DO UPDATE SET
		  name = excluded.name, type = excluded.type,
		  out_path_len = excluded.out_path_len, last_advert = excluded.last_advert,
		  lat = excluded.lat, lon = excluded.lon, updated_at = excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	for _, c := range cs {
		var adv int64
		if !c.LastAdvert.IsZero() {
			adv = c.LastAdvert.Unix()
		}
		if _, err := stmt.Exec(c.PubKey, c.Prefix, c.Type, c.Name, c.OutPathLen,
			adv, c.Lat, c.Lon, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Contacts returns the cached contact list, most recently heard first.
// typeFilter of -1 means every type.
func (s *Store) Contacts(typeFilter int) ([]Contact, error) {
	q := `SELECT pubkey, prefix, type, name, out_path_len, last_advert, lat, lon, updated_at
	      FROM contacts`
	var args []any
	if typeFilter >= 0 {
		q += ` WHERE type = ?`
		args = append(args, typeFilter)
	}
	q += ` ORDER BY last_advert DESC, name COLLATE NOCASE`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		var (
			c        Contact
			adv, upd int64
		)
		if err := rows.Scan(&c.PubKey, &c.Prefix, &c.Type, &c.Name, &c.OutPathLen,
			&adv, &c.Lat, &c.Lon, &upd); err != nil {
			return nil, err
		}
		if adv > 0 {
			c.LastAdvert = time.Unix(adv, 0)
		}
		c.UpdatedAt = time.Unix(upd, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ContactByPrefix resolves a key prefix to a cached contact.
func (s *Store) ContactByPrefix(prefix string) (Contact, bool) {
	prefix = strings.ToLower(prefix)
	all, err := s.Contacts(-1)
	if err != nil {
		return Contact{}, false
	}
	for _, c := range all {
		if strings.HasPrefix(c.Prefix, prefix) {
			return c, true
		}
	}
	return Contact{}, false
}

// ---------------------------------------------------------------------------
// Rooms
// ---------------------------------------------------------------------------

// RoomPassword returns the stored password for a room server, if any.
func (s *Store) RoomPassword(prefix string) string {
	var pw string
	if err := s.db.QueryRow(`SELECT password FROM rooms WHERE prefix = ?`, prefix).Scan(&pw); err != nil {
		return ""
	}
	return pw
}

// HasRoomPassword reports whether a password is stored.
func (s *Store) HasRoomPassword(prefix string) bool { return s.RoomPassword(prefix) != "" }

// SetRoomPassword stores or (with an empty password) forgets one.
func (s *Store) SetRoomPassword(prefix, password string) error {
	if password == "" {
		_, err := s.db.Exec(`DELETE FROM rooms WHERE prefix = ?`, prefix)
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO rooms(prefix, password) VALUES(?, ?)
		ON CONFLICT(prefix) DO UPDATE SET password = excluded.password`, prefix, password)
	return err
}

// RecordLogin notes the outcome of a room login attempt.
func (s *Store) RecordLogin(prefix, result string) error {
	_, err := s.db.Exec(`
		INSERT INTO rooms(prefix, last_login, last_result) VALUES(?, ?, ?)
		ON CONFLICT(prefix) DO UPDATE SET last_login = excluded.last_login,
		                                  last_result = excluded.last_result`,
		prefix, time.Now().Unix(), result)
	return err
}

// RoomsWithPasswords lists every room the bridge can log in to unattended.
func (s *Store) RoomsWithPasswords() ([]string, error) {
	rows, err := s.db.Query(`SELECT prefix FROM rooms WHERE password != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// Delivery states an outbound message passes through.
//
// The distinction between Transmitted and Delivered is not cosmetic: MeshCore
// cannot acknowledge group messages at all, so a channel send can only ever be
// reported as transmitted. Showing a tick there would claim a delivery the
// protocol is incapable of proving.
const (
	DeliveryPending     = "pending"     // sent, waiting for an ack
	DeliveryDelivered   = "delivered"   // the node confirmed it
	DeliveryFailed      = "failed"      // rejected, or no ack before the deadline
	DeliveryTransmitted = "transmitted" // it went out; no ack is possible
	DeliveryRefused     = "refused"     // never sent, and why is in the body
	DeliveryReceived    = "received"    // inbound
)

// Message is one message in either direction.
type Message struct {
	ID         int64
	CreatedAt  time.Time
	Direction  string // in | out
	Kind       RouteKind
	MeshKey    string
	PeerLabel  string
	Author     string
	Body       string
	ChannelID  string
	MessageID  string
	DiscordUsr string
	HaveHops   bool
	Hops       int
	PathRaw    int
	HaveSNR    bool
	SNR        float64
	Flooded    bool
	Ack        uint32
	Delivery   string
	RoundTrip  time.Duration
	ChunkIndex int
	ChunkTotal int
}

const messageCols = `id, created_at, direction, kind, mesh_key, peer_label, author, body,
	discord_channel_id, discord_message_id, discord_user, have_hops, hops, path_raw,
	have_snr, snr, flooded, ack, delivery, round_trip_ms, chunk_index, chunk_total`

// InsertMessage records a message and returns its row id.
func (s *Store) InsertMessage(m Message) (int64, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO messages
		(created_at, direction, kind, mesh_key, peer_label, author, body,
		 discord_channel_id, discord_message_id, discord_user, have_hops, hops, path_raw,
		 have_snr, snr, flooded, ack, delivery, round_trip_ms, chunk_index, chunk_total)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.CreatedAt.Unix(), m.Direction, string(m.Kind), m.MeshKey, m.PeerLabel, m.Author, m.Body,
		m.ChannelID, m.MessageID, m.DiscordUsr, boolInt(m.HaveHops), m.Hops, m.PathRaw,
		boolInt(m.HaveSNR), m.SNR, boolInt(m.Flooded), m.Ack, m.Delivery,
		m.RoundTrip.Milliseconds(), m.ChunkIndex, m.ChunkTotal)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetDiscordMessageID attaches the Discord message a mesh message was relayed
// into, so a later reaction can find its way back.
func (s *Store) SetDiscordMessageID(id int64, channelID, messageID string) error {
	_, err := s.db.Exec(`UPDATE messages SET discord_channel_id = ?, discord_message_id = ? WHERE id = ?`,
		channelID, messageID, id)
	return err
}

// SetDelivery updates a message's delivery state.
func (s *Store) SetDelivery(id int64, state string, rtt time.Duration) error {
	_, err := s.db.Exec(`UPDATE messages SET delivery = ?, round_trip_ms = ? WHERE id = ?`,
		state, rtt.Milliseconds(), id)
	return err
}

// MessageByDiscordID finds the message relayed into a given Discord message.
func (s *Store) MessageByDiscordID(messageID string) (Message, bool) {
	row := s.db.QueryRow(`SELECT `+messageCols+` FROM messages WHERE discord_message_id = ? LIMIT 1`, messageID)
	m, err := scanMessage(row)
	return m, err == nil
}

// MessageQuery filters the history page.
type MessageQuery struct {
	Kind    RouteKind // empty for any
	MeshKey string    // empty for any
	Search  string    // substring of the body or the peer label
	Limit   int
	Before  int64 // id, for paging back through history
}

// Messages runs a history query, newest first.
func (s *Store) Messages(q MessageQuery) ([]Message, error) {
	var (
		where []string
		args  []any
	)
	if q.Kind != "" {
		where = append(where, `kind = ?`)
		args = append(args, string(q.Kind))
	}
	if q.MeshKey != "" {
		where = append(where, `mesh_key = ?`)
		args = append(args, q.MeshKey)
	}
	if q.Search != "" {
		where = append(where, `(body LIKE ? OR peer_label LIKE ? OR author LIKE ?)`)
		like := "%" + q.Search + "%"
		args = append(args, like, like, like)
	}
	if q.Before > 0 {
		where = append(where, `id < ?`)
		args = append(args, q.Before)
	}
	sqlText := `SELECT ` + messageCols + ` FROM messages`
	if len(where) > 0 {
		sqlText += ` WHERE ` + strings.Join(where, " AND ")
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	sqlText += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var (
		m                          Message
		created, rtt               int64
		haveHops, haveSNR, flooded int
		kind                       string
	)
	err := sc.Scan(&m.ID, &created, &m.Direction, &kind, &m.MeshKey, &m.PeerLabel, &m.Author,
		&m.Body, &m.ChannelID, &m.MessageID, &m.DiscordUsr, &haveHops, &m.Hops, &m.PathRaw,
		&haveSNR, &m.SNR, &flooded, &m.Ack, &m.Delivery, &rtt, &m.ChunkIndex, &m.ChunkTotal)
	if err != nil {
		return m, err
	}
	m.CreatedAt = time.Unix(created, 0)
	m.Kind = RouteKind(kind)
	m.HaveHops, m.HaveSNR, m.Flooded = haveHops != 0, haveSNR != 0, flooded != 0
	m.RoundTrip = time.Duration(rtt) * time.Millisecond
	return m, nil
}

// Stats summarises the database for the dashboard.
type Stats struct {
	Messages    int
	MessagesIn  int
	MessagesOut int
	Delivered   int
	Failed      int
	Contacts    int
	Routes      int
	Last24h     int
	DBBytes     int64
	Oldest      time.Time
}

// Stats computes the dashboard summary in one pass per table.
func (s *Store) Stats(dbPath string) Stats {
	var st Stats
	_ = s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(direction = 'in'), 0),
		COALESCE(SUM(direction = 'out'), 0),
		COALESCE(SUM(delivery = 'delivered'), 0),
		COALESCE(SUM(delivery = 'failed'), 0),
		COALESCE(SUM(created_at > ?), 0),
		COALESCE(MIN(created_at), 0)
		FROM messages`, time.Now().Add(-24*time.Hour).Unix()).
		Scan(&st.Messages, &st.MessagesIn, &st.MessagesOut, &st.Delivered, &st.Failed,
			&st.Last24h, new(int64))

	var oldest int64
	_ = s.db.QueryRow(`SELECT COALESCE(MIN(created_at), 0) FROM messages`).Scan(&oldest)
	if oldest > 0 {
		st.Oldest = time.Unix(oldest, 0)
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&st.Contacts)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM routes`).Scan(&st.Routes)
	if fi, err := os.Stat(dbPath); err == nil {
		st.DBBytes = fi.Size()
	}
	return st
}

// Prune deletes history older than the retention window, and events with it.
// Zero or negative keeps everything.
func (s *Store) Prune(retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retention).Unix()
	res, err := s.db.Exec(`DELETE FROM messages WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.Exec(`DELETE FROM events WHERE created_at < ?`, cutoff); err != nil {
		return n, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// Event is one line of the bridge's own activity log.
type Event struct {
	ID        int64
	CreatedAt time.Time
	Level     string
	Source    string
	Message   string
}

// LogEvent records something worth being able to look up later.
func (s *Store) LogEvent(level, source, message string) {
	_, _ = s.db.Exec(`INSERT INTO events(created_at, level, source, message) VALUES(?,?,?,?)`,
		time.Now().Unix(), level, source, message)
}

// Events returns the most recent log lines.
func (s *Store) Events(limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id, created_at, level, source, message
		FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e  Event
			ts int64
		)
		if err := rows.Scan(&e.ID, &ts, &e.Level, &e.Source, &e.Message); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
