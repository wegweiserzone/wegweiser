package dns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// What RFC 1996 §3.6 recommends in the note beneath it, taken as written.
const (
	// notifyInterval is how long to wait for a response before sending again.
	notifyInterval = 60 * time.Second

	// notifyAttempts is one notification plus the five retransmissions the RFC
	// allows, after which a secondary that has never answered is left to its
	// own refresh timer.
	notifyAttempts = 6

	// notifyReadBuffer is large enough for a response, which is the request
	// echoed back with the response bit set (RFC 1996 §4.7).
	notifyReadBuffer = 512
)

// NotifyTarget is one secondary told when a zone changes.
type NotifyTarget struct {
	Addr netip.AddrPort
	// Key is the TSIG key the notification is signed with, and is the zero
	// name for one that goes out unsigned (docs/decisions/d28-tsig.md).
	Key zone.Name
}

// NotifyConfig is what a [Notifier] needs. The zero value is usable.
type NotifyConfig struct {
	// Targets are the secondaries told when a zone changes. Empty tells
	// nobody, which is where a server starts (docs/decisions/d27-notify.md).
	Targets []NotifyTarget

	// Keys are the TSIG keys a target may name. One it does not hold sends the
	// notification unsigned and reports it: a secondary insisting on TSIG then
	// ignores the news rather than acting on something forgeable.
	Keys Keyring

	// OnError is called for a notification that could not be sent or that was
	// never answered. Never for a write: a notification that does not arrive
	// leaves the secondary on its refresh timer, which is where it was before
	// any of this existed. May be nil.
	OnError func(error)

	// Observe is called for each step of telling one secondary. It is what
	// feeds the metrics, composed by the wiring the way the query path's is.
	// May be nil, and then nothing is counted.
	Observe func(NotifyEvent)

	// Interval and Attempts override RFC 1996's recommendation. Zero takes it.
	// Tests set them; an operator has no reason to.
	Interval time.Duration
	Attempts int
}

// NotifyOutcome is what happened to one notification.
type NotifyOutcome string

const (
	// NotifySent is one datagram written, retransmissions included.
	NotifySent NotifyOutcome = "sent"
	// NotifyAnswered is a secondary that responded, whatever it responded.
	NotifyAnswered NotifyOutcome = "answered"
	// NotifyAbandoned is a secondary that never did, and has now been left to
	// its own refresh timer.
	NotifyAbandoned NotifyOutcome = "abandoned"
)

// NotifyEvent is one step of telling one secondary about one zone.
type NotifyEvent struct {
	Zone    zone.Name
	Target  netip.AddrPort
	Outcome NotifyOutcome
}

// Notifier tells secondaries that a zone has a new version.
//
// It is the one part of this package that sends rather than answers. A
// notification is started by the control plane after a write and carries only
// what the published snapshot holds, so nothing here is on the path of a query
// (invariant 2).
type Notifier struct {
	cfg NotifyConfig

	// targets is held apart from the config because the list lives in the
	// database and changes while the server runs, like the transfer policy.
	mu      sync.Mutex
	targets []NotifyTarget
	keys    Keyring
	pending map[zone.Name]*notification
	closed  bool

	conn *net.UDPConn
	// wake carries a nudge to the sender, and never blocks: a nudge already
	// waiting says everything a second one would.
	wake chan struct{}
	done chan struct{}
	wg   sync.WaitGroup
}

// notification is one zone's outstanding news, and the secondaries that have
// not acknowledged it yet.
type notification struct {
	// msg is the notification unsigned, which is what an entry naming no key
	// gets. A keyed one is signed from unsigned per target: the digest covers
	// the key, so no two of them are the same bytes.
	msg      []byte
	unsigned *wire.Msg
	id       uint16
	// waiting counts what has been sent to each target so far. A target drops
	// out of the map when it answers or when it has been told often enough.
	waiting map[netip.AddrPort]int
	// keys is the key each target signs with, where it names one.
	keys map[netip.AddrPort]TSIGKey
	next time.Time
}

// NewNotifier returns a notifier that has not opened its socket yet.
func NewNotifier(cfg NotifyConfig) *Notifier {
	if cfg.Interval <= 0 {
		cfg.Interval = notifyInterval
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = notifyAttempts
	}
	return &Notifier{
		cfg:     cfg,
		targets: cfg.Targets,
		keys:    cfg.Keys,
		pending: make(map[zone.Name]*notification),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// Start opens the socket a notification goes out on and begins sending.
//
// The socket is the notifier's own rather than the one queries arrive on: a
// response has to come back to the port the request left from, and that port is
// where every query in flight is being answered.
func (n *Notifier) Start() error {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return fmt.Errorf("dns: open a socket to notify from: %w", err)
	}
	n.conn = conn

	n.wg.Add(2)
	go n.read()
	go n.send()
	return nil
}

// SetTargets publishes who is told, for every notification from here on.
func (n *Notifier) SetTargets(targets []NotifyTarget) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.targets = targets
}

// Notify starts telling the secondaries that a zone has a new version.
//
// It is called with the snapshot queries are already being answered from, never
// with one about to be published: RFC 1996 §4.2 sends a notification once the
// version it announces is being served, and a secondary that came back early
// would be told to fetch a serial this server does not yet have.
//
// A zone the snapshot does not hold has been deleted, and there is no version
// to announce. It returns without blocking; the sending happens elsewhere.
func (n *Notifier) Notify(snap *Snapshot, apex zone.Name) {
	t := snap.zoneAt(apex)
	if t == nil {
		return
	}

	n.mu.Lock()
	if n.closed || len(n.targets) == 0 {
		n.mu.Unlock()
		return
	}
	targets, ring := n.targets, n.keys
	n.mu.Unlock()

	unsigned, id, err := notifyMessage(t)
	if err != nil {
		n.report(err)
		return
	}
	packed, err := unsigned.Pack()
	if err != nil {
		n.report(fmt.Errorf("dns: pack a notification for %s: %w", t.name, err))
		return
	}

	waiting := make(map[netip.AddrPort]int, len(targets))
	keys := make(map[netip.AddrPort]TSIGKey, len(targets))
	for _, target := range targets {
		waiting[target.Addr] = 0
		if target.Key.IsZero() {
			continue
		}
		key, held := ring.Key(target.Key)
		switch {
		case !held:
			// Unsigned rather than not at all: a secondary that insists on TSIG
			// ignores it, which is the same as never hearing, and one that does
			// not still gets the news.
			n.report(fmt.Errorf(
				"%s is told about %s with the key %s, which this server does not hold; "+
					"the notification goes unsigned", target.Addr, apex, target.Key))
		default:
			keys[target.Addr] = key
		}
	}

	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	// A newer version replaces one still being retried rather than queueing
	// behind it. What a secondary needs is the serial this server is at, not
	// every serial it passed through.
	n.pending[apex] = &notification{
		msg: packed, unsigned: unsigned, id: id, waiting: waiting, keys: keys,
	}
	n.mu.Unlock()

	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Close stops sending and waits for the goroutines, or for ctx to give up.
func (n *Notifier) Close(ctx context.Context) error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	close(n.done)
	n.mu.Unlock()

	if n.conn != nil {
		// Closing wakes the reader, which is otherwise sitting in a read with
		// no deadline.
		if err := n.conn.Close(); err != nil {
			return err
		}
	}

	drained := make(chan struct{})
	go func() { n.wg.Wait(); close(drained) }()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dns: the notifier did not stop: %w", ctx.Err())
	}
}

// notifyMessage builds the request for one zone: opcode NOTIFY, authoritative,
// one question for the zone's start of authority (RFC 1996 §4.5), and the start
// of authority itself in the answer section.
//
// §3.7 calls that an unsecure hint, and §3.8 forbids a receiver acting on it
// alone. It is here because a secondary willing to compare it with what it
// holds can decide it has nothing to do without a round trip.
func notifyMessage(t *zoneTree) (msg *wire.Msg, id uint16, err error) {
	soa, err := t.apexSOA()
	if err != nil {
		return nil, 0, err
	}

	// Unpredictable, like any DNS message identifier. It is what says a
	// response belongs to this notification, and a guessable one would let an
	// off-path host end the retransmissions for a secondary that never heard.
	var raw [2]byte
	if _, rerr := rand.Read(raw[:]); rerr != nil {
		return nil, 0, fmt.Errorf("dns: no entropy for a notification identifier: %w", rerr)
	}
	id = binary.BigEndian.Uint16(raw[:])

	m := &wire.Msg{
		MsgHdr: wire.MsgHdr{
			Id: id, Opcode: wire.OpcodeNotify, Authoritative: true,
		},
		Question: []wire.Question{{
			Name: t.name.String(), Qtype: wire.TypeSOA, Qclass: wire.ClassINET,
		}},
		Answer: []wire.RR{soa},
	}
	return m, id, nil
}

// signNotification signs one notification for one secondary.
//
// It works from a copy: signing appends a TSIG record and rewrites the message,
// and the next target has to start from the same unsigned form.
func signNotification(unsigned *wire.Msg, key TSIGKey) ([]byte, error) {
	m := unsigned.Copy()
	m.SetTsig(key.Name.String(), key.Algorithm.String(), tsigFudge, time.Now().Unix())

	packed, _, err := wire.TsigGenerate(m, key.base64Secret(), "", false)
	if err != nil {
		return nil, fmt.Errorf("sign a notification with %s: %w", key.Name, err)
	}
	return packed, nil
}

// send is the one goroutine that writes. It wakes on a new notification and on
// the retransmission of an old one, and does nothing in between.
func (n *Notifier) send() {
	defer n.wg.Done()

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		wait := n.round(time.Now())
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-n.done:
			return
		case <-n.wake:
		case <-timer.C:
		}
	}
}

// round sends everything that is due and reports how long until the next thing
// is. Nothing outstanding waits an hour, which a nudge cuts short.
func (n *Notifier) round(now time.Time) time.Duration {
	type datagram struct {
		msg  []byte
		apex zone.Name
		addr netip.AddrPort
	}
	// A target that drops out, and why. The two reasons read differently: one
	// never answered, the other could not be signed for.
	type dropped struct {
		ev  NotifyEvent
		err error
	}
	var (
		out     []datagram
		giveUps []dropped
	)

	n.mu.Lock()
	wait := time.Hour
	for apex, p := range n.pending {
		if p.next.After(now) {
			wait = min(wait, p.next.Sub(now))
			continue
		}
		for addr, sent := range p.waiting {
			if sent >= n.cfg.Attempts {
				delete(p.waiting, addr)
				giveUps = append(giveUps, dropped{
					ev: NotifyEvent{Zone: apex, Target: addr, Outcome: NotifyAbandoned},
					err: fmt.Errorf(
						"%s was told %d times that %s changed and never answered; it will find "+
							"out when its refresh timer fires", addr, sent, apex),
				})
				continue
			}
			msg := p.msg
			if key, keyed := p.keys[addr]; keyed {
				// Signed per target: the digest covers the key, so no two
				// secondaries get the same bytes. A retransmission is signed
				// afresh, because the time signed is part of what it covers.
				signed, serr := signNotification(p.unsigned, key)
				if serr != nil {
					delete(p.waiting, addr)
					giveUps = append(giveUps, dropped{
						ev:  NotifyEvent{Zone: apex, Target: addr, Outcome: NotifyAbandoned},
						err: fmt.Errorf("notify %s: %w", addr, serr),
					})
					continue
				}
				msg = signed
			}
			out = append(out, datagram{msg: msg, apex: apex, addr: addr})
			p.waiting[addr] = sent + 1
		}
		if len(p.waiting) == 0 {
			delete(n.pending, apex)
			continue
		}
		p.next = now.Add(n.cfg.Interval)
		wait = min(wait, n.cfg.Interval)
	}
	n.mu.Unlock()

	for _, gone := range giveUps {
		n.observe(gone.ev)
		n.report(gone.err)
	}
	for _, d := range out {
		if _, err := n.conn.WriteToUDPAddrPort(d.msg, d.addr); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				n.report(fmt.Errorf("notify %s: %w", d.addr, err))
			}
			continue
		}
		n.observe(NotifyEvent{Zone: d.apex, Target: d.addr, Outcome: NotifySent})
	}
	return wait
}

// read takes the responses. A secondary answers with the request echoed back
// (RFC 1996 §4.7), so the identifier and the address it came from are enough to
// say which notification it settles.
func (n *Notifier) read() {
	defer n.wg.Done()

	buf := make([]byte, notifyReadBuffer)
	for {
		length, from, err := n.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			// Every exit from here is the socket being closed by Close.
			return
		}
		if length < headerLen {
			continue
		}
		n.acknowledge(binary.BigEndian.Uint16(buf[:2]), netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
	}
}

// acknowledge stops retransmitting to one secondary.
//
// A response carrying any rcode counts. The secondary is running, it heard, and
// what it does next is its business: RFC 1996 §3.12 has one that does not
// implement the opcode answer NOTIMP, and telling it a fifth time would not
// change that.
func (n *Notifier) acknowledge(id uint16, from netip.AddrPort) {
	var answered *NotifyEvent

	n.mu.Lock()
	for apex, p := range n.pending {
		if p.id != id {
			continue
		}
		if _, waiting := p.waiting[from]; !waiting {
			continue
		}
		delete(p.waiting, from)
		if len(p.waiting) == 0 {
			delete(n.pending, apex)
		}
		answered = &NotifyEvent{Zone: apex, Target: from, Outcome: NotifyAnswered}
		break
	}
	n.mu.Unlock()

	if answered != nil {
		n.observe(*answered)
	}
}

// observe hands a step to the hook, if there is one.
func (n *Notifier) observe(ev NotifyEvent) {
	if n.cfg.Observe != nil {
		n.cfg.Observe(ev)
	}
}

// SetKeys publishes the keys a target may name, for every notification from
// here on.
func (n *Notifier) SetKeys(ring Keyring) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.keys = ring
}

// report hands a fault to the hook, if there is one.
func (n *Notifier) report(err error) {
	if n.cfg.OnError != nil {
		n.cfg.OnError(err)
	}
}
