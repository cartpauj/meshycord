package bridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"meshycord/internal/ctl"
	"meshycord/internal/meshcore"
)

// ControlHandler answers requests from meshycord-cli.
//
// Everything here is a thin translation: the CLI names an operation, this
// picks the bridge method, and the error text is passed through unchanged
// because it was written to be read by a person at a terminal.
func (b *Bridge) ControlHandler() ctl.Handler {
	return func(ctx context.Context, req ctl.Request) ctl.Response {
		switch req.Op {
		case "cli":
			return b.ctlRunCLI(ctx, req)
		case "login":
			out, err := b.SetCLIPassword(ctx, req.Target, req.Arg)
			return respond(out, err)
		case "list":
			return b.ctlList()
		case "status":
			return respond(b.cmdStatus(), nil)
		case "clock":
			return b.ctlClock(ctx, false)
		case "clock-sync":
			return b.ctlClock(ctx, true)
		default:
			return ctl.Response{Err: fmt.Sprintf("unknown operation %q", req.Op)}
		}
	}
}

func respond(out string, err error) ctl.Response {
	if err != nil {
		return ctl.Response{Err: err.Error()}
	}
	return ctl.Response{OK: true, Out: out}
}

func (b *Bridge) ctlRunCLI(ctx context.Context, req ctl.Request) ctl.Response {
	out, err := b.RunCLI(ctx, req.Target, req.Arg)
	if errors.Is(err, ErrCLINoReply) {
		// Distinguished from a failure on purpose. The command may well have
		// run: `reboot` and `poweroff` cannot answer, because the node is gone
		// before the reply would leave.
		return ctl.Response{
			NoReply: true,
			Err: fmt.Sprintf("no reply within %s. The command may still have run — "+
				"`reboot` and `poweroff` never answer, and a busy mesh can simply be slow. "+
				"Check with `clock` or `advert` before sending it again.", CLIReplyTimeout),
		}
	}
	return respond(out, err)
}

func (b *Bridge) ctlList() ctl.Response {
	targets, err := b.CLIContacts()
	if err != nil {
		return ctl.Response{Err: err.Error()}
	}
	if len(targets) == 0 {
		return ctl.Response{OK: true, Out: "No repeaters or room servers in the node's contact list.\n" +
			"Add one in Discord with `contact add <64-hex-key> repeater <name>`."}
	}
	sort.Slice(targets, func(i, j int) bool {
		return strings.ToLower(targets[i].Label) < strings.ToLower(targets[j].Label)
	})

	var s strings.Builder
	for _, t := range targets {
		admin := ""
		if b.cliAdmin(t.Key) {
			admin = "  (admin session active)"
		} else if b.db.RoomPassword(t.Key) != "" {
			admin = "  (password stored)"
		}
		fmt.Fprintf(&s, "%-14s  %-10s  %s%s\n", t.Key, meshcore.AdvTypeName(t.Type), t.Label, admin)
	}
	return ctl.Response{OK: true, Out: strings.TrimRight(s.String(), "\n")}
}

// ctlClock reports the attached node's clock, and optionally corrects it.
//
// Both times are printed in UTC and in this machine's local zone. MeshCore
// keeps every clock in UTC and has no timezone concept at all, so a node in
// Colorado reporting 21:04 at three in the afternoon is correct — and being
// shown only the UTC figure is how somebody talks themselves into "fixing" a
// clock that was never wrong.
func (b *Bridge) ctlClock(ctx context.Context, sync bool) ctl.Response {
	var (
		c   NodeClock
		err error
	)
	if sync {
		c, err = b.SyncNodeClock(ctx)
	} else {
		c, err = b.NodeClockStatus(ctx)
	}
	if errors.Is(err, meshcore.ErrClockBackwards) {
		return ctl.Response{Err: fmt.Sprintf(
			"the node's clock is %s AHEAD of this machine, and MeshCore clocks never move backwards.\n"+
				"  node : %s\n"+
				"  here : %s\n"+
				"Nothing over the air can fix this. Connect the node over USB and run `clkreboot`, "+
				"then let meshycord set the clock when it reconnects.",
			c.Drift.Round(time.Second), formatClock(c.Node), formatClock(c.Local))}
	}
	if err != nil {
		return ctl.Response{Err: err.Error()}
	}

	var s strings.Builder
	fmt.Fprintf(&s, "node : %s\n", formatClock(c.Node))
	fmt.Fprintf(&s, "here : %s\n", formatClock(c.Local))

	drift := c.Drift.Round(time.Second)
	switch {
	case sync && drift.Abs() < 2*time.Second:
		s.WriteString("\nThe node was already in step; nothing changed.")
	case sync:
		fmt.Fprintf(&s, "\nThe node was %s behind. Its clock is now set from this machine.",
			(-drift).Round(time.Second))
	case drift.Abs() < 2*time.Second:
		s.WriteString("\nIn step.")
	default:
		fmt.Fprintf(&s, "\nOff by %s. Correct it with `meshycord-cli -clock-sync`.", drift.Abs())
	}

	// The reason this command exists at all, said plainly.
	s.WriteString("\n\nThis is the clock a repeater inherits from `clock sync`, so a wrong one here " +
		"spreads. To set a repeater from THIS machine regardless, send `time <epoch>` instead.")
	return ctl.Response{OK: true, Out: s.String()}
}

// formatClock prints an instant in UTC and in the local zone, because MeshCore
// speaks only UTC and the person reading lives somewhere else.
func formatClock(t time.Time) string {
	local := t.Local()
	if local.Format("-0700") == "+0000" {
		return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
	}
	return fmt.Sprintf("%s UTC  (%s)",
		t.UTC().Format("2006-01-02 15:04:05"),
		local.Format("15:04:05 MST"))
}
