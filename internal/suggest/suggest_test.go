package suggest

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
		if got := Nearest(tt.typed, candidates); got != tt.want {
			t.Errorf("Nearest(%q) = %q, want %q", tt.typed, got, tt.want)
		}
	}
}

func TestDidYouMean(t *testing.T) {
	t.Parallel()

	kinds := []string{"forward", "reverse"}
	tests := []struct {
		typed, want string
	}{
		{"froward", "; did you mean forward?"},
		{"fwd", ""},     // too far to be a slip
		{"FORWARD", ""}, // what was typed, only louder
		{"reverse", ""}, // nothing to suggest for a word that was right
	}
	for _, tt := range tests {
		if got := DidYouMean(tt.typed, kinds); got != tt.want {
			t.Errorf("DidYouMean(%q) = %q, want %q", tt.typed, got, tt.want)
		}
	}
}
