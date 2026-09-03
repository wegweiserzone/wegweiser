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

// holder is a fixed snapshot, standing in for the server a probe reads the
// published serial from.
type holder struct{ snap *Snapshot }

func (h holder) Snapshot() *Snapshot { return h.snap }

// answerer is a secondary that answers a probe with the serial it was given,
// or does not answer at all.
type answerer struct {
	addr netip.AddrPort

	mu     sync.Mutex
	asked  []*wire.Msg
	serial uint32
	mute   bool
}

// newAnswerer starts one holding the given serial. A muted one takes the
// question and says nothing, which is the unreachable secondary.
func newAnswerer(t *testing.T, serial uint32, mute bool) *answerer {
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
	a := &answerer{
		addr:   netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()),
		serial: serial,
		mute:   mute,
	}

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

			a.mu.Lock()
			a.asked = append(a.asked, m)
			serial, mute := a.serial, a.mute
			a.mu.Unlock()

			if mute || len(m.Question) != 1 {
				continue
			}
			reply := new(wire.Msg)
			reply.SetReply(m)
			reply.Authoritative = true
			reply.Answer = []wire.RR{&wire.SOA{
				Hdr: wire.RR_Header{
					Name: m.Question[0].Name, Rrtype: wire.TypeSOA,
					Class: wire.ClassINET, Ttl: 3600,
				},
				Ns: "ns1.example.com.", Mbox: "hostmaster.example.com.",
				Serial: serial, Refresh: 3600, Retry: 600, Expire: 604800, Minttl: 300,
			}}
			packed, perr := reply.Pack()
			if perr != nil {
				continue
			}
			if _, werr := conn.WriteToUDPAddrPort(packed, from); werr != nil {
				return
			}
		}
	}()
	return a
}

// questions is how many probes have arrived.
func (a *answerer) questions() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asked)
}

// probeLog gathers what a prober reports, which is what a test asserts on.
type probeLog struct {
	mu     sync.Mutex
	events []ProbeEvent
	gone   []probeKey
}

func (c *probeLog) observe(ev ProbeEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *probeLog) forget(z zone.Name, addr netip.AddrPort) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gone = append(c.gone, probeKey{zone: z, addr: addr})
}

func (c *probeLog) seen() []ProbeEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ProbeEvent(nil), c.events...)
}

func (c *probeLog) forgotten() []probeKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]probeKey(nil), c.gone...)
}

// startProber returns one already asking, stopped when the test ends.
func startProber(t *testing.T, cfg ProbeConfig) *Prober {
	t.Helper()

	if cfg.Wait == 0 {
		cfg.Wait = 100 * time.Millisecond
	}
	p := NewProber(cfg)
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p
}

// settled waits for the first event about a pair and returns it.
func settled(t *testing.T, c *probeLog) ProbeEvent {
	t.Helper()

	var got ProbeEvent
	waitFor(t, "the probe has been answered", func() bool {
		seen := c.seen()
		if len(seen) == 0 {
			return false
		}
		got = seen[0]
		return true
	})
	return got
}

func TestProbeFindsASecondaryInStep(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 12, false)
	c := new(probeLog)

	// A short floor, so the sweep that D36 makes the backstop is what asks.
	startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Floor:     20 * time.Millisecond,
	})

	ev := settled(t, c)
	if ev.Outcome != ProbeInStep {
		t.Errorf("the secondary is %s, want %s", ev.Outcome, ProbeInStep)
	}
	if !ev.Zone.Equal(z.Name) {
		t.Errorf("the event names %s, want %s", ev.Zone, z.Name)
	}
	if ev.Target != sec.addr {
		t.Errorf("the event names %s, want %s", ev.Target, sec.addr)
	}
	if ev.Lag != 0 {
		t.Errorf("a secondary in step is reported %d behind, want 0", ev.Lag)
	}
}

func TestProbeReportsHowFarBehind(t *testing.T) {
	t.Parallel()

	// D2 advances a zone by one per commit, so the distance is the number of
	// commits the secondary has yet to see.
	_, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 9, false)
	c := new(probeLog)

	startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Floor:     20 * time.Millisecond,
	})

	ev := settled(t, c)
	if ev.Outcome != ProbeBehind {
		t.Fatalf("the secondary is %s, want %s", ev.Outcome, ProbeBehind)
	}
	if ev.Lag != 3 {
		t.Errorf("it is reported %d commits behind, want 3", ev.Lag)
	}
}

func TestProbeCountsTheLagAcrossTheSerialWrap(t *testing.T) {
	t.Parallel()

	// RFC 1982 arithmetic: 1 is newer than 4294967295, and the distance
	// between them is two commits, not four billion.
	_, snap := ixfrZone(t, 1)
	sec := newAnswerer(t, 4294967295, false)
	c := new(probeLog)

	startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Floor:     20 * time.Millisecond,
	})

	ev := settled(t, c)
	if ev.Outcome != ProbeBehind {
		t.Fatalf("the secondary is %s, want %s", ev.Outcome, ProbeBehind)
	}
	if ev.Lag != 2 {
		t.Errorf("it is reported %d commits behind, want 2", ev.Lag)
	}
}

func TestProbeReportsASecondaryThatDoesNotAnswer(t *testing.T) {
	t.Parallel()

	_, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 12, true)
	c := new(probeLog)

	startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Wait:      20 * time.Millisecond,
		Floor:     20 * time.Millisecond,
	})

	ev := settled(t, c)
	if ev.Outcome != ProbeSilent {
		t.Errorf("the secondary is %s, want %s", ev.Outcome, ProbeSilent)
	}
	if sec.questions() == 0 {
		t.Error("nothing was asked")
	}
}

func TestProbeSaysNothingWhileANotificationIsOutstanding(t *testing.T) {
	t.Parallel()

	// The secondary is behind, and is behind for the ordinary reason: it has
	// just been told and has not fetched yet. D36 keeps that out of the report.
	z, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 11, false)
	c := new(probeLog)

	// An hour's floor, so the only thing that can schedule a probe here is the
	// notification finishing.
	p := startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Floor:     time.Hour,
	})

	p.Notified(NotifyEvent{Zone: z.Name, Target: sec.addr, Outcome: NotifySent})
	time.Sleep(50 * time.Millisecond)
	if got := c.seen(); len(got) != 0 {
		t.Fatalf("reported %v while the secondary was still being told, want nothing", got)
	}

	// Once it has answered the notification it has had its chance, and what it
	// holds is worth reporting.
	p.Notified(NotifyEvent{Zone: z.Name, Target: sec.addr, Outcome: NotifyAnswered})

	ev := settled(t, c)
	if ev.Outcome != ProbeBehind {
		t.Errorf("the secondary is %s, want %s", ev.Outcome, ProbeBehind)
	}
}

func TestProbeAsksAfterTheNotifierGivesUp(t *testing.T) {
	t.Parallel()

	// Abandoned counts as having had its chance too: a secondary that never
	// answered the notification is exactly the one worth asking about.
	z, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 12, false)
	c := new(probeLog)

	p := startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Floor:     time.Hour,
	})

	p.Notified(NotifyEvent{Zone: z.Name, Target: sec.addr, Outcome: NotifyAbandoned})

	if ev := settled(t, c); ev.Outcome != ProbeInStep {
		t.Errorf("the secondary is %s, want %s", ev.Outcome, ProbeInStep)
	}
}

func TestProbeForgetsAPairThatStopsExisting(t *testing.T) {
	t.Parallel()

	// A gauge outlives what it describes unless somebody says so, and a target
	// dropped from the notify list is the ordinary way that happens.
	z, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 12, false)
	c := new(probeLog)

	p := startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Forget:    c.forget,
		Floor:     20 * time.Millisecond,
	})

	settled(t, c)
	p.SetTargets(nil)

	waitFor(t, "the pair is forgotten", func() bool { return len(c.forgotten()) > 0 })
	gone := c.forgotten()[0]
	if !gone.zone.Equal(z.Name) || gone.addr != sec.addr {
		t.Errorf("forgot %s at %s, want %s at %s", gone.zone, gone.addr, z.Name, sec.addr)
	}
}

func TestProbeAsksForTheStartOfAuthorityWithoutRecursion(t *testing.T) {
	t.Parallel()

	apex := zone.MustParseName("example.com.")
	packed, id, err := probeMessage(apex)
	if err != nil {
		t.Fatalf("probeMessage: %v", err)
	}

	m := new(wire.Msg)
	if uerr := m.Unpack(packed); uerr != nil {
		t.Fatalf("unpack: %v", uerr)
	}
	if m.Id != id {
		t.Errorf("the message carries id %d, want the one reported, %d", m.Id, id)
	}
	if m.Opcode != wire.OpcodeQuery {
		t.Errorf("opcode is %d, want QUERY", m.Opcode)
	}
	if m.Response {
		t.Error("the probe is marked as a response")
	}
	// This server does not resolve and does not ask anyone else to (D17).
	if m.RecursionDesired {
		t.Error("the probe asks for recursion")
	}
	if len(m.Question) != 1 {
		t.Fatalf("it carries %d questions, want one", len(m.Question))
	}
	if q := m.Question[0]; q.Qtype != wire.TypeSOA || !zone.MustParseName(q.Name).Equal(apex) {
		t.Errorf("it asks %s %s, want %s SOA", q.Name, wire.TypeToString[q.Qtype], apex)
	}
}

func TestProbeStandingReportsWhatWasFound(t *testing.T) {
	t.Parallel()

	z, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 9, false)
	c := new(probeLog)

	p := startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Observe:   c.observe,
		Floor:     20 * time.Millisecond,
	})

	settled(t, c)
	standing := p.Standing()
	if len(standing) != 1 {
		t.Fatalf("Standing reports %d pairs, want one", len(standing))
	}

	got := standing[0]
	if !got.Zone.Equal(z.Name) || got.Target != sec.addr {
		t.Errorf("it reports %s at %s, want %s at %s", got.Zone, got.Target, z.Name, sec.addr)
	}
	if got.Outcome != ProbeBehind {
		t.Errorf("the outcome is %s, want %s", got.Outcome, ProbeBehind)
	}
	if !got.Known || got.Serial != zone.NewSerial(9) {
		t.Errorf("it holds serial %v (known %v), want 9", got.Serial, got.Known)
	}
	if got.Lag != 3 {
		t.Errorf("it is %d commits behind, want 3", got.Lag)
	}
	if got.At.IsZero() {
		t.Error("the reading carries no time")
	}
}

// A pair the sweep has scheduled but not yet asked about is a state of its own,
// and reporting it as in step would be the one lie this feature exists to stop.
func TestProbeStandingSaysWhenNothingHasBeenAskedYet(t *testing.T) {
	t.Parallel()

	_, snap := ixfrZone(t, 12)
	sec := newAnswerer(t, 12, false)

	p := startProber(t, ProbeConfig{
		Targets:   []NotifyTarget{{Addr: sec.addr}},
		Snapshots: holder{snap},
		Floor:     time.Hour,
	})

	waitFor(t, "the pair is known", func() bool { return len(p.Standing()) == 1 })
	if got := p.Standing()[0]; got.Outcome != "" || got.Known {
		t.Errorf("an unasked pair reports outcome %q (known %v), want neither", got.Outcome, got.Known)
	}
}
