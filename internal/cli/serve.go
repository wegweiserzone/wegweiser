package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
	"github.com/wegweiserzone/wegweiser/internal/config"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/metrics"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/stream"
)

// drainTimeout is how long a shutdown waits for the queries already in flight.
// They are answered from memory and take microseconds, so this is a bound on a
// failure rather than on the ordinary case.
const drainTimeout = 5 * time.Second

// serveFlags are the bootstrap settings weg serve takes on the command line.
//
// They are the strongest of the four sources: a flag beats an environment
// variable, which beats the file, which beats the built-in default
// (docs/decisions/ D11). Which of them a value came from is carried through
// to `weg config show`, so an operator can see why rather than guess.
type serveFlags struct {
	config     string
	listen     string
	apiListen  string
	ui         bool
	database   string
	udpSize    uint16
	tcpClients int
	transfers  int
	logLevel   string
}

// asFlags reads what the command line actually said. A flag nobody typed is
// nil, which is what lets the sources below it be heard.
func (f *serveFlags) asFlags(c *cobra.Command) config.Flags {
	var out config.Flags
	if c.Flags().Changed("listen") {
		out.DNSListen = &f.listen
	}
	if c.Flags().Changed("api-listen") {
		out.APIListen = &f.apiListen
	}
	if c.Flags().Changed("ui") {
		out.APIUI = &f.ui
	}
	if c.Flags().Changed("db") {
		out.Database = &f.database
	}
	if c.Flags().Changed("udp-response-size") {
		out.UDPResponseSize = &f.udpSize
	}
	if c.Flags().Changed("max-tcp-clients") {
		out.MaxTCPClients = &f.tcpClients
	}
	if c.Flags().Changed("max-transfers") {
		out.MaxTransfers = &f.transfers
	}
	if c.Flags().Changed("log-level") {
		out.LogLevel = &f.logLevel
	}
	return out
}

// serveStatus is what the server reports once it is answering. It is a value
// rather than a log line because --output json has to produce something a
// script can read.
type serveStatus struct {
	Address    string `json:"address"`
	APIAddress string `json:"apiAddress"`
	Database   string `json:"database"`
	Zones      int    `json:"zones"`
	Records    int    `json:"records"`
}

// newServeCommand runs the DNS server.
func newServeCommand(opts *options) *cobra.Command {
	var f serveFlags

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Answer DNS queries from a database",
		Long: "Run the authoritative DNS server.\n\n" +
			"The database is the source of truth; what the server answers from is\n" +
			"an in-memory snapshot built from it at startup. Port 53 needs\n" +
			"CAP_NET_BIND_SERVICE, not root.",
		Args:                  usageArgs(cobra.NoArgs),
		DisableFlagsInUseLine: true,
		Example: "  weg serve\n" +
			"  weg serve --listen 127.0.0.1:5353 --db ./wegweiser.db\n" +
			"  weg serve --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := config.Load(f.config, f.asFlags(c))
			if err != nil {
				return err
			}
			return runServe(c.Context(), opts, cfg)
		},
	}

	registerServeFlags(cmd, &f)
	return cmd
}

// registerServeFlags declares the settings a command line may set. The
// defaults shown in --help are the ones config.Load falls back to, so what a
// person reads there is what they get.
func registerServeFlags(cmd *cobra.Command, f *serveFlags) {
	cmd.Flags().StringVar(&f.config, "config", "",
		"path to the configuration file (default "+config.DefaultPath+", or $"+config.PathEnv+")")
	cmd.Flags().StringVarP(&f.listen, "listen", "l", config.Defaults.DNSListen,
		"address to answer queries on, as host:port")
	cmd.Flags().StringVar(&f.apiListen, "api-listen", config.Defaults.APIListen,
		"address to serve the HTTP API and the web interface on")
	cmd.Flags().BoolVar(&f.ui, "ui", config.Defaults.APIUI,
		"serve the web interface; --ui=false serves only the API")
	cmd.Flags().StringVar(&f.database, "db", config.Defaults.Database,
		"path to the SQLite database")
	cmd.Flags().Uint16Var(&f.udpSize, "udp-response-size", config.Defaults.UDPResponseSize,
		"largest UDP response to send, whatever a client advertises")
	cmd.Flags().IntVar(&f.transfers, "max-transfers", config.Defaults.MaxTransfers,
		"how many zone transfers may run at once (0 for the built-in default, negative for no bound)")
	cmd.Flags().IntVar(&f.tcpClients, "max-tcp-clients", config.Defaults.MaxTCPClients,
		"connections to answer at once; 0 takes the default, a negative number removes the bound")
	cmd.Flags().StringVar(&f.logLevel, "log-level", config.Defaults.LogLevel,
		"how loudly to report: "+strings.Join(config.LogLevels, ", "))

	registerFlagCompletion(cmd, "log-level", completeStatic(config.LogLevels...))
}

// runServe is the startup order of architecture §4, in order: open the
// store, build the snapshot, bind the sockets, start the API, and only then say
// anything. Serving before the snapshot is complete would mean answering
// NXDOMAIN for zones this host is supposed to hold, which is worse than not
// answering at all, and reporting ready before the API is up would tell a
// supervisor the control plane exists when it does not.
func runServe(ctx context.Context, opts *options, cfg *config.Config) (err error) {
	st, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Value})
	if err != nil {
		return err
	}
	// Registered first so it runs last: the store outlives everything that
	// reads it, including a shutdown that is still draining.
	defer func() { err = errors.Join(err, st.Close()) }()

	if merr := st.Migrate(ctx); merr != nil {
		return fmt.Errorf("bring the database up to date: %w", merr)
	}

	snap, err := buildSnapshot(ctx, st)
	if err != nil {
		return err
	}

	keys, err := readKeyring(ctx, st)
	if err != nil {
		return err
	}

	p := opts.Printer()
	log := newLogger(p, cfg.LogLevel.Value)
	report := faultReporter(log)

	met := metrics.New()
	tail := stream.NewHub(stream.Options{})

	// Reverse automation is on unless a zone says otherwise; see [apply.Options].
	applier, err := apply.New(st, apply.Options{})
	if err != nil {
		return err
	}

	srv := dns.NewServer(dns.Config{
		Addr:          cfg.DNSListen.Value,
		Limits:        dns.Limits{MaxUDPResponse: cfg.UDPResponseSize.Value},
		MaxTCPClients: cfg.MaxTCPClients.Value,
		MaxTransfers:  cfg.MaxTransfers.Value,
		OnError:       report,
		// An incremental transfer replays the journal; everything else the
		// server answers comes out of the snapshot (invariant 2).
		History: applier,
		Keys:    keys,
		// Both consumers of the one hook. Composing them is the wiring's job:
		// the query path answers queries and does not know what anybody wants
		// to count or watch (architecture §2.9).
		Observe: func(ev dns.Event) {
			met.Observe(ev)
			tail.Observe(ev)
		},
	})

	// Every publish goes through the pair rather than through the server, so
	// that what the metrics report is what queries are actually answered from,
	// including this first one, which no write is responsible for.
	snapshots := &observedSnapshots{server: srv, metrics: met}
	snapshots.SetSnapshot(snap)

	// Who may pull a whole zone, and who is told when one changes, live in the
	// database like the other settings, so they are read once here and
	// republished by the API whenever they change.
	var (
		allow  apply.TransferAllow
		notify []apply.NotifyTarget
	)
	if verr := st.View(ctx, func(r store.Reader) error {
		var serr error
		if allow, serr = apply.StoredTransferAllow(ctx, r); serr != nil {
			return serr
		}
		notify, serr = apply.StoredNotifyTargets(ctx, r)
		return serr
	}); verr != nil {
		return verr
	}
	srv.SetTransfers(dns.Allow{Prefixes: allow.Prefixes, Keys: allow.Keys})

	notifier := dns.NewNotifier(dns.NotifyConfig{
		Targets: notifyTargets(notify), Keys: keys,
		OnError: report, Observe: met.ObserveNotify,
	})
	if nerr := notifier.Start(); nerr != nil {
		return nerr
	}
	defer func() { err = errors.Join(err, stopNotifier(notifier)) }()

	if serr := srv.Start(); serr != nil {
		return serr
	}
	defer func() { err = errors.Join(err, shutdown(srv)) }()

	// The first start mints an administrator token and shows it once. What is
	// stored is its hash, so this is the only moment it exists in readable
	// form (docs/decisions/ D5).
	secret, err := api.EnsureBootstrapToken(ctx, st, applier, time.Now())
	if err != nil {
		return err
	}

	apiSrv, handler, err := api.New(api.Config{
		Store:     st,
		Applier:   applier,
		Snapshots: snapshots,
		Transfers: srv,
		Keyring:   keyPublishers{server: srv, notifier: notifier},
		Notifier:  notifier,
		Metrics:   met,
		Stream:    tail,
		UI:        cfg.APIUI.Value,
		OnError:   report,
	})
	if err != nil {
		return err
	}
	// Registered before the listener, so it runs after it: what the API is
	// holding is written out once nothing more can arrive.
	defer func() { err = errors.Join(err, apiSrv.Close()) }()

	apiListener, lerr := new(net.ListenConfig).Listen(ctx, "tcp", cfg.APIListen.Value)
	if lerr != nil {
		return fmt.Errorf("listen on %s for the API: %w", cfg.APIListen.Value, lerr)
	}
	httpSrv := &http.Server{
		Handler: handler,
		// A header that never finishes arriving is how a connection is held
		// open for free; the rest of the request is bounded by the handler.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if serr := httpSrv.Serve(apiListener); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			report(serr)
		}
	}()
	defer func() {
		drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		defer cancel()
		err = errors.Join(err, httpSrv.Shutdown(drain))
	}()

	status := serveStatus{
		Address:    srv.Addr().String(),
		APIAddress: apiListener.Addr().String(),
		Database:   cfg.Database.Value,
		Zones:      snap.Zones(),
		Records:    snap.Records(),
	}
	if err := p.Print(status, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w,
			"weg is answering on %s — %d zones, %d records from %s\nthe API is on http://%s\n",
			status.Address, status.Zones, status.Records, status.Database, status.APIAddress)
		return werr
	}); err != nil {
		return err
	}
	if secret != "" {
		fmt.Fprintf(p.ErrOut(),
			"weg: this is the first start. The administrator token is shown once:\n\n    %s\n\n"+
				"Store it now; only its hash is kept.\n", secret)
	}

	<-ctx.Done()
	log.Info("stopping, draining the queries in flight")

	// A clean stop is a success. Returning the cancellation would make every
	// SIGTERM look like a failure to whatever supervises the process. The
	// listeners are brought down by the deferred shutdowns above, in the
	// reverse of the order they came up.
	return nil
}

// shutdown stops the DNS server, giving the queries in flight a deadline of
// their own: the context that got us here is already cancelled.
// notifyTargets is the notifier's view of the list the database holds.
func notifyTargets(in []apply.NotifyTarget) []dns.NotifyTarget {
	out := make([]dns.NotifyTarget, len(in))
	for i, t := range in {
		out[i] = dns.NotifyTarget{Addr: t.Addr, Key: t.Key}
	}
	return out
}

// keyPublishers hands a new keyring to everything that holds one: the query
// path verifies signatures with it, and the notifier signs with it.
type keyPublishers struct {
	server   *dns.Server
	notifier *dns.Notifier
}

// SetKeys implements [api.Keyring].
func (k keyPublishers) SetKeys(ring dns.Keyring) {
	k.server.SetKeys(ring)
	k.notifier.SetKeys(ring)
}

// readKeyring reads the TSIG keys the query path verifies and signs with.
//
// Read once here and republished by the API whenever a key is created or
// withdrawn: a signed query has to be verified before it is answered, and a
// database read there would put a disk on the path of every query
// (invariant 2). A withdrawn key is left out, because the store no longer
// holds its secret (docs/decisions/d28-tsig.md).
func readKeyring(ctx context.Context, st store.Store) (dns.Keyring, error) {
	var ring dns.Keyring
	err := st.View(ctx, func(r store.Reader) error {
		keys, lerr := r.ListTSIGKeys(ctx)
		if lerr != nil {
			return lerr
		}
		ring = make(dns.Keyring, len(keys))
		for _, k := range keys {
			if !k.Active() {
				continue
			}
			ring[k.Name] = dns.TSIGKey{Name: k.Name, Algorithm: k.Algorithm, Secret: k.Secret}
		}
		return nil
	})
	return ring, err
}

// stopNotifier gives the notifier the same grace a shutdown gives the server.
func stopNotifier(n *dns.Notifier) error {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return n.Close(ctx)
}

func shutdown(srv *dns.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

// buildSnapshot reads the whole database into the form the query path answers
// from. It runs in one read transaction, so the snapshot is a picture of one
// moment rather than of several.
func buildSnapshot(ctx context.Context, st store.Store) (*dns.Snapshot, error) {
	var snap *dns.Snapshot
	err := st.View(ctx, func(r store.Reader) error {
		var berr error
		snap, berr = dns.Rebuild(ctx, r)
		return berr
	})
	if err != nil {
		return nil, fmt.Errorf("build the snapshot to answer from: %w", err)
	}
	return snap, nil
}

// newLogger returns the logger the running server reports through.
func newLogger(p *output.Printer, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: logLevel(level)}
	if p.Format() == output.FormatText {
		return slog.New(slog.NewTextHandler(p.ErrOut(), opts))
	}
	return slog.New(slog.NewJSONHandler(p.ErrOut(), opts))
}

// logLevel turns the configured name into a level. The name has already been
// checked against the list, so anything else here is a programming error and
// info is the safe thing to be wrong with.
func logLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// faultReporter adapts the OnError hook that the server and the API take to
// the logger.
//
// The hook is called from every reader goroutine there is. A handler is safe
// for concurrent use and serialises its own writes, which is what this used to
// need a mutex for: two goroutines writing to one file without one is a data
// race, not merely untidy output.
func faultReporter(log *slog.Logger) func(error) {
	return func(err error) {
		if err != nil {
			log.Error(err.Error())
		}
	}
}

// observedSnapshots publishes a snapshot to the data plane and tells the
// metrics what is now being answered from.
type observedSnapshots struct {
	server  *dns.Server
	metrics *metrics.Metrics
}

func (o *observedSnapshots) Snapshot() *dns.Snapshot { return o.server.Snapshot() }

func (o *observedSnapshots) SetSnapshot(snap *dns.Snapshot) {
	o.server.SetSnapshot(snap)
	o.metrics.SetSnapshot(snap, time.Now())
}
