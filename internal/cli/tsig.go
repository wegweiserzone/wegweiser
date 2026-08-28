package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// newTSIGCommand groups the keys a secondary signs a transfer with.
func newTSIGCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "tsig",
		Aliases: []string{"key", "keys"},
		Short:   "Manage the keys a secondary signs a zone transfer with",
		Long: "Manage TSIG keys (RFC 8945).\n\n" +
			"A key on the transfer list grants a transfer from any address, which\n" +
			"is what an address list cannot do: tell two hosts behind one NAT\n" +
			"apart, or authenticate a secondary somebody else runs.\n\n" +
			"Unlike an API token, a secret can be read back. Verifying a signature\n" +
			"means recomputing it, so this server has to keep it.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newTSIGListCommand(opts, &f))
	cmd.AddCommand(newTSIGCreateCommand(opts, &f))
	cmd.AddCommand(newTSIGShowCommand(opts, &f))
	cmd.AddCommand(newTSIGRevokeCommand(opts, &f))
	return cmd
}

// tsigListed is one line of the listing, without the secret.
type tsigListed struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Algorithm string     `json:"algorithm"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

func newTSIGListCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the keys this server holds",
		Long: "List every key, withdrawn ones included, so that a name a secondary\n" +
			"still has configured looks up to something.\n\n" +
			"No secret is shown here. `weg tsig show NAME` reads one back.",
		Args:    usageArgs(cobra.NoArgs),
		Example: "  weg tsig list\n  weg tsig list --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			return runTSIGList(c.Context(), opts, f)
		},
	}
}

func runTSIGList(ctx context.Context, opts *options, f *clientFlags) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	keys, err := fetchTSIGKeys(ctx, client, f)
	if err != nil {
		return err
	}

	listed := make([]tsigListed, 0, len(keys))
	for i := range keys {
		listed = append(listed, listTSIGKey(&keys[i]))
	}

	p := opts.Printer()
	return p.Print(listed, func(w io.Writer) error {
		if len(listed) == 0 {
			_, werr := fmt.Fprintln(w,
				"no keys. `weg tsig create NAME` makes one, and "+
					"`weg settings set --transfer-allow key:NAME` lets it transfer")
			return werr
		}

		t := newTable(w, "NAME", "ALGORITHM", "CREATED", "STATUS")
		for i := range listed {
			k := &listed[i]
			colour := output.ColorGreen
			if k.Status != "active" {
				colour = output.ColorYellow
			}
			t.row(k.Name, k.Algorithm, since(&k.CreatedAt), p.Paint(colour, k.Status))
		}
		return t.flush()
	})
}

func fetchTSIGKeys(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags,
) ([]gen.TSIGKey, error) {
	resp, err := client.ListTSIGKeysWithResponse(ctx)
	if err != nil {
		return nil, reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return nil, apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return *resp.JSON200, nil
}

func listTSIGKey(k *gen.TSIGKey) tsigListed {
	out := tsigListed{
		ID:        k.Id,
		Name:      k.Name,
		Algorithm: strings.TrimSuffix(string(k.Algorithm), "."),
		Status:    "active",
		CreatedAt: k.CreatedAt,
		RevokedAt: k.RevokedAt,
	}
	if k.RevokedAt != nil {
		out.Status = "withdrawn"
	}
	return out
}

func newTSIGCreateCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		algorithm string
		secret    string
	)

	cmd := &cobra.Command{
		Use:     "create NAME",
		Aliases: []string{"add", "new"},
		Short:   "Create a key",
		Long: "Create a key and print its secret.\n\n" +
			"Without --secret one is generated, at least as long as the hash output\n" +
			"of the algorithm, which is what an operator configuring both ends\n" +
			"wants. Pass one to match a secondary that already has a key.\n\n" +
			"Creating a key does not let it transfer anything. Put it on the list\n" +
			"with `weg settings set --transfer-allow key:NAME`.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg tsig create secondary.example.com.\n" +
			"  weg tsig create ns2.example.com. --algorithm hmac-sha512\n" +
			"  weg tsig create ns3.example.com. --secret \"$(cat secret.b64)\"",

		RunE: func(c *cobra.Command, args []string) error {
			name, err := zone.ParseName(args[0])
			if err != nil {
				return usageError{fmt.Errorf(
					"a key is named in domain name syntax, and %q is not: %w", args[0], err)}
			}

			in := gen.CreateTSIGKey{Name: name.String()}
			if algorithm != "" {
				alg, aerr := zone.ParseTSIGAlgorithm(algorithm)
				if aerr != nil {
					return usageError{fmt.Errorf("--algorithm: %w", aerr)}
				}
				in.Algorithm = ptr(gen.TSIGAlgorithm(alg))
			}
			if secret != "" {
				in.Secret = &secret
			}
			return runTSIGCreate(c.Context(), opts, f, in)
		},
	}

	cmd.Flags().StringVar(&algorithm, "algorithm", "",
		"the keyed hash it signs with: "+tsigAlgorithmNames()+" (default hmac-sha256)")
	cmd.Flags().StringVar(&secret, "secret", "",
		"the shared secret, base64; generated when left out")
	registerFlagCompletion(cmd, "algorithm", completeStatic(tsigAlgorithms()...))
	return cmd
}

// tsigCreated is what creating a key reports.
type tsigCreated struct {
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	Secret    string `json:"secret"`
}

func runTSIGCreate(
	ctx context.Context, opts *options, f *clientFlags, in gen.CreateTSIGKey,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	resp, err := client.CreateTSIGKeyWithResponse(ctx, in)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON201 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return printTSIGSecret(opts, tsigCreated{
		Name:      resp.JSON201.Key.Name,
		Algorithm: strings.TrimSuffix(string(resp.JSON201.Key.Algorithm), "."),
		Secret:    resp.JSON201.Secret,
	}, "created")
}

func newTSIGShowCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "show NAME",
		Aliases: []string{"secret", "get"},
		Short:   "Print a key's secret",
		Long: "Print the secret of a key, so it can be configured on the other end.\n\n" +
			"A command of its own rather than a column in the listing, so that a\n" +
			"secret appears when somebody asked for it. It is not a boundary: the\n" +
			"database holds it either way.",
		Args:              usageArgs(cobra.ExactArgs(1)),
		Example:           "  weg tsig show secondary.example.com.",
		ValidArgsFunction: completeTSIGKeys(f),

		RunE: func(c *cobra.Command, args []string) error {
			return runTSIGShow(c.Context(), opts, f, args[0])
		},
	}
}

func runTSIGShow(ctx context.Context, opts *options, f *clientFlags, name string) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	target, err := pickTSIGKey(ctx, client, f, name)
	if err != nil {
		return err
	}

	resp, err := client.ReadTSIGKeySecretWithResponse(ctx, target.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return printTSIGSecret(opts, tsigCreated{
		Name:      resp.JSON200.Key.Name,
		Algorithm: strings.TrimSuffix(string(resp.JSON200.Key.Algorithm), "."),
		Secret:    resp.JSON200.Secret,
	}, "")
}

// printTSIGSecret writes the secret and says what it is for.
//
// The secret goes to standard output on its own line so it can be captured;
// everything about it goes to standard error, so that capturing it does not
// capture the prose as well.
func printTSIGSecret(opts *options, k tsigCreated, verb string) error {
	p := opts.Printer()
	return p.Print(k, func(w io.Writer) error {
		if _, werr := fmt.Fprintln(w, k.Secret); werr != nil {
			return werr
		}
		what := fmt.Sprintf("%q signs with %s.", k.Name, k.Algorithm)
		if verb != "" {
			what = fmt.Sprintf("%s %q, signing with %s.", verb, k.Name, k.Algorithm)
		}
		_, werr := fmt.Fprintf(p.ErrOut(),
			"%s Configure the same name, algorithm and secret on the secondary, and put the "+
				"key on the transfer list with `weg settings set --transfer-allow key:%s`.\n",
			what, k.Name)
		return werr
	})
}

func newTSIGRevokeCommand(opts *options, f *clientFlags) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "revoke NAME",
		Aliases: []string{"rm", "delete", "remove"},
		Short:   "Withdraw a key",
		Long: "Stop a key signing.\n\n" +
			"Its secret is cleared, which is the one thing withdrawing a token does\n" +
			"not do: a token leaves only a hash behind, and a key would leave\n" +
			"material nothing will read again. The name and the dates stay, and the\n" +
			"name is free for a replacement, so rotating a key does not mean\n" +
			"renaming it on the secondary.",
		Args:              usageArgs(cobra.ExactArgs(1)),
		Example:           "  weg tsig revoke secondary.example.com.\n  weg tsig revoke ns2.example.com. --yes",
		ValidArgsFunction: completeTSIGKeys(f),

		RunE: func(c *cobra.Command, args []string) error {
			return runTSIGRevoke(c.Context(), opts, f, args[0], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "withdraw without asking")
	return cmd
}

// tsigRevoked is what a withdrawal reports.
type tsigRevoked struct {
	Name string `json:"name"`
}

func runTSIGRevoke(
	ctx context.Context, opts *options, f *clientFlags, name string, yes bool,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	target, err := pickTSIGKey(ctx, client, f, name)
	if err != nil {
		return err
	}

	if !yes {
		if cerr := confirm(opts, fmt.Sprintf(
			"stop the key %q signing, and clear its secret", target.Name)); cerr != nil {
			return cerr
		}
	}

	resp, err := client.RevokeTSIGKeyWithResponse(ctx, target.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.StatusCode() != http.StatusNoContent {
		return apiError(resp.StatusCode(), resp.Body)
	}

	p := opts.Printer()
	return p.Print(tsigRevoked{Name: target.Name}, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w, "%s no longer signs, and its secret is gone\n", target.Name)
		return werr
	})
}

// pickTSIGKey resolves the name a command was given, accepting it with or
// without its trailing dot.
func pickTSIGKey(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags, name string,
) (*gen.TSIGKey, error) {
	keys, err := fetchTSIGKeys(ctx, client, f)
	if err != nil {
		return nil, err
	}

	wanted, perr := zone.ParseName(name)
	if perr != nil {
		return nil, fmt.Errorf("%q is not a key name: %w", name, perr)
	}

	var withdrawn *gen.TSIGKey
	for i := range keys {
		held, herr := zone.ParseName(keys[i].Name)
		if herr != nil || !held.Equal(wanted) {
			continue
		}
		if keys[i].RevokedAt == nil {
			return &keys[i], nil
		}
		withdrawn = &keys[i]
	}
	if withdrawn != nil {
		return withdrawn, nil
	}
	return nil, fmt.Errorf("no key named %s; `weg tsig list` shows the ones there are", wanted)
}

// tsigAlgorithms are the names the flag accepts, without the trailing dot an
// operator would not type.
func tsigAlgorithms() []string {
	out := make([]string, 0, len(zone.TSIGAlgorithms()))
	for _, a := range zone.TSIGAlgorithms() {
		out = append(out, strings.TrimSuffix(a.String(), "."))
	}
	return out
}

func tsigAlgorithmNames() string { return strings.Join(tsigAlgorithms(), ", ") }
