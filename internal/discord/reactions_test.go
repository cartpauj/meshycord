package discord

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captures the request a call actually makes.
func capture(t *testing.T, fn func(c *Client)) (method, path string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(func() string { return "test-token" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.BaseURL = srv.URL
	fn(c)
	return method, path
}

// Removing our own reaction and removing somebody else's are different
// endpoints with different permission requirements. Sending @me when we meant
// a user silently removes the wrong thing.
func TestReactionRoutes(t *testing.T) {
	ctx := context.Background()

	m, p := capture(t, func(c *Client) { _ = c.React(ctx, "CH", "MSG", "✅") })
	if m != http.MethodPut {
		t.Errorf("React uses %s, want PUT", m)
	}
	if want := "/channels/CH/messages/MSG/reactions/✅/@me"; p != want {
		t.Errorf("React path = %q, want %q", p, want)
	}

	m, p = capture(t, func(c *Client) { _ = c.Unreact(ctx, "CH", "MSG", "🔄") })
	if m != http.MethodDelete {
		t.Errorf("Unreact uses %s, want DELETE", m)
	}
	if want := "/channels/CH/messages/MSG/reactions/🔄/@me"; p != want {
		t.Errorf("Unreact path = %q, want %q — it must only ever remove our own", p, want)
	}

	m, p = capture(t, func(c *Client) { _ = c.UnreactUser(ctx, "CH", "MSG", "🔄", "USER42") })
	if m != http.MethodDelete {
		t.Errorf("UnreactUser uses %s, want DELETE", m)
	}
	if want := "/channels/CH/messages/MSG/reactions/🔄/USER42"; p != want {
		t.Errorf("UnreactUser path = %q, want %q — @me here would remove the wrong reaction", p, want)
	}
}

// A reaction that is already gone is a success, not an error: two clicks in
// quick succession must not produce a scary log line.
func TestRemovingAMissingReactionIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Unknown Emoji","code":10014}`))
	}))
	defer srv.Close()

	c := NewClient(func() string { return "t" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.BaseURL = srv.URL
	if err := c.UnreactUser(context.Background(), "CH", "MSG", "🔄", "U"); err != nil {
		t.Errorf("removing an absent reaction reported an error: %v", err)
	}
	if err := c.Unreact(context.Background(), "CH", "MSG", "🔄"); err != nil {
		t.Errorf("removing an absent own-reaction reported an error: %v", err)
	}
}
