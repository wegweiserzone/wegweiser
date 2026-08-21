package dns

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// The query shapes below are kept apart rather than averaged, because the ones
// that matter for robustness are not the ones that dominate ordinary traffic. A
// name that does not exist, one label below a name that does, is what a
// random-subdomain flood consists of, and it has to stay as cheap as a hit.

// benchSnapshot builds n zones spread over a few top-level domains, each with
// an apex and nothing else: this measures zone selection, not what happens
// inside a zone.
func benchSnapshot(b *testing.B, n int) *Snapshot {
	b.Helper()

	tlds := []string{"com", "net", "org", "de"}
	s := &Snapshot{zones: make(map[zone.Name]*zoneTree, n)}
	for i := range n {
		name := zone.MustParseName(fmt.Sprintf("zone%d.%s.", i, tlds[i%len(tlds)]))
		s.add(name, &zoneTree{name: name, nodes: map[zone.Name]*node{name: {name: name}}})
	}
	return s
}

func BenchmarkZoneFor(b *testing.B) {
	for _, n := range []int{100, 10000} {
		s := benchSnapshot(b, n)

		// The index has to match the top-level domain benchSnapshot gave it, or
		// a "hit" shape quietly measures a miss and the numbers mean nothing.
		// The guard below is there because that has already happened once.
		hit := n/2 - (n/2)%4
		queries := []struct {
			shape string
			qname zone.Name
			want  bool
		}{
			{"shallow-hit", zone.MustParseName(fmt.Sprintf("www.zone%d.com.", hit)), true},
			{"deep-hit", zone.MustParseName(fmt.Sprintf("a.b.c.d.zone%d.net.", hit+1)), true},
			{"miss", zone.MustParseName("www.nothing-we-serve.invalid."), false},
		}

		for _, q := range queries {
			if got := s.zoneFor(q.qname) != nil; got != q.want {
				b.Fatalf("%s: zoneFor(%q) found a zone = %v, want %v", q.shape, q.qname, got, q.want)
			}
			b.Run(fmt.Sprintf("zones=%d/%s", n, q.shape), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					s.zoneFor(q.qname)
				}
			})
		}
	}
}

// benchName is the owner name benchRecords gives its i-th record. Deriving it
// rather than writing it out is what keeps a "hit" shape from being a miss.
func benchName(i int) string {
	return fmt.Sprintf("host%d.rack%d.example.com.", i, i%64)
}

// benchRecords is one zone's worth of address records under a handful of parent
// names, the way a large zone actually looks.
func benchRecords(b *testing.B, n int) (*zone.Zone, source) {
	b.Helper()

	z := &zone.Zone{
		ID:   "01BENCH0000000000000000000",
		Name: zone.MustParseName("example.com."),
		SOA: zone.DefaultSOA(
			zone.MustParseName("ns1.example.com."),
			zone.MustParseName("hostmaster.example.com."),
		),
	}
	recs := make([]*zone.Record, 0, n)
	for i := range n {
		name := zone.MustParseName(benchName(i))
		rec, err := zone.NewRecord(z.ID, name, zone.ClassIN, zone.TypeA, 300,
			fmt.Sprintf("192.0.%d.%d", (i/256)%256, i%256))
		if err != nil {
			b.Fatal(err)
		}
		recs = append(recs, &rec)
	}
	return z, source{records: map[zone.ZoneID][]*zone.Record{z.ID: recs}}
}

func BenchmarkZoneTreeLookup(b *testing.B) {
	z, src := benchRecords(b, 100000)
	tr, err := buildZone(b.Context(), z, src)
	if err != nil {
		b.Fatal(err)
	}

	queries := []struct {
		shape string
		qname zone.Name
		// want is whether the name itself is expected to exist, and empty
		// whether it is expected to be an empty non-terminal.
		want  bool
		empty bool
	}{
		{"exact-hit", zone.MustParseName(benchName(50000)), true, false},
		{"empty-non-terminal", zone.MustParseName("rack7.example.com."), true, true},
		{"flood-miss", zone.MustParseName("nothing.rack7.example.com."), false, false},
		{"deep-miss", zone.MustParseName("a.b.c.d.e.example.com."), false, false},
	}

	for _, q := range queries {
		got := tr.lookup(q.qname)
		if (got.node != nil) != q.want {
			b.Fatalf("%s: %q exists = %v, want %v", q.shape, q.qname, got.node != nil, q.want)
		}
		if q.want && got.node.empty() != q.empty {
			b.Fatalf("%s: %q is an empty non-terminal = %v, want %v",
				q.shape, q.qname, got.node.empty(), q.empty)
		}
		b.Run(q.shape, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tr.lookup(q.qname)
			}
		})
	}
}

// BenchmarkResolve measures whole answers rather than lookups, over a zone of
// the size D12 sizes a single zone for.
//
// Each shape is checked against what it claims to be before it is measured. A
// benchmark that quietly measures a miss where it says "hit" is worse than no
// benchmark, because it reads like evidence.
func BenchmarkResolve(b *testing.B) {
	z, src := benchRecords(b, 100000)
	snap, err := Build(b.Context(), []*zone.Zone{z}, src)
	if err != nil {
		b.Fatal(err)
	}

	shapes := []struct {
		shape string
		qname string
		qtype zone.RRType

		rcode  int
		aa     bool
		answer int
	}{
		{"hit", benchName(50000), zone.TypeA, wire.RcodeSuccess, true, 1},
		{"nodata", benchName(50000), zone.TypeAAAA, wire.RcodeSuccess, true, 0},
		{"empty-non-terminal", "rack7.example.com.", zone.TypeA, wire.RcodeSuccess, true, 0},
		// The shape a random-subdomain flood consists of: the zone is found,
		// the name below it is not.
		{"flood-miss", "nothing.rack7.example.com.", zone.TypeA, wire.RcodeNameError, true, 0},
		{"refused", "www.not-ours.invalid.", zone.TypeA, wire.RcodeRefused, false, 0},
	}

	for _, s := range shapes {
		q := Question{Name: zone.MustParseName(s.qname), Class: zone.ClassIN, Type: s.qtype}

		var a Answer
		snap.Resolve(q, &a)
		switch {
		case a.Rcode != s.rcode:
			b.Fatalf("%s: rcode = %s, want %s", s.shape,
				wire.RcodeToString[a.Rcode], wire.RcodeToString[s.rcode])
		case a.Authoritative != s.aa:
			b.Fatalf("%s: AA = %v, want %v", s.shape, a.Authoritative, s.aa)
		case len(a.Answer) != s.answer:
			b.Fatalf("%s: %d records in the answer, want %d", s.shape, len(a.Answer), s.answer)
		}

		b.Run(s.shape, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				snap.Resolve(q, &a)
			}
		})
	}
}

// BenchmarkResolveAdditional measures the two answers that carry addresses
// along: a referral, which is the whole of a delegation-heavy server's traffic,
// and an MX, which is a large share of everyone else's.
func BenchmarkResolveAdditional(b *testing.B) {
	z := &zone.Zone{
		ID:   "01BENCH0000000000000000001",
		Name: zone.MustParseName("example.com."),
		SOA: zone.DefaultSOA(
			zone.MustParseName("ns1.example.com."),
			zone.MustParseName("hostmaster.example.com."),
		),
	}
	recs := make([]*zone.Record, 0, 6)
	for _, r := range [][4]string{
		{"sub.example.com.", "NS", "ns1.sub.example.com.", ""},
		{"ns1.sub.example.com.", "A", "192.0.2.1", ""},
		{"ns1.sub.example.com.", "AAAA", "2001:db8::1", ""},
		{"mail.example.com.", "MX", "10 mx.example.com.", ""},
		{"mx.example.com.", "A", "192.0.2.2", ""},
		{"mx.example.com.", "AAAA", "2001:db8::2", ""},
	} {
		typ, err := zone.ParseRRType(r[1])
		if err != nil {
			b.Fatal(err)
		}
		rec, err := zone.NewRecord(z.ID, zone.MustParseName(r[0]), zone.ClassIN, typ, 300, r[2])
		if err != nil {
			b.Fatal(err)
		}
		recs = append(recs, &rec)
	}

	snap, err := Build(b.Context(), []*zone.Zone{z},
		source{records: map[zone.ZoneID][]*zone.Record{z.ID: recs}})
	if err != nil {
		b.Fatal(err)
	}

	for _, s := range []struct {
		shape      string
		qname      string
		qtype      zone.RRType
		aa         bool
		answer     int
		additional int
	}{
		{"referral", "www.sub.example.com.", zone.TypeA, false, 0, 2},
		{"mx", "mail.example.com.", zone.TypeMX, true, 1, 2},
	} {
		q := Question{Name: zone.MustParseName(s.qname), Class: zone.ClassIN, Type: s.qtype}

		var a Answer
		snap.Resolve(q, &a)
		switch {
		case a.Authoritative != s.aa:
			b.Fatalf("%s: AA = %v, want %v", s.shape, a.Authoritative, s.aa)
		case len(a.Answer) != s.answer:
			b.Fatalf("%s: %d records in the answer, want %d", s.shape, len(a.Answer), s.answer)
		case len(a.Additional) != s.additional:
			b.Fatalf("%s: %d records in the additional section, want %d",
				s.shape, len(a.Additional), s.additional)
		}

		b.Run(s.shape, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				snap.Resolve(q, &a)
			}
		})
	}
}

// BenchmarkRespond measures the whole query path that a socket sees: bytes in,
// bytes out, over the zone size D12 sizes a single zone for.
//
// This is where the allocation target of D12 is actually decided, because the
// resolver behind it allocates nothing and everything counted here comes from
// parsing the query and packing the response.
func BenchmarkRespond(b *testing.B) {
	z, src := benchRecords(b, 100000)
	snap, err := Build(b.Context(), []*zone.Zone{z}, src)
	if err != nil {
		b.Fatal(err)
	}

	packQ := func(name string, qtype zone.RRType, edns bool) []byte {
		m := new(wire.Msg)
		m.SetQuestion(name, uint16(qtype))
		if edns {
			m.SetEdns0(1232, false)
		}
		packed, err := m.Pack()
		if err != nil {
			b.Fatal(err)
		}
		return packed
	}

	for _, s := range []struct {
		shape string
		query []byte
		tr    Transport

		rcode  int
		answer int
	}{
		{"hit", packQ(benchName(50000), zone.TypeA, false), UDP, wire.RcodeSuccess, 1},
		{"hit-edns", packQ(benchName(50000), zone.TypeA, true), UDP, wire.RcodeSuccess, 1},
		{"hit-tcp", packQ(benchName(50000), zone.TypeA, false), TCP, wire.RcodeSuccess, 1},
		{"hit-0x20", packQ("HoSt50000.RaCk16.ExAmPlE.cOm.", zone.TypeA, false), UDP,
			wire.RcodeSuccess, 1},
		{"flood-miss", packQ("nothing.rack7.example.com.", zone.TypeA, false), UDP,
			wire.RcodeNameError, 0},
		{"refused", packQ("www.not-ours.invalid.", zone.TypeA, false), UDP,
			wire.RcodeRefused, 0},
	} {
		r := NewResponder(DefaultLimits())
		out := make([]byte, wire.MaxMsgSize)

		// The guard is the same one the resolver benchmarks carry: a shape that
		// measures something other than what it is called is not a measurement.
		packed, err := r.Respond(snap, s.query, s.tr, out)
		if err != nil {
			b.Fatalf("%s: %v", s.shape, err)
		}
		got := new(wire.Msg)
		if err := got.Unpack(packed); err != nil {
			b.Fatalf("%s: the response does not parse: %v", s.shape, err)
		}
		switch {
		case got.Rcode != s.rcode:
			b.Fatalf("%s: rcode = %s, want %s", s.shape,
				wire.RcodeToString[got.Rcode], wire.RcodeToString[s.rcode])
		case len(got.Answer) != s.answer:
			b.Fatalf("%s: %d records in the answer, want %d", s.shape, len(got.Answer), s.answer)
		}

		b.Run(s.shape, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Respond(snap, s.query, s.tr, out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBuildZone is the cold-start gate of D12 in miniature: a million
// records in under 30 seconds leaves 30 µs per thousand.
func BenchmarkBuildZone(b *testing.B) {
	for _, n := range []int{1000, 100000} {
		z, src := benchRecords(b, n)
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := buildZone(b.Context(), z, src); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "records/s")
		})
	}
}

// BenchmarkWithZone measures republishing one zone out of many, which is what
// every commit does. The zone itself is tiny here: what is being measured is
// the index rebuild that carries the other zones over.
func BenchmarkWithZone(b *testing.B) {
	for _, n := range []int{100, 10000} {
		s := benchSnapshot(b, n)
		z := &zone.Zone{
			ID:   "01BENCH0000000000000000000",
			Name: zone.MustParseName("zone1.com."),
			SOA: zone.DefaultSOA(
				zone.MustParseName("ns1.example.com."),
				zone.MustParseName("hostmaster.example.com."),
			),
		}
		src := source{}

		b.Run(fmt.Sprintf("zones=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.WithZone(b.Context(), z, src); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// The measurement architecture §2.1 was waiting for: what a query costs
// end to end, over a real socket, against the same query answered by the wire
// library's own server.
//
// Three stacks, one client, one question. "miekg" is that server used the way
// its interface asks to be used: a fresh message per query, written back with
// WriteMsg. "miekg-prepacked" hands it a response that was packed once and
// writes the bytes straight out, which answers nothing but isolates what its
// socket layer costs on its own: a goroutine and a writer for every datagram.

// benchQuery is the question every server stack in this file is asked.
func benchQuery(b *testing.B) []byte {
	b.Helper()

	m := new(wire.Msg)
	m.SetQuestion("www.example.com.", wire.TypeA)
	m.Id = 0x2A2A
	packed, err := m.Pack()
	if err != nil {
		b.Fatal(err)
	}
	return packed
}

// benchExchange drives one server with one client socket, synchronously, so
// that every packet's worth of server work falls inside the measurement.
//
// The shape is checked before the timer starts: a stack that answers REFUSED
// because its zone never loaded would otherwise benchmark as the fastest of
// them all.
func benchExchange(b *testing.B, addr string, query []byte) {
	b.Helper()

	conn, err := net.Dial("udp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Minute)); err != nil {
		b.Fatal(err)
	}

	buf := make([]byte, 4096)
	exchange := func() *wire.Msg {
		if _, err := conn.Write(query); err != nil {
			b.Fatal(err)
		}
		n, err := conn.Read(buf)
		if err != nil {
			b.Fatal(err)
		}
		got := new(wire.Msg)
		if err := got.Unpack(buf[:n]); err != nil {
			b.Fatalf("the response does not parse: %v", err)
		}
		return got
	}

	switch got := exchange(); {
	case got.Rcode != wire.RcodeSuccess:
		b.Fatalf("rcode = %s, want NOERROR", wire.RcodeToString[got.Rcode])
	case !got.Authoritative:
		b.Fatal("AA is not set, so this is not the answer being measured")
	case len(got.Answer) != 2:
		b.Fatalf("%d records in the answer, want 2", len(got.Answer))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := conn.Write(query); err != nil {
			b.Fatal(err)
		}
		if _, err := conn.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}

// benchExchangeParallel is benchExchange with a client per goroutine, each on a
// socket of its own.
//
// This is the half that decides the design rather than describes it. Answering
// a datagram in the reader goroutine wins on latency by not scheduling, but it
// only spreads across cores if the sockets do, so if SO_REUSEPORT did not
// deliver, this is where it would show, as a stack that does not get faster
// with more clients while a goroutine-per-packet one does.
func benchExchangeParallel(b *testing.B, addr string, query []byte) {
	b.Helper()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.Dial("udp", addr)
		if err != nil {
			b.Error(err)
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Minute)); err != nil {
			b.Error(err)
			return
		}

		buf := make([]byte, 4096)
		for pb.Next() {
			if _, err := conn.Write(query); err != nil {
				b.Error(err)
				return
			}
			if _, err := conn.Read(buf); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkServerUDP(b *testing.B) {
	snap := resolveFixture(b)
	query := benchQuery(b)

	// The records the wire library's server answers with, taken from the same
	// snapshot so that all three stacks send the same bytes back.
	var answer Answer
	snap.Resolve(Question{
		Name:  zone.MustParseName("www.example.com."),
		Class: zone.ClassIN,
		Type:  zone.TypeA,
	}, &answer)
	if len(answer.Answer) != 2 {
		b.Fatalf("the fixture answers %d records, want 2", len(answer.Answer))
	}
	records := slices.Clone(answer.Answer)

	b.Run("wegweiser", func(b *testing.B) {
		s := NewServer(Config{Addr: "127.0.0.1:0"})
		s.SetSnapshot(snap)
		if err := s.Start(); err != nil {
			b.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				b.Error(err)
			}
		}()

		b.Run("serial", func(b *testing.B) { benchExchange(b, s.Addr().String(), query) })
		b.Run("parallel", func(b *testing.B) { benchExchangeParallel(b, s.Addr().String(), query) })
	})

	reply := new(wire.Msg)
	prepacked := func() []byte {
		req := new(wire.Msg)
		if err := req.Unpack(query); err != nil {
			b.Fatal(err)
		}
		reply.SetReply(req)
		reply.Authoritative = true
		reply.Answer = records
		out, err := reply.Pack()
		if err != nil {
			b.Fatal(err)
		}
		return out
	}()

	for _, tt := range []struct {
		name    string
		handler wire.HandlerFunc
	}{
		{"miekg", func(w wire.ResponseWriter, req *wire.Msg) {
			m := new(wire.Msg)
			m.SetReply(req)
			m.Authoritative = true
			m.Answer = records
			w.WriteMsg(m) //nolint:errcheck // a benchmark has no error path to take
		}},
		{"miekg-prepacked", func(w wire.ResponseWriter, _ *wire.Msg) {
			w.Write(prepacked) //nolint:errcheck // a benchmark has no error path to take
		}},
	} {
		b.Run(tt.name, func(b *testing.B) {
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			srv := &wire.Server{PacketConn: pc, Handler: tt.handler}
			started := make(chan struct{})
			srv.NotifyStartedFunc = func() { close(started) }
			go func() {
				if err := srv.ActivateAndServe(); err != nil {
					b.Error(err)
				}
			}()
			<-started
			defer srv.Shutdown() //nolint:errcheck // the benchmark is over either way

			addr := pc.LocalAddr().String()
			b.Run("serial", func(b *testing.B) { benchExchange(b, addr, query) })
			b.Run("parallel", func(b *testing.B) { benchExchangeParallel(b, addr, query) })
		})
	}
}

// BenchmarkServerObserved is what watching costs. It is the same exchange as
// BenchmarkServerUDP/wegweiser/serial, with a hook attached that does nothing
// but count, so the difference between the two numbers is the whole price of
// the two clock reads and the call, and nothing an observer of its own does.
//
// It matters because the price is paid on the reader's goroutine, per query,
// whether or not anybody is looking at the result.
func BenchmarkServerObserved(b *testing.B) {
	snap := resolveFixture(b)
	query := benchQuery(b)

	var seen atomic.Int64
	s := NewServer(Config{
		Addr:    "127.0.0.1:0",
		Observe: func(Event) { seen.Add(1) },
	})
	s.SetSnapshot(snap)
	if err := s.Start(); err != nil {
		b.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			b.Error(err)
		}
	}()

	b.Run("serial", func(b *testing.B) { benchExchange(b, s.Addr().String(), query) })
	b.Run("parallel", func(b *testing.B) { benchExchangeParallel(b, s.Addr().String(), query) })

	// Without this, a hook that was never called would benchmark as free.
	if seen.Load() == 0 {
		b.Fatal("nothing was observed, so this benchmark measured the wrong thing")
	}
}

// BenchmarkObserve is the marginal cost of the hook itself, without a socket
// in the way: BenchmarkServerObserved measures it against a round trip that
// is five times larger, where a difference of tens of nanoseconds does not
// show. The two clock reads are most of what is left.
func BenchmarkObserve(b *testing.B) {
	snap := resolveFixture(b)
	query := benchQuery(b)
	from := netip.MustParseAddrPort("192.0.2.1:53210")

	r := NewResponder(DefaultLimits())
	packed, err := r.Respond(snap, query, UDP, make([]byte, wire.MaxMsgSize))
	if err != nil {
		b.Fatalf("respond: %v", err)
	}
	if ev := r.Observed(); ev.Name == "" || ev.Dropped {
		b.Fatalf("the exchange observed as %q dropped=%v, so there is nothing to hand over",
			ev.Name, ev.Dropped)
	}

	var seen int64
	for _, tt := range []struct {
		name    string
		observe func(Event)
	}{
		{"nobody watching", nil},
		{"watching", func(Event) { seen++ }},
	} {
		b.Run(tt.name, func(b *testing.B) {
			s := NewServer(Config{Observe: tt.observe})
			b.ReportAllocs()
			for b.Loop() {
				s.observe(r, UDP, from, s.startedAt(), len(packed))
			}
		})
	}

	if seen == 0 {
		b.Fatal("nothing was observed, so this benchmark measured the wrong thing")
	}
}
