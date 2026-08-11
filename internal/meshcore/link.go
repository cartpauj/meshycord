package meshcore

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Link keeps exactly one Session alive against a Dialer, redialing with
// backoff whenever the radio goes away.
//
// This is aprgo's Supervisor.reconcile() shape: a goroutine whose only job is
// to make reality match the desired state — one live link — and everything
// else reads the current session through a lock and copes with nil. Nothing
// above ever dials, retries, or reasons about backoff.
type Link struct {
	// dialer is guarded because the operator can change transport at runtime —
	// serial to BLE, say — and the supervisor has to pick that up without a
	// restart.
	dialerMu sync.RWMutex
	dialer   Dialer

	log *slog.Logger

	// OnConnect runs once per successful connection, before the session is
	// published. It is where the bridge re-reads channels, re-enumerates
	// contacts and re-establishes room sessions — all of which have to happen
	// on EVERY reconnect, not just the first, because none of that state
	// survives the link going away.
	OnConnect func(context.Context, *Session) error

	// OnDisconnect runs after a session ends, for status reporting.
	OnDisconnect func()

	mu      sync.RWMutex
	sess    *Session
	lastErr error
	upSince time.Time
	attempt int
}

// NewLink creates a supervisor for a dialer. Nothing happens until Run.
func NewLink(d Dialer, log *slog.Logger) *Link {
	if log == nil {
		log = slog.Default()
	}
	return &Link{dialer: d, log: log}
}

// currentDialer reads the dialer under lock.
func (l *Link) currentDialer() Dialer {
	l.dialerMu.RLock()
	defer l.dialerMu.RUnlock()
	return l.dialer
}

// SetDialer swaps the transport. The caller should then close the live session
// so the supervisor redials with it.
func (l *Link) SetDialer(d Dialer) {
	l.dialerMu.Lock()
	l.dialer = d
	l.dialerMu.Unlock()
	l.log.Info("radio transport changed", "transport", d.Describe())
}

// Backoff bounds, deliberately gentle at the bottom and capped at a minute.
// A radio that has just been unplugged should not be hammered, and a radio
// that has just come back should be picked up quickly.
const (
	dialBackoffMin = 2 * time.Second
	dialBackoffMax = 60 * time.Second
)

// Run holds a link open until ctx is cancelled. It never returns an error:
// a failure to dial is an expected condition, not a fault.
func (l *Link) Run(ctx context.Context) {
	backoff := dialBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}

		l.mu.Lock()
		l.attempt++
		l.mu.Unlock()

		sess, err := l.connect(ctx)
		if err != nil {
			l.setErr(err)
			l.log.Info("cannot reach the node; will retry",
				"transport", l.currentDialer().Describe(), "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > dialBackoffMax {
				backoff = dialBackoffMax
			}
			continue
		}

		backoff = dialBackoffMin
		l.publish(sess)

		select {
		case <-ctx.Done():
			sess.Close()
			l.publish(nil)
			return
		case <-sess.Done():
			l.log.Info("link to the node went down")
			l.publish(nil)
			if l.OnDisconnect != nil {
				l.OnDisconnect()
			}
			// A brief pause before redialling. A node that just rebooted is
			// not ready to be talked to the instant it stops answering.
			select {
			case <-ctx.Done():
				return
			case <-time.After(dialBackoffMin):
			}
		}
	}
}

func (l *Link) connect(ctx context.Context) (*Session, error) {
	dialer := l.currentDialer()
	tr, err := dialer.Dial(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := NewSession(ctx, tr, l.log)
	if err != nil {
		_ = tr.Close()
		// The link opened and then proved unusable. Some transports can repair
		// themselves from that — see FailureReporter.
		if fr, ok := dialer.(FailureReporter); ok {
			fr.SessionFailed(err)
		}
		return nil, err
	}
	if fr, ok := dialer.(FailureReporter); ok {
		fr.SessionEstablished()
	}
	if l.OnConnect != nil {
		if err := l.OnConnect(ctx, sess); err != nil {
			// A failed post-connect setup means the session is not usable —
			// channel names unknown, contacts unclassifiable. Tear it down and
			// let the backoff try again rather than run half-configured.
			sess.Close()
			return nil, err
		}
	}
	return sess, nil
}

func (l *Link) publish(s *Session) {
	l.mu.Lock()
	l.sess = s
	if s != nil {
		l.upSince = time.Now()
		l.lastErr = nil
	} else {
		l.upSince = time.Time{}
	}
	l.mu.Unlock()
}

func (l *Link) setErr(err error) {
	l.mu.Lock()
	l.lastErr = err
	l.mu.Unlock()
}

// Session returns the live session, or nil when the link is down. Callers must
// handle nil — a radio that is unplugged is a normal state, not an error.
func (l *Link) Session() *Session {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.sess
}

// Status describes the link for the status page and the admin console.
type Status struct {
	Connected bool
	Transport string
	UpSince   time.Time
	LastError string
	Attempts  int
	NodeName  string
	NodeKey   string
	Firmware  string
	Contacts  int
	Channels  int
	// Radio config, as the node reports it at handshake. Worth surfacing:
	// "which frequency am I actually on" is otherwise a phone-app trip.
	FreqKHz    uint32
	BandwidthK uint32
	SpreadFact byte
	CodingRate byte
}

// FrequencyMHz renders the node's operating frequency, or "" if unknown.
func (s Status) FrequencyMHz() string {
	if s.FreqKHz == 0 {
		return ""
	}
	return fmt.Sprintf("%.3f MHz", float64(s.FreqKHz)/1000)
}

// RadioParams renders the LoRa modem settings compactly.
func (s Status) RadioParams() string {
	if s.SpreadFact == 0 {
		return ""
	}
	return fmt.Sprintf("SF%d · BW%.0fk · CR%d", s.SpreadFact, float64(s.BandwidthK)/1000, s.CodingRate)
}

// Status snapshots the link.
func (l *Link) Status() Status {
	l.mu.RLock()
	sess, lastErr, upSince, attempt := l.sess, l.lastErr, l.upSince, l.attempt
	l.mu.RUnlock()

	st := Status{Transport: l.currentDialer().Describe(), UpSince: upSince, Attempts: attempt}
	if lastErr != nil {
		st.LastError = lastErr.Error()
	}
	if sess == nil {
		return st
	}
	st.Connected = true
	st.Transport = sess.Describe()
	self := sess.SelfInfo()
	st.NodeName = self.Name
	st.FreqKHz, st.BandwidthK = self.FreqKHz, self.BandwidthK
	st.SpreadFact, st.CodingRate = self.SpreadFact, self.CodingRate
	if k := self.PubKeyHex(); k != "" {
		st.NodeKey = k
	}
	dev := sess.DeviceInfo()
	if dev.Firmware != "" {
		st.Firmware = dev.Firmware
	} else if dev.FirmwareVer != 0 {
		st.Firmware = fmt.Sprintf("protocol v%d", dev.FirmwareVer)
	}
	st.Contacts = sess.ContactCount()
	st.Channels = len(sess.Channels())
	return st
}
