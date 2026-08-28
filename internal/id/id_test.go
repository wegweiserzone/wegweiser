package id_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/id"
)

func TestNewIsCanonicalAndUnique(t *testing.T) {
	t.Parallel()

	const n = 10000
	seen := make(map[string]struct{}, n)

	for range n {
		got := id.New()
		if len(got) != id.Size {
			t.Fatalf("New() = %q, want %d characters", got, id.Size)
		}
		if !id.Valid(got) {
			t.Fatalf("New() = %q, which Valid rejects", got)
		}
		if _, dup := seen[got]; dup {
			t.Fatalf("New() returned %q twice", got)
		}
		seen[got] = struct{}{}
	}
}

// The point of a ULID over a UUIDv4 is that plain string order follows creation
// order, which is what keeps index inserts local. If that ever stops holding,
// the reason for the whole choice is gone.
func TestNewSortsByCreationTime(t *testing.T) {
	t.Parallel()

	first := id.New()
	// Two milliseconds, because the timestamp has millisecond resolution and
	// identifiers minted within one millisecond are ordered only by chance.
	time.Sleep(2 * time.Millisecond)
	second := id.New()

	if first >= second {
		t.Errorf("%q was minted before %q but does not sort before it", first, second)
	}
}

func TestValid(t *testing.T) {
	t.Parallel()

	sample := id.New()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"freshly minted", sample, true},
		{"all zeroes", strings.Repeat("0", id.Size), true},
		{"largest representable timestamp", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", true},

		{"empty", "", false},
		{"too short", sample[:id.Size-1], false},
		{"too long", sample + "0", false},
		// Crockford base32 omits I, L, O and U so that they cannot be confused
		// with 1, 1, 0 and V.
		{"letter I", strings.Repeat("0", id.Size-1) + "I", false},
		{"letter L", strings.Repeat("0", id.Size-1) + "L", false},
		{"letter O", strings.Repeat("0", id.Size-1) + "O", false},
		{"letter U", strings.Repeat("0", id.Size-1) + "U", false},
		{"punctuation", strings.Repeat("0", id.Size-1) + "-", false},
		// 26 base32 characters hold 130 bits, but a ULID is 128, so the first
		// character cannot exceed 7 without overflowing the timestamp.
		{"timestamp overflow", "8" + strings.Repeat("0", id.Size-1), false},
		// Decodable, but not the spelling the database stores.
		{"lowercase", strings.ToLower(sample), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := id.Valid(tc.in); got != tc.want {
				t.Errorf("Valid(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func BenchmarkNew(b *testing.B) {
	for b.Loop() {
		_ = id.New()
	}
}
