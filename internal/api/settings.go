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
	changes, err := settingChanges(req.Body)
	if err != nil {
		return nil, err
	}
	// Through the applier, not into the store: a setting is read while a
	// change is planned, so it is replicated state and travels in a batch like
	// everything else this path writes (D32).
	if len(changes) > 0 {
		if serr := s.applier.SetSettings(ctx, changes); serr != nil {
			return nil, serr
		}
	}

	// Read back rather than echo what was sent: the response then says what
	// the server holds, including the parts this request did not touch.
	var cur settings
	if verr := s.store.View(ctx, func(r store.Reader) error {
		var rerr error
		cur, rerr = s.settings(ctx, r)
		return rerr
	}); verr != nil {
		return nil, verr
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

// settingChanges turns what a request asked for into the changes that carry it
// out. A field the request leaves out produces none, so it is left alone.
func settingChanges(body *gen.UpdateSettings) ([]apply.SettingChange, error) {
	if body == nil {
		return nil, nil
	}

	var out []apply.SettingChange
	if body.ReverseConflictPolicy != nil {
		c, err := apply.PolicyChange(apply.Policy(*body.ReverseConflictPolicy))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if body.NotifyTargets != nil {
		targets, err := apply.ParseNotifyTargets(*body.NotifyTargets)
		if err != nil {
			return nil, err
		}
		c, cerr := apply.NotifyTargetsChange(targets)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, c)
	}
	if body.TransferAllow != nil {
		allow, err := apply.ParseTransferAllow(*body.TransferAllow)
		if err != nil {
			return nil, err
		}
		c, cerr := apply.TransferAllowChange(allow)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, c)
	}
	return out, nil
}
