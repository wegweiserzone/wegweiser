package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
	"github.com/wegweiserzone/wegweiser/internal/suggest"
)

// newIDPCommand groups the providers people sign in through.
func newIDPCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:     "idp",
		Aliases: []string{"idps", "provider", "providers"},
		Short:   "Manage the identity providers people sign in through",
		Long: "Manage the OpenID Connect providers and LDAP directories people sign\n" +
			"in through, and which of their groups is given which scope.\n\n" +
			"A person holds the strongest scope any of their groups maps to, and a\n" +
			"person in no mapped group is refused, however well they signed in.\n" +
			"Tokens go on working beside them, and are what a program should use.\n\n" +
			"A client secret or bind password is read from a file, or from standard\n" +
			"input with -, so that it stays out of the shell's history. It is never\n" +
			"shown again.",
		Args: usageArgs(cobra.NoArgs),
		Example: "  weg idp add oidc Authentik --issuer https://auth.example.com/application/o/weg/ \\\n" +
			"      --client-id weg --client-secret-file secret.txt \\\n" +
			"      --redirect-uri https://ns1.example.com/api/v1/auth/oidc/callback --map dns-admins=admin\n" +
			"  weg idp add ldap FreeIPA --url ldaps://ipa.example.com \\\n" +
			"      --user-base cn=users,cn=accounts,dc=example,dc=com --user-filter '(uid={username})' \\\n" +
			"      --ca-file /etc/ipa/ca.crt --map admins=admin\n" +
			"  weg idp list",
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	f.register(cmd)

	add := &cobra.Command{
		Use:   "add",
		Short: "Add a provider",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	add.AddCommand(newIDPAddCommand(opts, &f, gen.IdentityProviderKindOidc))
	add.AddCommand(newIDPAddCommand(opts, &f, gen.IdentityProviderKindLdap))

	cmd.AddCommand(newIDPListCommand(opts, &f))
	cmd.AddCommand(newIDPShowCommand(opts, &f))
	cmd.AddCommand(add)
	cmd.AddCommand(newIDPSetCommand(opts, &f))
	cmd.AddCommand(newIDPRemoveCommand(opts, &f))
	return cmd
}

// idpListed is one line of the listing.
type idpListed struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Status   string   `json:"status"`
	Mappings []string `json:"mappings"`
}

func newIDPListCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the providers",
		Args:    usageArgs(cobra.NoArgs),
		Example: "  weg idp list\n  weg idp list --output json",
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := f.client()
			if err != nil {
				return err
			}
			list, err := fetchIDPs(c.Context(), client)
			if err != nil {
				return err
			}
			listed := make([]idpListed, 0, len(list))
			for i := range list {
				listed = append(listed, listIDP(&list[i]))
			}
			p := opts.Printer()
			return p.Print(listed, func(w io.Writer) error {
				if len(listed) == 0 {
					_, werr := fmt.Fprintln(w, "no identity providers. `weg idp add oidc` or `weg idp add ldap` adds one")
					return werr
				}
				t := newTable(w, "NAME", "KIND", "STATUS", "GROUPS")
				for i := range listed {
					l := &listed[i]
					colour := output.ColorNone
					if l.Status != "enabled" {
						colour = output.ColorYellow
					}
					groups := strings.Join(l.Mappings, ", ")
					if groups == "" {
						groups = p.Paint(output.ColorYellow, "none mapped, so nobody signs in")
					}
					t.row(l.Name, l.Kind, p.Paint(colour, l.Status), groups)
				}
				return t.flush()
			})
		},
	}
}

func listIDP(p *gen.IdentityProvider) idpListed {
	out := idpListed{ID: p.Id, Name: p.Name, Kind: string(p.Kind), Status: "enabled", Mappings: []string{}}
	if !p.Enabled {
		out.Status = "disabled"
	}
	for _, m := range p.Mappings {
		out.Mappings = append(out.Mappings, m.Group+"="+string(m.Scope))
	}
	return out
}

func newIDPShowCommand(opts *options, f *clientFlags) *cobra.Command {
	return &cobra.Command{
		Use:               "show NAME",
		Aliases:           []string{"get"},
		Short:             "Print what a provider says about itself",
		Long:              "Print a provider's settings. Its client secret or bind password is not\namong them: it is written and never read back.",
		Args:              usageArgs(cobra.ExactArgs(1)),
		Example:           "  weg idp show Authentik\n  weg idp show FreeIPA --output json",
		ValidArgsFunction: completeIDPs(f),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.client()
			if err != nil {
				return err
			}
			p, err := pickIDP(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			return opts.Printer().Print(p, func(w io.Writer) error { return writeIDP(w, p) })
		},
	}
}

// writeIDP lists a provider's settings, one to a line.
func writeIDP(w io.Writer, p *gen.IdentityProvider) error {
	l := listIDP(p)
	lines := [][2]string{
		{"name", p.Name}, {"kind", l.Kind}, {"status", l.Status},
		{"groups", strings.Join(l.Mappings, ", ")},
	}
	if p.CaCertificate != nil {
		lines = append(lines, [2]string{"ca", "a certificate of its own"})
	}
	if o := p.Oidc; o != nil {
		lines = append(lines,
			[2]string{"issuer", o.Issuer}, [2]string{"client id", o.ClientId},
			[2]string{"scopes", strings.Join(valueOf(o.Scopes), " ")},
			[2]string{"groups claim", valueOf(o.GroupsClaim)}, [2]string{"name claim", valueOf(o.NameClaim)},
			[2]string{"redirect uris", strings.Join(o.RedirectUris, ", ")})
	}
	if d := p.Ldap; d != nil {
		lines = append(lines, [2]string{"url", d.Url})
		if valueOf(d.StartTls) {
			lines = append(lines, [2]string{"starttls", "yes"})
		}
		if d.BindDn != nil {
			lines = append(lines, [2]string{"bind dn", *d.BindDn})
		}
		lines = append(lines,
			[2]string{"user base", d.UserBase}, [2]string{"user filter", d.UserFilter},
			[2]string{"name attribute", valueOf(d.NameAttribute)},
			[2]string{"groups from", string(valueOf(d.Groups))})
		if d.GroupBase != nil {
			lines = append(lines, [2]string{"group base", *d.GroupBase})
		}
		if d.GroupFilter != nil {
			lines = append(lines, [2]string{"group filter", *d.GroupFilter})
		}
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(w, "%-15s %s\n", line[0], line[1]); err != nil {
			return err
		}
	}
	return nil
}

// idpFlags are what adding or changing a provider can say. A flag left out of
// a change leaves that setting as it was.
type idpFlags struct {
	caFile   string
	mappings []string
	disabled bool
	enabled  bool

	issuer, clientID, clientSecretFile string
	scopes                             []string
	groupsClaim, nameClaim             string
	redirectURIs                       []string

	url, bindDN, bindPasswordFile       string
	startTLS                            bool
	userBase, userFilter, nameAttribute string
	groups, groupBase, groupFilter      string
}

func (fl *idpFlags) registerCommon(cmd *cobra.Command) {
	cmd.Flags().StringVar(&fl.caFile, "ca-file", "", "a PEM file with the CA the provider's certificate is signed by")
	cmd.Flags().StringArrayVar(&fl.mappings, "map", nil,
		"GROUP=SCOPE: give the members of a group a scope (read, write or admin); repeatable")
	cmd.Flags().BoolVar(&fl.disabled, "disabled", false, "offer it nobody to sign in through, for now")
}

func (fl *idpFlags) registerOIDC(cmd *cobra.Command) {
	cmd.Flags().StringVar(&fl.issuer, "issuer", "", "the provider's issuer URL")
	cmd.Flags().StringVar(&fl.clientID, "client-id", "", "the client ID this server is registered under")
	cmd.Flags().StringVar(&fl.clientSecretFile, "client-secret-file", "",
		"a file holding the client secret, or - for standard input")
	cmd.Flags().StringSliceVar(&fl.scopes, "scopes", nil, "the scopes asked for (default openid,profile,email,groups)")
	cmd.Flags().StringVar(&fl.groupsClaim, "groups-claim", "", "the claim the groups are read from (default groups)")
	cmd.Flags().StringVar(&fl.nameClaim, "name-claim", "", "the claim a person is named by (default preferred_username)")
	cmd.Flags().StringArrayVar(&fl.redirectURIs, "redirect-uri", nil,
		"a callback registered with the provider, ending in /api/v1/auth/oidc/callback; one per member")
}

func (fl *idpFlags) registerLDAP(cmd *cobra.Command) {
	cmd.Flags().StringVar(&fl.url, "url", "", "ldaps://host, or ldap://host with --start-tls")
	cmd.Flags().BoolVar(&fl.startTLS, "start-tls", false, "upgrade an ldap:// connection with StartTLS")
	cmd.Flags().StringVar(&fl.bindDN, "bind-dn", "", "who searches for a person's entry; left out, the search is anonymous")
	cmd.Flags().StringVar(&fl.bindPasswordFile, "bind-password-file", "",
		"a file holding the bind password, or - for standard input")
	cmd.Flags().StringVar(&fl.userBase, "user-base", "", "the base people are searched for under")
	cmd.Flags().StringVar(&fl.userFilter, "user-filter", "", "the filter finding a person, holding {username}")
	cmd.Flags().StringVar(&fl.nameAttribute, "name-attribute", "", "the attribute a person is named by (default uid)")
	cmd.Flags().StringVar(&fl.groups, "groups", "", "where the groups are read from: memberOf or search (default memberOf)")
	registerFlagCompletion(cmd, "groups", completeStatic("memberOf", "search"))
	cmd.Flags().StringVar(&fl.groupBase, "group-base", "", "for a search, the base groups are searched for under")
	cmd.Flags().StringVar(&fl.groupFilter, "group-filter", "", "for a search, the filter naming a person's groups, holding {dn} or {username}")
}

// apply writes the flags that were given onto in.
func (fl *idpFlags) apply(cmd *cobra.Command, opts *options, in *gen.IdentityProviderInput) error {
	changed := cmd.Flags().Changed
	if changed("ca-file") {
		raw, err := os.ReadFile(fl.caFile)
		if err != nil {
			return usageError{fmt.Errorf("--ca-file: %w", err)}
		}
		in.CaCertificate = ptr(string(raw))
	}
	if changed("map") {
		mappings := make([]gen.GroupMapping, 0, len(fl.mappings))
		for _, m := range fl.mappings {
			group, scope, ok := strings.Cut(m, "=")
			if !ok || strings.TrimSpace(group) == "" {
				return usageError{fmt.Errorf("--map %q: write it GROUP=SCOPE, such as dns-admins=admin", m)}
			}
			switch gen.Scope(scope) {
			case gen.ScopeRead, gen.ScopeWrite, gen.ScopeAdmin:
			default:
				return usageError{fmt.Errorf("--map %q: the scopes are read, write and admin", m)}
			}
			mappings = append(mappings, gen.GroupMapping{Group: group, Scope: gen.Scope(scope)})
		}
		in.Mappings = &mappings
	}
	switch {
	case changed("disabled"):
		in.Enabled = ptr(!fl.disabled)
	case changed("enabled"):
		in.Enabled = ptr(fl.enabled)
	}

	if o := in.Oidc; o != nil {
		setFlag(changed, "issuer", &o.Issuer, fl.issuer)
		setFlag(changed, "client-id", &o.ClientId, fl.clientID)
		if changed("client-secret-file") {
			secret, err := readSecret(opts, "--client-secret-file", fl.clientSecretFile)
			if err != nil {
				return err
			}
			o.ClientSecret = &secret
		}
		if changed("scopes") {
			o.Scopes = ptr(fl.scopes)
		}
		setFlagPtr(changed, "groups-claim", &o.GroupsClaim, fl.groupsClaim)
		setFlagPtr(changed, "name-claim", &o.NameClaim, fl.nameClaim)
		if changed("redirect-uri") {
			o.RedirectUris = fl.redirectURIs
		}
	}
	if d := in.Ldap; d != nil {
		setFlag(changed, "url", &d.Url, fl.url)
		if changed("start-tls") {
			d.StartTls = ptr(fl.startTLS)
		}
		setFlagPtr(changed, "bind-dn", &d.BindDn, fl.bindDN)
		if changed("bind-password-file") {
			secret, err := readSecret(opts, "--bind-password-file", fl.bindPasswordFile)
			if err != nil {
				return err
			}
			d.BindPassword = &secret
		}
		setFlag(changed, "user-base", &d.UserBase, fl.userBase)
		setFlag(changed, "user-filter", &d.UserFilter, fl.userFilter)
		setFlagPtr(changed, "name-attribute", &d.NameAttribute, fl.nameAttribute)
		if changed("groups") {
			d.Groups = ptr(gen.LDAPSettingsGroups(fl.groups))
		}
		setFlagPtr(changed, "group-base", &d.GroupBase, fl.groupBase)
		setFlagPtr(changed, "group-filter", &d.GroupFilter, fl.groupFilter)
	}
	return nil
}

func setFlag(changed func(string) bool, flag string, to *string, v string) {
	if changed(flag) {
		*to = v
	}
}

func setFlagPtr(changed func(string) bool, flag string, to **string, v string) {
	if changed(flag) {
		*to = &v
	}
}

// readSecret reads a secret from a file, or from standard input for -. One
// trailing line break is dropped, which is what an editor or echo leaves.
func readSecret(opts *options, flag, path string) (string, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		in := opts.stdin
		if in == nil {
			in = os.Stdin
		}
		raw, err = io.ReadAll(in)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", usageError{fmt.Errorf("%s: %w", flag, err)}
	}
	secret := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if secret == "" {
		return "", usageError{fmt.Errorf("%s: the secret is empty", flag)}
	}
	return secret, nil
}

func newIDPAddCommand(opts *options, f *clientFlags, kind gen.IdentityProviderKind) *cobra.Command {
	var fl idpFlags
	cmd := &cobra.Command{
		Use:  string(kind) + " NAME",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, args []string) error {
			in := gen.IdentityProviderInput{Name: args[0], Kind: kind}
			if kind == gen.IdentityProviderKindOidc {
				in.Oidc = &gen.OIDCSettings{}
			} else {
				in.Ldap = &gen.LDAPSettings{}
			}
			if err := fl.apply(c, opts, &in); err != nil {
				return err
			}
			client, err := f.client()
			if err != nil {
				return err
			}
			resp, err := client.CreateIdentityProviderWithResponse(c.Context(), in)
			if err != nil {
				return err
			}
			if resp.JSON201 == nil {
				return apiError(resp.HTTPResponse.StatusCode, resp.Body)
			}
			return printIDPChanged(opts, resp.JSON201, "added")
		},
	}
	fl.registerCommon(cmd)
	if kind == gen.IdentityProviderKindOidc {
		cmd.Short = "Add an OpenID Connect provider"
		cmd.Long = "Add an OpenID Connect provider, such as Authentik, Keycloak or Entra ID.\n\n" +
			"Register this server with it as a confidential client first, with one\n" +
			"redirect URI for each address a member is reached by, each ending in\n" +
			"/api/v1/auth/oidc/callback, and have it put the groups in a claim."
		cmd.Example = "  weg idp add oidc Authentik --issuer https://auth.example.com/application/o/weg/ \\\n" +
			"      --client-id weg --client-secret-file - \\\n" +
			"      --redirect-uri https://ns1.example.com/api/v1/auth/oidc/callback \\\n" +
			"      --map dns-admins=admin --map dns-readers=read"
		fl.registerOIDC(cmd)
		for _, required := range []string{"issuer", "client-id", "client-secret-file", "redirect-uri"} {
			mustRequire(cmd, required)
		}
	} else {
		cmd.Short = "Add an LDAP directory"
		cmd.Long = "Add a directory people bind to as themselves, such as FreeIPA, Active\n" +
			"Directory or OpenLDAP. Their password is passed to it and forgotten.\n\n" +
			"FreeIPA keeps memberOf on every entry, which is the default here. Active\n" +
			"Directory's nested groups are found with --groups search and\n" +
			"--group-filter '(member:1.2.840.113556.1.4.1941:={dn})'."
		cmd.Example = "  weg idp add ldap FreeIPA --url ldaps://ipa.example.com --ca-file /etc/ipa/ca.crt \\\n" +
			"      --bind-dn uid=weg,cn=sysaccounts,cn=etc,dc=example,dc=com --bind-password-file - \\\n" +
			"      --user-base cn=users,cn=accounts,dc=example,dc=com --user-filter '(uid={username})' \\\n" +
			"      --map admins=admin"
		fl.registerLDAP(cmd)
		for _, required := range []string{"url", "user-base", "user-filter"} {
			mustRequire(cmd, required)
		}
	}
	return cmd
}

// mustRequire marks a flag the command cannot do without. It fails only for a
// flag that does not exist, which is this file's mistake and not the user's.
func mustRequire(cmd *cobra.Command, flag string) {
	if err := cmd.MarkFlagRequired(flag); err != nil {
		panic(err)
	}
}

func newIDPSetCommand(opts *options, f *clientFlags) *cobra.Command {
	var fl idpFlags
	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Change a provider",
		Long: "Change the settings given, and leave the rest as they are. --map\n" +
			"replaces every mapping the provider had. A secret that is not given is\n" +
			"kept; a bind password only while the bind DN stays the same.",
		Args: usageArgs(cobra.ExactArgs(1)),
		Example: "  weg idp set Authentik --map dns-admins=admin --map dns-ops=write\n" +
			"  weg idp set FreeIPA --disabled",
		ValidArgsFunction: completeIDPs(f),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.client()
			if err != nil {
				return err
			}
			p, err := pickIDP(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			in := inputOf(p)
			if aerr := fl.apply(c, opts, &in); aerr != nil {
				return aerr
			}
			resp, err := client.UpdateIdentityProviderWithResponse(c.Context(), p.Id, in)
			if err != nil {
				return err
			}
			if resp.JSON200 == nil {
				return apiError(resp.HTTPResponse.StatusCode, resp.Body)
			}
			return printIDPChanged(opts, resp.JSON200, "changed")
		},
	}
	fl.registerCommon(cmd)
	cmd.Flags().BoolVar(&fl.enabled, "enabled", false, "offer it to sign in through again")
	cmd.MarkFlagsMutuallyExclusive("enabled", "disabled")
	fl.registerOIDC(cmd)
	fl.registerLDAP(cmd)
	return cmd
}

// inputOf is a provider as it stands, ready to be sent back with changes.
// The secrets are absent, which the server reads as keeping them.
func inputOf(p *gen.IdentityProvider) gen.IdentityProviderInput {
	in := gen.IdentityProviderInput{
		Name: p.Name, Kind: p.Kind, Enabled: ptr(p.Enabled),
		Mappings: ptr(p.Mappings), CaCertificate: p.CaCertificate,
	}
	if p.Oidc != nil {
		o := *p.Oidc
		o.ClientSecret = nil
		in.Oidc = &o
	}
	if p.Ldap != nil {
		d := *p.Ldap
		d.BindPassword = nil
		in.Ldap = &d
	}
	return in
}

// idpChanged is what adding or changing a provider reports.
type idpChanged struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func printIDPChanged(opts *options, p *gen.IdentityProvider, verb string) error {
	pr := opts.Printer()
	return pr.Print(idpChanged{Name: p.Name, Kind: string(p.Kind)}, func(w io.Writer) error {
		if _, err := fmt.Fprintf(w, "%s %s\n", verb, p.Name); err != nil {
			return err
		}
		if len(p.Mappings) == 0 {
			_, err := fmt.Fprintf(pr.ErrOut(),
				"No group is mapped to a scope yet, so nobody signs in through it: `weg idp set %q --map GROUP=SCOPE`.\n",
				p.Name)
			return err
		}
		return nil
	})
}

func newIDPRemoveCommand(opts *options, f *clientFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:               "remove NAME",
		Aliases:           []string{"rm", "delete"},
		Short:             "Remove a provider",
		Long:              "Remove a provider. Nobody signs in through it afterwards.",
		Args:              usageArgs(cobra.ExactArgs(1)),
		Example:           "  weg idp remove Authentik\n  weg idp remove FreeIPA --yes",
		ValidArgsFunction: completeIDPs(f),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := f.client()
			if err != nil {
				return err
			}
			p, err := pickIDP(c.Context(), client, args[0])
			if err != nil {
				return err
			}
			if !yes {
				if cerr := confirm(opts, fmt.Sprintf("remove %s, so that nobody signs in through it", p.Name)); cerr != nil {
					return cerr
				}
			}
			resp, err := client.DeleteIdentityProviderWithResponse(c.Context(), p.Id)
			if err != nil {
				return err
			}
			if resp.StatusCode() != http.StatusNoContent {
				return apiError(resp.StatusCode(), resp.Body)
			}
			return opts.Printer().Print(idpChanged{Name: p.Name, Kind: string(p.Kind)}, func(w io.Writer) error {
				_, werr := fmt.Fprintf(w, "removed %s\n", p.Name)
				return werr
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "remove without asking")
	return cmd
}

func fetchIDPs(ctx context.Context, client *gen.ClientWithResponses) ([]gen.IdentityProvider, error) {
	resp, err := client.ListIdentityProvidersWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	return *resp.JSON200, nil
}

// pickIDP resolves the name a command was given, ignoring case as the server
// does when it keeps two from sharing one.
func pickIDP(ctx context.Context, client *gen.ClientWithResponses, name string) (*gen.IdentityProvider, error) {
	list, err := fetchIDPs(ctx, client)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for i := range list {
		if strings.EqualFold(list[i].Name, name) {
			return &list[i], nil
		}
		names = append(names, list[i].Name)
	}
	if meant := suggest.DidYouMean(name, names); meant != "" {
		return nil, fmt.Errorf("no identity provider named %s%s", name, meant)
	}
	return nil, fmt.Errorf("no identity provider named %s; `weg idp list` shows the ones there are", name)
}

// valueOf is what p points at, or the zero value.
func valueOf[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
