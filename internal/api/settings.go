package api

import (
	"context"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// settings is what a client is told, and what the server holds after a change.
type settings struct {
	policy apply.Policy
	allow  apply.TransferAllow
	notify []apply.NotifyTarget
}

// GetSettings reports the defaults a zone that says nothing inherits.
func (s *Server) GetSettings(
	ctx context.Context, _ gen.GetSettingsRequestObject,
) (gen.GetSettingsResponseObject, error) {
	var cur settings
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		cur, verr = s.settings(ctx, r)
		return verr
	}); err != nil {
		return nil, err
	}
	return gen.GetSettings200JSONResponse(settingsToAPI(cur)), nil
}

// UpdateSettings changes them. A field the request leaves out is left alone,
// so a client may send only what it means to change.
func (s *Server) UpdateSettings(
	ctx context.Context, req gen.UpdateSettingsRequestObject,
) (gen.UpdateSettingsResponseObject, error) {
	var cur settings
	if err := s.store.Update(ctx, func(tx store.Tx) error {
		if req.Body != nil && req.Body.ReverseConflictPolicy != nil {
			p := apply.Policy(*req.Body.ReverseConflictPolicy)
			if serr := apply.SetStoredPolicy(ctx, tx, p); serr != nil {
				return serr
			}
		}
		if req.Body != nil && req.Body.NotifyTargets != nil {
			targets, perr := apply.ParseNotifyTargets(*req.Body.NotifyTargets)
			if perr != nil {
				return perr
			}
			if serr := apply.SetStoredNotifyTargets(ctx, tx, targets); serr != nil {
				return serr
			}
		}
		if req.Body != nil && req.Body.TransferAllow != nil {
			allow, perr := apply.ParseTransferAllow(*req.Body.TransferAllow)
			if perr != nil {
				return perr
			}
			if serr := apply.SetStoredTransferAllow(ctx, tx, allow); serr != nil {
				return serr
			}
		}
		// Read back rather than echo what was sent: the response then says what
		// the server holds, including the parts this request did not touch.
		var verr error
		cur, verr = s.settings(ctx, tx)
		return verr
	}); err != nil {
		return nil, err
	}

	// The list is enforced by the query path, which holds its own copy so a
	// transfer costs no database read. Publishing it here is what makes a
	// change take effect now rather than at the next restart.
	if s.transfers != nil {
		s.transfers.SetTransfers(dns.Allow{Prefixes: cur.allow.Prefixes, Keys: cur.allow.Keys})
	}
	if s.notifier != nil {
		s.notifier.SetTargets(notifyTargets(cur.notify))
	}
	return gen.UpdateSettings200JSONResponse(settingsToAPI(cur)), nil
}

// notifyTargets is the query path's view of the notify list.
func notifyTargets(in []apply.NotifyTarget) []dns.NotifyTarget {
	out := make([]dns.NotifyTarget, len(in))
	for i, t := range in {
		out[i] = dns.NotifyTarget{Addr: t.Addr, Key: t.Key}
	}
	return out
}

// settings reads everything a client asking about them is told.
func (s *Server) settings(ctx context.Context, r store.Reader) (settings, error) {
	pol, err := s.applier.Policy(ctx, r)
	if err != nil {
		return settings{}, err
	}
	allow, err := apply.StoredTransferAllow(ctx, r)
	if err != nil {
		return settings{}, err
	}
	notify, err := apply.StoredNotifyTargets(ctx, r)
	if err != nil {
		return settings{}, err
	}
	return settings{policy: pol, allow: allow, notify: notify}, nil
}

// settingsToAPI renders the settings in force.
func settingsToAPI(cur settings) gen.Settings {
	return gen.Settings{
		ReverseConflictPolicy: gen.ReverseConflictPolicy(cur.policy),
		// An empty list is nobody rather than absent, so a client can tell "no
		// transfers" from "this server did not say".
		TransferAllow: apply.TransferAllowText(cur.allow),
		NotifyTargets: apply.NotifyTargetsText(cur.notify),
	}
}
