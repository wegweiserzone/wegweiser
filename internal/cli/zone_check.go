package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// zoneChecked is what a check reports.
type zoneChecked struct {
	Zone      string        `json:"zone"`
	Records   int           `json:"records"`
	Findings  []zoneFinding `json:"findings"`
	Truncated bool          `json:"truncated,omitempty"`
}

// zoneFinding is one thing wrong with the zone.
type zoneFinding struct {
	Scope  string `json:"scope"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

func newZoneCheckCommand(opts *options, f *clientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check ZONE",
		Short: "Report what is wrong with a zone as it stands",
		Long: "Apply the rules the write path enforces to the zone as it is stored,\n" +
			"and list everything that fails instead of stopping at the first.\n\n" +
			"A zone edited only through this server stays sound, so a quiet check\n" +
			"is the ordinary answer. What this reaches is data the write path never\n" +
			"saw: written before a rule existed, or put there by a hand on the\n" +
			"database file.\n\n" +
			"Findings are the answer rather than a failure, so the exit status is\n" +
			"zero either way. With --output json the list is what a script reads.",
		Args:    usageArgs(cobra.ExactArgs(1)),
		Example: "  weg zone check example.com\n  weg zone check example.com --output json",

		RunE: func(c *cobra.Command, args []string) error {
			return runZoneCheck(c.Context(), opts, f, args[0])
		},
		ValidArgsFunction: completeZones(f),
	}
	return cmd
}

func runZoneCheck(ctx context.Context, opts *options, f *clientFlags, name string) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	z, err := findZone(ctx, client, f, name)
	if err != nil {
		return err
	}

	resp, err := client.CheckZoneWithResponse(ctx, z.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	report := zoneChecked{
		Zone:      z.Name,
		Records:   resp.JSON200.Records,
		Truncated: resp.JSON200.Truncated,
		Findings:  make([]zoneFinding, 0, len(resp.JSON200.Findings)),
	}
	for _, got := range resp.JSON200.Findings {
		report.Findings = append(report.Findings, zoneFinding{
			Scope: string(got.Scope), Name: got.Name, Detail: got.Detail,
		})
	}

	return opts.Printer().Print(report, func(w io.Writer) error {
		return writeCheck(w, report)
	})
}

// writeCheck renders a report for a person: a block per finding rather than a
// table, because the sentence is the useful part and a column would wrap it.
func writeCheck(w io.Writer, report zoneChecked) error {
	if len(report.Findings) == 0 {
		_, err := fmt.Fprintf(w, "%s is sound: %s checked.\n",
			report.Zone, counted(report.Records, "record"))
		return err
	}

	if _, err := fmt.Fprintf(w, "%s: %s in %s.\n\n",
		report.Zone, counted(len(report.Findings), "finding"),
		counted(report.Records, "record")); err != nil {
		return err
	}
	for i, got := range report.Findings {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "  %s  %s\n    %s\n",
			got.Scope, got.Name, got.Detail); err != nil {
			return err
		}
	}
	if report.Truncated {
		if _, err := fmt.Fprintf(w,
			"\nThe list stops here. A zone with this many findings has one fault rather\n"+
				"than %d, and the rest would say the same thing again.\n",
			len(report.Findings)); err != nil {
			return err
		}
	}
	return nil
}

// counted writes a number with its noun, so a report does not say "1 findings".
func counted(n int, word string) string {
	return fmt.Sprintf("%d %s", n, plural(n, word))
}
