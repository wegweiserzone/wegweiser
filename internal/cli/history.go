package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// newHistoryCommand groups everything that reads what was changed.
func newHistoryCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "history",
		Aliases: []string{"h", "log"},
		Short:   "Read what was changed, by whom and when",
		Long: "Every write is a commit, so this is the audit log, the diff view and\n" +
			"the source an earlier state is restored from, all reading from the one\n" +
			"structure.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newHistoryListCommand(opts, &f))
	cmd.AddCommand(newHistoryShowCommand(opts, &f))
	return cmd
}

// commitListed is one line of history.
type commitListed struct {
	ID         string    `json:"id"`
	Zone       string    `json:"zone"`
	Kind       string    `json:"kind"`
	SerialFrom int64     `json:"serialFrom"`
	SerialTo   int64     `json:"serialTo"`
	RevertsTo  *int64    `json:"revertsTo,omitempty"`
	Source     string    `json:"source"`
	Actor      string    `json:"actor,omitempty"`
	Comment    string    `json:"comment,omitempty"`
	At         time.Time `json:"at"`
}

func newHistoryListCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		kinds   []string
		sources []string
		actor   string
		since   string
		until   string
		limit   int
	)

	cmd := &cobra.Command{
		Use:     "list [ZONE]",
		Aliases: []string{"ls"},
		Short:   "List the commits, newest first",
		Long: "List what was changed. With a ZONE, only that zone's history.\n\n" +
			"A commit's identifier is what `weg history show` takes, and the serial\n" +
			"it produced is what `weg zone rollback` takes.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		Example: "  weg history list\n" +
			"  weg history list example.com\n" +
			"  weg history list example.com --kind rollback\n" +
			"  weg history list --source api --source cli   # what people did\n" +
			"  weg history list --since 2026-08-01 --actor alice",

		RunE: func(c *cobra.Command, args []string) error {
			var params gen.ListCommitsParams
			for _, k := range kinds {
				kind := gen.CommitKind(strings.ToLower(k))
				if !kind.Valid() {
					return usageError{fmt.Errorf(
						"%q is not a kind of commit; they are edit, import, rollback, "+
							"zone_create, zone_update and zone_delete", k)}
				}
				if params.Kind == nil {
					params.Kind = &[]gen.CommitKind{}
				}
				*params.Kind = append(*params.Kind, kind)
			}
			for _, name := range sources {
				src := gen.CommitSource(strings.ToLower(name))
				if !src.Valid() {
					return usageError{fmt.Errorf(
						"%q is not a cause; they are api, cli, import and system", name)}
				}
				if params.Source == nil {
					params.Source = &[]gen.CommitSource{}
				}
				*params.Source = append(*params.Source, src)
			}
			if actor != "" {
				params.Actor = &actor
			}
			for _, when := range []struct {
				flag string
				text string
				out  **time.Time
			}{
				{"since", since, &params.Since},
				{"until", until, &params.Until},
			} {
				if when.text == "" {
					continue
				}
				t, err := parseWhen(when.text)
				if err != nil {
					return usageError{fmt.Errorf("--%s: %w", when.flag, err)}
				}
				*when.out = &t
			}
			zone := ""
			if len(args) == 1 {
				zone = args[0]
			}
			return runHistoryList(c.Context(), opts, f, zone, params, limit)
		},
		ValidArgsFunction: completeZones(f),
	}

	cmd.Flags().StringArrayVar(&kinds, "kind", nil, "only these kinds of commit (repeatable)")
	cmd.Flags().StringArrayVar(&sources, "source", nil,
		"only changes with these causes: api, cli, import, system; system is the "+
			"server's own doing, such as the reverse entries it keeps in step (repeatable)")
	cmd.Flags().StringVar(&actor, "actor", "", "only what this actor did")
	cmd.Flags().StringVar(&since, "since", "", "only at or after this time (a date, or a date and time)")
	cmd.Flags().StringVar(&until, "until", "", "only before this time")
	cmd.Flags().IntVar(&limit, "limit", 50,
		"stop after this many commits (0 is all of them)")
	registerFlagCompletion(cmd, "source", completeStatic("api", "cli", "import", "system"))
	registerFlagCompletion(cmd, "kind", completeStatic(
		"edit", "import", "rollback", "zone_create", "zone_update", "zone_delete"))
	return cmd
}

// parseWhen reads the ways a person writes a moment, shortest first.
func parseWhen(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		// Local, not UTC: somebody typing a date means the day they are having.
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time; try 2026-08-19, 2026-08-19 14:30, "+
		"or a full RFC 3339 timestamp", s)
}

func runHistoryList(
	ctx context.Context, opts *options, f *clientFlags,
	zoneName string, params gen.ListCommitsParams, limit int,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	if zoneName != "" {
		z, zerr := findZone(ctx, client, f, zoneName)
		if zerr != nil {
			return zerr
		}
		params.ZoneId = &z.Id
	}

	commits, err := allCommits(ctx, client, f, params, limit)
	if err != nil {
		return err
	}

	listed := make([]commitListed, 0, len(commits))
	for i := range commits {
		listed = append(listed, listCommit(&commits[i]))
	}

	p := opts.Printer()
	return p.Print(listed, func(w io.Writer) error {
		if len(listed) == 0 {
			_, werr := fmt.Fprintln(w, "no commits match that")
			return werr
		}

		t := newTable(w, "WHEN", "ZONE", "SERIAL", "KIND", "WHO", "COMMENT")
		for i := range listed {
			c := &listed[i]
			who := c.Actor
			if who == "" {
				who = c.Source
			}
			comment := c.Comment
			if c.RevertsTo != nil {
				// The one fact a rollback carries that nothing else does, and
				// the one a person scanning the log is looking for.
				comment = strings.TrimSpace(fmt.Sprintf("back to serial %d %s", *c.RevertsTo, comment))
			}
			t.row(
				c.At.Local().Format("2006-01-02 15:04:05"),
				c.Zone,
				fmt.Sprintf("%d→%d", c.SerialFrom, c.SerialTo),
				c.Kind,
				who,
				comment,
			)
		}
		return t.flush()
	})
}

// allCommits follows the cursor to the end, or until limit rows are in hand.
func allCommits(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags,
	params gen.ListCommitsParams, limit int,
) ([]gen.Commit, error) {
	var out []gen.Commit
	size := pageSize
	if limit > 0 && limit < size {
		size = limit
	}
	params.Limit = ptr(size)

	for {
		resp, err := client.ListCommitsWithResponse(ctx, &params)
		if err != nil {
			return nil, reachable(err, f.server)
		}
		if resp.JSON200 == nil {
			return nil, apiError(resp.HTTPResponse.StatusCode, resp.Body)
		}

		out = append(out, resp.JSON200.Items...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
		if resp.JSON200.NextCursor == nil {
			return out, nil
		}
		params.Cursor = resp.JSON200.NextCursor
	}
}

func listCommit(c *gen.Commit) commitListed {
	return commitListed{
		ID:         c.Id,
		Zone:       c.ZoneName,
		Kind:       string(c.Kind),
		SerialFrom: c.SerialFrom,
		SerialTo:   c.SerialTo,
		RevertsTo:  c.RevertsTo,
		Source:     string(c.Source),
		Actor:      opt(c.Actor, ""),
		Comment:    opt(c.Comment, ""),
		At:         c.CreatedAt,
	}
}

// commitShown is one commit with what it did.
type commitShown struct {
	commitListed
	Events []commitEvent `json:"events"`
}

type commitEvent struct {
	Op    string `json:"op"`
	Name  string `json:"name"`
	TTL   int64  `json:"ttl"`
	Class string `json:"class"`
	Type  string `json:"type"`
	Data  string `json:"data"`
}

func newHistoryShowCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "show COMMIT",
		Aliases: []string{"get", "diff"},
		Short:   "Print one commit and what it changed",
		Long: "Print a commit and the records it removed and added, as a diff.\n\n" +
			"There is no \"modify\" in the history: a change is a removal and an\n" +
			"addition, which is how RFC 1995 §2 expresses it too. Both directions\n" +
			"carry the whole record, so what is printed is enough to undo it as\n" +
			"well as to replay it.",
		Args:    usageArgs(cobra.ExactArgs(1)),
		Example: "  weg history show 01K2XQ8N4G7BVYJ0MZ9WTREHPD",

		RunE: func(c *cobra.Command, args []string) error {
			return runHistoryShow(c.Context(), opts, f, args[0])
		},
	}
}

func runHistoryShow(ctx context.Context, opts *options, f *clientFlags, id string) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	resp, err := client.GetCommitWithResponse(ctx, id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	c := resp.JSON200

	shown := commitShown{commitListed: listCommit(c)}
	for _, e := range deref(c.Events, nil) {
		shown.Events = append(shown.Events, commitEvent{
			Op: string(e.Op), Name: e.Name, TTL: e.Ttl,
			Class: e.Class, Type: e.Type, Data: e.Data,
		})
	}

	p := opts.Printer()
	return p.Print(shown, func(w io.Writer) error {
		who := shown.Actor
		if who == "" {
			who = shown.Source
		}
		header := fmt.Sprintf("%s  %s  %s  serial %d→%d  by %s",
			shown.ID, shown.At.Local().Format("2006-01-02 15:04:05"),
			shown.Kind, shown.SerialFrom, shown.SerialTo, who)
		if _, werr := fmt.Fprintln(w, header); werr != nil {
			return werr
		}
		if shown.Comment != "" {
			if _, werr := fmt.Fprintf(w, "%s\n", shown.Comment); werr != nil {
				return werr
			}
		}
		if shown.RevertsTo != nil {
			if _, werr := fmt.Fprintf(w, "restored the zone to serial %d\n", *shown.RevertsTo); werr != nil {
				return werr
			}
		}
		if len(shown.Events) == 0 {
			_, werr := fmt.Fprintln(w, "\nno records changed")
			return werr
		}
		if _, werr := fmt.Fprintln(w); werr != nil {
			return werr
		}

		// A diff, in the shape everybody already reads one in. All the
		// removals come before all the additions, which is the order RFC 1995
		// §2 requires of the journal itself.
		for _, e := range shown.Events {
			sign, colour := "+", output.ColorGreen
			if e.Op == string(gen.EventOpDel) {
				sign, colour = "-", output.ColorRed
			}
			line := fmt.Sprintf("%s%s %d %s %s %s", sign, e.Name, e.TTL, e.Class, e.Type, e.Data)
			if _, werr := fmt.Fprintln(w, p.Paint(colour, line)); werr != nil {
				return werr
			}
		}
		return nil
	})
}
