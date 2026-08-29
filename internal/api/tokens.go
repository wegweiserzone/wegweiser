package api

import (
	"context"
	"net/http"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// ListTokens returns every token, revoked and expired ones included.
func (s *Server) ListTokens(
	ctx context.Context, _ gen.ListTokensRequestObject,
) (gen.ListTokensResponseObject, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}

	var toks []*store.Token
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		toks, verr = r.ListTokens(ctx)
		return verr
	}); err != nil {
		return nil, err
	}

	out := make([]gen.Token, 0, len(toks))
	for _, t := range toks {
		out = append(out, tokenToAPI(t))
	}
	return gen.ListTokens200JSONResponse(out), nil
}

// CreateToken mints a token and shows its secret for the only time.
func (s *Server) CreateToken(
	ctx context.Context, req gen.CreateTokenRequestObject,
) (gen.CreateTokenResponseObject, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if req.Body.Name == "" {
		return nil, badRequest("a token needs a name, so that it can be told apart later")
	}
	if len(req.Body.Scopes) == 0 {
		return nil, badRequest("a token with no scopes could not do anything")
	}

	scopes := make([]Scope, 0, len(req.Body.Scopes))
	for _, sc := range req.Body.Scopes {
		scope := Scope(sc)
		if rank(scope) == 0 {
			return nil, badRequest("%q is not a scope", sc)
		}
		scopes = append(scopes, scope)
	}

	secret, tok, err := MintToken(req.Body.Name, scopes, s.now())
	if err != nil {
		return nil, err
	}
	if req.Body.ExpiresAt != nil {
		if !req.Body.ExpiresAt.After(s.now()) {
			return nil, badRequest("a token expiring at %s would already be unusable",
				req.Body.ExpiresAt.Format(time.RFC3339))
		}
		tok.ExpiresAt = *req.Body.ExpiresAt
	}

	// Through the applier: a token created on one node has to authenticate on
	// every node, or the API works for some requests and not others (D32).
	if err := s.applier.CreateToken(ctx, &tok); err != nil {
		return nil, err
	}

	return gen.CreateToken201JSONResponse{
		Token:  tokenToAPI(&tok),
		Secret: secret,
	}, nil
}

// RevokeToken withdraws a token.
func (s *Server) RevokeToken(
	ctx context.Context, req gen.RevokeTokenRequestObject,
) (gen.RevokeTokenResponseObject, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}

	// The guard runs inside the plan's own read, so the check and the write it
	// permits cannot be separated by another revocation.
	now := s.now()
	if err := s.applier.RevokeToken(ctx, store.TokenID(req.TokenId),
		func(toks []*store.Token) error {
			return checkNotLastAdmin(toks, store.TokenID(req.TokenId), now)
		}); err != nil {
		return nil, err
	}
	return gen.RevokeToken204Response{}, nil
}

// checkNotLastAdmin refuses to withdraw the last credential that could still
// administer the server.
func checkNotLastAdmin(toks []*store.Token, revoking store.TokenID, now time.Time) error {
	var target *store.Token
	remaining := 0
	for _, t := range toks {
		if t.ID == revoking {
			target = t
		}
		if t.ID == revoking || !t.Active(now) || !hasAdmin(t) {
			continue
		}
		remaining++
	}
	if target == nil {
		return store.ErrNotFound
	}
	if hasAdmin(target) && target.Active(now) && remaining == 0 {
		return conflict("this is the only token that can still administer the server; " +
			"mint another one with the admin scope before withdrawing it")
	}
	return nil
}

// hasAdmin reports whether a token carries the administrator scope.
func hasAdmin(t *store.Token) bool {
	for _, sc := range t.Scopes {
		if Scope(sc) == ScopeAdmin {
			return true
		}
	}
	return false
}

// requireAdmin refuses a caller that may write but may not administer.
func requireAdmin(ctx context.Context) error {
	sub := subjectOf(ctx)
	if sub == nil || !sub.allows(ScopeAdmin) {
		return &apiError{
			status: http.StatusForbidden,
			kind:   typeForbidden,
			title:  "Not allowed",
			detail: "managing tokens needs the \"admin\" scope",
		}
	}
	return nil
}

// tokenToAPI renders a token. There is no branch here that could render the
// secret, because the server does not hold it.
func tokenToAPI(t *store.Token) gen.Token {
	out := gen.Token{
		Id:        string(t.ID),
		Name:      t.Name,
		Prefix:    t.Prefix,
		Scopes:    make([]gen.Scope, 0, len(t.Scopes)),
		CreatedAt: t.CreatedAt,
	}
	for _, sc := range t.Scopes {
		out.Scopes = append(out.Scopes, gen.Scope(sc))
	}
	if !t.LastUsedAt.IsZero() {
		out.LastUsedAt = ptr(t.LastUsedAt)
	}
	if !t.ExpiresAt.IsZero() {
		out.ExpiresAt = ptr(t.ExpiresAt)
	}
	if !t.RevokedAt.IsZero() {
		out.RevokedAt = ptr(t.RevokedAt)
	}
	return out
}
