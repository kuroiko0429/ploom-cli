package hcible

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// mustWireUUID converts a canonical hyphenated UUID string into its 16-byte
// little-endian wire form (the inverse of uuid128String), for constructing
// synthetic ATT PDUs in tests.
func mustWireUUID(t *testing.T, s string) []byte {
	t.Helper()
	be, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(be) != 16 {
		t.Fatalf("bad uuid %q", s)
	}
	wire := make([]byte, 16)
	for i := range 16 {
		wire[i] = be[15-i]
	}
	return wire
}

// newTestConnection wires up a real Connection against one end of a Unix
// socketpair, so tests can drive the actual L2CAP encode/decode and ATT
// request/response plumbing (via the real HCI.readLoop) without any
// hardware, by acting as the "peer" on the other file descriptor.
func newTestConnection(t *testing.T) (conn *Connection, sendFromPeer func(l2capPDU []byte), readFromClient func() []byte, sendEvent func(code byte, params []byte)) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}

	h := &HCI{fd: fds[0], cmdResp: make(chan event, 1), closed: make(chan struct{})}
	go h.readLoop()
	t.Cleanup(func() {
		unix.Close(fds[1]) // peer gone: h.Close()'s reset attempt fails fast instead of timing out
		h.Close()
	})

	conn = newConnection(h, 1, [6]byte{0xdd, 0x37, 0x94, 0xda, 0xb8, 0x7c}, 0)

	sendFromPeer = func(l2capPDU []byte) {
		acl := make([]byte, 4+len(l2capPDU))
		binary.LittleEndian.PutUint16(acl[0:2], 1) // conn handle=1, PB=first
		binary.LittleEndian.PutUint16(acl[2:4], uint16(len(l2capPDU)))
		copy(acl[4:], l2capPDU)
		full := append([]byte{pktACLData}, acl...)
		if _, err := unix.Write(fds[1], full); err != nil {
			t.Fatalf("peer write: %v", err)
		}
	}
	readFromClient = func() []byte {
		buf := make([]byte, 512)
		n, err := unix.Read(fds[1], buf)
		if err != nil {
			t.Fatalf("peer read: %v", err)
		}
		return buf[:n]
	}
	sendEvent = func(code byte, params []byte) {
		full := append([]byte{pktEvent, code, byte(len(params))}, params...)
		if _, err := unix.Write(fds[1], full); err != nil {
			t.Fatalf("peer write event: %v", err)
		}
	}
	return conn, sendFromPeer, readFromClient, sendEvent
}

// TestDiscoverAll_RealPloomGATTLayout replays the exact service/
// characteristic/CCCD handle layout observed (via the M5central NimBLE
// bridge) on a real Ploom aura unit's vendor-specific service, to verify
// the ATT discovery pipeline end to end.
func TestDiscoverAll_RealPloomGATTLayout(t *testing.T) {
	conn, sendFromPeer, readFromClient, _ := newTestConnection(t)

	svcUUID := "53654010-a391-4a65-83fa-bc58084aca28"
	char1UUID := "53654011-a391-4a65-83fa-bc58084aca28" // write
	char2UUID := "53654012-a391-4a65-83fa-bc58084aca28" // notify

	// --- discoverServices: one Read By Group Type Response, then
	// Attribute Not Found to terminate the scan.
	go func() {
		readFromClient()
		resp := []byte{attOpReadByGroupTypeResponse, 20}
		elem := make([]byte, 20)
		binary.LittleEndian.PutUint16(elem[0:2], 0x0016)
		binary.LittleEndian.PutUint16(elem[2:4], 0x001b)
		copy(elem[4:20], mustWireUUID(t, svcUUID))
		sendFromPeer(l2capEncode(l2capCIDATT, append(resp, elem...)))

		readFromClient()
		sendFromPeer(l2capEncode(l2capCIDATT, []byte{attOpErrorResponse, attOpReadByGroupTypeRequest, 0x1c, 0x00, attErrAttributeNotFound}))
	}()

	services, err := conn.att.discoverServices()
	if err != nil {
		t.Fatalf("discoverServices: %v", err)
	}
	if len(services) != 1 || services[0].uuid != svcUUID || services[0].startHandle != 0x16 || services[0].endHandle != 0x1b {
		t.Fatalf("services = %+v", services)
	}

	// --- discoverCharacteristics: both characteristics in one response,
	// then Attribute Not Found.
	go func() {
		readFromClient()
		resp := []byte{attOpReadByTypeResponse, 21}
		e1 := make([]byte, 21)
		binary.LittleEndian.PutUint16(e1[0:2], 0x0017)
		e1[2] = 0x08 // write
		binary.LittleEndian.PutUint16(e1[3:5], 0x0018)
		copy(e1[5:21], mustWireUUID(t, char1UUID))
		e2 := make([]byte, 21)
		binary.LittleEndian.PutUint16(e2[0:2], 0x0019)
		e2[2] = 0x20 // notify
		binary.LittleEndian.PutUint16(e2[3:5], 0x001a)
		copy(e2[5:21], mustWireUUID(t, char2UUID))
		sendFromPeer(l2capEncode(l2capCIDATT, append(append(resp, e1...), e2...)))

		readFromClient()
		sendFromPeer(l2capEncode(l2capCIDATT, []byte{attOpErrorResponse, attOpReadByTypeRequest, 0x1a, 0x00, attErrAttributeNotFound}))
	}()

	chars, err := conn.att.discoverCharacteristics(services[0])
	if err != nil {
		t.Fatalf("discoverCharacteristics: %v", err)
	}
	if len(chars) != 2 {
		t.Fatalf("got %d characteristics, want 2", len(chars))
	}
	if chars[0].uuid != char1UUID || chars[0].valueHandle != 0x18 || chars[0].properties != 0x08 {
		t.Fatalf("char[0] = %+v", chars[0])
	}
	if chars[1].uuid != char2UUID || chars[1].valueHandle != 0x1a || chars[1].properties != 0x20 {
		t.Fatalf("char[1] = %+v", chars[1])
	}

	// --- findCCCD for the notify characteristic.
	go func() {
		readFromClient()
		resp := []byte{attOpFindInformationResponse, 1}
		e := make([]byte, 4)
		binary.LittleEndian.PutUint16(e[0:2], 0x001b)
		binary.LittleEndian.PutUint16(e[2:4], 0x2902)
		sendFromPeer(l2capEncode(l2capCIDATT, append(resp, e...)))
	}()

	cccd, err := conn.att.findCCCD(chars[1].valueHandle+1, services[0].endHandle)
	if err != nil {
		t.Fatalf("findCCCD: %v", err)
	}
	if cccd != 0x001b {
		t.Fatalf("cccd = 0x%04x, want 0x001b", cccd)
	}

	// --- read: value "F400" as seen on the real device for one of these
	// characteristics.
	go func() {
		readFromClient()
		sendFromPeer(l2capEncode(l2capCIDATT, []byte{attOpReadResponse, 0xf4, 0x00}))
	}()
	val, err := conn.att.read(chars[0].valueHandle)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if hex.EncodeToString(val) != "f400" {
		t.Fatalf("read value = %x, want f400", val)
	}

	// --- write with response.
	go func() {
		req := readFromClient()
		if len(req) < 5 {
			t.Errorf("write request too short: %x", req)
		}
		sendFromPeer(l2capEncode(l2capCIDATT, []byte{attOpWriteResponse}))
	}()
	if err := conn.att.write(chars[0].valueHandle, []byte{0x01}); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// extractL2CAPPayload pulls the L2CAP payload (opcode + data, for SMP/ATT
// PDUs) out of a raw ACL data packet as written by the client under test.
func extractL2CAPPayload(t *testing.T, raw []byte) []byte {
	t.Helper()
	if len(raw) < 9 || raw[0] != pktACLData {
		t.Fatalf("not an ACL data packet: % x", raw)
	}
	l2capLen := int(binary.LittleEndian.Uint16(raw[5:7]))
	if len(raw) < 9+l2capLen {
		t.Fatalf("truncated ACL packet: % x", raw)
	}
	return raw[9 : 9+l2capLen]
}

// extractCommand pulls the opcode and parameters out of a raw HCI command
// packet as written by the client under test.
func extractCommand(t *testing.T, raw []byte) (opcode uint16, params []byte) {
	t.Helper()
	if len(raw) < 4 || raw[0] != pktCommand {
		t.Fatalf("not a command packet: % x", raw)
	}
	opcode = binary.LittleEndian.Uint16(raw[1:3])
	plen := int(raw[3])
	if len(raw) < 4+plen {
		t.Fatalf("truncated command packet: % x", raw)
	}
	return opcode, raw[4 : 4+plen]
}

// TestSMPPairing_JustWorks drives the full LE Legacy "Just Works" pairing
// state machine (pair()) against a simulated peer that independently
// computes its own confirm/random values with the same (kernel-verified)
// smpC1/smpS1 functions, checking that the client: proactively sends
// Pairing Request without waiting for a Security Request (the entire point
// of this package, see smp.go), correctly verifies the peer's confirm
// value, and derives the STK the responder's own math would predict before
// issuing LE Start Encryption.
func TestSMPPairing_JustWorks(t *testing.T) {
	conn, sendFromPeer, readFromClient, sendEvent := newTestConnection(t)

	ownAddr := [6]byte{0x66, 0x55, 0x44, 0x33, 0x22, 0x11} // fake controller BD_ADDR
	peerAddr := conn.peerAddr
	peerAddrType := conn.peerAddrType
	var tk [16]byte // Just Works
	var srand [16]byte
	for i := range srand {
		srand[i] = byte(0x30 + i)
	}

	peerErr := make(chan error, 1)
	go func() {
		// 1. Pair() reads our own BD_ADDR first.
		opcode, _ := extractCommand(t, readFromClient())
		if opcode != opReadBDAddr {
			peerErr <- fmt.Errorf("expected Read BD_ADDR, got opcode 0x%04x", opcode)
			return
		}
		opReadBDAddrU16 := uint16(opReadBDAddr)
		ccParams := append([]byte{1, byte(opReadBDAddrU16), byte(opReadBDAddrU16 >> 8), 0x00}, ownAddr[:]...)
		sendEvent(evtCommandComplete, ccParams)

		// 2. Client proactively sends Pairing Request (no Security Request
		// from us first - that's the point).
		preqPDU := extractL2CAPPayload(t, readFromClient())
		if len(preqPDU) != 7 || preqPDU[0] != smpOpPairingRequest {
			peerErr <- fmt.Errorf("expected 7-byte Pairing Request, got % x", preqPDU)
			return
		}
		var preq [7]byte
		copy(preq[:], preqPDU)

		// 3. Peer replies with Pairing Response.
		pres := [7]byte{smpOpPairingResponse, ioCapNoInputNoOutput, 0x00, 0x01, 16, 0x00, 0x00}
		sendFromPeer(l2capEncode(l2capCIDSMP, pres[:]))

		// 4. Client sends its Pairing Confirm (Mconfirm) - value itself
		// isn't independently re-derivable by us without its Mrand, which
		// arrives later; just check shape.
		mconfirmPDU := extractL2CAPPayload(t, readFromClient())
		if len(mconfirmPDU) != 17 || mconfirmPDU[0] != smpOpPairingConfirm {
			peerErr <- fmt.Errorf("expected 17-byte Pairing Confirm, got % x", mconfirmPDU)
			return
		}

		// 5. Peer sends its own Pairing Confirm.
		sconfirm := smpC1(tk, srand, preq, pres, 0x00, ownAddr, peerAddrType, peerAddr)
		sendFromPeer(l2capEncode(l2capCIDSMP, append([]byte{smpOpPairingConfirm}, sconfirm[:]...)))

		// 6. Client reveals Mrand.
		mrandPDU := extractL2CAPPayload(t, readFromClient())
		if len(mrandPDU) != 17 || mrandPDU[0] != smpOpPairingRandom {
			peerErr <- fmt.Errorf("expected 17-byte Pairing Random, got % x", mrandPDU)
			return
		}
		var mrand [16]byte
		copy(mrand[:], mrandPDU[1:])

		// Sanity: the client's own confirm must actually match its
		// revealed random (this is exactly what the client will separately
		// check of *our* confirm/random - verifying the client holds
		// itself to the same rule catches sequencing bugs).
		wantMconfirm := smpC1(tk, mrand, preq, pres, 0x00, ownAddr, peerAddrType, peerAddr)
		if !bytes.Equal(mconfirmPDU[1:], wantMconfirm[:]) {
			peerErr <- fmt.Errorf("client's Mconfirm doesn't match its own revealed Mrand")
			return
		}

		// 7. Peer reveals its random.
		sendFromPeer(l2capEncode(l2capCIDSMP, append([]byte{smpOpPairingRandom}, srand[:]...)))

		// 8. Client issues LE Start Encryption with the STK derived from
		// (TK, Srand, Mrand) - verify it matches what the responder side
		// of smp_random() in the kernel reference would derive.
		opcode, params := extractCommand(t, readFromClient())
		if opcode != opLEStartEncryption || len(params) != 2+8+2+16 {
			peerErr <- fmt.Errorf("expected 28-byte LE Start Encryption, got opcode 0x%04x len %d", opcode, len(params))
			return
		}
		var gotSTK [16]byte
		copy(gotSTK[:], params[12:28])
		wantSTK := smpS1(tk, srand, mrand)
		if gotSTK != wantSTK {
			peerErr <- fmt.Errorf("client STK = %x, want %x", gotSTK, wantSTK)
			return
		}

		opLEStartEncU16 := uint16(opLEStartEncryption)
		sendEvent(evtCommandStatus, []byte{0x00, 1, byte(opLEStartEncU16), byte(opLEStartEncU16 >> 8)})
		sendEvent(evtEncryptionChange, []byte{0x00, 0x01, 0x00, 0x01}) // status=0 handle=1 enabled=1
		peerErr <- nil
	}()

	if err := conn.Pair(3 * time.Second); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := <-peerErr; err != nil {
		t.Fatalf("peer: %v", err)
	}
}
