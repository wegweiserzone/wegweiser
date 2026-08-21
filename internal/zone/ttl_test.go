package zone_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestParseTTL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want zone.TTL
	}{
		{"plain seconds", "3600", 3600},
		{"zero", "0", 0},
		{"leading zeroes", "007", 7},
		{"surrounding space", "  300  ", 300},
		{"seconds suffix", "30s", 30},
		{"minutes", "5m", 300},
		{"hours", "1h", 3600},
		{"days", "1d", 86400},
		{"weeks", "1w", 604800},
		{"uppercase suffix", "1H", 3600},
		{"combined", "1h30m", 5400},
		{"combined out of order", "30m1h", 5400},
		{"full combination", "1w2d3h4m5s", 604800 + 2*86400 + 3*3600 + 4*60 + 5},
		{"maximum", "2147483647", zone.MaxTTL},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseTTL(tc.in)
			if err != nil {
				t.Fatalf("ParseTTL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseTTL(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseTTLInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"only space", "   "},
		{"not a number", "abc"},
		{"unknown unit", "5y"},
		{"unit without a number", "h"},
		{"unit without a number after a section", "1hm"},
		{"number without a unit at the end", "1h30"},
		{"repeated unit", "1h1h"},
		{"negative", "-1"},
		{"one above the maximum", "2147483648"},
		// RFC 2181 §8: the top bit must be zero, so anything a full uint32
		// could hold above 2^31-1 is refused rather than silently truncated.
		{"full uint32 maximum", "4294967295"},
		{"a single term overflows", "999999w"},
		// Each term is in range on its own; only the sum is not. 24855 days
		// is 2147472000 seconds, so five hours more crosses the limit.
		{"only the sum overflows", "24855d5h"},
		{"embedded space", "1h 30m"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseTTL(tc.in)
			if !errors.Is(err, zone.ErrInvalidTTL) {
				t.Fatalf("ParseTTL(%q) = %d, %v; want ErrInvalidTTL", tc.in, got, err)
			}
			if !errors.Is(err, zone.ErrInvalid) {
				t.Errorf("error %v does not wrap ErrInvalid", err)
			}
		})
	}
}

func TestTTLAccessors(t *testing.T) {
	t.Parallel()

	ttl := zone.TTL(3600)
	if got := ttl.String(); got != "3600" {
		t.Errorf("String() = %q, want %q", got, "3600")
	}
	if got := ttl.Duration(); got != time.Hour {
		t.Errorf("Duration() = %v, want %v", got, time.Hour)
	}
	if !ttl.Valid() {
		t.Error("3600 should be a valid TTL")
	}
	if !zone.MaxTTL.Valid() {
		t.Error("MaxTTL should be valid")
	}
	if (zone.MaxTTL + 1).Valid() {
		t.Error("one above MaxTTL should be invalid")
	}
}

func TestTTLJSON(t *testing.T) {
	t.Parallel()

	type record struct {
		TTL zone.TTL `json:"ttl"`
	}

	t.Run("marshals as a number", func(t *testing.T) {
		t.Parallel()

		b, err := json.Marshal(record{TTL: 3600})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if want := `{"ttl":3600}`; string(b) != want {
			t.Errorf("Marshal = %s, want %s", b, want)
		}
	})

	t.Run("accepts a number or a suffixed string", func(t *testing.T) {
		t.Parallel()

		for _, in := range []string{`{"ttl":3600}`, `{"ttl":"1h"}`, `{"ttl":"3600"}`} {
			var out record
			if err := json.Unmarshal([]byte(in), &out); err != nil {
				t.Fatalf("Unmarshal(%s): %v", in, err)
			}
			if out.TTL != 3600 {
				t.Errorf("Unmarshal(%s) = %d, want 3600", in, out.TTL)
			}
		}
	})

	t.Run("rejects out-of-range and malformed values", func(t *testing.T) {
		t.Parallel()

		for _, in := range []string{`{"ttl":2147483648}`, `{"ttl":"5y"}`, `{"ttl":true}`, `{"ttl":-1}`} {
			var out record
			if err := json.Unmarshal([]byte(in), &out); !errors.Is(err, zone.ErrInvalidTTL) {
				t.Errorf("Unmarshal(%s) error = %v, want ErrInvalidTTL", in, err)
			}
		}
	})
}
