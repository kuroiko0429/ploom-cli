package hcible

import (
	crand "crypto/rand"
	"fmt"
	"time"
)

// SMP opcodes (Core Spec Vol 3 Part H 3.3).
const (
	smpOpPairingRequest  = 0x01
	smpOpPairingResponse = 0x02
	smpOpPairingConfirm  = 0x03
	smpOpPairingRandom   = 0x04
	smpOpPairingFailed   = 0x05
	smpOpSecurityRequest = 0x0B
)

const ioCapNoInputNoOutput = 0x03

// smpReasonConfirmValueFailed is the Pairing Failed reason code sent when a
// received confirm value doesn't match (Core Spec Vol 3 Part H 3.5.5).
const smpReasonConfirmValueFailed = 0x04

// smpClient implements LE Legacy Pairing, initiator role, "Just Works"
// association (IO capability NoInputNoOutput on both the request we send
// and, per the IO capability mapping table, regardless of what the peer
// requests - the only way to get MITM protection is OOB or a display/input
// capable device, neither of which apply here).
//
// It deliberately does not implement LE Secure Connections (no ECDH/P-256):
// setting our own AuthReq.SC=0 guarantees LE Legacy Pairing is used
// regardless of what the peer supports (Core Spec Vol 3 Part H 2.3.5.1).
// It also requests and offers zero keys for the bonding key-distribution
// phase (no LTK/IRK/CSRK persistence) - pairing completes as soon as STK
// encryption starts. This means every run re-pairs from scratch; a known,
// accepted limitation in exchange for a much smaller implementation.
type smpClient struct {
	conn *Connection

	ownAddr     [6]byte
	ownAddrType byte

	preq [7]byte // our Pairing Request, exactly as sent (needed by c1)
	pres [7]byte // peer's Pairing Response, exactly as received (needed by c1)

	mrand [16]byte

	responseCh chan [7]byte
	confirmCh  chan [16]byte
	randomCh   chan [16]byte
	failedCh   chan byte
	encDone    chan error
}

func newSMPClient(c *Connection) *smpClient {
	return &smpClient{
		conn:       c,
		responseCh: make(chan [7]byte, 1),
		confirmCh:  make(chan [16]byte, 1),
		randomCh:   make(chan [16]byte, 1),
		failedCh:   make(chan byte, 1),
		encDone:    make(chan error, 1),
	}
}

func (s *smpClient) handlePDU(pdu []byte) {
	if len(pdu) < 1 {
		return
	}
	switch pdu[0] {
	case smpOpPairingResponse:
		if len(pdu) < 7 {
			return
		}
		var r [7]byte
		copy(r[:], pdu[:7])
		trySend(s.responseCh, r)
	case smpOpPairingConfirm:
		if len(pdu) < 17 {
			return
		}
		var c [16]byte
		copy(c[:], pdu[1:17])
		trySend(s.confirmCh, c)
	case smpOpPairingRandom:
		if len(pdu) < 17 {
			return
		}
		var r [16]byte
		copy(r[:], pdu[1:17])
		trySend(s.randomCh, r)
	case smpOpPairingFailed:
		var reason byte
		if len(pdu) >= 2 {
			reason = pdu[1]
		}
		trySend(s.failedCh, reason)
	case smpOpSecurityRequest:
		// We always pair proactively (see pair()); nothing to do.
	default:
		// Key distribution PDUs (Encryption/Master/Identity/Signing
		// Information) are neither requested nor offered (both key
		// distribution fields are 0 in our Pairing Request), but some
		// peripherals send them unsolicited anyway. Ignore.
	}
}

func trySend[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func (s *smpClient) onEncryptionChange(handle uint16, status byte, enabled bool) {
	if handle != s.conn.handle {
		return
	}
	var err error
	switch {
	case status != 0x00:
		err = fmt.Errorf("hcible: encryption failed: status 0x%02x", status)
	case !enabled:
		err = fmt.Errorf("hcible: encryption not enabled")
	}
	trySend(s.encDone, err)
}

func (s *smpClient) sendFailed(reason byte) {
	s.conn.send(l2capCIDSMP, []byte{smpOpPairingFailed, reason})
}

// pair actively initiates pairing as soon as it's called - no other HCI
// command or L2CAP/ATT traffic needs to happen first. ownAddr is this
// adapter's own public Bluetooth address (see HCI.ReadBDAddr).
func (s *smpClient) pair(ownAddr [6]byte, timeout time.Duration) error {
	s.ownAddr = ownAddr
	s.ownAddrType = 0x00 // public

	// AuthReq = Bonding(bit0)=1, MITM=0, SC=0 (forces LE Legacy Pairing
	// regardless of peer capability), Keypress=0, CT2=0.
	s.preq = [7]byte{smpOpPairingRequest, ioCapNoInputNoOutput, 0x00, 0x01, 16, 0x00, 0x00}
	if err := s.conn.send(l2capCIDSMP, s.preq[:]); err != nil {
		return fmt.Errorf("hcible: send Pairing Request: %w", err)
	}

	deadline := time.Now().Add(timeout)
	remaining := func() time.Duration {
		d := time.Until(deadline)
		if d < 0 {
			d = 0
		}
		return d
	}

	select {
	case s.pres = <-s.responseCh:
	case reason := <-s.failedCh:
		return fmt.Errorf("hcible: pairing failed: reason 0x%02x", reason)
	case <-time.After(remaining()):
		return fmt.Errorf("hcible: timeout waiting for Pairing Response")
	}

	if _, err := crand.Read(s.mrand[:]); err != nil {
		return fmt.Errorf("hcible: generate random nonce: %w", err)
	}

	var tk [16]byte // Just Works: TK is all zeroes
	mconfirm := smpC1(tk, s.mrand, s.preq, s.pres, s.ownAddrType, s.ownAddr, s.conn.peerAddrType, s.conn.peerAddr)

	if err := s.conn.send(l2capCIDSMP, append([]byte{smpOpPairingConfirm}, mconfirm[:]...)); err != nil {
		return fmt.Errorf("hcible: send Pairing Confirm: %w", err)
	}

	var sconfirm [16]byte
	select {
	case sconfirm = <-s.confirmCh:
	case reason := <-s.failedCh:
		return fmt.Errorf("hcible: pairing failed: reason 0x%02x", reason)
	case <-time.After(remaining()):
		return fmt.Errorf("hcible: timeout waiting for peer's Pairing Confirm")
	}

	if err := s.conn.send(l2capCIDSMP, append([]byte{smpOpPairingRandom}, s.mrand[:]...)); err != nil {
		return fmt.Errorf("hcible: send Pairing Random: %w", err)
	}

	var srand [16]byte
	select {
	case srand = <-s.randomCh:
	case reason := <-s.failedCh:
		return fmt.Errorf("hcible: pairing failed: reason 0x%02x", reason)
	case <-time.After(remaining()):
		return fmt.Errorf("hcible: timeout waiting for peer's Pairing Random")
	}

	check := smpC1(tk, srand, s.preq, s.pres, s.ownAddrType, s.ownAddr, s.conn.peerAddrType, s.conn.peerAddr)
	if check != sconfirm {
		s.sendFailed(smpReasonConfirmValueFailed)
		return fmt.Errorf("hcible: pairing failed: confirm value mismatch")
	}

	// Initiator's STK = s1(TK, r1=responder's random, r2=initiator's
	// random) - see net/bluetooth/smp.c smp_random(), initiator branch.
	stk := smpS1(tk, srand, s.mrand)

	if err := s.conn.startEncryption(0, 0, stk); err != nil {
		return fmt.Errorf("hcible: LE Start Encryption: %w", err)
	}

	select {
	case err := <-s.encDone:
		return err
	case <-time.After(remaining()):
		return fmt.Errorf("hcible: timeout waiting for encryption to start")
	}
}
