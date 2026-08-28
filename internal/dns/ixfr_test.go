package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/journal"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// fixedHistory answers every range with what the test put in it.
type fixedHistory struct {
	commits []*journal.Commit
	held    bool
	err     error
}

func (h fixedHistory) Since(
	context.Context, zone.Name, zone.Serial, zone.Serial,
) ([]*journal.Commit, bool, error) {
	return h.commits, h.held, h.err
}

// ixfrZone is one small zone at a serial the test names, so that a difference
// against it can be written out by hand.
func ixfrZone(t *testing.T, serial uint32) (*zone.Zone, *Snapshot) {
	t.Helper()
	z := newZone(t, "example.com.")
	z.SOA.Serial = zone.NewSerial(serial)
	return z, build(t, z,
		"example.com. 3600 IN NS ns1.example.com.",
		"example.com. 3600 IN NS ns2.example.com.",
		"ns1.example.com. 3600 IN A 192.0.2.1",
		"ns2.example.com. 3600 IN A 192.0.2.2",
		"www.example.com. 300 IN A 192.0.2.10",
		"www.example.com. 300 IN AAAA 2001:db8::10",
		"mail.example.com. 300 IN MX 10 mx.example.com.",
		"mx.example.com. 300 IN A 192.0.2.50",
	)
}

// soaLine is the zone's start of authority as a transfer sends it, at a serial
// the test names.
func soaLine(t *testing.T, z *zone.Zone, serial uint32) string {
	t.Helper()
	soa := z.SOA
	soa.Serial = zone.NewSerial(serial)
	rr, err := soaRRFor(z.Name, soa.TTL, soa)
	if err != nil {
		t.Fatalf("build the start of authority: %v", err)
	}
	return rr.String()
}

// event turns one zonefile line into a journal event.
func event(t *testing.T, op journal.Op, line string) journal.Event {
	t.Helper()
	rec := record(t, "", line)
	return journal.Event{
		Op: op, Name: rec.Name, Class: rec.Class, Type: rec.Type, TTL: rec.TTL, RData: rec.RData,
	}
}

// soaEvent records a change to the zone's own start of authority, which the
// journal carries even though no record holds it.
func soaEvent(t *testing.T, op journal.Op, name zone.Name, soa zone.SOA) journal.Event {
	t.Helper()
	data, err := zone.ParseRData(zone.TypeSOA, zone.ClassIN, soa.RData())
	if err != nil {
		t.Fatalf("parse the start of authority: %v", err)
	}
	return journal.Event{
		Op: op, Name: name, Class: zone.ClassIN, Type: zone.TypeSOA, TTL: soa.TTL, RData: data,
	}
}

// commit is one step of a zone's history, with its deletions before its
// additions the way the journal records them (RFC 1995 §2).
func commit(t *testing.T, from, to uint32, del, add []string) *journal.Commit {
	t.Helper()
	c := &journal.Commit{
		ZoneName:   zone.MustParseName("example.com."),
		SerialFrom: zone.NewSerial(from),
		SerialTo:   zone.NewSerial(to),
		Kind:       journal.KindEdit,
	}
	for _, line := range del {
		c.Events = append(c.Events, event(t, journal.OpDel, line))
	}
	for _, line := range add {
		c.Events = append(c.Events, event(t, journal.OpAdd, line))
	}
	for i := range c.Events {
		c.Events[i].Seq = i
	}
	return c
}

// withSerial puts the version the client holds in the authority section, which
// is where RFC 1995 §3 asks for it.
func withSerial(apex string, serial uint32) func(*wire.Msg) {
	return func(m *wire.Msg) {
		m.Ns = append(m.Ns, &wire.SOA{
			Hdr:    wire.RR_Header{Name: apex, Rrtype: wire.TypeSOA, Class: wire.ClassINET},
			Ns:     "ns1." + apex,
			Mbox:   "hostmaster." + apex,
			Serial: serial,
		})
	}
}

// readIXFR reads an incremental transfer the way a secondary does. The answer
// opens and closes on the current version, and each difference sequence in
// between opens on an older one and then names the version it reaches.
func readIXFR(t *testing.T, conn net.Conn) []*wire.Msg {
	t.Helper()

	var (
		msgs    []*wire.Msg
		rrs     []wire.RR
		read    int
		target  uint32
		adding  bool
		started bool
	)
	for {
		m := parse(t, readFramed(t, conn))
		msgs = append(msgs, m)
		if m.Rcode != wire.RcodeSuccess || len(m.Answer) == 0 {
			return msgs
		}
		rrs = append(rrs, m.Answer...)
		for ; read < len(rrs); read++ {
			soa, ok := rrs[read].(*wire.SOA)
			if !ok {
				continue
			}
			switch {
			case !started:
				target, started = soa.Serial, true
			case adding:
				adding = false
			case soa.Serial == target:
				return msgs
			default:
				adding = true
			}
		}
	}
}

// askIXFR sends one incremental request and reads the whole answer.
func askIXFR(t *testing.T, addr, name string, serial uint32) []*wire.Msg {
	t.Helper()
	conn := dialTCP(t, addr)
	writeFramed(t, conn, packQuery(t, name, zone.TypeIXFR, withSerial(name, serial)))
	return readIXFR(t, conn)
}

// askOnce sends a request and reads a single message, for the answers that are
// one message by definition.
func askOnce(t *testing.T, addr string, query []byte) *wire.Msg {
	t.Helper()
	conn := dialTCP(t, addr)
	writeFramed(t, conn, query)
	return parse(t, readFramed(t, conn))
}

func TestIncrementalTransferSendsOnlyWhatChanged(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 8)
	s := startServer(t, snap, Config{
		Transfers: loopback,
		History: fixedHistory{held: true, commits: []*journal.Commit{
			commit(t, 7, 8,
				[]string{"old.example.com. 300 IN A 192.0.2.99"},
				[]string{"www.example.com. 300 IN A 192.0.2.10"}),
		}},
	})

	got := transferred(askIXFR(t, s.Addr().String(), "example.com.", 7))
	want := []string{
		soaLine(t, z, 8),
		soaLine(t, z, 7),
		"old.example.com.\t300\tIN\tA\t192.0.2.99",
		soaLine(t, z, 8),
		"www.example.com.\t300\tIN\tA\t192.0.2.10",
		soaLine(t, z, 8),
	}
	if !slices.Equal(got, want) {
		t.Errorf("the difference is\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestIncrementalTransferReconstructsEveryVersionItPassesThrough(t *testing.T) {
	t.Parallel()

	// Two steps, neither of which touched the start of authority. The version
	// each one begins at is nowhere in the journal, so it has to be worked back
	// from the one the snapshot holds.
	z, snap := ixfrZone(t, 9)
	s := startServer(t, snap, Config{
		Transfers: loopback,
		History: fixedHistory{held: true, commits: []*journal.Commit{
			commit(t, 7, 8, nil, []string{"a.example.com. 300 IN A 192.0.2.1"}),
			commit(t, 8, 9, nil, []string{"b.example.com. 300 IN A 192.0.2.2"}),
		}},
	})

	got := transferred(askIXFR(t, s.Addr().String(), "example.com.", 7))
	want := []string{
		soaLine(t, z, 9),
		soaLine(t, z, 7),
		soaLine(t, z, 8),
		"a.example.com.\t300\tIN\tA\t192.0.2.1",
		soaLine(t, z, 8),
		soaLine(t, z, 9),
		"b.example.com.\t300\tIN\tA\t192.0.2.2",
		soaLine(t, z, 9),
	}
	if !slices.Equal(got, want) {
		t.Errorf("the difference is\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestIncrementalTransferCarriesTheStartOfAuthorityThatChanged(t *testing.T) {
	t.Parallel()

	// A commit that did change the start of authority records it, and then that
	// record is what frames the step rather than the one the snapshot holds.
	z, snap := ixfrZone(t, 8)
	was := z.SOA
	was.Serial = zone.NewSerial(7)
	was.Refresh = 7200

	c := commit(t, 7, 8, nil, nil)
	c.Events = []journal.Event{
		soaEvent(t, journal.OpDel, z.Name, was),
		soaEvent(t, journal.OpAdd, z.Name, z.SOA),
	}
	s := startServer(t, snap, Config{
		Transfers: loopback,
		History:   fixedHistory{held: true, commits: []*journal.Commit{c}},
	})

	got := transferred(askIXFR(t, s.Addr().String(), "example.com.", 7))
	wasRR, err := soaRRFor(z.Name, was.TTL, was)
	if err != nil {
		t.Fatalf("build the start of authority: %v", err)
	}
	want := []string{soaLine(t, z, 8), wasRR.String(), soaLine(t, z, 8), soaLine(t, z, 8)}
	if !slices.Equal(got, want) {
		t.Errorf("the difference is\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestIncrementalTransferOfAVersionTheClientAlreadyHas(t *testing.T) {
	t.Parallel()

	// RFC 1995 §2: the version, and nothing else. Asking again is how a
	// secondary polls, so this is the common case rather than an edge one.
	z, snap := ixfrZone(t, 8)
	s := startServer(t, snap, Config{Transfers: loopback, History: fixedHistory{held: true}})

	for _, serial := range []uint32{8, 9} {
		m := askOnce(t, s.Addr().String(),
			packQuery(t, "example.com.", zone.TypeIXFR, withSerial("example.com.", serial)))
		got := transferred([]*wire.Msg{m})
		if want := []string{soaLine(t, z, 8)}; !slices.Equal(got, want) {
			t.Errorf("at serial %d the answer is %v, want %v", serial, got, want)
		}
	}
}

func TestIncrementalTransferWithoutASerialIsAFormatError(t *testing.T) {
	t.Parallel()

	_, snap := ixfrZone(t, 8)
	s := startServer(t, snap, Config{Transfers: loopback})

	m := askOnce(t, s.Addr().String(), packQuery(t, "example.com.", zone.TypeIXFR))
	if m.Rcode != wire.RcodeFormatError {
		t.Errorf("rcode is %s, want FORMERR", wire.RcodeToString[m.Rcode])
	}
}

func TestIncrementalTransferFallsBackToTheWholeZone(t *testing.T) {
	t.Parallel()

	_, snap := ixfrZone(t, 8)
	whole := transferred(func() []*wire.Msg {
		s := startServer(t, snap, Config{Transfers: loopback})
		return askAXFR(t, s.Addr().String(), "example.com.")
	}())

	tests := []struct {
		name string
		cfg  Config
		from uint32
	}{
		{
			name: "nothing supplies a history at all",
			cfg:  Config{Transfers: loopback},
			from: 7,
		},
		{
			name: "the history does not reach back that far",
			cfg:  Config{Transfers: loopback, History: fixedHistory{}},
			from: 2,
		},
		{
			name: "the history could not be read",
			cfg: Config{
				Transfers: loopback,
				History:   fixedHistory{err: errors.New("the database is gone")},
				OnError:   func(error) {},
			},
			from: 7,
		},
		{
			name: "the difference is larger than the zone",
			cfg: Config{Transfers: loopback, History: fixedHistory{held: true, commits: []*journal.Commit{
				commit(t, 7, 8, nil, churn(12)),
			}}},
			from: 7,
		},
		{
			name: "the commits do not join up to the version being served",
			cfg: Config{Transfers: loopback, History: fixedHistory{held: true, commits: []*journal.Commit{
				commit(t, 7, 999, nil, []string{"a.example.com. 300 IN A 192.0.2.1"}),
			}}},
			from: 7,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := startServer(t, snap, tc.cfg)
			got := transferred(askIXFR(t, s.Addr().String(), "example.com.", tc.from))
			if !slices.Equal(got, whole) {
				t.Errorf("the answer is\n\t%v\nwant the whole zone\n\t%v", got, whole)
			}
		})
	}
}

// churn is more added records than the fixture zone holds.
func churn(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("h%d.example.com. 300 IN A 192.0.2.%d", i, i+100)
	}
	return out
}

func TestIncrementalTransferIsRefusedLikeAWholeOne(t *testing.T) {
	t.Parallel()

	_, snap := ixfrZone(t, 8)
	s := startServer(t, snap, Config{})

	m := askOnce(t, s.Addr().String(),
		packQuery(t, "example.com.", zone.TypeIXFR, withSerial("example.com.", 7)))
	if m.Rcode != wire.RcodeRefused {
		t.Errorf("rcode is %s, want REFUSED", wire.RcodeToString[m.Rcode])
	}
}
