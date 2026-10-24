package api

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// oidcCallbackPath is where a provider sends a person back after signing in.
// Every redirect URI a provider lists ends in it.
const oidcCallbackPath = "/auth/oidc/callback"

// ListIdentityProviders returns every provider, without secrets.
func (s *Server) ListIdentityProviders(
	ctx context.Context, _ gen.ListIdentityProvidersRequestObject,
) (gen.ListIdentityProvidersResponseObject, error) {
	if err := requireAdmin(ctx, "listing the identity providers"); err != nil {
		return nil, err
	}
	list, err := s.identityProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gen.IdentityProvider, 0, len(list))
	for i := range list {
		out = append(out, providerToAPI(&list[i]))
	}
	return gen.ListIdentityProviders200JSONResponse(out), nil
}

// CreateIdentityProvider adds a provider.
func (s *Server) CreateIdentityProvider(
	ctx context.Context, req gen.CreateIdentityProviderRequestObject,
) (gen.CreateIdentityProviderResponseObject, error) {
	if err := requireAdmin(ctx, "adding an identity provider"); err != nil {
		return nil, err
	}
	p, err := providerFromAPI(req.Body)
	if err != nil {
		return nil, err
	}
	made, err := s.applier.CreateIdentityProvider(ctx, p)
	if err != nil {
		return nil, err
	}
	return gen.CreateIdentityProvider201JSONResponse(providerToAPI(made)), nil
}

// GetIdentityProvider returns one provider, without its secret.
func (s *Server) GetIdentityProvider(
	ctx context.Context, req gen.GetIdentityProviderRequestObject,
) (gen.GetIdentityProviderResponseObject, error) {
	if err := requireAdmin(ctx, "reading an identity provider"); err != nil {
		return nil, err
	}
	list, err := s.identityProviders(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list, func(p apply.IdentityProvider) bool { return p.ID == req.ProviderId })
	if i < 0 {
		return nil, fmt.Errorf("%w: there is no identity provider %s", store.ErrNotFound, req.ProviderId)
	}
	return gen.GetIdentityProvider200JSONResponse(providerToAPI(&list[i])), nil
}

// UpdateIdentityProvider replaces what a provider says about itself.
func (s *Server) UpdateIdentityProvider(
	ctx context.Context, req gen.UpdateIdentityProviderRequestObject,
) (gen.UpdateIdentityProviderResponseObject, error) {
	if err := requireAdmin(ctx, "changing an identity provider"); err != nil {
		return nil, err
	}
	p, err := providerFromAPI(req.Body)
	if err != nil {
		return nil, err
	}
	made, err := s.applier.UpdateIdentityProvider(ctx, req.ProviderId, p)
	if err != nil {
		return nil, err
	}
	return gen.UpdateIdentityProvider200JSONResponse(providerToAPI(made)), nil
}

// DeleteIdentityProvider removes a provider.
func (s *Server) DeleteIdentityProvider(
	ctx context.Context, req gen.DeleteIdentityProviderRequestObject,
) (gen.DeleteIdentityProviderResponseObject, error) {
	if err := requireAdmin(ctx, "removing an identity provider"); err != nil {
		return nil, err
	}
	if err := s.applier.DeleteIdentityProvider(ctx, req.ProviderId); err != nil {
		return nil, err
	}
	return gen.DeleteIdentityProvider204Response{}, nil
}

// identityProviders reads the providers the store holds.
func (s *Server) identityProviders(ctx context.Context) ([]apply.IdentityProvider, error) {
	var list []apply.IdentityProvider
	err := s.store.View(ctx, func(r store.Reader) error {
		var verr error
		list, verr = apply.StoredIdentityProviders(ctx, r)
		return verr
	})
	return list, err
}

// providerFromAPI reads a provider as a client sent it. What the applier
// checks is checked there; what is the API's vocabulary is checked here: the
// scopes, and the path a provider sends a person back to.
func providerFromAPI(in *gen.IdentityProviderInput) (apply.IdentityProvider, error) {
	if in == nil {
		return apply.IdentityProvider{}, badRequest("a provider has to be sent")
	}
	p := apply.IdentityProvider{
		Name:          strings.TrimSpace(in.Name),
		Kind:          apply.ProviderKind(in.Kind),
		Enabled:       in.Enabled == nil || *in.Enabled,
		CACertificate: deref(in.CaCertificate, ""),
	}
	if in.Mappings != nil {
		for _, m := range *in.Mappings {
			if rank(Scope(m.Scope)) == 0 {
				return apply.IdentityProvider{}, badRequest("%q is not a scope", m.Scope)
			}
			p.Mappings = append(p.Mappings, apply.GroupMapping{Group: strings.TrimSpace(m.Group), Scope: string(m.Scope)})
		}
	}
	if o := in.Oidc; o != nil {
		for _, raw := range o.RedirectUris {
			if u, err := url.Parse(raw); err == nil && !strings.HasSuffix(u.Path, basePath+oidcCallbackPath) {
				return apply.IdentityProvider{}, badRequest(
					"the redirect URI %q has to end in %s, which is where a person comes back to",
					raw, basePath+oidcCallbackPath)
			}
		}
		p.OIDC = &apply.OIDCProvider{
			Issuer:       strings.TrimSpace(o.Issuer),
			ClientID:     o.ClientId,
			ClientSecret: deref(o.ClientSecret, ""),
			GroupsClaim:  deref(o.GroupsClaim, ""),
			NameClaim:    deref(o.NameClaim, ""),
			RedirectURIs: o.RedirectUris,
		}
		if o.Scopes != nil {
			p.OIDC.Scopes = *o.Scopes
		}
	}
	if l := in.Ldap; l != nil {
		p.LDAP = &apply.LDAPProvider{
			URL:           strings.TrimSpace(l.Url),
			StartTLS:      l.StartTls != nil && *l.StartTls,
			BindDN:        deref(l.BindDn, ""),
			BindPassword:  deref(l.BindPassword, ""),
			UserBase:      l.UserBase,
			UserFilter:    l.UserFilter,
			NameAttribute: deref(l.NameAttribute, ""),
			GroupBase:     deref(l.GroupBase, ""),
			GroupFilter:   deref(l.GroupFilter, ""),
		}
		if l.Groups != nil {
			p.LDAP.Groups = apply.LDAPGroups(*l.Groups)
		}
	}
	return p, nil
}

// providerToAPI renders a provider without its client secret or bind password.
func providerToAPI(p *apply.IdentityProvider) gen.IdentityProvider {
	out := gen.IdentityProvider{
		Id:        p.ID,
		Name:      p.Name,
		Kind:      gen.IdentityProviderKind(p.Kind),
		Enabled:   p.Enabled,
		Mappings:  make([]gen.GroupMapping, 0, len(p.Mappings)),
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
	for _, m := range p.Mappings {
		out.Mappings = append(out.Mappings, gen.GroupMapping{Group: m.Group, Scope: gen.Scope(m.Scope)})
	}
	if p.CACertificate != "" {
		out.CaCertificate = ptr(p.CACertificate)
	}
	if o := p.OIDC; o != nil {
		out.Oidc = &gen.OIDCSettings{
			Issuer:       o.Issuer,
			ClientId:     o.ClientID,
			Scopes:       ptr(slices.Clone(o.Scopes)),
			GroupsClaim:  ptr(o.GroupsClaim),
			NameClaim:    ptr(o.NameClaim),
			RedirectUris: slices.Clone(o.RedirectURIs),
		}
	}
	if l := p.LDAP; l != nil {
		out.Ldap = &gen.LDAPSettings{
			Url:           l.URL,
			StartTls:      ptr(l.StartTLS),
			UserBase:      l.UserBase,
			UserFilter:    l.UserFilter,
			NameAttribute: ptr(l.NameAttribute),
			Groups:        ptr(gen.LDAPSettingsGroups(l.Groups)),
		}
		if l.BindDN != "" {
			out.Ldap.BindDn = ptr(l.BindDN)
		}
		if l.GroupBase != "" {
			out.Ldap.GroupBase = ptr(l.GroupBase)
		}
		if l.GroupFilter != "" {
			out.Ldap.GroupFilter = ptr(l.GroupFilter)
		}
	}
	return out
}
