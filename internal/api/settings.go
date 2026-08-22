package api

import (
	"context"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// GetSettings reports the defaults a zone that says nothing inherits.
func (s *Server) GetSettings(
	ctx context.Context, _ gen.GetSettingsRequestObject,
) (gen.GetSettingsResponseObject, error) {
	var pol apply.Policy
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		pol, verr = s.applier.Policy(ctx, r)
		return verr
	}); err != nil {
		return nil, err
	}
	return gen.GetSettings200JSONResponse(settingsToAPI(pol)), nil
}

// UpdateSettings changes them. A field the request leaves out is left alone,
// so a client may send only what it means to change.
func (s *Server) UpdateSettings(
	ctx context.Context, req gen.UpdateSettingsRequestObject,
) (gen.UpdateSettingsResponseObject, error) {
	var pol apply.Policy
	if err := s.store.Update(ctx, func(tx store.Tx) error {
		if req.Body != nil && req.Body.ReverseConflictPolicy != nil {
			p := apply.Policy(*req.Body.ReverseConflictPolicy)
			if serr := apply.SetStoredPolicy(ctx, tx, p); serr != nil {
				return serr
			}
		}
		// Read back rather than echo what was sent: the response then says what
		// the server holds, including the parts this request did not touch.
		var verr error
		pol, verr = s.applier.Policy(ctx, tx)
		return verr
	}); err != nil {
		return nil, err
	}
	return gen.UpdateSettings200JSONResponse(settingsToAPI(pol)), nil
}

// settingsToAPI renders the settings in force.
func settingsToAPI(pol apply.Policy) gen.Settings {
	return gen.Settings{ReverseConflictPolicy: gen.ReverseConflictPolicy(pol)}
}
