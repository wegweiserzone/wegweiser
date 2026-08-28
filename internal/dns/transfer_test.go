package dns

import (
	"net"
	"net/netip"
	"slices"
	"testing"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// loopback is the only client any of these tests can be.
var loopback = Allow{Prefixes: []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"),
}}

// readAXFR reads a whole transfer: messages until the second start of
// authority closes it, or one message where the server said no.
func readAXFR(t *testing.T, conn net.Conn) []*wire.Msg {
	t.Helper()

	var msgs []*wire.Msg
	soas := 0
	for {
		m := parse(t, readFramed(t, conn))
		msgs = append(msgs, m)
		if m.Rcode != wire.RcodeSuccess || len(m.Answer) == 0 {
			return msgs
		}
		for _, rr := range m.Answer {
			if _, ok := rr.(*wire.SOA); ok {
				soas++
			}
		}
		if soas >= 2 {
			return msgs
		}
	}
}

// records flattens a transfer into the lines it carried.
func transferred(msgs []*wire.Msg) []string {
	var out []string
	for _, m := range msgs {
		for _, rr := range m.Answer {
			out = append(out, rr.String())
		}
	}
	return out
}

// ask sends one AXFR query for name and reads what comes back.
func askAXFR(t *testing.T, addr, name string) []*wire.Msg {
	t.Helper()

	conn := dialTCP(t, addr)
	writeFramed(t, conn, packQuery(t, name, zone.TypeAXFR))
	return readAXFR(t, conn)
}

func TestTransferIsRefusedUntilSomebodyIsAllowed(t *testing.T) {
	t.Parallel()

	// The default, and the one that matters: a server nobody configured for
	// transfers hands its zones to nobody (D26).
	s := startServer(t, resolveFixture(t), Config{})

	msgs := askAXFR(t, s.Addr().String(), "example.com.")
	if len(msgs) != 1 {
		t.Fatalf("the refusal took %d messages", len(msgs))
	}
	if msgs[0].Rcode != wire.RcodeRefused {
		t.Errorf("rcode is %s, want REFUSED", wire.RcodeToString[msgs[0].Rcode])
	}
	if len(msgs[0].Answer) != 0 {
		t.Errorf("the refusal carried %d records", len(msgs[0].Answer))
	}
}

func TestTransferIsRefusedFromOutsideTheList(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{
		Transfers: Allow{Prefixes: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}},
	})

	msgs := askAXFR(t, s.Addr().String(), "example.com.")
	if msgs[0].Rcode != wire.RcodeRefused {
		t.Errorf("rcode is %s, want REFUSED", wire.RcodeToString[msgs[0].Rcode])
	}
}

func TestTransferOfAZoneThisServerDoesNotHold(t *testing.T) {
	t.Parallel()

	s := startServer(t, resolveFixture(t), Config{Transfers: loopback})

	msgs := askAXFR(t, s.Addr().String(), "example.org.")
	if msgs[0].Rcode != wire.RcodeRefused {
		t.Errorf("rcode is %s, want REFUSED", wire.RcodeToString[msgs[0].Rcode])
	}
}

func TestTransferSendsTheWholeZoneBetweenTwoSOAs(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)
	s := startServer(t, snap, Config{Transfers: loopback})

	msgs := askAXFR(t, s.Addr().String(), "example.com.")
	got := transferred(msgs)
	if len(got) < 3 {
		t.Fatalf("the transfer carried %v", got)
	}

	if _, ok := msgs[0].Answer[0].(*wire.SOA); !ok {
		t.Errorf("the transfer opens with %s, want the start of authority", got[0])
	}
	last := msgs[len(msgs)-1].Answer
	if _, ok := last[len(last)-1].(*wire.SOA); !ok {
		t.Errorf("the transfer closes with %s, want the start of authority", got[len(got)-1])
	}
	if got[0] != got[len(got)-1] {
		t.Errorf("it opens with %q and closes with %q", got[0], got[len(got)-1])
	}

	// Every record of the zone, once, plus the start of authority a second
	// time to close (RFC 5936 §2.2).
	want := snap.zoneAt(zone.MustParseName("example.com.")).count + 1
	if len(got) != want {
		t.Errorf("the transfer carried %d records, want %d", len(got), want)
	}

	for _, line := range []string{
		"www.example.com.\t300\tIN\tA\t192.0.2.10",
		"mail.example.com.\t300\tIN\tMX\t10 mx.example.com.",
	} {
		if !slices.Contains(got, line) {
			t.Errorf("the transfer is missing %q; it carried %v", line, got)
		}
	}

	for _, m := range msgs {
		if !m.Authoritative {
			t.Error("a transfer message is not authoritative")
		}
	}
}

func TestTransferSendsTheSameThingTwice(t *testing.T) {
	t.Parallel()

	// The zone lives in a map, so without an order of its own a transfer would
	// differ between two runs and nothing downstream could compare them.
	s := startServer(t, resolveFixture(t), Config{Transfers: loopback})
	addr := s.Addr().String()

	first := transferred(askAXFR(t, addr, "example.com."))
	for range 5 {
		if got := transferred(askAXFR(t, addr, "example.com.")); !slices.Equal(got, first) {
			t.Fatalf("two transfers of one zone differ\n got: %v\nwant: %v", got, first)
		}
	}
}

func TestAQueryStillWorksAfterATransfer(t *testing.T) {
	t.Parallel()

	// RFC 5936 §4.1 lets a client keep the connection, and the loop that reads
	// queries has to come back to it.
	s := startServer(t, resolveFixture(t), Config{Transfers: loopback})

	conn := dialTCP(t, s.Addr().String())
	writeFramed(t, conn, packQuery(t, "example.com.", zone.TypeAXFR))
	if msgs := readAXFR(t, conn); msgs[0].Rcode != wire.RcodeSuccess {
		t.Fatalf("the transfer was refused: %s", wire.RcodeToString[msgs[0].Rcode])
	}

	writeFramed(t, conn, packQuery(t, "www.example.com.", zone.TypeA))
	m := parse(t, readFramed(t, conn))
	if m.Rcode != wire.RcodeSuccess || len(m.Answer) == 0 {
		t.Errorf("the query after the transfer got %s with %d answers",
			wire.RcodeToString[m.Rcode], len(m.Answer))
	}
}

func TestAllow(t *testing.T) {
	t.Parallel()

	list := Allow{
		Prefixes: []netip.Prefix{
			netip.MustParsePrefix("192.0.2.0/24"),
			netip.MustParsePrefix("2001:db8::/32"),
		},
		Keys: []zone.Name{zone.MustParseName("secondary.example.com.")},
	}

	tests := []struct {
		name   string
		client string
		key    string
		want   bool
	}{
		{name: "inside the prefix", client: "192.0.2.7", want: true},
		{name: "the last address of it", client: "192.0.2.255", want: true},
		{name: "another network", client: "198.51.100.7", want: false},
		{name: "inside the v6 prefix", client: "2001:db8::1", want: true},
		{name: "beside it", client: "2001:db9::1", want: false},
		// A client reaching a dual-stack socket over IPv4 arrives mapped, and
		// no IPv4 prefix contains the mapped form.
		{name: "mapped, inside", client: "::ffff:192.0.2.7", want: true},
		{name: "mapped, outside", client: "::ffff:198.51.100.7", want: false},
		{
			// The whole point of a key: it is not an address.
			name:   "a named key, from anywhere at all",
			client: "203.0.113.9", key: "secondary.example.com.", want: true,
		},
		{name: "a key nobody named", client: "203.0.113.9", key: "other.example.com.", want: false},
		{
			name:   "a key is matched the way a name is",
			client: "203.0.113.9", key: "SECONDARY.Example.COM.", want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var key zone.Name
			if tt.key != "" {
				key = zone.MustParseName(tt.key)
			}
			got := list.MayTransfer(
				netip.MustParseAddr(tt.client), key, zone.MustParseName("example.com."))
			if got != tt.want {
				t.Errorf("MayTransfer(%s, %q) = %v, want %v", tt.client, tt.key, got, tt.want)
			}
		})
	}

	empty := Allow{}
	if empty.MayTransfer(netip.MustParseAddr("192.0.2.7"), zone.Name{}, zone.Name{}) {
		t.Error("an empty list allowed somebody")
	}
	if empty.MayTransfer(netip.MustParseAddr("192.0.2.7"),
		zone.MustParseName("secondary.example.com."), zone.Name{}) {
		t.Error("an empty list allowed a key it does not name")
	}
}

func TestATransferIsObserved(t *testing.T) {
	t.Parallel()

	// Without this a transfer is invisible: it appears in no metric and in no
	// live stream, which is exactly where an operator looks to find out that
	// somebody is pulling their zones.
	tests := []struct {
		name      string
		transfers Transfers
		zone      string
		rcode     int
		carried   bool
	}{
		{name: "one that is served", transfers: loopback, zone: "example.com.",
			rcode: wire.RcodeSuccess, carried: true},
		{name: "one that is refused", transfers: nil, zone: "example.com.",
			rcode: wire.RcodeRefused},
		{name: "one for a zone that is not here", transfers: loopback, zone: "example.org.",
			rcode: wire.RcodeRefused},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &collector{}
			s := startServer(t, resolveFixture(t), Config{
				Transfers: tt.transfers,
				Observe:   c.observe,
			})
			askAXFR(t, s.Addr().String(), tt.zone)

			// One event for the transfer, however many messages it took.
			evs := c.await(t, 1)
			if len(evs) != 1 {
				t.Fatalf("a transfer produced %d events, want one", len(evs))
			}
			ev := evs[0]
			if ev.Type != zone.TypeAXFR {
				t.Errorf("type = %s, want AXFR", ev.Type)
			}
			if ev.Name != tt.zone {
				t.Errorf("name = %q, want %q", ev.Name, tt.zone)
			}
			if ev.Transport != TCP {
				t.Errorf("transport = %s, want tcp", ev.Transport)
			}
			if ev.Rcode != tt.rcode {
				t.Errorf("rcode = %s, want %s",
					wire.RcodeToString[ev.Rcode], wire.RcodeToString[tt.rcode])
			}
			if ev.Dropped {
				t.Error("the event says nothing reached the wire")
			}
			// A whole zone is more than a refusal, and both are more than
			// nothing.
			if tt.carried && ev.Size < 500 {
				t.Errorf("size = %d, want the whole zone", ev.Size)
			}
			if ev.Size == 0 {
				t.Error("size = 0")
			}
		})
	}
}
