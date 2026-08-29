package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/secondary"
)

// newSecondaryCommand groups what the other end of a zone transfer needs.
func newSecondaryCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "secondary",
		Aliases: []string{"secondaries"},
		Short:   "Set up the other end of a zone transfer",
		Long: "A second nameserver takes a copy of a zone from this one and answers\n" +
			"from it, so that one machine rebooting does not take the zone with it.\n\n" +
			"What this server needs is `weg settings`: who may take a copy, and who\n" +
			"is told when a zone changes. What the other end needs is written here.\n\n" +
			"This writes a file and never installs one. Where that file goes and how\n" +
			"that server is reloaded belong to whatever owns that machine.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newSecondaryConfigCommand(opts, &f))
	return cmd
}

// secondaryConfigured is what the command reports when it is asked for
// something other than the file itself.
type secondaryConfigured struct {
	Format   string   `json:"format"`
	Content  string   `json:"content"`
	Warnings []string `json:"warnings"`
}

// secondaryRequest is what the command was asked to write.
type secondaryRequest struct {
	format   string
	zones    []string
	primary  string
	far      string
	key      string
	unsigned bool
	zoneDir  string
}

func newSecondaryConfigCommand(opts *options, f *clientFlags) *cobra.Command {
	var req secondaryRequest

	cmd := &cobra.Command{
		Use:   "config SOFTWARE [ZONE...]",
		Short: "Write the configuration a secondary needs",
		Long: "Write the configuration for the software running at the other end,\n" +
			"for " + strings.Join(formatNames(), " or ") + ".\n\n" +
			"Every zone this server holds goes in, the reverse zones among them,\n" +
			"unless zones are named. A zone that is switched off is left out: a\n" +
			"transfer of it is refused, so a secondary configured for one would\n" +
			"retry for ever.\n\n" +
			"The file carries the transfer key's secret in clear, which is what it\n" +
			"is for. Move it the way you would move a credential, and expect this\n" +
			"command to need a token with the admin scope.\n\n" +
			"With --output text the file goes to standard output unchanged, so it\n" +
			"can be redirected or piped, and what is wrong with the arrangement\n" +
			"goes to standard error rather than into the file. The other formats\n" +
			"carry both together.\n\n" +
			"Checking the syntax is the other program's job: write the file, then\n" +
			"run its own checker over it.",
		Args: usageArgs(cobra.MinimumNArgs(1)),
		Example: "  weg secondary config bind --primary 192.0.2.1 > /etc/named/wegweiser.conf\n" +
			"  weg secondary config knot --primary 192.0.2.1 --secondary 198.51.100.53\n" +
			"  weg secondary config bind --primary 192.0.2.1 example.com --output json",

		RunE: func(c *cobra.Command, args []string) error {
			if req.primary == "" {
				return missingPrimary(f)
			}
			req.format, req.zones = args[0], args[1:]
			return runSecondaryConfig(c.Context(), opts, f, req)
		},
		ValidArgsFunction: completeSecondaryArgs(f),
	}

	cmd.Flags().StringVar(&req.primary, "primary", "",
		"the address a secondary reaches this server at, with an optional port; "+
			"this server cannot work it out itself")
	cmd.Flags().StringVar(&req.far, "secondary", "",
		"the address this configuration is for; it goes nowhere in the file, and "+
			"naming it is what lets the transfer and notify lists be checked")
	cmd.Flags().StringVar(&req.key, "key", "",
		"the TSIG key the secondary signs with; the one on the transfer list where "+
			"there is exactly one")
	cmd.Flags().BoolVar(&req.unsigned, "unsigned", false,
		"write a configuration that signs nothing, for a secondary the address list grants")
	cmd.Flags().StringVar(&req.zoneDir, "zone-dir", "",
		"where the secondary keeps its copies of the zones; each program's usual "+
			"place by default")
	registerFlagCompletion(cmd, "key", completeTSIGKeys(f))
	return cmd
}

func runSecondaryConfig(
	ctx context.Context, opts *options, f *clientFlags, req secondaryRequest,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	params := gen.GetSecondaryConfigParams{
		Format:  gen.SecondaryFormat(req.format),
		Primary: req.primary,
	}
	if req.far != "" {
		params.Secondary = &req.far
	}
	if len(req.zones) > 0 {
		params.Zone = &req.zones
	}
	if req.key != "" {
		params.Key = &req.key
	}
	if req.unsigned {
		signed := false
		params.Signed = &signed
	}
	if req.zoneDir != "" {
		params.ZoneDir = &req.zoneDir
	}

	resp, err := client.GetSecondaryConfigWithResponse(ctx, &params)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	got := secondaryConfigured{
		Format:   string(resp.JSON200.Format),
		Content:  resp.JSON200.Content,
		Warnings: resp.JSON200.Warnings,
	}
	printer := opts.Printer()
	return printer.Print(got, func(w io.Writer) error {
		// The warnings first, and to standard error, so that the file stays a
		// file and redirecting it does not swallow them. The other formats
		// carry them in the object instead.
		if werr := writeSecondaryWarnings(printer.ErrOut(), got.Warnings); werr != nil {
			return werr
		}
		_, werr := io.WriteString(w, got.Content)
		return werr
	})
}

// writeSecondaryWarnings reports what will stop the arrangement working.
//
// The exit status stays zero. The file was written and it is correct; what is
// missing is a setting somewhere else, and half an arrangement is what somebody
// setting one up has.
func writeSecondaryWarnings(w io.Writer, warnings []string) error {
	for _, s := range warnings {
		if _, err := fmt.Fprintf(w, "warning: %s\n", s); err != nil {
			return err
		}
	}
	return nil
}

// missingPrimary explains what has to be given, and offers the host the API was
// reached at where that is one a secondary could plausibly use.
func missingPrimary(f *clientFlags) error {
	msg := "--primary is missing: this server does not know which of its addresses a " +
		"secondary reaches it on"
	if host := apiHost(f); host != "" {
		msg += fmt.Sprintf(", and a hidden primary is named by no record to ask.\n"+
			"Did you mean --primary %s?", host)
	}
	return usageError{errors.New(msg)}
}

// apiHost is the address the API was reached at, or empty where that says
// nothing: loopback and the unspecified address are where a client starts.
func apiHost(f *clientFlags) string {
	u, err := url.Parse(f.address())
	if err != nil {
		return ""
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.IsLoopback() || addr.IsUnspecified() {
		return ""
	}
	return addr.String()
}

// formatNames is the software a configuration can be written for.
func formatNames() []string {
	offered := secondary.Formats()
	out := make([]string, 0, len(offered))
	for _, s := range offered {
		out = append(out, s.String())
	}
	return out
}

// completeSecondaryArgs completes the software first and zone names after it.
func completeSecondaryArgs(f *clientFlags) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeStatic(formatNames()...)(c, args, prefix)
		}
		// completeZones refuses a second name, because everywhere else a zone
		// is the only argument. Here every argument after the software is one.
		return completeZones(f)(c, nil, prefix)
	}
}
