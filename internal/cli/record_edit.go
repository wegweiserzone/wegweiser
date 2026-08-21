package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
)

// recordChanged is what an edit reports: the record as it now stands, and what
// the change caused elsewhere.
type recordChanged struct {
	Record       recordListed     `json:"record"`
	Was          string           `json:"was,omitempty"`
	Generated    []recordListed   `json:"generated,omitempty"`
	Conflicts    []recordConflict `json:"conflicts,omitempty"`
	MissingZones []missingZoneRef `json:"missingZones,omitempty"`
}

func newRecordUpdateCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		in   gen.UpdateRecord
		ttl  int64
		data string
		note string
	)

	cmd := &cobra.Command{
		Use:     "update ZONE NAME TYPE [DATA...]",
		Aliases: []string{"edit", "set"},
		Short:   "Change a record",
		Long: "Change a record's TTL, its data or its comment, keeping the record\n" +
			"itself.\n\n" +
			"The positional arguments say which record (the same way `weg record\n" +
			"delete` does) and the flags say what to change about it. Keeping the\n" +
			"record rather than removing and adding one is what keeps its comment,\n" +
			"where it came from, and the history pointing at it.\n\n" +
			"A record this server generated is refused: take it over with\n" +
			"`weg record detach` first (docs/decisions.md D4).",
		Args: usageArgs(cobra.MinimumNArgs(3)),
		Example: "  weg record update example.com www A 192.0.2.10 --ttl 60\n" +
			"  weg record update example.com www A --data 192.0.2.99\n" +
			"  weg record update example.com @ MX 10 mail.example.com. --comment \"the new relay\"",

		RunE: func(c *cobra.Command, args []string) error {
			if c.Flags().Changed("ttl") {
				in.Ttl = &ttl
			}
			if c.Flags().Changed("data") {
				in.Data = ptr(quoteText(strings.ToUpper(args[2]), data))
			}
			if c.Flags().Changed("comment") {
				in.Comment = &note
			}
			if in == (gen.UpdateRecord{}) {
				return usageError{fmt.Errorf(
					"nothing to change: pass --ttl, --data or --comment")}
			}
			return runRecordEdit(c.Context(), opts, f, args[0], args[1],
				strings.ToUpper(args[2]), strings.Join(args[3:], " "), in, "updated")
		},
		ValidArgsFunction: completeRecordArgs(f, true),
	}

	cmd.Flags().Int64Var(&ttl, "ttl", 0, "the TTL to give it")
	cmd.Flags().StringVar(&data, "data", "", "the data to give it")
	cmd.Flags().StringVar(&note, "comment", "", "a note about what this record is for")
	return cmd
}

// newRecordDisableCommand and its opposite are separate commands rather than a
// flag, because switching a record off is a thing a person does, not a
// property they set, and because `weg record disable` is what somebody
// reaches for at two in the morning.
func newRecordDisableCommand(opts *options, f *clientFlags) *cobra.Command {
	return newRecordSwitchCommand(opts, f, true)
}

func newRecordEnableCommand(opts *options, f *clientFlags) *cobra.Command {
	return newRecordSwitchCommand(opts, f, false)
}

func newRecordSwitchCommand(opts *options, f *clientFlags, disable bool) *cobra.Command {
	verb, past, what := "enable", "enabled", "answered with again"
	if disable {
		verb, past, what = "disable", "disabled", "kept but not answered with"
	}

	return &cobra.Command{
		Use:   verb + " ZONE NAME TYPE [DATA...]",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " a record",
		Long: fmt.Sprintf("%s a record: it is %s.\n\n"+
			"The record keeps everything else (its identity, its comment, its\n"+
			"history) so this is reversible by running the opposite command.",
			strings.ToUpper(verb[:1])+verb[1:], what),
		Args: usageArgs(cobra.MinimumNArgs(3)),
		Example: fmt.Sprintf("  weg record %s example.com www A 192.0.2.10\n"+
			"  weg record %s example.com old TXT", verb, verb),

		RunE: func(c *cobra.Command, args []string) error {
			return runRecordEdit(c.Context(), opts, f, args[0], args[1],
				strings.ToUpper(args[2]), strings.Join(args[3:], " "),
				gen.UpdateRecord{Disabled: &disable}, past)
		},
		ValidArgsFunction: completeRecordArgs(f, true),
	}
}

func runRecordEdit(
	ctx context.Context, opts *options, f *clientFlags,
	zoneName, name, typ, data string, in gen.UpdateRecord, past string,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, zoneName)
	if err != nil {
		return err
	}
	target, err := resolveRecord(ctx, client, f, z, name, typ, data)
	if err != nil {
		return err
	}
	was := fmt.Sprintf("%s %d IN %s %s", target.Name, target.Ttl, target.Type, target.Data)

	resp, err := client.UpdateRecordWithResponse(ctx, target.Id, in)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	changed := recordWritten(resp.JSON200)
	changed.Was = was
	return printRecordChange(opts, changed, past)
}

func newRecordDetachCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "detach ZONE NAME TYPE [DATA...]",
		Short: "Take over a record this server generated",
		Long: "Stop the automation maintaining a record, and keep the record.\n\n" +
			"A generated record (the PTR for an address, the CNAME of an RFC 2317\n" +
			"delegation) follows the record it came from, and editing it directly\n" +
			"is refused. Detaching keeps the data and the identity, drops the link,\n" +
			"and hands it over: from then on it is yours to change and yours to\n" +
			"keep correct (docs/decisions.md D4).\n\n" +
			"A record that was already yours is returned unchanged.",
		Args: usageArgs(cobra.MinimumNArgs(3)),
		Example: "  weg record detach 2.0.192.in-addr.arpa 10 PTR\n" +
			"  weg record detach 2.0.192.in-addr.arpa 10.2.0.192.in-addr.arpa. PTR",

		RunE: func(c *cobra.Command, args []string) error {
			return runRecordDetach(c.Context(), opts, f, args[0], args[1],
				strings.ToUpper(args[2]), strings.Join(args[3:], " "))
		},
		ValidArgsFunction: completeRecordArgs(f, true),
	}
}

func runRecordDetach(
	ctx context.Context, opts *options, f *clientFlags, zoneName, name, typ, data string,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, zoneName)
	if err != nil {
		return err
	}
	target, err := resolveRecord(ctx, client, f, z, name, typ, data)
	if err != nil {
		return err
	}
	generated := target.ManagedBy != nil || target.ManagedKind != nil

	resp, err := client.DetachRecordWithResponse(ctx, target.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	past := "detached"
	if !generated {
		// The API returns an already-authored record unchanged rather than
		// refusing, and saying "detached" about it would claim something
		// happened that did not.
		past = "already yours"
	}
	return printRecordChange(opts, recordWritten(resp.JSON200), past)
}

// recordWritten reads what a write produced into the shape a command reports.
func recordWritten(out *gen.RecordWritten) recordChanged {
	changed := recordChanged{Record: listRecord(&out.Record)}

	generated := deref(out.Generated, nil)
	for i := range generated {
		changed.Generated = append(changed.Generated, listRecord(&generated[i]))
	}
	for _, c := range deref(out.Conflicts, nil) {
		changed.Conflicts = append(changed.Conflicts, recordConflict{
			Address: c.Address, ExistingName: c.ExistingName, RequestedName: c.RequestedName,
		})
	}
	for _, m := range deref(out.MissingZones, nil) {
		changed.MissingZones = append(changed.MissingZones, missingZoneRef{
			Address: m.Address, ZoneName: m.ZoneName,
		})
	}
	return changed
}

// printRecordChange reports a record that was written, and what it caused.
func printRecordChange(opts *options, changed recordChanged, past string) error {
	return opts.Printer().Print(changed, func(w io.Writer) error {
		r := changed.Record
		now := fmt.Sprintf("%s %d IN %s %s", r.Name, r.TTL, r.Type, r.Data)
		if changed.Was != "" && changed.Was != now {
			// Both halves, because "updated" without the old value leaves a
			// person unable to tell a change from a no-op.
			if _, werr := fmt.Fprintf(w, "%s %s\n     from %s\n", past, now, changed.Was); werr != nil {
				return werr
			}
		} else if _, werr := fmt.Fprintf(w, "%s %s\n", past, now); werr != nil {
			return werr
		}

		for _, g := range changed.Generated {
			if _, werr := fmt.Fprintf(w, "  generated %s %d IN %s %s\n",
				g.Name, g.TTL, g.Type, g.Data); werr != nil {
				return werr
			}
		}
		for _, c := range changed.Conflicts {
			if _, werr := fmt.Fprintf(w,
				"  %s already answers with %s, so no entry was made for %s\n",
				c.Address, c.ExistingName, c.RequestedName); werr != nil {
				return werr
			}
		}
		for _, m := range changed.MissingZones {
			if _, werr := fmt.Fprintf(w,
				"  no reverse zone covers %s; create %s to have PTRs generated for it\n",
				m.Address, m.ZoneName); werr != nil {
				return werr
			}
		}
		return nil
	})
}
