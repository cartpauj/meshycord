package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"meshycord/internal/meshcore"
)

// Two commands to the same node would produce two replies that are
// indistinguishable on the wire, and the wrong one could easily be handed
// back. Only one may be in flight.
func TestOnlyOneCLICommandPerTargetAtATime(t *testing.T) {
	b, _ := newTestBridge(t)

	first := b.awaitCLIReply("aabbccddeeff")
	if first == nil {
		t.Fatal("the first command could not register a waiter")
	}
	if second := b.awaitCLIReply("aabbccddeeff"); second != nil {
		t.Error("a second command registered while the first was still waiting")
	}
	// A different node is unaffected.
	if other := b.awaitCLIReply("112233445566"); other == nil {
		t.Error("a command to a different node was blocked")
	}

	b.releaseCLIReply("aabbccddeeff")
	if again := b.awaitCLIReply("aabbccddeeff"); again == nil {
		t.Error("the slot was not freed after the first command finished")
	}
}

// The node reports its sender prefix in whatever case it likes; the caller
// addressed it in whatever case they typed. A mismatch would strand the reply.
func TestCLIRepliesMatchRegardlessOfKeyCase(t *testing.T) {
	b, _ := newTestBridge(t)

	pend := b.awaitCLIReply("AABBCCDDEEFF")
	if pend == nil {
		t.Fatal("could not register a waiter")
	}
	if !b.deliverCLIReply("aabbccddeeff", "OK - clock set") {
		t.Fatal("a reply from the same node was not matched")
	}
	select {
	case got := <-pend.reply:
		if got != "OK - clock set" {
			t.Errorf("reply = %q", got)
		}
	default:
		t.Error("the reply never reached the waiter")
	}
}

// An unmatched CLI reply must be reported as unhandled, so the caller relays
// it somewhere rather than dropping a repeater's console output into a chat
// channel.
func TestUnmatchedCLIReplyIsReportedUnhandled(t *testing.T) {
	b, _ := newTestBridge(t)
	if b.deliverCLIReply("aabbccddeeff", "some late answer") {
		t.Error("a reply nobody was waiting for was reported as delivered")
	}
}

// Long output arrives as several messages with nothing marking the last one.
func TestCollectCLIReplyJoinsContinuationLines(t *testing.T) {
	b, _ := newTestBridge(t)

	pend := &cliPending{reply: make(chan string, 4)}
	pend.reply <- "second line"
	pend.reply <- "third line"

	got := b.collectCLIReply(context.Background(), pend, "first line")
	want := "first line\nsecond line\nthird line"
	if got != want {
		t.Errorf("collected = %q, want %q", got, want)
	}
}

func TestCollectCLIReplyStopsWhenTheContextEnds(t *testing.T) {
	b, _ := newTestBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := b.collectCLIReply(ctx, &cliPending{reply: make(chan string, 1)}, "only line")
	if got != "only line" {
		t.Errorf("collected = %q, want %q", got, "only line")
	}
}

// A stale admin session is invisible: the far node just discards commands. So
// trust must expire, and must not survive the link dropping.
func TestCLIAdminSessionsExpireAndAreForgottenOnDisconnect(t *testing.T) {
	b, _ := newTestBridge(t)

	b.noteCLIAdmin("aabbccddeeff")
	if !b.cliAdmin("aabbccddeeff") {
		t.Fatal("a session recorded moments ago is not trusted")
	}

	b.forgetCLIAdmins()
	if b.cliAdmin("aabbccddeeff") {
		t.Error("an admin session survived the link going down")
	}

	// An aged session is not trusted either.
	b.noteCLIAdmin("aabbccddeeff")
	b.cliMu.Lock()
	b.cliAdminAt["aabbccddeeff"] = time.Now().Add(-24 * time.Hour)
	b.cliMu.Unlock()
	if b.cliAdmin("aabbccddeeff") {
		t.Error("a day-old session is still being trusted")
	}
}

// Refusing to run a command with no link, no target and no text is what keeps
// the failure messages useful rather than a nil dereference.
func TestRunCLIRefusesEmptyAndOversizedCommands(t *testing.T) {
	b, _ := newTestBridge(t)
	ctx := context.Background()

	if _, err := b.RunCLI(ctx, "somewhere", ""); err == nil {
		t.Error("an empty command was accepted")
	}
	long := strings.Repeat("x", meshcore.MaxMsgLen+1)
	_, err := b.RunCLI(ctx, "somewhere", long)
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("oversized command err = %v, want a message about the mesh ceiling", err)
	}
}

func TestRunCLISaysTheLinkIsDownRatherThanCrashing(t *testing.T) {
	b, _ := newTestBridge(t)
	_, err := b.RunCLI(context.Background(), "ridge-repeater", "clock")
	if err == nil {
		t.Fatal("running a command with no radio link succeeded")
	}
	if !strings.Contains(err.Error(), "link is down") {
		t.Errorf("err = %v, want it to say the link is down", err)
	}
}

// A CLI login verdict belongs to the command that started it, not to the room
// state machine — a repeater has no room channel to announce anything in.
func TestCLILoginVerdictIsClaimedByTheWaitingCommand(t *testing.T) {
	b, _ := newTestBridge(t)

	verdict := make(chan meshcore.LoginResult, 1)
	b.cliMu.Lock()
	b.cliLogins["aabbccddeeff"] = verdict
	b.cliMu.Unlock()

	r := meshcore.LoginResult{Prefix: "aabbccddeeff", OK: true, Perms: 1}
	if !b.deliverCLILogin(r) {
		t.Fatal("the waiting command did not receive its login verdict")
	}
	if got := <-verdict; !got.OK || !got.IsAdmin() {
		t.Errorf("verdict = %+v, want an admin success", got)
	}

	// A verdict for a room nobody asked about on the CLI side falls through.
	if b.deliverCLILogin(meshcore.LoginResult{Prefix: "999999999999", OK: true}) {
		t.Error("an unrelated room login was swallowed by the CLI path")
	}
}
