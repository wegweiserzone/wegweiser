package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// newQueryCommand groups everything about the queries this server is answering.
func newQueryCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "query",
		Aliases: []string{"q"},
		Short:   "Watch the queries the server is answering",
		Args:    usageArgs(cobra.NoArgs),
		RunE:    func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newQueryTailCommand(opts, &f))
	return cmd
}

// tailFilter is what the user asked to see.
type tailFilter struct {
	name   string
	types  []string
	rcodes []string
	client string
}

func newQueryTailCommand(opts *options, f *clientFlags) *cobra.Command {
	var filter tailFilter

	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Follow the queries as they are answered",
		Long: "Follow the queries this server answers, as it answers them.\n\n" +
			"The filter is applied on the server, before anything is buffered, so " +
			"watching one zone stays complete however busy the rest of the server is. " +
			"When more matches than a stream carries, it samples and says so on standard " +
			"error rather than quietly leaving them out.",
		Example: strings.Join([]string{
			"  weg query tail",
			"  weg query tail --name example.com.",
			"  weg query tail --type A --type AAAA",
			"  weg query tail --rcode NXDOMAIN --rcode REFUSED",
			"  weg query tail --client 192.0.2.0/24",
			"  weg query tail --output json | jq -r 'select(.rcode != \"NOERROR\") | .name'",
		}, "\n"),
		Args:         usageArgs(cobra.NoArgs),
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runQueryTail(c.Context(), opts, f, filter)
		},
	}

	cmd.Flags().StringVar(&filter.name, "name", "",
		"watch a name and everything below it")
	cmd.Flags().StringArrayVar(&filter.types, "type", nil,
		"watch only these question types (repeatable)")
	cmd.Flags().StringArrayVar(&filter.rcodes, "rcode", nil,
		"watch only these response codes, by mnemonic (repeatable)")
	cmd.Flags().StringVar(&filter.client, "client", "",
		"watch only this address or network in CIDR notation")

	registerFlagCompletion(cmd, "name", func(
		c *cobra.Command, _ []string, prefix string,
	) ([]string, cobra.ShellCompDirective) {
		return completeZones(f)(c, nil, prefix)
	})
	registerFlagCompletion(cmd, "type", completeStatic(recordTypes...))
	registerFlagCompletion(cmd, "rcode", completeStatic(
		"NOERROR", "FORMERR", "SERVFAIL", "NXDOMAIN", "NOTIMP", "REFUSED", "BADVERS"))
	return cmd
}

func runQueryTail(ctx context.Context, opts *options, f *clientFlags, filter tailFilter) error {
	client, err := f.streamClient()
	if err != nil {
		return err
	}

	params := &gen.StreamQueriesParams{}
	if filter.name != "" {
		params.Name = &filter.name
	}
	if len(filter.types) > 0 {
		params.Type = &filter.types
	}
	if len(filter.rcodes) > 0 {
		params.Rcode = &filter.rcodes
	}
	if filter.client != "" {
		params.Client = &filter.client
	}

	resp, err := client.StreamQueries(ctx, params)
	if err != nil {
		return reachable(err, client.Server)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck // the status is the error
		return apiError(resp.StatusCode, body)
	}

	return followStream(ctx, resp.Body, opts.Printer())
}

// followStream reads Server-Sent Events until the stream or the context ends.
func followStream(ctx context.Context, body io.Reader, p *output.Printer) error {
	t := &tail{p: p, status: output.New(p.ErrOut(), p.ErrOut(), p.Format())}
	if err := t.header(); err != nil {
		return err
	}

	scan := bufio.NewScanner(body)
	// A single event is a line of JSON holding one exchange, which is far
	// below this. The bound is here because a scanner without one stops at
	// 64 KiB and reports a line that was merely long as the end of the stream.
	scan.Buffer(make([]byte, 0, 64<<10), 1<<20)

	// Anything that is neither of these two fields is skipped: a comment, which
	// the format uses to keep a connection alive, and the fields this command
	// has no use for.
	var name string
	for scan.Scan() {
		line := scan.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")

		case strings.HasPrefix(line, "data: "):
			if err := t.dispatch(name, strings.TrimPrefix(line, "data: ")); err != nil {
				return err
			}
		}
	}

	select {
	case <-ctx.Done():
		// Stopped from this end. Ctrl-C is how a tail is meant to end, which
		// is the rule weg serve already follows for a signal: interrupting a
		// command that runs until it is interrupted is not a failure of it,
		// and whatever the read failed with on the way out says nothing.
		return nil
	default:
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("read the stream: %w", err)
	}
	return nil
}

// tail renders what arrives.
type tail struct {
	p *output.Printer

	// status is a printer over standard error. What the stream is leaving out
	// is a diagnostic rather than a result: standard output stays one shape in
	// every format, so `weg query tail --output json | jq` sees exchanges and
	// nothing else.
	status *output.Printer

	wroteHeader bool
	lastRatio   int
	lastDropped int
}

func (t *tail) dispatch(name, data string) error {
	switch name {
	case "query":
		var ev gen.QueryEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("read an event from the stream: %w", err)
		}
		return t.event(ev)

	case "status":
		var st gen.StreamStatus
		if err := json.Unmarshal([]byte(data), &st); err != nil {
			return fmt.Errorf("read a status from the stream: %w", err)
		}
		return t.stat(st)

	default:
		// A message this version does not know about is not a reason to stop
		// showing the ones it does.
		return nil
	}
}

// header writes the column titles, once, and only where there are columns.
func (t *tail) header() error {
	if t.p.Format() != output.FormatText || t.wroteHeader {
		return nil
	}
	t.wroteHeader = true
	_, err := fmt.Fprintln(t.p.Out(), t.p.Paint(output.ColorDim, fmt.Sprintf(
		"%-12s  %-21s  %-3s  %-9s  %8s  %5s  %-6s  %s",
		"TIME", "CLIENT", "TR", "RCODE", "LATENCY", "SIZE", "TYPE", "NAME")))
	return err
}

func (t *tail) event(ev gen.QueryEvent) error {
	if t.p.Format() == output.FormatYAML {
		// A stream of YAML is a stream of documents, and a document begins
		// with a marker. Without it a reader sees one mapping with every key
		// repeated.
		if _, err := io.WriteString(t.p.Out(), "---\n"); err != nil {
			return err
		}
	}

	return t.p.Print(ev, func(w io.Writer) error {
		rcode, colour := ev.Rcode, output.ColorGreen
		switch {
		case ev.Dropped:
			rcode, colour = "DROPPED", output.ColorRed
		case ev.Rcode == "NXDOMAIN":
			colour = output.ColorYellow
		case ev.Rcode != "NOERROR":
			colour = output.ColorRed
		}

		client := ev.Client
		if ev.Port != nil {
			client = fmt.Sprintf("%s:%d", ev.Client, *ev.Port)
		}

		_, err := fmt.Fprintf(w, "%-12s  %-21s  %-3s  %s  %8s  %5d  %-6s  %s\n",
			ev.At.Local().Format("15:04:05.000"),
			client,
			ev.Transport,
			t.p.Paint(colour, fmt.Sprintf("%-9s", rcode)),
			latency(ev.LatencyUs),
			ev.Size,
			ev.Type,
			ev.Name,
		)
		return err
	})
}

// stat says what the stream is leaving out, and only when it is leaving
// something out: a stream showing everything has nothing to report, and saying
// so on every heartbeat would be noise a person learns to ignore.
func (t *tail) stat(st gen.StreamStatus) error {
	if st.Ratio == t.lastRatio && st.Dropped == t.lastDropped {
		return nil
	}
	t.lastRatio, t.lastDropped = st.Ratio, st.Dropped
	if st.Ratio <= 1 && st.Dropped == 0 {
		return nil
	}

	return t.status.Print(st, func(w io.Writer) error {
		parts := make([]string, 0, 2)
		if st.Ratio > 1 {
			parts = append(parts, fmt.Sprintf("showing 1 query in %d", st.Ratio))
		}
		if st.Dropped > 0 {
			parts = append(parts, fmt.Sprintf("%d dropped because this end was too slow", st.Dropped))
		}
		_, err := fmt.Fprintln(w, t.p.Paint(output.ColorDim, "— "+strings.Join(parts, ", ")))
		return err
	})
}

// latency renders microseconds the way a person reads them: a query answered
// from memory is tens of microseconds, and printing that as "0.00 ms" would
// throw away the only digits that vary.
func latency(us int) string {
	switch {
	case us < 1000:
		return fmt.Sprintf("%dµs", us)
	case us < 1_000_000:
		return fmt.Sprintf("%.2fms", float64(us)/1000)
	default:
		return fmt.Sprintf("%.2fs", float64(us)/1_000_000)
	}
}
