package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshycord/internal/bridge"
	"meshycord/internal/config"
	"meshycord/internal/meshcore"
	"meshycord/internal/store"
)

// A page that renders fine when empty can still blow up the moment there is a
// row in it — a template error inside a {{range}} only fires when the range
// has something to iterate. So every fixture below is populated.
func newTestServer(t *testing.T) (*Server, *store.Store, *config.Store) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg, err := config.New(db)
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	// The console sections that are switched off by default in production are
	// switched ON here, so their handlers and templates stay exercised while
	// they are hidden. TestHiddenSectionsAreNotRouted covers the real default.
	ShowLinks, ShowMessages, ShowContacts = true, true, true
	t.Cleanup(func() { ShowLinks, ShowMessages, ShowContacts = false, false, false })

	br := bridge.New(cfg, db, discardLogger())
	srv, err := New(Options{
		Listen:  "127.0.0.1:0",
		DBPath:  filepath.Join(t.TempDir(), "test.sqlite"),
		Version: "test",
	}, cfg, db, br, discardLogger())
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return srv, db, cfg
}

func seed(t *testing.T, db *store.Store) {
	t.Helper()

	for _, r := range []struct {
		kind  store.RouteKind
		key   string
		label string
	}{
		{store.KindChannel, "0", "Public"},
		{store.KindRoom, "aabbccddeeff", "Ridge Room"},
		{store.KindDM, "112233445566", "Alice"},
	} {
		if _, err := db.PutRoute(r.kind, r.key, "chan-"+r.key, r.label); err != nil {
			t.Fatalf("put route: %v", err)
		}
	}
	if err := db.SetRoomPassword("aabbccddeeff", "fake-test-password"); err != nil {
		t.Fatalf("room password: %v", err)
	}

	contacts := []store.Contact{
		{PubKey: strings.Repeat("aa", 32), Prefix: "aaaaaaaaaaaa", Type: meshcore.AdvTypeChat,
			Name: "Alice", OutPathLen: 2, LastAdvert: time.Now().Add(-time.Hour), Lat: 45.1, Lon: -122.9},
		{PubKey: strings.Repeat("bb", 32), Prefix: "bbbbbbbbbbbb", Type: meshcore.AdvTypeRoom,
			Name: "Ridge Room", OutPathLen: 255},
		{PubKey: strings.Repeat("cc", 32), Prefix: "cccccccccccc", Type: meshcore.AdvTypeRepeater,
			Name: "Hilltop 🏔", OutPathLen: 1, LastAdvert: time.Now()},
		{PubKey: strings.Repeat("dd", 32), Prefix: "dddddddddddd", Type: meshcore.AdvTypeSensor, Name: ""},
	}
	if err := db.ReplaceContacts(contacts); err != nil {
		t.Fatalf("contacts: %v", err)
	}

	// One message of every shape the templates branch on.
	msgs := []store.Message{
		{Direction: "in", Kind: store.KindChannel, MeshKey: "0", PeerLabel: "Public",
			Body: "hello from the mesh", HaveHops: true, Hops: 3, HaveSNR: true, SNR: -7.25,
			Delivery: store.DeliveryReceived},
		{Direction: "in", Kind: store.KindRoom, MeshKey: "aabbccddeeff", PeerLabel: "Ridge Room",
			Author: "Bob", Body: "a room post", HaveHops: false, PathRaw: 255,
			Delivery: store.DeliveryReceived},
		{Direction: "out", Kind: store.KindDM, MeshKey: "112233445566", PeerLabel: "Alice",
			Body: "on my way", Delivery: store.DeliveryDelivered, RoundTrip: 8500 * time.Millisecond,
			DiscordUsr: "cartpauj"},
		{Direction: "out", Kind: store.KindChannel, MeshKey: "0", PeerLabel: "Public",
			Body: "[1/2] a split message", Delivery: store.DeliveryTransmitted,
			ChunkIndex: 1, ChunkTotal: 2},
		{Direction: "out", Kind: store.KindDM, MeshKey: "112233445566", PeerLabel: "Alice",
			Body: "never arrived", Delivery: store.DeliveryFailed},
		{Direction: "out", Kind: store.KindRoom, MeshKey: "aabbccddeeff", PeerLabel: "Ridge Room",
			Body: "too long to send", Delivery: store.DeliveryRefused},
		{Direction: "out", Kind: store.KindDM, MeshKey: "112233445566", PeerLabel: "Alice",
			Body: "waiting", Delivery: store.DeliveryPending, Ack: 1234},
	}
	for _, m := range msgs {
		if _, err := db.InsertMessage(m); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}

	db.LogEvent("info", "admin", "linked room aabbccddeeff")
	db.LogEvent("warn", "mesh", "link to the node went down")
	db.LogEvent("error", "discord", "setup failed: no server id")
}

// get fetches a page. With no cookie it is an anonymous request, which since
// the console gained a default password means a redirect to /login — so most
// callers pass the one from session().
func get(t *testing.T, h http.Handler, path string, jar ...*http.Cookie) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range jar {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

// session logs in through the real handler and returns the session cookie, so
// every page test also exercises the login path rather than a forged cookie.
func session(t *testing.T, srv *Server, user, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Value != "" {
			return c
		}
	}
	t.Fatalf("login as %q did not set a session cookie (status %d)", user, rec.Code)
	return nil
}

// defaultSession logs in with the shipped credentials.
func defaultSession(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	return session(t, srv, config.DefaultUsername, config.DefaultPassword)
}

func TestEveryPageRendersWithData(t *testing.T) {
	srv, db, _ := newTestServer(t)
	seed(t, db)
	h := srv.Handler()
	ck := defaultSession(t, srv)

	for _, tc := range []struct {
		path     string
		contains []string
	}{
		{"/", []string{"Dashboard", "hello from the mesh", "3 hops", "snr -7.2"}},
		{"/messages", []string{"Messages", "a room post", "via known path", "delivered",
			"transmitted", "failed", "refused", "awaiting ack", "part 1/2", "via Bob"}},
		{"/links", []string{"Links", "Ridge Room", "aabbccddeeff", "mesh channel",
			"room server", "password stored"}},
		{"/contacts", []string{"Contacts", "Alice", "Hilltop 🏔", "repeater", "sensor",
			"companion", "45.1000, -122.9000", "unnamed"}},
		{"/logs", []string{"Activity", "linked room", "went down", "setup failed"}},
		{"/settings", []string{"Settings", "Bot token", "Radio link", "Pairing PIN",
			"Console login", "Danger zone"}},
		{"/fragment/status", []string{"Radio", "Discord", "Traffic"}},
		{"/fragment/recent", []string{"hello from the mesh"}},
		{"/fragment/events", []string{"linked room"}},
		{"/login", []string{"Sign in"}},
		{"/healthz", []string{"ok"}},
	} {
		code, body := get(t, h, tc.path, ck)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d", tc.path, code)
			continue
		}
		for _, want := range tc.contains {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: missing %q", tc.path, want)
			}
		}
		// html/template writes what it has and then stops on an error, so a
		// broken template shows up as a page that never reaches its footer.
		if tc.path != "/healthz" && tc.path != "/login" && !strings.HasPrefix(tc.path, "/fragment") {
			if !strings.Contains(body, "</html>") {
				t.Errorf("GET %s: page is truncated — a template failed mid-render", tc.path)
			}
		}
	}
}

func TestMessageFiltersAndPaging(t *testing.T) {
	srv, db, _ := newTestServer(t)
	seed(t, db)
	h := srv.Handler()
	ck := defaultSession(t, srv)

	code, body := get(t, h, "/messages?kind=room", ck)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, "a room post") {
		t.Error("room filter dropped a room message")
	}
	if strings.Contains(body, "hello from the mesh") {
		t.Error("room filter let a channel message through")
	}

	_, body = get(t, h, "/messages?q=never", ck)
	if !strings.Contains(body, "never arrived") {
		t.Error("search did not find a message by body text")
	}
	if strings.Contains(body, "on my way") {
		t.Error("search returned a message it should not have")
	}

	// An unknown kind must be ignored rather than returning nothing.
	_, body = get(t, h, "/messages?kind=nonsense", ck)
	if !strings.Contains(body, "on my way") {
		t.Error("an invalid kind filter hid everything")
	}
}

func TestContactFilters(t *testing.T) {
	srv, db, _ := newTestServer(t)
	seed(t, db)
	h := srv.Handler()
	ck := defaultSession(t, srv)

	_, body := get(t, h, "/contacts?type=2", ck)
	if !strings.Contains(body, "Hilltop") {
		t.Error("repeater filter dropped the repeater")
	}
	if strings.Contains(body, ">Alice<") {
		t.Error("repeater filter let a companion through")
	}

	_, body = get(t, h, "/contacts?q=alice", ck)
	if !strings.Contains(body, "Alice") {
		t.Error("name search failed")
	}
}

// The console holds the bot token. It must never render back to a browser.
func TestBotTokenIsNeverRendered(t *testing.T) {
	srv, db, cfg := newTestServer(t)
	seed(t, db)
	ck := defaultSession(t, srv)
	const token = "fake-bot-token-not-a-real-credential"
	if err := cfg.SetBotToken(token); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	for _, path := range []string{"/", "/settings", "/links", "/contacts", "/messages", "/logs"} {
		_, body := get(t, h, path, ck)
		if strings.Contains(body, token) {
			t.Errorf("GET %s leaked the bot token", path)
		}
	}
	// The room password must not leak either.
	_, body := get(t, h, "/links", ck)
	if strings.Contains(body, "fake-test-password") {
		t.Error("/links leaked a stored room password")
	}
}

// The console ships with a default password, so it asks for a login from the
// first request rather than serving the bot token to whoever reaches the port.
//
// The banner is the part that matters. admin/admin is published, so a default
// that quietly looked like a configured password would be worse than no password
// at all — nobody fixes what nothing complains about.
func TestDefaultPasswordStillWarnsUntilChanged(t *testing.T) {
	srv, db, cfg := newTestServer(t)
	seed(t, db)
	h := srv.Handler()

	// Anonymous requests do not get in, even on a fresh install.
	if code, _ := get(t, h, "/links"); code != http.StatusFound {
		t.Errorf("an anonymous request got %d, want a redirect to /login", code)
	}

	ck := defaultSession(t, srv)
	code, body := get(t, h, "/links", ck)
	if code != http.StatusOK {
		t.Fatalf("the default credentials did not work: got %d", code)
	}
	if !strings.Contains(body, "default password") {
		t.Error("no warning that the default password is still in use")
	}

	// A wrong password must still be refused.
	if cfg.CheckPassword("not-the-default") {
		t.Error("any password was accepted")
	}

	// Changing it silences the banner and invalidates the old session.
	if err := cfg.SetPassword("fake-console-password"); err != nil {
		t.Fatal(err)
	}
	if cfg.IsDefaultPassword() {
		t.Error("still reported as the default after being changed")
	}
	if cfg.CheckPassword(config.DefaultPassword) {
		t.Error("the default password still works after being changed")
	}
	if code, _ := get(t, h, "/links", ck); code != http.StatusFound {
		t.Error("the session from before the password change still works")
	}

	ck2 := session(t, srv, config.DefaultUsername, "fake-console-password")
	_, body = get(t, h, "/links", ck2)
	if strings.Contains(body, "default password") {
		t.Error("the warning survived the password being changed")
	}
}

func TestPostsRequireACSRFToken(t *testing.T) {
	srv, db, _ := newTestServer(t)
	seed(t, db)
	h := srv.Handler()

	ck := defaultSession(t, srv)

	// Logged in on purpose: without a session these would redirect to /login and
	// the CSRF check would never be reached, so the test would pass for the
	// wrong reason.
	post := func(path string, form url.Values) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, path := range []string{
		"/links/unlink", "/links/tidy", "/contacts/add", "/contacts/remove", "/settings/save",
	} {
		if code := post(path, url.Values{"csrf": {"wrong"}}); code != http.StatusForbidden {
			t.Errorf("POST %s with a bad token = %d, want 403", path, code)
		}
		if code := post(path, url.Values{}); code != http.StatusForbidden {
			t.Errorf("POST %s with no token = %d, want 403", path, code)
		}
	}

	// A GET on a POST-only route must not act.
	req := httptest.NewRequest(http.MethodGet, "/settings/reset", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /settings/reset = %d, want 405", rec.Code)
	}
}

func TestUnlinkThroughTheConsole(t *testing.T) {
	srv, db, _ := newTestServer(t)
	seed(t, db)
	h := srv.Handler()

	ck := defaultSession(t, srv)

	// Take a real token off a rendered page, exactly as a browser would.
	req := httptest.NewRequest(http.MethodGet, "/links", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	token := extractCSRF(string(body))
	if token == "" {
		t.Fatal("no CSRF token in the rendered page")
	}

	form := url.Values{"csrf": {token}, "kind": {"room"}, "key": {"aabbccddeeff"}}
	req = httptest.NewRequest(http.MethodPost, "/links/unlink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unlink returned %d", rec.Code)
	}

	if _, err := db.Route(store.KindRoom, "aabbccddeeff"); err == nil {
		t.Error("the link is still there after unlinking")
	}
	// Unlinking a room server must not leave its password behind.
	if db.HasRoomPassword("aabbccddeeff") {
		t.Error("the stored room password survived the unlink")
	}
}

func extractCSRF(body string) string {
	const marker = `name="csrf" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// discardLogger keeps test output readable; the bridge logs a lot at startup.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Links, message history and contacts are managed from the Discord admin
// channel. They must be genuinely unreachable in the console, not merely
// unlinked in the nav.
func TestHiddenSectionsAreNotRouted(t *testing.T) {
	srv, db, _ := newTestServer(t)
	ck := defaultSession(t, srv)
	ShowLinks, ShowMessages, ShowContacts = false, false, false
	seed(t, db)
	h := srv.Handler()

	for _, path := range []string{
		"/links", "/links/add", "/links/unlink", "/links/tidy", "/links/rediscover",
		"/messages",
		"/contacts", "/contacts/add", "/contacts/remove", "/contacts/refresh",
	} {
		if code, _ := get(t, h, path, ck); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 while the section is switched off", path, code)
		}
	}

	// What remains must still work, and must not advertise what is gone.
	for _, path := range []string{"/", "/logs", "/settings"} {
		code, body := get(t, h, path, ck)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d", path, code)
			continue
		}
		for _, gone := range []string{`href="/links"`, `href="/messages"`, `href="/contacts"`} {
			if strings.Contains(body, gone) {
				t.Errorf("GET %s still links to %s", path, gone)
			}
		}
	}
}

// Attempts are only ever pruned for the address being checked, so an address
// that tries once and never returns keeps its entry for the life of the process.
// Anything scanning the network drives that, and the box this runs on may have
// 512 MB to spend.
//
// Timestamps are planted directly rather than slept through: the leak is about
// entries outliving their window, and a test should not take a minute to say so.
func TestLoginLimiterSweepsStaleAddresses(t *testing.T) {
	l := newLoginLimiter()
	stale := time.Now().Add(-2 * loginWindow)
	for i := 0; i < loginSweepAt; i++ {
		l.attempts[fmt.Sprintf("10.0.%d.%d", i/256, i%256)] = []time.Time{stale}
	}

	// The next real attempt is what triggers the sweep.
	if !l.allow("192.0.2.7") {
		t.Fatal("a first attempt was refused")
	}
	if got := len(l.attempts); got != 1 {
		t.Errorf("limiter holds %d addresses; only the live one should remain", got)
	}

	// Sweeping must not weaken the limit. One attempt is already recorded above.
	for i := 1; i < loginMaxTries; i++ {
		if !l.allow("192.0.2.7") {
			t.Fatalf("attempt %d was refused before the limit", i+1)
		}
	}
	if l.allow("192.0.2.7") {
		t.Error("the limit stopped applying after a sweep")
	}
}

// A live address must survive a sweep, or a burst of traffic from elsewhere
// would clear somebody's attempt history and hand them a fresh set of tries.
func TestLoginLimiterKeepsLiveAttemptsThroughASweep(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < loginMaxTries; i++ {
		l.allow("192.0.2.7")
	}
	if l.allow("192.0.2.7") {
		t.Fatal("setup: the limit did not apply")
	}

	l.sweep(time.Now())

	if l.allow("192.0.2.7") {
		t.Error("a sweep reset a limited address, so the limit can be bypassed by filling the map")
	}
}
