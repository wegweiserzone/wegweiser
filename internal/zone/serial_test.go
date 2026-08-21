package zone_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

const (
	half = uint32(1) << 31
	maxU = math.MaxUint32
)

func TestSerialCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b uint32
		want int // sign of a.Compare(b)
	}{
		{"equal", 1, 1, 0},
		{"equal at zero", 0, 0, 0},
		{"adjacent", 1, 2, -1},
		{"adjacent reversed", 2, 1, 1},

		// The whole point: the space wraps, so the largest value is older than
		// zero. Comparing these as plain integers is how a secondary ends up
		// permanently refusing a transfer.
		{"wrap past the end", maxU, 0, -1},
		{"wrap past the end reversed", 0, maxU, 1},
		{"wrap by more than one", maxU - 5, 10, -1},

		// RFC 1982 §3.2: the ordering holds as long as the gap is under half
		// the space.
		{"just under half the space", 0, half - 1, -1},
		{"just under half the space reversed", half - 1, 0, 1},
		{"just over half the space", 0, half + 1, 1},

		{"near the top", maxU - 1, maxU, -1},
		{"zero and one", 0, 1, -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, b := zone.NewSerial(tc.a), zone.NewSerial(tc.b)
			if got := sign(a.Compare(b)); got != tc.want {
				t.Errorf("NewSerial(%d).Compare(%d) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			if got, want := a.After(b), tc.want > 0; got != want {
				t.Errorf("After = %v, want %v", got, want)
			}
			if got, want := a.Before(b), tc.want < 0; got != want {
				t.Errorf("Before = %v, want %v", got, want)
			}
		})
	}
}

// TestSerialAmbiguity covers the case RFC 1982 §3.2 admits it cannot decide:
// two serials exactly half the space apart, where neither is newer.
func TestSerialAmbiguity(t *testing.T) {
	t.Parallel()

	pairs := [][2]uint32{
		{0, half},
		{1, half + 1},
		{half, 0},
		{100, half + 100},
		{maxU, half - 1},
	}

	for _, p := range pairs {
		a, b := zone.NewSerial(p[0]), zone.NewSerial(p[1])

		if a.Comparable(b) {
			t.Errorf("%d and %d are half the space apart and should not be comparable", p[0], p[1])
		}
		if b.Comparable(a) {
			t.Errorf("comparability should be symmetric for %d and %d", p[0], p[1])
		}

		// Compare still has to answer, and antisymmetrically, or sorting a
		// zone's own history would depend on argument order.
		if sign(a.Compare(b)) != -sign(b.Compare(a)) {
			t.Errorf("Compare is not antisymmetric for %d and %d: %d vs %d",
				p[0], p[1], a.Compare(b), b.Compare(a))
		}
	}

	// Everything else is comparable.
	for _, p := range [][2]uint32{{0, 0}, {0, 1}, {0, half - 1}, {0, half + 1}, {maxU, 0}} {
		if !zone.NewSerial(p[0]).Comparable(zone.NewSerial(p[1])) {
			t.Errorf("%d and %d should be comparable", p[0], p[1])
		}
	}
}

// TestSerialCompareIsAntisymmetric checks the property across the interesting
// parts of the space, including the boundaries where the arithmetic turns over.
func TestSerialCompareIsAntisymmetric(t *testing.T) {
	t.Parallel()

	values := []uint32{0, 1, 2, 100, half - 2, half - 1, half, half + 1, half + 2, maxU - 1, maxU}

	for _, x := range values {
		for _, y := range values {
			a, b := zone.NewSerial(x), zone.NewSerial(y)
			ab, ba := sign(a.Compare(b)), sign(b.Compare(a))
			if ab != -ba {
				t.Errorf("Compare(%d, %d) = %d but Compare(%d, %d) = %d", x, y, ab, y, x, ba)
			}
			if (x == y) != (ab == 0) {
				t.Errorf("Compare(%d, %d) = %d; equality and zero must agree", x, y, ab)
			}
		}
	}
}

// TestSerialIsNotTransitive documents the property rather than guarding
// against it. Serial arithmetic genuinely admits a < b < c < a once the values
// span more than half the space, which is why an arbitrary list of serials
// cannot be sorted meaningfully.
func TestSerialIsNotTransitive(t *testing.T) {
	t.Parallel()

	a := zone.NewSerial(0)
	b := zone.NewSerial(half - 1)
	c := zone.NewSerial(maxU - 1)

	if !a.Before(b) || !b.Before(c) {
		t.Fatalf("expected a < b < c, got a<b=%v b<c=%v", a.Before(b), b.Before(c))
	}
	if !c.Before(a) {
		t.Error("expected c < a as well; if this ever holds transitively, the " +
			"documented caveat on Serial is wrong")
	}
}

func TestSerialNextAndAdd(t *testing.T) {
	t.Parallel()

	t.Run("next advances by one and is newer", func(t *testing.T) {
		t.Parallel()

		for _, v := range []uint32{0, 1, half, maxU - 1, maxU} {
			s := zone.NewSerial(v)
			n := s.Next()
			if n.Uint32() != v+1 {
				t.Errorf("NewSerial(%d).Next() = %d, want %d", v, n.Uint32(), v+1)
			}
			if !n.After(s) {
				t.Errorf("NewSerial(%d).Next() should be newer than its predecessor", v)
			}
		}
	})

	t.Run("add accepts the whole defined range", func(t *testing.T) {
		t.Parallel()

		s := zone.NewSerial(10)
		for _, n := range []uint32{0, 1, 1000, zone.MaxSerialIncrement} {
			got, err := s.Add(n)
			if err != nil {
				t.Fatalf("Add(%d): %v", n, err)
			}
			if got.Uint32() != 10+n {
				t.Errorf("Add(%d) = %d, want %d", n, got.Uint32(), 10+n)
			}
		}
	})

	t.Run("add refuses a step RFC 1982 does not define", func(t *testing.T) {
		t.Parallel()

		if _, err := zone.NewSerial(0).Add(zone.MaxSerialIncrement + 1); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("error = %v, want ErrInvalid", err)
		}
	})

	t.Run("wrapping past the top stays newer", func(t *testing.T) {
		t.Parallel()

		// The step a migrated zone might take right at the end of the space.
		s := zone.NewSerial(maxU)
		n := s.Next()
		if n.Uint32() != 0 {
			t.Fatalf("Next() = %d, want 0", n.Uint32())
		}
		if !n.After(s) {
			t.Error("0 must be newer than 4294967295")
		}
	})
}

func TestSerialJSON(t *testing.T) {
	t.Parallel()

	type payload struct {
		Serial zone.Serial `json:"serial"`
	}

	b, err := json.Marshal(payload{Serial: zone.NewSerial(2026081601)})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"serial":2026081601}`; string(b) != want {
		t.Errorf("Marshal = %s, want %s", b, want)
	}

	var out payload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Serial.Uint32() != 2026081601 {
		t.Errorf("round trip = %d", out.Serial.Uint32())
	}

	for _, bad := range []string{`{"serial":-1}`, `{"serial":4294967296}`, `{"serial":"x"}`} {
		if err := json.Unmarshal([]byte(bad), &out); !errors.Is(err, zone.ErrInvalid) {
			t.Errorf("Unmarshal(%s) error = %v, want ErrInvalid", bad, err)
		}
	}
}
