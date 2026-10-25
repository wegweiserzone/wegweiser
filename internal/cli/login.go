package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/api/handoff"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
	"github.com/wegweiserzone/wegweiser/internal/suggest"
)

// loginWait is how long weg login waits for the browser. The server gives a
// sign-in ten minutes, and waiting longer would wait for one that can no
// longer finish.
const loginWait = 10 * time.Minute

// loginFlags are how weg login reaches a browser.
type loginFlags struct {
	noBrowser bool
	port      int
}

// loggedIn is what weg login reports.
type loggedIn struct {
	Server    string    `json:"server"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func newLoginCommand(opts *options) *cobra.Command {
	f := clientFlags{opts: opts}
	var lf loginFlags

	cmd := &cobra.Command{
		Use:     "login [PROVIDER]",
		Aliases: []string{"signin"},
		Short:   "Sign in through an identity provider",
		Long: "Sign in through one of the identity providers the server offers, and\n" +
			"keep the session for the commands that follow, for as long as a\n" +
			"browser would keep it.\n\n" +
			"A directory asks for the name and password here, without echo. An\n" +
			"OpenID Connect provider opens a browser, which comes back to this\n" +
			"command on a port of 127.0.0.1. With --no-browser the address is\n" +
			"printed instead, and with --port as well it can be reached through\n" +
			"ssh -L from a machine that has a browser.\n\n" +
			"The session is kept per server address, in a file only you can read.\n" +
			"A token in --token or $" + tokenEnv + " still wins over it.",
		Args: usageArgs(cobra.MaximumNArgs(1)),
		Example: "  weg login\n" +
			"  weg login FreeIPA\n" +
			"  weg login Authentik --server https://ns1.example.com:8053\n" +
			"  weg login Authentik --no-browser --port 8400",

		RunE: func(c *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runLogin(c.Context(), opts, &f, name, lf)
		},
	}
	f.registerServer(cmd)
	cmd.Flags().BoolVar(&lf.noBrowser, "no-browser", false,
		"print the address to sign in at rather than opening a browser")
	cmd.Flags().IntVar(&lf.port, "port", 0,
		"port of 127.0.0.1 to wait for the browser on (default any free one)")
	return cmd
}

func runLogin(ctx context.Context, opts *options, f *clientFlags, name string, lf loginFlags) error {
	if lf.port < 0 || lf.port > 65535 {
		return usageError{fmt.Errorf("--port %d is not a port", lf.port)}
	}
	server, _ := f.address()
	client, err := f.openClient(clientTimeout)
	if err != nil {
		return err
	}
	p, err := pickSignInProvider(ctx, client, name)
	if err != nil {
		return err
	}

	var got *heldSession
	switch p.Kind {
	case gen.IdentityProviderKindLdap:
		got, err = loginToDirectory(ctx, opts, client, server, p)
	case gen.IdentityProviderKindOidc:
		got, err = loginThroughOIDC(ctx, opts, server, p, lf)
	default:
		err = fmt.Errorf("%s is a kind of provider this weg cannot sign in through: %s", p.Name, p.Kind)
	}
	if err != nil {
		return err
	}

	// Asked once with the new session, so that what is kept is known to work
	// and the person is told what it holds.
	resp, err := client.GetSessionWithResponse(ctx, func(_ context.Context, r *http.Request) error {
		got.attach(r)
		return nil
	})
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	got.Name = resp.JSON200.Name

	held, path, err := opts.readSessions()
	if err != nil {
		return err
	}
	held.Servers[server] = *got
	if err := writeSessions(path, held, time.Now()); err != nil {
		return err
	}

	shown := loggedIn{Server: server, Name: got.Name, Provider: p.Name, ExpiresAt: got.Expires}
	for _, sc := range resp.JSON200.Scopes {
		shown.Scopes = append(shown.Scopes, string(sc))
	}
	pr := opts.Printer()
	return pr.Print(shown, func(w io.Writer) error {
		_, werr := fmt.Fprintf(w, "signed in at %s as %s, with %s, until %s\n",
			shown.Server, shown.Name, strings.Join(shown.Scopes, ","),
			shown.ExpiresAt.Local().Format(time.DateTime))
		return werr
	})
}

// pickSignInProvider is the provider by this name or identifier, or the only
// one there is when no name is given.
func pickSignInProvider(ctx context.Context, client *gen.ClientWithResponses, name string) (*gen.SignInProvider, error) {
	resp, err := client.ListSignInProvidersWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	list := *resp.JSON200
	if len(list) == 0 {
		return nil, errors.New("this server offers no identity provider to sign in through; " +
			"an administrator adds one with weg idp add, and a token works meanwhile")
	}
	names := make([]string, 0, len(list))
	for i := range list {
		names = append(names, list[i].Name)
	}
	if name == "" {
		if len(list) == 1 {
			return &list[0], nil
		}
		return nil, usageError{fmt.Errorf("this server offers %s; name the one to sign in through",
			strings.Join(names, ", "))}
	}
	for i := range list {
		if strings.EqualFold(list[i].Name, name) || list[i].Id == name {
			return &list[i], nil
		}
	}
	if meant := suggest.DidYouMean(name, names); meant != "" {
		return nil, fmt.Errorf("no identity provider named %s%s", name, meant)
	}
	return nil, fmt.Errorf("no identity provider named %s; this server offers %s", name, strings.Join(names, ", "))
}

// loginToDirectory asks for a name and a password and signs in with them.
func loginToDirectory(
	ctx context.Context, opts *options, client *gen.ClientWithResponses, server string, p *gen.SignInProvider,
) (*heldSession, error) {
	// The server refuses this too, but only once the password has arrived,
	// which is too late (D52).
	if err := checkPasswordTransport(server); err != nil {
		return nil, err
	}
	in := newPrompter(opts)
	user, err := in.line(ctx, "Username at "+p.Name+": ")
	if err != nil {
		return nil, err
	}
	pass, err := in.secret(ctx, "Password: ")
	if err != nil {
		return nil, err
	}
	if user == "" || pass == "" {
		return nil, usageError{errors.New("a directory signs in with a username and a password, and one was empty")}
	}

	resp, err := client.CreateDirectorySessionWithResponse(ctx, p.Id,
		gen.CreateDirectorySessionJSONRequestBody{Username: user, Password: pass})
	if err != nil {
		return nil, err
	}
	if resp.JSON201 == nil || resp.JSON201.ExpiresAt == nil {
		return nil, apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	got := &heldSession{Expires: *resp.JSON201.ExpiresAt}
	for _, c := range resp.HTTPResponse.Cookies() {
		switch c.Name {
		case sessionCookie:
			got.Value = c.Value
		case csrfCookie:
			got.CSRF = c.Value
		}
	}
	if got.Value == "" || got.CSRF == "" {
		return nil, errors.New("the server signed you in and handed over no session")
	}
	return got, nil
}

// checkPasswordTransport refuses to send a password over plain HTTP to
// another machine.
func checkPasswordTransport(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return usageError{fmt.Errorf("%s is not an address: %w", server, err)}
	}
	if u.Scheme == "https" || u.Hostname() == "localhost" {
		return nil
	}
	if addr, perr := netip.ParseAddr(u.Hostname()); perr == nil && addr.IsLoopback() {
		return nil
	}
	return fmt.Errorf("a password would cross the network in the clear to %s; "+
		"reach the server over https://, or use a token", u.Host)
}

// loginThroughOIDC sends a browser to the provider by way of the server, and
// waits on 127.0.0.1 for the session the server seals to this sign-in's key.
func loginThroughOIDC(
	ctx context.Context, opts *options, server string, p *gen.SignInProvider, lf loginFlags,
) (*heldSession, error) {
	key, err := handoff.NewKey()
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(lf.port)))
	if err != nil {
		return nil, fmt.Errorf("cannot wait for the browser on 127.0.0.1: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, fmt.Errorf("cannot wait for the browser: listening on %s", ln.Addr())
	}

	results := make(chan loopbackResult, 1)
	srv := &http.Server{Handler: loopbackHandler(results), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // it ends when the Shutdown below ends it
	defer func() {
		// Long enough for the page to reach the browser that brought the
		// session, and no longer.
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		srv.Shutdown(done) //nolint:errcheck // the answer is in hand; a browser slow to leave changes nothing
	}()

	start := server + basePath + "/auth/oidc/" + url.PathEscape(p.Id) + "/start?" +
		url.Values{"loopback": {strconv.Itoa(addr.Port)}, "key": {key.Public()}}.Encode()
	switch {
	case lf.noBrowser:
		fmt.Fprintf(opts.stderr, "Open this address in a browser to sign in through %s:\n\n  %s\n\n", p.Name, start)
	default:
		if berr := opts.browse(ctx, start); berr != nil {
			fmt.Fprintf(opts.stderr, "No browser could be opened (%v). Open this address in one:\n\n  %s\n\n", berr, start)
		} else {
			fmt.Fprintf(opts.stderr, "A browser opened to sign in through %s. If none did, open:\n\n  %s\n\n", p.Name, start)
		}
	}
	fmt.Fprintln(opts.stderr, "Waiting for the browser…")

	wait, cancel := context.WithTimeout(ctx, loginWait)
	defer cancel()
	select {
	case <-wait.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("no sign-in came back within ten minutes; run weg login again")
	case r := <-results:
		if r.err != "" {
			return nil, errors.New(r.err)
		}
		s, err := key.Open(r.session)
		if err != nil {
			return nil, err
		}
		return &heldSession{Value: s.Value, CSRF: s.CSRF, Expires: s.Expires}, nil
	}
}

// loopbackResult is what the server sent the browser back with: a sealed
// session, or why there is none.
type loopbackResult struct {
	session string
	err     string
}

// loopbackHandler takes the browser back from the server, once.
func loopbackHandler(results chan<- loopbackResult) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/" || (!q.Has("session") && !q.Has("error")) {
			http.NotFound(w, r)
			return
		}
		res := loopbackResult{session: q.Get("session"), err: q.Get("error")}
		heading, detail := "Back to the terminal", "weg has what it needs. This tab can be closed."
		if res.err != "" {
			heading, detail = "Not signed in", res.err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, loopbackPage, html.EscapeString(heading), html.EscapeString(heading), html.EscapeString(detail))
		once.Do(func() { results <- res })
	})
}

// loopbackPage is what the browser shows once it has handed weg the answer.
const loopbackPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>weg login: %s</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root { color-scheme: dark light; font-family: system-ui, sans-serif; }
body { display: grid; place-items: center; min-height: 100vh; margin: 0; }
main { max-width: 32rem; padding: 0 1rem; }
h1 { font-size: 1.25rem; }
</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>
`

// browse sends a browser to an address: $BROWSER if it names one, else
// xdg-open.
func (o *options) browse(ctx context.Context, address string) error {
	if o.openBrowser != nil {
		return o.openBrowser(ctx, address)
	}
	name := "xdg-open"
	if b := os.Getenv("BROWSER"); b != "" {
		name = b
	}
	// Not tied to the command's context: a browser started here outlives weg,
	// and stopping weg must not close it.
	cmd := exec.Command(name, address) //nolint:gosec,noctx // G204: the browser the person named, given an address weg built
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // reaped, and how the browser exits is its own business
	return nil
}

// prompter asks the person in the terminal.
type prompter struct {
	stderr io.Writer
	in     *bufio.Reader
	// tty is standard input when it is a terminal, whose echo is switched
	// off while a password is typed.
	tty *os.File
}

func newPrompter(opts *options) *prompter {
	p := &prompter{stderr: opts.stderr}
	in := opts.stdin
	if in == nil {
		in = os.Stdin
		if output.IsTerminal(os.Stdin) {
			p.tty = os.Stdin
		}
	}
	p.in = bufio.NewReader(in)
	return p
}

// line asks for something that may be seen.
func (p *prompter) line(ctx context.Context, prompt string) (string, error) {
	fmt.Fprint(p.stderr, prompt)
	return p.read(ctx)
}

// secret asks for something that may not: typed into a terminal, it is not
// echoed.
func (p *prompter) secret(ctx context.Context, prompt string) (string, error) {
	fmt.Fprint(p.stderr, prompt)
	if p.tty != nil {
		restore, err := noEcho(p.tty)
		if err != nil {
			return "", err
		}
		defer func() {
			restore()
			// The newline the person typed was not echoed either.
			fmt.Fprintln(p.stderr)
		}()
	}
	return p.read(ctx)
}

// read reads one line. An interrupt ends the wait for it: the read itself
// cannot be interrupted, and a terminal left without echo by a prompt that
// never returned is a broken terminal.
func (p *prompter) read(ctx context.Context) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := p.in.ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		line := strings.TrimRight(r.line, "\r\n")
		if r.err != nil && (!errors.Is(r.err, io.EOF) || line == "") {
			return "", fmt.Errorf("nothing was typed: %w", r.err)
		}
		return line, nil
	}
}

// noEcho switches a terminal's echo off, and returns how to switch it back.
func noEcho(f *os.File) (func(), error) {
	fd := int(f.Fd())
	was, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("switch the terminal's echo off: %w", err)
	}
	quiet := *was
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return nil, fmt.Errorf("switch the terminal's echo off: %w", err)
	}
	return func() {
		unix.IoctlSetTermios(fd, unix.TCSETS, was) //nolint:errcheck // a terminal that will not take it back leaves nothing better to do
	}, nil
}

// loggedOut is what weg logout reports.
type loggedOut struct {
	Server string `json:"server"`
	Name   string `json:"name,omitempty"`
	// Ended is whether the server was reached to end the session. Where it
	// was not, the session is forgotten here and stays good there until it
	// runs out.
	Ended bool `json:"ended"`
}

func newLogoutCommand(opts *options) *cobra.Command {
	f := clientFlags{opts: opts}

	cmd := &cobra.Command{
		Use:     "logout",
		Aliases: []string{"signout"},
		Short:   "End the session weg login opened",
		Long: "End the session weg login opened at this server, and forget it here.\n\n" +
			"The server records it as ended on every member while the cluster takes\n" +
			"writes, as signing out of the interface does. Where the server cannot\n" +
			"be reached, the session is forgotten here all the same.",
		Args: usageArgs(cobra.NoArgs),
		Example: "  weg logout\n" +
			"  weg logout --server https://ns1.example.com:8053",

		RunE: func(c *cobra.Command, _ []string) error {
			return runLogout(c.Context(), opts, &f)
		},
	}
	f.registerServer(cmd)
	return cmd
}

func runLogout(ctx context.Context, opts *options, f *clientFlags) error {
	server, _ := f.address()
	held, path, err := opts.readSessions()
	if err != nil {
		return err
	}
	h, ok := held.Servers[server]
	shown := loggedOut{Server: server, Name: h.Name}
	if ok && time.Now().Before(h.Expires) {
		client, cerr := f.openClient(clientTimeout)
		if cerr != nil {
			return cerr
		}
		resp, derr := client.DeleteSessionWithResponse(ctx, func(_ context.Context, r *http.Request) error {
			h.attach(r)
			return nil
		})
		if errors.Is(derr, context.Canceled) {
			return derr
		}
		shown.Ended = derr == nil && resp.StatusCode() == http.StatusNoContent
	}
	if ok {
		delete(held.Servers, server)
		if err := writeSessions(path, held, time.Now()); err != nil {
			return err
		}
	}

	pr := opts.Printer()
	return pr.Print(shown, func(w io.Writer) error {
		var werr error
		switch {
		case !ok:
			_, werr = fmt.Fprintf(w, "not signed in at %s\n", server)
		case shown.Ended:
			_, werr = fmt.Fprintf(w, "signed %s out at %s\n", shown.Name, server)
		default:
			_, werr = fmt.Fprintf(w, "forgot the session of %s here; %s could not be told, "+
				"and accepts it until %s\n", shown.Name, server, h.Expires.Local().Format(time.DateTime))
		}
		return werr
	})
}
