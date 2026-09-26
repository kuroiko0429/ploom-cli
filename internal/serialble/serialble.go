// Package serialble talks to an M5Stick (or any microcontroller running the
// matching firmware, see tools/M5central) over a USB-serial line, using it
// as a BLE central. This exists because on this project's target hardware
// BlueZ's Device1.Connect() cannot hold a connection to the Ploom aura
// device long enough to complete pairing (see README "Known device
// behavior"), while a NimBLE-based microcontroller connects fine. The
// microcontroller does the actual BLE work; this package is a thin RPC
// client over a line-based text protocol, producing the same
// ploom-cli/internal/bledevice interfaces as the BlueZ backend so the rest
// of ploom-cli (REPL, config) doesn't need to know which transport is in
// use.
//
// Protocol (see tools/M5central/README.md for the authoritative copy):
// every line the firmware sends that starts with '#' is a protocol event;
// everything else is a debug/log line, passed through to the console
// as-is. Commands sent to the firmware are plain text lines with no prefix.
package serialble

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"

	"github.com/kuroiko0429/ploom-cli/internal/bledevice"
)

const commandTimeout = 5 * time.Second

// ScanResult is one advertisement observed during Scan.
type ScanResult struct {
	Address string
	Name    string
	RSSI    int
	UUIDs   []string
}

// AuthInfo reports the outcome of pairing/bonding after Connect.
type AuthInfo struct {
	Bonded        bool
	Encrypted     bool
	Authenticated bool
}

type event struct {
	tag    string
	fields []string
}

// Bridge is an open connection to the microcontroller's serial port.
type Bridge struct {
	port serial.Port

	mu     sync.Mutex // serializes request/response conversations
	respCh chan event

	pendingReadHandle atomic.Int64 // handle of an in-flight Read, or -1

	notifyMu  sync.Mutex
	notifyCbs map[uint16]func([]byte)

	advCB     atomic.Pointer[func(ScanResult)]
	scanEndCh chan struct{}

	connected atomic.Bool

	// Logf receives passthrough debug/log lines from the firmware (any
	// line not starting with '#'). If nil, such lines are discarded.
	Logf func(line string)
}

// Open opens the serial port at baud and waits up to readyTimeout for
// evidence the firmware is alive: either a fresh "#READY" banner (opening
// most USB-serial adapters resets the microcontroller, so a fresh boot is
// the common case) or, if the board was already running (some USB-serial
// chips don't reset on a bare port open, so a previously-flashed/running
// board stays up and never reprints "#READY"), a successful PING/PONG
// round trip.
func Open(portName string, baud int, readyTimeout time.Duration) (*Bridge, error) {
	port, err := serial.Open(portName, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", portName, err)
	}

	b := &Bridge{
		port:      port,
		respCh:    make(chan event, 64),
		notifyCbs: make(map[uint16]func([]byte)),
		scanEndCh: make(chan struct{}, 1),
	}
	b.pendingReadHandle.Store(-1)
	go b.readLoop()

	deadline := time.Now().Add(readyTimeout)
	pingTicker := time.NewTicker(500 * time.Millisecond)
	defer pingTicker.Stop()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			port.Close()
			return nil, fmt.Errorf("no response from %s within %s (wrong firmware, wrong port, or still booting)", portName, readyTimeout)
		}
		select {
		case ev := <-b.respCh:
			if ev.tag == "READY" || ev.tag == "PONG" {
				return b, nil
			}
		case <-pingTicker.C:
			// Nudge an already-running board that won't reset on port open
			// and so will never (re-)print "#READY".
			_ = b.send("PING")
		case <-time.After(remaining):
			port.Close()
			return nil, fmt.Errorf("no response from %s within %s (wrong firmware, wrong port, or still booting)", portName, readyTimeout)
		}
	}
}

// Close closes the serial port.
func (b *Bridge) Close() error {
	return b.port.Close()
}

func (b *Bridge) readLoop() {
	sc := bufio.NewScanner(b.port)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "#") {
			if line != "" && b.Logf != nil {
				b.Logf(line)
			}
			continue
		}
		fields := strings.Fields(line[1:])
		if len(fields) == 0 {
			continue
		}
		tag, rest := fields[0], fields[1:]

		switch tag {
		case "ADV":
			if cb := b.advCB.Load(); cb != nil {
				if r, ok := parseAdv(rest); ok {
					(*cb)(r)
				}
			}
		case "SCANEND":
			select {
			case b.scanEndCh <- struct{}{}:
			default:
			}
		case "DISCONNECTED":
			b.connected.Store(false)
		case "VAL":
			handle, val, ok := parseVal(rest)
			if !ok {
				continue
			}
			if pr := b.pendingReadHandle.Load(); pr >= 0 && uint16(pr) == handle {
				b.respCh <- event{tag, rest}
				continue
			}
			b.notifyMu.Lock()
			cb, has := b.notifyCbs[handle]
			b.notifyMu.Unlock()
			if has {
				cb(val)
			}
			// Unregistered, unsolicited VAL: drop (nothing is waiting on it).
		default:
			b.respCh <- event{tag, rest}
		}
	}
}

func parseAdv(fields []string) (ScanResult, bool) {
	// #ADV <addr> <rssi> <uuids_csv_or_-> <name...to end of line, may be empty>
	if len(fields) < 3 {
		return ScanResult{}, false
	}
	rssi, _ := strconv.Atoi(fields[1])
	r := ScanResult{Address: fields[0], RSSI: rssi}
	if fields[2] != "-" {
		r.UUIDs = strings.Split(fields[2], ",")
	}
	if len(fields) > 3 {
		r.Name = strings.Join(fields[3:], " ")
	}
	return r, true
}

func parseVal(fields []string) (handle uint16, val []byte, ok bool) {
	if len(fields) < 1 {
		return 0, nil, false
	}
	h, err := strconv.ParseUint(fields[0], 10, 16)
	if err != nil {
		return 0, nil, false
	}
	var data []byte
	if len(fields) >= 2 && fields[1] != "-" {
		data, err = hex.DecodeString(fields[1])
		if err != nil {
			return 0, nil, false
		}
	}
	return uint16(h), data, true
}

func drain(ch chan event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// send writes cmd (without a trailing newline) to the firmware.
func (b *Bridge) send(cmd string) error {
	_, err := b.port.Write([]byte(cmd + "\n"))
	return err
}

// request sends cmd and waits for the first event whose tag is one of
// successTags (returned as-is) or "ERR" (turned into an error). Any other
// tag observed while waiting is ignored. Must be called with b.mu held.
func (b *Bridge) request(cmd string, successTags ...string) (event, error) {
	drain(b.respCh)
	if err := b.send(cmd); err != nil {
		return event{}, err
	}
	deadline := time.After(commandTimeout)
	for {
		select {
		case ev := <-b.respCh:
			for _, t := range successTags {
				if ev.tag == t {
					return ev, nil
				}
			}
			if ev.tag == "ERR" {
				return event{}, fmt.Errorf("%s", strings.Join(ev.fields, " "))
			}
		case <-deadline:
			return event{}, fmt.Errorf("timeout waiting for reply to %q", cmd)
		}
	}
}

// Ping checks that the firmware is alive and responsive.
func (b *Bridge) Ping() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.request("PING", "PONG")
	return err
}

// ErrScanTimeout is returned by Scan when no matching device was found
// before the deadline.
var ErrScanTimeout = fmt.Errorf("serialble: no matching device found before timeout")

// ErrScanInterrupted is returned by Scan when the user aborts (Ctrl+C)
// before a matching device was found.
var ErrScanInterrupted = fmt.Errorf("serialble: scan interrupted")

// Scan starts a BLE scan on the microcontroller and calls onSeen for every
// advertisement observed. It returns the first result for which onSeen
// returns true.
func (b *Bridge) Scan(timeout time.Duration, onSeen func(ScanResult) bool) (ScanResult, error) {
	found := make(chan ScanResult, 1)
	var once sync.Once
	cb := func(r ScanResult) {
		if onSeen(r) {
			once.Do(func() { found <- r })
		}
	}
	b.advCB.Store(&cb)
	defer b.advCB.Store(nil)

	if err := b.send("SCAN"); err != nil {
		return ScanResult{}, err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	var result ScanResult
	var err error
	select {
	case result = <-found:
	case <-time.After(timeout):
		err = ErrScanTimeout
	case <-sigCh:
		err = ErrScanInterrupted
	}
	b.send("STOP")
	return result, err
}

// Connect connects to addr (must have already been seen by a preceding
// Scan - the firmware connects by looking the address up in its own
// scan-result list), pairs/bonds, and discovers the full GATT table.
func (b *Bridge) Connect(addr string, timeout time.Duration) (*Device, []bledevice.Service, AuthInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	drain(b.respCh)
	if err := b.send("CONNECT " + addr); err != nil {
		return nil, nil, AuthInfo{}, err
	}

	var services []bledevice.Service
	var auth AuthInfo
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-b.respCh:
			switch ev.tag {
			case "CONNECTING", "CONNECTED":
				// milestone only
			case "CONNECTFAIL":
				return nil, nil, AuthInfo{}, fmt.Errorf("connect to %s failed", addr)
			case "ERR":
				return nil, nil, AuthInfo{}, fmt.Errorf("%s", strings.Join(ev.fields, " "))
			case "AUTH":
				auth = parseAuth(ev.fields)
			case "SVC":
				if len(ev.fields) < 1 {
					continue
				}
				services = append(services, bledevice.Service{UUID: ev.fields[0]})
			case "CHR":
				if len(services) == 0 || len(ev.fields) < 2 {
					continue
				}
				handle, err := strconv.ParseUint(ev.fields[1], 10, 16)
				if err != nil {
					continue
				}
				ch := &Characteristic{bridge: b, uuid: ev.fields[0], handle: uint16(handle)}
				last := len(services) - 1
				services[last].Chars = append(services[last].Chars, ch)
			case "DISCOVERDONE":
				b.connected.Store(true)
				return &Device{bridge: b, addr: addr}, services, auth, nil
			}
		case <-deadline:
			return nil, nil, AuthInfo{}, fmt.Errorf("timeout connecting to %s", addr)
		}
	}
}

func parseAuth(fields []string) AuthInfo {
	var a AuthInfo
	for _, f := range fields {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		v := kv[1] == "1"
		switch kv[0] {
		case "bonded":
			a.Bonded = v
		case "encrypted":
			a.Encrypted = v
		case "authenticated":
			a.Authenticated = v
		}
	}
	return a
}

// Device is a connected peripheral reached through the microcontroller
// bridge. It implements bledevice.Device.
type Device struct {
	bridge *Bridge
	addr   string
}

func (d *Device) Disconnect() error {
	d.bridge.mu.Lock()
	defer d.bridge.mu.Unlock()
	_, err := d.bridge.request("DISCONNECT", "OK")
	return err
}

func (d *Device) Connected() (bool, error) {
	return d.bridge.connected.Load(), nil
}

func (d *Device) MTU() (uint16, error) {
	d.bridge.mu.Lock()
	defer d.bridge.mu.Unlock()
	ev, err := d.bridge.request("MTU", "MTU")
	if err != nil {
		return 0, err
	}
	if len(ev.fields) < 1 {
		return 0, fmt.Errorf("malformed MTU response")
	}
	n, err := strconv.ParseUint(ev.fields[0], 10, 16)
	return uint16(n), err
}

// Characteristic is a GATT characteristic reached through the
// microcontroller bridge. It implements bledevice.Characteristic.
type Characteristic struct {
	bridge *Bridge
	uuid   string
	handle uint16
}

func (c *Characteristic) UUID() string { return c.uuid }

func (c *Characteristic) Read() ([]byte, error) {
	b := c.bridge
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pendingReadHandle.Store(int64(c.handle))
	defer b.pendingReadHandle.Store(-1)
	drain(b.respCh)

	if err := b.send(fmt.Sprintf("READ %d", c.handle)); err != nil {
		return nil, err
	}
	deadline := time.After(commandTimeout)
	for {
		select {
		case ev := <-b.respCh:
			switch ev.tag {
			case "VAL":
				handle, val, ok := parseVal(ev.fields)
				if !ok || handle != c.handle {
					continue
				}
				return val, nil
			case "ERR":
				return nil, fmt.Errorf("%s", strings.Join(ev.fields, " "))
			}
		case <-deadline:
			return nil, fmt.Errorf("timeout reading handle %d", c.handle)
		}
	}
}

func (c *Characteristic) Write(data []byte) error {
	b := c.bridge
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.request(fmt.Sprintf("WRITE %d %s", c.handle, hex.EncodeToString(data)), "OK")
	return err
}

func (c *Characteristic) WriteWithoutResponse(data []byte) error {
	b := c.bridge
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.request(fmt.Sprintf("WRITEC %d %s", c.handle, hex.EncodeToString(data)), "OK")
	return err
}

func (c *Characteristic) EnableNotifications(cb func([]byte)) error {
	b := c.bridge
	b.mu.Lock()
	on := "OFF"
	if cb != nil {
		on = "ON"
	}
	_, err := b.request(fmt.Sprintf("NOTIFY %d %s", c.handle, on), "OK")
	b.mu.Unlock()
	if err != nil {
		return err
	}

	b.notifyMu.Lock()
	defer b.notifyMu.Unlock()
	if cb == nil {
		delete(b.notifyCbs, c.handle)
	} else {
		b.notifyCbs[c.handle] = cb
	}
	return nil
}
