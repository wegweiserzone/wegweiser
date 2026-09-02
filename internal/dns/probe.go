package dns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"net"
	"net/netip"
	"sync"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// What a probe waits and how often it asks. D36 fixes these rather than
// offering them: they are not numbers an operator has any information with
// which to choose.
const (
	// probeWait is how long one query has to come back. A few seconds rather
	// than one, because the secondaries D34 writes configuration for are other
	// people's machines and some of them are across a link.
	probeWait = 5 * time.Second

	// probeFloor is the longest a pair goes unasked. It is what catches the
	// secondary that quietly lost a zone it already had, where nothing changed
	// here and so nothing would ever become due.
	probeFloor = time.Hour

	// probeBackoff is how long until the next ask after one that did not find
	// the secondary in step, doubling up to the floor.
	probeBackoff = 30 * time.Second

	// probeBurst is how many queries one pass sends, and probePace how long
	// until the next pass when there is more due than that. A server holding
	// thousands of zones would otherwise ask about all of them in one breath,
	// and the picture is worth having promptly rather than instantly.
	probeBurst = 64
	probePace  = 100 * time.Millisecond

	// probeSlots is how many places in the floor window a pair's first ask can
	// land. Fine enough that a large set spreads evenly, coarse enough that the
	// arithmetic stays whole.
	probeSlots = 1024

	// probeReadBuffer is large enough for an answer holding one start of
	// authority, which is all a probe asks for.
	probeReadBuffer = 1232
)

// Snapshots is where a probe reads the serial this server publishes. A
// [*Server] is one.
type Snapshots interface {
	Snapshot() *Snapshot
}

// ProbeOutcome is what asking one secondary about one zone found.
type ProbeOutcome string

const (
	// ProbeInStep is a secondary holding the serial this server publishes.
	ProbeInStep ProbeOutcome = "in_step"
	// ProbeBehind is one holding an older serial, which is the fault the
	// generated configuration cannot rule out (D34).
	ProbeBehind ProbeOutcome = "behind"
	// ProbeAhead is one holding a newer serial. D2 advances a zone by one per
	// commit here, so a secondary in front of us took its copy from somewhere
	// this server is not.
	ProbeAhead ProbeOutcome = "ahead"
	// ProbeUnordered is the pair RFC 1982 §3.2 leaves undefined, exactly half
	// the space apart. Neither is older, and saying so beats guessing.
	ProbeUnordered ProbeOutcome = "unordered"
	// ProbeSilent is a secondary that did not answer in time.
	ProbeSilent ProbeOutcome = "silent"
	// ProbeNoSerial is one that answered without a start of authority to read,
	// which is what an rcode other than NOERROR looks like from here.
	ProbeNoSerial ProbeOutcome = "no_serial"
)

// ProbeEvent is one answered question about one zone on one secondary.
type ProbeEvent struct {
	Zone    zone.Name
	Target  netip.AddrPort
	Outcome ProbeOutcome

	// Lag is how far behind the secondary is, and is meaningful only for
	// [ProbeBehind]. D2 steps a serial once per commit, so it counts the
	// commits the secondary has yet to see rather than an opaque distance.
	Lag uint32
}

// ProbeConfig is what a [Prober] needs. The zero value is usable, and probes
// nobody: the notify list is where the targets come from and it starts empty.
type ProbeConfig struct {
	// Targets are the secondaries asked. It is the notify list (D27): a
	// secondary worth telling is the one worth asking, and there is no second
	// list to keep in step with the first.
	Targets []NotifyTarget

	// Snapshots reports what this server publishes, which is what "in step"
	// is measured against. A probe compares the secondary with what is being
	// answered now, not with what it was told at the time.
	Snapshots Snapshots

	// OnError is called for a query that could not be sent or built. Never for
	// a secondary that is behind: that is what the probe is for, and it
	// travels as an event rather than as a fault.
	OnError func(error)

	// Observe is called for each answered question, and is what feeds the
	// metrics. May be nil, and then nothing is counted.
	Observe func(ProbeEvent)

	// Forget is called for a pair that has stopped existing, because a target
	// left the notify list or a zone was deleted. A gauge outlives what it
	// describes unless somebody says so. May be nil.
	Forget func(zone.Name, netip.AddrPort)

	// Wait and Floor override the constants above. Tests set them; an operator
	// has no reason to.
	Wait  time.Duration
	Floor time.Duration
}

// Prober asks the secondaries what serial they hold.
//
// Like the notifier beside it, this is the part of the package that sends
// rather than answers, it is driven by the control plane, and nothing here is
// on the path of a query (invariant 2). See docs/decisions/, D36.
type Prober struct {
	cfg ProbeConfig

	mu      sync.Mutex
	targets []NotifyTarget
	pending map[probeKey]*probeState
	closed  bool
	// seed spreads the first ask for each pair across the floor window, so
	// that a server holding many zones does not ask about all of them in the
	// same second every time it restarts.
	seed maphash.Seed

	conn *net.UDPConn
	wake chan struct{}
	done chan struct{}
	wg   sync.WaitGroup
}

// probeKey is one secondary and one zone, which is the unit that is in step or
// is not.
type probeKey struct {
	zone zone.Name
	addr netip.AddrPort
}

// probeState is when to ask about one pair next, and what is outstanding.
type probeState struct {
	due time.Time

	// backoff is how long until the next ask while the secondary is not in
	// step. It doubles up to the floor and is dropped once the serials agree.
	backoff time.Duration

	// id identifies the query in flight and deadline is when it stops being
	// worth waiting for. A zero deadline means nothing is outstanding.
	id       uint16
	deadline time.Time

	// suppressed is set while this zone has a notification outstanding to this
	// secondary. It has been told and has not had its chance yet, so a serial
	// behind ours says nothing (D36).
	suppressed bool
}

// NewProber returns a prober that has not opened its socket yet.
func NewProber(cfg ProbeConfig) *Prober {
	if cfg.Wait <= 0 {
		cfg.Wait = probeWait
	}
	if cfg.Floor <= 0 {
		cfg.Floor = probeFloor
	}
	return &Prober{
		cfg:     cfg,
		targets: cfg.Targets,
		pending: make(map[probeKey]*probeState),
		seed:    maphash.MakeSeed(),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// Start opens the socket probes go out on and begins asking.
//
// The socket is the prober's own, for the reason the notifier's is: an answer
// comes back to the port the question left from, and that port is where every
// query in flight is being answered.
func (p *Prober) Start() error {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return fmt.Errorf("dns: open a socket to probe from: %w", err)
	}
	p.conn = conn

	p.wg.Add(2)
	go p.read()
	go p.ask()
	return nil
}

// SetTargets publishes who is asked, for every probe from here on.
func (p *Prober) SetTargets(targets []NotifyTarget) {
	p.mu.Lock()
	p.targets = targets
	p.mu.Unlock()
	p.nudge()
}

// Notified takes a step of the notifier's work and schedules from it.
//
// A notification going out puts the pair in flight, and the answer to it, or
// giving up on it, is what makes the pair worth asking about: the secondary
// has now had its chance. A zone nobody edits produces no probes this way at
// all, which is what keeps the traffic proportional to how much this server is
// changed rather than to how many zones it holds.
func (p *Prober) Notified(ev NotifyEvent) {
	key := probeKey{zone: ev.Zone, addr: ev.Target}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	st, known := p.pending[key]
	if !known {
		st = &probeState{}
		p.pending[key] = st
	}
	switch ev.Outcome {
	case NotifySent:
		st.suppressed = true
		// What is in flight is about to be out of date whatever it says.
		st.id, st.deadline = 0, time.Time{}
	case NotifyAnswered, NotifyAbandoned:
		st.suppressed = false
		st.backoff = 0
		st.due = time.Now()
	}
	p.mu.Unlock()

	if ev.Outcome != NotifySent {
		p.nudge()
	}
}

// Close stops asking and waits for the goroutines to leave.
func (p *Prober) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.done)
	p.mu.Unlock()

	if p.conn != nil {
		// Closing wakes the reader, which is otherwise sitting in a read with
		// no deadline.
		if err := p.conn.Close(); err != nil {
			return err
		}
	}

	drained := make(chan struct{})
	go func() { p.wg.Wait(); close(drained) }()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dns: the prober did not stop: %w", ctx.Err())
	}
}

// ask runs the sending side, on the shape the notifier's sender has.
func (p *Prober) ask() {
	defer p.wg.Done()

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		wait := p.round(time.Now())
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-p.done:
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// round sends what is due, gives up on what has waited long enough, and reports
// how long until the next thing is due.
func (p *Prober) round(now time.Time) time.Duration {
	type question struct {
		msg  []byte
		key  probeKey
		id   uint16
		when time.Time
	}
	var (
		out     []question
		silent  []ProbeEvent
		gone    []probeKey
		faults  []error
		snap    = p.snapshot()
		nextDue = time.Hour
		held    bool
	)

	p.mu.Lock()
	live := p.reconcile(snap, now, &gone)
	for key, st := range p.pending {
		switch {
		case !live[key] || st.suppressed:
			// Not a pair this server has, or one whose secondary is still
			// being told and has not had its chance yet.
			continue

		case !st.deadline.IsZero() && st.deadline.After(now):
			nextDue = min(nextDue, st.deadline.Sub(now))

		case !st.deadline.IsZero():
			// Waited long enough. A secondary that is genuinely unreachable
			// says so by saying nothing repeatedly, which the backoff spaces
			// out rather than turning into a stream of questions.
			st.id, st.deadline = 0, time.Time{}
			p.retry(st, now)
			nextDue = min(nextDue, st.due.Sub(now))
			silent = append(silent, ProbeEvent{Zone: key.zone, Target: key.addr, Outcome: ProbeSilent})

		case st.due.After(now):
			nextDue = min(nextDue, st.due.Sub(now))

		case len(out) >= probeBurst:
			held = true

		default:
			msg, id, err := probeMessage(key.zone)
			if err != nil {
				faults = append(faults, err)
				st.due = now.Add(p.cfg.Floor)
				continue
			}
			st.id, st.deadline = id, now.Add(p.cfg.Wait)
			out = append(out, question{msg: msg, key: key, id: id, when: now})
			nextDue = min(nextDue, p.cfg.Wait)
		}
	}
	p.mu.Unlock()

	if held {
		nextDue = min(nextDue, probePace)
	}

	for _, key := range gone {
		p.forget(key)
	}
	for _, ev := range silent {
		p.observe(ev)
	}
	for _, err := range faults {
		p.report(err)
	}
	for _, q := range out {
		if _, err := p.conn.WriteToUDPAddrPort(q.msg, q.key.addr); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				p.report(fmt.Errorf("probe %s for %s: %w", q.key.addr, q.key.zone, err))
			}
			p.mu.Lock()
			if st, still := p.pending[q.key]; still && st.id == q.id {
				st.id, st.deadline = 0, time.Time{}
				p.retry(st, q.when)
			}
			p.mu.Unlock()
		}
	}
	return max(nextDue, time.Millisecond)
}

// reconcile brings the pending set in line with the targets and the zones this
// server holds, and reports which pairs are live. A pair that has stopped
// existing is dropped and named, so that whatever is counting it can stop.
//
// Called with the lock held.
func (p *Prober) reconcile(snap *Snapshot, now time.Time, gone *[]probeKey) map[probeKey]bool {
	live := make(map[probeKey]bool, len(p.targets)*snap.Zones())
	for _, t := range p.targets {
		for apex := range snap.zones {
			key := probeKey{zone: apex, addr: t.Addr}
			live[key] = true
			if _, known := p.pending[key]; known {
				continue
			}
			// First sight. Spread it across the floor window rather than
			// asking about every zone in the same second.
			p.pending[key] = &probeState{due: now.Add(p.slot(key, p.cfg.Floor))}
		}
	}
	for key := range p.pending {
		if live[key] {
			continue
		}
		delete(p.pending, key)
		*gone = append(*gone, key)
	}
	return live
}

// slot is where in the window a pair's first ask falls. It is a hash rather
// than a random draw so that a pair lands in the same place for as long as the
// process runs, instead of drifting every time the set is reconciled.
func (p *Prober) slot(key probeKey, window time.Duration) time.Duration {
	var h maphash.Hash
	h.SetSeed(p.seed)
	_, _ = h.WriteString(key.zone.String())
	_, _ = h.WriteString(key.addr.String())
	// A place among a fixed number of them rather than a nanosecond offset, so
	// that the window never has to be read as an unsigned count.
	return window / probeSlots * time.Duration(h.Sum64()%probeSlots)
}

// retry spaces out the next ask about a pair that was not found in step,
// doubling up to the floor. Called with the lock held.
func (p *Prober) retry(st *probeState, now time.Time) {
	switch {
	case st.backoff <= 0:
		st.backoff = probeBackoff
	default:
		st.backoff = min(st.backoff*2, p.cfg.Floor)
	}
	st.due = now.Add(st.backoff)
}

// read takes the answers. A secondary answers a query with the identifier
// echoed back, so that and the address it came from say which question it
// settles.
func (p *Prober) read() {
	defer p.wg.Done()

	buf := make([]byte, probeReadBuffer)
	for {
		length, from, err := p.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			// Every exit from here is the socket being closed by Close.
			return
		}
		if length < headerLen {
			continue
		}
		m := new(wire.Msg)
		if m.Unpack(buf[:length]) != nil {
			continue
		}
		p.answer(m, netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
	}
}

// answer settles one outstanding question against what came back.
func (p *Prober) answer(m *wire.Msg, from netip.AddrPort) {
	if len(m.Question) != 1 {
		return
	}
	apex, err := zone.ParseName(m.Question[0].Name)
	if err != nil {
		return
	}
	key := probeKey{zone: apex, addr: from}

	p.mu.Lock()
	st, waiting := p.pending[key]
	if !waiting || st.deadline.IsZero() || st.id != m.Id {
		p.mu.Unlock()
		return
	}
	st.id, st.deadline = 0, time.Time{}
	suppressed := st.suppressed
	p.mu.Unlock()

	// A notification went out while this was in flight, so whatever the
	// secondary says is about the version before the one it is now fetching.
	if suppressed {
		return
	}

	ev := p.compare(key, theirSerial(m))

	p.mu.Lock()
	st, still := p.pending[key]
	if still {
		if ev.Outcome == ProbeInStep {
			st.backoff = 0
			st.due = time.Now().Add(p.cfg.Floor)
		} else {
			p.retry(st, time.Now())
		}
		// A notification went out while this was being worked out, which is
		// the same reason as above to keep quiet about it.
		suppressed = st.suppressed
	}
	p.mu.Unlock()

	if suppressed {
		return
	}
	p.observe(ev)
	p.nudge()
}

// compare works out where the secondary stands against what this server
// publishes for the zone right now.
func (p *Prober) compare(key probeKey, theirs *zone.Serial) ProbeEvent {
	ev := ProbeEvent{Zone: key.zone, Target: key.addr}
	if theirs == nil {
		ev.Outcome = ProbeNoSerial
		return ev
	}

	ours, ok := p.publishedSerial(key.zone)
	if !ok {
		// The zone went while the question was in flight. The next round
		// drops the pair.
		ev.Outcome = ProbeNoSerial
		return ev
	}

	switch {
	case ours == *theirs:
		ev.Outcome = ProbeInStep
	case !theirs.Comparable(ours):
		ev.Outcome = ProbeUnordered
	case theirs.After(ours):
		ev.Outcome = ProbeAhead
	default:
		ev.Outcome = ProbeBehind
		// Wrapping subtraction, which is the arithmetic RFC 1982 is about and
		// the reason this is not a plain difference of two numbers.
		ev.Lag = ours.Uint32() - theirs.Uint32()
	}
	return ev
}

// publishedSerial is the serial this server answers with for a zone.
func (p *Prober) publishedSerial(apex zone.Name) (zone.Serial, bool) {
	t := p.snapshot().zoneAt(apex)
	if t == nil {
		return zone.Serial{}, false
	}
	rr, err := t.apexSOA()
	if err != nil {
		return zone.Serial{}, false
	}
	soa, ok := rr.(*wire.SOA)
	if !ok {
		return zone.Serial{}, false
	}
	return zone.NewSerial(soa.Serial), true
}

// snapshot is what is being answered from, and never nil: a prober with no
// source of one asks about no zones rather than panicking.
func (p *Prober) snapshot() *Snapshot {
	if p.cfg.Snapshots == nil {
		return &Snapshot{}
	}
	snap := p.cfg.Snapshots.Snapshot()
	if snap == nil {
		return &Snapshot{}
	}
	return snap
}

// theirSerial reads the serial out of an answer, or nil where there is none to
// read. An rcode other than NOERROR arrives here as an answer with nothing in
// it, which is the same thing from the asking end.
func theirSerial(m *wire.Msg) *zone.Serial {
	for _, rr := range m.Answer {
		if soa, ok := rr.(*wire.SOA); ok {
			s := zone.NewSerial(soa.Serial)
			return &s
		}
	}
	return nil
}

// probeMessage builds the question: one query for the zone's start of
// authority, unsigned.
//
// TSIG signs what this server sends a secondary to act on, because a forged
// NOTIFY is worth forging. This is not that. Nothing is asked for beyond what
// the secondary answers anyone, and the configuration D34 generates puts no
// restriction on a query.
func probeMessage(apex zone.Name) (msg []byte, id uint16, err error) {
	// Unpredictable, like any DNS message identifier: it is what says an
	// answer belongs to this question rather than to an off-path host with an
	// opinion about how far behind a secondary is.
	var raw [2]byte
	if _, rerr := rand.Read(raw[:]); rerr != nil {
		return nil, 0, fmt.Errorf("dns: no entropy for a probe identifier: %w", rerr)
	}
	id = binary.BigEndian.Uint16(raw[:])

	// Built by hand rather than with SetQuestion, which mints an identifier of
	// its own and turns the recursion desired bit on. Neither is wanted: the
	// identifier is the one drawn above, and asking a secondary to recurse is
	// asking for something no authoritative server owes (D17).
	m := &wire.Msg{
		MsgHdr:   wire.MsgHdr{Id: id},
		Question: []wire.Question{{Name: apex.String(), Qtype: wire.TypeSOA, Qclass: wire.ClassINET}},
	}

	packed, perr := m.Pack()
	if perr != nil {
		return nil, 0, fmt.Errorf("dns: build a probe for %s: %w", apex, perr)
	}
	return packed, id, nil
}

// nudge wakes the sender without ever blocking: a nudge already waiting says
// everything a second one would.
func (p *Prober) nudge() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// observe hands an answered question to the hook, if there is one.
func (p *Prober) observe(ev ProbeEvent) {
	if p.cfg.Observe != nil {
		p.cfg.Observe(ev)
	}
}

// forget says a pair has stopped existing, if anybody is listening.
func (p *Prober) forget(key probeKey) {
	if p.cfg.Forget != nil {
		p.cfg.Forget(key.zone, key.addr)
	}
}

// report hands a fault to the hook, if there is one.
func (p *Prober) report(err error) {
	if p.cfg.OnError != nil {
		p.cfg.OnError(err)
	}
}
