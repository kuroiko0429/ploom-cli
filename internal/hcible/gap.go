package hcible

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

// ErrScanTimeout is returned by Scan when no matching device was found
// before the deadline.
var ErrScanTimeout = errors.New("hcible: no matching device found before timeout")

// ErrScanInterrupted is returned by Scan when the user aborts (Ctrl+C)
// before a matching device was found.
var ErrScanInterrupted = errors.New("hcible: scan interrupted")

// ScanResult is one advertisement observed during Scan, with the
// advertising data already parsed.
type ScanResult struct {
	Address      [6]byte
	AddressType  byte // 0 = public, 1 = random
	RSSI         int8
	LocalName    string
	ServiceUUIDs []string
}

func (r ScanResult) AddressString() string { return formatAddr(r.Address) }

func formatAddr(addr [6]byte) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", addr[5], addr[4], addr[3], addr[2], addr[1], addr[0])
}

// ParseAddr parses a colon-separated MAC address string (as printed by
// formatAddr/AddressString) back into wire-order bytes.
func ParseAddr(s string) ([6]byte, error) {
	var out [6]byte
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return out, fmt.Errorf("hcible: invalid address %q", s)
	}
	for i := range 6 {
		var b int
		if _, err := fmt.Sscanf(parts[i], "%02x", &b); err != nil {
			return out, fmt.Errorf("hcible: invalid address %q: %w", s, err)
		}
		out[5-i] = byte(b)
	}
	return out, nil
}

type evtLEConnectionCompleteParams struct {
	status       byte
	handle       uint16
	role         byte
	peerAddrType byte
	peerAddr     [6]byte
	interval     uint16
	latency      uint16
	timeout      uint16
}

type advertisingReport struct {
	addrType byte
	addr     [6]byte
	rssi     int8
	data     []byte
}

func (h *HCI) handleLEMeta(params []byte) {
	if len(params) < 1 {
		return
	}
	subevent := params[0]
	body := params[1:]

	switch subevent {
	case subevtLEConnectionComplete:
		if len(body) < 18 {
			return
		}
		var p evtLEConnectionCompleteParams
		p.status = body[0]
		p.handle = binary.LittleEndian.Uint16(body[1:3])
		p.role = body[3]
		p.peerAddrType = body[4]
		copy(p.peerAddr[:], body[5:11])
		p.interval = binary.LittleEndian.Uint16(body[11:13])
		p.latency = binary.LittleEndian.Uint16(body[13:15])
		p.timeout = binary.LittleEndian.Uint16(body[15:17])

		h.mu.Lock()
		cb := h.connHandler
		h.mu.Unlock()
		if cb != nil {
			cb(p)
		}

	case subevtLEAdvertisingReport:
		h.mu.Lock()
		cb := h.advHandler
		h.mu.Unlock()
		if cb == nil {
			return
		}
		for _, r := range parseAdvertisingReports(body) {
			cb(r)
		}
	}
}

// parseAdvertisingReports decodes the struct-of-arrays layout of the LE
// Advertising Report event (Core Spec Vol 4 Part E 7.7.65.2).
func parseAdvertisingReports(body []byte) []advertisingReport {
	if len(body) < 1 {
		return nil
	}
	numReports := int(body[0])
	pos := 1
	need := func(n int) bool { return pos+n <= len(body) }

	if !need(numReports) {
		return nil
	}
	pos += numReports // event types: not used

	if !need(numReports) {
		return nil
	}
	addrTypes := body[pos : pos+numReports]
	pos += numReports

	if !need(numReports * 6) {
		return nil
	}
	addrs := make([][6]byte, numReports)
	for i := range numReports {
		copy(addrs[i][:], body[pos:pos+6])
		pos += 6
	}

	if !need(numReports) {
		return nil
	}
	lens := body[pos : pos+numReports]
	pos += numReports

	datas := make([][]byte, numReports)
	for i := range numReports {
		l := int(lens[i])
		if !need(l) {
			return nil
		}
		datas[i] = body[pos : pos+l]
		pos += l
	}

	if !need(numReports) {
		return nil
	}
	rssis := body[pos : pos+numReports]

	out := make([]advertisingReport, numReports)
	for i := range numReports {
		out[i] = advertisingReport{
			addrType: addrTypes[i],
			addr:     addrs[i],
			rssi:     int8(rssis[i]),
			data:     datas[i],
		}
	}
	return out
}

// parseAD extracts the local name and service UUIDs from a raw
// advertising/scan-response data payload (a sequence of
// length-prefixed AD structures, Core Spec Vol 3 Part C 11).
func parseAD(data []byte) (name string, serviceUUIDs []string) {
	for len(data) >= 2 {
		fieldLen := int(data[0])
		if fieldLen < 1 || fieldLen+1 > len(data) {
			break
		}
		fieldType := data[1]
		fieldData := data[2 : fieldLen+1]
		rest := data[fieldLen+1:]

		switch fieldType {
		case 0x08, 0x09: // Shortened / Complete Local Name
			name = string(fieldData)
		case 0x02, 0x03: // Incomplete / Complete List of 16-bit Service UUIDs
			for i := 0; i+2 <= len(fieldData); i += 2 {
				u := binary.LittleEndian.Uint16(fieldData[i : i+2])
				serviceUUIDs = append(serviceUUIDs, fmt.Sprintf("%04x", u))
			}
		case 0x06, 0x07: // Incomplete / Complete List of 128-bit Service UUIDs
			for i := 0; i+16 <= len(fieldData); i += 16 {
				serviceUUIDs = append(serviceUUIDs, uuid128String(fieldData[i:i+16]))
			}
		}
		data = rest
	}
	return name, serviceUUIDs
}

// uuid128String formats a 16-byte little-endian UUID (as carried in AD
// structures and ATT PDUs) as a standard hyphenated string.
func uuid128String(b []byte) string {
	// b is little-endian on the wire; the canonical string form is
	// big-endian, so reverse it first.
	var be [16]byte
	for i := range 16 {
		be[i] = b[15-i]
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", be[0:4], be[4:6], be[6:8], be[8:10], be[10:16])
}

// Scan starts LE scanning and calls onSeen for every advertisement (and
// scan response) observed, applying the AD-structure-derived name/UUIDs.
// It returns the first result for which onSeen returns true.
func (h *HCI) Scan(timeout time.Duration, onSeen func(ScanResult) bool) (ScanResult, error) {
	params := make([]byte, 7)
	params[0] = 0x01 // active scan
	binary.LittleEndian.PutUint16(params[1:3], 0x0012)
	binary.LittleEndian.PutUint16(params[3:5], 0x0012)
	params[5] = 0x00 // own address type: public
	params[6] = 0x00 // filter policy: accept all advertisements
	if _, err := h.sendCommandComplete(opLESetScanParameters, params, 2*time.Second); err != nil {
		return ScanResult{}, fmt.Errorf("LE Set Scan Parameters: %w", err)
	}

	found := make(chan ScanResult, 1)
	var once sync.Once
	h.mu.Lock()
	h.advHandler = func(r advertisingReport) {
		name, uuids := parseAD(r.data)
		sr := ScanResult{Address: r.addr, AddressType: r.addrType, RSSI: r.rssi, LocalName: name, ServiceUUIDs: uuids}
		if onSeen(sr) {
			once.Do(func() { found <- sr })
		}
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.advHandler = nil
		h.mu.Unlock()
	}()

	if _, err := h.sendCommandComplete(opLESetScanEnable, []byte{0x01, 0x00}, 2*time.Second); err != nil {
		return ScanResult{}, fmt.Errorf("LE Set Scan Enable: %w", err)
	}
	defer h.sendCommandComplete(opLESetScanEnable, []byte{0x00, 0x00}, 2*time.Second)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	select {
	case r := <-found:
		return r, nil
	case <-time.After(timeout):
		return ScanResult{}, ErrScanTimeout
	case <-sigCh:
		return ScanResult{}, ErrScanInterrupted
	}
}

// ConnParams are the LE connection parameters requested when connecting.
// Interval fields are in 1.25ms units, SupervisionTimeout in 10ms units,
// matching the raw HCI encoding (Core Spec Vol 4 Part E 7.8.12).
type ConnParams struct {
	MinInterval        uint16
	MaxInterval        uint16
	Latency            uint16
	SupervisionTimeout uint16
}

// DefaultConnParams matches the connection interval/timeout an Android
// phone was observed using against a Ploom aura unit (15ms / 5s) - see the
// main README's "Known device behavior" section. BlueZ's hardcoded 30-50ms
// default is not enough for that specific device to stay connected long
// enough to pair.
var DefaultConnParams = ConnParams{MinInterval: 12, MaxInterval: 12, Latency: 0, SupervisionTimeout: 500}

// Connect establishes an LE connection to addr. Unlike BlueZ, this issues
// exactly one HCI command (LE Create Connection) with the given parameters
// and does not send any other command (e.g. no automatic "LE Read Remote
// Used Features") before the caller gets a chance to run pairing - see the
// package doc comment for why that matters.
func (h *HCI) Connect(addr [6]byte, addrType byte, params ConnParams, timeout time.Duration) (*Connection, error) {
	// Best-effort: make sure scanning is stopped before connecting: most
	// controllers refuse (or silently never complete) LE Create Connection
	// while a scan is still active. A short grace period gives the
	// controller time to actually vacate the radio before we ask it to
	// initiate.
	h.sendCommandComplete(opLESetScanEnable, []byte{0x00, 0x00}, 1*time.Second)
	time.Sleep(50 * time.Millisecond)

	body := make([]byte, 25)
	binary.LittleEndian.PutUint16(body[0:2], 0x0060) // scan interval (60ms) used internally while connecting
	binary.LittleEndian.PutUint16(body[2:4], 0x0060) // scan window (60ms): continuous
	body[4] = 0x00                                   // filter policy: use peer address below
	body[5] = addrType
	copy(body[6:12], addr[:])
	body[12] = 0x00 // own address type: public
	binary.LittleEndian.PutUint16(body[13:15], params.MinInterval)
	binary.LittleEndian.PutUint16(body[15:17], params.MaxInterval)
	binary.LittleEndian.PutUint16(body[17:19], params.Latency)
	binary.LittleEndian.PutUint16(body[19:21], params.SupervisionTimeout)
	binary.LittleEndian.PutUint16(body[21:23], 0x0000) // min connection event length
	binary.LittleEndian.PutUint16(body[23:25], 0x0000) // max connection event length

	connCh := make(chan evtLEConnectionCompleteParams, 1)
	h.mu.Lock()
	h.connHandler = func(p evtLEConnectionCompleteParams) {
		select {
		case connCh <- p:
		default:
		}
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.connHandler = nil
		h.mu.Unlock()
	}()

	if err := h.sendCommandStatus(opLECreateConnection, body, 2*time.Second); err != nil {
		return nil, fmt.Errorf("LE Create Connection: %w", err)
	}

	select {
	case p := <-connCh:
		if p.status != 0x00 {
			return nil, fmt.Errorf("hcible: connect to %s failed: status 0x%02x", formatAddr(addr), p.status)
		}
		return newConnection(h, p.handle, p.peerAddr, p.peerAddrType), nil
	case <-time.After(timeout):
		h.sendCommandComplete(opLECreateConnectionCncl, nil, 2*time.Second)
		// The cancel can race with a connection that actually completed at
		// almost the same moment (arriving too late to cancel anything);
		// if that happens, an LE Connection Complete is still coming and
		// must not be left dangling on the controller uncleaned - a
		// half-forgotten connection here is exactly the kind of state that
		// can wedge a controller (observed on real hardware: an Intel
		// AX201 stopped responding to any HCI command, even at the USB
		// level, until the btusb driver was unloaded and reloaded).
		select {
		case p := <-connCh:
			if p.status == 0x00 {
				newConnection(h, p.handle, p.peerAddr, p.peerAddrType).Disconnect()
			}
		case <-time.After(500 * time.Millisecond):
		}
		return nil, fmt.Errorf("hcible: timeout connecting to %s", formatAddr(addr))
	}
}
