package hcible

import (
	"reflect"
	"testing"
)

// Real advertising data captured (via sudo btmon) from an actual Ploom aura
// 802029KL unit: primary advertisement (flags + 128-bit service UUID +
// manufacturer data) and its scan response (complete local name).
func TestParseAD_PloomAdvertisement(t *testing.T) {
	adv := []byte{
		0x02, 0x01, 0x06,
		0x11, 0x07, 0x28, 0xca, 0x4a, 0x08, 0x58, 0xbc, 0xfa, 0x83, 0x65, 0x4a, 0x91,
		0xa3, 0x10, 0x40, 0x65, 0x53,
		0x06, 0xff, 0x00, 0x00, 0x00, 0xc3, 0x09,
	}
	name, uuids := parseAD(adv)
	if name != "" {
		t.Fatalf("name = %q, want empty (this payload has no name field)", name)
	}
	want := []string{"53654010-a391-4a65-83fa-bc58084aca28"}
	if !reflect.DeepEqual(uuids, want) {
		t.Fatalf("uuids = %v, want %v", uuids, want)
	}
}

func TestParseAD_PloomScanResponse(t *testing.T) {
	scanRsp := []byte{
		0x14, 0x09, 0x50, 0x6c, 0x6f, 0x6f, 0x6d, 0x20, 0x61, 0x75, 0x72, 0x61,
		0x20, 0x38, 0x30, 0x32, 0x30, 0x32, 0x39, 0x4b, 0x4c,
	}
	name, _ := parseAD(scanRsp)
	if name != "Ploom aura 802029KL" {
		t.Fatalf("name = %q, want %q", name, "Ploom aura 802029KL")
	}
}

func TestParseAdvertisingReports_SingleReport(t *testing.T) {
	// num_reports=1, event_type=0x00, addr_type=0x00 (public),
	// addr=7c:b8:da:94:37:dd (wire order, LSB first), length=3,
	// data={0x02,0x01,0x06}, rssi=-60 (0xC4).
	body := []byte{
		0x01,
		0x00,
		0x00,
		0xdd, 0x37, 0x94, 0xda, 0xb8, 0x7c,
		0x03,
		0x02, 0x01, 0x06,
		0xc4,
	}
	reports := parseAdvertisingReports(body)
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}
	r := reports[0]
	if r.addrType != 0x00 {
		t.Errorf("addrType = %d, want 0", r.addrType)
	}
	wantAddr := [6]byte{0xdd, 0x37, 0x94, 0xda, 0xb8, 0x7c}
	if r.addr != wantAddr {
		t.Errorf("addr = %x, want %x", r.addr, wantAddr)
	}
	if r.rssi != -60 {
		t.Errorf("rssi = %d, want -60", r.rssi)
	}
	if !reflect.DeepEqual(r.data, []byte{0x02, 0x01, 0x06}) {
		t.Errorf("data = %x, want 020106", r.data)
	}
	if got := formatAddr(r.addr); got != "7C:B8:DA:94:37:DD" {
		t.Errorf("formatAddr = %s, want 7C:B8:DA:94:37:DD", got)
	}
}

func TestParseAddrRoundTrip(t *testing.T) {
	addr, err := ParseAddr("7C:B8:DA:94:37:DD")
	if err != nil {
		t.Fatal(err)
	}
	if got := formatAddr(addr); got != "7C:B8:DA:94:37:DD" {
		t.Fatalf("round trip = %s, want 7C:B8:DA:94:37:DD", got)
	}
}
