package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
	"github.com/wegweiserzone/wegweiser/internal/metrics"
)

func newStatusCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Say how this server stands, and what it has been answering",
		Long: "How the server stands, then what it has been asked and how it answered.\n\n" +
			"The first lines answer whether everything is all right: what the server\n" +
			"serves, whether its cluster takes writes and how many voters it can lose\n" +
			"(`weg cluster status` has the members), and how many copies of its zones\n" +
			"on secondaries are known to be in step (`weg secondary status` has each).\n" +
			"A single server, or one with no secondaries, has no line for them.\n\n" +
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
		return err
	}
	if resp.HTTPResponse.StatusCode != http.StatusOK {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	got, err := metrics.Summarise(bytes.NewReader(resp.Body))
	if err != nil {
		return err
	}
	st := standingOf(ctx, client, got)

	p := opts.Printer()
	return p.Print(st, func(w io.Writer) error {
		if err := writeStanding(w, p, st); err != nil {
			return err
		}
		return writeStatus(w, p, got)
	})
}

// serverStanding is how the server stands, said before what it has been
// answering: whether its cluster takes writes, and whether the secondaries
// hold what it holds. The summary's own fields stay where they were, so a
// script reading them is not disturbed by the two that were added.
type serverStanding struct {
	*metrics.Summary
	Cluster     *clusterStanding `json:"cluster,omitempty"`
	Secondaries *copiesStanding  `json:"secondaries,omitempty"`

	status *gen.ClusterStatus
}

// clusterStanding is the majority a write needs, as the server asked counts it.
type clusterStanding struct {
	Voters   int    `json:"voters"`
	Needed   int    `json:"needed"`
	Answered int    `json:"answered"`
	Leader   string `json:"leader,omitempty"`
}

// copiesStanding counts the copies of zones on secondaries, and the ones known
// to be in step.
type copiesStanding struct {
	Copies int `json:"copies"`
	InStep int `json:"inStep"`
}

// standingOf asks for the cluster and the secondaries. Neither is required: a
// single server has no cluster and a server with no secondaries has nothing to
// say about them, and either question failing leaves its line out rather than
// failing the status.
func standingOf(ctx context.Context, client *gen.ClientWithResponses, s *metrics.Summary) *serverStanding {
	st := &serverStanding{Summary: s}
	if resp, err := client.GetClusterWithResponse(ctx); err == nil && resp.JSON200 != nil && resp.JSON200.Quorum != nil {
		q := resp.JSON200.Quorum
		st.status = resp.JSON200
		st.Cluster = &clusterStanding{Voters: q.Voters, Needed: q.Needed, Answered: q.Answered}
		for _, m := range resp.JSON200.Members {
			if m.Leader {
				st.Cluster.Leader = m.Id
			}
		}
	}
	if resp, err := client.GetSecondaryStatusWithResponse(ctx); err == nil && resp.JSON200 != nil && len(*resp.JSON200) > 0 {
		c := &copiesStanding{Copies: len(*resp.JSON200)}
		for _, pair := range *resp.JSON200 {
			if pair.State == gen.SecondaryStandingStateInStep {
				c.InStep++
			}
		}
		st.Secondaries = c
	}
	return st
}

// writeStanding is the answer to "is everything all right", a line for each
// thing that can be wrong, before the numbers.
func writeStanding(w io.Writer, p *output.Printer, st *serverStanding) error {
	label := func(s string) string { return p.Paint(output.ColorDim, fmt.Sprintf("%-11s", s)) }
	if _, err := fmt.Fprintf(w, "%s  %s\n", label("serving"), held(st.Summary)); err != nil {
		return err
	}
	if st.status != nil {
		if _, err := fmt.Fprintf(w, "%s  %s\n", label("cluster"), verdict(p, st.status)); err != nil {
			return err
		}
	}
	if c := st.Secondaries; c != nil {
		line := p.Paint(output.ColorGreen, fmt.Sprintf("all %d copies in step", c.Copies))
		if off := c.Copies - c.InStep; off > 0 {
			line = p.Paint(output.ColorYellow, fmt.Sprintf("%d of %d copies not known to be in step", off, c.Copies))
		}
		if _, err := fmt.Fprintf(w, "%s  %s\n", label("secondaries"), line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// writeStatus renders the summary for a terminal.
func writeStatus(w io.Writer, p *output.Printer, s *metrics.Summary) error {
	// What the snapshot holds is the serving line above, and is not said twice.
	if s.Queries == 0 {
		_, err := fmt.Fprintln(w, "no queries answered yet")
		return err
	}

	if _, err := fmt.Fprintf(w, "%d queries answered  %s\n", s.Queries, transportSplit(s)); err != nil {
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
