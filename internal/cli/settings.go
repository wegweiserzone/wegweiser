package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// reversePolicies are the values the reverse conflict policy accepts, in the
// order docs/decisions/ D3 argues them.
var reversePolicies = []string{
	string(gen.FirstWins), string(gen.LastWins), string(gen.Multi), string(gen.Reject),
}

// serverSettings is what the commands here report.
type serverSettings struct {
	ReverseConflictPolicy string   `json:"reverseConflictPolicy"`
	TransferAllow         []string `json:"transferAllow"`
	NotifyTargets         []string `json:"notifyTargets"`
}

func newSettingsCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Read and change what this server does by default",
		Long: "Server-wide settings.\n\n" +
			"These are the defaults a zone that says nothing about itself inherits.\n" +
			"They live in the database rather than in the configuration file, so\n" +
			"every client reaches them and a change takes effect on the next write\n" +
			"without a restart.",
		Args: usageArgs(cobra.NoArgs),
	}
	cmd.AddCommand(newSettingsShowCommand(opts))
	cmd.AddCommand(newSettingsSetCommand(opts))
	return cmd
}

func newSettingsShowCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "show",
		Short:   "Print the settings in force",
		Args:    usageArgs(cobra.NoArgs),
		Example: "  weg settings show\n  weg settings show --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			return runSettingsShow(c.Context(), opts, &f)
		},
	}
	f.register(cmd)
	return cmd
}

func runSettingsShow(ctx context.Context, opts *options, f *clientFlags) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	resp, err := client.GetSettingsWithResponse(ctx)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return printSettings(opts, *resp.JSON200)
}

func newSettingsSetCommand(opts *options) *cobra.Command {
	var (
		f      clientFlags
		policy string
		allow  []string
		notify []string
	)

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Change a setting",
		Long: "Change what this server does when nobody has said otherwise.\n\n" +
			"Only what is named on the command line changes; everything else is\n" +
			"left as it was. The settings as they stand afterwards are printed, so\n" +
			"a change can be read back without a second command.",
		Args: usageArgs(cobra.NoArgs),
		Example: "  weg settings set --reverse-conflict-policy last-wins\n" +
			"  weg settings set --transfer-allow 192.0.2.0/24,2001:db8::/32\n" +
			"  weg settings set --transfer-allow \"\"   # nobody, which is where a server starts\n" +
			"  weg settings set --notify 192.0.2.53,198.51.100.53\n" +
			"  weg settings set --notify \"192.0.2.53 key:secondary.example.com.\"",

		RunE: func(c *cobra.Command, _ []string) error {
			// A pointer says "the caller named this", which is what the API
			// means by a field being present. An empty transfer list is a
			// value, not an omission.
			var (
				setPolicy *string
				setAllow  *[]string
				setNotify *[]string
			)
			if c.Flags().Changed("reverse-conflict-policy") {
				setPolicy = &policy
			}
			if c.Flags().Changed("transfer-allow") {
				setAllow = &allow
			}
			if c.Flags().Changed("notify") {
				setNotify = &notify
			}
			if setPolicy == nil && setAllow == nil && setNotify == nil {
				return errors.New("nothing to change: name a setting, or see `weg settings show`")
			}
			return runSettingsSet(c.Context(), opts, &f, setPolicy, setAllow, setNotify)
		},
	}
	cmd.Flags().StringVar(&policy, "reverse-conflict-policy", "",
		"what to do when an address already answers with another name: "+
			strings.Join(reversePolicies, ", "))
	registerFlagCompletion(cmd, "reverse-conflict-policy", completeStatic(reversePolicies...))
	cmd.Flags().StringSliceVar(&allow, "transfer-allow", nil,
		"who may transfer a zone, whole or incrementally, "+
			"as addresses or CIDR prefixes; empty is nobody")
	cmd.Flags().StringSliceVar(&notify, "notify", nil,
		"who is told when a zone changes, as addresses with an optional port and "+
			"an optional `key:<name>` after them; empty is nobody")
	f.register(cmd)
	return cmd
}

func runSettingsSet(
	ctx context.Context, opts *options, f *clientFlags,
	policy *string, allow *[]string, notify *[]string,
) error {
	var body gen.UpdateSettingsJSONRequestBody

	if policy != nil {
		// Refused here as well as by the server, so that a typo costs a message
		// rather than a round trip and a schema error naming a JSON field.
		if !validPolicy(*policy) {
			return fmt.Errorf("unknown reverse conflict policy %q: it is one of %s",
				*policy, strings.Join(reversePolicies, ", "))
		}
		p := gen.ReverseConflictPolicy(*policy)
		body.ReverseConflictPolicy = &p
	}

	if allow != nil {
		parsed, perr := apply.ParseTransferAllow(*allow)
		if perr != nil {
			return perr
		}
		text := apply.TransferAllowText(parsed)
		body.TransferAllow = &text
	}

	if notify != nil {
		parsed, perr := apply.ParseNotifyTargets(*notify)
		if perr != nil {
			return perr
		}
		text := apply.NotifyTargetsText(parsed)
		body.NotifyTargets = &text
	}

	client, err := f.client()
	if err != nil {
		return err
	}

	resp, err := client.UpdateSettingsWithResponse(ctx, body)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return printSettings(opts, *resp.JSON200)
}

// validPolicy reports whether v is one of the values D3 defines.
func validPolicy(v string) bool {
	for _, p := range reversePolicies {
		if v == p {
			return true
		}
	}
	return false
}

// printSettings renders the settings for both commands, so that setting one
// shows the same thing reading them does.
func printSettings(opts *options, s gen.Settings) error {
	got := serverSettings{
		ReverseConflictPolicy: string(s.ReverseConflictPolicy),
		TransferAllow:         s.TransferAllow,
		NotifyTargets:         s.NotifyTargets,
	}
	if got.TransferAllow == nil {
		got.TransferAllow = []string{}
	}
	if got.NotifyTargets == nil {
		got.NotifyTargets = []string{}
	}
	p := opts.Printer()
	return p.Print(got, func(w io.Writer) error {
		if _, werr := fmt.Fprintf(w, "reverse conflict policy  %s\n",
			p.Paint(output.ColorGreen, got.ReverseConflictPolicy)); werr != nil {
			return werr
		}
		who := p.Paint(output.ColorYellow, "nobody")
		if len(got.TransferAllow) > 0 {
			who = p.Paint(output.ColorGreen, strings.Join(got.TransferAllow, ", "))
		}
		if _, werr := fmt.Fprintf(w, "zone transfer to         %s\n", who); werr != nil {
			return werr
		}
		told := p.Paint(output.ColorYellow, "nobody")
		if len(got.NotifyTargets) > 0 {
			told = p.Paint(output.ColorGreen, strings.Join(got.NotifyTargets, ", "))
		}
		_, werr := fmt.Fprintf(w, "a change is announced to %s\n", told)
		return werr
	})
}
