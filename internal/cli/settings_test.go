package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// D50: the resolvers whose word on a client is believed are set, shown and
// taken away again from the command line, as the other lists are.
func TestSettingsResolvers(t *testing.T) {
	t.Parallel()
	srv := newServer(t)

	if out := mustRun(t, srv, "settings", "show"); !strings.Contains(out, "a client is named by     nobody") {
		t.Errorf("a fresh server does not say it believes nobody:\n%s", out)
	}

	set := mustRun(t, srv, "settings", "set", "--resolvers", "127.0.0.1,::1")
	if !strings.Contains(set, "a client is named by     127.0.0.1/32, ::1/128") {
		t.Errorf("setting the list does not read it back:\n%s", set)
	}

	var shown struct {
		Resolvers []string `json:"resolvers"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, srv, "settings", "show", "--output", "json")), &shown); err != nil {
		t.Fatalf("the JSON does not parse: %v", err)
	}
	if want := []string{"127.0.0.1/32", "::1/128"}; !slices.Equal(shown.Resolvers, want) {
		t.Errorf("the JSON holds %v, want %v", shown.Resolvers, want)
	}

	code, _, errOut := run(t, srv, "settings", "set", "--resolvers", "resolver.example.com.")
	if code == ExitOK || !strings.Contains(errOut, "not an address or a CIDR prefix") {
		t.Errorf("a name was accepted as a resolver: code %d, stderr: %s", code, errOut)
	}

	if out := mustRun(t, srv, "settings", "set", "--resolvers", ""); !strings.Contains(out, "a client is named by     nobody") {
		t.Errorf("the list cannot be taken away again:\n%s", out)
	}
}
