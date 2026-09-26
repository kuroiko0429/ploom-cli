// Package hcible is a from-scratch BLE central implementation (HCI, L2CAP,
// ATT, SMP legacy pairing) talking directly to a Bluetooth controller over
// a raw HCI_CHANNEL_USER socket, bypassing BlueZ/bluetoothd entirely.
//
// Why this exists: on this project's test hardware, BlueZ's Device1.Connect()
// cannot hold a connection to a Ploom aura device long enough to complete
// pairing (see the main README's "Known device behavior" section) - the
// peripheral disconnects after ~2 connection events, before BlueZ/the kernel
// ever gets around to exchanging SMP data. This package controls the exact
// HCI command sequence sent to the controller (in particular: it does not
// send "LE Read Remote Used Features" or anything else before the security
// handshake), to see whether that unblocks it.
//
// Root/CAP_NET_RAW is required to open the raw socket. Opening a
// HCI_CHANNEL_USER socket takes exclusive control of the adapter (the
// kernel signals bluetoothd that the adapter went away for the duration);
// no need to manually stop bluetoothd first, though on some kernels/drivers
// that may still be necessary if binding fails with EBUSY.
package hcible

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// HCI packet type indicator bytes (first byte of every read/write on a raw
// HCI socket - see net/bluetooth/hci_sock.c hci_sock_sendmsg()).
const (
	pktCommand = 0x01
	pktACLData = 0x02
	pktEvent   = 0x04
)

// HCI event codes used by this package.
const (
	evtDisconnectionComplete = 0x05
	evtEncryptionChange      = 0x08
	evtCommandComplete       = 0x0E
	evtCommandStatus         = 0x0F
	evtNumberOfCompleted     = 0x13
	evtLEMeta                = 0x3E
)

// LE Meta sub-event codes.
const (
	subevtLEConnectionComplete = 0x01
	subevtLEAdvertisingReport  = 0x02
)

// HCI command opcodes used by this package, as (OGF<<10 | OCF).
const (
	opReset                  = 0x0C03
	opSetEventMask           = 0x0C01
	opLESetEventMask         = 0x2001
	opLESetScanParameters    = 0x200B
	opLESetScanEnable        = 0x200C
	opLECreateConnection     = 0x200D
	opLECreateConnectionCncl = 0x200E
	opDisconnect             = 0x0406
	opLEStartEncryption      = 0x2019
	opReadBDAddr             = 0x1009
)

// event is a decoded HCI event or ACL data packet handed from the read loop
// to whichever part of the stack is waiting for it.
type event struct {
	code   byte // HCI event code (0x0E, 0x0F, 0x3E, ...)
	params []byte
}

// HCI owns a raw HCI_CHANNEL_USER socket to one adapter and implements the
// bare command/event transport. Higher layers (gap.go, l2cap.go, att.go,
// smp.go) build on top of it.
type HCI struct {
	fd int

	cmdMu   sync.Mutex // serializes command send + wait-for-response
	cmdResp chan event // delivers the Command Complete/Status for the pending command

	// aclHandler receives every ACL data packet's full L2CAP PDU, keyed by
	// nothing (single connection at a time is all this package supports).
	aclHandler func(payload []byte)

	// connHandler/disconnHandler/encHandler are set by gap.go/smp.go while
	// a connect/encryption operation is outstanding, and by the active
	// Device for disconnect notification.
	mu             sync.Mutex
	connHandler    func(evtLEConnectionCompleteParams)
	disconnHandler func(handle uint16, reason byte)
	encHandler     func(handle uint16, status byte, enabled bool)
	advHandler     func(advertisingReport)

	closeOnce sync.Once
	closed    chan struct{}
}

// parseAdapterID accepts "hci0"-style names or a bare index and returns the
// numeric device index.
func parseAdapterID(id string) (uint16, error) {
	id = strings.TrimPrefix(id, "hci")
	n, err := strconv.Atoi(id)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("hcible: invalid adapter %q (want e.g. \"hci0\")", id)
	}
	return uint16(n), nil
}

// Open takes exclusive raw control of the named adapter (e.g. "hci0").
// Requires root or CAP_NET_RAW.
func Open(adapterID string) (*HCI, error) {
	devID, err := parseAdapterID(adapterID)
	if err != nil {
		return nil, err
	}

	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("hcible: socket: %w", err)
	}

	sa := &unix.SockaddrHCI{Dev: devID, Channel: unix.HCI_CHANNEL_USER}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("hcible: bind hci%d user channel: %w (adapter busy/managed by bluetoothd? try: sudo systemctl stop bluetooth)", devID, err)
	}

	h := &HCI{
		fd:      fd,
		cmdResp: make(chan event, 1),
		closed:  make(chan struct{}),
	}
	go h.readLoop()

	if err := h.init(); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Close leaves the controller in a clean state (a best-effort HCI Reset -
// this also clears any connection attempt still in flight, e.g. after a
// Connect timeout) and releases the adapter back to the kernel (and, in
// turn, bluetoothd, once unmasked).
func (h *HCI) Close() error {
	h.sendCommandComplete(opReset, nil, 2*time.Second)
	h.closeOnce.Do(func() { close(h.closed) })
	return unix.Close(h.fd)
}

// init resets the controller and enables the event reporting this package
// depends on.
func (h *HCI) init() error {
	if _, err := h.sendCommandComplete(opReset, nil, 2*time.Second); err != nil {
		return fmt.Errorf("hcible: HCI Reset: %w", err)
	}

	// Classic Event Mask: enable Disconnection Complete (bit 4),
	// Encryption Change (bit 7), Encryption Key Refresh Complete (bit 47),
	// and LE Meta Event (bit 61). Command Complete/Status/Number Of
	// Completed Packets are always enabled regardless of this mask.
	mask := make([]byte, 8)
	setBit(mask, 4)
	setBit(mask, 7)
	setBit(mask, 47)
	setBit(mask, 61)
	if _, err := h.sendCommandComplete(opSetEventMask, mask, 2*time.Second); err != nil {
		return fmt.Errorf("hcible: Set Event Mask: %w", err)
	}

	// LE Event Mask: enable everything relevant (Connection Complete,
	// Advertising Report, Connection Update Complete, ...). Setting bits
	// for features the controller doesn't support is harmless.
	leMask := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := h.sendCommandComplete(opLESetEventMask, leMask, 2*time.Second); err != nil {
		return fmt.Errorf("hcible: LE Set Event Mask: %w", err)
	}

	return nil
}

// ReadBDAddr returns this controller's own public Bluetooth address, in
// wire byte order (see formatAddr/ParseAddr).
func (h *HCI) ReadBDAddr() ([6]byte, error) {
	var addr [6]byte
	ret, err := h.sendCommandComplete(opReadBDAddr, nil, 2*time.Second)
	if err != nil {
		return addr, fmt.Errorf("hcible: Read BD_ADDR: %w", err)
	}
	if len(ret) < 6 {
		return addr, fmt.Errorf("hcible: malformed Read BD_ADDR response")
	}
	copy(addr[:], ret[:6])
	return addr, nil
}

func setBit(mask []byte, bit int) {
	mask[bit/8] |= 1 << (bit % 8)
}

// --- low-level command send/receive -----------------------------------

func (h *HCI) writePacket(pktType byte, payload []byte) error {
	buf := make([]byte, 1+len(payload))
	buf[0] = pktType
	copy(buf[1:], payload)
	_, err := unix.Write(h.fd, buf)
	return err
}

// sendCommandComplete sends an HCI command and waits for its Command
// Complete event, returning the return-parameters with the leading status
// byte already checked and stripped (for the very small number of HCI
// commands with no status byte at all, callers must not use this helper).
//
// Any event received while waiting that isn't a Command Complete for this
// exact opcode is discarded and waiting continues: controllers can (and,
// observed in practice on real hardware, do) deliver a stale/duplicate
// Command Complete for an earlier command after this one was already sent;
// without this loop that stale event gets mistaken for the current
// command's answer, permanently shifting every subsequent command/response
// pairing by one.
func (h *HCI) sendCommandComplete(opcode uint16, params []byte, timeout time.Duration) ([]byte, error) {
	h.cmdMu.Lock()
	defer h.cmdMu.Unlock()

	drain(h.cmdResp) // discard anything stale left over from before this call
	if err := h.sendRawCommand(opcode, params); err != nil {
		return nil, err
	}
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-h.cmdResp:
			if ev.code != evtCommandComplete || len(ev.params) < 4 {
				continue
			}
			if binary.LittleEndian.Uint16(ev.params[1:3]) != opcode {
				continue // stale response for a different command
			}
			withStatus := ev.params[3:]
			if len(withStatus) < 1 {
				return nil, fmt.Errorf("hcible: command 0x%04x: Command Complete has no status byte", opcode)
			}
			status := withStatus[0]
			ret := withStatus[1:]
			if status != 0x00 {
				return ret, fmt.Errorf("hcible: command 0x%04x failed: status 0x%02x", opcode, status)
			}
			return ret, nil
		case <-deadline:
			return nil, fmt.Errorf("hcible: timeout waiting for Command Complete of opcode 0x%04x", opcode)
		}
	}
}

// sendCommandStatus sends an HCI command and waits for its Command Status
// event (used for commands whose real completion is signalled later by a
// separate event, e.g. LE Create Connection -> LE Connection Complete).
// See sendCommandComplete's doc comment for why mismatched events are
// discarded rather than treated as an error.
func (h *HCI) sendCommandStatus(opcode uint16, params []byte, timeout time.Duration) error {
	h.cmdMu.Lock()
	defer h.cmdMu.Unlock()

	drain(h.cmdResp)
	if err := h.sendRawCommand(opcode, params); err != nil {
		return err
	}
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-h.cmdResp:
			if ev.code != evtCommandStatus || len(ev.params) < 4 {
				continue
			}
			if binary.LittleEndian.Uint16(ev.params[2:4]) != opcode {
				continue // stale response for a different command
			}
			if status := ev.params[0]; status != 0x00 {
				return fmt.Errorf("hcible: command 0x%04x failed: status 0x%02x", opcode, status)
			}
			return nil
		case <-deadline:
			return fmt.Errorf("hcible: timeout waiting for Command Status of opcode 0x%04x", opcode)
		}
	}
}

func (h *HCI) sendRawCommand(opcode uint16, params []byte) error {
	buf := make([]byte, 3+len(params))
	binary.LittleEndian.PutUint16(buf[0:2], opcode)
	buf[2] = byte(len(params))
	copy(buf[3:], params)
	return h.writePacket(pktCommand, buf)
}

// sendACL writes one L2CAP PDU as (possibly fragmented) HCI ACL Data
// packets. aclMTU bounds each fragment's payload size; 27 is the LE
// mandatory minimum and a safe default absent an explicit negotiated value.
func (h *HCI) sendACL(connHandle uint16, l2capPDU []byte, aclMTU int) error {
	if aclMTU <= 0 {
		aclMTU = 27
	}
	first := true
	for len(l2capPDU) > 0 {
		n := len(l2capPDU)
		if n > aclMTU {
			n = aclMTU
		}
		chunk := l2capPDU[:n]
		l2capPDU = l2capPDU[n:]

		var pb uint16 = 0x01 // continuing fragment
		if first {
			pb = 0x00 // first (non-automatically-flushable, standard for LE)
			first = false
		}
		handleFlags := (connHandle & 0x0FFF) | (pb << 12)

		buf := make([]byte, 4+len(chunk))
		binary.LittleEndian.PutUint16(buf[0:2], handleFlags)
		binary.LittleEndian.PutUint16(buf[2:4], uint16(len(chunk)))
		copy(buf[4:], chunk)
		if err := h.writePacket(pktACLData, buf); err != nil {
			return err
		}
	}
	return nil
}

// --- read loop -----------------------------------------------------------

func (h *HCI) readLoop() {
	buf := make([]byte, 4096)
	// L2CAP reassembly state (single connection at a time).
	var reassembly []byte
	var reassemblyWant int

	for {
		n, err := unix.Read(h.fd, buf)
		if err != nil {
			select {
			case <-h.closed:
			default:
			}
			return
		}
		if n < 1 {
			continue
		}
		pktType := buf[0]
		data := append([]byte(nil), buf[1:n]...)

		switch pktType {
		case pktEvent:
			h.handleEvent(data)
		case pktACLData:
			h.handleACL(data, &reassembly, &reassemblyWant)
		}
	}
}

func (h *HCI) handleEvent(data []byte) {
	if len(data) < 2 {
		return
	}
	code := data[0]
	plen := int(data[1])
	if len(data) < 2+plen {
		return
	}
	params := data[2 : 2+plen]

	switch code {
	case evtCommandComplete, evtCommandStatus:
		select {
		case h.cmdResp <- event{code: code, params: params}:
		default:
		}
	case evtDisconnectionComplete:
		if len(params) >= 4 {
			handle := binary.LittleEndian.Uint16(params[1:3])
			reason := params[3]
			h.mu.Lock()
			cb := h.disconnHandler
			h.mu.Unlock()
			if cb != nil {
				cb(handle, reason)
			}
		}
	case evtEncryptionChange:
		if len(params) >= 4 {
			status := params[0]
			handle := binary.LittleEndian.Uint16(params[1:3])
			enabled := params[3] != 0
			h.mu.Lock()
			cb := h.encHandler
			h.mu.Unlock()
			if cb != nil {
				cb(handle, status, enabled)
			}
		}
	case evtLEMeta:
		h.handleLEMeta(params)
	}
}

func (h *HCI) handleACL(data []byte, reassembly *[]byte, want *int) {
	if len(data) < 4 {
		return
	}
	handleFlags := binary.LittleEndian.Uint16(data[0:2])
	pb := (handleFlags >> 12) & 0x3
	dataLen := binary.LittleEndian.Uint16(data[2:4])
	if len(data) < 4+int(dataLen) {
		return
	}
	payload := data[4 : 4+int(dataLen)]

	if pb == 0x01 { // continuing fragment
		*reassembly = append(*reassembly, payload...)
	} else { // first fragment (also the common "whole PDU in one fragment" case)
		if len(payload) < 4 {
			return
		}
		l2capLen := int(binary.LittleEndian.Uint16(payload[0:2]))
		*reassembly = append([]byte(nil), payload...)
		*want = 4 + l2capLen
	}

	if *want > 0 && len(*reassembly) >= *want {
		pdu := (*reassembly)[:*want]
		*reassembly = nil
		*want = 0
		if h.aclHandler != nil {
			h.aclHandler(pdu)
		}
	}
}
