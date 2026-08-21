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
)

// newTokenCommand groups the credentials that may use this API.
func newTokenCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "token",
		Aliases: []string{"tokens"},
		Short:   "Manage the credentials that may use this server",
		Long: "Manage API tokens.\n\n" +
			"Managing them needs the admin scope, which is the one place the\n" +
			"method does not decide what a request needs: a write token that could\n" +
			"mint an admin token would not be a write token.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	cmd.AddCommand(newTokenListCommand(opts, &f))
	cmd.AddCommand(newTokenCreateCommand(opts, &f))
	cmd.AddCommand(newTokenRevokeCommand(opts, &f))
	return cmd
}

// tokenListed is one line of the listing. It never carries a secret: what the
// server keeps is a SHA-256, and it could not print one if it wanted to.
type tokenListed struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

func newTokenListCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the tokens this server accepts",
		Long: "List every token, including the revoked and expired ones, so that a\n" +
			"name in the history still looks up to something.\n\n" +
			"No secret is shown, here or anywhere: the server keeps a SHA-256 and\n" +
			"could not print one if it wanted to. The prefix is there to tell two\n" +
			"tokens apart, not to authenticate with.",
		Args:    usageArgs(cobra.NoArgs),
		Example: "  weg token list\n  weg token list --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			return runTokenList(c.Context(), opts, f)
		},
	}
}

func runTokenList(ctx context.Context, opts *options, f *clientFlags) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	tokens, err := fetchTokens(ctx, client, f)
	if err != nil {
		return err
	}

	listed := make([]tokenListed, 0, len(tokens))
	for i := range tokens {
		listed = append(listed, listToken(&tokens[i], time.Now()))
	}

	p := opts.Printer()
	return p.Print(listed, func(w io.Writer) error {
		if len(listed) == 0 {
			_, werr := fmt.Fprintln(w, "no tokens. `weg token create NAME --scope admin` mints one")
			return werr
		}

		t := newTable(w, "NAME", "PREFIX", "SCOPES", "LAST USED", "STATUS")
		for i := range listed {
			tok := &listed[i]
			colour := output.ColorGreen
			switch tok.Status {
			case "revoked", "expired":
				colour = output.ColorYellow
			}
			t.row(tok.Name, tok.Prefix, strings.Join(tok.Scopes, ","),
				since(tok.LastUsedAt), p.Paint(colour, tok.Status))
		}
		return t.flush()
	})
}

func fetchTokens(
	ctx context.Context, client *gen.ClientWithResponses, f *clientFlags,
) ([]gen.Token, error) {
	resp, err := client.ListTokensWithResponse(ctx)
	if err != nil {
		return nil, reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		return nil, apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return *resp.JSON200, nil
}

func listToken(t *gen.Token, now time.Time) tokenListed {
	out := tokenListed{
		ID: t.Id, Name: t.Name, Prefix: t.Prefix,
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
		LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt,
		Status: "active",
	}
	for _, s := range t.Scopes {
		out.Scopes = append(out.Scopes, string(s))
	}
	switch {
	case t.RevokedAt != nil:
		out.Status = "revoked"
	case t.ExpiresAt != nil && t.ExpiresAt.Before(now):
		out.Status = "expired"
	}
	return out
}

// since renders a moment as how long ago it was, which is what a person reads
// a "last used" column for. A token that has never been used says so.
func since(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// tokenMinted is what creating a token reports. The secret is in it because
// this is the only place it will ever be.
type tokenMinted struct {
	Secret    string     `json:"secret"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

func newTokenCreateCommand(opts *options, f *clientFlags) *cobra.Command {
	var (
		scopes  []string
		expires string
	)

	cmd := &cobra.Command{
		Use:     "create NAME",
		Aliases: []string{"mint", "add", "new"},
		Short:   "Mint a token",
		Long: "Mint a token and show its secret, once.\n\n" +
			"The secret is 256 bits from a cryptographic source, and what this\n" +
			"server keeps is its SHA-256, so a copy of the database is not a copy\n" +
			"of the credentials, and there is no command that shows it again. Put\n" +
			"it somewhere before the terminal scrolls.\n\n" +
			"The scopes are ordered: admin allows everything write allows, and\n" +
			"write everything read allows.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg token create ansible --scope write\n" +
			"  weg token create prometheus --scope read --expires 2027-01-01\n" +
			"  export WEG_TOKEN=$(weg token create ci --scope write --output json | jq -r .secret)",

		RunE: func(c *cobra.Command, args []string) error {
			in := gen.CreateToken{Name: args[0]}
			for _, s := range scopes {
				scope := gen.Scope(strings.ToLower(s))
				if !scope.Valid() {
					return usageError{fmt.Errorf(
						"%q is not a scope; they are read, write and admin", s)}
				}
				in.Scopes = append(in.Scopes, scope)
			}
			if len(in.Scopes) == 0 {
				return usageError{fmt.Errorf(
					"a token needs at least one scope: pass --scope read, write or admin")}
			}
			if expires != "" {
				when, err := parseWhen(expires)
				if err != nil {
					return usageError{fmt.Errorf("--expires: %w", err)}
				}
				in.ExpiresAt = &when
			}
			return runTokenCreate(c.Context(), opts, f, in)
		},
	}

	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"what the token may do: read, write or admin (repeatable)")
	cmd.Flags().StringVar(&expires, "expires", "",
		"when it stops working (default never)")
	registerFlagCompletion(cmd, "scope", completeStatic("read", "write", "admin"))
	return cmd
}

func runTokenCreate(
	ctx context.Context, opts *options, f *clientFlags, in gen.CreateToken,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	resp, err := client.CreateTokenWithResponse(ctx, in)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON201 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	out := resp.JSON201

	minted := tokenMinted{
		Secret:    out.Secret,
		Name:      out.Token.Name,
		ExpiresAt: out.Token.ExpiresAt,
	}
	for _, s := range out.Token.Scopes {
		minted.Scopes = append(minted.Scopes, string(s))
	}

	p := opts.Printer()
	return p.Print(minted, func(w io.Writer) error {
		// The secret goes to standard output on its own line, so that it can
		// be captured; everything about it goes to standard error, so that
		// capturing it does not capture the prose as well.
		if _, werr := fmt.Fprintln(w, minted.Secret); werr != nil {
			return werr
		}
		_, werr := fmt.Fprintf(p.ErrOut(),
			"minted %q with %s. This is the only time the secret is shown — "+
				"what the server keeps is its hash.\n",
			minted.Name, strings.Join(minted.Scopes, ", "))
		return werr
	})
}

func newTokenRevokeCommand(opts *options, f *clientFlags) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "revoke NAME",
		Aliases: []string{"rm", "delete", "remove"},
		Short:   "Withdraw a token",
		Long: "Stop a token working, naming it the way `weg token list` does.\n\n" +
			"The token is marked unusable rather than removed, so that the history\n" +
			"it appears in still names something. The last token that can still\n" +
			"administer this server is refused: a server nobody can reach is not a\n" +
			"state it will put itself in.",
		Args:    usageArgs(cobra.ExactArgs(1)),
		Example: "  weg token revoke ansible\n  weg token revoke ansible --yes",

		RunE: func(c *cobra.Command, args []string) error {
			return runTokenRevoke(c.Context(), opts, f, args[0], yes)
		},
		ValidArgsFunction: completeTokens(f),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "revoke without asking")
	return cmd
}

// tokenRevoked is what a revocation reports.
type tokenRevoked struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

func runTokenRevoke(
	ctx context.Context, opts *options, f *clientFlags, name string, yes bool,
) error {
	client, err := f.client()
	if err != nil {
		return err
	}

	tokens, err := fetchTokens(ctx, client, f)
	if err != nil {
		return err
	}
	target, err := pickToken(tokens, name)
	if err != nil {
		return err
	}

	if !yes {
		if cerr := confirm(opts, fmt.Sprintf(
			"stop the token %q (%s) working", target.Name, target.Prefix)); cerr != nil {
			return cerr
		}
	}

	resp, err := client.RevokeTokenWithResponse(ctx, target.Id)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.HTTPResponse.StatusCode != http.StatusNoContent {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}

	return opts.Printer().Print(
		tokenRevoked{Name: target.Name, Prefix: target.Prefix},
		func(w io.Writer) error {
			_, werr := fmt.Fprintf(w, "revoked %q (%s); it is still in the history\n",
				target.Name, target.Prefix)
			return werr
		})
}

// pickToken finds the token a person named, by its name or by its prefix.
func pickToken(tokens []gen.Token, name string) (*gen.Token, error) {
	var matches []*gen.Token
	for i := range tokens {
		if tokens[i].Name == name || tokens[i].Prefix == name || tokens[i].Id == name {
			matches = append(matches, &tokens[i])
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no token called %q; `weg token list` shows them", name)
	case 1:
		return matches[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%d tokens are called %q; name one by its prefix:", len(matches), name)
		for _, t := range matches {
			fmt.Fprintf(&b, "\n  %s", t.Prefix)
		}
		return nil, usageError{fmt.Errorf("%s", b.String())}
	}
}
