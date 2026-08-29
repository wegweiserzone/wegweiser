// Package secondary writes the configuration the other end of a zone transfer
// needs.
//
// It writes a file and never installs one;
// docs/decisions/d34-generated-secondary-configuration.md says why the line is
// drawn there.
package secondary

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// dnsPort is the port RFC 1035 §4.2 assigns, and the one a configuration says
// nothing about.
const dnsPort = 53

// Format is the software a configuration is written for.
type Format string

const (
	// FormatBIND is BIND 9.16 or newer, which is where `masters` became
	// `primaries`.
	FormatBIND Format = "bind"

	// FormatKnot is Knot DNS 3.
	FormatKnot Format = "knot"
)

// Formats are the ones that can be written, in the order they are offered.
//
// PowerDNS is absent on purpose rather than for want of a template: its zones
// live in a backend rather than in a file, so the equivalent is a list of
// `pdnsutil` commands, which is a different thing to generate.
func Formats() []Format { return []Format{FormatBIND, FormatKnot} }

// Valid reports whether f names a format this package writes.
func (f Format) Valid() bool { return slices.Contains(Formats(), f) }

// String returns the format the way it is named on a command line.
func (f Format) String() string { return string(f) }

// ErrUnknownFormat is returned for software this package does not write for.
var ErrUnknownFormat = errors.New("secondary: no configuration is written for that software")

// Key is the TSIG key both ends sign with (RFC 8945).
type Key struct {
	// Name is the key name, in domain name syntax. Both programs written for
	// here take it with its trailing dot.
	Name zone.Name

	// Algorithm is stored with the trailing dot RFC 8945 gives it and is
	// written without one, because that is how both programs spell it. Getting
	// it wrong costs a BADKEY that says nothing about a dot.
	Algorithm zone.TSIGAlgorithm

	// Secret is the shared secret, base64, as every implementation writes one.
	Secret string
}

// Config is what the other end needs to know.
type Config struct {
	// Primary is where this server answers a transfer request. It is given
	// rather than worked out: a server does not know which of its addresses
	// the world reaches it on, and a hidden primary is named by no record to
	// ask (D34).
	Primary netip.AddrPort

	// Zones are the apexes to mirror, written out in this order.
	Zones []zone.Name

	// Key is what the secondary signs its requests with, or nil where the
	// transfer list grants by address alone.
	Key *Key

	// ZoneDir is where the secondary keeps its copies. Empty leaves each
	// program the place it uses anyway.
	ZoneDir string
}

// Render writes c in the syntax f.
//
// It is deterministic. The same configuration renders byte for byte the same,
// so regenerating one under configuration management produces no diff when
// nothing has changed, which is why nothing here is stamped with a time or a
// version.
func Render(w io.Writer, f Format, c Config) error {
	if err := c.validate(); err != nil {
		return err
	}

	var b strings.Builder
	switch f {
	case FormatBIND:
		renderBIND(&b, c)
	case FormatKnot:
		renderKnot(&b, c)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownFormat, f)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// validate refuses what would render into a file that cannot work, as opposed
// to one that works and is pointed at the wrong arrangement; [Arrangement] has
// the latter.
func (c Config) validate() error {
	if !c.Primary.IsValid() {
		return errors.New("secondary: the address this server is reached on is missing")
	}
	if c.Key != nil {
		switch {
		case c.Key.Name.IsZero():
			return errors.New("secondary: the key has no name")
		case !c.Key.Algorithm.Valid():
			return fmt.Errorf("secondary: %q is not an algorithm this server signs with",
				c.Key.Algorithm)
		case c.Key.Secret == "":
			// A revoked key has no secret, and writing the block without one
			// produces a file that parses and authenticates nothing.
			return fmt.Errorf("secondary: the key %s has no secret", c.Key.Name)
		}
	}
	return nil
}

// Arrangement is what this server has been told about the transfer. None of it
// is written out: it is what [Arrangement.Warnings] reads.
type Arrangement struct {
	// AllowPrefixes and AllowKeys are the transfer list, which starts empty and
	// grants nothing until somebody is named (D26).
	AllowPrefixes []netip.Prefix
	AllowKeys     []zone.Name

	// Notify is where a change is announced. A secondary missing from it still
	// gets the zone, on its own refresh timer (D27).
	Notify []netip.Addr

	// Secondary is the address the configuration is for, and is the zero value
	// where nobody named it. Naming it is what lets the two lists be checked
	// rather than only described.
	Secondary netip.Addr
}

// Warnings reports what about this arrangement will not work, in the order it
// is set up in.
//
// Each of them produces a configuration that is syntactically perfect and
// either fetches nothing or fetches the zone and never hears that it changed.
// They state what holds; offering the fix beside it is a client's job, the way
// a zone check's findings work (D31).
func (a Arrangement) Warnings(c Config) []string {
	var out []string

	switch {
	case len(a.AllowPrefixes) == 0 && len(a.AllowKeys) == 0:
		out = append(out, "nobody may transfer a zone from this server, "+
			"so every request this configuration makes is refused")
	case c.Key != nil && !containsName(a.AllowKeys, c.Key.Name):
		out = append(out, fmt.Sprintf("the key %s is not on the transfer list, "+
			"so a request signed with it is refused", c.Key.Name))
	case c.Key == nil && a.Secondary.IsValid() && !covers(a.AllowPrefixes, a.Secondary):
		out = append(out, fmt.Sprintf("%s is not on the transfer list and this "+
			"configuration signs nothing, so its requests are refused", a.Secondary))
	}

	switch {
	case len(a.Notify) == 0:
		out = append(out, "nobody is told when a zone changes, so this secondary "+
			"waits out its refresh timer for every change")
	case a.Secondary.IsValid() && !containsAddr(a.Notify, a.Secondary):
		out = append(out, fmt.Sprintf("%s is not on the notify list, so it waits "+
			"out its refresh timer for every change", a.Secondary))
	}

	if len(c.Zones) == 0 {
		out = append(out, "this server holds no zones, so there is nothing to mirror")
	}
	return out
}

// containsName reports whether names holds n, compared the way RFC 8945 §9
// compares a key name.
func containsName(names []zone.Name, n zone.Name) bool {
	return slices.ContainsFunc(names, n.Equal)
}

// containsAddr reports whether addrs holds a. Both sides are unmapped first, so
// that a v4-in-v6 address matches the v4 one it means.
func containsAddr(addrs []netip.Addr, a netip.Addr) bool {
	a = a.Unmap()
	return slices.ContainsFunc(addrs, func(c netip.Addr) bool { return c.Unmap() == a })
}

// covers reports whether any prefix holds a.
func covers(prefixes []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(a) })
}

// zoneFile is where a secondary keeps its copy of a zone. It is joined with
// slashes rather than with the separator of whatever is running this, because
// the path belongs to the machine being configured.
func zoneFile(dir string, name zone.Name) string {
	return strings.TrimSuffix(dir, "/") + "/" + strings.TrimSuffix(name.String(), ".") + ".zone"
}

// algorithm writes a TSIG algorithm the way both programs spell it, which is
// without the trailing dot it is stored with.
func algorithm(a zone.TSIGAlgorithm) string {
	return strings.TrimSuffix(a.String(), ".")
}
