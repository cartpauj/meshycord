// Package sdnotify speaks systemd's notification protocol.
//
// This is the replacement for the ESP32's watchdog scaffolding. There, a
// hardware watchdog had to be fed by hand from every loop that could run long,
// and missing one rebooted the device — which is exactly what happened. Here
// the supervision lives outside the process: a heartbeat goroutine that has
// nothing else to do pings systemd, and if the process wedges, systemd
// restarts it. Nothing in the application code has to remember anything.
//
// The whole protocol is "write a short string to a unix datagram socket whose
// path is in $NOTIFY_SOCKET". When the variable is unset — running from a
// terminal, or under another supervisor — every call here is a no-op.
package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Notify sends one status line to systemd. A nil error with no socket
// configured is not a failure: it means nobody is listening, which is the
// normal case outside systemd.
func Notify(state string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	// A leading '@' means an abstract socket, which Go writes as a leading NUL.
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// Ready tells systemd the service has finished starting. Required by
// Type=notify units, which otherwise sit in "activating" until they time out.
func Ready() error { return Notify("READY=1") }

// Status sets the one-line description shown by `systemctl status`.
func Status(text string) error { return Notify("STATUS=" + text) }

// Stopping tells systemd a clean shutdown is underway, so it does not treat
// the delay as a hang.
func Stopping() error { return Notify("STOPPING=1") }

// WatchdogInterval is how often systemd expects a heartbeat, derived from
// WatchdogSec in the unit. Zero means the watchdog is off.
//
// systemd's own guidance is to ping at half the configured interval, which is
// what this returns — so the caller can use it directly as a tick period
// without having to know the convention.
func WatchdogInterval() time.Duration {
	usec := os.Getenv("WATCHDOG_USEC")
	if usec == "" {
		return 0
	}
	// WATCHDOG_PID guards against a child process inheriting the environment
	// and answering on the real service's behalf.
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" {
		if n, err := strconv.Atoi(pid); err != nil || n != os.Getpid() {
			return 0
		}
	}
	n, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Microsecond / 2
}

// Watchdog pings systemd. If these stop arriving, systemd restarts the
// service — which is the entire recovery story for a hang.
func Watchdog() error { return Notify("WATCHDOG=1") }

// Errno reports a failure code to systemd, shown in `systemctl status`.
func Errno(n int) error { return Notify(fmt.Sprintf("ERRNO=%d", n)) }
