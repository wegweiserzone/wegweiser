package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/oapi-codegen/nullable"
	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// zoneUpdated is what changing a zone reports: what it now is, and what the
// change cost in serials.
type zoneUpdated struct {
	Zone       string `json:"zone"`
	Serial     int64  `json:"serial"`
	SerialFrom int64  `json:"serialFrom"`
	Status     string `json:"status"`
}

func newZoneUpdateCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		soa     gen.SOAInput
		ns      string
		email   string
		ttl     int64
		note    string
		reverse string
		soaIn   struct{ refresh, retry, expire, minimum, ttl int64 }
	)

	cmd := &cobra.Command{
		Use:     "update ZONE",
		Aliases: []string{"edit", "set"},
		Short:   "Change a zone's settings",
		Long: "Change what a zone is, rather than what is in it.\n\n" +
			"Only the flags given are changed; everything else is left as it was.\n" +
			"The serial is not among them: one commit advances it by exactly one,\n" +
			"and that is what lets the history be replayed (docs/decisions/ D2).\n\n" +
			"`--auto-reverse` has three states, and `server` is not `off`: it puts\n" +
			"the zone back on the server-wide setting, so changing that setting\n" +
			"reaches this zone again.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg zone update example.com --ttl 300\n" +
			"  weg zone update example.com --email hostmaster@example.com\n" +
			"  weg zone update example.com --auto-reverse off\n" +
			"  weg zone update example.com --auto-reverse server --comment \"back on the default\"",

		RunE: func(c *cobra.Command, args []string) error {
			var in gen.UpdateZone

			if c.Flags().Changed("ns") {
				soa.PrimaryNs = &ns
			}
			if c.Flags().Changed("email") {
				mbox, err := mailbox(email)
				if err != nil {
					return err
				}
				soa.Mailbox = &mbox
			}
			for _, opt := range []struct {
				flag string
				from *int64
				to   **int64
			}{
				{"refresh", &soaIn.refresh, &soa.Refresh},
				{"retry", &soaIn.retry, &soa.Retry},
				{"expire", &soaIn.expire, &soa.Expire},
				{"minimum", &soaIn.minimum, &soa.Minimum},
				{"soa-ttl", &soaIn.ttl, &soa.Ttl},
			} {
				if c.Flags().Changed(opt.flag) {
					*opt.to = opt.from
				}
			}
			if soa != (gen.SOAInput{}) {
				in.Soa = &soa
			}
			if c.Flags().Changed("ttl") {
				in.DefaultTtl = &ttl
			}
			if c.Flags().Changed("comment") {
				in.Comment = &note
			}
			if c.Flags().Changed("auto-reverse") {
				v, err := autoReverse(reverse)
				if err != nil {
					return err
				}
				in.AutoReverse = v
			}

			if nothingToChange(in) {
				return usageError{fmt.Errorf(
					"nothing to change: pass --ttl, --ns, --email, --comment, " +
						"--auto-reverse, or one of the SOA timers")}
			}
			return runZoneUpdate(c.Context(), opts, f, args[0], in, "updated")
		},
		ValidArgsFunction: completeZones(f),
	}

	cmd.Flags().StringVar(&ns, "ns", "", "the primary name server")
	cmd.Flags().StringVar(&email, "email", "",
		"the administrator, as an address or as a DNS name")
	cmd.Flags().Int64Var(&ttl, "ttl", 0, "the TTL a record added without one gets")
	cmd.Flags().StringVar(&note, "comment", "", "a note about what this zone is for")
	cmd.Flags().StringVar(&reverse, "auto-reverse", "",
		"whether address records here generate PTRs: on, off, or server")
	cmd.Flags().Int64Var(&soaIn.refresh, "refresh", 0, "SOA refresh interval, in seconds")
	cmd.Flags().Int64Var(&soaIn.retry, "retry", 0, "SOA retry interval, in seconds")
	cmd.Flags().Int64Var(&soaIn.expire, "expire", 0, "SOA expire interval, in seconds")
	cmd.Flags().Int64Var(&soaIn.minimum, "minimum", 0,
		"SOA minimum, the negative-caching TTL of RFC 2308 §4")
	cmd.Flags().Int64Var(&soaIn.ttl, "soa-ttl", 0, "the TTL of the SOA record itself")

	registerFlagCompletion(cmd, "auto-reverse", completeStatic("on", "off", "server"))
	return cmd
}

// autoReverse reads the three states the setting has.
func autoReverse(s string) (nullable.Nullable[bool], error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes":
		return nullable.NewNullableWithValue(true), nil
	case "off", "false", "no":
		return nullable.NewNullableWithValue(false), nil
	case "server", "default", "inherit":
		return nullable.NewNullNullable[bool](), nil
	}
	return nullable.Nullable[bool]{}, usageError{fmt.Errorf(
		"%q is not one of on, off or server", s)}
}

// nothingToChange reports a request that would send an empty patch.
func nothingToChange(in gen.UpdateZone) bool {
	return in.Soa == nil &&
		in.DefaultTtl == nil &&
		in.Comment == nil &&
		in.Disabled == nil &&
		!in.AutoReverse.IsSpecified()
}

// newZoneDisableCommand and its opposite are separate commands rather than a
// flag on update, the same way they are for records: taking a zone off the
// wire is a thing a person does at two in the morning, and `weg zone disable`
// is what they reach for.
func newZoneDisableCommand(opts *options, f *clientFlags) *cobra.Command {
	return newZoneSwitchCommand(opts, f, true)
}

func newZoneEnableCommand(opts *options, f *clientFlags) *cobra.Command {
	return newZoneSwitchCommand(opts, f, false)
}

func newZoneSwitchCommand(opts *options, f *clientFlags, disable bool) *cobra.Command {
	verb, past, what := "enable", "enabled", "answered for again"
	if disable {
		verb, past, what = "disable", "disabled",
			"kept, and answered for as though this server held nothing"
	}
	title := strings.ToUpper(verb[:1]) + verb[1:]

	return &cobra.Command{
		Use:   verb + " ZONE",
		Short: title + " a zone",
		Long: fmt.Sprintf("%s a zone: it is %s.\n\n"+
			"A disabled zone is invisible rather than merely marked, a query for\n"+
			"a name inside it is answered as if the zone were not here at all. Its\n"+
			"records are untouched, so this is reversible by running the opposite\n"+
			"command.", title, what),
		Args:    usageArgs(cobra.ExactArgs(1)),
		Example: fmt.Sprintf("  weg zone %s example.com", verb),

		RunE: func(c *cobra.Command, args []string) error {
			return runZoneUpdate(c.Context(), opts, f, args[0],
				gen.UpdateZone{Disabled: &disable}, past)
		},
		ValidArgsFunction: completeZones(f),
	}
}

func runZoneUpdate(
	ctx context.Context, opts *options, f *clientFlags,
	name string, in gen.UpdateZone, past string,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	before, err := findZone(ctx, client, f, name)
	if err != nil {
		return err
	}

	resp, err := client.UpdateZoneWithResponse(ctx, before.Id, in)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	z := resp.JSON200

	changed := zoneUpdated{
		Zone:       z.Name,
		Serial:     opt(z.Soa.Serial, 0),
		SerialFrom: opt(before.Soa.Serial, 0),
		Status:     statusText(z.Disabled),
	}

	p := opts.Printer()
	return p.Print(changed, func(w io.Writer) error {
		if _, werr := fmt.Fprintf(w, "%s %s\n", past, changed.Zone); werr != nil {
			return werr
		}
		t := newRows(w, 2)
		t.row(p.Paint(output.ColorDim, "serial"),
			fmt.Sprintf("%d → %d", changed.SerialFrom, changed.Serial))
		t.row(p.Paint(output.ColorDim, "status"), changed.Status)
		t.row(p.Paint(output.ColorDim, "reverse automation"), autoReverseText(z.AutoReverse))
		return t.flush()
	})
}
