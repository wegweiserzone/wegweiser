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
	// knotACLSigned is the same rule for a notification that carries the key.
	knotACLSigned = "wegweiser-notify-signed"
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
	// Two rules where there is a key, because one cannot cover both cases. A
	// rule naming a key matches only a signed request, and a rule naming none
	// matches only an unsigned one. Whether a notification is signed is a
	// separate setting here from whether a transfer is, so a secondary
	// configured from one key has to accept either: with a single rule, one of
	// the two arrangements fetches the zone and then drops every notification,
	// and the zone stays correct while the news takes a refresh interval.
	// Nothing reports that, which is why both are written whether or not
	// anybody asked.
	b.WriteString("\nacl:\n")
	fmt.Fprintf(b, "  - id: %s\n", knotACL)
	fmt.Fprintf(b, "    address: %s\n", c.Primary.Addr())
	b.WriteString("    action: notify\n")
	if c.Key != nil {
		fmt.Fprintf(b, "  - id: %s\n", knotACLSigned)
		fmt.Fprintf(b, "    address: %s\n", c.Primary.Addr())
		fmt.Fprintf(b, "    key: %s\n", c.Key.Name)
		b.WriteString("    action: notify\n")
	}

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
		if c.Key != nil {
			fmt.Fprintf(b, "    acl: [%s, %s]\n", knotACL, knotACLSigned)
		} else {
			fmt.Fprintf(b, "    acl: %s\n", knotACL)
		}
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
