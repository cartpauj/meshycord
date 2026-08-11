//go:build linux

package meshcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"tinygo.org/x/bluetooth"
)

// MeshCore's companion is a plain Nordic UART Service.
// src/helpers/esp32/SerialBLEInterface.cpp:7
var (
	nusService = bluetooth.NewUUID([16]byte{
		0x6E, 0x40, 0x00, 0x01, 0xB5, 0xA3, 0xF3, 0x93,
		0xE0, 0xA9, 0xE5, 0x0E, 0x24, 0xDC, 0xCA, 0x9E})
	nusRX = bluetooth.NewUUID([16]byte{ // we write commands here
		0x6E, 0x40, 0x00, 0x02, 0xB5, 0xA3, 0xF3, 0x93,
		0xE0, 0xA9, 0xE5, 0x0E, 0x24, 0xDC, 0xCA, 0x9E})
	nusTX = bluetooth.NewUUID([16]byte{ // we subscribe for notifications
		0x6E, 0x40, 0x00, 0x03, 0xB5, 0xA3, 0xF3, 0x93,
		0xE0, 0xA9, 0xE5, 0x0E, 0x24, 0xDC, 0xCA, 0x9E})
)

// BLEDialer connects to a MeshCore companion over Bluetooth Low Energy.
//
// This is the transport to use when the bridge is not physically next to the
// radio. It is also the flakiest and the only genuinely OS-specific component,
// which is why serial is the recommended default.
//
// The companion's TX characteristic is ESP_GATT_PERM_READ_ENC_MITM, so an
// encrypted, MITM-protected, AUTHENTICATED link is mandatory: the client must
// bond with passkey entry, not merely connect. See the agent below.
type BLEDialer struct {
	// Address is a MAC such as "AA:BB:CC:DD:EE:FF". When set it wins over Name.
	Address string
	// Name matches as a substring of the advertised name. Empty means "the
	// first device advertising the MeshCore NUS service".
	Name string
	// PIN is the companion's six-digit fixed pairing PIN, set in the MeshCore
	// phone app. MeshCore's own default is 123456.
	//
	// The node MUST have a fixed PIN configured. Without one it generates a
	// random PIN each boot and shows it on its screen, which a headless bridge
	// cannot read.
	PIN string
	// ScanTimeout bounds the discovery phase. 15s when zero.
	ScanTimeout time.Duration

	Log *slog.Logger
}

func (d BLEDialer) Describe() string {
	switch {
	case d.Address != "":
		return "ble " + d.Address
	case d.Name != "":
		return "ble name~" + d.Name
	default:
		return "ble (first MeshCore node)"
	}
}

func (d BLEDialer) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d BLEDialer) pin() string {
	if d.PIN == "" {
		return "123456" // MeshCore's default
	}
	return d.PIN
}

// bleAdapterOnce guards Enable(). BlueZ tolerates repeated enables badly and
// the adapter is process-wide anyway.
var (
	bleAdapterOnce sync.Once
	bleAdapterErr  error
)

// Stale-bond recovery state.
//
// Process-wide rather than per-dialer because BLEDialer is used as a value and
// there is only ever one radio link. `lastTarget` is the address the most
// recent scan settled on, which is the only way to name the bond when the
// dialer was configured by name rather than by MAC.
var (
	bleStateMu     sync.Mutex
	bleLastTarget  string
	bleAuthFailure int
)

func rememberBLETarget(addr string) {
	bleStateMu.Lock()
	bleLastTarget = addr
	bleStateMu.Unlock()
}

// SessionEstablished clears the stale-bond counter.
func (d BLEDialer) SessionEstablished() {
	bleStateMu.Lock()
	bleAuthFailure = 0
	bleStateMu.Unlock()
}

// SessionFailed drops the bond when the handshake keeps failing on a link that
// connected successfully.
//
// That combination means the link is encrypted but NOT authenticated, so the
// companion rejects every write and CMD_APP_START times out. A stale bond is
// reused indefinitely and stays unauthenticated forever — most often after the
// PIN was changed on the node — so the only fix is to remove it and re-pair.
//
// Purged on the first failure and every third after that, matching the ESP32
// firmware: purging on every attempt would re-pair needlessly when the real
// problem is a node that is simply busy or out of range.
func (d BLEDialer) SessionFailed(err error) {
	log := d.logger()

	bleStateMu.Lock()
	bleAuthFailure++
	n := bleAuthFailure
	target := bleLastTarget
	bleStateMu.Unlock()

	if target == "" || n%3 != 1 {
		return
	}
	log.Warn("the node connected but would not complete the handshake; "+
		"dropping the bond so the next attempt re-pairs. If this repeats, check the "+
		"pairing PIN matches the one set in the MeshCore app.",
		"addr", target, "failures", n, "err", err)

	if ferr := ForgetBond(target, log); ferr != nil {
		log.Warn("could not remove the stale bond", "addr", target, "err", ferr)
		return
	}
	log.Info("bond removed; the next connection will pair from scratch", "addr", target)
}

func (d BLEDialer) Dial(ctx context.Context) (Transport, error) {
	log := d.logger()

	adapter := bluetooth.DefaultAdapter
	bleAdapterOnce.Do(func() { bleAdapterErr = adapter.Enable() })
	if bleAdapterErr != nil {
		return nil, fmt.Errorf("enable bluetooth adapter (is bluetoothd running?): %w", bleAdapterErr)
	}

	// The pairing agent must exist BEFORE the connection attempt: BlueZ asks
	// it for the passkey during pairing, and with no agent registered the
	// pairing silently falls back to Just Works — which produces an encrypted
	// but UNAUTHENTICATED link, and the companion then rejects every write.
	// That failure presents as "connected fine, nothing works".
	agent, err := ensurePairingAgent(d.pin(), log)
	if err != nil {
		// Not fatal on its own: an already-bonded device needs no agent. Say
		// so rather than refusing to try.
		log.Warn("could not register a bluetooth pairing agent; a first-time pairing will fail", "err", err)
	}

	timeout := d.ScanTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	found, err := d.scan(scanCtx, adapter, log)
	if err != nil {
		return nil, err
	}
	// Remember which device we settled on, so a stale bond can be named later
	// even when the dialer was configured by name rather than by MAC.
	rememberBLETarget(found.Address.String())

	// Bond before connecting for GATT. BlueZ's Pair() both connects and runs
	// the security procedure; doing it explicitly is what gets our agent
	// asked for the passkey.
	if agent != nil {
		if err := agent.pair(ctx, found.Address.String(), log); err != nil {
			log.Warn("pairing did not complete", "addr", found.Address.String(), "err", err)
		}
	}

	dev, err := adapter.Connect(found.Address, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", found.Address.String(), err)
	}

	t, err := newBLETransport(dev, found.LocalName(), log)
	if err != nil {
		_ = dev.Disconnect()
		return nil, err
	}
	if agent != nil && !agent.sawPasskeyRequest() {
		// Either the bond already existed (fine, and the common case after the
		// first run) or the link came up unauthenticated (not fine). We cannot
		// tell the two apart from here, so this is a note rather than an
		// error — writes failing immediately afterwards is the real symptom.
		log.Debug("no passkey was requested; using an existing bond")
	}
	return t, nil
}

// scan looks for the node, matching by address, then name, then by the NUS
// service UUID in the advertisement.
func (d BLEDialer) scan(ctx context.Context, adapter *bluetooth.Adapter, log *slog.Logger) (bluetooth.ScanResult, error) {
	var (
		mu     sync.Mutex
		result bluetooth.ScanResult
		got    bool
	)
	wantAddr := strings.ToUpper(strings.TrimSpace(d.Address))
	wantName := strings.ToLower(strings.TrimSpace(d.Name))

	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = adapter.StopScan()
		close(done)
	}()

	err := adapter.Scan(func(a *bluetooth.Adapter, sr bluetooth.ScanResult) {
		var match bool
		switch {
		case wantAddr != "":
			match = strings.EqualFold(sr.Address.String(), wantAddr)
		case wantName != "":
			match = strings.Contains(strings.ToLower(sr.LocalName()), wantName)
		default:
			match = sr.HasServiceUUID(nusService)
		}
		if !match {
			return
		}
		mu.Lock()
		if !got {
			result, got = sr, true
			log.Info("found MeshCore node", "name", sr.LocalName(), "addr", sr.Address.String(), "rssi", sr.RSSI)
		}
		mu.Unlock()
		_ = a.StopScan()
	})
	// StopScan from either path makes Scan return; the context goroutine may
	// still be waiting, so make sure it is released.
	select {
	case <-done:
	default:
	}

	mu.Lock()
	defer mu.Unlock()
	if got {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("bluetooth scan: %w", err)
	}
	return result, errors.New("no MeshCore node found: check it is powered, in range, and not already connected to a phone")
}

// ---------------------------------------------------------------------------
// The transport itself
// ---------------------------------------------------------------------------

// bleTransport carries frames over the Nordic UART Service.
//
// BLE gives frame boundaries for free — one notification is exactly one
// protocol frame — so unlike serial and TCP there is no length prefix here.
type bleTransport struct {
	dev  bluetooth.Device
	rx   bluetooth.DeviceCharacteristic // node's RX: we write
	name string

	frames chan []byte
	closed chan struct{}
	once   sync.Once
	log    *slog.Logger
}

func newBLETransport(dev bluetooth.Device, name string, log *slog.Logger) (*bleTransport, error) {
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{nusService})
	if err != nil || len(svcs) == 0 {
		return nil, fmt.Errorf("this device does not expose the MeshCore service: %w", err)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{nusRX, nusTX})
	if err != nil || len(chars) < 2 {
		return nil, fmt.Errorf("MeshCore service is missing its characteristics: %w", err)
	}

	var rx, tx bluetooth.DeviceCharacteristic
	for _, c := range chars {
		switch c.UUID() {
		case nusRX:
			rx = c
		case nusTX:
			tx = c
		}
	}

	t := &bleTransport{
		dev:    dev,
		rx:     rx,
		name:   name,
		frames: make(chan []byte, 64),
		closed: make(chan struct{}),
		log:    log,
	}

	// The notification callback runs on BlueZ's D-Bus signal goroutine and
	// must never block. A full buffer drops the frame rather than wedging the
	// whole Bluetooth stack — the same rule the ESP32 callback followed.
	if err := tx.EnableNotifications(func(buf []byte) {
		if len(buf) == 0 {
			return
		}
		f := make([]byte, len(buf))
		copy(f, buf)
		select {
		case t.frames <- f:
		case <-t.closed:
		default:
			t.log.Warn("dropped a BLE frame: reader is not keeping up")
		}
	}); err != nil {
		return nil, fmt.Errorf("subscribe to notifications: %w", err)
	}

	go t.watchConnection()
	return t, nil
}

// watchConnection notices a link that has gone away. BlueZ will not tell us
// through the GATT layer, and without this a dropped link is indistinguishable
// from a quiet mesh — the bridge would wait forever.
func (t *bleTransport) watchConnection() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.closed:
			return
		case <-tick.C:
			if ok, err := t.dev.Connected(); err != nil || !ok {
				t.log.Info("BLE link dropped")
				_ = t.Close()
				return
			}
		}
	}
}

func (t *bleTransport) Describe() string {
	if t.name != "" {
		return "ble " + t.name + " (" + t.dev.Address.String() + ")"
	}
	return "ble " + t.dev.Address.String()
}

func (t *bleTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case f, ok := <-t.frames:
		if !ok {
			return nil, errors.New("ble: link closed")
		}
		return f, nil
	case <-t.closed:
		return nil, errors.New("ble: link closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *bleTransport) WriteFrame(ctx context.Context, f []byte) error {
	select {
	case <-t.closed:
		return errors.New("ble: link closed")
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := t.rx.Write(f)
	return err
}

func (t *bleTransport) Close() error {
	t.once.Do(func() {
		close(t.closed)
		_ = t.dev.Disconnect()
	})
	return nil
}

// ---------------------------------------------------------------------------
// BlueZ pairing agent
//
// This is the piece with no equivalent in the ESP32 firmware, where NimBLE
// took a passkey callback directly. On Linux, pairing is bluetoothd's job, and
// it asks a registered "agent" over D-Bus for whatever the pairing needs.
//
// The capability string matters more than it looks. "KeyboardOnly" means "this
// client can type a passkey", which is what makes BlueZ perform passkey entry
// and produce an AUTHENTICATED link. "NoInputNoOutput" forces Just Works
// pairing, which is encrypted but never authenticated — and the companion
// rejects writes on an unauthenticated link.
// ---------------------------------------------------------------------------

const (
	agentPath       = dbus.ObjectPath("/org/meshycord/agent")
	agentIface      = "org.bluez.Agent1"
	agentManager    = "org.bluez.AgentManager1"
	bluezService    = "org.bluez"
	bluezRootPath   = dbus.ObjectPath("/org/bluez")
	agentCapKeyOnly = "KeyboardOnly"
)

type pairingAgent struct {
	conn *dbus.Conn
	pin  string
	log  *slog.Logger

	mu      sync.Mutex
	asked   bool
	adapter string // e.g. "hci0", learned from the first device path we see
}

var (
	agentOnce sync.Once
	agentInst *pairingAgent
	agentErr  error
)

// ensurePairingAgent registers a single process-wide agent, once.
func ensurePairingAgent(pin string, log *slog.Logger) (*pairingAgent, error) {
	agentOnce.Do(func() {
		conn, err := dbus.SystemBus()
		if err != nil {
			agentErr = fmt.Errorf("connect to the system D-Bus: %w", err)
			return
		}
		a := &pairingAgent{conn: conn, pin: pin, log: log}
		if err := conn.Export(a, agentPath, agentIface); err != nil {
			agentErr = fmt.Errorf("export the pairing agent: %w", err)
			return
		}
		mgr := conn.Object(bluezService, bluezRootPath)
		if call := mgr.Call(agentManager+".RegisterAgent", 0, agentPath, agentCapKeyOnly); call.Err != nil {
			// AlreadyExists is benign — another instance of us, or a restart.
			if !strings.Contains(call.Err.Error(), "AlreadyExists") {
				agentErr = fmt.Errorf("register the pairing agent: %w", call.Err)
				return
			}
		}
		// Becoming the default agent stops bluetoothd handing the request to
		// whatever desktop agent may also be listening — on a headless Pi
		// there is none, and the pairing would simply be rejected.
		if call := mgr.Call(agentManager+".RequestDefaultAgent", 0, agentPath); call.Err != nil {
			log.Debug("could not become the default pairing agent", "err", call.Err)
		}
		log.Info("bluetooth pairing agent registered", "capability", agentCapKeyOnly)
		agentInst = a
	})
	if agentInst != nil {
		agentInst.setPIN(pin)
	}
	return agentInst, agentErr
}

func (a *pairingAgent) setPIN(pin string) {
	a.mu.Lock()
	a.pin = pin
	a.mu.Unlock()
}

func (a *pairingAgent) sawPasskeyRequest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// pair runs BlueZ's pairing procedure if the device is not already bonded.
//
// A device that is already paired must NOT be paired again: BlueZ answers
// AlreadyExists and, worse, a stale bond from a node whose PIN has since
// changed will keep being reused and keep failing. That case is handled by
// Forget, which the caller reaches for after repeated auth failures.
func (a *pairingAgent) pair(ctx context.Context, mac string, log *slog.Logger) error {
	obj, err := a.deviceObject(mac)
	if err != nil {
		return err
	}
	if paired, err := getBoolProp(obj, "org.bluez.Device1", "Paired"); err == nil && paired {
		log.Debug("device is already bonded", "addr", mac)
		return nil
	}

	a.mu.Lock()
	a.asked = false
	a.mu.Unlock()

	// Pair blocks for the whole security procedure, which involves at least
	// one round trip to the radio.
	call := obj.CallWithContext(ctx, "org.bluez.Device1.Pair", 0)
	if call.Err != nil {
		if strings.Contains(call.Err.Error(), "AlreadyExists") {
			return nil
		}
		return call.Err
	}
	// Trusting the device stops bluetoothd asking for authorisation on every
	// later reconnect, which on a headless box nobody is there to answer.
	if c := obj.Call("org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.Device1", "Trusted", dbus.MakeVariant(true)); c.Err != nil {
		log.Debug("could not mark the device trusted", "err", c.Err)
	}
	log.Info("bonded with the MeshCore node", "addr", mac)
	return nil
}

// Forget removes a bond so the next attempt re-pairs from scratch.
//
// The ESP32 hit this exactly: a stale bond is reused, stays unauthenticated
// forever, and the bridge retries the dead bond indefinitely. Changing the
// PIN on the node produces the same state. Dropping the bond is the only fix.
func (a *pairingAgent) Forget(mac string) error {
	obj, err := a.deviceObject(mac)
	if err != nil {
		return err
	}
	_ = obj // resolved for its path only
	adapterPath := dbus.ObjectPath("/org/bluez/" + a.adapterName())
	ad := a.conn.Object(bluezService, adapterPath)
	devPath := dbus.ObjectPath(string(adapterPath) + "/dev_" + strings.ReplaceAll(strings.ToUpper(mac), ":", "_"))
	return ad.Call("org.bluez.Adapter1.RemoveDevice", 0, devPath).Err
}

func (a *pairingAgent) adapterName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.adapter == "" {
		return "hci0"
	}
	return a.adapter
}

func (a *pairingAgent) deviceObject(mac string) (dbus.BusObject, error) {
	if a.conn == nil {
		return nil, errors.New("no D-Bus connection")
	}
	path := dbus.ObjectPath("/org/bluez/" + a.adapterName() + "/dev_" +
		strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(mac)), ":", "_"))
	return a.conn.Object(bluezService, path), nil
}

func getBoolProp(obj dbus.BusObject, iface, name string) (bool, error) {
	v, err := obj.GetProperty(iface + "." + name)
	if err != nil {
		return false, err
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("%s is not a boolean", name)
	}
	return b, nil
}

// --- org.bluez.Agent1 implementation ---------------------------------------
//
// Method names and signatures are fixed by BlueZ. Every one of them must
// exist, even the ones a KeyboardOnly agent never sees, or bluetoothd errors
// out mid-pairing with UnknownMethod.

// RequestPinCode answers legacy (pre-4.2) pairing, which asks for a string.
func (a *pairingAgent) RequestPinCode(device dbus.ObjectPath) (string, *dbus.Error) {
	a.noteAsked(device)
	a.log.Info("bluetooth: supplying the pairing PIN", "device", device)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pin, nil
}

// RequestPasskey answers Secure Simple Pairing, which asks for a number.
// This is the one MeshCore actually uses.
func (a *pairingAgent) RequestPasskey(device dbus.ObjectPath) (uint32, *dbus.Error) {
	a.noteAsked(device)
	a.mu.Lock()
	pin := a.pin
	a.mu.Unlock()

	var n uint32
	for _, c := range pin {
		if c < '0' || c > '9' {
			a.log.Error("the configured pairing PIN is not six digits", "pin_length", len(pin))
			return 0, dbus.MakeFailedError(errors.New("pairing PIN must be digits"))
		}
		n = n*10 + uint32(c-'0')
	}
	a.log.Info("bluetooth: supplying the pairing passkey", "device", device)
	return n, nil
}

// DisplayPasskey and DisplayPinCode exist so BlueZ never fails with
// UnknownMethod. A headless bridge has no screen, so they only log.
func (a *pairingAgent) DisplayPasskey(device dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	a.log.Info("bluetooth: node is showing a passkey", "device", device, "passkey", passkey)
	return nil
}

func (a *pairingAgent) DisplayPinCode(device dbus.ObjectPath, pincode string) *dbus.Error {
	a.log.Info("bluetooth: node is showing a PIN", "device", device, "pin", pincode)
	return nil
}

// RequestConfirmation is numeric comparison. There is nobody to compare on a
// headless box, so accept — the link is still encrypted, and MeshCore's
// KeyboardOnly path means we should not reach here in normal operation.
func (a *pairingAgent) RequestConfirmation(device dbus.ObjectPath, passkey uint32) *dbus.Error {
	a.log.Info("bluetooth: confirming pairing", "device", device, "passkey", passkey)
	return nil
}

func (a *pairingAgent) RequestAuthorization(device dbus.ObjectPath) *dbus.Error {
	a.noteAsked(device)
	return nil
}

func (a *pairingAgent) AuthorizeService(device dbus.ObjectPath, uuid string) *dbus.Error {
	return nil
}

func (a *pairingAgent) Cancel() *dbus.Error {
	a.log.Warn("bluetooth: pairing was cancelled by the node")
	return nil
}

func (a *pairingAgent) Release() *dbus.Error { return nil }

// noteAsked records that a passkey was requested, and learns which adapter the
// device sits under so Forget can address it later.
func (a *pairingAgent) noteAsked(device dbus.ObjectPath) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = true
	// Paths look like /org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF
	parts := strings.Split(string(device), "/")
	if len(parts) >= 4 && strings.HasPrefix(parts[3], "hci") {
		a.adapter = parts[3]
	}
}

// ForgetBond drops the bond for a MAC, so the next connection re-pairs. Used
// by the web UI when a PIN has changed on the node.
func ForgetBond(mac string, log *slog.Logger) error {
	a, err := ensurePairingAgent("123456", log)
	if err != nil || a == nil {
		return fmt.Errorf("no bluetooth agent available: %w", err)
	}
	return a.Forget(mac)
}
