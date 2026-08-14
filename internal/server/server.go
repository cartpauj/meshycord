// Package server is the web console: stdlib net/http, html/template and an
// embedded htmx. No framework, no npm, no build step.
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"meshycord/internal/bridge"
	"meshycord/internal/config"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
	"meshycord/internal/web"
)

// Console sections that are currently switched off.
//
// Links, message history and contact management are all driven from the
// Discord admin channel instead — that works from anywhere, which the console
// does not, and having one place to manage things means the two cannot
// disagree about what happened.
//
// These are switches rather than deletions on purpose. Every handler,
// template and query behind them still exists and still compiles; flip one to
// true and the page and its nav link come straight back.
//
// Variables rather than constants so the tests can switch them on and keep
// exercising the hidden handlers. Code that is never run is code that quietly
// rots, and the whole point of hiding rather than deleting is to be able to
// bring it back.
var (
	ShowLinks    = false
	ShowMessages = false
	ShowContacts = false
)

// Options configures the console.
type Options struct {
	Listen  string
	DBPath  string
	Version string
}

// Server is the console.
type Server struct {
	opts   Options
	cfg    *config.Store
	db     *store.Store
	bridge *bridge.Bridge
	log    *slog.Logger
	tmpl   *template.Template
	static fs.FS

	loginLimit *loginLimiter
}

// New builds the console.
func New(opts Options, cfg *config.Store, db *store.Store, br *bridge.Bridge, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		opts:       opts,
		cfg:        cfg,
		db:         db,
		bridge:     br,
		log:        log,
		loginLimit: newLoginLimiter(),
	}

	tmpl, err := template.New("").Funcs(s.funcs()).ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = tmpl

	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}
	s.static = sub
	return s, nil
}

// Handler returns the console's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("/static/", http.StripPrefix("/static/", s.staticHandler()))

	authed := http.NewServeMux()
	authed.HandleFunc("/", s.handleDashboard)

	// Switched-off sections are simply not routed, so a stale bookmark gets a
	// clean 404 rather than a half-working page.
	if ShowMessages {
		authed.HandleFunc("/messages", s.handleMessages)
	}
	if ShowLinks {
		authed.HandleFunc("/links", s.handleLinks)
		authed.HandleFunc("/links/add", s.post(s.handleLinkAdd))
		authed.HandleFunc("/links/unlink", s.post(s.handleLinkRemove))
		authed.HandleFunc("/links/tidy", s.post(s.handleLinkTidy))
		authed.HandleFunc("/links/rediscover", s.post(s.handleRediscover))
	}
	if ShowContacts {
		authed.HandleFunc("/contacts", s.handleContacts)
		authed.HandleFunc("/contacts/add", s.post(s.handleContactAdd))
		authed.HandleFunc("/contacts/remove", s.post(s.handleContactRemove))
		authed.HandleFunc("/contacts/refresh", s.post(s.handleContactRefresh))
	}
	authed.HandleFunc("/logs", s.handleLogs)
	authed.HandleFunc("/settings", s.handleSettings)
	authed.HandleFunc("/settings/save", s.post(s.handleSettingsSave))
	authed.HandleFunc("/settings/reset", s.post(s.handleReset))
	authed.HandleFunc("/fragment/status", s.handleFragmentStatus)
	authed.HandleFunc("/fragment/recent", s.handleFragmentRecent)
	authed.HandleFunc("/fragment/events", s.handleFragmentEvents)

	mux.Handle("/", s.requireLogin(authed))
	return s.recoverPanics(mux)
}

// ListenAndServe runs the console.
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:    s.opts.Listen,
		Handler: s.Handler(),
		// A Pi Zero serving a page with a few hundred rows is not fast. These
		// are generous on purpose; the risk being bounded is a stuck socket,
		// not a slow render.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.log.Info("console listening", "addr", s.opts.Listen)
	return srv.ListenAndServe()
}

func (s *Server) staticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets are embedded in the binary, so they only ever change when the
		// binary does. A long cache is safe and saves a Pi Zero a lot of work.
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving a page", "path", r.URL.Path, "panic", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

const (
	cookieName = "meshycord_session"
	// A console left open in a browser tab should not log itself out. Bounded
	// so an abandoned session still expires.
	cookieMaxAge = 30 * 24 * time.Hour
)

// authOptional reports whether the console will serve a page without a login.
//
// It will not. A fresh install accepts config.DefaultUsername and
// DefaultPassword, so the console asks for credentials from the first request
// instead of handing the bot token to anyone who can reach the port. Those
// credentials are published, so every page keeps warning until the password is
// changed — see IsDefaultPassword.
func (s *Server) authOptional() bool { return !s.cfg.HasPassword() }

func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authOptional() || s.sessionUser(r) != "" {
			next.ServeHTTP(w, r)
			return
		}
		target := "/login"
		if r.Method == http.MethodGet {
			target += "?next=" + r.URL.Path
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

func (s *Server) sign(payload string) string {
	m := hmac.New(sha256.New, s.cfg.SessionKey())
	_, _ = m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) issueCookie(w http.ResponseWriter, r *http.Request, user string) {
	exp := time.Now().Add(cookieMaxAge).Unix()
	// The password generation is baked in, so changing the password (or the
	// username) invalidates every outstanding session.
	payload := fmt.Sprintf("%s|%d|%d", user, s.cfg.PasswordGeneration(), exp)
	value := base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + s.sign(payload)))
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		// Lax, not Strict: Strict blocks the cookie on the redirected GET after
		// the login POST, which leaves the user in a redirect loop back to the
		// login page. Cross-site POSTs are blocked by the CSRF token instead.
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  time.Now().Add(cookieMaxAge),
	})
}

func (s *Server) sessionUser(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return ""
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return ""
	}
	user, gen, exp, mac := parts[0], parts[1], parts[2], parts[3]
	if !hmac.Equal([]byte(mac), []byte(s.sign(user+"|"+gen+"|"+exp))) {
		return ""
	}
	if g, err := strconv.ParseUint(gen, 10, 64); err != nil || g != s.cfg.PasswordGeneration() {
		return ""
	}
	if e, err := strconv.ParseInt(exp, 10, 64); err != nil || time.Now().Unix() > e {
		return ""
	}
	if user != s.cfg.Username() {
		return ""
	}
	return user
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if r.Method != http.MethodPost {
		s.renderLogin(w, "", next)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, "Could not read that form.", next)
		return
	}
	if v := r.FormValue("next"); v != "" {
		next = v
	}

	// A slow hash is the whole point of bcrypt, and on a Pi Zero it is slow
	// enough to be a denial-of-service lever. Rate limit by source.
	if !s.loginLimit.allow(clientIP(r)) {
		s.renderLogin(w, "Too many attempts. Wait a minute and try again.", next)
		return
	}

	user := r.FormValue("username")
	pass := r.FormValue("password")
	// Always run the password check even when the username is wrong, so the
	// timing cannot be used to discover a valid username.
	ok := s.cfg.CheckPassword(pass)
	if !ok || user != s.cfg.Username() {
		s.log.Warn("failed console login", "ip", clientIP(r))
		s.renderLogin(w, "Wrong username or password.", next)
		return
	}

	s.loginLimit.reset(clientIP(r))
	s.issueCookie(w, r, user)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) renderLogin(w http.ResponseWriter, errMsg, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_ = s.tmpl.ExecuteTemplate(w, "login.html", map[string]any{
		"Error": errMsg,
		"Next":  next,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// csrfToken binds a form to this session.
//
// The session cookie is SameSite=Lax, which already blocks cross-site POSTs in
// every current browser; this is the belt to that pair of braces, and it also
// covers the no-password first-run state where there is no cookie at all.
func (s *Server) csrfToken(r *http.Request) string {
	seed := "anonymous"
	if c, err := r.Cookie(cookieName); err == nil {
		seed = c.Value
	}
	return s.sign("csrf|" + seed)
}

func (s *Server) checkCSRF(r *http.Request) bool {
	return hmac.Equal([]byte(r.FormValue("csrf")), []byte(s.csrfToken(r)))
}

// post wraps a handler so it only accepts POST, and only with a valid token.
func (s *Server) post(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !s.checkCSRF(r) {
			s.log.Warn("rejected a form with a bad CSRF token", "path", r.URL.Path, "ip", clientIP(r))
			http.Error(w, "this form has expired — reload the page and try again", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// ---------------------------------------------------------------------------
// Login rate limiting
// ---------------------------------------------------------------------------

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string][]time.Time{}}
}

const (
	loginWindow   = time.Minute
	loginMaxTries = 8
	// loginSweepAt is when a sweep of the whole map becomes worth its cost.
	// Attempts are only ever pruned for the address being checked, so without
	// this an address that tries once and never returns keeps its entry for the
	// life of the process — a slow leak on a box with 512 MB to spend, driven by
	// anything that scans the network.
	loginSweepAt = 1024
)

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var kept []time.Time
	for _, t := range l.attempts[ip] {
		if now.Sub(t) < loginWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= loginMaxTries {
		l.attempts[ip] = kept
		return false
	}
	if len(l.attempts) >= loginSweepAt {
		l.sweep(now)
	}
	l.attempts[ip] = append(kept, now)
	return true
}

// sweep drops addresses with nothing left inside the window. Caller holds mu.
func (l *loginLimiter) sweep(now time.Time) {
	for addr, ts := range l.attempts {
		live := false
		for _, t := range ts {
			if now.Sub(t) < loginWindow {
				live = true
				break
			}
		}
		if !live {
			delete(l.attempts, addr)
		}
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.attempts, ip)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	// No X-Forwarded-For: this console is meant to be reached directly on a
	// LAN, and trusting that header from an untrusted source would let anyone
	// spoof their way around the rate limit.
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"ago":   humanAgo,
		"stamp": func(t time.Time) string { return t.Format("2006-01-02 15:04:05") },
		"bytes": humanBytes,
		"shortKey": func(k string) string {
			if len(k) > 16 {
				return k[:16] + "…"
			}
			return k
		},
		"kindLabel": func(k store.RouteKind) string {
			switch k {
			case store.KindChannel:
				return "mesh channel"
			case store.KindRoom:
				return "room server"
			case store.KindDM:
				return "direct"
			}
			return string(k)
		},
		"advType":   func(t int) string { return meshcore.AdvTypeName(byte(t)) },
		"hopsLabel": bridge.HopsLabel,
		"hopsText": func(m store.Message) string {
			// Same wording as Discord gets, and for the same reason: a stored
			// path can be many hops and says nothing about distance, so it must
			// not read as "nearby".
			var out string
			switch {
			case !m.HaveHops:
				out = "via known path"
			case m.Hops == 0:
				out = "heard direct"
			case m.Hops == 1:
				out = "1 hop"
			default:
				out = fmt.Sprintf("%d hops", m.Hops)
			}
			if m.HaveSNR {
				out += fmt.Sprintf(", snr %.1f", m.SNR)
			}
			return out
		},
		"deliveryLabel": deliveryLabel,
		"deliveryClass": deliveryClass,
		"levelClass": func(level string) string {
			switch level {
			case "error":
				return "down"
			case "warn":
				return "warn"
			default:
				return "mute"
			}
		},
		"roundTrip": func(d time.Duration) string {
			if d <= 0 {
				return ""
			}
			return "acked in " + d.Round(100*time.Millisecond).String()
		},
		"portListed": func(ports []string, want string) bool {
			for _, p := range ports {
				if p == want {
					return true
				}
			}
			return false
		},
		"roomLoggedIn":    s.bridgeRoomLoggedIn,
		"roomHasPassword": func(prefix string) bool { return s.db.HasRoomPassword(prefix) },
		"channelLinked": func(idx byte) bool {
			_, err := s.db.Route(store.KindChannel, strconv.Itoa(int(idx)))
			return err == nil
		},
	}
}

func (s *Server) bridgeRoomLoggedIn(prefix string) bool {
	return s.bridge.RoomLoggedIn(prefix)
}

func deliveryLabel(state string) string {
	switch state {
	case store.DeliveryDelivered:
		return "delivered"
	case store.DeliveryFailed:
		return "failed"
	case store.DeliveryTransmitted:
		return "transmitted"
	case store.DeliveryHeard:
		return "repeated by the mesh"
	case store.DeliveryPending:
		return "awaiting ack"
	case store.DeliveryRefused:
		return "refused"
	}
	return state
}

func deliveryClass(state string) string {
	switch state {
	case store.DeliveryDelivered, store.DeliveryHeard:
		return "badge up"
	case store.DeliveryFailed, store.DeliveryRefused:
		return "badge down"
	case store.DeliveryPending:
		return "badge warn"
	}
	return "badge mute"
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2 Jan 2006")
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
