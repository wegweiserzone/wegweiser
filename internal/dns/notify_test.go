package dns

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// secondary is a server that records what it is notified of, and answers only
// once it has been told often enough for the test's purpose.
type secondary struct {
	addr netip.AddrPort

	mu     sync.Mutex
	got    []*wire.Msg
	silent int
}

// newSecondary starts one that ignores its first silent notifications.
func newSecondary(t *testing.T, silent int) *secondary {
	t.Helper()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("the listener reports a %T, want a UDP address", conn.LocalAddr())
	}
	ap := local.AddrPort()
	s := &secondary{addr: netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), silent: silent}

	go func() {
		buf := make([]byte, 512)
		for {
			length, from, rerr := conn.ReadFromUDPAddrPort(buf)
			if rerr != nil {
				return
			}
			m := new(wire.Msg)
			if m.Unpack(buf[:length]) != nil {
				continue
			}

			s.mu.Lock()
			s.got = append(s.got, m)
			answer := len(s.got) > s.silent
			s.mu.Unlock()

			if !answer {
				continue
			}
			reply := m.Copy()
			reply.Response = true
			packed, perr := reply.Pack()
			if perr != nil {
				continue
			}
			if _, werr := conn.WriteToUDPAddrPort(packed, from); werr != nil {
				return
			}
		}
	}()
	return s
}

// seen is how many notifications have arrived.
func (s *secondary) seen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// last is the most recent notification, or nil.
func (s *secondary) last() *wire.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return nil
	}
	return s.got[len(s.got)-1]
}

// startNotifier returns one already sending, stopped when the test ends.
func startNotifier(t *testing.T, cfg NotifyConfig) *Notifier {
	t.Helper()

	// Short enough that a retransmission is a test rather than a wait. The
	// interval RFC 1996 recommends is checked where the default is read.
	if cfg.Interval == 0 {
		cfg.Interval = 10 * time.Millisecond
	}
	n := NewNotifier(cfg)
	if err := n.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := n.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return n
}

// outstanding is how many zones are still waiting to be acknowledged. A test
// asks this rather than counting datagrams against a clock: once a zone has
// dropped out, no further notification for it is possible.
func outstanding(n *Notifier) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.pending)
}

// waitFor polls until cond holds, so a test says what it is waiting for rather
// than how long it guessed that would take.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}

func TestNotifyCarriesTheVersionItAnnounces(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 8)
	sec := newSecondary(t, 0)
	n := startNotifier(t, NotifyConfig{Targets: []NotifyTarget{{Addr: sec.addr}}})

	n.Notify(snap, z.Name)
	waitFor(t, "the secondary has been told", func() bool { return sec.seen() > 0 })

	m := sec.last()
	if m.Opcode != wire.OpcodeNotify {
		t.Errorf("opcode is %d, want NOTIFY (RFC 1996 §4.5)", m.Opcode)
	}
	if !m.Authoritative {
		t.Error("the notification is not authoritative")
	}
	if m.Response {
		t.Error("the notification is marked as a response")
	}
	if len(m.Question) != 1 {
		t.Fatalf("it carries %d questions, want one", len(m.Question))
	}
	if q := m.Question[0]; q.Qtype != wire.TypeSOA || !zone.MustParseName(q.Name).Equal(z.Name) {
		t.Errorf("it asks %s %s, want %s SOA", q.Name, wire.TypeToString[q.Qtype], z.Name)
	}

	// The hint of RFC 1996 §3.7, which is what lets a secondary decide it has
	// nothing to fetch without a round trip.
	if len(m.Answer) != 1 {
		t.Fatalf("the answer section holds %d records, want the start of authority", len(m.Answer))
	}
	soa, ok := m.Answer[0].(*wire.SOA)
	if !ok {
		t.Fatalf("the answer section holds %s, want the start of authority", m.Answer[0])
	}
	if soa.Serial != 8 {
		t.Errorf("it announces serial %d, want 8", soa.Serial)
	}
}

func TestNotifyStopsOnceItIsAnswered(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 8)
	sec := newSecondary(t, 0)
	// An interval no test will wait out, so nothing here settles by giving up:
	// the only way this zone stops being outstanding is the answer.
	n := startNotifier(t, NotifyConfig{
		Targets: []NotifyTarget{{Addr: sec.addr}}, Interval: time.Hour,
	})

	n.Notify(snap, z.Name)
	waitFor(t, "the answer has settled it", func() bool { return outstanding(n) == 0 })
	if got := sec.seen(); got != 1 {
		t.Errorf("the secondary was told %d times, want once", got)
	}
}

func TestNotifyRetriesUntilItIsAnswered(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 8)
	sec := newSecondary(t, 2)
	// More attempts than the test can use up, so that settling means the answer
	// arrived rather than that the retransmissions ran out.
	n := startNotifier(t, NotifyConfig{
		Targets: []NotifyTarget{{Addr: sec.addr}}, Attempts: 1000,
	})

	n.Notify(snap, z.Name)
	waitFor(t, "the two it ignored have been sent again", func() bool { return sec.seen() > 2 })
	waitFor(t, "its answer has settled it", func() bool { return outstanding(n) == 0 })
}

func TestNotifyGivesUpAndSaysSo(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 8)
	// A secondary that never answers. RFC 1996 §3.6 gives up rather than
	// retransmitting for ever.
	sec := newSecondary(t, 1000)

	var (
		mu     sync.Mutex
		faults []error
	)
	n := startNotifier(t, NotifyConfig{
		Targets:  []NotifyTarget{{Addr: sec.addr}},
		Attempts: 3,
		OnError: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			faults = append(faults, err)
		},
	})

	n.Notify(snap, z.Name)
	waitFor(t, "it has given up", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(faults) > 0
	})

	time.Sleep(100 * time.Millisecond)
	if got := sec.seen(); got != 3 {
		t.Errorf("the secondary was told %d times, want the three it was given", got)
	}
}

func TestNotifyReplacesAVersionStillBeingRetried(t *testing.T) {
	t.Parallel()

	// A zonefile import commits many times in a row. What the secondary needs
	// is the serial this server is at, not every serial it passed through.
	z, older := ixfrZone(t, 8)
	_, newer := ixfrZone(t, 9)
	sec := newSecondary(t, 1000)
	n := startNotifier(t, NotifyConfig{
		Targets: []NotifyTarget{{Addr: sec.addr}}, Interval: time.Hour, OnError: func(error) {},
	})

	n.Notify(older, z.Name)
	waitFor(t, "the first version has gone out", func() bool { return sec.seen() > 0 })
	n.Notify(newer, z.Name)
	waitFor(t, "the second has too", func() bool { return sec.seen() > 1 })

	soa, ok := sec.last().Answer[0].(*wire.SOA)
	if !ok {
		t.Fatalf("the answer section holds %s, want the start of authority", sec.last().Answer[0])
	}
	if soa.Serial != 9 {
		t.Errorf("the secondary was last told serial %d, want 9", soa.Serial)
	}
}

func TestNotifyTellsNobodyUntilSomebodyIsNamed(t *testing.T) {
	t.Parallel()

	// The default, and the one that matters (D27).
	z, snap := ixfrZone(t, 8)
	sec := newSecondary(t, 0)
	n := startNotifier(t, NotifyConfig{})

	n.Notify(snap, z.Name)
	time.Sleep(100 * time.Millisecond)
	if got := sec.seen(); got != 0 {
		t.Errorf("a server nobody configured told a secondary %d times", got)
	}

	n.SetTargets([]NotifyTarget{{Addr: sec.addr}})
	n.Notify(snap, z.Name)
	waitFor(t, "the named secondary has been told", func() bool { return sec.seen() > 0 })
}

func TestNotifyOfAZoneThatIsNoLongerHere(t *testing.T) {
	t.Parallel()

	// The zone was deleted, so there is no version to announce.
	_, snap := ixfrZone(t, 8)
	sec := newSecondary(t, 0)
	n := startNotifier(t, NotifyConfig{Targets: []NotifyTarget{{Addr: sec.addr}}})

	n.Notify(snap, zone.MustParseName("gone.example."))
	time.Sleep(100 * time.Millisecond)
	if got := sec.seen(); got != 0 {
		t.Errorf("a zone this server does not hold produced %d notifications", got)
	}
}

func TestNotifyDefaultsToWhatTheRFCRecommends(t *testing.T) {
	t.Parallel()

	// RFC 1996 §3.6, the note beneath it: sixty seconds, five retransmissions.
	n := NewNotifier(NotifyConfig{})
	if n.cfg.Interval != 60*time.Second {
		t.Errorf("the interval is %s, want 60s", n.cfg.Interval)
	}
	if n.cfg.Attempts != 6 {
		t.Errorf("it sends %d times, want one notification and five retransmissions", n.cfg.Attempts)
	}
}

// steps records what a notifier reports, so a test can ask what became of a
// notification rather than infer it.
type steps struct {
	mu  sync.Mutex
	got []NotifyEvent
}

func (s *steps) observe(ev NotifyEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, ev)
}

// count is how many of one outcome have been reported.
func (s *steps) count(outcome NotifyOutcome) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.got {
		if ev.Outcome == outcome {
			n++
		}
	}
	return n
}

func TestNotifyReportsWhatBecameOfIt(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 8)
	answers := newSecondary(t, 0)
	// One that never answers, so both endings are reported from one run.
	silent := newSecondary(t, 1000)

	var seen steps
	n := startNotifier(t, NotifyConfig{
		Targets:  []NotifyTarget{{Addr: answers.addr}, {Addr: silent.addr}},
		Attempts: 2,
		OnError:  func(error) {},
		Observe:  seen.observe,
	})

	n.Notify(snap, z.Name)
	waitFor(t, "both secondaries are done with", func() bool { return outstanding(n) == 0 })

	if got := seen.count(NotifyAnswered); got != 1 {
		t.Errorf("%d secondaries answered, want one", got)
	}
	if got := seen.count(NotifyAbandoned); got != 1 {
		t.Errorf("%d secondaries were given up on, want one", got)
	}
	// One to each, then at least one more to the silent one before it was
	// given up. Not an exact count: whether the answering one takes a
	// retransmission along the way is a race with its own reply, and the
	// reporting is what this test is about.
	if got := seen.count(NotifySent); got < 3 {
		t.Errorf("%d datagrams were reported sent, want at least three", got)
	}
}

func TestNotifyIsSignedWhereTheTargetNamesAKey(t *testing.T) {
	t.Parallel()

	// A secondary that insists on TSIG has to be able to trust the news as
	// well as the transfer (D28).
	z, snap := ixfrZone(t, 8)
	key := testKey("secondary.example.com.", zone.HMACSHA256)
	signed := newSecondary(t, 0)
	plain := newSecondary(t, 0)

	n := startNotifier(t, NotifyConfig{
		Targets: []NotifyTarget{
			{Addr: signed.addr, Key: key.Name},
			{Addr: plain.addr},
		},
		Keys: ring(key),
	})

	n.Notify(snap, z.Name)
	waitFor(t, "both have been told", func() bool {
		return signed.seen() > 0 && plain.seen() > 0
	})

	rr := signed.last().IsTsig()
	if rr == nil {
		t.Fatal("the notification to the keyed secondary is unsigned")
	}
	if rr.Hdr.Name != key.Name.String() {
		t.Errorf("it is signed with %q, want %q", rr.Hdr.Name, key.Name)
	}
	if plain.last().IsTsig() != nil {
		t.Error("the notification to the secondary naming no key is signed")
	}
}

func TestNotifyGoesOutUnsignedWhenTheKeyIsGone(t *testing.T) {
	t.Parallel()

	// A key that has been withdrawn is not a reason to stop telling anybody:
	// a secondary insisting on TSIG ignores it, which is the same as never
	// hearing, and one that does not still gets the news.
	z, snap := ixfrZone(t, 8)
	sec := newSecondary(t, 0)

	var (
		mu     sync.Mutex
		faults []error
	)
	n := startNotifier(t, NotifyConfig{
		Targets: []NotifyTarget{{Addr: sec.addr, Key: zone.MustParseName("gone.example.com.")}},
		Keys:    Keyring{},
		OnError: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			faults = append(faults, err)
		},
	})

	n.Notify(snap, z.Name)
	waitFor(t, "the secondary has been told anyway", func() bool { return sec.seen() > 0 })

	if sec.last().IsTsig() != nil {
		t.Error("it was signed with a key this server does not hold")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(faults) == 0 {
		t.Error("nothing was reported about the missing key")
	}
}
