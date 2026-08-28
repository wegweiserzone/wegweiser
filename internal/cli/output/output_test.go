package output_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

func TestParseFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    output.Format
		wantErr bool
	}{
		{"empty defaults to text", "", output.FormatText, false},
		{"text", "text", output.FormatText, false},
		{"json", "json", output.FormatJSON, false},
		{"yaml", "yaml", output.FormatYAML, false},
		{"yml alias", "yml", output.FormatYAML, false},
		{"case insensitive", "JSON", output.FormatJSON, false},
		{"surrounding space", "  yaml  ", output.FormatYAML, false},
		{"unknown", "toml", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := output.ParseFormat(tc.in)
			if tc.wantErr {
				if !errors.Is(err, output.ErrUnknownFormat) {
					t.Fatalf("ParseFormat(%q) error = %v, want ErrUnknownFormat", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFormat(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// payload exercises that JSON and YAML derive field names from the same tags.
type payload struct {
	Name    string `json:"name"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment,omitempty"`
}

func TestPrinterPrint(t *testing.T) {
	t.Parallel()

	v := payload{Name: "www.example.com.", TTL: 3600}

	tests := []struct {
		name     string
		format   output.Format
		text     func(w io.Writer) error
		contains []string
		exact    string
	}{
		{
			name:     "json uses struct tags and omits empty fields",
			format:   output.FormatJSON,
			contains: []string{`"name": "www.example.com."`, `"ttl": 3600`},
		},
		{
			name:     "yaml uses the same field names as json",
			format:   output.FormatYAML,
			contains: []string{"name: www.example.com.", "ttl: 3600"},
		},
		{
			name:   "text uses the supplied renderer",
			format: output.FormatText,
			text: func(w io.Writer) error {
				_, err := io.WriteString(w, "www.example.com. 3600\n")
				return err
			},
			exact: "www.example.com. 3600\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			p := output.New(&buf, io.Discard, tc.format)
			if err := p.Print(v, tc.text); err != nil {
				t.Fatalf("Print: %v", err)
			}

			got := buf.String()
			if tc.exact != "" && got != tc.exact {
				t.Fatalf("Print = %q, want %q", got, tc.exact)
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("Print output missing %q\ngot:\n%s", want, got)
				}
			}
			if tc.format != output.FormatText && strings.Contains(got, "comment") {
				t.Errorf("empty field was not omitted\ngot:\n%s", got)
			}
		})
	}
}

func TestPrinterPrintTextWithoutRenderer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := output.New(&buf, io.Discard, output.FormatText)
	if err := p.Print("plain", nil); err != nil {
		t.Fatalf("Print: %v", err)
	}
	if got, want := buf.String(), "plain\n"; got != want {
		t.Errorf("Print = %q, want %q", got, want)
	}
}

func TestColorEnabled(t *testing.T) {
	// Not parallel: these subtests manipulate process environment variables.
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"not a terminal", nil, false},
		{"CLICOLOR_FORCE overrides a non-terminal", map[string]string{"CLICOLOR_FORCE": "1"}, true},
		{"CLICOLOR_FORCE=0 does not force", map[string]string{"CLICOLOR_FORCE": "0"}, false},
		{"empty CLICOLOR_FORCE does not force", map[string]string{"CLICOLOR_FORCE": ""}, false},
		{"NO_COLOR disables", map[string]string{"NO_COLOR": "1"}, false},
		{"empty NO_COLOR is ignored, per spec", map[string]string{"NO_COLOR": ""}, false},
		{
			name: "CLICOLOR_FORCE takes precedence over NO_COLOR",
			env:  map[string]string{"NO_COLOR": "1", "CLICOLOR_FORCE": "1"},
			want: true,
		},
		{"TERM=dumb disables", map[string]string{"TERM": "dumb"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Establish a known-clean environment, then apply the case.
			t.Setenv("NO_COLOR", "")
			t.Setenv("CLICOLOR_FORCE", "")
			t.Setenv("TERM", "xterm")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			// A bytes.Buffer is never a character device, which isolates the
			// environment rules from terminal detection.
			if got := output.ColorEnabled(&bytes.Buffer{}); got != tc.want {
				t.Errorf("ColorEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}
