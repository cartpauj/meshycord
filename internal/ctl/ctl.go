// Package ctl is the local control socket: how meshycord-cli reaches the
// running bridge.
//
// It exists because of a hardware fact rather than a design preference. A
// MeshCore node serves exactly one client at a time, and the bridge holds that
// slot for as long as it is running. A second program cannot open the radio to
// run a command — not over USB, not over BLE, not over TCP. So the CLI does
// not talk to the node at all: it asks the process that already has the node
// to do the work, and prints what comes back.
//
// A Unix socket rather than an HTTP endpoint on the web console, because the
// console has a password and a session cookie, and requiring those to run a
// command from the same machine that hosts the daemon would be ceremony with
// no security benefit. Access is filesystem permissions instead: the socket is
// created 0600 and owned by whoever runs the daemon, so reaching it means
// already being root or the service user.
package ctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultSocket is where the daemon listens and the CLI looks.
//
// /run is a tmpfs on every systemd system, which is right for a socket: it
// disappears on reboot, so a stale one from an unclean shutdown can never
// mislead the CLI into thinking a dead daemon is alive. The systemd unit
// creates the directory with RuntimeDirectory=meshycord.
const DefaultSocket = "/run/meshycord/ctl.sock"

// Request is one command from the CLI.
type Request struct {
	// Op is what to do: "cli", "login", "list", "status", "clock", "clock-sync".
	Op string `json:"op"`
	// Target names the repeater or room server, by key prefix or name.
	Target string `json:"target,omitempty"`
	// Arg is the command text, or the password for a login.
	Arg string `json:"arg,omitempty"`
}

// Response is what the daemon says back.
type Response struct {
	OK bool `json:"ok"`
	// Out is the answer to print on stdout.
	Out string `json:"out,omitempty"`
	// Err explains a failure, in words meant for a person.
	Err string `json:"err,omitempty"`
	// NoReply marks the specific case of a command that was sent but never
	// answered, which is not necessarily a failure — `reboot` never answers.
	NoReply bool `json:"no_reply,omitempty"`
}

// Handler runs one request. Implemented by the bridge.
type Handler func(ctx context.Context, req Request) Response

// Server accepts CLI connections.
type Server struct {
	path    string
	handler Handler
	log     *slog.Logger
	ln      net.Listener
}

// NewServer prepares a control socket. Nothing listens until Serve.
func NewServer(path string, h Handler, log *slog.Logger) *Server {
	if path == "" {
		path = DefaultSocket
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{path: path, handler: h, log: log}
}

// Listen binds the socket.
//
// A leftover socket file is removed first. On a tmpfs there should never be
// one, but a daemon killed with SIGKILL on a system where /run is not tmpfs
// leaves the file behind, and bind then fails with "address already in use" —
// which reads as "another copy is running" when nothing is.
func (s *Server) Listen() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create %s: %w", dir, err)
	}

	// Only unlink something that is actually a dead socket. Refusing to touch
	// anything else means a mistyped -ctl path cannot delete a real file.
	if fi, err := os.Stat(s.path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket; refusing to replace it", s.path)
		}
		if c, err := net.DialTimeout("unix", s.path, time.Second); err == nil {
			c.Close()
			return fmt.Errorf("another meshycord is already listening on %s", s.path)
		}
		_ = os.Remove(s.path)
	}

	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return err
	}
	// Before anyone can connect: the socket carries admin passwords in the
	// clear on their way to a repeater.
	if err := os.Chmod(s.path, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("could not restrict %s: %w", s.path, err)
	}
	s.ln = ln
	return nil
}

// Serve handles connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

// Close stops listening and removes the socket file.
func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

// Path is where the server is listening.
func (s *Server) Path() string { return s.path }

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// A command crosses the mesh and back, so the deadline has to cover the
	// whole round trip with room to spare rather than a typical socket's
	// few seconds: a cold flood login plus the command's own reply, and then
	// margin, so that this deadline is never what fails first.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))

	var req Request
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		s.reply(conn, Response{Err: "could not read the request: " + err.Error()})
		return
	}

	resp := s.handler(ctx, req)
	s.reply(conn, resp)
}

func (s *Server) reply(conn net.Conn, resp Response) {
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		s.log.Debug("could not answer a control request", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// ErrNoDaemon means nothing is listening. Its own error because it is by far
// the most likely thing to go wrong, and the fix is not obvious from a bare
// "connection refused".
var ErrNoDaemon = errors.New("meshycord is not running")

// Do sends one request and waits for the answer.
func Do(path string, req Request, timeout time.Duration) (Response, error) {
	if path == "" {
		path = DefaultSocket
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return Response{}, fmt.Errorf("%w (%s): %v", ErrNoDaemon, path, err)
		}
		return Response{}, fmt.Errorf("%w (%s): %v", ErrNoDaemon, path, err)
	}
	defer conn.Close()

	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("could not send the request: %w", err)
	}

	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("could not read the answer: %w", err)
	}
	return resp, nil
}
