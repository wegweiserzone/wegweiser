package secondary

import (
	"fmt"
	"net/netip"
	"strings"
)

// bindZoneDir is where a copy goes when nobody said. BIND keeps a secondary
// zone across a restart only if it has somewhere to write it, so this is
// filled in rather than left out, and it is the one line in the output that
// varies by distribution.
const bindZoneDir = "/var/lib/named"

// renderBIND writes named.conf syntax for BIND 9.16 or newer.
func renderBIND(b *strings.Builder, c Config) {
	b.WriteString("// Written by `weg secondary config bind`. Regenerate it rather than edit it.\n")
	b.WriteString("// BIND 9.16 or newer, which is where `masters` became `primaries`.\n")
	b.WriteString("// Include this file from named.conf.\n")

	if c.Key != nil {
		fmt.Fprintf(b, "\nkey \"%s\" {\n", c.Key.Name)
		fmt.Fprintf(b, "    algorithm %s;\n", algorithm(c.Key.Algorithm))
		fmt.Fprintf(b, "    secret \"%s\";\n", c.Key.Secret)
		b.WriteString("};\n")

		// The server clause is what makes BIND sign what it sends here. A key
		// block on its own is a secret nothing reaches for.
		fmt.Fprintf(b, "\nserver %s {\n", c.Primary.Addr())
		fmt.Fprintf(b, "    keys { \"%s\"; };\n", c.Key.Name)
		b.WriteString("};\n")
	}

	dir := c.ZoneDir
	if dir == "" {
		dir = bindZoneDir
	}
	for _, z := range c.Zones {
		fmt.Fprintf(b, "\nzone \"%s\" {\n", z)
		b.WriteString("    type secondary;\n")
		fmt.Fprintf(b, "    primaries { %s; };\n", bindPrimary(c.Primary))
		fmt.Fprintf(b, "    file \"%s\";\n", zoneFile(dir, z))
		b.WriteString("};\n")
	}
}

// bindPrimary writes where a zone is fetched from, naming the port only where
// it is not the one a configuration would otherwise mean.
func bindPrimary(ap netip.AddrPort) string {
	if ap.Port() == dnsPort {
		return ap.Addr().String()
	}
	return fmt.Sprintf("%s port %d", ap.Addr(), ap.Port())
}
