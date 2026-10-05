package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/cluster"
	"github.com/wegweiserzone/wegweiser/internal/config"
	"github.com/wegweiserzone/wegweiser/internal/store"
)

// joinFlags say which member to ask, and in what role, when this node is to
// join a cluster (docs/decisions/d44-starting-and-joining.md). They are not
// settings: they name an act done once, which is why the file has no place
// for them.
type joinFlags struct {
	addr string
	role string
}

// check refuses a combination that cannot mean anything.
func (j joinFlags) check(cfg *config.Config) error {
	switch {
	case j.addr == "" && j.role != string(cluster.RoleVoter):
		return errors.New("--role says what to join as, and needs --join")
	case j.addr == "":
		return nil
	case cfg.Cluster == nil:
		return errors.New("--join needs a cluster section in the configuration file: " +
			"the address to advertise and the cluster's secret")
	case j.role != string(cluster.RoleVoter) && j.role != string(cluster.RoleNonvoter):
		return fmt.Errorf("--role %q is not a role; a node joins as %s or %s",
			j.role, cluster.RoleVoter, cluster.RoleNonvoter)
	}
	return nil
}

// clusterPort is this node's cluster port, opened before the applier is built
// because the applier's writes go through the member that serves on it.
type clusterPort struct {
	id   string
	tr   *cluster.Transport
	mux  *cluster.Mux
	repl *cluster.Replication
}

// openClusterPort listens on the cluster port. Nothing is a member yet: that
// waits for the applier and the query path, which the member applies the log
// through.
func openClusterPort(ctx context.Context, cfg *config.Cluster, st store.Store, log *slog.Logger) (*clusterPort, error) {
	id, err := cluster.Identity(ctx, st, cfg.ID.Value)
	if err != nil {
		return nil, err
	}
	tr, err := cluster.New(cluster.Config{
		Secret: cfg.Secret,
		// A port a network can reach is probed by strangers, so a refusal is
		// worth seeing and is not a fault of this server.
		OnRefused: func(remote net.Addr, err error) {
			log.Warn("turned a connection to the cluster port away", "remote", remote.String(), "reason", err.Error())
		},
	})
	if err != nil {
		return nil, err
	}
	l, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.Listen.Value)
	if err != nil {
		return nil, fmt.Errorf("listen on %s for the cluster: %w", cfg.Listen.Value, err)
	}
	return &clusterPort{id: id, tr: tr, mux: tr.Serve(l), repl: &cluster.Replication{}}, nil
}

// startMember brings this node's member up, and has it ask to join when the
// command line says to.
func startMember(
	ctx context.Context, cfg *config.Cluster, port *clusterPort, join joinFlags,
	st store.Store, applier *apply.Applier, loader cluster.Loader, log *slog.Logger, report func(error),
) (*cluster.Node, error) {
	node, err := cluster.Start(cluster.NodeConfig{
		ID: port.id, Advertise: cfg.Advertise.Value, Dir: cfg.Dir,
		Transport: port.tr, Mux: port.mux,
		Store: st, Applier: applier, Loader: loader,
		Logger: log, OnError: report,
	})
	if err != nil {
		return nil, err
	}
	port.repl.Bind(node)

	switch {
	case join.addr == "":
	case node.Replicating():
		// A unit file that keeps the flag is harmless, and restarting a
		// repaired member with the same line is exactly its rejoin (D44).
		log.Info("this node is a cluster member already; --join is ignored", "member", port.id)
	default:
		if herr := refuseHeldContent(ctx, st); herr != nil {
			return nil, errors.Join(herr, node.Close())
		}
		log.Info("asking to join the cluster", "member", port.id, "via", join.addr, "role", join.role)
		if jerr := node.Join(ctx, join.addr, cluster.Role(join.role)); jerr != nil {
			return nil, errors.Join(jerr, node.Close())
		}
	}
	return node, nil
}

// refuseHeldContent stops a node that would join with data of its own. What
// it holds is in no entry of the cluster's log, so it would either be thrown
// away by the first log snapshot or, worse, outlive it on this node alone
// (docs/decisions/d32-what-else-the-cluster-replicates.md).
func refuseHeldContent(ctx context.Context, st store.Store) error {
	held := map[store.ReplicatedKind]int{}
	err := st.View(ctx, func(r store.Reader) error {
		for item, ierr := range r.ExportReplicated(ctx) {
			if ierr != nil {
				return ierr
			}
			held[item.Kind]++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("look at what this node holds before it joins: %w", err)
	}
	if len(held) == 0 {
		return nil
	}
	found := make([]string, 0, len(held))
	for kind, n := range held {
		found = append(found, fmt.Sprintf("%d %s", n, kind))
	}
	sort.Strings(found)
	return fmt.Errorf("a node joins a cluster with an empty database, and this one holds %s; "+
		"start it on a new one", strings.Join(found, ", "))
}

// printMemberStatus adds a line about the cluster to what serve reports at
// start, when this node has a cluster section.
func printMemberStatus(w io.Writer, m *memberStatus) error {
	if m == nil {
		return nil
	}
	state := "not in a cluster yet"
	if m.Replicating {
		state = "a cluster member"
	}
	_, err := fmt.Fprintf(w, "this node is %s, as %s on %s; Raft keeps its log in %s\n",
		state, m.Member, m.Advertise, m.RaftDir)
	return err
}
