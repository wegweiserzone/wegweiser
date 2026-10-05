package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
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
