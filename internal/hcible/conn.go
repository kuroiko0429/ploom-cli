package hcible

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// Connection is a single active LE connection: L2CAP fixed-channel
// dispatch on top of the raw HCI layer, plus the ATT and SMP clients built
// on that. This package supports exactly one Connection at a time per HCI.
type Connection struct {
	hci          *HCI
	handle       uint16
	peerAddr     [6]byte
	peerAddrType byte

	mu     sync.Mutex
	chans  map[uint16]func([]byte)
	aclMTU int

	disconnectOnce   sync.Once
	disconnected     chan struct{}
	disconnectReason byte

	att *attClient
	smp *smpClient
}

func newConnection(h *HCI, handle uint16, peerAddr [6]byte, peerAddrType byte) *Connection {
	c := &Connection{
		hci:          h,
		handle:       handle,
		peerAddr:     peerAddr,
		peerAddrType: peerAddrType,
		chans:        make(map[uint16]func([]byte)),
		aclMTU:       27, // LE mandatory minimum; refined once ATT MTU is exchanged
		disconnected: make(chan struct{}),
	}

	h.aclHandler = c.handleACL
	h.mu.Lock()
	h.disconnHandler = c.onDisconnect
	h.mu.Unlock()

	c.att = newATTClient(c)
	c.smp = newSMPClient(c)
	h.mu.Lock()
	h.encHandler = c.smp.onEncryptionChange
	h.mu.Unlock()

	c.registerChannel(l2capCIDATT, c.att.handlePDU)
	c.registerChannel(l2capCIDSMP, c.smp.handlePDU)
	return c
}

func (c *Connection) registerChannel(cid uint16, fn func([]byte)) {
	c.mu.Lock()
	c.chans[cid] = fn
	c.mu.Unlock()
}

func (c *Connection) handleACL(pdu []byte) {
	cid, payload, ok := l2capDecode(pdu)
	if !ok {
		return
	}
	c.mu.Lock()
	fn := c.chans[cid]
	c.mu.Unlock()
	if fn != nil {
		fn(payload)
	}
}

func (c *Connection) send(cid uint16, payload []byte) error {
	return c.hci.sendACL(c.handle, l2capEncode(cid, payload), c.aclMTU)
}

func (c *Connection) onDisconnect(handle uint16, reason byte) {
	if handle != c.handle {
		return
	}
	c.disconnectOnce.Do(func() {
		c.disconnectReason = reason
		close(c.disconnected)
	})
}

// Connected reports whether the link is still up.
func (c *Connection) Connected() (bool, error) {
	select {
	case <-c.disconnected:
		return false, nil
	default:
		return true, nil
	}
}

// Disconnect tears down the connection.
func (c *Connection) Disconnect() error {
	select {
	case <-c.disconnected:
		return nil
	default:
	}
	params := make([]byte, 3)
	binary.LittleEndian.PutUint16(params[0:2], c.handle)
	params[2] = 0x13 // Remote User Terminated Connection: standard host-initiated disconnect reason
	if err := c.hci.sendCommandStatus(opDisconnect, params, 3*time.Second); err != nil {
		return fmt.Errorf("hcible: disconnect: %w", err)
	}
	select {
	case <-c.disconnected:
	case <-time.After(3 * time.Second):
	}
	return nil
}

// MTU returns the negotiated ATT MTU for this connection.
func (c *Connection) MTU() (uint16, error) {
	if c.att.mtu == 0 {
		return 23, nil // ATT default MTU before any Exchange MTU
	}
	return uint16(c.att.mtu), nil
}

// StartEncryption issues LE Start Encryption using the given (rand, ediv,
// ltk) - for a freshly paired (not previously bonded) link via LE Legacy
// Pairing, rand=0, ediv=0, ltk=STK.
func (c *Connection) startEncryption(rand uint64, ediv uint16, ltk [16]byte) error {
	params := make([]byte, 2+8+2+16)
	binary.LittleEndian.PutUint16(params[0:2], c.handle)
	binary.LittleEndian.PutUint64(params[2:10], rand)
	binary.LittleEndian.PutUint16(params[10:12], ediv)
	copy(params[12:28], ltk[:])
	return c.hci.sendCommandStatus(opLEStartEncryption, params, 3*time.Second)
}
