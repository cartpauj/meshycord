//go:build linux

package meshcore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// SerialDialer opens a MeshCore companion over USB serial.
//
// This is the recommended primary transport when the bridge sits next to the
// radio: no pairing, no PIN, no bonding, no dropped links, and no BlueZ. It is
// also the only transport with no OS-specific component beyond termios.
type SerialDialer struct {
	// Device is a path such as /dev/ttyACM0. Empty means "find one", which is
	// almost always right — a MeshCore node is normally the only CDC-ACM
	// device on a Pi.
	Device string
	// Baud is ignored by CDC-ACM devices (USB has no baud rate) but matters
	// for a real UART. 115200 when zero.
	Baud int
}

func (d SerialDialer) Describe() string {
	if d.Device == "" {
		return "serial (auto-detect)"
	}
	return "serial " + d.Device
}

func (d SerialDialer) Dial(ctx context.Context) (Transport, error) {
	dev := d.Device
	if dev == "" {
		found, err := FindSerialPorts()
		if err != nil || len(found) == 0 {
			return nil, fmt.Errorf("no serial device found: plug the node in, or set the device explicitly")
		}
		dev = found[0]
	}
	baud := d.Baud
	if baud == 0 {
		baud = 115200
	}

	// O_NONBLOCK on open matters twice over. It stops the open itself blocking
	// on modem control lines for a device that never asserts DCD, and it lets
	// Go register the fd with its network poller — which is what makes a
	// blocked Read unblock when Close is called from another goroutine.
	// Without it, a dead link leaves a reader goroutine wedged forever.
	fd, err := unix.Open(dev, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dev, err)
	}
	f := os.NewFile(uintptr(fd), dev)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open %s: could not wrap file descriptor", dev)
	}
	if err := setRaw(f, baud); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("configure %s: %w", dev, err)
	}
	return NewStreamTransport(f, "serial "+dev), nil
}

// setRaw configures fd as a raw 8N1 tty.
//
// The Linux tty layer defaults to cooked mode, which would translate CR/LF,
// treat 0x11/0x13 as flow control and 0x03 as an interrupt — all of which
// occur freely inside binary protocol frames. Raw mode is not optional.
func setRaw(f *os.File, baud int) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.ICRNL | unix.IXON | unix.IXOFF | unix.IGNCR | unix.INLCR |
		unix.ISTRIP | unix.IXANY | unix.IMAXBEL | unix.BRKINT | unix.PARMRK | unix.INPCK
	t.Oflag &^= unix.OPOST | unix.ONLCR | unix.OCRNL | unix.ONOCR | unix.ONLRET
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL | unix.ICANON |
		unix.ISIG | unix.IEXTEN | unix.TOSTOP
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD
	if b, ok := baudConst(baud); ok {
		t.Cflag &^= unix.CBAUD
		t.Cflag |= b
		t.Ispeed = b
		t.Ospeed = b
	}
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

func baudConst(b int) (uint32, bool) {
	switch b {
	case 9600:
		return unix.B9600, true
	case 19200:
		return unix.B19200, true
	case 38400:
		return unix.B38400, true
	case 57600:
		return unix.B57600, true
	case 115200:
		return unix.B115200, true
	case 230400:
		return unix.B230400, true
	case 460800:
		return unix.B460800, true
	case 921600:
		return unix.B921600, true
	}
	return 0, false
}

// FindSerialPorts lists plausible companion devices, best guess first.
//
// /dev/serial/by-id entries come first because they are stable across reboots
// and across which USB port the node is plugged into — a raw /dev/ttyACM0 can
// become ttyACM1 the moment a second device appears.
func FindSerialPorts() ([]string, error) {
	var out []string
	seen := map[string]bool{}

	add := func(p string) {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			real = p
		}
		if seen[real] {
			return
		}
		seen[real] = true
		out = append(out, p)
	}

	byID, _ := filepath.Glob("/dev/serial/by-id/*")
	sort.Strings(byID)
	for _, p := range byID {
		add(p)
	}

	for _, pat := range []string{"/dev/ttyACM*", "/dev/ttyUSB*"} {
		m, _ := filepath.Glob(pat)
		sort.Strings(m)
		for _, p := range m {
			add(p)
		}
	}
	return out, nil
}

// SerialPortLabel renders a device path for a menu, preferring the by-id name
// because it says what the hardware actually is.
func SerialPortLabel(p string) string {
	if strings.HasPrefix(p, "/dev/serial/by-id/") {
		return strings.TrimPrefix(p, "/dev/serial/by-id/")
	}
	return p
}
