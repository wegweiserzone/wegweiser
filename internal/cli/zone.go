package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
)

// newZoneCommand groups everything that acts on a whole zone.
func newZoneCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "zone",
		Aliases: []string{"z"},
		Short:   "Work with whole zones",
		Args:    usageArgs(cobra.NoArgs),
		RunE:    func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newZoneListCommand(opts, &f))
	cmd.AddCommand(newZoneShowCommand(opts, &f))
	cmd.AddCommand(newZoneCreateCommand(opts, &f))
	cmd.AddCommand(newZoneUpdateCommand(opts, &f))
	cmd.AddCommand(newZoneEnableCommand(opts, &f))
	cmd.AddCommand(newZoneDisableCommand(opts, &f))
	cmd.AddCommand(newZoneDeleteCommand(opts, &f))
	cmd.AddCommand(newZoneRollbackCommand(opts, &f))
	cmd.AddCommand(newZoneImportCommand(opts, &f))
	cmd.AddCommand(newZoneExportCommand(opts, &f))
	cmd.AddCommand(newZoneCheckCommand(opts, &f))
	return cmd
}

// zoneImported is what an import reports.
type zoneImported struct {
	Zone         string           `json:"zone"`
	Records      int              `json:"records"`
	Serial       int64            `json:"serial"`
	Skipped      []skippedRecord  `json:"skipped,omitempty"`
	MissingZones []missingZoneRef `json:"missingZones,omitempty"`
}

type skippedRecord struct {
	Record string `json:"record"`
	Reason string `json:"reason"`
}

type missingZoneRef struct {
	Address  string `json:"address"`
	ZoneName string `json:"zoneName"`
}

func newZoneImportCommand(opts *options, f *clientFlags) *cobra.Command {
	var origin string

	cmd := &cobra.Command{
		Use:   "import [FILE]",
		Short: "Bring a zone in from a zonefile",
		Long: "Read a zonefile and create the zone it describes.\n\n" +
			"The zone is whichever name the file's SOA sits at, and the serial in\n" +
			"the file is the serial the zone starts at, so secondaries that have\n" +
			"already seen it keep working. With no FILE, or with -, the file is\n" +
			"read from standard input.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		Example: "  weg zone import db.example.com\n" +
			"  named-checkzone -D example.com db.example.com | weg zone import\n" +
			"  weg zone import --origin example.com. db.example.com --output json",

		RunE: func(c *cobra.Command, args []string) error {
			return runZoneImport(c.Context(), opts, f, args, origin)
		},
	}
	cmd.Flags().StringVar(&origin, "origin", "",
		"where relative names resolve from, for a file with no $ORIGIN")
	return cmd
}

func runZoneImport(
	ctx context.Context, opts *options, f *clientFlags, args []string, origin string,
) (err error) {
	client, err := f.client()
	if err != nil {
		return err
	}

	body, closeBody, err := openInput(args, opts)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeBody()) }()

	var params gen.ImportZoneParams
	if origin != "" {
		params.Origin = &origin
	}

	resp, err := client.ImportZoneWithBodyWithResponse(ctx, &params, "text/dns", body)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON201 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	out := resp.JSON201

	report := zoneImported{
		Zone:    out.Zone.Name,
		Records: out.Records,
	}
	if out.Zone.Soa.Serial != nil {
		report.Serial = *out.Zone.Soa.Serial
	}
	for _, s := range deref(out.Skipped, nil) {
		report.Skipped = append(report.Skipped, skippedRecord{Record: s.Record, Reason: s.Reason})
	}
	for _, m := range deref(out.MissingZones, nil) {
		report.MissingZones = append(report.MissingZones, missingZoneRef{
			Address: m.Address, ZoneName: m.ZoneName,
		})
	}

	return opts.Printer().Print(report, func(w io.Writer) error {
		if _, werr := fmt.Fprintf(w, "imported %s: %d records, serial %d\n",
			report.Zone, report.Records, report.Serial); werr != nil {
			return werr
		}
		// The two things a person has to decide about, spelled out rather than
		// left in a JSON field nobody looked at.
		for _, s := range report.Skipped {
			if _, werr := fmt.Fprintf(w, "  skipped %s\n    %s\n", s.Record, s.Reason); werr != nil {
				return werr
			}
		}
		for _, m := range report.MissingZones {
			if _, werr := fmt.Fprintf(w,
				"  no reverse zone covers %s; create %s to have PTRs generated for it\n",
				m.Address, m.ZoneName); werr != nil {
				return werr
			}
		}
		return nil
	})
}

// zoneExported is what an export reports when it is asked for something other
// than the file itself.
type zoneExported struct {
	Zone    string `json:"zone"`
	Content string `json:"content"`
}

func newZoneExportCommand(opts *options, f *clientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export ZONE",
		Short: "Write a zone out as a zonefile",
		Long: "Write a zone in the presentation format of RFC 1035 §5, which any\n" +
			"other authoritative server reads.\n\n" +
			"With --output text the file goes to standard output unchanged, so it\n" +
			"can be redirected into a file or piped into another tool. The other\n" +
			"formats wrap it, for a caller that wants the zone name with it.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg zone export example.com > db.example.com\n" +
			"  weg zone export example.com | named-checkzone example.com /dev/stdin",

		RunE: func(c *cobra.Command, args []string) error {
			return runZoneExport(c.Context(), opts, f, args[0])
		},
		ValidArgsFunction: completeZones(f),
	}
	return cmd
}

func runZoneExport(ctx context.Context, opts *options, f *clientFlags, name string) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	z, err := findZone(ctx, client, f, name)
	if err != nil {
		return err
	}

	resp, err := client.ExportZoneWithResponse(ctx, z.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.HTTPResponse.StatusCode != http.StatusOK {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	return opts.Printer().Print(
		zoneExported{Zone: z.Name, Content: string(resp.Body)},
		func(w io.Writer) error {
			// The file itself, unchanged: this is the one command whose output
			// is meant to be read by another server rather than by a person.
			_, werr := w.Write(resp.Body)
			return werr
		})
}

// findZone turns a name a person typed into the zone it belongs to.
func findZone(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags, name string,
) (*gen.Zone, error) {
	// A trailing dot is optional here for the same reason it is optional
	// everywhere else a person types a zone name.
	qualified := name
	if !strings.HasSuffix(qualified, ".") {
		qualified += "."
	}

	resp, err := client.ListZonesWithResponse(ctx, &gen.ListZonesParams{Name: &qualified})
	if err != nil {
		return nil, reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return nil, apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	if len(resp.JSON200.Items) == 0 {
		return nil, fmt.Errorf("no zone named %q on this server", qualified)
	}
	return &resp.JSON200.Items[0], nil
}

// openInput returns the file to read, and a function that closes it.
//
// Standard input is not closed: it is not ours to close, and a caller that
// piped into us may have more to do with it.
func openInput(args []string, opts *options) (io.Reader, func() error, error) {
	if len(args) == 0 || args[0] == "-" {
		if opts.stdin != nil {
			return opts.stdin, func() error { return nil }, nil
		}
		return os.Stdin, func() error { return nil }, nil
	}

	file, err := os.Open(args[0])
	if err != nil {
		return nil, nil, fmt.Errorf("read the zonefile: %w", err)
	}
	// Handed over as a plain reader rather than as the file. A request closes
	// a body it can close, so passing the file would have it closed twice —
	// once by the transport and once here, and the second close is an error
	// that would surface as the command's failure. Ownership stays here, which
	// is also the only place that can close it when the request is never made.
	return readerOnly{file}, file.Close, nil
}

// readerOnly hides a reader's Close method from whoever it is handed to.
type readerOnly struct{ io.Reader }

// deref reads an optional slice, falling back to nil.
func deref[T any](p *[]T, fallback []T) []T {
	if p == nil {
		return fallback
	}
	return *p
}
