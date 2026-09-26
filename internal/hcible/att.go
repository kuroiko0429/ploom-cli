package hcible

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// ATT opcodes used by this client (Core Spec Vol 3 Part F 3.4).
const (
	attOpErrorResponse           = 0x01
	attOpExchangeMTURequest      = 0x02
	attOpExchangeMTUResponse     = 0x03
	attOpFindInformationRequest  = 0x04
	attOpFindInformationResponse = 0x05
	attOpReadByTypeRequest       = 0x08
	attOpReadByTypeResponse      = 0x09
	attOpReadRequest             = 0x0A
	attOpReadResponse            = 0x0B
	attOpReadByGroupTypeRequest  = 0x10
	attOpReadByGroupTypeResponse = 0x11
	attOpWriteRequest            = 0x12
	attOpWriteResponse           = 0x13
	attOpWriteCommand            = 0x52
	attOpHandleValueNotification = 0x1B
	attOpHandleValueIndication   = 0x1D
	attOpHandleValueConfirmation = 0x1E
)

// GATT declaration UUIDs used for discovery.
const (
	uuidPrimaryService   = "2800"
	uuidCharacteristic   = "2803"
	uuidClientCharConfig = "2902"
)

const attErrAttributeNotFound = 0x0A

type attError struct {
	reqOpcode byte
	handle    uint16
	code      byte
}

func (e *attError) Error() string {
	return fmt.Sprintf("hcible: ATT error 0x%02x on opcode 0x%02x handle 0x%04x", e.code, e.reqOpcode, e.handle)
}

func isAttrNotFound(err error) bool {
	ae, ok := err.(*attError)
	return ok && ae.code == attErrAttributeNotFound
}

type attService struct {
	uuid        string
	startHandle uint16
	endHandle   uint16
}

type attCharacteristic struct {
	uuid        string
	handle      uint16 // characteristic declaration handle
	valueHandle uint16
	properties  byte
	cccdHandle  uint16 // 0 if no Client Characteristic Configuration descriptor found
}

// attClient implements the ATT protocol (client role only) over a
// Connection's fixed ATT channel.
type attClient struct {
	conn *Connection

	reqMu   sync.Mutex // serializes request/response (ATT allows exactly one outstanding request)
	pending chan []byte
	mtu     int

	notifyMu  sync.Mutex
	notifyCbs map[uint16]func([]byte) // value handle -> callback
}

func newATTClient(c *Connection) *attClient {
	return &attClient{
		conn:      c,
		pending:   make(chan []byte, 1),
		mtu:       23, // ATT default MTU (Core Spec Vol 3 Part F 3.2.8) until Exchange MTU
		notifyCbs: make(map[uint16]func([]byte)),
	}
}

func (a *attClient) handlePDU(pdu []byte) {
	if len(pdu) < 1 {
		return
	}
	switch pdu[0] {
	case attOpHandleValueNotification:
		if len(pdu) < 3 {
			return
		}
		handle := binary.LittleEndian.Uint16(pdu[1:3])
		a.dispatchNotify(handle, append([]byte(nil), pdu[3:]...))
	case attOpHandleValueIndication:
		if len(pdu) < 3 {
			return
		}
		handle := binary.LittleEndian.Uint16(pdu[1:3])
		a.dispatchNotify(handle, append([]byte(nil), pdu[3:]...))
		// Indications require confirmation or the server will eventually
		// consider the link unresponsive.
		a.conn.send(l2capCIDATT, []byte{attOpHandleValueConfirmation})
	default:
		select {
		case a.pending <- append([]byte(nil), pdu...):
		default:
		}
	}
}

func (a *attClient) dispatchNotify(handle uint16, value []byte) {
	a.notifyMu.Lock()
	cb := a.notifyCbs[handle]
	a.notifyMu.Unlock()
	if cb != nil {
		cb(value)
	}
}

// request sends an ATT request PDU and waits for its response, translating
// an Error Response into a Go error.
func (a *attClient) request(pdu []byte, timeout time.Duration) ([]byte, error) {
	a.reqMu.Lock()
	defer a.reqMu.Unlock()

	drain(a.pending)
	if err := a.conn.send(l2capCIDATT, pdu); err != nil {
		return nil, err
	}
	select {
	case resp := <-a.pending:
		if len(resp) >= 1 && resp[0] == attOpErrorResponse {
			if len(resp) < 5 {
				return nil, fmt.Errorf("hcible: malformed ATT error response")
			}
			return nil, &attError{reqOpcode: resp[1], handle: binary.LittleEndian.Uint16(resp[2:4]), code: resp[4]}
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("hcible: ATT request timeout")
	}
}

func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// exchangeMTU negotiates the ATT MTU (Core Spec Vol 3 Part F 3.4.2).
func (a *attClient) exchangeMTU(clientMTU int) error {
	req := make([]byte, 3)
	req[0] = attOpExchangeMTURequest
	binary.LittleEndian.PutUint16(req[1:3], uint16(clientMTU))
	resp, err := a.request(req, 3*time.Second)
	if err != nil {
		return fmt.Errorf("hcible: Exchange MTU: %w", err)
	}
	if len(resp) < 3 || resp[0] != attOpExchangeMTUResponse {
		return fmt.Errorf("hcible: unexpected Exchange MTU response")
	}
	serverMTU := int(binary.LittleEndian.Uint16(resp[1:3]))
	mtu := min(clientMTU, serverMTU)
	a.mtu = max(mtu, 23)
	return nil
}

// discoverServices performs primary service discovery (Read By Group Type
// with UUID 0x2800, Core Spec Vol 3 Part G 4.4.1).
func (a *attClient) discoverServices() ([]attService, error) {
	var services []attService
	start := uint16(0x0001)
	for {
		req := make([]byte, 7)
		req[0] = attOpReadByGroupTypeRequest
		binary.LittleEndian.PutUint16(req[1:3], start)
		binary.LittleEndian.PutUint16(req[3:5], 0xFFFF)
		binary.LittleEndian.PutUint16(req[5:7], 0x2800)

		resp, err := a.request(req, 5*time.Second)
		if err != nil {
			if isAttrNotFound(err) {
				break
			}
			return nil, fmt.Errorf("hcible: discover services: %w", err)
		}
		if len(resp) < 2 || resp[0] != attOpReadByGroupTypeResponse {
			return nil, fmt.Errorf("hcible: unexpected Read By Group Type response")
		}
		elemLen := int(resp[1])
		if elemLen < 4 {
			return nil, fmt.Errorf("hcible: malformed Read By Group Type response")
		}
		body := resp[2:]
		var lastEnd uint16
		for len(body) >= elemLen {
			handle := binary.LittleEndian.Uint16(body[0:2])
			endHandle := binary.LittleEndian.Uint16(body[2:4])
			services = append(services, attService{
				uuid:        uuidFromBytes(body[4:elemLen]),
				startHandle: handle,
				endHandle:   endHandle,
			})
			lastEnd = endHandle
			body = body[elemLen:]
		}
		if lastEnd == 0xFFFF {
			break
		}
		start = lastEnd + 1
	}
	return services, nil
}

// discoverCharacteristics discovers every characteristic declaration
// (Read By Type with UUID 0x2803) within a service's handle range.
func (a *attClient) discoverCharacteristics(svc attService) ([]attCharacteristic, error) {
	var chars []attCharacteristic
	if svc.startHandle == 0xFFFF {
		return chars, nil
	}
	start := svc.startHandle + 1
	for start <= svc.endHandle {
		req := make([]byte, 7)
		req[0] = attOpReadByTypeRequest
		binary.LittleEndian.PutUint16(req[1:3], start)
		binary.LittleEndian.PutUint16(req[3:5], svc.endHandle)
		binary.LittleEndian.PutUint16(req[5:7], 0x2803)

		resp, err := a.request(req, 5*time.Second)
		if err != nil {
			if isAttrNotFound(err) {
				break
			}
			return nil, fmt.Errorf("hcible: discover characteristics: %w", err)
		}
		if len(resp) < 2 || resp[0] != attOpReadByTypeResponse {
			return nil, fmt.Errorf("hcible: unexpected Read By Type response")
		}
		elemLen := int(resp[1])
		if elemLen < 5 {
			return nil, fmt.Errorf("hcible: malformed Read By Type response")
		}
		body := resp[2:]
		var lastHandle uint16
		for len(body) >= elemLen {
			handle := binary.LittleEndian.Uint16(body[0:2])
			props := body[2]
			valueHandle := binary.LittleEndian.Uint16(body[3:5])
			chars = append(chars, attCharacteristic{
				uuid:        uuidFromBytes(body[5:elemLen]),
				handle:      handle,
				valueHandle: valueHandle,
				properties:  props,
			})
			lastHandle = handle
			body = body[elemLen:]
		}
		if lastHandle >= svc.endHandle {
			break
		}
		start = lastHandle + 1
	}
	return chars, nil
}

// findCCCD looks for a Client Characteristic Configuration descriptor
// (UUID 0x2902) in [startHandle, endHandle] via Find Information.
func (a *attClient) findCCCD(startHandle, endHandle uint16) (uint16, error) {
	if startHandle > endHandle {
		return 0, nil
	}
	req := make([]byte, 5)
	req[0] = attOpFindInformationRequest
	binary.LittleEndian.PutUint16(req[1:3], startHandle)
	binary.LittleEndian.PutUint16(req[3:5], endHandle)

	resp, err := a.request(req, 5*time.Second)
	if err != nil {
		if isAttrNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("hcible: find CCCD: %w", err)
	}
	if len(resp) < 2 || resp[0] != attOpFindInformationResponse {
		return 0, fmt.Errorf("hcible: unexpected Find Information response")
	}
	format := resp[1]
	elemLen := 4
	if format == 2 {
		elemLen = 18
	}
	body := resp[2:]
	for len(body) >= elemLen {
		handle := binary.LittleEndian.Uint16(body[0:2])
		uuid := uuidFromBytes(body[2:elemLen])
		if uuid == uuidClientCharConfig {
			return handle, nil
		}
		body = body[elemLen:]
	}
	return 0, nil
}

func (a *attClient) read(handle uint16) ([]byte, error) {
	req := make([]byte, 3)
	req[0] = attOpReadRequest
	binary.LittleEndian.PutUint16(req[1:3], handle)
	resp, err := a.request(req, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if len(resp) < 1 || resp[0] != attOpReadResponse {
		return nil, fmt.Errorf("hcible: unexpected Read response")
	}
	return resp[1:], nil
}

func (a *attClient) write(handle uint16, value []byte) error {
	req := make([]byte, 3+len(value))
	req[0] = attOpWriteRequest
	binary.LittleEndian.PutUint16(req[1:3], handle)
	copy(req[3:], value)
	resp, err := a.request(req, 5*time.Second)
	if err != nil {
		return err
	}
	if len(resp) < 1 || resp[0] != attOpWriteResponse {
		return fmt.Errorf("hcible: unexpected Write response")
	}
	return nil
}

func (a *attClient) writeCommand(handle uint16, value []byte) error {
	req := make([]byte, 3+len(value))
	req[0] = attOpWriteCommand
	binary.LittleEndian.PutUint16(req[1:3], handle)
	copy(req[3:], value)
	return a.conn.send(l2capCIDATT, req)
}

// setNotify enables (cb != nil) or disables (cb == nil) notifications on a
// characteristic via its CCCD. Prefers Notify over Indicate when both are
// supported (no confirmation round-trip needed).
func (a *attClient) setNotify(ch attCharacteristic, cb func([]byte)) error {
	if ch.cccdHandle == 0 {
		return fmt.Errorf("hcible: characteristic %s has no notify/indicate descriptor", ch.uuid)
	}

	a.notifyMu.Lock()
	if cb != nil {
		a.notifyCbs[ch.valueHandle] = cb
	} else {
		delete(a.notifyCbs, ch.valueHandle)
	}
	a.notifyMu.Unlock()

	var val uint16
	if cb != nil {
		switch {
		case ch.properties&0x10 != 0: // notify
			val = 0x0001
		case ch.properties&0x20 != 0: // indicate
			val = 0x0002
		default:
			return fmt.Errorf("hcible: characteristic %s does not support notify or indicate", ch.uuid)
		}
	}
	buf := make([]byte, 2)
	binary.LittleEndian.PutUint16(buf, val)
	return a.write(ch.cccdHandle, buf)
}

// uuidFromBytes formats a 2- or 16-byte little-endian UUID from an ATT PDU.
func uuidFromBytes(b []byte) string {
	if len(b) == 2 {
		return fmt.Sprintf("%04x", binary.LittleEndian.Uint16(b))
	}
	return uuid128String(b)
}
