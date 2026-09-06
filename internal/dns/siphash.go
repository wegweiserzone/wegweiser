package dns

import (
	"encoding/binary"
	"math/bits"
)

// sipHash24 returns the SipHash-2-4 of msg under the 128-bit key k0, k1.
//
// RFC 9018 §6 makes this the one mandatory hash for a Server Cookie, so it is
// not a choice between primitives: an interoperable cookie is a SipHash-2-4
// cookie. The algorithm is Aumasson and Bernstein's, and the constants below
// are the ones their paper fixes.
//
// It lives here rather than behind a dependency because it is fifty lines
// pinned by published test vectors, and because the one caller is on the query
// path, where a package boundary buys nothing.
func sipHash24(k0, k1 uint64, msg []byte) uint64 {
	v0 := k0 ^ 0x736f6d6570736575
	v1 := k1 ^ 0x646f72616e646f6d
	v2 := k0 ^ 0x6c7967656e657261
	v3 := k1 ^ 0x7465646279746573

	// The length of the message goes into the top octet of the final block, so
	// that two messages differing only in trailing zero octets do not hash the
	// same.
	last := uint64(len(msg)) << 56

	for len(msg) >= 8 {
		m := binary.LittleEndian.Uint64(msg)
		msg = msg[8:]

		v3 ^= m
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
		v0 ^= m
	}

	for i, b := range msg {
		last |= uint64(b) << (8 * i)
	}

	v3 ^= last
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	v0 ^= last

	v2 ^= 0xff
	for range 4 {
		v0, v1, v2, v3 = sipRound(v0, v1, v2, v3)
	}
	return v0 ^ v1 ^ v2 ^ v3
}

// sipRound is one round of the SipHash permutation, on the four words of
// state it both takes and returns.
func sipRound(a, b, c, d uint64) (v0, v1, v2, v3 uint64) {
	v0, v1, v2, v3 = a, b, c, d

	v0 += v1
	v1 = bits.RotateLeft64(v1, 13)
	v1 ^= v0
	v0 = bits.RotateLeft64(v0, 32)

	v2 += v3
	v3 = bits.RotateLeft64(v3, 16)
	v3 ^= v2

	v0 += v3
	v3 = bits.RotateLeft64(v3, 21)
	v3 ^= v0

	v2 += v1
	v1 = bits.RotateLeft64(v1, 17)
	v1 ^= v2
	v2 = bits.RotateLeft64(v2, 32)

	return v0, v1, v2, v3
}
