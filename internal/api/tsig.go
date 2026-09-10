package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// ListTSIGKeys returns every key, revoked ones included.
func (s *Server) ListTSIGKeys(
	ctx context.Context, _ gen.ListTSIGKeysRequestObject,
) (gen.ListTSIGKeysResponseObject, error) {
	if err := requireAdmin(ctx, "listing the transfer keys"); err != nil {
		return nil, err
	}

	var keys []*store.TSIGKey
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		keys, verr = r.ListTSIGKeys(ctx)
		return verr
	}); err != nil {
		return nil, err
	}

	out := make([]gen.TSIGKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, tsigKeyToAPI(k))
	}
	return gen.ListTSIGKeys200JSONResponse(out), nil
}

// CreateTSIGKey stores a key and returns its secret.
func (s *Server) CreateTSIGKey(
	ctx context.Context, req gen.CreateTSIGKeyRequestObject,
) (gen.CreateTSIGKeyResponseObject, error) {
	if err := requireAdmin(ctx, "creating a transfer key"); err != nil {
		return nil, err
	}

	name, err := zone.ParseName(req.Body.Name)
	if err != nil {
		return nil, badRequest(
			"a key is named in domain name syntax, and %q is not (RFC 8945 §4.2)", req.Body.Name)
	}

	alg := zone.HMACSHA256
	if req.Body.Algorithm != nil {
		parsed, aerr := zone.ParseTSIGAlgorithm(string(*req.Body.Algorithm))
		if aerr != nil {
			return nil, badRequest("%s", aerr)
		}
		alg = parsed
	}

	secret, err := tsigSecret(req.Body.Secret, alg)
	if err != nil {
		return nil, err
	}

	// Through the applier: a key is replicated state, and a node that does not
	// have it cannot answer a request signed with it at all (D32).
	key, err := s.applier.CreateKey(ctx, name, alg, secret)
	if err != nil {
		return nil, err
	}

	return gen.CreateTSIGKey201JSONResponse{
		Key:    tsigKeyToAPI(key),
		Secret: base64.StdEncoding.EncodeToString(key.Secret),
	}, nil
}

// ReadTSIGKeySecret returns the secret of one key.
func (s *Server) ReadTSIGKeySecret(
	ctx context.Context, req gen.ReadTSIGKeySecretRequestObject,
) (gen.ReadTSIGKeySecretResponseObject, error) {
	if err := requireAdmin(ctx, "reading a key's secret"); err != nil {
		return nil, err
	}

	var key *store.TSIGKey
	if err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		key, verr = r.TSIGKeyByID(ctx, store.TSIGKeyID(req.KeyId))
		return verr
	}); err != nil {
		return nil, err
	}
	if !key.Active() {
		return nil, conflict("that key was withdrawn, and withdrawing one clears its secret")
	}

	return gen.ReadTSIGKeySecret200JSONResponse{
		Key:    tsigKeyToAPI(key),
		Secret: base64.StdEncoding.EncodeToString(key.Secret),
	}, nil
}

// RevokeTSIGKey withdraws a key and clears its secret.
func (s *Server) RevokeTSIGKey(
	ctx context.Context, req gen.RevokeTSIGKeyRequestObject,
) (gen.RevokeTSIGKeyResponseObject, error) {
	if err := requireAdmin(ctx, "withdrawing a transfer key"); err != nil {
		return nil, err
	}

	if err := s.applier.RevokeKey(ctx, store.TSIGKeyID(req.KeyId)); err != nil {
		return nil, err
	}
	return gen.RevokeTSIGKey204Response{}, nil
}

// tsigSecret is the secret a new key gets: the one the caller sent, or a fresh
// one long enough for the algorithm (RFC 8945 §8).
func tsigSecret(sent *string, alg zone.TSIGAlgorithm) ([]byte, error) {
	if sent == nil {
		secret := make([]byte, alg.SecretBytes())
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("no entropy for a TSIG secret: %w", err)
		}
		return secret, nil
	}

	secret, err := base64.StdEncoding.DecodeString(*sent)
	if err != nil {
		return nil, badRequest("a secret is base64, the way every implementation writes one")
	}
	if len(secret) == 0 {
		return nil, badRequest("a key with no secret signs nothing; leave it out to have one generated")
	}
	// Shorter than the hash output is legal and weaker, so it is said rather
	// than refused: the other end may already have it.
	return secret, nil
}

// tsigKeyToAPI renders a key without its secret.
func tsigKeyToAPI(k *store.TSIGKey) gen.TSIGKey {
	out := gen.TSIGKey{
		Id:        string(k.ID),
		Name:      k.Name.String(),
		Algorithm: gen.TSIGAlgorithm(k.Algorithm),
		CreatedAt: k.CreatedAt,
	}
	if !k.RevokedAt.IsZero() {
		out.RevokedAt = ptr(k.RevokedAt)
	}
	return out
}
