package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// newClusterCommand groups what a node does about the servers it keeps its
// data in step with.
func newClusterCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Start a cluster, and manage its members",
		Long: "A cluster is a few servers holding the same zones, kept in step through one\n" +
			"log, so that every one of them answers what any change made.\n\n" +
			"A node takes part once its configuration file has a cluster section. One\n" +
			"node starts the cluster with `weg cluster init`, and every other one joins\n" +
			"it from its first start, with `weg serve --join`. A member leaves with\n" +
			"`weg cluster leave`, or is taken out from another with `weg cluster remove`\n" +
			"(docs/decisions/d44-starting-and-joining.md).",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newClusterStatusCommand(opts, &f))
	cmd.AddCommand(newClusterInitCommand(opts, &f))
	cmd.AddCommand(newClusterLeaveCommand(opts, &f))
	cmd.AddCommand(newClusterRemoveCommand(opts, &f))
	return cmd
}

// clusterStarted is what starting a cluster reports.
type clusterStarted struct {
	Member  string `json:"member"`
	Address string `json:"address"`
}

func newClusterInitCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Start a cluster with this server as its first member",
		Long: "Make the server the only member of a new cluster, with everything it holds:\n" +
			"zones, tokens, keys and settings. It is done once, on one server, and\n" +
			"needs the admin scope.\n\n" +
			"The server needs a cluster section in its configuration file, and is\n" +
			"refused if it is a member of a cluster already.",
		Args:    usageArgs(cobra.NoArgs),
		Example: "  weg cluster init\n  weg cluster init --server http://10.0.0.5:8053",

		RunE: func(c *cobra.Command, _ []string) error {
			return runClusterInit(c.Context(), opts, f)
		},
	}
}

func runClusterInit(ctx context.Context, opts *options, f *clientFlags) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	resp, err := client.InitClusterWithResponse(ctx)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	got := clusterStarted{Member: resp.JSON200.Id, Address: resp.JSON200.Address}
	return opts.Printer().Print(got, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w,
			"started a cluster; this server is its first member, %s at %s\n"+
				"another server joins it from its first start: weg serve --join %s\n",
			got.Member, got.Address, got.Address)
		return werr
	})
}

func newClusterStatusCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"show", "members"},
		Short:   "Say who the members are, and how far each has got",
		Long: "List the members as the server's copy of the cluster's configuration has\n" +
			"them, with the role each holds, which one leads, and how far each has got\n" +
			"through the log. The server asks every other member as it answers, and one\n" +
			"that does not answer within a couple of seconds is listed as not reached\n" +
			"(docs/decisions/d47-status-asks-every-member.md).\n\n" +
			"A member that has left the cluster over an entry it could not apply says\n" +
			"where it stopped and why (docs/decisions/d29-a-node-that-cannot-apply.md).",
		Args:    usageArgs(cobra.NoArgs),
		Example: "  weg cluster status\n  weg cluster status --server http://10.0.0.6:8053 --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			return runClusterStatus(c.Context(), opts, f)
		},
	}
}

func runClusterStatus(ctx context.Context, opts *options, f *clientFlags) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	resp, err := client.GetClusterWithResponse(ctx)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	st := resp.JSON200

	p := opts.Printer()
	return p.Print(st, func(w io.Writer) error { return printClusterStatus(w, p, st) })
}

func printClusterStatus(w io.Writer, p *output.Printer, st *gen.ClusterStatus) error {
	switch {
	case st.Removed:
		_, err := fmt.Fprintf(w, "%s %s at %s has been taken out of the cluster.\n%s",
			p.Paint(output.ColorRed, "left:"), st.Self.Id, st.Self.Address, afterLeaving)
		return err
	case st.Behind != nil:
		where := fmt.Sprintf("entry %d", st.Behind.Entry)
		if st.Behind.Entry == 0 {
			where = "a log snapshot"
		}
		_, err := fmt.Fprintf(w, "%s %s at %s has left the cluster: it stopped at %s, %s, because %s.\n"+
			"It answers queries with what it held then and refuses writes. Repair it by removing it\n"+
			"from the cluster, discarding its database and its Raft directory, and joining it again.\n",
			p.Paint(output.ColorRed, "behind:"), st.Self.Id, st.Self.Address, where,
			since(&st.Behind.Since), st.Behind.Reason)
		return err
	case !st.Replicating:
		_, err := fmt.Fprintf(w, "%s at %s is in no cluster yet. `weg cluster init` starts one with it,\n"+
			"or it joins one when started with `weg serve --join`.\n", st.Self.Id, st.Self.Address)
		return err
	}

	state := p.Paint(output.ColorGreen, "current")
	if st.Applied < st.Committed {
		state = p.Paint(output.ColorYellow, fmt.Sprintf("%d entries behind", st.Committed-st.Applied))
	}
	if _, err := fmt.Fprintf(w, "%s at %s: applied %d of %d, %s\n\n",
		st.Self.Id, st.Self.Address, st.Applied, st.Committed, state); err != nil {
		return err
	}

	furthest := st.Committed
	for _, m := range st.Members {
		if m.Progress != nil {
			furthest = max(furthest, m.Progress.Committed)
		}
	}

	t := newTable(w, "MEMBER", "ADDRESS", "ROLE", "LOG", "")
	var trouble []string
	for _, m := range st.Members {
		lead := ""
		if m.Leader {
			lead = p.Paint(output.ColorGreen, "leader")
		}
		name := m.Id
		if m.Id == st.Self.Id {
			name += " (this one)"
		}
		if m.Trouble != nil {
			trouble = append(trouble, fmt.Sprintf("%s: %s\n", m.Id, *m.Trouble))
		}
		t.row(name, m.Address, string(m.Role), progress(p, m.Progress, furthest), lead)
	}
	if err := t.flush(); err != nil {
		return err
	}
	if len(trouble) > 0 {
		_, err := fmt.Fprintf(w, "\n%s", strings.Join(trouble, ""))
		return err
	}
	return nil
}

// progress says how far a member has got, measured against the furthest
// commit any member reported (docs/decisions/d47-status-asks-every-member.md).
func progress(p *output.Printer, g *gen.ClusterProgress, furthest int64) string {
	switch {
	case g == nil:
		return p.Paint(output.ColorYellow, "not reached")
	case g.Behind != nil:
		return p.Paint(output.ColorRed, fmt.Sprintf("stopped at %d", g.Behind.Entry))
	case g.Removed:
		return p.Paint(output.ColorRed, "left")
	case g.Applied < furthest:
		return p.Paint(output.ColorYellow, fmt.Sprintf("%d behind", furthest-g.Applied))
	}
	return p.Paint(output.ColorGreen, "current")
}

// afterLeaving is what becomes of a member taken out of its cluster, and the
// two ways on from there (docs/decisions/d46-a-member-that-has-left.md).
const afterLeaving = "It answers queries with what it held and refuses writes. To join it again,\n" +
	"discard its database and its Raft directory and start it with --join; to run\n" +
	"it on its own, discard its Raft directory only.\n"

// clusterMemberRemoved is what leaving, or removing a member, reports.
type clusterMemberRemoved struct {
	Member string `json:"member"`
}

func newClusterLeaveCommand(opts *options, f *clientFlags) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Take this server out of its cluster",
		Long: "Take the server out of the cluster it is a member of. Any member can be\n" +
			"asked; the one leading carries it out. Needs the admin scope.\n\n" +
			"The server goes on answering queries with what it held, and refuses\n" +
			"writes. Joining it again takes an emptied database\n" +
			"(docs/decisions/d46-a-member-that-has-left.md). A member that is off,\n" +
			"or has stopped over a change it could not apply, cannot ask for itself:\n" +
			"take it out from another with `weg cluster remove`.",
		Args:    usageArgs(cobra.NoArgs),
		Example: "  weg cluster leave --server http://10.0.0.6:8053\n  weg cluster leave --yes",

		RunE: func(c *cobra.Command, _ []string) error {
			return runClusterLeave(c.Context(), opts, f, yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "leave without asking")
	return cmd
}

func runClusterLeave(ctx context.Context, opts *options, f *clientFlags, yes bool) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	resp, err := client.GetClusterWithResponse(ctx)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	self := resp.JSON200.Self
	if !resp.JSON200.Replicating {
		return fmt.Errorf("%s at %s is in no cluster, so there is nothing to leave", self.Id, self.Address)
	}

	if !yes {
		if cerr := confirm(opts, fmt.Sprintf(
			"take %s at %s out of its cluster", self.Id, self.Address)); cerr != nil {
			return cerr
		}
	}
	if rerr := removeMember(ctx, client, f, self.Id); rerr != nil {
		return rerr
	}

	got := clusterMemberRemoved{Member: self.Id}
	return opts.Printer().Print(got, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w, "%s has left the cluster. %s", self.Id, afterLeaving)
		return werr
	})
}

func newClusterRemoveCommand(opts *options, f *clientFlags) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "remove ID",
		Aliases: []string{"rm"},
		Short:   "Take another member out of the cluster",
		Long: "Take a member out of the cluster, naming it the way `weg cluster status`\n" +
			"does. This is for the member that cannot ask for itself: one that is off,\n" +
			"or has stopped over a change it could not apply. Needs the admin scope.\n\n" +
			"The cluster's only voter cannot be removed. Take out a member that is off\n" +
			"before one that is running: removing a running voter while another is off\n" +
			"can leave the rest without a majority\n" +
			"(docs/decisions/d46-a-member-that-has-left.md).",
		Args:    usageArgs(cobra.ExactArgs(1)),
		Example: "  weg cluster remove ns3\n  weg cluster remove ns3 --yes",

		RunE: func(c *cobra.Command, args []string) error {
			return runClusterRemove(c.Context(), opts, f, args[0], yes)
		},
		ValidArgsFunction: completeClusterMembers(f),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "remove without asking")
	return cmd
}

func runClusterRemove(ctx context.Context, opts *options, f *clientFlags, id string, yes bool) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	if !yes {
		if cerr := confirm(opts, fmt.Sprintf("take the member %s out of the cluster", id)); cerr != nil {
			return cerr
		}
	}
	if rerr := removeMember(ctx, client, f, id); rerr != nil {
		return rerr
	}

	got := clusterMemberRemoved{Member: id}
	return opts.Printer().Print(got, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w, "took %s out of the cluster; if it is running, it answers queries "+
			"with what it held and refuses writes\n", id)
		return werr
	})
}

func removeMember(ctx context.Context, client *gen.ClientWithResponses, f *clientFlags, id string) error {
	resp, err := client.RemoveClusterMemberWithResponse(ctx, id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.HTTPResponse.StatusCode != http.StatusNoContent {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return nil
}
