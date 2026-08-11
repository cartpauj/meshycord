package meshcore

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Transport carries whole companion-protocol frames in both directions.
//
// Three implementations exist — serial, TCP and BLE — and the client above
// them cannot tell which it is talking to. That is the point: the protocol
// logic gets tested against a fake transport with no hardware in the room.
type Transport interface {
	// ReadFrame blocks until one complete frame arrives, or ctx is done.
	ReadFrame(ctx context.Context) ([]byte, error)
	// WriteFrame sends one complete frame.
	WriteFrame(ctx context.Context, f []byte) error
	Close() error
	// Describe names the link for logs and the web UI.
	Describe() string
}

// Dialer opens a Transport. A dial that fails is normal — the radio may be
// unplugged or out of range — so the supervisor above simply retries.
type Dialer interface {
	Dial(ctx context.Context) (Transport, error)
	Describe() string
}

// FailureReporter is an optional Dialer extension for transports that can
// repair themselves.
//
// It reports the case a dialer cannot see on its own: the link opened fine and
// then turned out to be unusable. Over BLE that is the signature of a bond
// that has gone stale — the connection succeeds, the link comes up encrypted
// but unauthenticated, and the companion silently rejects every write, so the
// handshake times out. Retrying that forever achieves nothing; the bond has to
// be dropped. The ESP32 firmware hit exactly this when the node's PIN changed.
type FailureReporter interface {
	// SessionFailed is called when a transport this dialer opened could not
	// complete the protocol handshake.
	SessionFailed(err error)
	// SessionEstablished resets whatever SessionFailed was counting.
	SessionEstablished()
}

// ---------------------------------------------------------------------------
// Stream framing (serial and TCP)
// ---------------------------------------------------------------------------
//
// BLE gives frame boundaries for free: one notification is one frame. A byte
// stream does not, so MeshCore length-prefixes every frame:
//
//	to the node:    '>' [length u16 LE] [payload]
//	from the node:  '<' [length u16 LE] [payload]
//
// This layout is what the official meshcore_py and meshcore.js clients speak,
// and it is the same for USB serial and for the TCP companion on port 5000.
const (
	frameToDevice   = '>'
	frameFromDevice = '<'
	// maxFrameLen is a sanity ceiling. A contact record is ~150 bytes and the
	// largest legitimate frame is well under 1 KB; anything claiming more is a
	// desynchronised stream, not a real frame.
	maxFrameLen = 8192
)

// streamTransport frames the companion protocol over any byte stream.
type streamTransport struct {
	rwc  io.ReadWriteCloser
	br   *bufio.Reader
	what string

	// Writes are serialised. Two goroutines interleaving halves of a frame
	// header would desynchronise the node with no way to recover except a
	// reconnect.
	wmu sync.Mutex

	closeOnce sync.Once
}

// NewStreamTransport wraps an already-open byte stream (a serial port, a TCP
// connection) in MeshCore's length-prefixed framing.
func NewStreamTransport(rwc io.ReadWriteCloser, describe string) Transport {
	return &streamTransport{
		rwc:  rwc,
		br:   bufio.NewReaderSize(rwc, 4096),
		what: describe,
	}
}

func (s *streamTransport) Describe() string { return s.what }

func (s *streamTransport) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.rwc.Close() })
	return err
}

func (s *streamTransport) WriteFrame(ctx context.Context, f []byte) error {
	if len(f) == 0 {
		return errors.New("meshcore: refusing to send an empty frame")
	}
	if len(f) > maxFrameLen {
		return fmt.Errorf("meshcore: frame of %d bytes exceeds the %d-byte limit", len(f), maxFrameLen)
	}
	hdr := make([]byte, 3, 3+len(f))
	hdr[0] = frameToDevice
	binary.LittleEndian.PutUint16(hdr[1:3], uint16(len(f)))
	hdr = append(hdr, f...)

	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.rwc.Write(hdr)
	return err
}

// ReadFrame reads one frame, resynchronising if the stream is out of step.
//
// Resync matters on a serial port: the node may have been mid-frame when the
// bridge attached, or a USB hiccup may have eaten a byte. Rather than dying,
// scan forward for the next start marker and carry on.
func (s *streamTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := s.br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != frameFromDevice {
			// Not a frame start. Some firmware prints plain-text boot banners
			// on the same port; skipping is the correct response.
			continue
		}
		var lenBuf [2]byte
		if _, err := io.ReadFull(s.br, lenBuf[:]); err != nil {
			return nil, err
		}
		n := int(binary.LittleEndian.Uint16(lenBuf[:]))
		if n == 0 {
			continue
		}
		if n > maxFrameLen {
			// A bogus length means we mistook payload bytes for a header.
			// Drop it and keep scanning rather than blocking on a read that
			// will never be satisfied.
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(s.br, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
}

// ---------------------------------------------------------------------------
// TCP
// ---------------------------------------------------------------------------

// TCPDialer reaches MeshCore's WiFi companion firmware, which serves the same
// protocol on port 5000. This is the transport that lets the bridge run
// somewhere else entirely — a NAS, a spare x86 box — instead of next to the
// radio.
type TCPDialer struct {
	Addr string // host:port; port 5000 is assumed when absent
}

func (d TCPDialer) Describe() string { return "tcp " + d.address() }

func (d TCPDialer) address() string {
	if _, _, err := net.SplitHostPort(d.Addr); err != nil {
		return net.JoinHostPort(d.Addr, "5000")
	}
	return d.Addr
}

func (d TCPDialer) Dial(ctx context.Context) (Transport, error) {
	var dl net.Dialer
	dl.Timeout = 10 * time.Second
	conn, err := dl.DialContext(ctx, "tcp", d.address())
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		// The companion link is idle for long stretches. Without keep-alives a
		// silently dropped connection looks identical to a quiet mesh, and the
		// bridge waits forever for a message that will never arrive.
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	return NewStreamTransport(conn, "tcp "+d.address()), nil
}
