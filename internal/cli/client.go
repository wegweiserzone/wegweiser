package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/config"
)

// Where a command finds the server when nothing on the command line says.
//
// The precedence is D11's: the flag, then the environment, then the
// configuration file's own API address if there is one on this machine, then
// the default. The token is not among the things the file may hold: a
// credential in /etc is readable by whoever can read /etc, and D5 keeps it in
// the environment for the same reason it keeps it off the command line.
const (
	// serverEnv names the environment variable holding the server address.
	serverEnv = "WEG_SERVER"
	// tokenEnv names the environment variable holding the API token. A token
	// belongs in the environment rather than on the command line, where every
	// other process on the machine can read it out of the process table.
	tokenEnv = "WEG_TOKEN"

	// defaultServer is where weg serve puts the API unless told otherwise.
	defaultServer = "http://127.0.0.1:8053"
)

// clientTimeout bounds a request. Generous, because an import carries a whole
// zone and the server writes it in one transaction.
const clientTimeout = 5 * time.Minute

// clientFlags are the settings every command that talks to a server takes.
type clientFlags struct {
	server string
	token  string
}

// register adds the flags to a command. They are persistent, so a command
// group declares them once and every subcommand inherits them.
func (f *clientFlags) register(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&f.server, "server", "",
		"address of the Wegweiser API (default "+defaultServer+", or $"+serverEnv+")")
	cmd.PersistentFlags().StringVar(&f.token, "token", "",
		"API token (default $"+tokenEnv+")")
}

// client builds a client for the configured server.
func (f *clientFlags) client() (*gen.ClientWithResponses, error) {
	server, opts, err := f.dial(clientTimeout)
	if err != nil {
		return nil, err
	}
	return gen.NewClientWithResponses(server, opts...)
}

// streamClient builds a client for a request that is meant to stay open.
func (f *clientFlags) streamClient() (*gen.Client, error) {
	server, opts, err := f.dial(0)
	if err != nil {
		return nil, err
	}
	return gen.NewClient(server, opts...)
}

// openClient builds a client for the one endpoint that needs no credential.
//
// Health is what a container runtime and a load balancer ask, and neither has
// anywhere to put a token, which is why /healthz is unauthenticated in the
// first place (docs/decisions/ D5). Requiring one here would make the check
// fail for a reason unrelated to health, which is the whole thing that
// endpoint exists to avoid.
func (f *clientFlags) openClient(timeout time.Duration) (*gen.ClientWithResponses, error) {
	server, check := f.address()
	return gen.NewClientWithResponses(server+basePath,
		gen.WithHTTPClient(advising{&http.Client{Timeout: timeout}, server, check}))
}

// address is where the server is, by the precedence of D11, and what to check
// when nothing answers there: the advice depends on which source named it.
func (f *clientFlags) address() (server, check string) {
	switch env, file := os.Getenv(serverEnv), configuredServer(); {
	case f.server != "":
		server, check = f.server, "is it running, and is --server right?"
	case env != "":
		server, check = env, "is it running, and is $"+serverEnv+" right?"
	case file != "":
		server, check = file, "is it running? The address is api.listen from the configuration file."
	default:
		server, check = defaultServer, "is it running? Name another with --server or $"+serverEnv+"."
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	return strings.TrimSuffix(server, "/"), check
}

// dial works out where the server is and how to authenticate to it.
func (f *clientFlags) dial(timeout time.Duration) (string, []gen.ClientOption, error) {
	server, check := f.address()

	token := firstNonEmpty(f.token, os.Getenv(tokenEnv))
	if token == "" {
		return "", nil, usageError{fmt.Errorf(
			"no API token: pass --token, or put one in $%s. The first start of weg serve "+
				"prints one", tokenEnv)}
	}

	return server + basePath, []gen.ClientOption{
		gen.WithHTTPClient(advising{&http.Client{Timeout: timeout}, server, check}),
		gen.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		}),
	}, nil
}

// configuredServer is where the file says the API on this machine listens, or
// nothing when there is no file or it does not say.
func configuredServer() string {
	if addr, ok := config.LocalAPIAddress(); ok {
		return addr
	}
	return ""
}

// basePath is where the API is mounted. It is repeated from internal/api
// rather than imported, because a client should not need the server to know
// where to knock.
const basePath = "/api/v1"

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// apiError turns a problem document into an error a person can act on.
//
// The server answers RFC 9457, which carries a sentence saying what went wrong
// with this particular request. Reporting the status code alone would throw
// that away and leave the operator with "400".
func apiError(status int, body []byte) error {
	var p gen.Problem
	if err := json.Unmarshal(body, &p); err == nil && p.Detail != nil && *p.Detail != "" {
		return fmt.Errorf("%s: %s", strings.ToLower(p.Title), *p.Detail)
	}
	if len(body) > 0 {
		return fmt.Errorf("the server answered %d: %s", status, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("the server answered %d", status)
}

// advising is the HTTP client every command talks through. A request that
// reaches no server comes back as advice, because the dial error underneath
// says what failed and nothing about what to do.
type advising struct {
	client *http.Client
	server string
	check  string
}

// Do sends the request. An interrupt stays what it is, so that Execute still
// recognises it.
func (a advising) Do(r *http.Request) (*http.Response, error) {
	resp, err := a.client.Do(r) //nolint:gosec // G704: the server the operator named is where weg is meant to go
	if err != nil && !errors.Is(err, context.Canceled) {
		return nil, fmt.Errorf("no Wegweiser server answered at %s (%s): %s",
			a.server, why(err), a.check)
	}
	return resp, err
}

// why is the part of a transport failure a person can act on.
func why(err error) string {
	var dnsErr *net.DNSError
	var netErr net.Error
	var urlErr *url.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &dnsErr):
		return dnsErr.Err
	case errors.As(err, &netErr) && netErr.Timeout():
		return "no answer in time"
	case errors.As(err, &urlErr):
		return urlErr.Err.Error()
	}
	return err.Error()
}
