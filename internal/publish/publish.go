// Package publish keeps what the query path answers from in step with the
// store.
//
// The data plane holds copies: a snapshot of the zones, the TSIG keys, who may
// transfer, who is told of a change, and the secrets cookies are computed
// under. Each is copied out of the store, never out of the change that
// prompted it, when the server starts and again after every batch that touches
// it. That happens on whichever node applied the batch and whatever caused it
// to, which is why it hangs off the applier rather than off the HTTP handlers
// (docs/decisions/d41-what-follows-applying-a-batch.md).
package publish

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// Snapshots is where the zones are answered from. A *dns.Server is one; the
// wiring wraps it so that the metrics see every swap.
type Snapshots interface {
	Snapshot() *dns.Snapshot
	SetSnapshot(*dns.Snapshot)
}

// TransferList holds who may pull a whole zone. A *dns.Server is one.
type TransferList interface {
	SetTransfers(dns.Transfers)
}

// Keyring holds the TSIG keys. The query path verifies with them and the
// notifier signs with them, so the wiring hands one ring to both.
type Keyring interface {
	SetKeys(dns.Keyring)
}

// NotifyList holds who is told a zone changed, which is also who is asked
// what it holds (docs/decisions/d36-probing-a-secondary.md).
type NotifyList interface {
	SetTargets([]dns.NotifyTarget)
}

// CookieSecrets holds what the server computes its cookies under. A
// *dns.Server is one.
type CookieSecrets interface {
	SetCookieSecrets(dns.CookieSecrets)
}

// Config is what a [Publisher] needs. The store and the snapshots are
// required. Every other copy may be left nil, and is then one that nothing on
// this node holds, which is the position of a test running without a query
// path.
type Config struct {
	Store     store.Store
	Snapshots Snapshots
	Transfers TransferList
	Keyring   Keyring
	Notify    NotifyList
	Cookies   CookieSecrets

	// OnError hears about a copy that could not be rebuilt after a batch. The
	// batch is committed by then and stays so; the copy answers from before
	// it until the next change that touches it, or a restart.
	OnError func(error)
}

// Publisher copies the store into the data plane.
type Publisher struct {
	cfg Config

	// mu puts one publish after another. Rebuilding a zone reads the snapshot
	// in force and installs one with that zone replaced, and two of those
	// interleaved would each install their own, dropping the zone the other
	// had just put in.
	mu sync.Mutex
}

// New returns a publisher. Nothing is copied until [Publisher.Load].
func New(cfg Config) (*Publisher, error) {
	if cfg.Store == nil {
		return nil, errors.New("publish: no store given")
	}
	if cfg.Snapshots == nil {
		return nil, errors.New("publish: no snapshots given")
	}
	return &Publisher{cfg: cfg}, nil
}

// Load copies everything the store holds, replacing what each copy had.
//
// It is what a server does as it starts, and what a node restoring a log
// snapshot will have to do, since that replaces the store wholesale
// (docs/decisions/d30-what-a-log-snapshot-contains.md). Unlike a publish after
// a batch, a failure here is returned: a server that cannot build what it
// answers from has nothing to answer with.
func (p *Publisher) Load(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	snap, err := read(ctx, p.cfg.Store, func(r store.Reader) (*dns.Snapshot, error) {
		return dns.Rebuild(ctx, r)
	})
	if err != nil {
		return fmt.Errorf("build the snapshot to answer from: %w", err)
	}
	p.cfg.Snapshots.SetSnapshot(snap)

	return errors.Join(p.keys(ctx), p.transfers(ctx), p.notify(ctx), p.cookies(ctx))
}

// Applied copies what one batch changed. It is the applier's hook
// ([apply.Options.OnApplied]).
//
// It fails nothing, because the batch it hears about is already committed. A
// copy it cannot rebuild is reported and left as it was.
func (p *Publisher) Applied(ctx context.Context, a apply.Applied) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(a.Zones) > 0 {
		p.report(p.zones(ctx, a.Zones))
	}
	if a.Keys {
		p.report(p.keys(ctx))
	}
	for _, key := range a.Settings {
		switch key {
		case apply.TransferSetting:
			p.report(p.transfers(ctx))
		case apply.NotifySetting:
			p.report(p.notify(ctx))
		case apply.CookieSecretSetting:
			p.report(p.cookies(ctx))
		default:
			// Read while a change is planned, and copied nowhere.
		}
	}
}

// zones rebuilds the zones a batch touched into the snapshot in force.
//
// Before [Publisher.Load] there is no snapshot to keep in step, and the load
// reads every zone the store holds by then, so there is nothing to do.
func (p *Publisher) zones(ctx context.Context, touched []apply.AppliedZone) error {
	next := p.cfg.Snapshots.Snapshot()
	if next == nil {
		return nil
	}
	err := p.cfg.Store.View(ctx, func(r store.Reader) error {
		for _, z := range touched {
			var berr error
			if next, berr = withZone(ctx, next, r, z); berr != nil {
				return berr
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("rebuild the zones a change touched: %w", err)
	}
	p.cfg.Snapshots.SetSnapshot(next)
	return nil
}

// withZone returns the snapshot with one zone as the store now holds it. A zone
// the store no longer holds was deleted, and is dropped by the name the batch
// carried, which is why a batch carries one.
func withZone(
	ctx context.Context, snap *dns.Snapshot, r store.Reader, z apply.AppliedZone,
) (*dns.Snapshot, error) {
	held, err := r.ZoneByID(ctx, z.ID)
	if errors.Is(err, store.ErrNotFound) {
		return snap.WithoutZone(z.Name), nil
	}
	if err != nil {
		return nil, err
	}
	return snap.WithZone(ctx, held, r)
}

// keys copies the TSIG keys that still sign. A withdrawn key is left out, the
// store no longer holding its secret (docs/decisions/d28-tsig.md).
func (p *Publisher) keys(ctx context.Context) error {
	if p.cfg.Keyring == nil {
		return nil
	}
	ring, err := read(ctx, p.cfg.Store, func(r store.Reader) (dns.Keyring, error) {
		keys, lerr := r.ListTSIGKeys(ctx)
		if lerr != nil {
			return nil, lerr
		}
		ring := make(dns.Keyring, len(keys))
		for _, k := range keys {
			if k.Active() {
				ring[k.Name] = dns.TSIGKey{Name: k.Name, Algorithm: k.Algorithm, Secret: k.Secret}
			}
		}
		return ring, nil
	})
	if err != nil {
		return fmt.Errorf("read the TSIG keys: %w", err)
	}
	p.cfg.Keyring.SetKeys(ring)
	return nil
}

// transfers copies who may pull a whole zone.
func (p *Publisher) transfers(ctx context.Context) error {
	if p.cfg.Transfers == nil {
		return nil
	}
	allow, err := read(ctx, p.cfg.Store, func(r store.Reader) (apply.TransferAllow, error) {
		return apply.StoredTransferAllow(ctx, r)
	})
	if err != nil {
		return fmt.Errorf("read who may transfer: %w", err)
	}
	p.cfg.Transfers.SetTransfers(dns.Allow{Prefixes: allow.Prefixes, Keys: allow.Keys})
	return nil
}

// notify copies who is told a zone changed.
func (p *Publisher) notify(ctx context.Context) error {
	if p.cfg.Notify == nil {
		return nil
	}
	stored, err := read(ctx, p.cfg.Store, func(r store.Reader) ([]apply.NotifyTarget, error) {
		return apply.StoredNotifyTargets(ctx, r)
	})
	if err != nil {
		return fmt.Errorf("read who is told of a change: %w", err)
	}
	targets := make([]dns.NotifyTarget, len(stored))
	for i, t := range stored {
		targets[i] = dns.NotifyTarget{Addr: t.Addr, Key: t.Key}
	}
	p.cfg.Notify.SetTargets(targets)
	return nil
}

// cookies copies the secrets cookies are computed under.
func (p *Publisher) cookies(ctx context.Context) error {
	if p.cfg.Cookies == nil {
		return nil
	}
	secrets, err := read(ctx, p.cfg.Store, func(r store.Reader) (apply.CookieSecrets, error) {
		return apply.StoredCookieSecrets(ctx, r)
	})
	if err != nil {
		return fmt.Errorf("read the cookie secrets: %w", err)
	}
	p.cfg.Cookies.SetCookieSecrets(dns.CookieSecrets{
		Current:  dns.CookieSecret(secrets.Current),
		Previous: dns.CookieSecret(secrets.Previous),
	})
	return nil
}

// report hands a copy that could not be brought up to date to the operator.
func (p *Publisher) report(err error) {
	if err == nil || p.cfg.OnError == nil {
		return
	}
	p.cfg.OnError(fmt.Errorf(
		"a change is committed but the query path still answers from a copy made before it; "+
			"the next change to the same thing, or a restart, brings it up to date: %w", err))
}

// read runs one read transaction and returns what fn made of it.
func read[T any](ctx context.Context, st store.Store, fn func(store.Reader) (T, error)) (T, error) {
	var out T
	err := st.View(ctx, func(r store.Reader) error {
		var ferr error
		out, ferr = fn(r)
		return ferr
	})
	return out, err
}
