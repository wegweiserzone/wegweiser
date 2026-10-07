package cli

import (
	"strings"
	"testing"
)

// A value with a slip in it is refused with the one it was probably meant to
// be, whether the list of values is fixed or is what the server holds.
func TestRefusalsSuggest(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com")
	mustRun(t, srv, "token", "create", "deploy", "--scope", "write")
	mustRun(t, srv, "tsig", "create", "ns2.example.com.")

	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"zone", "list", "--kind", "froward"}, ExitUsage, "did you mean forward?"},
		{[]string{"zone", "list", "--output", "jsno"}, ExitUsage, "did you mean json?"},
		{[]string{"zone", "update", "example.com", "--auto-reverse", "of"}, ExitUsage, "did you mean on?"},
		{[]string{"settings", "set", "--reverse-conflict-policy", "lastwins"}, ExitUsage, "did you mean last-wins?"},
		{[]string{"token", "create", "x", "--scope", "wirte"}, ExitUsage, "did you mean write?"},
		{[]string{"zone", "show", "exmaple.com"}, ExitError, "did you mean example.com.?"},
		{[]string{"token", "revoke", "deplyo", "--yes"}, ExitError, "did you mean deploy?"},
		{[]string{"tsig", "show", "ns2.exmaple.com."}, ExitError, "did you mean ns2.example.com.?"},
		{[]string{"record", "add", "example.com", "x", "AAA", "192.0.2.1"}, ExitError, `"AAA" is not a record type; did you mean`},
		{[]string{"zone", "show", "elsewhere.test"}, ExitError, `no zone named "elsewhere.test." on this server`},
	}
	for _, tt := range tests {
		code, _, errOut := run(t, srv, tt.args...)
		if code != tt.code || !strings.Contains(errOut, tt.want) {
			t.Errorf("weg %s: exit %d, stderr %q; want %d and %q",
				strings.Join(tt.args, " "), code, errOut, tt.code, tt.want)
		}
	}
}
