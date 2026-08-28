// Package stream carries answered queries to whoever is watching them.
//
// It is the second consumer of the query path's observer (architecture
// §2.9), beside the metrics. A watcher subscribes with a filter, and what it
// gets is a live tail of the exchanges that matched: the question, the answer,
// where it came from and how long it took.
//
// Three properties are load-bearing, and all three come from docs/decisions/
// D9. The filter is applied here, before anything is buffered, so a watcher
// looking at one zone gets a complete stream however busy the rest of the
// server is. When even the filtered stream is faster than a watcher can read,
// the stream samples rather than blocking, and says what ratio it is sampling
// at: a stream that silently drops while looking complete is worse than one
// that admits to showing every fiftieth query. And when a watcher falls behind
// anyway, its oldest events are dropped, never the newest: it is a live view,
// and the answer to "what is happening now" is not last minute's queries.
//
// Nothing here may block the query path. [Hub.Observe] runs on the goroutine
// that read the query.
package stream

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// Defaults for the knobs an [Options] leaves unset.
const (
	// defaultBuffer is how many exchanges a watcher may fall behind by before
	// the oldest are dropped. An event is about a hundred octets, so this is
	// a watcher's worth of memory rather than a decision about it.
	defaultBuffer = 1024

	// defaultMaxRate is how many exchanges a second one watcher is sent before
	// the stream starts sampling. Faster than this is not something a person
	// reads or a browser renders; it is only something a connection carries.
	defaultMaxRate = 200

	// defaultMaxWatchers bounds how many filters the query path evaluates per
	// query. Every watcher costs a filter on the hot path, so this is a bound
	// on what somebody with a credential can make the data plane do: measured
	// with every processor answering at once, one watcher costs 47 ns per
	// query and sixteen cost 200, against 1874 ns for the exchange itself.
	defaultMaxWatchers = 16
)

// ErrTooManyWatchers is returned by [Hub.Subscribe] when the bound is reached.
var ErrTooManyWatchers = errors.New("stream: too many watchers")

// Options configure a [Hub]. The zero value is usable and takes every default.
type Options struct {
	// Buffer is how many exchanges a watcher may fall behind by.
	Buffer int

	// MaxRate is how many exchanges a second a watcher is sent before the
	// stream begins sampling.
	MaxRate int

	// MaxWatchers is how many watchers may exist at once.
	MaxWatchers int
}

// Hub fans exchanges out to the watchers whose filters they match.
type Hub struct {
	opts Options

	// watchers is read once per query and written only when somebody
	// subscribes or leaves, so it is replaced whole rather than locked: a
	// query pays one atomic load, and a subscription that arrives mid-query
	// is seen by the next one.
	watchers atomic.Pointer[[]*Subscription]

	mu sync.Mutex // held only to publish a new watcher list
}

// NewHub returns a hub nobody is watching yet.
func NewHub(opts Options) *Hub {
	if opts.Buffer <= 0 {
		opts.Buffer = defaultBuffer
	}
	if opts.MaxRate <= 0 {
		opts.MaxRate = defaultMaxRate
	}
	if opts.MaxWatchers <= 0 {
		opts.MaxWatchers = defaultMaxWatchers
	}
	return &Hub{opts: opts}
}

// Observe offers one exchange to every watcher. It is what a [dns.Server] is
// given as its observer, and it never blocks.
func (h *Hub) Observe(ev dns.Event) {
	subs := h.watchers.Load()
	if subs == nil {
		return
	}
	for _, s := range *subs {
		s.offer(ev)
	}
}

// Watchers is how many subscriptions are open.
func (h *Hub) Watchers() int {
	subs := h.watchers.Load()
	if subs == nil {
		return 0
	}
	return len(*subs)
}

// Subscribe opens a live tail of the exchanges matching f.
//
// The caller reads from [Subscription.Events] and must call
// [Subscription.Close] when it is done, or the query path goes on evaluating a
// filter for a watcher that left.
func (h *Hub) Subscribe(f Filter) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	current := h.list()
	if len(current) >= h.opts.MaxWatchers {
		return nil, ErrTooManyWatchers
	}

	s := &Subscription{
		hub:     h,
		filter:  f,
		suffix:  f.Name.String(),
		maxRate: h.opts.MaxRate,
		events:  make(chan dns.Event, h.opts.Buffer),
		done:    make(chan struct{}),
	}
	s.ratio.Store(1)
	s.carried.Store(1)

	next := make([]*Subscription, len(current), len(current)+1)
	copy(next, current)
	next = append(next, s)
	h.watchers.Store(&next)
	return s, nil
}

// list is the current watchers, under h.mu.
func (h *Hub) list() []*Subscription {
	if subs := h.watchers.Load(); subs != nil {
		return *subs
	}
	return nil
}

// remove takes a watcher out of the list the query path reads.
func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()

	current := h.list()
	next := make([]*Subscription, 0, len(current))
	for _, other := range current {
		if other != s {
			next = append(next, other)
		}
	}
	h.watchers.Store(&next)
}

// Stats is what a watcher's stream has done so far. It is part of the
// interface rather than a debug detail: a stream that is sampling has to say
// so, or what it shows reads as everything that happened (D9).
type Stats struct {
	// Matched is how many exchanges passed the filter, Sent how many of those
	// reached the watcher.
	Matched uint64
	Sent    uint64

	// Sampled is how many were left out because the filtered stream was
	// faster than MaxRate, and Dropped how many were thrown away because the
	// watcher was not reading fast enough. The two are different failures and
	// a person acts differently on each.
	Sampled uint64
	Dropped uint64

	// Ratio is what the stream is sampling at now: 1 is everything, 50 means
	// one exchange in fifty is being sent.
	Ratio int
}

// Subscription is one watcher's live tail.
type Subscription struct {
	hub     *Hub
	filter  Filter
	suffix  string
	maxRate int

	events chan dns.Event
	done   chan struct{}
	closed atomic.Bool
	once   sync.Once

	matched atomic.Uint64
	sent    atomic.Uint64
	sampled atomic.Uint64
	dropped atomic.Uint64

	// window is the second the rate is being measured over, and count how many
	// exchanges have matched within it.
	//
	// carried is what the second before this one worked out, and ratio what is
	// in force now: the larger of the carried one and what this second has
	// already shown itself to be. Without the second half, a burst arriving
	// into a quiet stream would be sent whole and the sampling would begin a
	// second too late: measured against a real flood, fifty thousand lines
	// before it engaged rather than about a thousand.
	// next is the count at which the next exchange is sent. Spacing rather
	// than "every nth", because the ratio moves while a second is running and
	// a modulo against a moving divisor lets far more through than the ratio
	// it is reporting.
	window  atomic.Int64
	count   atomic.Int64
	next    atomic.Int64
	carried atomic.Int64
	ratio   atomic.Int64
}

// Events is the stream.
//
// It is never closed. The query path sends into it from several goroutines at
// once, and closing a channel somebody may be sending on is a panic waiting
// for the moment a watcher leaves while a query is in flight. [Subscription.Done]
// is what says the stream has ended.
func (s *Subscription) Events() <-chan dns.Event { return s.events }

// Done is closed when the subscription ends. A reader selects on it beside
// [Subscription.Events].
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Stats reports what this stream has done. It may be read at any time.
func (s *Subscription) Stats() Stats {
	return Stats{
		Matched: s.matched.Load(),
		Sent:    s.sent.Load(),
		Sampled: s.sampled.Load(),
		Dropped: s.dropped.Load(),
		Ratio:   int(s.ratio.Load()),
	}
}

// Close ends the subscription. It is safe to call more than once, and safe to
// call while the query path is offering an exchange.
func (s *Subscription) Close() {
	s.once.Do(func() {
		// Marked closed first, so that an offer already on its way finds the
		// flag and returns; whatever slips past it lands in a buffer nobody
		// will read, which costs nothing and cannot panic.
		s.closed.Store(true)
		s.hub.remove(s)
		close(s.done)
	})
}

// offer hands one exchange to this watcher, or accounts for why it did not.
//
// It is called from the query path, on the goroutine that read the query, and
// from every reader at once. Nothing in it waits.
func (s *Subscription) offer(ev dns.Event) {
	if s.closed.Load() || !s.matches(ev) {
		return
	}
	s.matched.Add(1)

	if !s.admit(ev) {
		s.sampled.Add(1)
		return
	}

	select {
	case s.events <- ev:
		s.sent.Add(1)
	default:
		// The watcher is behind. Make room by throwing away the oldest event
		// rather than the newest: this is a live view, and the answer to what
		// is happening now is not the queries from a minute ago.
		//
		// The receive races with the watcher's own, which at worst costs one
		// more event than it had to. A stream that drops is what D9 decided
		// on; which end it drops from is what makes it still a live one.
		select {
		case <-s.events:
			s.dropped.Add(1)
		default:
		}
		select {
		case s.events <- ev:
			s.sent.Add(1)
		default:
			s.dropped.Add(1)
		}
	}
}

// admit reports whether this exchange is one of the ones being sent, and moves
// the sampling window on when the second it belongs to has changed.
//
// The clock is not read: the exchange carries the moment it was read, which is
// what the window is measured in.
func (s *Subscription) admit(ev dns.Event) bool {
	sec := ev.At.Unix()
	if last := s.window.Load(); sec != last && s.window.CompareAndSwap(last, sec) {
		// One goroutine closes the window; the others carry on into the new
		// one. Whoever loses the swap has already counted its event, which is
		// the right way round for a rate.
		s.carried.Store(ratioFor(s.count.Swap(0), s.maxRate))
		s.next.Store(0)
	}

	n := s.count.Add(1)
	ratio := max(s.carried.Load(), ratioFor(n, s.maxRate))
	if s.ratio.Load() != ratio {
		// Stored only when it moves. What the stream is sampling at has to be
		// readable at any moment (D9), and writing the same number on every
		// query would be a contended cache line on the hot path for nothing.
		s.ratio.Store(ratio)
	}
	if ratio <= 1 {
		return true
	}

	// The loop turns only when an exchange is actually due, which under
	// sampling is by definition rare; every other one leaves on the first
	// load.
	for {
		next := s.next.Load()
		if n < next {
			return false
		}
		if s.next.CompareAndSwap(next, n+ratio) {
			return true
		}
	}
}

// ratioFor is what to sample at, given how many exchanges matched in the
// second that just ended.
func ratioFor(matched int64, maxRate int) int64 {
	cap64 := int64(maxRate)
	if cap64 <= 0 || matched <= cap64 {
		return 1
	}
	// Rounded up, so the ratio brings the rate to at most maxRate rather than
	// to just above it.
	return (matched + cap64 - 1) / cap64
}

// Filter selects which exchanges a watcher sees. The zero value matches
// everything.
type Filter struct {
	// Name selects the exchanges asking about a name or anything below it. The
	// zero name matches every question.
	Name zone.Name

	// Types and Rcodes select question types and response codes. An empty list
	// matches every one of them.
	Types  []zone.RRType
	Rcodes []int

	// Client selects the exchanges from a network. The zero prefix matches
	// every address.
	Client netip.Prefix
}

// matches reports whether this watcher asked to see ev.
func (s *Subscription) matches(ev dns.Event) bool {
	if s.suffix != "" && !underName(ev.Name, s.suffix) {
		return false
	}
	if len(s.filter.Types) > 0 && !contains(s.filter.Types, ev.Type) {
		return false
	}
	if len(s.filter.Rcodes) > 0 && !contains(s.filter.Rcodes, ev.Rcode) {
		return false
	}
	if s.filter.Client.IsValid() && !s.filter.Client.Contains(ev.Client.Addr()) {
		return false
	}
	return true
}

func contains[T comparable](list []T, want T) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// underName reports whether the question name lies at or below suffix, with
// both written in the presentation format of RFC 1035 §5.1 and fully
// qualified.
//
// Comparison is case-insensitive (RFC 4343). [strings.EqualFold] is safe to
// use for it even though DNS is an ASCII protocol: a name in presentation form
// holds nothing but printable US-ASCII, because everything else is escaped as
// \DDD on the way out of the wire format.
func underName(name, suffix string) bool {
	if suffix == "." {
		return true
	}
	if len(name) < len(suffix) {
		return false
	}
	at := len(name) - len(suffix)
	if !strings.EqualFold(name[at:], suffix) {
		return false
	}
	if at == 0 {
		return true
	}
	// The octet before the suffix has to be the separator between two labels
	// rather than a dot inside one. A dot written \. is part of a label, and
	// so is one written \\\. — what decides is whether the backslashes in
	// front of it are an even number.
	if name[at-1] != '.' {
		return false
	}
	backslashes := 0
	for i := at - 2; i >= 0 && name[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 0
}
