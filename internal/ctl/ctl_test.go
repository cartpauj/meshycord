package ctl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serveTest(t *testing.T, h Handler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctl.sock")
	s := NewServer(path, h, testLogger())
	if err := s.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
	t.Cleanup(func() { cancel(); s.Close() })
	return path
}

func TestRoundTrip(t *testing.T) {
	path := serveTest(t, func(_ context.Context, req Request) Response {
		if req.Op != "cli" || req.Target != "ridge" || req.Arg != "clock" {
			return Response{Err: "unexpected request"}
		}
		return Response{OK: true, Out: "18:22 - 13/8/2026 UTC"}
	})

	resp, err := Do(path, Request{Op: "cli", Target: "ridge", Arg: "clock"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.OK || resp.Out != "18:22 - 13/8/2026 UTC" {
		t.Errorf("response = %+v", resp)
	}
}

func TestErrorsComeBackAsWords(t *testing.T) {
	path := serveTest(t, func(_ context.Context, _ Request) Response {
		return Response{Err: "no admin password stored for Ridge"}
	})

	resp, err := Do(path, Request{Op: "cli"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Err, "no admin password") {
		t.Errorf("response = %+v", resp)
	}
}

// "Nothing came back" is not the same as "it failed", and the CLI exits with a
// different code for it, so the flag has to survive the round trip.
func TestNoReplyFlagSurvives(t *testing.T) {
	path := serveTest(t, func(_ context.Context, _ Request) Response {
		return Response{NoReply: true, Err: "no reply within 45s"}
	})

	resp, err := Do(path, Request{Op: "cli"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.NoReply {
		t.Error("the no-reply flag was lost in transit")
	}
}

// The likeliest failure by far, and "connection refused" on its own tells
// nobody that the daemon has to be running for this to work at all.
func TestMissingDaemonIsItsOwnError(t *testing.T) {
	_, err := Do(filepath.Join(t.TempDir(), "nothing-here.sock"), Request{Op: "list"}, time.Second)
	if !errors.Is(err, ErrNoDaemon) {
		t.Errorf("err = %v, want ErrNoDaemon", err)
	}
}

// The socket carries admin passwords on their way to a repeater. Anyone who
// can open it can reboot every repeater on the mesh.
func TestSocketIsNotReadableByOthers(t *testing.T) {
	path := serveTest(t, func(_ context.Context, _ Request) Response { return Response{OK: true} })

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", perm)
	}
}

// A daemon killed with SIGKILL can leave the file behind, and bind then fails
// with "address already in use" when nothing is running.
func TestAStaleSocketFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctl.sock")

	first := NewServer(path, func(_ context.Context, _ Request) Response { return Response{} }, testLogger())
	if err := first.Listen(); err != nil {
		t.Fatalf("first listen: %v", err)
	}
	// Close the listener WITHOUT removing the file, as a kill -9 would.
	first.ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Skip("this platform removed the socket file on close; nothing to test")
	}

	second := NewServer(path, func(_ context.Context, _ Request) Response { return Response{OK: true} }, testLogger())
	if err := second.Listen(); err != nil {
		t.Fatalf("a stale socket file blocked startup: %v", err)
	}
	second.Close()
}

// A live daemon must NOT be displaced by a second one: that would leave the
// first running and unreachable.
func TestALiveSocketIsNotStolen(t *testing.T) {
	path := serveTest(t, func(_ context.Context, _ Request) Response { return Response{OK: true} })

	second := NewServer(path, func(_ context.Context, _ Request) Response { return Response{} }, testLogger())
	err := second.Listen()
	if err == nil {
		second.Close()
		t.Fatal("a second daemon took over a socket that was already being served")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("err = %v, want it to say another meshycord is listening", err)
	}
}

// A mistyped -ctl path must not delete whatever it happens to point at.
func TestListenRefusesToReplaceARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "important.db")
	if err := os.WriteFile(path, []byte("real data"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := NewServer(path, func(_ context.Context, _ Request) Response { return Response{} }, testLogger())
	if err := s.Listen(); err == nil {
		t.Fatal("a regular file was replaced by a socket")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "real data" {
		t.Errorf("the file was damaged: %q, %v", b, err)
	}
}

func TestUnknownOperationsAreRejectedNotIgnored(t *testing.T) {
	path := serveTest(t, func(_ context.Context, req Request) Response {
		return Response{Err: "unknown operation " + req.Op}
	})

	resp, err := Do(path, Request{Op: "delete-everything"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.OK {
		t.Error("an unknown operation was reported as success")
	}
}
