package secondary_test

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/secondary"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// update rewrites the golden files instead of comparing against them. The
// output is another program's configuration syntax, so it is meant to be read
// by a person as well as by a test.
var update = flag.Bool("update", false, "rewrite the golden files")

func name(t *testing.T, s string) zone.Name {
	t.Helper()
	n, err := zone.ParseName(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

func addrPort(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ap
}

func render(t *testing.T, f secondary.Format, c secondary.Config) string {
	t.Helper()
	var b strings.Builder
	if err := secondary.Render(&b, f, c); err != nil {
		t.Fatalf("render %s: %v", f, err)
	}
	return b.String()
}

// golden compares against testdata/<file>, or writes it under -update.
func golden(t *testing.T, file, got string) {
	t.Helper()
	path := filepath.Join("testdata", file)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s does not match:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestRender(t *testing.T) {
	key := &secondary.Key{
		Name:      name(t, "ns2.example.com."),
		Algorithm: zone.HMACSHA256,
		Secret:    "aG93IG5vdyBicm93biBjb3c=",
	}
	zones := []zone.Name{
		name(t, "example.com."),
		name(t, "2.0.192.in-addr.arpa."),
	}

	cases := []struct {
		file   string
		format secondary.Format
		config secondary.Config
	}{
		{
			file:   "bind-key.conf",
			format: secondary.FormatBIND,
			config: secondary.Config{
				Primary: addrPort(t, "192.0.2.1:53"),
				Zones:   zones,
				Key:     key,
			},
		},
		{
			// No key: the transfer list grants by address, so there is nothing
			// to sign with and no server clause to point at a key.
			file:   "bind-open.conf",
			format: secondary.FormatBIND,
			config: secondary.Config{
				Primary: addrPort(t, "192.0.2.1:53"),
				Zones:   zones[:1],
				ZoneDir: "/var/named/secondary",
			},
		},
		{
			// A primary on a port of its own, reached over IPv6.
			file:   "bind-port.conf",
			format: secondary.FormatBIND,
			config: secondary.Config{
				Primary: addrPort(t, "[2001:db8::1]:5353"),
				Zones:   zones[:1],
				Key:     key,
			},
		},
		{
			file:   "knot-key.conf",
			format: secondary.FormatKnot,
			config: secondary.Config{
				Primary: addrPort(t, "192.0.2.1:53"),
				Zones:   zones,
				Key:     key,
			},
		},
		{
			file:   "knot-open.conf",
			format: secondary.FormatKnot,
			config: secondary.Config{
				Primary: addrPort(t, "192.0.2.1:53"),
				Zones:   zones[:1],
				ZoneDir: "/var/lib/knot/",
			},
		},
		{
			file:   "knot-port.conf",
			format: secondary.FormatKnot,
			config: secondary.Config{
				Primary: addrPort(t, "[2001:db8::1]:5353"),
				Zones:   zones[:1],
				Key:     key,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			golden(t, c.file, render(t, c.format, c.config))
		})
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	c := secondary.Config{
		Primary: addrPort(t, "192.0.2.1:53"),
		Zones:   []zone.Name{name(t, "example.com.")},
		Key: &secondary.Key{
			Name:      name(t, "ns2.example.com."),
			Algorithm: zone.HMACSHA512,
			Secret:    "c2Vjb25kIHJ1bg==",
		},
	}
	for _, f := range secondary.Formats() {
		if first, second := render(t, f, c), render(t, f, c); first != second {
			t.Errorf("%s renders differently on a second run", f)
		}
	}
}

func TestRenderRefuses(t *testing.T) {
	zones := []zone.Name{name(t, "example.com.")}
	good := addrPort(t, "192.0.2.1:53")

	cases := []struct {
		name   string
		format secondary.Format
		config secondary.Config
		want   string
	}{
		{
			name:   "software nobody writes for",
			format: secondary.Format("djbdns"),
			config: secondary.Config{Primary: good, Zones: zones},
			want:   "no configuration is written",
		},
		{
			name:   "no address to fetch from",
			format: secondary.FormatBIND,
			config: secondary.Config{Zones: zones},
			want:   "address this server is reached on is missing",
		},
		{
			name:   "a key with no name",
			format: secondary.FormatBIND,
			config: secondary.Config{
				Primary: good, Zones: zones,
				Key: &secondary.Key{Algorithm: zone.HMACSHA256, Secret: "eA=="},
			},
			want: "key has no name",
		},
		{
			name:   "an algorithm this server does not sign with",
			format: secondary.FormatKnot,
			config: secondary.Config{
				Primary: good, Zones: zones,
				Key: &secondary.Key{
					Name: name(t, "ns2.example.com."), Algorithm: "hmac-md5.", Secret: "eA==",
				},
			},
			want: "not an algorithm this server signs with",
		},
		{
			// A revoked key keeps its name and loses its secret, and a block
			// written from one authenticates nothing while parsing perfectly.
			name:   "a key whose secret is gone",
			format: secondary.FormatBIND,
			config: secondary.Config{
				Primary: good, Zones: zones,
				Key: &secondary.Key{Name: name(t, "ns2.example.com."), Algorithm: zone.HMACSHA256},
			},
			want: "has no secret",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := secondary.Render(&strings.Builder{}, c.format, c.config)
			if err == nil {
				t.Fatal("it rendered anyway")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not say %q:\n%v", c.want, err)
			}
		})
	}
}

// Knot matches an access rule naming a key only against a signed request, and
// one naming none only against an unsigned one. Whether a notification carries
// the key is a setting of its own here, separate from whether a transfer does,
// so a configuration written from one key has to accept either. With a single
// rule one of the two arrangements transfers the zone and then drops every
// notification, which leaves the zone correct and the news a refresh interval
// late, and nothing anywhere says so.
func TestKnotTakesANotificationSignedOrNot(t *testing.T) {
	t.Parallel()

	signed := secondary.Config{
		Primary: addrPort(t, "192.0.2.1:53"),
		Zones:   []zone.Name{name(t, "example.com.")},
		Key: &secondary.Key{
			Name:      name(t, "ns2.example.com."),
			Algorithm: zone.HMACSHA256,
			Secret:    "aG93IG5vdyBicm93biBjb3c=",
		},
	}

	var b strings.Builder
	if err := secondary.Render(&b, secondary.FormatKnot, signed); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := b.String()

	for _, want := range []string{
		"  - id: wegweiser-notify\n    address: 192.0.2.1\n    action: notify\n",
		"  - id: wegweiser-notify-signed\n    address: 192.0.2.1\n" +
			"    key: ns2.example.com.\n    action: notify\n",
		"    acl: [wegweiser-notify, wegweiser-notify-signed]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the configuration is missing:\n%s\ngot:\n%s", want, got)
		}
	}

	// Without a key there is nothing to sign with, so the second rule would
	// name a key that the file never defines.
	open := signed
	open.Key = nil
	b.Reset()
	if err := secondary.Render(&b, secondary.FormatKnot, open); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(b.String(), "wegweiser-notify-signed") {
		t.Errorf("an unsigned arrangement was given a rule for a key:\n%s", b.String())
	}
}
