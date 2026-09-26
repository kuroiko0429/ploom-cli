package hcible

import "encoding/binary"

// LE fixed L2CAP channel IDs used by this package (Core Spec Vol 3 Part A
// 2.1, Table 2.1 - LE-U logical link fixed channels).
const (
	l2capCIDATT = 0x0004
	l2capCIDSMP = 0x0006
)

// l2capEncode wraps payload in an L2CAP Basic frame header (length +
// channel ID) ready to hand to sendACL for fragmentation.
func l2capEncode(cid uint16, payload []byte) []byte {
	buf := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint16(buf[0:2], uint16(len(payload)))
	binary.LittleEndian.PutUint16(buf[2:4], cid)
	copy(buf[4:], payload)
	return buf
}

// l2capDecode splits a reassembled L2CAP PDU (as delivered by the ACL
// reassembly logic in hci.go) into its channel ID and payload.
func l2capDecode(pdu []byte) (cid uint16, payload []byte, ok bool) {
	if len(pdu) < 4 {
		return 0, nil, false
	}
	l2capLen := binary.LittleEndian.Uint16(pdu[0:2])
	cid = binary.LittleEndian.Uint16(pdu[2:4])
	body := pdu[4:]
	if int(l2capLen) > len(body) {
		return 0, nil, false
	}
	return cid, body[:l2capLen], true
}
