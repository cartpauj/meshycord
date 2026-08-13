// Command meshycord-cli runs MeshCore CLI commands on a repeater or room
// server, through the meshycord daemon's radio link.
//
// It is a client, not a second bridge. A MeshCore node serves one program at a
// time and meshycord holds that slot, so this asks the running daemon to send
// the command and prints what comes back. If meshycord is not running, this
// cannot work — there is no radio to borrow.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"meshycord/internal/ctl"
)

// Version is set at build time with -ldflags "-X main.Version=v1.2.3".
var Version = "dev"

func main() {
	var (
		command   = flag.String("c", "", "the CLI command to run on the target node")
		socket    = flag.String("socket", ctl.DefaultSocket, "path to the meshycord control socket")
		login     = flag.String("login", "", "store the ADMIN password for a target: -login <target> <password>")
		list      = flag.Bool("list", false, "list repeaters and room servers that commands can be sent to")
		status    = flag.Bool("status", false, "print the bridge's status")
		clock     = flag.Bool("clock", false, "compare the attached node's clock with this machine's")
		clockSync = flag.Bool("clock-sync", false, "set the attached node's clock from this machine")
		showVer   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Println("meshycord-cli", Version)
		return
	}

	req, timeout, err := buildRequest(*command, *login, *list, *status, *clock, *clockSync, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "meshycord-cli:", err)
		fmt.Fprintln(os.Stderr, "\nRun `meshycord-cli -h` for the full usage.")
		os.Exit(2)
	}

	resp, err := ctl.Do(*socket, req, timeout)
	if err != nil {
		if errors.Is(err, ctl.ErrNoDaemon) {
			fmt.Fprintln(os.Stderr, "meshycord-cli: meshycord does not seem to be running.")
			fmt.Fprintln(os.Stderr, "  Check it with:  systemctl status meshycord")
			fmt.Fprintln(os.Stderr, "  The radio can only be held by one program at a time, so this")
			fmt.Fprintln(os.Stderr, "  command works by asking the running bridge to send it.")
			fmt.Fprintf(os.Stderr, "  (socket: %s)\n", *socket)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "meshycord-cli:", err)
		os.Exit(1)
	}

	if resp.Err != "" {
		fmt.Fprintln(os.Stderr, "meshycord-cli: "+resp.Err)
		if resp.NoReply {
			// A distinct exit code, so a script can tell "it did not answer"
			// from "it did not work". They are genuinely different outcomes.
			os.Exit(3)
		}
		os.Exit(1)
	}
	if out := strings.TrimRight(plainText(resp.Out), "\n"); out != "" {
		fmt.Println(out)
	}
}

// plainText strips Discord formatting from answers that are shared with the
// Discord admin commands.
//
// `status` is written once and served to both, and a terminal showing literal
// ** and ``` fences looks broken. Stripping here rather than at the source
// keeps one copy of the text: Discord still gets its bold headings.
func plainText(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue // a fence, not content
		}
		line = strings.ReplaceAll(line, "**", "")
		line = strings.ReplaceAll(line, "`", "")
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// buildRequest turns the flags into one request, and rejects combinations that
// would silently do the wrong thing.
func buildRequest(command, login string, list, status, clock, clockSync bool, args []string) (ctl.Request, time.Duration, error) {
	// The whole round trip crosses the mesh twice and may include a login
	// first, so allow well beyond the daemon's own reply timeout.
	const meshTimeout = 2 * time.Minute

	chosen := 0
	for _, on := range []bool{command != "", login != "", list, status, clock, clockSync} {
		if on {
			chosen++
		}
	}
	if chosen == 0 {
		return ctl.Request{}, 0, errors.New("nothing to do: pass -c \"<command>\", or one of -list, -status, -clock")
	}
	if chosen > 1 {
		return ctl.Request{}, 0, errors.New("pick one thing at a time")
	}

	switch {
	case list:
		return ctl.Request{Op: "list"}, 30 * time.Second, nil
	case status:
		return ctl.Request{Op: "status"}, 30 * time.Second, nil
	case clock:
		return ctl.Request{Op: "clock"}, 30 * time.Second, nil
	case clockSync:
		return ctl.Request{Op: "clock-sync"}, 30 * time.Second, nil

	case login != "":
		// -login names the target; the password is the remaining argument.
		// Taking it positionally rather than as a flag value keeps it off the
		// flag parser's radar, but it is still visible in `ps` — which is why
		// the help says to prefer a shell that does not record history.
		if len(args) != 1 {
			return ctl.Request{}, 0, fmt.Errorf("usage: meshycord-cli -login %s <admin-password>", login)
		}
		return ctl.Request{Op: "login", Target: login, Arg: args[0]}, meshTimeout, nil

	default:
		if len(args) == 0 {
			return ctl.Request{}, 0, errors.New("no target: say which node to run it on, " +
				"e.g. meshycord-cli -c \"clock\" ridge-repeater")
		}
		if len(args) > 1 {
			return ctl.Request{}, 0, fmt.Errorf("expected one target, got %d (%s) — "+
				"quote a name with spaces", len(args), strings.Join(args, ", "))
		}
		return ctl.Request{Op: "cli", Target: args[0], Arg: command}, meshTimeout, nil
	}
}

func usage() {
	// WriteString, not Fprint: the examples contain `$(date +%s)`, and vet
	// reasonably reads a bare %s in a print call as a mistake.
	os.Stderr.WriteString(`meshycord-cli — run MeshCore CLI commands on a repeater, over the air.

USAGE
  meshycord-cli -c "<command>" <target>

  <target> is a repeater or room server, named or by 12-hex key prefix.
  It must be a contact on your node, and you must have stored its ADMIN
  password. A guest password is accepted by the repeater and then ignores
  every command silently, so it is refused here instead.

FIRST TIME
  meshycord-cli -list                              what can I talk to?
  meshycord-cli -login ridge-repeater <password>   store and verify the admin password

EXAMPLES
  meshycord-cli -c "clock" ridge-repeater          what time does it think it is?
  meshycord-cli -c "clock sync" ridge-repeater     set it from THIS NODE's clock
  meshycord-cli -c "time $(date +%s)" ridge-repeater
                                                   set it from THIS MACHINE's clock
  meshycord-cli -c "advert" ridge-repeater         send a flood advert
  meshycord-cli -c "neighbors" ridge-repeater      who can it hear?
  meshycord-cli -c "reboot" ridge-repeater         (never answers; that is normal)

CLOCKS
  MeshCore keeps every clock in UTC and has no timezone concept, so a repeater
  reporting 21:04 at three in the afternoon in Denver is correct.

  ` + "`clock sync`" + ` sets the repeater from your USB node's clock, NOT this
  machine's — the node stamps the message with its own RTC and there is no way
  to override it. Check that clock first:

  meshycord-cli -clock          compare the node's clock with this machine's
  meshycord-cli -clock-sync     set the node's clock from this machine

  Or bypass both and send this machine's time as a literal:
  meshycord-cli -c "time $(date +%s)" ridge-repeater

EXIT CODES
  0  it worked
  1  it failed
  2  the command line was wrong
  3  sent, but nothing came back (may still have run)

FLAGS
`)
	flag.PrintDefaults()
}
