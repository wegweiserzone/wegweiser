package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/store"
)

// tokenFlushInterval is how often the tokens seen since the last flush are
// written back.
const tokenFlushInterval = 30 * time.Second

// tokenUse collects which tokens authenticated a request, so that the write
// recording it happens once for all of them instead of once per request.
type tokenUse struct {
	mu      sync.Mutex
	pending map[store.TokenID]time.Time
}

func newTokenUse() *tokenUse {
	return &tokenUse{pending: make(map[store.TokenID]time.Time)}
}

// record notes that a token was used. Only the latest moment is kept: what is
// written is when a token was last used, so the ones in between are the same
// fact said again.
func (u *tokenUse) record(id store.TokenID, at time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if prev, seen := u.pending[id]; !seen || at.After(prev) {
		u.pending[id] = at
	}
}

// drain takes what has accumulated and starts again.
func (u *tokenUse) drain() map[store.TokenID]time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(u.pending) == 0 {
		return nil
	}
	out := u.pending
	u.pending = make(map[store.TokenID]time.Time, len(out))
	return out
}

// flushTokenUse writes what has accumulated, in one transaction.
func (s *Server) flushTokenUse(ctx context.Context) {
	seen := s.tokenUse.drain()
	if len(seen) == 0 {
		return
	}

	err := s.store.Update(ctx, func(tx store.Tx) error {
		for id, at := range seen {
			if terr := tx.TouchToken(ctx, id, at); terr != nil {
				return terr
			}
		}
		return nil
	})
	if err != nil {
		s.report(fmt.Errorf("record when tokens were last used: %w", err))
	}
}

// recordUse runs the flush loop until the server is closed.
func (s *Server) recordUse() {
	defer s.wg.Done()

	tick := time.NewTicker(tokenFlushInterval)
	defer tick.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			// Its own context: the requests that produced these are long over,
			// and the flush must not inherit a cancellation from any of them.
			ctx, cancel := context.WithTimeout(context.Background(), tokenFlushInterval)
			s.flushTokenUse(ctx)
			cancel()
		}
	}
}

// Close stops the background work the server does and writes out what it was
// holding.
//
// It is safe to call more than once, and a server that is closed still serves:
// the handler does not depend on any of this, so a request arriving during
// shutdown is answered rather than dropped.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wg.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), tokenFlushInterval)
		defer cancel()
		s.flushTokenUse(ctx)
	})
	return nil
}
