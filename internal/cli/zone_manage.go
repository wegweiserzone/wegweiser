package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// pageSize is how many zones or records are asked for at a time.
const pageSize = 500

// newZoneListCommand lists the zones this server holds.
func newZoneListCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		kind     string
		search   string
		disabled bool
		limit    int
	)

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the zones this server holds",
		Long: "List the zones this server holds, in the order it stores them.\n\n" +
			"The whole listing is printed: paging is how the wire carries it, not\n" +
			"something a reader should have to work through.",
		Args: usageArgs(cobra.NoArgs),
		Example: "  weg zone list\n" +
			"  weg zone list --kind reverse\n" +
			"  weg zone list --search example --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			var params gen.ListZonesParams
			if kind != "" {
				k := gen.ZoneKind(strings.ToLower(kind))
				if !k.Valid() {
					return usageError{fmt.Errorf(
						"%q is not a kind of zone; it is forward or reverse", kind)}
				}
				params.Kind = &k
			}
			if search != "" {
				params.Search = &search
			}
			if c.Flags().Changed("disabled") {
				params.Disabled = &disabled
			}
			return runZoneList(c.Context(), opts, f, params, limit)
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", "only forward or only reverse zones")
	cmd.Flags().StringVar(&search, "search", "", "match anywhere in the zone name")
	cmd.Flags().BoolVar(&disabled, "disabled", false,
		"only disabled zones, or with =false only enabled ones")
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after this many zones (0 is all of them)")
	registerFlagCompletion(cmd, "kind", completeStatic("forward", "reverse"))
	return cmd
}

// zoneListed is one line of a listing.
type zoneListed struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Serial      int64   `json:"serial"`
	DefaultTTL  int64   `json:"defaultTtl"`
	Disabled    bool    `json:"disabled"`
	Prefix      *string `json:"prefix,omitempty"`
	AutoReverse *bool   `json:"autoReverse,omitempty"`
	Comment     string  `json:"comment,omitempty"`
}

func runZoneList(
	ctx context.Context, opts *options, f *clientFlags, params gen.ListZonesParams, limit int,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	zones, err := allZones(ctx, client, f, params, limit)
	if err != nil {
		return err
	}

	listed := make([]zoneListed, 0, len(zones))
	for i := range zones {
		listed = append(listed, listZone(&zones[i]))
	}

	p := opts.Printer()
	return p.Print(listed, func(w io.Writer) error {
		if len(listed) == 0 {
			// An empty listing is a state a person can act on, not a blank
			// screen they have to guess at.
			_, werr := fmt.Fprintln(w, "no zones. `weg zone create NAME` makes one, "+
				"`weg zone import FILE` brings one in")
			return werr
		}

		t := newTable(w, "NAME", "KIND", "SERIAL", "TTL", "STATUS")
		for _, z := range listed {
			status, colour := "enabled", output.ColorGreen
			if z.Disabled {
				status, colour = "disabled", output.ColorYellow
			}
			t.row(z.Name, z.Kind, fmt.Sprint(z.Serial), fmt.Sprint(z.DefaultTTL),
				p.Paint(colour, status))
		}
		return t.flush()
	})
}

// allZones follows the cursor to the end, or until limit rows are in hand.
func allZones(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags,
	params gen.ListZonesParams, limit int,
) ([]gen.Zone, error) {
	var out []gen.Zone
	size := pageSize
	if limit > 0 && limit < size {
		size = limit
	}
	params.Limit = ptr(size)

	for {
		resp, err := client.ListZonesWithResponse(ctx, &params)
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

func listZone(z *gen.Zone) zoneListed {
	out := zoneListed{
		Name:        z.Name,
		Kind:        string(z.Kind),
		DefaultTTL:  z.DefaultTtl,
		Disabled:    z.Disabled,
		Prefix:      z.Prefix,
		AutoReverse: z.AutoReverse,
		Comment:     opt(z.Comment, ""),
	}
	if z.Soa.Serial != nil {
		out.Serial = *z.Soa.Serial
	}
	return out
}

// newZoneShowCommand prints one zone in full.
func newZoneShowCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "show ZONE",
		Aliases: []string{"get"},
		Short:   "Print one zone's settings",
		Long: "Print everything this server knows about a zone except its records.\n\n" +
			"`weg zone export` writes the records; this is the zone itself.",
		Args:    usageArgs(cobra.ExactArgs(1)),
		Example: "  weg zone show example.com\n  weg zone show example.com --output yaml",

		RunE: func(c *cobra.Command, args []string) error {
			return runZoneShow(c.Context(), opts, f, args[0])
		},
		ValidArgsFunction: completeZones(f),
	}
}

func runZoneShow(ctx context.Context, opts *options, f *clientFlags, name string) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, name)
	if err != nil {
		return err
	}
	// A name server this zone points at and has no address for is the one
	// thing about a zone that looks fine and is not (RFC 1912 §2.8).
	lame, err := lameNameServers(ctx, client, f, z)
	if err != nil {
		return err
	}

	// Embedded so that --output json carries the zone's own fields at the top
	// level, with the warning beside them rather than instead of them.
	shown := struct {
		*gen.Zone
		LameNameServers []lameNS `json:"lameNameServers,omitempty"`
	}{Zone: z, LameNameServers: lame}

	p := opts.Printer()
	return p.Print(shown, func(w io.Writer) error {
		rows := [][2]string{
			{"name", z.Name},
			{"kind", string(z.Kind)},
		}
		if z.Prefix != nil {
			rows = append(rows, [2]string{"network", *z.Prefix})
		}
		rows = append(rows,
			[2]string{"serial", fmt.Sprint(opt(z.Soa.Serial, 0))},
			[2]string{"default ttl", fmt.Sprint(z.DefaultTtl)},
			[2]string{"primary ns", z.Soa.PrimaryNs},
			[2]string{"mailbox", z.Soa.Mailbox},
			[2]string{"refresh", fmt.Sprint(z.Soa.Refresh)},
			[2]string{"retry", fmt.Sprint(z.Soa.Retry)},
			[2]string{"expire", fmt.Sprint(z.Soa.Expire)},
			[2]string{"minimum", fmt.Sprint(z.Soa.Minimum)},
			[2]string{"soa ttl", fmt.Sprint(z.Soa.Ttl)},
			[2]string{"reverse automation", autoReverseText(z.AutoReverse)},
			[2]string{"status", statusText(z.Disabled)},
			[2]string{"created", z.CreatedAt.Local().Format("2006-01-02 15:04:05")},
			[2]string{"updated", z.UpdatedAt.Local().Format("2006-01-02 15:04:05")},
		)
		if c := opt(z.Comment, ""); c != "" {
			rows = append(rows, [2]string{"comment", c})
		}

		t := newRows(w, 2)
		for _, r := range rows {
			t.row(p.Paint(output.ColorDim, r[0]), r[1])
		}
		if err := t.flush(); err != nil {
			return err
		}

		for _, l := range lame {
			if _, werr := fmt.Fprintf(w, "\n%s %s\n",
				p.Paint(output.ColorYellow, "warning:"), lameNote(l)); werr != nil {
				return werr
			}
		}
		return nil
	})
}

// autoReverseText says which of the three states the zone is in. Absent is not
// the same as off: it follows the server, and a person reading this has to be
// able to tell those apart.
func autoReverseText(v *bool) string {
	switch {
	case v == nil:
		return "follows the server"
	case *v:
		return "on"
	default:
		return "off"
	}
}

func statusText(disabled bool) string {
	if disabled {
		return "disabled"
	}
	return "enabled"
}

// opt reads an optional field, falling back to a default.
func opt[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

// ptr is the pointer an optional field in the generated models wants.
func ptr[T any](v T) *T { return &v }

// zoneCreated is what creating a zone reports.
type zoneCreated struct {
	Zone      string `json:"zone"`
	Kind      string `json:"kind"`
	Serial    int64  `json:"serial"`
	PrimaryNs string `json:"primaryNs"`
	Mailbox   string `json:"mailbox"`
	// NameServer holds the address records written for the primary, if any
	// were asked for.
	NameServer []string `json:"nameServer,omitempty"`
}

func newZoneCreateCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		in    gen.CreateZone
		soa   gen.SOAInput
		ns    string
		email string
		ttl   int64
		soaIn struct{ refresh, retry, expire, minimum, ttl int64 }
		note  string
		addrs []string
	)

	cmd := &cobra.Command{
		Use:     "create NAME",
		Aliases: []string{"add", "new"},
		Short:   "Create a zone",
		Long: "Create an empty zone: a start of authority and its own name server\n" +
			"record, and nothing else.\n\n" +
			"Everything not given comes from this server's defaults, so the short\n" +
			"form is enough for most zones.\n\n" +
			"NAME may be a network instead, and becomes the reverse zone that\n" +
			"answers for it: 192.168.0.0/16 is 168.192.in-addr.arpa., 192.0.2.0/25\n" +
			"is the classless 0/25.2.0.192.in-addr.arpa. of RFC 2317, and\n" +
			"2001:db8::/32 is eight nibbles written backwards. A reverse zone given\n" +
			"by name is recognised as one too, and needs no flag either way.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg zone create example.com\n" +
			"  weg zone create 192.168.0.0/16\n" +
			"  weg zone create example.com --ns ns1.example.com. --email hostmaster@example.com\n" +

			"  weg zone create example.com --ns-address 192.0.2.10 --ns-address 2001:db8::10\n" +
			"  weg zone create example.com --ttl 300 --comment \"the main zone\"",

		RunE: func(c *cobra.Command, args []string) error {
			in.Name = args[0]

			if ns != "" {
				soa.PrimaryNs = &ns
			}
			if email != "" {
				mbox, err := mailbox(email)
				if err != nil {
					return err
				}
				soa.Mailbox = &mbox
			}
			for _, opt := range []struct {
				flag string
				from *int64
				to   **int64
			}{
				{"refresh", &soaIn.refresh, &soa.Refresh},
				{"retry", &soaIn.retry, &soa.Retry},
				{"expire", &soaIn.expire, &soa.Expire},
				{"minimum", &soaIn.minimum, &soa.Minimum},
				{"soa-ttl", &soaIn.ttl, &soa.Ttl},
			} {
				if c.Flags().Changed(opt.flag) {
					*opt.to = opt.from
				}
			}
			if soa != (gen.SOAInput{}) {
				in.Soa = &soa
			}
			if c.Flags().Changed("ttl") {
				in.DefaultTtl = &ttl
			}
			if note != "" {
				in.Comment = &note
			}
			return runZoneCreate(c.Context(), opts, f, in, addrs)
		},
	}

	cmd.Flags().StringVar(&ns, "ns", "",
		"the primary name server (default ns1 under the zone itself)")
	cmd.Flags().StringArrayVar(&addrs, "ns-address", nil,
		"an address for the primary name server; repeat for IPv4 and IPv6")
	cmd.Flags().StringVar(&email, "email", "",
		"the administrator, as an address or as a DNS name (default hostmaster under the zone)")
	cmd.Flags().Int64Var(&ttl, "ttl", 0, "the TTL a record added without one gets")
	cmd.Flags().StringVar(&note, "comment", "", "a note about what this zone is for")
	cmd.Flags().Int64Var(&soaIn.refresh, "refresh", 0, "SOA refresh interval, in seconds")
	cmd.Flags().Int64Var(&soaIn.retry, "retry", 0, "SOA retry interval, in seconds")
	cmd.Flags().Int64Var(&soaIn.expire, "expire", 0, "SOA expiry, in seconds")
	cmd.Flags().Int64Var(&soaIn.minimum, "minimum", 0,
		"SOA minimum: the negative-caching TTL of RFC 2308 §4, not a floor on record TTLs")
	cmd.Flags().Int64Var(&soaIn.ttl, "soa-ttl", 0, "the TTL of the SOA record itself")
	return cmd
}

func runZoneCreate(
	ctx context.Context, opts *options, f *clientFlags, in gen.CreateZone, addrs []string,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	resp, err := client.CreateZoneWithResponse(ctx, in)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON201 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	z := resp.JSON201

	created := zoneCreated{
		Zone:      z.Name,
		Kind:      string(z.Kind),
		Serial:    opt(z.Soa.Serial, 0),
		PrimaryNs: z.Soa.PrimaryNs,
		Mailbox:   z.Soa.Mailbox,
	}

	// The address records are a second commit rather than part of the first.
	// Creating a zone and putting a record in it are two changes, and the
	// journal saying so is what makes either of them revertible on its own.
	for _, addr := range addrs {
		typ := "A"
		if strings.Contains(addr, ":") {
			typ = "AAAA"
		}
		rec, rerr := client.CreateRecordWithResponse(ctx, z.Id, gen.CreateRecord{
			Name: z.Soa.PrimaryNs,
			Type: typ,
			Data: addr,
		})
		if rerr != nil {
			return reachable(rerr, f.server)
		}
		if rec.JSON201 == nil {
			return apiError(rec.HTTPResponse.StatusCode, rec.Body)
		}
		created.NameServer = append(created.NameServer,
			fmt.Sprintf("%s %s %s", rec.JSON201.Record.Name, rec.JSON201.Record.Type,
				rec.JSON201.Record.Data))
	}

	return opts.Printer().Print(created, func(w io.Writer) error {
		if _, werr := fmt.Fprintf(w, "created %s (%s), serial %d, %s\n",
			created.Zone, created.Kind, created.Serial, created.PrimaryNs); werr != nil {
			return werr
		}
		for _, r := range created.NameServer {
			if _, werr := fmt.Fprintf(w, "  %s\n", r); werr != nil {
				return werr
			}
		}
		if len(created.NameServer) == 0 {
			// Said once, here, because this is the moment it can be fixed in
			// one command rather than diagnosed later from a resolver's logs.
			if _, werr := fmt.Fprintf(w,
				"%s has no address yet, so a resolver referred to it is told the name does\n"+
					"not exist. `weg record add %s %s A <address>` fixes it.\n",
				created.PrimaryNs, created.Zone, created.PrimaryNs); werr != nil {
				return werr
			}
		}
		_, werr := fmt.Fprintf(w, "it is answering now; `weg record add` puts something in it\n")
		return werr
	})
}

// mailbox turns what a person types into the RNAME of RFC 1035 §3.3.13.
func mailbox(s string) (string, error) {
	local, domain, found := strings.Cut(s, "@")
	if !found {
		// Already a DNS name, or something we have no business rewriting.
		return s, nil
	}
	if local == "" || domain == "" {
		return "", usageError{fmt.Errorf("%q is not an address: it needs something on both sides of the @", s)}
	}
	return strings.ReplaceAll(local, ".", `\.`) + "." + domain, nil
}

func newZoneDeleteCommand(opts *options, f *clientFlags) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "delete ZONE",
		Aliases: []string{"rm", "remove"},
		Short:   "Delete a zone and everything in it",
		Long: "Delete a zone, its records, and any generated reverse entries that\n" +
			"came from them.\n\n" +
			"The journal keeps what the zone was: the commits outlive it, so the\n" +
			"deletion is in the audit log and the state before it is still\n" +
			"readable. What stops immediately is the answering.",
		Args:    usageArgs(cobra.ExactArgs(1)),
		Example: "  weg zone delete example.com\n  weg zone delete example.com --yes",

		RunE: func(c *cobra.Command, args []string) error {
			return runZoneDelete(c.Context(), opts, f, args[0], yes)
		},
		ValidArgsFunction: completeZones(f),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "delete without asking")
	return cmd
}

// zoneDeleted is what a deletion reports.
type zoneDeleted struct {
	Zone string `json:"zone"`
}

func runZoneDelete(
	ctx context.Context, opts *options, f *clientFlags, name string, yes bool,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, name)
	if err != nil {
		return err
	}

	if !yes {
		if cerr := confirm(opts, fmt.Sprintf(
			"delete %s and every record in it", z.Name)); cerr != nil {
			return cerr
		}
	}

	resp, err := client.DeleteZoneWithResponse(ctx, z.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.HTTPResponse.StatusCode != http.StatusNoContent {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	return opts.Printer().Print(zoneDeleted{Zone: z.Name}, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w, "deleted %s; the journal still has what it was\n", z.Name)
		return werr
	})
}

// errCancelled is what answering no leaves behind. It is an error, not a
// success: what the command was asked to do did not happen, and a script
// checking the exit status has to see that.
var errCancelled = errors.New("cancelled")

// confirm asks before something that cannot be undone by running the command
// again. The action reads as the end of a sentence, without a question mark:
// it is put into one here, and into an error where there is nobody to ask.
func confirm(opts *options, action string) error {
	in := opts.stdin
	if in == nil {
		if !output.IsTerminal(os.Stdin) {
			return usageError{fmt.Errorf(
				"no terminal to ask on: pass --yes to %s", action)}
		}
		in = os.Stdin
	}

	// If the question cannot be written, the read below will not get an
	// answer either, and that is where it is reported.
	fmt.Fprintf(opts.stderr, "%s? [y/N] ", action)

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// Nothing came back. Whatever the reason, nobody said yes.
		return errCancelled
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return errCancelled
	}
}

// zoneRolledBack is what a rollback reports.
type zoneRolledBack struct {
	Zone         string           `json:"zone"`
	RestoredTo   int64            `json:"restoredTo"`
	Serial       int64            `json:"serial"`
	Commit       string           `json:"commit,omitempty"`
	Changed      int              `json:"changed"`
	Conflicts    []recordConflict `json:"conflicts,omitempty"`
	MissingZones []missingZoneRef `json:"missingZones,omitempty"`
}

func newZoneRollbackCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		note string
		yes  bool
	)

	cmd := &cobra.Command{
		Use:     "rollback ZONE SERIAL",
		Aliases: []string{"revert", "restore"},
		Short:   "Restore a zone to the state it had at a serial",
		Long: "Put a zone's records back the way they were at a serial it once had.\n\n" +
			"It moves forwards, not backwards: the difference between now and then\n" +
			"is written as a new commit with the next serial. A secondary that has\n" +
			"already seen serial 90 would never accept a jump back to 42: RFC 1982\n" +
			"arithmetic makes 42 the older number, and RFC 1995 has no way to say\n" +
			"\"go back\".\n\n" +
			"Records that did not change in between are left alone, so they keep\n" +
			"their comments and their history. `weg history list ZONE` is where the\n" +
			"serials come from.",
		Args: usageArgs(cobra.ExactArgs(2)),
		Example: "  weg history list example.com\n" +
			"  weg zone rollback example.com 41\n" +
			"  weg zone rollback example.com 41 --comment \"the migration broke mail\" --yes",

		RunE: func(c *cobra.Command, args []string) error {
			serial, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				// Not the strconv message: what a person needs here is not
				// "invalid syntax" but where the numbers come from.
				return usageError{fmt.Errorf(
					"%q is not a serial; `weg history list %s` shows the ones this zone has had",
					args[1], args[0])}
			}
			return runZoneRollback(c.Context(), opts, f, args[0], serial, note, yes)
		},
		ValidArgsFunction: completeZones(f),
	}

	cmd.Flags().StringVar(&note, "comment", "", "why, for the history")
	cmd.Flags().BoolVar(&yes, "yes", false, "restore without asking")
	return cmd
}

func runZoneRollback(
	ctx context.Context, opts *options, f *clientFlags,
	zoneName string, serial int64, note string, yes bool,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, zoneName)
	if err != nil {
		return err
	}

	if !yes {
		if cerr := confirm(opts, fmt.Sprintf(
			"put %s back to the state it had at serial %d", z.Name, serial)); cerr != nil {
			return cerr
		}
	}

	in := gen.RollbackZone{Serial: serial}
	if note != "" {
		in.Comment = &note
	}
	resp, err := client.RollbackZoneWithResponse(ctx, z.Id, in)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	out := resp.JSON200

	done := zoneRolledBack{Zone: z.Name, RestoredTo: serial, Serial: opt(z.Soa.Serial, 0)}
	if out.Commit != nil {
		done.Commit = out.Commit.Id
		done.Serial = out.Commit.SerialTo
		done.Changed = len(deref(out.Commit.Events, nil))
	}
	for _, c := range deref(out.Conflicts, nil) {
		done.Conflicts = append(done.Conflicts, recordConflict{
			Address: c.Address, ExistingName: c.ExistingName, RequestedName: c.RequestedName,
			Policy: string(c.Policy),
		})
	}
	for _, m := range deref(out.MissingZones, nil) {
		done.MissingZones = append(done.MissingZones, missingZoneRef{
			Address: m.Address, ZoneName: m.ZoneName,
		})
	}

	return opts.Printer().Print(done, func(w io.Writer) error {
		if done.Commit == "" {
			// The zone was already there. Saying "restored" would claim a
			// commit that does not exist.
			_, werr := fmt.Fprintf(w, "%s is already at serial %d; nothing was written\n",
				done.Zone, done.RestoredTo)
			return werr
		}
		if _, werr := fmt.Fprintf(w,
			"%s is back at the state it had at serial %d — %d record changes, now serial %d\n",
			done.Zone, done.RestoredTo, done.Changed, done.Serial); werr != nil {
			return werr
		}
		for _, m := range done.MissingZones {
			if _, werr := fmt.Fprintf(w,
				"  no reverse zone covers %s; create %s to have PTRs generated for it\n",
				m.Address, m.ZoneName); werr != nil {
				return werr
			}
		}
		for _, c := range done.Conflicts {
			if _, werr := fmt.Fprintf(w, "  %s\n", c); werr != nil {
				return werr
			}
		}
		return nil
	})
}
