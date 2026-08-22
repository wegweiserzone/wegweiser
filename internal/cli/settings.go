package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// reversePolicies are the values the reverse conflict policy accepts, in the
// order docs/decisions.md D3 argues them.
var reversePolicies = []string{
	string(gen.FirstWins), string(gen.LastWins), string(gen.Multi), string(gen.Reject),
}

// serverSettings is what the commands here report.
type serverSettings struct {
	ReverseConflictPolicy string `json:"reverseConflictPolicy"`
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
			"  weg settings set --reverse-conflict-policy reject --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			if !c.Flags().Changed("reverse-conflict-policy") {
				return errors.New("nothing to change: name a setting, or see `weg settings show`")
			}
			return runSettingsSet(c.Context(), opts, &f, policy)
		},
	}
	cmd.Flags().StringVar(&policy, "reverse-conflict-policy", "",
		"what to do when an address already answers with another name: "+
			strings.Join(reversePolicies, ", "))
	registerFlagCompletion(cmd, "reverse-conflict-policy", completeStatic(reversePolicies...))
	f.register(cmd)
	return cmd
}

func runSettingsSet(ctx context.Context, opts *options, f *clientFlags, policy string) error {
	// Refused here as well as by the server, so that a typo costs a message
	// rather than a round trip and a schema error naming a JSON field.
	if !validPolicy(policy) {
		return fmt.Errorf("unknown reverse conflict policy %q: it is one of %s",
			policy, strings.Join(reversePolicies, ", "))
	}

	client, err := f.client()
	if err != nil {
		return err
	}

	p := gen.ReverseConflictPolicy(policy)
	resp, err := client.UpdateSettingsWithResponse(ctx, gen.UpdateSettingsJSONRequestBody{
		ReverseConflictPolicy: &p,
	})
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
	got := serverSettings{ReverseConflictPolicy: string(s.ReverseConflictPolicy)}
	p := opts.Printer()
	return p.Print(got, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w, "reverse conflict policy  %s\n",
			p.Paint(output.ColorGreen, got.ReverseConflictPolicy))
		return werr
	})
}
