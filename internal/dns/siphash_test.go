package dns

import "testing"

// TestSipHash24 checks the primitive against the reference vectors that ship
// with Aumasson and Bernstein's implementation: the key is 00 01 .. 0f and the
// message the first n octets of 00 01 02 ..., for n from zero upwards.
//
// The cookie vectors in cookie_test.go exercise this too, but through the
// construction on top of it. A failure here says which of the two is wrong.
func TestSipHash24(t *testing.T) {
	const (
		k0 = 0x0706050403020100
		k1 = 0x0f0e0d0c0b0a0908
	)

	want := []uint64{
		0x726fdb47dd0e0e31,
		0x74f839c593dc67fd,
		0x0d6c8009d9a94f5a,
		0x85676696d7fb7e2d,
		0xcf2794e0277187b7,
		0x18765564cd99a68d,
		0xcbc9466e58fee3ce,
		0xab0200f58b01d137,
	}

	msg := make([]byte, 0, len(want))
	for n, w := range want {
		if got := sipHash24(k0, k1, msg); got != w {
			t.Errorf("a message of %d octets: %016x, want %016x", n, got, w)
		}
		msg = append(msg, byte(n))
	}
}
