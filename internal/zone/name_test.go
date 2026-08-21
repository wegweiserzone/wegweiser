package zone_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestParseNameValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		wantString string
		wantLabels []string
	}{
		{"root", ".", ".", nil},
		{"absolute", "www.example.com.", "www.example.com.", []string{"www", "example", "com"}},
		{
			name:       "trailing dot is optional",
			in:         "www.example.com",
			wantString: "www.example.com.",
			wantLabels: []string{"www", "example", "com"},
		},
		{
			// RFC 4343: comparison is case-insensitive, so the stored form is
			// lowercased and two spellings become one name.
			name:       "uppercase is folded",
			in:         "WWW.Example.COM.",
			wantString: "www.example.com.",
			wantLabels: []string{"www", "example", "com"},
		},
		{"single label", "localhost.", "localhost.", []string{"localhost"}},
		{"wildcard", "*.example.com.", "*.example.com.", []string{"*", "example", "com"}},
		{"underscore label", "_dmarc.example.com.", "_dmarc.example.com.", []string{"_dmarc", "example", "com"}},
		{
			name:       "hyphen and digits",
			in:         "0-test-9.example.com.",
			wantString: "0-test-9.example.com.",
			wantLabels: []string{"0-test-9", "example", "com"},
		},
		{
			// RFC 2317 classless delegation zones contain a slash.
			name:       "rfc2317 zone name",
			in:         "0/25.2.0.192.in-addr.arpa.",
			wantString: "0/25.2.0.192.in-addr.arpa.",
			wantLabels: []string{"0/25", "2", "0", "192", "in-addr", "arpa"},
		},
		{
			name:       "escaped dot stays inside the label",
			in:         `a\.b.example.com.`,
			wantString: `a\.b.example.com.`,
			wantLabels: []string{"a.b", "example", "com"},
		},
		{
			name:       "escaped backslash",
			in:         `a\\b.example.com.`,
			wantString: `a\\b.example.com.`,
			wantLabels: []string{`a\b`, "example", "com"},
		},
		{
			name:       "decimal escape",
			in:         `a\098c.example.com.`,
			wantString: "abc.example.com.",
			wantLabels: []string{"abc", "example", "com"},
		},
		{
			// \065 is 'A', which is folded exactly like a literal A would be.
			name:       "decimal escape of an uppercase letter is folded",
			in:         `\065.example.com.`,
			wantString: "a.example.com.",
			wantLabels: []string{"a", "example", "com"},
		},
		{
			name:       "escape of a non-special character is just the character",
			in:         `\a.example.com.`,
			wantString: "a.example.com.",
			wantLabels: []string{"a", "example", "com"},
		},
		{
			// A space uses the character form rather than the numeric one, to
			// match how the wire library prints names inside record data.
			name:       "space",
			in:         `a\032b.example.com.`,
			wantString: `a\ b.example.com.`,
			wantLabels: []string{"a b", "example", "com"},
		},
		{
			name:       "dollar is left literal",
			in:         `a\036b.example.com.`,
			wantString: "a$b.example.com.",
			wantLabels: []string{"a$b", "example", "com"},
		},
		{
			name:       "high octet round-trips",
			in:         `caf\233.example.com.`,
			wantString: `caf\233.example.com.`,
			wantLabels: []string{"caf\xe9", "example", "com"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.ParseName(tc.in)
			if err != nil {
				t.Fatalf("ParseName(%q): %v", tc.in, err)
			}
			if got.String() != tc.wantString {
				t.Errorf("String() = %q, want %q", got.String(), tc.wantString)
			}
			if labels := got.Labels(); !slices.Equal(labels, tc.wantLabels) {
				t.Errorf("Labels() = %q, want %q", labels, tc.wantLabels)
			}
			if got.LabelCount() != len(tc.wantLabels) {
				t.Errorf("LabelCount() = %d, want %d", got.LabelCount(), len(tc.wantLabels))
			}

			// Whatever String produces must parse back to the same name;
			// otherwise a zonefile export would not survive a re-import.
			back, err := zone.ParseName(got.String())
			if err != nil {
				t.Fatalf("String() output %q does not parse: %v", got.String(), err)
			}
			if !back.Equal(got) {
				t.Errorf("round trip changed the name: %q -> %q", got.String(), back.String())
			}
		})
	}
}

func TestParseNameInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{"empty string", "", zone.ErrInvalidName},
		{"leading dot", ".example.com.", zone.ErrEmptyLabel},
		{"double dot", "www..example.com.", zone.ErrEmptyLabel},
		{"dot after trailing dot", "example.com..", zone.ErrEmptyLabel},
		{"trailing backslash", `example.com.\`, zone.ErrBadEscape},
		{"short numeric escape", `\12.example.com.`, zone.ErrBadEscape},
		{"numeric escape out of range", `\256.example.com.`, zone.ErrBadEscape},
		{"label of 64 octets", strings.Repeat("a", 64) + ".example.com.", zone.ErrLabelTooLong},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := zone.ParseName(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseName(%q) error = %v, want %v", tc.in, err, tc.wantErr)
			}
			// Every parse failure is also reportable as a plain invalid name,
			// so a caller that does not care about the specific rule can test
			// for one error.
			if !errors.Is(err, zone.ErrInvalidName) {
				t.Errorf("error %v does not wrap ErrInvalidName", err)
			}
		})
	}
}

// TestParseNameLengthLimits pins the exact boundary of RFC 1035 §2.3.4, which
// counts the length octet of every label and the terminating zero.
func TestParseNameLengthLimits(t *testing.T) {
	t.Parallel()

	label63 := strings.Repeat("a", 63)

	t.Run("label of exactly 63 octets is allowed", func(t *testing.T) {
		t.Parallel()

		if _, err := zone.ParseName(label63 + ".example.com."); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// 3 * (1+63) + (1+61) + 1 terminator = 255, the largest legal encoding.
	maxName := label63 + "." + label63 + "." + label63 + "." + strings.Repeat("b", 61) + "."

	t.Run("name of exactly 255 encoded octets is allowed", func(t *testing.T) {
		t.Parallel()

		n, err := zone.ParseName(maxName)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n.WireLen() != zone.MaxNameWireLen {
			t.Errorf("WireLen() = %d, want %d", n.WireLen(), zone.MaxNameWireLen)
		}
	})

	t.Run("one octet more is rejected", func(t *testing.T) {
		t.Parallel()

		tooLong := label63 + "." + label63 + "." + label63 + "." + strings.Repeat("b", 62) + "."
		if _, err := zone.ParseName(tooLong); !errors.Is(err, zone.ErrNameTooLong) {
			t.Fatalf("error = %v, want ErrNameTooLong", err)
		}
	})
}

func TestNameFromWire(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []byte
		want    string
		wantErr error
	}{
		{
			name: "root",
			in:   []byte{0},
			want: ".",
		},
		{
			name: "two labels",
			in:   append([]byte{3, 'w', 'w', 'w', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e'}, 0),
			want: "www.example.",
		},
		{
			name: "uppercase is folded",
			in:   []byte{3, 'W', 'W', 'W', 0},
			want: "www.",
		},
		{
			name:    "empty buffer",
			in:      nil,
			wantErr: zone.ErrInvalidName,
		},
		{
			name:    "compression pointer is refused",
			in:      []byte{0xC0, 0x0C},
			wantErr: zone.ErrInvalidName,
		},
		{
			name:    "reserved label type is refused",
			in:      []byte{0x41, 'a', 0},
			wantErr: zone.ErrInvalidName,
		},
		{
			name:    "label overruns the buffer",
			in:      []byte{5, 'a', 'b', 0},
			wantErr: zone.ErrInvalidName,
		},
		{
			name:    "octets after the root label",
			in:      []byte{1, 'a', 0, 'x'},
			wantErr: zone.ErrInvalidName,
		},
		{
			name:    "missing root label",
			in:      []byte{1, 'a'},
			wantErr: zone.ErrInvalidName,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.NameFromWire(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("String() = %q, want %q", got.String(), tc.want)
			}
			if !bytes.Equal(got.Wire(), tc.in) && !strings.ContainsFunc(string(tc.in), isUpper) {
				t.Errorf("Wire() = %v, want %v", got.Wire(), tc.in)
			}
		})
	}
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }

func TestParentAndChild(t *testing.T) {
	t.Parallel()

	t.Run("parent walks up to the root", func(t *testing.T) {
		t.Parallel()

		n := zone.MustParseName("www.example.com.")
		want := []string{"example.com.", "com.", "."}

		for _, w := range want {
			p, ok := n.Parent()
			if !ok {
				t.Fatalf("Parent() of %q reported no parent", n)
			}
			if p.String() != w {
				t.Fatalf("Parent() = %q, want %q", p, w)
			}
			n = p
		}
		if _, ok := n.Parent(); ok {
			t.Error("the root should have no parent")
		}
	})

	t.Run("child prepends a label", func(t *testing.T) {
		t.Parallel()

		got, err := zone.MustParseName("example.com.").Child("WWW")
		if err != nil {
			t.Fatalf("Child: %v", err)
		}
		if got.String() != "www.example.com." {
			t.Errorf("Child() = %q, want %q", got, "www.example.com.")
		}
	})

	t.Run("child rejects an over-long result", func(t *testing.T) {
		t.Parallel()

		long := zone.MustParseName(strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
			strings.Repeat("c", 63) + "." + strings.Repeat("d", 61) + ".")
		if _, err := long.Child("x"); !errors.Is(err, zone.ErrNameTooLong) {
			t.Errorf("error = %v, want ErrNameTooLong", err)
		}
	})

	t.Run("child rejects an empty label", func(t *testing.T) {
		t.Parallel()

		if _, err := zone.MustParseName("example.com.").Child(""); !errors.Is(err, zone.ErrEmptyLabel) {
			t.Errorf("error = %v, want ErrEmptyLabel", err)
		}
	})
}

func TestFirstLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"an ordinary name", "www.example.com.", "www", true},
		{"a single label", "com.", "com", true},
		{"a wildcard", "*.example.com.", "*", true},
		{"an escaped dot stays one label", `a\.b.example.com.`, "a.b", true},
		{"casing is folded like everywhere else", "WWW.example.com.", "www", true},
		{"the root has none", ".", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := zone.MustParseName(tc.input).FirstLabel()
			if ok != tc.ok {
				t.Fatalf("FirstLabel() ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("FirstLabel() = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("the zero name has none", func(t *testing.T) {
		t.Parallel()

		var n zone.Name
		if _, ok := n.FirstLabel(); ok {
			t.Error("the zero Name reported a label")
		}
	})

	t.Run("it agrees with Labels", func(t *testing.T) {
		t.Parallel()

		n := zone.MustParseName("a.b.c.example.com.")
		got, ok := n.FirstLabel()
		if !ok {
			t.Fatal("FirstLabel() reported none")
		}
		if want := n.Labels()[0]; got != want {
			t.Errorf("FirstLabel() = %q, Labels()[0] = %q", got, want)
		}
	})
}

func TestIsSubDomainOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		child string
		super string
		want  bool
	}{
		{"direct child", "www.example.com.", "example.com.", true},
		{"deep descendant", "a.b.c.example.com.", "example.com.", true},
		{"a name is a subdomain of itself", "example.com.", "example.com.", true},
		{"everything is under the root", "example.com.", ".", true},
		{"the root is under itself", ".", ".", true},
		{"parent is not under its child", "example.com.", "www.example.com.", false},
		{"unrelated", "example.net.", "example.com.", false},
		{
			// The trap: the octets of "example.com." are a suffix here, but
			// not at a label boundary.
			name:  "suffix without a label boundary",
			child: "notexample.com.",
			super: "example.com.",
			want:  false,
		},
		{"case is irrelevant", "WWW.EXAMPLE.COM.", "example.com.", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			child := zone.MustParseName(tc.child)
			super := zone.MustParseName(tc.super)
			if got := child.IsSubDomainOf(super); got != tc.want {
				t.Errorf("%q.IsSubDomainOf(%q) = %v, want %v", child, super, got, tc.want)
			}
		})
	}

	t.Run("the zero name is nobody's subdomain", func(t *testing.T) {
		t.Parallel()

		var zero zone.Name
		if zero.IsSubDomainOf(zone.Root) {
			t.Error("the zero name should not be a subdomain of the root")
		}
		if zone.Root.IsSubDomainOf(zero) {
			t.Error("nothing should be a subdomain of the zero name")
		}
	})
}

// canonicalOrder is the RFC 4034 §6.1 example, extended with a wildcard and a
// name whose label contains a zero octet.
var canonicalOrder = []string{
	".",
	"example.",
	"a.example.",
	"yljkjljk.a.example.",
	"Z.a.example.",
	"zABC.a.EXAMPLE.",
	"z.example.",
	`\001.z.example.`,
	"*.z.example.",
	`\200.z.example.`,
}

func TestCompareCanonicalOrder(t *testing.T) {
	t.Parallel()

	names := make([]zone.Name, len(canonicalOrder))
	for i, s := range canonicalOrder {
		names[i] = zone.MustParseName(s)
	}

	for i := range names {
		for j := range names {
			got := names[i].Compare(names[j])
			var want int
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if sign(got) != want {
				t.Errorf("Compare(%q, %q) sign = %d, want %d", names[i], names[j], sign(got), want)
			}
		}
	}
}

// TestSortKeyAgreesWithCompare is the property that lets the database sort
// names with a plain ORDER BY: byte order over SortKey must reproduce
// canonical name order exactly.
func TestSortKeyAgreesWithCompare(t *testing.T) {
	t.Parallel()

	corpus := append([]string{}, canonicalOrder...)
	corpus = append(corpus,
		"www.example.com.",
		"example.com.",
		"com.",
		"*.example.com.",
		"a.example.com.",
		"notexample.com.",
		`a\.b.example.com.`,
		// A zero octet inside a label is legal. Naively joining reversed
		// labels with a zero separator would make this collide with
		// "b.a.example.com.", which is why SortKey escapes it.
		`a\000b.example.com.`,
		"b.a.example.com.",
		"0/25.2.0.192.in-addr.arpa.",
		"2.0.192.in-addr.arpa.",
	)

	names := make([]zone.Name, len(corpus))
	for i, s := range corpus {
		names[i] = zone.MustParseName(s)
	}

	for i := range names {
		for j := range names {
			want := sign(names[i].Compare(names[j]))
			got := sign(bytes.Compare(names[i].SortKey(), names[j].SortKey()))
			if got != want {
				t.Errorf("SortKey order for (%q, %q) = %d, Compare = %d", names[i], names[j], got, want)
			}
		}
	}
}

func TestSortKeyDistinguishesEmbeddedZeroOctet(t *testing.T) {
	t.Parallel()

	// One label containing a zero octet, versus two labels. Both would encode
	// identically if the separator were a bare zero.
	oneLabel := zone.MustParseName(`a\000b.example.com.`)
	twoLabels := zone.MustParseName("b.a.example.com.")

	if bytes.Equal(oneLabel.SortKey(), twoLabels.SortKey()) {
		t.Fatalf("sort keys collide: %q and %q both encode to %v",
			oneLabel, twoLabels, oneLabel.SortKey())
	}
}

func TestZeroAndRoot(t *testing.T) {
	t.Parallel()

	var zero zone.Name
	if !zero.IsZero() {
		t.Error("the zero value should report IsZero")
	}
	if zero.IsRoot() {
		t.Error("the zero value is not the root")
	}
	if zero.String() != "" {
		t.Errorf("zero String() = %q, want empty", zero.String())
	}
	if _, ok := zero.Parent(); ok {
		t.Error("the zero value should have no parent")
	}

	if !zone.Root.IsRoot() {
		t.Error("Root should report IsRoot")
	}
	if zone.Root.IsZero() {
		t.Error("Root is a real name, not the zero value")
	}
	if zone.Root.LabelCount() != 0 {
		t.Errorf("Root LabelCount() = %d, want 0", zone.Root.LabelCount())
	}
	if zone.Root.WireLen() != 1 {
		t.Errorf("Root WireLen() = %d, want 1", zone.Root.WireLen())
	}
}

func TestNameJSONRoundTrip(t *testing.T) {
	t.Parallel()

	type record struct {
		Name zone.Name `json:"name"`
	}

	in := record{Name: zone.MustParseName("WWW.Example.com")}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"name":"www.example.com."}`; string(b) != want {
		t.Errorf("Marshal = %s, want %s", b, want)
	}

	var out record
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !out.Name.Equal(in.Name) {
		t.Errorf("round trip = %q, want %q", out.Name, in.Name)
	}

	// An invalid name must not be able to enter the model through decoding.
	if err := json.Unmarshal([]byte(`{"name":"a..b"}`), &out); !errors.Is(err, zone.ErrInvalidName) {
		t.Errorf("Unmarshal of an invalid name: error = %v, want ErrInvalidName", err)
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
