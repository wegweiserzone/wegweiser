package secondary

import (
	"fmt"
	"net/netip"
	"strings"
)

// knotRemote and knotACL are the identifiers the generated blocks refer to each
// other by. They are fixed rather than derived from anything, because a
// configuration that is regenerated has to name the same things it did before.
const (
	knotRemote = "wegweiser"
	knotACL    = "wegweiser-notify"
)

// renderKnot writes knot.conf syntax for Knot DNS 3.
func renderKnot(b *strings.Builder, c Config) {
	b.WriteString("# Written by `weg secondary config knot`. Regenerate it rather than edit it.\n")
	b.WriteString("# Knot DNS 3. Merge these blocks into knot.conf: a section given twice is an error.\n")

	if c.Key != nil {
		b.WriteString("\nkey:\n")
		fmt.Fprintf(b, "  - id: %s\n", c.Key.Name)
		fmt.Fprintf(b, "    algorithm: %s\n", algorithm(c.Key.Algorithm))
		fmt.Fprintf(b, "    secret: %s\n", c.Key.Secret)
	}

	b.WriteString("\nremote:\n")
	fmt.Fprintf(b, "  - id: %s\n", knotRemote)
	fmt.Fprintf(b, "    address: %s\n", knotAddress(c.Primary))
	if c.Key != nil {
		fmt.Fprintf(b, "    key: %s\n", c.Key.Name)
	}

	// Without this Knot fetches the zone and then drops every notification, so
	// the zone stays correct and the news takes a refresh interval. Nothing
	// reports it, which is why it is written whether or not anybody asked.
	//
	// The address is the whole rule even where there is a key. An ACL naming
	// one demands a signature, and whether a notification carries one is a
	// separate setting here from whether a transfer is signed, so a rule that
	// insisted would drop the notifications of an arrangement that is set up
	// correctly.
	b.WriteString("\nacl:\n")
	fmt.Fprintf(b, "  - id: %s\n", knotACL)
	fmt.Fprintf(b, "    address: %s\n", c.Primary.Addr())
	b.WriteString("    action: notify\n")

	if c.ZoneDir != "" {
		b.WriteString("\ntemplate:\n")
		b.WriteString("  - id: default\n")
		fmt.Fprintf(b, "    storage: %s\n", strings.TrimSuffix(c.ZoneDir, "/"))
	}

	if len(c.Zones) == 0 {
		return
	}
	b.WriteString("\nzone:\n")
	for _, z := range c.Zones {
		fmt.Fprintf(b, "  - domain: %s\n", z)
		fmt.Fprintf(b, "    master: %s\n", knotRemote)
		fmt.Fprintf(b, "    acl: %s\n", knotACL)
	}
}

// knotAddress writes an address the way knot.conf takes one, with a port after
// an @ only where it is not the one it would otherwise mean.
func knotAddress(ap netip.AddrPort) string {
	if ap.Port() == dnsPort {
		return ap.Addr().String()
	}
	return fmt.Sprintf("%s@%d", ap.Addr(), ap.Port())
}
