package config

import (
	"path/filepath"
	"testing"
	"time"

	"meshycord/internal/store"
)

func newCfg(t *testing.T) *Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := New(db)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// The trust window follows the keep-alive when the keep-alive is the thing
// actually maintaining the session — otherwise a background refresh every four
// hours would be pointless, because the session would be distrusted long
// before it ran.
func TestRoomTrustWindowFollowsTheKeepAlive(t *testing.T) {
	c := newCfg(t)

	// A keep-alive longer than the TTL widens the window, with a margin for a
	// refresh that was late or lost.
	if err := c.SetRoomSessionSeconds(600); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRoomKeepAliveSeconds(14400); err != nil {
		t.Fatal(err)
	}
	if got, want := c.RoomTrustWindow(), 5*time.Hour; got != want {
		t.Errorf("trust window = %v, want %v (4h + a quarter)", got, want)
	}

	// A TTL longer than the keep-alive wins: it is an explicit instruction to
	// re-check less often, and the refresh does not shorten it.
	if err := c.SetRoomSessionSeconds(86400); err != nil {
		t.Fatal(err)
	}
	if got, want := c.RoomTrustWindow(), 24*time.Hour; got != want {
		t.Errorf("trust window = %v, want %v", got, want)
	}

	// Turning the keep-alive off leaves the TTL alone.
	if err := c.SetRoomKeepAliveSeconds(0); err != nil {
		t.Fatal(err)
	}
	if got, want := c.RoomTrustWindow(), 24*time.Hour; got != want {
		t.Errorf("trust window = %v, want %v", got, want)
	}
}

// "Log in before every message" is a deliberate choice for certainty, and a
// background job is not allowed to overrule it.
func TestZeroTTLIsNotOverriddenByTheKeepAlive(t *testing.T) {
	c := newCfg(t)
	if err := c.SetRoomSessionSeconds(0); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRoomKeepAliveSeconds(14400); err != nil {
		t.Fatal(err)
	}
	if got := c.RoomTrustWindow(); got != 0 {
		t.Errorf("trust window = %v, want 0", got)
	}
}

// The defaults reflect what the room firmware actually does: a login does not
// expire, so the re-check interval is hours rather than minutes, and the
// background refresh sits comfortably inside it.
func TestRoomDefaultsRefreshInsideTheTrustWindow(t *testing.T) {
	c := newCfg(t)
	if ka, w := c.RoomKeepAlive(), c.RoomTrustWindow(); ka >= w {
		t.Errorf("keep-alive %v does not fit inside the trust window %v", ka, w)
	}
}
