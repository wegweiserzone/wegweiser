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

// newRecordCommand groups everything that acts on the records inside a zone.
func newRecordCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "record",
		Aliases: []string{"r", "rr"},
		Short:   "Work with the records inside a zone",
		Args:    usageArgs(cobra.NoArgs),
		RunE:    func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newRecordListCommand(opts, &f))
	cmd.AddCommand(newRecordAddCommand(opts, &f))
	cmd.AddCommand(newRecordUpdateCommand(opts, &f))
	cmd.AddCommand(newRecordEnableCommand(opts, &f))
	cmd.AddCommand(newRecordDisableCommand(opts, &f))
	cmd.AddCommand(newRecordDetachCommand(opts, &f))
	cmd.AddCommand(newRecordDeleteCommand(opts, &f))
	return cmd
}

// recordListed is one line of a listing.
type recordListed struct {
	Name      string `json:"name"`
	TTL       int64  `json:"ttl"`
	Class     string `json:"class"`
	Type      string `json:"type"`
	Data      string `json:"data"`
	Disabled  bool   `json:"disabled,omitempty"`
	Generated bool   `json:"generated,omitempty"`
	ID        string `json:"id"`
	Comment   string `json:"comment,omitempty"`
}

func newRecordListCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		name   string
		typ    string
		search string
		limit  int
	)

	cmd := &cobra.Command{
		Use:     "list ZONE",
		Aliases: []string{"ls"},
		Short:   "List the records of a zone",
		Long: "List what a zone holds, in the order the server stores it.\n\n" +
			"A record this server generated (the PTR for an address, the CNAME of\n" +
			"an RFC 2317 delegation) is marked as such: it follows the record it\n" +
			"came from, and editing it means taking it over first.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg record list example.com\n" +
			"  weg record list example.com --type A\n" +
			"  weg record list example.com --name www.example.com.\n" +
			"  weg record list example.com --search 192.0.2.10\n" +
			"  weg record list example.com --output json | jq -r '.[].data'",

		RunE: func(c *cobra.Command, args []string) error {
			return runRecordList(c.Context(), opts, f, args[0], name, typ, search, limit)
		},
		ValidArgsFunction: completeZones(f),
	}

	cmd.Flags().StringVar(&name, "name", "", "only this owner name")
	cmd.Flags().StringVar(&typ, "type", "", "only this record type")
	cmd.Flags().StringVar(&search, "search", "",
		"only records whose name or data contains this, case-insensitively")
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after this many records (0 is all of them)")
	registerFlagCompletion(cmd, "type", completeStatic(recordTypes...))
	return cmd
}

func runRecordList(
	ctx context.Context, opts *options, f *clientFlags,
	zoneName, name, typ, search string, limit int,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, zoneName)
	if err != nil {
		return err
	}

	var params gen.ListRecordsParams
	if name != "" {
		params.Name = ptr(qualify(name, z.Name))
	}
	if typ != "" {
		params.Type = ptr(strings.ToUpper(typ))
	}
	if search != "" {
		params.Search = ptr(search)
	}

	records, err := allRecords(ctx, client, f, z.Id, params, limit)
	if err != nil {
		return err
	}

	listed := make([]recordListed, 0, len(records))
	for i := range records {
		listed = append(listed, listRecord(&records[i]))
	}

	p := opts.Printer()
	return p.Print(listed, func(w io.Writer) error {
		if len(listed) == 0 {
			_, werr := fmt.Fprintf(w, "%s holds no record matching that. "+
				"`weg record add %s NAME TYPE DATA` puts one in\n", z.Name, z.Name)
			return werr
		}

		t := newTable(w, "NAME", "TTL", "TYPE", "DATA")
		for _, r := range listed {
			// The mark goes on the end of the last column: it is the one place
			// colour does not push the columns out of line, and it reads as an
			// aside rather than as a field, which is what it is.
			data := r.Data
			switch {
			case r.Disabled:
				data += p.Paint(output.ColorYellow, "  (disabled)")
			case r.Generated:
				data += p.Paint(output.ColorDim, "  (generated)")
			}
			t.row(r.Name, fmt.Sprint(r.TTL), r.Type, data)
		}
		return t.flush()
	})
}

// allRecords follows the cursor to the end, or until limit rows are in hand.
func allRecords(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags,
	zoneID string, params gen.ListRecordsParams, limit int,
) ([]gen.Record, error) {
	var out []gen.Record
	size := pageSize
	if limit > 0 && limit < size {
		size = limit
	}
	params.Limit = ptr(size)

	for {
		resp, err := client.ListRecordsWithResponse(ctx, zoneID, &params)
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

func listRecord(r *gen.Record) recordListed {
	return recordListed{
		Name:      r.Name,
		TTL:       r.Ttl,
		Class:     r.Class,
		Type:      r.Type,
		Data:      r.Data,
		Disabled:  r.Disabled,
		Generated: r.ManagedBy != nil || r.ManagedKind != nil,
		ID:        r.Id,
		Comment:   opt(r.Comment, ""),
	}
}

// qualify completes an owner name against the zone apex, the way a zonefile
// does: `@` is the apex, a name with a trailing dot is already absolute, and
// anything else is relative to the zone.
func qualify(name, apex string) string {
	switch {
	case name == "@" || name == "":
		return apex
	case strings.HasSuffix(name, "."):
		return name
	default:
		return name + "." + apex
	}
}

// recordConflict is an address that already answers with another name.
type recordConflict struct {
	Address       string `json:"address"`
	ExistingName  string `json:"existingName"`
	RequestedName string `json:"requestedName"`
	Policy        string `json:"policy"`
}

// String says what the policy in force did about the conflict. The wording has
// to follow the policy: under last-wins an entry was made, and reporting that
// none was would describe the opposite of what happened.
func (c recordConflict) String() string {
	switch c.Policy {
	case string(gen.LastWins):
		return fmt.Sprintf("%s answered with %s and now answers with %s",
			c.Address, c.ExistingName, c.RequestedName)
	case string(gen.Multi):
		return fmt.Sprintf("%s already answers with %s and now answers with %s as well",
			c.Address, c.ExistingName, c.RequestedName)
	default:
		return fmt.Sprintf("%s already answers with %s, so no entry was made for %s",
			c.Address, c.ExistingName, c.RequestedName)
	}
}

func newRecordAddCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		ttl  int64
		note string
	)

	cmd := &cobra.Command{
		Use:     "add ZONE NAME TYPE DATA...",
		Aliases: []string{"create", "new"},
		Short:   "Add a record to a zone",
		Long: "Add one record, written the way a zonefile writes it.\n\n" +
			"NAME is relative to the zone unless it ends in a dot, and `@` is the\n" +
			"zone itself. DATA is the rest of the line, so it needs no quoting\n" +
			"except where the shell would eat something.\n\n" +
			"An address record generates the matching PTR in whichever reverse\n" +
			"zone answers for it. What that produced, and any reverse zone it\n" +
			"would have needed, is printed rather than left for you to find.",
		Args: usageArgs(cobra.MinimumNArgs(4)),
		Example: "  weg record add example.com www A 192.0.2.10\n" +
			"  weg record add example.com @ MX 10 mail.example.com.\n" +
			"  weg record add example.com www AAAA 2001:db8::10 --ttl 60\n" +
			`  weg record add example.com @ TXT "v=spf1 -all"`,

		RunE: func(c *cobra.Command, args []string) error {
			in := gen.CreateRecord{
				Name: args[1],
				Type: strings.ToUpper(args[2]),
				// The rest of the line, joined back together: a record's data
				// is one field with spaces in it, and making somebody quote
				// `10 mail.example.com.` would be making them work around us.
				Data: strings.Join(args[3:], " "),
			}
			in.Data = quoteText(in.Type, in.Data)
			if c.Flags().Changed("ttl") {
				in.Ttl = &ttl
			}
			if note != "" {
				in.Comment = &note
			}
			return runRecordAdd(c.Context(), opts, f, args[0], in)
		},
		// A record being added need not exist yet, so the type argument gets
		// the whole vocabulary rather than what the name already has.
		ValidArgsFunction: completeRecordArgs(f, false),
	}

	cmd.Flags().Int64Var(&ttl, "ttl", 0, "the record's TTL (default the zone's)")
	cmd.Flags().StringVar(&note, "comment", "", "a note about what this record is for")
	return cmd
}

func runRecordAdd(
	ctx context.Context, opts *options, f *clientFlags, zoneName string, in gen.CreateRecord,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, zoneName)
	if err != nil {
		return err
	}
	in.Name = qualify(in.Name, z.Name)

	resp, err := client.CreateRecordWithResponse(ctx, z.Id, in)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON201 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return printRecordChange(opts, recordWritten(resp.JSON201), "added")
}

func newRecordDeleteCommand(opts *options, f *clientFlags) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "delete ZONE NAME TYPE [DATA...]",
		Aliases: []string{"rm", "remove"},
		Short:   "Delete a record from a zone",
		Long: "Delete one record, named the way it was added.\n\n" +
			"Without DATA the name and type have to identify exactly one record;\n" +
			"where they do not, the candidates are printed and nothing is deleted,\n" +
			"because guessing which of several a person meant is how the wrong one\n" +
			"goes.",
		Args: usageArgs(cobra.MinimumNArgs(3)),
		Example: "  weg record delete example.com www A 192.0.2.10\n" +
			"  weg record delete example.com www AAAA\n" +
			"  weg record delete example.com www A 192.0.2.10 --yes",

		RunE: func(c *cobra.Command, args []string) error {
			return runRecordDelete(c.Context(), opts, f, args[0], args[1],
				strings.ToUpper(args[2]), strings.Join(args[3:], " "), yes)
		},
		ValidArgsFunction: completeRecordArgs(f, true),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "delete without asking")
	return cmd
}

// recordDeleted is what a deletion reports.
type recordDeleted struct {
	Record recordListed `json:"record"`
}

func runRecordDelete(
	ctx context.Context, opts *options, f *clientFlags,
	zoneName, name, typ, data string, yes bool,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}
	z, err := findZone(ctx, client, f, zoneName)
	if err != nil {
		return err
	}

	target, err := resolveRecord(ctx, client, f, z, name, typ, data)
	if err != nil {
		return err
	}

	if !yes {
		if cerr := confirm(opts, fmt.Sprintf("delete %s %d IN %s %s",
			target.Name, target.Ttl, target.Type, target.Data)); cerr != nil {
			return cerr
		}
	}

	resp, err := client.DeleteRecordWithResponse(ctx, target.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.HTTPResponse.StatusCode != http.StatusNoContent {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	return opts.Printer().Print(recordDeleted{Record: listRecord(target)}, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w, "deleted %s %d IN %s %s\n",
			target.Name, target.Ttl, target.Type, target.Data)
		return werr
	})
}

// resolveRecord finds the one record a name, a type and possibly some data
// describe, inside a zone that has already been looked up.
func resolveRecord(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags,
	z *gen.Zone, name, typ, data string,
) (*gen.Record, error) {
	owner := qualify(name, z.Name)
	found, err := allRecords(ctx, client, f, z.Id, gen.ListRecordsParams{
		Name: &owner, Type: &typ,
	}, 0)
	if err != nil {
		return nil, err
	}
	return pickRecord(found, owner, typ, data)
}

// pickRecord finds the one record the arguments describe.
func pickRecord(found []gen.Record, name, typ, data string) (*gen.Record, error) {
	if data != "" {
		for i := range found {
			if strings.EqualFold(found[i].Data, data) {
				return &found[i], nil
			}
		}
		return nil, fmt.Errorf("%s holds no %s record with data %q", name, typ, data)
	}

	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%s holds no %s record", name, typ)
	case 1:
		return &found[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%s holds %d %s records; say which one:", name, len(found), typ)
		for i := range found {
			fmt.Fprintf(&b, "\n  %s", found[i].Data)
		}
		return nil, usageError{fmt.Errorf("%s", b.String())}
	}
}

// quoteText wraps the data of a text record in the quotes the presentation
// format needs, where the shell has already taken them off.
//
// A TXT record's data is one or more character-strings (RFC 1035 §3.3.14), and
// a space between two unquoted words is what separates them. So
// `weg record add example.com @ TXT "v=spf1 -all"` (where the shell keeps the
// quotes for itself) would otherwise arrive as two strings and be stored as
// `"v=spf1" "-all"`, which is not the SPF record anybody meant and is a
// silence that only shows up as mail being rejected.
func quoteText(typ, data string) string {
	switch typ {
	case "TXT", "SPF":
	default:
		// Every other type either has no character-string in it or has
		// structure around it, HINFO's two strings, CAA's tag and value —
		// that quoting the whole line would destroy.
		return data
	}
	if strings.HasPrefix(strings.TrimSpace(data), `"`) {
		return data
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(data) + `"`
}
