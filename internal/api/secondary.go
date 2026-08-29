package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/secondary"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// dnsPort is the port RFC 1035 §4.2 assigns, and what an address given without
// one means.
const dnsPort = 53

// GetSecondaryConfig writes the configuration the other end of a transfer
// needs, and says what about the arrangement will stop it working.
func (s *Server) GetSecondaryConfig(
	ctx context.Context, req gen.GetSecondaryConfigRequestObject,
) (gen.GetSecondaryConfigResponseObject, error) {
	// The output carries a key's secret, so it is guarded the way reading one
	// is (D34).
	if err := requireAdmin(ctx, "writing a secondary's configuration"); err != nil {
		return nil, err
	}

	format := secondary.Format(req.Params.Format)
	if !format.Valid() {
		return nil, badRequest("no configuration is written for %q; this server writes %s",
			req.Params.Format, formatsOffered())
	}

	primary, err := parseAddrPort("the address this server is reached on", req.Params.Primary)
	if err != nil {
		return nil, err
	}

	cfg := secondary.Config{Primary: primary, ZoneDir: deref(req.Params.ZoneDir, "")}
	arr := secondary.Arrangement{}
	if named := deref(req.Params.Secondary, ""); named != "" {
		far, aerr := netip.ParseAddr(strings.TrimSpace(named))
		if aerr != nil {
			return nil, badRequest(
				"the secondary is one address rather than a network, and %q is not one", named)
		}
		arr.Secondary = far
	}

	if verr := s.store.View(ctx, func(r store.Reader) error {
		cur, serr := s.settings(ctx, r)
		if serr != nil {
			return serr
		}
		arr.AllowPrefixes = cur.allow.Prefixes
		arr.AllowKeys = cur.allow.Keys
		arr.Notify = notifyAddrs(cur.notify)

		zones, zerr := secondaryZones(ctx, r, req.Params.Zone)
		if zerr != nil {
			return zerr
		}
		cfg.Zones = zones

		key, kerr := secondaryKey(ctx, r, req.Params, cur.allow)
		if kerr != nil {
			return kerr
		}
		cfg.Key = key
		return nil
	}); verr != nil {
		return nil, verr
	}

	var out strings.Builder
	// Everything Render refuses has been settled above, so a failure here is
	// this server's own rather than something a client sent.
	if rerr := secondary.Render(&out, format, cfg); rerr != nil {
		return nil, internal(rerr)
	}
	// An empty list rather than a missing one: the field is always there, so a
	// client ranges over it without asking whether it is null first.
	warnings := arr.Warnings(cfg)
	if warnings == nil {
		warnings = []string{}
	}
	return gen.GetSecondaryConfig200JSONResponse{
		Format:   gen.SecondaryFormat(format),
		Content:  out.String(),
		Warnings: warnings,
	}, nil
}

// formatsOffered names the software a configuration is written for.
func formatsOffered() string {
	offered := secondary.Formats()
	names := make([]string, 0, len(offered))
	for _, f := range offered {
		names = append(names, f.String())
	}
	return strings.Join(names, " and ")
}

// parseAddrPort reads an address with an optional port, which is how the notify
// list is written as well.
func parseAddrPort(what, s string) (netip.AddrPort, error) {
	s = strings.TrimSpace(s)
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap, nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.AddrPort{}, badRequest(
			"%s is an address with an optional port, and %q is not", what, s)
	}
	return netip.AddrPortFrom(addr, dnsPort), nil
}

// notifyAddrs reduces the notify list to addresses. Which port a notification
// is sent to says nothing about which secondary receives it.
func notifyAddrs(targets []apply.NotifyTarget) []netip.Addr {
	out := make([]netip.Addr, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Addr.Addr())
	}
	return out
}

// secondaryZones resolves the zones to write out.
//
// A disabled zone is left out whether or not it was asked for. It is not in the
// snapshot, so a transfer of it is refused, and a configuration naming one
// leaves a secondary retrying something that will never be served.
func secondaryZones(ctx context.Context, r store.Reader, named *[]string) ([]zone.Name, error) {
	enabled := false
	if named == nil || len(*named) == 0 {
		var out []zone.Name
		f := store.ZoneFilter{Disabled: &enabled}
		for {
			page, err := r.ListZones(ctx, f)
			if err != nil {
				return nil, err
			}
			for _, z := range page.Items {
				out = append(out, z.Name)
			}
			if page.NextCursor == "" {
				return out, nil
			}
			f.Cursor = page.NextCursor
		}
	}

	out := make([]zone.Name, 0, len(*named))
	for _, s := range *named {
		name, err := parseName("the zone name", s)
		if err != nil {
			return nil, err
		}
		page, lerr := r.ListZones(ctx, store.ZoneFilter{Name: name})
		if lerr != nil {
			return nil, lerr
		}
		if len(page.Items) == 0 {
			return nil, badRequest("no zone named %s on this server", name)
		}
		if page.Items[0].Disabled {
			return nil, badRequest("the zone %s is switched off, so a transfer of it is "+
				"refused; a configuration naming it would retry for ever", name)
		}
		out = append(out, page.Items[0].Name)
	}
	return out, nil
}

// secondaryKey works out which key the configuration signs with, which is the
// one thing here a caller can leave to the server: a transfer list with one key
// on it is not ambiguous.
func secondaryKey(
	ctx context.Context, r store.Reader,
	params gen.GetSecondaryConfigParams, allow apply.TransferAllow,
) (*secondary.Key, error) {
	if !deref(params.Signed, true) {
		return nil, nil
	}

	var name zone.Name
	switch {
	case deref(params.Key, "") != "":
		var err error
		if name, err = parseName("the key name", *params.Key); err != nil {
			return nil, err
		}
	case len(allow.Keys) == 1:
		name = allow.Keys[0]
	case len(allow.Keys) > 1:
		return nil, badRequest("more than one key may transfer from this server (%s); "+
			"name the one this secondary signs with", strings.Join(keyNames(allow.Keys), ", "))
	default:
		// No key grants anything here, so there is nothing to sign with. The
		// warnings say whether the address list grants instead.
		return nil, nil
	}

	key, err := r.TSIGKeyByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, badRequest("no key named %s signs here; a withdrawn one does not "+
			"either, because its secret is gone", name)
	}
	if err != nil {
		return nil, err
	}
	return &secondary.Key{
		Name:      key.Name,
		Algorithm: key.Algorithm,
		Secret:    base64.StdEncoding.EncodeToString(key.Secret),
	}, nil
}

// keyNames renders key names for a message that has to list them.
func keyNames(names []zone.Name) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n.String())
	}
	return out
}
