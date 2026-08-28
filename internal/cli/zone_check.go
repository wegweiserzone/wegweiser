package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
)

// zoneChecked is what a check reports.
type zoneChecked struct {
	Zone    string `json:"zone"`
	Records int    `json:"records"`

	// Errors and Warnings are counted here so that a caller reading the JSON
	// does not have to tally the list to learn whether anything is actually
	// wrong. What each means is D31.
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`

	Findings  []zoneFinding `json:"findings"`
	Truncated bool          `json:"truncated,omitempty"`
}

// zoneFinding is one thing wrong with the zone.
type zoneFinding struct {
	Severity string `json:"severity"`
	Scope    string `json:"scope"`
	Name     string `json:"name"`
	Detail   string `json:"detail"`
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
		if got.Severity == gen.Error {
			report.Errors++
		} else {
			report.Warnings++
		}
		report.Findings = append(report.Findings, zoneFinding{
			Severity: string(got.Severity), Scope: string(got.Scope),
			Name: got.Name, Detail: got.Detail,
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
		report.Zone, summarise(report),
		counted(report.Records, "record")); err != nil {
		return err
	}
	for i, got := range report.Findings {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "  %s  %s  %s\n    %s\n",
			got.Severity, got.Scope, got.Name, got.Detail); err != nil {
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

// summarise says what was found, so a zone missing a glue record does not read
// like one holding a record nothing can answer (D31).
func summarise(report zoneChecked) string {
	switch {
	case report.Warnings == 0:
		return counted(report.Errors, "error")
	case report.Errors == 0:
		return counted(report.Warnings, "warning")
	default:
		return counted(report.Errors, "error") + " and " +
			counted(report.Warnings, "warning")
	}
}
