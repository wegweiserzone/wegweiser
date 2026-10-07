package cli

import "testing"

func TestNearest(t *testing.T) {
	t.Parallel()

	candidates := []string{"example.com.", "example.org.", "internal.lan."}
	tests := []struct {
		typed, want string
	}{
		{"example.com.", "example.com."},
		{"exmaple.com.", "example.com."}, // two letters swapped
		{"exmaple.cmo.", "example.com."}, // two swaps, two steps
		{"exmalpe.cmo.", ""},             // three is another name
		{"EXAMPLE.COM.", "example.com."},
		{"internl.lan.", "internal.lan."},
		{"elsewhere.test.", ""}, // a different name, not a typo
		{"", ""},
	}
	for _, tt := range tests {
		if got := nearest(tt.typed, candidates); got != tt.want {
			t.Errorf("nearest(%q) = %q, want %q", tt.typed, got, tt.want)
		}
	}
}
