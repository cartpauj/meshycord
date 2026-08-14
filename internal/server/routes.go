package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"meshycord/internal/bridge"
	"meshycord/internal/config"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// page is everything every template can reach. Keeping one type means the
// banners in the layout — no password, Discord rejected, setup unfinished —
// render on every page without each handler remembering to fill them in.
type page struct {
	Title       string
	Version     string
	DBPath      string
	Status      bridge.Status
	UptimeText  string
	HasPassword bool
	// DefaultPassword is true while the console still accepts the shipped
	// admin/admin. The banner keyed off HasPassword alone would go quiet the
	// moment a default existed, which is the failure mode worth avoiding.
	DefaultPassword bool
	HasToken        bool
	Missing         []string
	Flash           string
	FlashKind       string
	// Which console sections are switched on. Links, message history and
	// contacts live in the Discord admin channel for now.
	ShowLinks    bool
	ShowMessages bool
	ShowContacts bool
	// CSRF is the hidden input for forms. Per-request, so it cannot be a
	// template function — those bind once at parse time.
	CSRF template.HTML

	Cfg                  *config.Store
	Messages             []store.Message
	Events               []store.Event
	Routes               []store.Route
	Candidates           []linkCandidate
	Contacts             []store.Contact
	Channels             []meshcore.ChannelInfo
	SerialPorts          []string
	Filter               string
	Show                 string
	TypeStr              string
	Query                messageQuery
	NextBefore           int64
	ChunkCapacity        int
	ChunkGapMS           int
	RetentionDays        int
	RoomSessionSeconds   int
	RoomKeepAliveSeconds int
}

type messageQuery struct {
	Search  string
	KindStr string
}

type linkCandidate struct {
	Kind       store.RouteKind
	Key        string
	Label      string
	Hops       int
	LastAdvert time.Time
	Linked     bool
}

func (s *Server) newPage(r *http.Request, title string) *page {
	st := s.bridge.Status(s.opts.DBPath)
	p := &page{
		Title:           title,
		Version:         s.opts.Version,
		DBPath:          s.opts.DBPath,
		Status:          st,
		UptimeText:      humanDuration(st.Uptime),
		HasPassword:     s.cfg.HasPassword(),
		DefaultPassword: s.cfg.IsDefaultPassword(),
		HasToken:        s.cfg.BotToken() != "",
		Missing:         s.cfg.MissingSettings(),
		Cfg:             s.cfg,
		ShowLinks:       ShowLinks,
		ShowMessages:    ShowMessages,
		ShowContacts:    ShowContacts,
		CSRF: template.HTML(fmt.Sprintf(
			`<input type="hidden" name="csrf" value="%s">`, template.HTMLEscapeString(s.csrfToken(r)))),
	}
	if f := r.URL.Query().Get("msg"); f != "" {
		p.Flash = f
		p.FlashKind = "warn"
		if r.URL.Query().Get("ok") == "1" {
			p.FlashKind = "warn" // a success banner still uses the soft style
		}
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, name string, p *page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, p); err != nil {
		s.log.Error("could not render a page", "template", name, "err", err)
	}
}

// redirect sends the browser back with a message to show.
func redirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	if msg != "" {
		path += "?msg=" + urlEscape(msg)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	p := s.newPage(r, "Dashboard")
	p.Messages, _ = s.db.Messages(store.MessageQuery{Limit: 25})
	s.render(w, "dashboard.html", p)
}

func (s *Server) handleFragmentStatus(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "Dashboard")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "status-panel", p)
}

func (s *Server) handleFragmentRecent(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "Dashboard")
	p.Messages, _ = s.db.Messages(store.MessageQuery{Limit: 25})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "recent-messages", p)
}

func (s *Server) handleFragmentEvents(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "Activity")
	p.Events, _ = s.db.Events(200)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "event-list", p)
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := store.RouteKind(q.Get("kind"))
	if kind != "" && !kind.Valid() {
		kind = ""
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)

	const perPage = 100
	msgs, err := s.db.Messages(store.MessageQuery{
		Kind:   kind,
		Search: strings.TrimSpace(q.Get("q")),
		Before: before,
		Limit:  perPage,
	})
	if err != nil {
		s.log.Error("could not read message history", "err", err)
	}

	p := s.newPage(r, "Messages")
	p.Messages = msgs
	p.Query = messageQuery{Search: q.Get("q"), KindStr: string(kind)}
	if len(msgs) == perPage {
		p.NextBefore = msgs[len(msgs)-1].ID
	}
	s.render(w, "messages.html", p)
}

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "Links")
	p.Routes, _ = s.db.Routes()
	p.Filter = strings.TrimSpace(r.URL.Query().Get("q"))
	p.Show = r.URL.Query().Get("kind")
	if p.Show == "" {
		p.Show = "unlinked"
	}
	p.Candidates = s.linkCandidates(p.Show, p.Filter)
	s.render(w, "links.html", p)
}

// linkCandidates builds the "things you could link" table from the mirrored
// contacts plus the radio's channel slots.
func (s *Server) linkCandidates(show, filter string) []linkCandidate {
	filter = strings.ToLower(filter)
	var out []linkCandidate

	matches := func(label, key string) bool {
		if filter == "" {
			return true
		}
		return strings.Contains(strings.ToLower(label), filter) || strings.HasPrefix(key, filter)
	}

	if show != "rooms" && show != "people" {
		if sess := s.bridge.Link().Session(); sess != nil {
			for _, ci := range sess.Channels() {
				key := strconv.Itoa(int(ci.Index))
				_, err := s.db.Route(store.KindChannel, key)
				linked := err == nil
				if show == "unlinked" && linked {
					continue
				}
				if !matches(ci.Name, key) {
					continue
				}
				out = append(out, linkCandidate{
					Kind: store.KindChannel, Key: key, Label: ci.Name, Hops: 0xFF, Linked: linked,
				})
			}
		}
	}
	if show == "channels" {
		return out
	}

	typeFilter := -1
	switch show {
	case "rooms":
		typeFilter = meshcore.AdvTypeRoom
	case "people":
		typeFilter = meshcore.AdvTypeChat
	}

	contacts, _ := s.db.Contacts(typeFilter)
	for _, c := range contacts {
		// Repeaters and sensors cannot exchange messages, so linking one a
		// channel would produce a channel nothing ever arrives in.
		if c.Type != meshcore.AdvTypeChat && c.Type != meshcore.AdvTypeRoom {
			continue
		}
		kind := store.KindDM
		if c.Type == meshcore.AdvTypeRoom {
			kind = store.KindRoom
		}
		_, err := s.db.RouteByPrefix(c.Prefix)
		linked := err == nil
		if show == "unlinked" && linked {
			continue
		}
		label := c.Name
		if label == "" {
			label = c.Prefix
		}
		if !matches(label, c.Prefix) {
			continue
		}
		out = append(out, linkCandidate{
			Kind: kind, Key: c.Prefix, Label: label, Hops: c.OutPathLen,
			LastAdvert: c.LastAdvert, Linked: linked,
		})
		if len(out) >= 500 {
			break
		}
	}
	return out
}

func (s *Server) handleLinkAdd(w http.ResponseWriter, r *http.Request) {
	kind := store.RouteKind(r.FormValue("kind"))
	key := strings.TrimSpace(r.FormValue("key"))
	if !kind.Valid() || key == "" {
		redirect(w, r, "/links", "Nothing to link.")
		return
	}
	// CreateLink rather than Exec("add <key>"): here a mesh channel is
	// identified by its slot number, and a bare number in the admin console
	// means "row N of the last listing" instead.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	redirect(w, r, "/links", stripMarkdown(s.bridge.CreateLink(ctx, kind, key, "")))
}

func (s *Server) handleLinkRemove(w http.ResponseWriter, r *http.Request) {
	kind := store.RouteKind(r.FormValue("kind"))
	key := strings.TrimSpace(r.FormValue("key"))
	if !kind.Valid() || key == "" {
		redirect(w, r, "/links", "Nothing to unlink.")
		return
	}
	gone, err := s.db.DeleteRoute(kind, key)
	if err != nil {
		redirect(w, r, "/links", "Could not unlink: "+err.Error())
		return
	}
	if !gone {
		redirect(w, r, "/links", "That was not linked.")
		return
	}
	extra := ""
	if s.db.HasRoomPassword(key) {
		_ = s.db.SetRoomPassword(key, "")
		extra = " Its stored room password has been forgotten."
	}
	s.db.LogEvent("info", "web", fmt.Sprintf("unlinked %s %s", kind, key))
	redirect(w, r, "/links", "Unlinked. The Discord channel is left in place."+extra)
}

func (s *Server) handleLinkTidy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	redirect(w, r, "/links", stripMarkdown(s.bridge.Exec(ctx, "web", "tidy", "", "")))
}

func (s *Server) handleRediscover(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go func() {
		msg := s.bridge.Rediscover(ctx)
		s.log.Info("rediscover finished", "result", msg)
	}()
	redirect(w, r, "/links", "Rebuilding the Discord side. Reload in a few seconds.")
}

// ---------------------------------------------------------------------------
// Contacts
// ---------------------------------------------------------------------------

func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "Contacts")
	p.Filter = strings.TrimSpace(r.URL.Query().Get("q"))
	p.TypeStr = r.URL.Query().Get("type")

	typeFilter := -1
	if n, err := strconv.Atoi(p.TypeStr); err == nil {
		typeFilter = n
	}
	all, _ := s.db.Contacts(typeFilter)

	filter := strings.ToLower(p.Filter)
	for _, c := range all {
		if filter != "" &&
			!strings.Contains(strings.ToLower(c.Name), filter) &&
			!strings.HasPrefix(c.Prefix, filter) {
			continue
		}
		p.Contacts = append(p.Contacts, c)
		// A mesh can carry hundreds of contacts and a Pi Zero renders slowly.
		// Cap the page and let the search narrow it.
		if len(p.Contacts) >= 400 {
			break
		}
	}
	if sess := s.bridge.Link().Session(); sess != nil {
		p.Channels = sess.Channels()
	}
	s.render(w, "contacts.html", p)
}

func (s *Server) handleContactAdd(w http.ResponseWriter, r *http.Request) {
	sess := s.bridge.Link().Session()
	if sess == nil {
		redirect(w, r, "/contacts", "The radio is not connected.")
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	name := strings.TrimSpace(r.FormValue("name"))
	advType := byte(meshcore.AdvTypeChat)
	if r.FormValue("room") != "" {
		advType = meshcore.AdvTypeRoom
	}
	if name == "" {
		redirect(w, r, "/contacts", "A name is required: without one the contact cannot be found by name later.")
		return
	}
	if _, err := meshcore.ParsePubKey(key); err != nil {
		redirect(w, r, "/contacts", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := sess.AddContact(ctx, key, name, advType); err != nil {
		redirect(w, r, "/contacts", "The radio rejected that contact: "+err.Error())
		return
	}
	s.bridge.SyncContacts(ctx)
	s.db.LogEvent("info", "web", "added contact "+name)
	redirect(w, r, "/contacts", "Added "+name+".")
}

func (s *Server) handleContactRemove(w http.ResponseWriter, r *http.Request) {
	sess := s.bridge.Link().Session()
	if sess == nil {
		redirect(w, r, "/contacts", "The radio is not connected.")
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := sess.RemoveContact(ctx, key); err != nil {
		redirect(w, r, "/contacts", "The radio would not remove that contact — it may already be gone.")
		return
	}
	s.bridge.SyncContacts(ctx)
	redirect(w, r, "/contacts", "Removed. It comes back if that node adverts again.")
}

func (s *Server) handleContactRefresh(w http.ResponseWriter, r *http.Request) {
	sess := s.bridge.Link().Session()
	if sess == nil {
		redirect(w, r, "/contacts", "The radio is not connected.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	n, complete, err := sess.RefreshContacts(ctx)
	if err != nil {
		redirect(w, r, "/contacts", "Could not read the contacts: "+err.Error())
		return
	}
	s.bridge.SyncContacts(ctx)
	if !complete {
		redirect(w, r, "/contacts", fmt.Sprintf(
			"Read %d contacts, but the radio stopped answering before the end of its list — "+
				"this was a partial read, so nothing was removed.", n))
		return
	}
	redirect(w, r, "/contacts", fmt.Sprintf("Read %d contacts from the radio.", n))
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "Activity")
	p.Events, _ = s.db.Events(200)
	s.render(w, "logs.html", p)
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(r, "Settings")
	p.SerialPorts, _ = meshcore.FindSerialPorts()
	p.ChunkCapacity = bridge.ChunkCapacity(meshcore.MaxMsgLen, s.cfg.MaxChunks())
	p.ChunkGapMS = int(s.cfg.ChunkGap() / time.Millisecond)
	p.RetentionDays = int(s.cfg.Retention() / (24 * time.Hour))
	p.RoomSessionSeconds = int(s.cfg.RoomSessionTTL() / time.Second)
	p.RoomKeepAliveSeconds = int(s.cfg.RoomKeepAlive() / time.Second)
	s.render(w, "settings.html", p)
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var notes []string

	// Blank secret fields mean "keep the current value", so the page never has
	// to echo a token or a password back to the browser.
	if v := strings.TrimSpace(r.FormValue("token")); v != "" {
		if err := s.cfg.SetBotToken(v); err != nil {
			notes = append(notes, "token: "+err.Error())
		} else {
			notes = append(notes, "bot token updated")
		}
	}
	_ = s.cfg.SetGuildID(r.FormValue("guild"))

	transportChanged := false
	if v := r.FormValue("transport"); v != "" && v != s.cfg.Transport() {
		if err := s.cfg.SetTransport(v); err != nil {
			notes = append(notes, err.Error())
		} else {
			transportChanged = true
		}
	}
	if v := r.FormValue("serial_device"); v != s.cfg.SerialDevice() {
		_ = s.cfg.SetSerialDevice(v)
		transportChanged = true
	}
	if n, err := strconv.Atoi(r.FormValue("serial_baud")); err == nil && n != s.cfg.SerialBaud() {
		_ = s.cfg.SetSerialBaud(n)
		transportChanged = true
	}
	if v := r.FormValue("ble_name"); v != s.cfg.BLEName() {
		_ = s.cfg.SetBLEName(v)
		transportChanged = true
	}
	if v := r.FormValue("ble_addr"); !strings.EqualFold(v, s.cfg.BLEAddress()) {
		_ = s.cfg.SetBLEAddress(v)
		transportChanged = true
	}
	if v := r.FormValue("ble_pin"); v != s.cfg.BLEPin() {
		_ = s.cfg.SetBLEPin(v)
		transportChanged = true
	}
	if v := r.FormValue("tcp_addr"); v != s.cfg.TCPAddress() {
		_ = s.cfg.SetTCPAddress(v)
		transportChanged = true
	}

	_ = s.cfg.SetAutoCreateChannels(r.FormValue("auto_channels") != "")
	_ = s.cfg.SetAutoCreateRooms(r.FormValue("auto_rooms") != "")
	_ = s.cfg.SetAutoCreateDMs(r.FormValue("auto_dms") != "")

	if n, err := strconv.Atoi(r.FormValue("max_chunks")); err == nil {
		_ = s.cfg.SetMaxChunks(n)
	}
	if n, err := strconv.Atoi(r.FormValue("chunk_gap")); err == nil && n >= 0 {
		_ = s.cfg.SetChunkGapMS(n)
	}
	if n, err := strconv.Atoi(r.FormValue("retention")); err == nil && n >= 0 {
		_ = s.cfg.SetRetentionDays(n)
	}
	if n, err := strconv.Atoi(r.FormValue("room_session")); err == nil && n >= 0 {
		_ = s.cfg.SetRoomSessionSeconds(n)
	}
	if n, err := strconv.Atoi(r.FormValue("room_keepalive")); err == nil && n >= 0 {
		_ = s.cfg.SetRoomKeepAliveSeconds(n)
	}

	if v := strings.TrimSpace(r.FormValue("username")); v != "" && v != s.cfg.Username() {
		if err := s.cfg.SetUsername(v); err != nil {
			notes = append(notes, err.Error())
		}
	}
	if v := r.FormValue("password"); v != "" {
		if err := s.cfg.SetPassword(v); err != nil {
			notes = append(notes, "password: "+err.Error())
		} else {
			// Changing the password invalidates every session including this
			// one, so hand out a fresh cookie rather than bouncing the user to
			// the login page immediately after a successful save.
			s.issueCookie(w, r, s.cfg.Username())
			notes = append(notes, "password changed; other sessions signed out")
		}
	}

	if transportChanged {
		s.bridge.RebuildLink()
		notes = append(notes, "reconnecting to the radio")
	}
	s.db.LogEvent("info", "web", "settings saved")

	msg := "Settings saved."
	if len(notes) > 0 {
		msg += " " + strings.Join(notes, "; ") + "."
	}
	redirect(w, r, "/settings", msg)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	// Deleting dozens of channels takes far longer than a browser will wait,
	// and Discord's rate limits set the pace. Run it in the background and let
	// the Activity page report what happened.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		msg := s.bridge.Reset(ctx)
		s.log.Info("reset finished", "result", msg)
	}()
	redirect(w, r, "/settings", "Deleting everything the bridge created. Watch the Activity page.")
}

// stripMarkdown flattens the Discord formatting in a shared reply, since the
// console renders plain text.
func stripMarkdown(s string) string {
	r := strings.NewReplacer("**", "", "`", "", "\n", " ")
	return strings.TrimSpace(r.Replace(s))
}
