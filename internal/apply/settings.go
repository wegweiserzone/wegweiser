package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// PolicySetting is the key the server-wide reverse conflict policy is stored
// under.
//
// It lives in the database rather than in the configuration file because an
// operator changes it and every client has to be able to reach it
// (docs/decisions/ D11). Reading it inside the write transaction is what
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

// TransferSetting is the key the list of clients allowed to pull a whole zone
// is stored under. Empty means nobody, which is where a server starts
// (docs/decisions/d26-outbound-zone-transfer.md).
const TransferSetting = "transfer_allow"

// TransferAllow is who may pull a zone off this server: client addresses, and
// the TSIG keys a request may sign itself with. One entry matching is enough
// (docs/decisions/d28-tsig.md).
type TransferAllow struct {
	Prefixes []netip.Prefix
	Keys     []zone.Name
}

// Empty reports whether the list permits nobody, which is where a server
// starts.
func (a TransferAllow) Empty() bool { return len(a.Prefixes) == 0 && len(a.Keys) == 0 }

// keyPrefix marks an entry that names a TSIG key rather than an address. A
// prefix never starts with it, so the two cannot be confused.
const keyPrefix = "key:"

// StoredTransferAllow returns who a zone transfer is served to.
func StoredTransferAllow(ctx context.Context, r store.Reader) (TransferAllow, error) {
	raw, err := r.Setting(ctx, TransferSetting)
	if errors.Is(err, store.ErrNotFound) {
		return TransferAllow{}, nil
	}
	if err != nil {
		return TransferAllow{}, err
	}

	var text []string
	if uerr := json.Unmarshal(raw, &text); uerr != nil {
		return TransferAllow{}, fmt.Errorf("apply: the stored transfer list is not readable: %w", uerr)
	}
	return ParseTransferAllow(text)
}

// SetStoredTransferAllow records who may pull a zone.
func SetStoredTransferAllow(ctx context.Context, w store.Writer, allow TransferAllow) error {
	raw, err := json.Marshal(TransferAllowText(allow))
	if err != nil {
		return fmt.Errorf("apply: encode the transfer list: %w", err)
	}
	return w.PutSetting(ctx, TransferSetting, raw)
}

// ParseTransferAllow turns what a client sent into addresses and key names.
//
// An entry beginning with "key:" names a TSIG key, which grants a transfer from
// any address at all; that is what a key is for. Everything else is an address.
//
// A bare address is the single-host prefix it means, so that an operator naming
// one secondary does not have to write /32 to be understood. A prefix with host
// bits set is refused rather than masked: 192.0.2.7/24 is either a typo for the
// host or for the network, and guessing which would sometimes hand a zone to a
// network nobody meant to name.
func ParseTransferAllow(in []string) (TransferAllow, error) {
	var out TransferAllow
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if after, found := strings.CutPrefix(s, keyPrefix); found {
			name, err := zone.ParseName(strings.TrimSpace(after))
			if err != nil {
				return TransferAllow{}, fmt.Errorf(
					"%w: %q does not name a TSIG key: %w", zone.ErrInvalid, s, err)
			}
			out.Keys = append(out.Keys, name)
			continue
		}
		if addr, err := netip.ParseAddr(s); err == nil {
			out.Prefixes = append(out.Prefixes, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return TransferAllow{}, fmt.Errorf(
				"%w: %q is not an address, a CIDR prefix, or a key named as key:<name>",
				zone.ErrInvalid, s)
		}
		if p.Masked() != p {
			return TransferAllow{}, fmt.Errorf(
				"%w: %q has bits set below its prefix length; write %s for the network or %s for the host",
				zone.ErrInvalid, s, p.Masked(), netip.PrefixFrom(p.Addr(), p.Addr().BitLen()))
		}
		out.Prefixes = append(out.Prefixes, p)
	}
	return out, nil
}

// TransferAllowText renders the list the way a client sends it, addresses
// first.
func TransferAllowText(allow TransferAllow) []string {
	out := make([]string, 0, len(allow.Prefixes)+len(allow.Keys))
	for _, p := range allow.Prefixes {
		out = append(out, p.String())
	}
	for _, k := range allow.Keys {
		out = append(out, keyPrefix+k.String())
	}
	return out
}

// NotifySetting is the key the list of secondaries told about a change is
// stored under. Empty means nobody, which is where a server starts
// (docs/decisions/d27-notify.md).
const NotifySetting = "notify_targets"

// dnsPort is the port RFC 1035 §4.2 assigns, and what a target named without
// one means.
const dnsPort = 53

// NotifyTarget is one secondary told when a zone changes.
type NotifyTarget struct {
	Addr netip.AddrPort
	// Key is the TSIG key the notification is signed with, and is the zero
	// name for one that goes out unsigned.
	Key zone.Name
}

// StoredNotifyTargets returns the secondaries a notification is sent to.
func StoredNotifyTargets(ctx context.Context, r store.Reader) ([]NotifyTarget, error) {
	raw, err := r.Setting(ctx, NotifySetting)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var text []string
	if uerr := json.Unmarshal(raw, &text); uerr != nil {
		return nil, fmt.Errorf("apply: the stored notify list is not readable: %w", uerr)
	}
	return ParseNotifyTargets(text)
}

// SetStoredNotifyTargets records who is told when a zone changes.
func SetStoredNotifyTargets(ctx context.Context, w store.Writer, targets []NotifyTarget) error {
	raw, err := json.Marshal(NotifyTargetsText(targets))
	if err != nil {
		return fmt.Errorf("apply: encode the notify list: %w", err)
	}
	return w.PutSetting(ctx, NotifySetting, raw)
}

// ParseNotifyTargets turns what a client sent into addresses to send to.
//
// A port may be given, because a secondary does not have to listen on 53. Where
// none is, the port RFC 1035 §4.2 assigns is meant. Unlike the transfer list
// this takes no prefixes: that list decides who may ask, and this one has to
// name somewhere a datagram can arrive.
func ParseNotifyTargets(in []string) ([]NotifyTarget, error) {
	out := make([]NotifyTarget, 0, len(in))
	for _, entry := range in {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// A key follows the address, the way BIND writes an also-notify, and
		// is marked the same way as an entry in the transfer list.
		where, keyed, found := strings.Cut(entry, " ")
		var target NotifyTarget
		if found {
			name, ok := strings.CutPrefix(strings.TrimSpace(keyed), keyPrefix)
			if !ok {
				return nil, fmt.Errorf(
					"%w: %q has something after the address that is not a key; write it as "+
						"%s %s<name>", zone.ErrInvalid, entry, where, keyPrefix)
			}
			parsed, err := zone.ParseName(strings.TrimSpace(name))
			if err != nil {
				return nil, fmt.Errorf(
					"%w: %q does not name a TSIG key: %w", zone.ErrInvalid, entry, err)
			}
			target.Key = parsed
		}

		addr, err := parseNotifyAddr(where)
		if err != nil {
			return nil, err
		}
		target.Addr = addr
		out = append(out, target)
	}
	return out, nil
}

// parseNotifyAddr reads the address half of an entry.
func parseNotifyAddr(s string) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf(
			"%w: %q is not an address, with or without a port; a prefix cannot be notified",
			zone.ErrInvalid, s)
	}
	return netip.AddrPortFrom(addr.Unmap(), dnsPort), nil
}

// NotifyTargetsText renders the list the way a client sends it, leaving the
// port off where it is the one every resolver would have assumed.
func NotifyTargetsText(targets []NotifyTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		where := t.Addr.String()
		if t.Addr.Port() == dnsPort {
			where = t.Addr.Addr().String()
		}
		if t.Key.IsZero() {
			out[i] = where
			continue
		}
		out[i] = where + " " + keyPrefix + t.Key.String()
	}
	return out
}
