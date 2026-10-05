package cli

import (
	"context"
	"fmt"
	"io"

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
			"it from its first start, with `weg serve --join`\n" +
			"(docs/decisions/d44-starting-and-joining.md).",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newClusterStatusCommand(opts, &f))
	cmd.AddCommand(newClusterInitCommand(opts, &f))
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
		Short:   "Say who the members are, and how far this server has got",
		Long: "List the members as the server's copy of the cluster's configuration has\n" +
			"them, with the role each holds and which one leads, and say how far the\n" +
			"server itself has got through the log.\n\n" +
			"How far the others have got is theirs to say: ask each of them with\n" +
			"--server. A member that has left the cluster over an entry it could not\n" +
			"apply says where it stopped and why\n" +
			"(docs/decisions/d29-a-node-that-cannot-apply.md).",
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

	t := newTable(w, "MEMBER", "ADDRESS", "ROLE", "")
	for _, m := range st.Members {
		lead := ""
		if m.Leader {
			lead = p.Paint(output.ColorGreen, "leader")
		}
		name := m.Id
		if m.Id == st.Self.Id {
			name += " (this one)"
		}
		t.row(name, m.Address, string(m.Role), lead)
	}
	return t.flush()
}
