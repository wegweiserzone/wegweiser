package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// idpChecked is what a check of a provider reports.
type idpChecked struct {
	Name   string             `json:"name"`
	OK     bool               `json:"ok"`
	Steps  []gen.CheckStep    `json:"steps"`
	Person *gen.CheckedPerson `json:"person,omitempty"`
}

func newIDPCheckCommand(opts *options, f *clientFlags) *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "check NAME",
		Short: "Try what a sign-in through a provider needs",
		Long: "Try what a sign-in through a provider needs, short of somebody signing\n" +
			"in, from the server asked, and say what does not work.\n\n" +
			"An OpenID Connect provider is asked for its discovery document. Its\n" +
			"client secret is only tried when somebody signs in, and the check says\n" +
			"so. A directory is connected to and bound to, and its bases have to be\n" +
			"there. With --username a directory is also asked who that is, which\n" +
			"groups they are in and the scope those give them, without a password.\n\n" +
			"A failed step is the answer rather than a failure of the command, so the\n" +
			"exit status is zero either way. With --output json, ok is what a script\n" +
			"reads.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg idp check Authentik\n" +
			"  weg idp check FreeIPA --username alice",
		ValidArgsFunction: completeIDPs(f),
		RunE: func(c *cobra.Command, args []string) error {
			return runIDPCheck(c.Context(), opts, f, args[0], username)
		},
	}
	cmd.Flags().StringVar(&username, "username", "",
		"for a directory: somebody to look up as a sign-in would, without their password")
	return cmd
}

func runIDPCheck(ctx context.Context, opts *options, f *clientFlags, name, username string) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	p, err := pickIDP(ctx, client, name)
	if err != nil {
		return err
	}
	body := gen.IdentityProviderCheckRequest{Id: &p.Id}
	if username != "" {
		body.Username = &username
	}
	return checkIDP(ctx, opts, client, p.Name, body)
}

// checkIDP asks for a check and prints what came of it, for a saved provider
// or for a draft that --dry-run is trying before it is saved.
func checkIDP(
	ctx context.Context, opts *options, client *gen.ClientWithResponses, name string, body gen.IdentityProviderCheckRequest,
) error {
	resp, err := client.CheckIdentityProviderWithResponse(ctx, body)
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	report := idpChecked{Name: name, OK: resp.JSON200.Ok, Steps: resp.JSON200.Steps, Person: resp.JSON200.Person}
	pr := opts.Printer()
	return pr.Print(report, func(w io.Writer) error {
		return writeIDPCheck(w, pr, report)
	})
}

// writeIDPCheck renders a check for a person: a block per step, because the
// sentence is the useful part and a column would wrap it.
func writeIDPCheck(w io.Writer, pr *output.Printer, report idpChecked) error {
	verdict := "works as far as can be told from here"
	if !report.OK {
		verdict = "does not work as it stands"
	}
	if _, err := fmt.Fprintf(w, "%s %s.\n\n", report.Name, verdict); err != nil {
		return err
	}

	colours := map[gen.CheckStepStatus]output.Color{
		gen.CheckStepStatusOk:      output.ColorGreen,
		gen.CheckStepStatusFailed:  output.ColorRed,
		gen.CheckStepStatusUnknown: output.ColorYellow,
	}
	for _, s := range report.Steps {
		status := fmt.Sprintf("%-7s", s.Status)
		if _, err := fmt.Fprintf(w, "  %s  %s\n           %s\n",
			pr.Paint(colours[s.Status], status), s.Step, s.Detail); err != nil {
			return err
		}
	}

	if p := report.Person; p != nil {
		scope := "no scope"
		if p.Scope != nil {
			scope = string(*p.Scope)
		}
		groups := "no group"
		if len(p.Groups) > 0 {
			groups = strings.Join(p.Groups, "\n           ")
		}
		if _, err := fmt.Fprintf(w, "\n  %s is %s, with %s.\n  groups   %s\n",
			p.Name, p.Dn, scope, groups); err != nil {
			return err
		}
	}
	return nil
}
