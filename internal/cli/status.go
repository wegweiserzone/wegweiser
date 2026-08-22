package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/cli/output"
	"github.com/wegweiserzone/wegweiser/internal/metrics"
)

func newStatusCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report what this server has been answering",
		Long: "What the server has been asked, and how it answered.\n\n" +
			"`weg zone list` says what a server holds. This says what it has been\n" +
			"doing: answers by response code, questions by type, and where the\n" +
			"latencies fall against the sub-millisecond target.\n\n" +
			"The numbers are counters since the process started, read from the same\n" +
			"metrics a monitoring system scrapes, which is the only place that knows\n" +
			"what has been *asked* rather than what is *there*.",
		Args: usageArgs(cobra.NoArgs),
		Example: "  weg status\n" +
			"  weg status --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			return runStatus(c.Context(), opts, &f)
		},
	}
	f.register(cmd)
	return cmd
}

func runStatus(ctx context.Context, opts *options, f *clientFlags) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	resp, err := client.GetMetricsWithResponse(ctx)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.HTTPResponse.StatusCode != http.StatusOK {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	got, err := metrics.Summarise(bytes.NewReader(resp.Body))
	if err != nil {
		return err
	}

	p := opts.Printer()
	return p.Print(got, func(w io.Writer) error { return writeStatus(w, p, got) })
}

// writeStatus renders the summary for a terminal.
func writeStatus(w io.Writer, p *output.Printer, s *metrics.Summary) error {
	if s.Queries == 0 {
		_, err := fmt.Fprintf(w, "no queries answered yet: the snapshot holds %s\n",
			held(s))
		return err
	}

	if _, err := fmt.Fprintf(w, "%d queries answered  %s  %s\n",
		s.Queries, transportSplit(s), held(s)); err != nil {
		return err
	}
	if s.Dropped > 0 || s.Truncated > 0 {
		if _, err := fmt.Fprintf(w, "%d dropped, %d truncated\n",
			s.Dropped, s.Truncated); err != nil {
			return err
		}
	}

	if s.WithinTarget >= 0 {
		colour := output.ColorGreen
		if s.WithinTarget < 0.99 {
			colour = output.ColorYellow
		}
		if _, err := fmt.Fprintf(w, "%s answered inside a millisecond\n",
			p.Paint(colour, fmt.Sprintf("%.1f%%", s.WithinTarget*100))); err != nil {
			return err
		}
	}

	if err := writeCounts(w, "By response code", s.ByRcode, s.Queries); err != nil {
		return err
	}
	return writeCounts(w, "By question type", s.ByType, s.Queries)
}

// held describes the snapshot being answered from.
func held(s *metrics.Summary) string {
	return fmt.Sprintf("%d %s, %d %s",
		s.Zones, plural(s.Zones, "zone"), s.Records, plural(s.Records, "record"))
}

// plural is the English "add an s unless there is exactly one" rule, which is
// all this package needs.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// transportSplit reports the transports only when both were used, because "0
// over TCP" on a server nobody has opened a connection to is noise.
func transportSplit(s *metrics.Summary) string {
	switch {
	case s.TCP == 0:
		return "all over UDP"
	case s.UDP == 0:
		return "all over TCP"
	default:
		return fmt.Sprintf("%d UDP, %d TCP", s.UDP, s.TCP)
	}
}

// writeCounts prints one breakdown, with a bar so the shape is readable without
// dividing anything in your head.
func writeCounts(w io.Writer, title string, counts []metrics.Count, total uint64) error {
	if len(counts) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", title); err != nil {
		return err
	}

	widest := 0
	for _, c := range counts {
		widest = max(widest, len(c.Label))
	}
	for _, c := range counts {
		share := float64(c.Count) / float64(total)
		if _, err := fmt.Fprintf(w, "  %-*s  %8d  %5.1f%%  %s\n",
			widest, c.Label, c.Count, share*100, bar(share)); err != nil {
			return err
		}
	}
	return nil
}

// bar draws a share as twenty columns, so a glance sorts the list.
func bar(share float64) string {
	const width = 20
	filled := int(math.Round(share * width))
	return string(bytes.Repeat([]byte("█"), filled))
}
