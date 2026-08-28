package cli

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
)

// completionTimeout bounds how long a shell waits for a suggestion.
const completionTimeout = 2 * time.Second

// recordTypes are what `--type` and the TYPE argument suggest.
//
// A hint rather than a rule: the server takes any type, including the
// TYPE<number> form of RFC 3597 §5, and completion that only offered what is
// on this list would be teaching the wrong lesson. These are the ones people
// type.
var recordTypes = []string{
	"A", "AAAA", "CAA", "CNAME", "DNAME", "HINFO", "HTTPS", "LOC", "MX",
	"NAPTR", "NS", "PTR", "SOA", "SRV", "SSHFP", "SVCB", "TLSA", "TXT",
}

// completeZones suggests the zones this server holds.
func completeZones(f *clientFlags) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		zones, _, err := zonesAndClient(c.Context(), f, prefix)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		names := make([]string, 0, len(zones))
		for i := range zones {
			names = append(names, zones[i].Name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeRecordArgs suggests a zone, then an owner name in it, then a type at
// that name.
func completeRecordArgs(f *clientFlags, existing bool) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return completeZones(f)(c, args, prefix)

		case 1:
			return completeOwners(c.Context(), f, args[0], prefix)

		case 2:
			if !existing {
				return recordTypes, cobra.ShellCompDirectiveNoFileComp
			}
			return completeTypesAt(c.Context(), f, args[0], args[1], prefix)

		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
}

// completeOwners suggests the names a zone holds records at.
func completeOwners(
	ctx context.Context, f *clientFlags, zoneName, prefix string,
) ([]string, cobra.ShellCompDirective) {
	records, err := completionRecords(ctx, f, zoneName, gen.ListRecordsParams{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	seen := make(map[string]struct{}, len(records))
	var names []string
	for i := range records {
		name := records[i].Name
		if _, done := seen[name]; done || !strings.HasPrefix(name, prefix) {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeTypesAt suggests the types a name actually has records of.
func completeTypesAt(
	ctx context.Context, f *clientFlags, zoneName, owner, prefix string,
) ([]string, cobra.ShellCompDirective) {
	records, err := completionRecords(ctx, f, zoneName, gen.ListRecordsParams{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	seen := make(map[string]struct{}, len(records))
	var types []string
	for i := range records {
		// The owner is compared against what the user has typed so far, which
		// may still be relative, the same completion the command itself does.
		if !strings.EqualFold(records[i].Name, owner) && !strings.EqualFold(records[i].Name, owner+".") {
			continue
		}
		typ := records[i].Type
		if _, done := seen[typ]; done || !strings.HasPrefix(typ, strings.ToUpper(prefix)) {
			continue
		}
		seen[typ] = struct{}{}
		types = append(types, typ)
	}
	if len(types) == 0 {
		// The name may be relative and not yet resolvable, or simply new.
		// Offering nothing would look like a broken shell.
		return recordTypes, cobra.ShellCompDirectiveNoFileComp
	}
	return types, cobra.ShellCompDirectiveNoFileComp
}

// completionRecords reads a zone's records for a suggestion, under the short
// timeout a shell can wait for.
func completionRecords(
	ctx context.Context, f *clientFlags, zoneName string, params gen.ListRecordsParams,
) ([]gen.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()

	client, err := f.client()
	if err != nil {
		return nil, err
	}
	z, err := findZone(ctx, client, f, zoneName)
	if err != nil {
		return nil, err
	}
	// Bounded: a suggestion list nobody can read is not worth a slow keystroke.
	return allRecords(ctx, client, f, z.Id, params, 2000)
}

// zonesAndClient reads the zones for a suggestion, under the same bound.
func zonesAndClient(
	ctx context.Context, f *clientFlags, prefix string,
) ([]gen.Zone, *gen.ClientWithResponses, error) {
	ctx, cancel := context.WithTimeout(ctx, completionTimeout)
	defer cancel()

	client, err := f.client()
	if err != nil {
		return nil, nil, err
	}

	var params gen.ListZonesParams
	if prefix != "" {
		params.Search = &prefix
	}
	zones, err := allZones(ctx, client, f, params, 2000)
	if err != nil {
		return nil, nil, err
	}
	return zones, client, nil
}

// completeTokens suggests the tokens that can still be revoked. A revoked one
// is left out: revoking it again is not an error, but offering it is offering
// something to do that does nothing.
func completeTokens(f *clientFlags) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		ctx, cancel := context.WithTimeout(c.Context(), completionTimeout)
		defer cancel()

		client, err := f.client()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		tokens, err := fetchTokens(ctx, client, f)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		var names []string
		for i := range tokens {
			if tokens[i].RevokedAt != nil || !strings.HasPrefix(tokens[i].Name, prefix) {
				continue
			}
			names = append(names, tokens[i].Name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeTSIGKeys suggests the keys this server holds, withdrawn ones left
// out: nothing a command does to one of those would change anything.
func completeTSIGKeys(f *clientFlags) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		ctx, cancel := context.WithTimeout(c.Context(), completionTimeout)
		defer cancel()

		client, err := f.client()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		keys, err := fetchTSIGKeys(ctx, client, f)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		var names []string
		for i := range keys {
			if keys[i].RevokedAt != nil || !strings.HasPrefix(keys[i].Name, prefix) {
				continue
			}
			names = append(names, keys[i].Name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeStatic suggests a fixed set, for a flag whose values are a closed
// vocabulary this side already knows.
func completeStatic(values ...string) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		var out []string
		for _, v := range values {
			if strings.HasPrefix(v, strings.ToLower(prefix)) {
				out = append(out, v)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// registerFlagCompletion attaches a completion function to a flag, and says so
// loudly if the flag is not there: a silent failure would be a completion
// that quietly never fires.
func registerFlagCompletion(cmd *cobra.Command, flag string, fn cobra.CompletionFunc) {
	if err := cmd.RegisterFlagCompletionFunc(flag, fn); err != nil {
		panic("cli: completion for --" + flag + " on " + cmd.Name() + ": " + err.Error())
	}
}
