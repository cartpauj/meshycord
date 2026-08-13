package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"meshycord/internal/meshcore"
)

// Remote CLI: running commands on a repeater or room server over the air.
//
// MeshCore has no command for this. What it has is a text message with one
// byte changed — TxtTypeCLIData instead of TxtTypePlain — which a repeater
// interprets as a command line rather than chat, runs through its CommonCLI,
// and answers with another TxtTypeCLIData message carrying the output.
//
// Three consequences shape everything in this file:
//
//  1. The far node demands an ADMIN login first. A guest login succeeds and
//     then every command is discarded without a word.
//  2. Nothing correlates a reply with its command. There is no request id and
//     no sequence number; the output simply arrives as an inbound message
//     some seconds later. Matching is by sender, which is why only one
//     command per target may be in flight at a time.
//  3. Silence is a legitimate outcome. `reboot` and `poweroff` never answer,
//     because the node is gone before it could, and the firmware also sends
//     nothing at all when it treats a message as a retry.
//
// There is deliberately no support for running a CLI command on the LOCALLY
// attached node: the companion protocol has no such command in any firmware
// version. Its own CLI exists only in USB "CLI Rescue" mode, which replaces
// the binary protocol entirely and cannot coexist with the bridge.

// CLIReplyTimeout bounds the wait for a repeater's answer.
//
// Generous on purpose. The reply is delayed by CLI_REPLY_DELAY_MILLIS at the
// far end, crosses the mesh hop by hop, and then waits for the local node to
// hand it over on the next sync. Several seconds is normal, not slow.
const CLIReplyTimeout = 45 * time.Second

// cliLoginTimeout bounds the wait for an admin login verdict, which also
// travels over the air.
const cliLoginTimeout = 30 * time.Second

// ErrCLINoReply means the command went out and nothing came back. It is not
// proof of failure: some commands never answer by design.
var ErrCLINoReply = errors.New("no reply from the node")

// cliPending is one command awaiting its answer.
type cliPending struct {
	reply chan string
}

// awaitCLIReply registers interest in the next CLI output from a target.
//
// Returns nil if another command is already in flight for that target. Only
// one at a time is allowed, because two commands to the same node would race
// for two indistinguishable replies and could easily be matched up the wrong
// way round.
func (b *Bridge) awaitCLIReply(prefix string) *cliPending {
	prefix = strings.ToLower(prefix)
	b.cliMu.Lock()
	defer b.cliMu.Unlock()
	if _, busy := b.cliWaiters[prefix]; busy {
		return nil
	}
	p := &cliPending{reply: make(chan string, 4)}
	b.cliWaiters[prefix] = p
	return p
}

func (b *Bridge) releaseCLIReply(prefix string) {
	b.cliMu.Lock()
	delete(b.cliWaiters, strings.ToLower(prefix))
	b.cliMu.Unlock()
}

// deliverCLIReply hands an inbound CLI message to whoever is waiting for it.
// Reports whether anyone was.
func (b *Bridge) deliverCLIReply(prefix, text string) bool {
	b.cliMu.Lock()
	p, ok := b.cliWaiters[strings.ToLower(prefix)]
	b.cliMu.Unlock()
	if !ok {
		return false
	}
	select {
	case p.reply <- text:
		return true
	default:
		// The buffer is deep enough for any real multi-part answer. A full one
		// means nobody is reading, so drop rather than block the mesh loop.
		return false
	}
}

// deliverCLILogin hands a login verdict to a waiting CLI command, if any.
func (b *Bridge) deliverCLILogin(r meshcore.LoginResult) bool {
	b.cliMu.Lock()
	ch, ok := b.cliLogins[strings.ToLower(r.Prefix)]
	b.cliMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- r:
		return true
	default:
		return false
	}
}

// CLITarget is a node a command can be sent to.
type CLITarget struct {
	Key   string // 12-hex key prefix
	Label string
	Type  byte
}

// ResolveCLITarget finds the node a command is addressed to.
//
// Accepts a key prefix or a name, and insists on exactly one match: guessing
// which repeater somebody meant is not a risk worth taking when the command
// might be `reboot`.
func (b *Bridge) ResolveCLITarget(target string) (CLITarget, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return CLITarget{}, errors.New("no target given: name the repeater or room server to run this on")
	}
	sess := b.link.Session()
	if sess == nil {
		return CLITarget{}, errors.New("the radio link is down, so the contact list cannot be read")
	}

	if c, ok := sess.LookupContact(target); ok {
		return CLITarget{Key: c.PubKeyHex()[:12], Label: c.Name, Type: c.Type}, nil
	}

	matches := sess.FindContacts(target)
	switch len(matches) {
	case 0:
		return CLITarget{}, fmt.Errorf("no contact matches %q — check `meshycord-cli -list`", target)
	case 1:
		c := matches[0]
		return CLITarget{Key: c.PubKeyHex()[:12], Label: c.Name, Type: c.Type}, nil
	}

	var names []string
	for _, c := range matches {
		names = append(names, fmt.Sprintf("%s (%s)", c.Name, c.PubKeyHex()[:12]))
	}
	return CLITarget{}, fmt.Errorf("%q matches %d contacts, so it is ambiguous: %s",
		target, len(matches), strings.Join(names, ", "))
}

// RunCLI runs one command on a remote node and returns its output.
//
// The whole sequence — resolve, log in if needed, transmit, wait — happens
// here because every step of it can fail in a way the caller has to be told
// about in words rather than as an error code.
func (b *Bridge) RunCLI(ctx context.Context, target, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("no command given")
	}
	if len(command) > meshcore.MaxMsgLen {
		return "", fmt.Errorf("the command is %d characters; the mesh ceiling is %d",
			len(command), meshcore.MaxMsgLen)
	}

	t, err := b.ResolveCLITarget(target)
	if err != nil {
		return "", err
	}
	// A companion is somebody's phone or handheld. It has no CLI, and sending
	// it one would put mystery text on a stranger's screen.
	if t.Type == meshcore.AdvTypeChat {
		return "", fmt.Errorf("%s is a companion (a person's device), not a repeater or room server — "+
			"it has no CLI to run commands on", t.Label)
	}

	sess := b.link.Session()
	if sess == nil {
		return "", errors.New("the radio link is down")
	}

	if err := b.ensureCLIAdmin(ctx, t); err != nil {
		return "", err
	}

	// Register BEFORE transmitting. The reply is a plain inbound message and
	// a fast one could arrive while we were still setting up to listen.
	pend := b.awaitCLIReply(t.Key)
	if pend == nil {
		return "", fmt.Errorf("another command is already running on %s; wait for it to finish", t.Label)
	}
	defer b.releaseCLIReply(t.Key)

	b.txMu.Lock()
	_, err = sess.SendCLI(ctx, t.Key, command)
	b.txMu.Unlock()
	if err != nil {
		if errors.Is(err, meshcore.ErrRejected) {
			return "", fmt.Errorf("the radio refused to send the command to %s — it is most likely "+
				"not in the node's contact list", t.Label)
		}
		return "", fmt.Errorf("could not send the command: %w", err)
	}
	b.log.Info("sent a remote CLI command", "target", t.Label, "key", t.Key, "command", command)
	b.db.LogEvent("info", "cli", fmt.Sprintf("ran %q on %s", command, t.Label))

	select {
	case out := <-pend.reply:
		return b.collectCLIReply(ctx, pend, out), nil
	case <-time.After(CLIReplyTimeout):
		return "", ErrCLINoReply
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// collectCLIReply gathers any continuation lines that follow the first.
//
// A long answer — `neighbors` on a busy repeater — arrives as several
// messages, and nothing marks the last one. So take the first, then keep
// anything that turns up in the short window after it.
func (b *Bridge) collectCLIReply(ctx context.Context, pend *cliPending, first string) string {
	parts := []string{first}
	for {
		select {
		case more := <-pend.reply:
			parts = append(parts, more)
		case <-time.After(3 * time.Second):
			return strings.Join(parts, "\n")
		case <-ctx.Done():
			return strings.Join(parts, "\n")
		}
	}
}

// ensureCLIAdmin makes sure there is an admin session on the target.
func (b *Bridge) ensureCLIAdmin(ctx context.Context, t CLITarget) error {
	if b.cliAdmin(t.Key) {
		return nil
	}

	password := b.db.RoomPassword(t.Key)
	if password == "" {
		return fmt.Errorf("no admin password stored for %s.\n"+
			"Store one first:  meshycord-cli -login %s <admin-password>\n"+
			"It must be the repeater's ADMIN password — a guest login is accepted and then "+
			"ignores every command silently.", t.Label, t.Key)
	}

	verdict := make(chan meshcore.LoginResult, 1)
	b.cliMu.Lock()
	if _, busy := b.cliLogins[t.Key]; busy {
		b.cliMu.Unlock()
		return fmt.Errorf("a login to %s is already in progress", t.Label)
	}
	b.cliLogins[t.Key] = verdict
	b.cliMu.Unlock()
	defer func() {
		b.cliMu.Lock()
		delete(b.cliLogins, t.Key)
		b.cliMu.Unlock()
	}()

	sess := b.link.Session()
	if sess == nil {
		return errors.New("the radio link is down")
	}
	if err := b.sendRoomLogin(ctx, sess, t.Key, password); err != nil {
		if errors.Is(err, meshcore.ErrNotContact) {
			return fmt.Errorf("%s is not in the node's contact list, and a login needs its full "+
				"64-hex key. Add it in Discord with `contact add <64-hex-key> repeater <name>`.", t.Label)
		}
		return fmt.Errorf("the radio would not send the login: %w", err)
	}

	select {
	case r := <-verdict:
		if !r.OK {
			return fmt.Errorf("%s rejected the stored password", t.Label)
		}
		if !r.IsAdmin() {
			return fmt.Errorf("logged in to %s, but as %s — not admin.\n"+
				"Remote CLI commands need the ADMIN password; this session can connect but every "+
				"command would be discarded without an error.", t.Label, meshcore.RoleName(r.Role()))
		}
		b.noteCLIAdmin(t.Key)
		return nil
	case <-time.After(cliLoginTimeout):
		return fmt.Errorf("%s did not answer the login within %s — it may be out of range",
			t.Label, cliLoginTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cliAdmin reports whether there is an admin session recent enough to trust.
//
// The far node decides when a session really ends and never says so, exactly
// as with room servers, so trust expires on a timer. Being wrong here is
// invisible — the command is simply discarded — which makes a stale session
// far more expensive than the round trip of logging in again.
func (b *Bridge) cliAdmin(prefix string) bool {
	ttl := b.cfg.RoomSessionTTL()
	if ttl == 0 {
		return false
	}
	b.cliMu.Lock()
	defer b.cliMu.Unlock()
	at, ok := b.cliAdminAt[strings.ToLower(prefix)]
	return ok && time.Since(at) < ttl
}

func (b *Bridge) noteCLIAdmin(prefix string) {
	b.cliMu.Lock()
	b.cliAdminAt[strings.ToLower(prefix)] = time.Now()
	b.cliMu.Unlock()
}

// forgetCLIAdmins drops every remembered session. Called when the link drops:
// the far nodes keep their own state, but ours is now a guess.
func (b *Bridge) forgetCLIAdmins() {
	b.cliMu.Lock()
	b.cliAdminAt = map[string]time.Time{}
	b.cliMu.Unlock()
}

// SetCLIPassword stores an admin password for a target and verifies it.
func (b *Bridge) SetCLIPassword(ctx context.Context, target, password string) (string, error) {
	t, err := b.ResolveCLITarget(target)
	if err != nil {
		return "", err
	}
	if password == "" {
		if err := b.db.SetRoomPassword(t.Key, ""); err != nil {
			return "", err
		}
		b.cliMu.Lock()
		delete(b.cliAdminAt, t.Key)
		b.cliMu.Unlock()
		return fmt.Sprintf("Forgot the stored password for %s.", t.Label), nil
	}
	if err := b.db.SetRoomPassword(t.Key, password); err != nil {
		return "", err
	}
	b.cliMu.Lock()
	delete(b.cliAdminAt, t.Key)
	b.cliMu.Unlock()

	// Prove it works now rather than at 2am when something needs rebooting.
	if err := b.ensureCLIAdmin(ctx, t); err != nil {
		return "", fmt.Errorf("password stored, but the login did not succeed:\n%w", err)
	}
	return fmt.Sprintf("Admin login to %s (%s) confirmed. The password is stored.", t.Label, t.Key), nil
}

// CLIContacts lists the contacts a command could be addressed to.
func (b *Bridge) CLIContacts() ([]CLITarget, error) {
	sess := b.link.Session()
	if sess == nil {
		return nil, errors.New("the radio link is down")
	}
	var out []CLITarget
	for _, c := range sess.Contacts() {
		if c.Type != meshcore.AdvTypeRepeater && c.Type != meshcore.AdvTypeRoom {
			continue
		}
		out = append(out, CLITarget{Key: c.PubKeyHex()[:12], Label: c.Name, Type: c.Type})
	}
	return out, nil
}

// NodeClock reports the locally attached node's clock against this machine's.
//
// Worth its own command because the node's clock — not the Pi's — is what
// gets stamped on a remote CLI message, and therefore what `clock sync`
// copies onto a repeater.
type NodeClock struct {
	Node  time.Time
	Local time.Time
	Drift time.Duration // node minus local; positive means the node is ahead
}

// NodeClockStatus reads the node's clock.
func (b *Bridge) NodeClockStatus(ctx context.Context) (NodeClock, error) {
	sess := b.link.Session()
	if sess == nil {
		return NodeClock{}, errors.New("the radio link is down")
	}
	now := time.Now()
	nodeTime, err := sess.DeviceTime(ctx)
	if err != nil {
		return NodeClock{}, err
	}
	return NodeClock{Node: nodeTime, Local: now, Drift: nodeTime.Sub(now)}, nil
}

// SyncNodeClock sets the attached node's clock from this machine.
func (b *Bridge) SyncNodeClock(ctx context.Context) (NodeClock, error) {
	before, err := b.NodeClockStatus(ctx)
	if err != nil {
		return NodeClock{}, err
	}
	sess := b.link.Session()
	if sess == nil {
		return NodeClock{}, errors.New("the radio link is down")
	}
	if err := sess.SetDeviceTime(ctx, time.Now()); err != nil {
		return before, err
	}
	return before, nil
}
