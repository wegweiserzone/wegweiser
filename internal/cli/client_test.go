package cli_test

import (
	"net"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/cli"
)

// A server that is not there is reported at the address that was tried, with
// the source of that address named, wherever the address came from.
func TestNoServerAnswered(t *testing.T) {
	// Nothing listens on a port that was just given back.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	t.Setenv("WEG_TOKEN", "weg_test")
	t.Setenv("WEG_CONFIG", t.TempDir()+"/none.yaml")

	tests := []struct {
		name  string
		env   string
		args  []string
		check string
	}{
		{"flag", "", []string{"zone", "list", "--server", addr}, "is --server right?"},
		{"environment", addr, []string{"zone", "list"}, "is $WEG_SERVER right?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WEG_SERVER", tt.env)

			code, _, stderr := run(t, tt.args...)
			if code != cli.ExitError {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitError, stderr)
			}
			want := "no Wegweiser server answered at http://" + addr + " (connection refused): "
			if !strings.Contains(stderr, want) || !strings.Contains(stderr, tt.check) {
				t.Errorf("stderr = %q, want %q and %q", stderr, want, tt.check)
			}
		})
	}
}
