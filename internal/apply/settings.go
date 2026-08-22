package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// PolicySetting is the key the server-wide reverse conflict policy is stored
// under.
//
// It lives in the database rather than in the configuration file because an
// operator changes it and every client has to be able to reach it
// (docs/decisions.md D11). Reading it inside the write transaction is what
// makes a change take effect on the next write instead of the next restart.
const PolicySetting = "reverse_policy"

// StoredPolicy returns the policy the database holds, or the empty policy when
// nothing has been set.
//
// An unreadable or unknown value is an error rather than a silent fallback: a
// server that quietly ignores the policy an operator asked for is worse than
// one that refuses to write until the value is fixed.
func StoredPolicy(ctx context.Context, r store.Reader) (Policy, error) {
	raw, err := r.Setting(ctx, PolicySetting)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	var p Policy
	if uerr := json.Unmarshal(raw, &p); uerr != nil {
		return "", fmt.Errorf("apply: the stored reverse policy is not readable: %w", uerr)
	}
	if !p.Valid() {
		return "", fmt.Errorf("%w reverse policy %q in the database", zone.ErrInvalid, p)
	}
	return p, nil
}

// SetStoredPolicy records the policy every zone that says nothing about itself
// inherits.
func SetStoredPolicy(ctx context.Context, w store.Writer, p Policy) error {
	if !p.Valid() {
		return fmt.Errorf("%w reverse policy %q", zone.ErrInvalid, p)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("apply: encode the reverse policy: %w", err)
	}
	return w.PutSetting(ctx, PolicySetting, raw)
}

// effectivePolicy is the policy the write in progress runs under: what the
// database says, falling back to the one this applier was built with.
func (a *Applier) effectivePolicy(ctx context.Context, r store.Reader) (Policy, error) {
	p, err := StoredPolicy(ctx, r)
	if err != nil {
		return "", err
	}
	if p == "" {
		return a.policy, nil
	}
	return p, nil
}

// Policy returns the reverse conflict policy in force, which is what a client
// asking to see the settings is told.
func (a *Applier) Policy(ctx context.Context, r store.Reader) (Policy, error) {
	return a.effectivePolicy(ctx, r)
}
