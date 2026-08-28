package cli_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/cli"
)

// run executes the command tree and returns its exit code and streams.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Execute(t.Context(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionOutputFormats(t *testing.T) {
	t.Parallel()

	t.Run("text", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := run(t, "version")
		if code != cli.ExitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
		}
		if !strings.HasPrefix(stdout, "wegweiser ") {
			t.Errorf("stdout = %q, want it to start with %q", stdout, "wegweiser ")
		}
	})

	t.Run("json is valid and carries the expected fields", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := run(t, "version", "--output", "json")
		if code != cli.ExitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
		}

		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("output is not valid JSON: %v\ngot: %s", err, stdout)
		}
		for _, key := range []string{"version", "goVersion", "platform"} {
			if _, ok := got[key]; !ok {
				t.Errorf("JSON output missing key %q\ngot: %s", key, stdout)
			}
		}
	})

	t.Run("yaml", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := run(t, "version", "-o", "yaml")
		if code != cli.ExitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
		}
		if !strings.Contains(stdout, "version:") {
			t.Errorf("YAML output missing version key\ngot: %s", stdout)
		}
	})
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{
			name:     "unknown output format",
			args:     []string{"version", "--output", "toml"},
			wantCode: cli.ExitUsage,
			wantErr:  "unknown output format",
		},
		{
			name:     "unknown subcommand",
			args:     []string{"frobnicate"},
			wantCode: cli.ExitUsage,
			wantErr:  "unknown command",
		},
		{
			name:     "unexpected argument to a subcommand",
			args:     []string{"version", "extra"},
			wantCode: cli.ExitUsage,
			wantErr:  "unknown command",
		},
		{
			name:     "unknown flag",
			args:     []string{"version", "--nope"},
			wantCode: cli.ExitUsage,
			wantErr:  "unknown flag",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := run(t, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d (stderr: %s)", code, tc.wantCode, stderr)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
		})
	}
}

func TestBareInvocationShowsHelp(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := run(t)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}
	if !strings.Contains(stdout, "Usage:") {
		t.Errorf("bare invocation should print help, got: %s", stdout)
	}
}

// TestUsageErrorPrintsUsageOfFailingCommand guards the detail that makes a
// usage error actually helpful: the usage block shown belongs to the
// subcommand that failed, not to the root.
func TestUsageErrorPrintsUsageOfFailingCommand(t *testing.T) {
	t.Parallel()

	code, _, stderr := run(t, "version", "extra")
	if code != cli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(stderr, "Usage:\n  weg version") {
		t.Errorf("stderr should show usage for 'weg version', got:\n%s", stderr)
	}
}
