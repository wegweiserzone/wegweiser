package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// newAskCommand asks the server what it answers for a question.
func newAskCommand(opts *options) *cobra.Command {
	f := clientFlags{opts: opts}

	cmd := &cobra.Command{
		Use:   "ask NAME [TYPE]",
		Short: "Say what the server answers for a name",
		Long: "Say what the server answers for a name and a type (A when none is given),\n" +
			"as a query over TCP would be told, and what kind of answer that is.\n\n" +
			"It asks over the API, so it needs no route to the DNS port and no dig: the\n" +
			"server decides the answer from the data it is serving, by the same function\n" +
			"that answers queries. Asked of a cluster member, it is that member's answer.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		Example: "  weg ask www.example.com\n" +
			"  weg ask example.com MX\n" +
			"  weg ask 10.2.0.192.in-addr.arpa PTR\n" +
			"  weg ask www.example.com AAAA --output json",

		RunE: func(c *cobra.Command, args []string) error {
			typ := "A"
			if len(args) == 2 {
				typ = strings.ToUpper(args[1])
			}
			return runAsk(c.Context(), opts, &f, args[0], typ)
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 1 {
				return recordTypes, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	f.register(cmd)
	return cmd
}

func runAsk(ctx context.Context, opts *options, f *clientFlags, name, typ string) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	resp, err := client.AskWithResponse(ctx, &gen.AskParams{Name: name, Type: &typ})
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	got := resp.JSON200

	p := opts.Printer()
	return p.Print(got, func(w io.Writer) error {
		if _, werr := fmt.Fprintln(w, verdictOfAsk(p, got)); werr != nil {
			return werr
		}
		for _, section := range []struct {
			title   string
			records []gen.AnsweredRecord
		}{
			{"answer", got.Answer},
			{"authority", got.Authority},
			{"additional", got.Additional},
		} {
			if len(section.records) == 0 {
				continue
			}
			if _, werr := fmt.Fprintf(w, "\n%s\n", p.Paint(output.ColorDim, section.title)); werr != nil {
				return werr
			}
			t := newRows(w, 5)
			for _, r := range section.records {
				t.row(r.Name, strconv.FormatInt(r.Ttl, 10), r.Class, r.Type, r.Data)
			}
			if werr := t.flush(); werr != nil {
				return werr
			}
		}
		return nil
	})
}

// verdictOfAsk is the answer in a sentence, before the records it is made of:
// the response code and the zone that gave it, then what kind of answer it is.
func verdictOfAsk(p *output.Printer, a *gen.Asked) string {
	code := a.Rcode
	switch a.Outcome {
	case gen.AskedOutcomeNxdomain, gen.AskedOutcomeRefused, gen.AskedOutcomeNotImplemented:
		code = p.Paint(output.ColorYellow, code)
	case gen.AskedOutcomeFailed:
		code = p.Paint(output.ColorRed, code)
	case gen.AskedOutcomeAlias:
		if a.Rcode != "NOERROR" {
			code = p.Paint(output.ColorYellow, code)
		}
	case gen.AskedOutcomeAnswer, gen.AskedOutcomeNodata, gen.AskedOutcomeReferral:
	}
	if a.Zone != nil {
		code += " from " + *a.Zone
	}

	n := len(a.Answer)
	what := ""
	switch a.Outcome {
	case gen.AskedOutcomeAnswer:
		what = fmt.Sprintf("%d %s", n, plural(n, "record"))
	case gen.AskedOutcomeAlias:
		what = fmt.Sprintf("an alias, %d %s starting with the CNAME", n, plural(n, "record"))
		if a.Rcode == "NXDOMAIN" {
			what += ", and the name it leads to does not exist"
		}
	case gen.AskedOutcomeNodata:
		what = fmt.Sprintf("%s exists and has no %s record", a.Name, a.Type)
	case gen.AskedOutcomeNxdomain:
		what = "no such name"
	case gen.AskedOutcomeReferral:
		what = "a referral; the name is delegated, and the name servers below answer for it"
	case gen.AskedOutcomeRefused, gen.AskedOutcomeNotImplemented, gen.AskedOutcomeFailed:
	}
	if a.Explanation != nil {
		if what != "" {
			what += "; "
		}
		what += *a.Explanation
	}
	if what == "" {
		return code
	}
	return code + ": " + what
}
