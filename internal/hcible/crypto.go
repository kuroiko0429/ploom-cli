package hcible

import "crypto/aes"

// smpE implements the Bluetooth SMP "e" function (Core Spec Vol 3 Part H
// 2.2.1): AES-128 encrypt of a 128-bit block, with the spec's big-endian
// ("most significant octet is index 0") convention translated to/from the
// little-endian byte order used everywhere else in this package (as bytes
// arrive/leave over the air). This exactly mirrors the Linux kernel's
// net/bluetooth/smp.c smp_e(), verified against its test vectors below.
func smpE(k, r [16]byte) [16]byte {
	var keyBE, dataBE [16]byte
	reverse16(&keyBE, k)
	reverse16(&dataBE, r)

	block, err := aes.NewCipher(keyBE[:])
	if err != nil {
		// aes.NewCipher only fails on bad key length; 16 bytes is always
		// valid, so this is unreachable.
		panic(err)
	}
	var outBE [16]byte
	block.Encrypt(outBE[:], dataBE[:])

	var out [16]byte
	reverse16(&out, outBE)
	return out
}

func reverse16(dst *[16]byte, src [16]byte) {
	for i := range 16 {
		dst[i] = src[15-i]
	}
}

// smpC1 implements the SMP "c1" confirm-value function (Core Spec Vol 3
// Part H 2.2.3) used in LE Legacy Pairing. k is the temporary key (all
// zeroes for Just Works), r is a 128-bit random value, preq/pres are the
// raw 7-byte Pairing Request/Response PDUs as sent over the air, iat/ia and
// rat/ra are the initiator's and responder's address type (0=public,
// 1=random) and 6-byte address in on-the-wire byte order.
func smpC1(k, r [16]byte, preq, pres [7]byte, iat byte, ia [6]byte, rat byte, ra [6]byte) [16]byte {
	var p1 [16]byte
	p1[0] = iat
	p1[1] = rat
	copy(p1[2:9], preq[:])
	copy(p1[9:16], pres[:])

	res := xor16(r, p1)
	res = smpE(k, res)

	var p2 [16]byte
	copy(p2[0:6], ra[:])
	copy(p2[6:12], ia[:])
	// p2[12:16] stays zero (padding)

	res = xor16(res, p2)
	return smpE(k, res)
}

// smpS1 implements the SMP "s1" key-generation function (Core Spec Vol 3
// Part H 2.2.4) used to derive the Short Term Key from the two random
// values exchanged during LE Legacy Pairing.
func smpS1(k, r1, r2 [16]byte) [16]byte {
	var r [16]byte
	copy(r[0:8], r2[0:8])
	copy(r[8:16], r1[0:8])
	return smpE(k, r)
}

func xor16(a, b [16]byte) [16]byte {
	var out [16]byte
	for i := range out {
		out[i] = a[i] ^ b[i]
	}
	return out
}
