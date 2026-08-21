// Package output renders command results in the three formats every weg
// command supports: human-readable text, JSON and YAML.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
	"sigs.k8s.io/yaml"
)

// Format selects how a command renders its result.
type Format string

const (
	// FormatText is the default, human-readable rendering.
	FormatText Format = "text"
	// FormatJSON is indented JSON, suitable for piping into jq.
	FormatJSON Format = "json"
	// FormatYAML is YAML produced from the same field names as [FormatJSON].
	FormatYAML Format = "yaml"
)

// ErrUnknownFormat is returned when a format string is not one of the
// supported values.
var ErrUnknownFormat = errors.New("unknown output format")

// Formats lists the supported formats, for flag help and shell completion.
func Formats() []string {
	return []string{string(FormatText), string(FormatJSON), string(FormatYAML)}
}

// ParseFormat converts a flag value into a [Format]. It is case-insensitive
// and accepts "yml" as an alias for YAML.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text":
		return FormatText, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("%w %q: expected one of %s",
			ErrUnknownFormat, s, strings.Join(Formats(), ", "))
	}
}

// Printer renders command results to a writer in a chosen format.
type Printer struct {
	out    io.Writer
	errOut io.Writer
	format Format
	color  bool
}

// New returns a Printer writing to out, with errors and diagnostics going to
// errOut. Colour is enabled only when the format is text and out is a terminal
// that has not opted out; see [ColorEnabled].
func New(out, errOut io.Writer, format Format) *Printer {
	return &Printer{
		out:    out,
		errOut: errOut,
		format: format,
		color:  format == FormatText && ColorEnabled(out),
	}
}

// Format reports which format the Printer renders.
func (p *Printer) Format() Format { return p.format }

// Color reports whether colour escape sequences should be emitted.
//
// Commands must consult this rather than deciding for themselves, so that
// NO_COLOR and non-terminal output are honoured consistently.
func (p *Printer) Color() bool { return p.color }

// Color is a terminal colour, as the SGR parameter that selects it.
//
// TODO: Lipgloss is the eventual home for this, once there is a table to draw
// with it. Until then a handful of escape sequences is less than a dependency,
// and having them here rather than in a command is what keeps the NO_COLOR and
// TTY rules in one place.
type Color string

// The colours anything this CLI prints may use. One accent, and the three
// states an operator scans for.
const (
	ColorNone   Color = ""
	ColorRed    Color = "31"
	ColorGreen  Color = "32"
	ColorYellow Color = "33"
	ColorDim    Color = "2"
)

// Paint wraps s in c, or returns it unchanged when colour is not appropriate.
func (p *Printer) Paint(c Color, s string) string {
	if !p.color || c == ColorNone {
		return s
	}
	return "\x1b[" + string(c) + "m" + s + "\x1b[0m"
}

// Out returns the writer results are printed to.
func (p *Printer) Out() io.Writer { return p.out }

// ErrOut returns the writer diagnostics are printed to.
func (p *Printer) ErrOut() io.Writer { return p.errOut }

// Print renders v.
//
// For JSON and YAML, v is marshalled directly. For text, the supplied renderer
// is called with the output writer. A nil renderer falls back to fmt.Fprintln,
// which is adequate for scalars and types with a String method.
func (p *Printer) Print(v any, text func(w io.Writer) error) error {
	switch p.format {
	case FormatJSON:
		enc := json.NewEncoder(p.out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
		return nil

	case FormatYAML:
		b, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode yaml: %w", err)
		}
		if _, err := p.out.Write(b); err != nil {
			return fmt.Errorf("write yaml: %w", err)
		}
		return nil

	case FormatText:
		if text == nil {
			_, err := fmt.Fprintln(p.out, v)
			return err
		}
		return text(p.out)

	default:
		return fmt.Errorf("%w: %q", ErrUnknownFormat, p.format)
	}
}

// ColorEnabled reports whether colour output is appropriate for w.
func ColorEnabled(w io.Writer) bool {
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(w)
}

// IsTerminal reports whether f is backed by a character device.
func IsTerminal(f any) bool {
	return isTerminal(f)
}

// isTerminal reports whether w is a terminal.
func isTerminal(w any) bool {
	f, ok := w.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
