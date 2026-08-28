package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/config"
)

// write puts a configuration file in a temporary directory and returns its
// path.
func write(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	return path
}

func ptr[T any](v T) *T { return &v }

// Nothing configured at all is the case a person trying the server out hits,
// and it has to work.
func TestLoadWithNothing(t *testing.T) {
	// Not parallel: it reads the environment, and t.Setenv forbids both.
	t.Setenv("WEG_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))

	// An absent file named by the environment is still named on purpose.
	if _, err := config.Load("", config.Flags{}); err == nil {
		t.Error("a file asked for by name and not there was accepted")
	}

	os.Unsetenv("WEG_CONFIG")
	cfg, err := config.Load("", config.Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Path != "" {
		t.Errorf("path = %q, want none read", cfg.Path)
	}
	if cfg.DNSListen.Value != config.Defaults.DNSListen ||
		cfg.DNSListen.Source != config.FromDefault {
		t.Errorf("listen = %+v, want the default", cfg.DNSListen)
	}
}

func TestLoadFromFile(t *testing.T) {
	path := write(t, `
dns:
  listen: "127.0.0.1:5353"
  udpResponseSize: 4096
  maxTCPClients: 0
api:
  listen: "0.0.0.0:8053"
  ui: false
database:
  path: "wegweiser.db"
log:
  level: "debug"
`)

	cfg, err := config.Load(path, config.Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"listen", cfg.DNSListen.Value, "127.0.0.1:5353"},
		{"api listen", cfg.APIListen.Value, "0.0.0.0:8053"},
		{"api ui", cfg.APIUI.Value, false},
		{"udp size", cfg.UDPResponseSize.Value, uint16(4096)},
		{"log level", cfg.LogLevel.Value, "debug"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if cfg.DNSListen.Source != config.FromFile {
		t.Errorf("source = %q, want %q", cfg.DNSListen.Source, config.FromFile)
	}
	// A field is a pointer so that "said to be zero" and "not mentioned" are
	// different: maxTCPClients: 0 is a decision, and the default is also 0.
	if cfg.MaxTCPClients.Source != config.FromFile {
		t.Errorf("maxTCPClients came from the %s, want the file to be credited for saying 0",
			cfg.MaxTCPClients.Source)
	}
}

// D11: flags → environment → file → default, and every value remembers which.
func TestPrecedence(t *testing.T) {
	path := write(t, "dns:\n  listen: \"from-file:53\"\n")

	t.Run("the file beats the default", func(t *testing.T) {
		cfg, err := config.Load(path, config.Flags{})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.DNSListen.Value != "from-file:53" || cfg.DNSListen.Source != config.FromFile {
			t.Errorf("listen = %+v, want the file's", cfg.DNSListen)
		}
	})

	t.Run("the environment beats the file", func(t *testing.T) {
		t.Setenv("WEG_LISTEN", "from-env:53")
		cfg, err := config.Load(path, config.Flags{})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.DNSListen.Value != "from-env:53" || cfg.DNSListen.Source != config.FromEnvironment {
			t.Errorf("listen = %+v, want the environment's", cfg.DNSListen)
		}
	})

	t.Run("the flag beats the environment", func(t *testing.T) {
		t.Setenv("WEG_LISTEN", "from-env:53")
		cfg, err := config.Load(path, config.Flags{DNSListen: ptr("from-flag:53")})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.DNSListen.Value != "from-flag:53" || cfg.DNSListen.Source != config.FromFlag {
			t.Errorf("listen = %+v, want the flag's", cfg.DNSListen)
		}
	})

	t.Run("an empty environment variable is not a value", func(t *testing.T) {
		t.Setenv("WEG_LISTEN", "")
		cfg, err := config.Load(path, config.Flags{})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.DNSListen.Source != config.FromFile {
			t.Errorf("source = %q, want an empty variable to be no answer at all",
				cfg.DNSListen.Source)
		}
	})
}

// A key this version does not know is a setting somebody believes is in force.
func TestUnknownKeyIsRefused(t *testing.T) {
	t.Parallel()

	path := write(t, "dns:\n  listen: \":53\"\n  lisen: \":5353\"\n")
	_, err := config.Load(path, config.Flags{})
	if err == nil {
		t.Fatal("a misspelt key was accepted")
	}
	if !strings.Contains(err.Error(), "lisen") {
		t.Errorf("error = %v, want it to name the key", err)
	}
}

func TestRefusals(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		env   [2]string
		flags config.Flags
		want  string
	}{
		{
			name: "a log level that is not one",
			file: "log:\n  level: loud\n",
			want: "loud",
		},
		{
			name: "an empty database path",
			file: "database:\n  path: \"\"\n",
			want: "database path is empty",
		},
		{
			name: "an environment value that is not a number",
			file: "",
			env:  [2]string{"WEG_UDP_RESPONSE_SIZE", "big"},
			want: "WEG_UDP_RESPONSE_SIZE",
		},
		{
			name: "a number too large for the field",
			file: "",
			env:  [2]string{"WEG_UDP_RESPONSE_SIZE", "70000"},
			want: "65535",
		},
		{
			name: "a switch that is neither",
			file: "",
			env:  [2]string{"WEG_API_UI", "maybe"},
			want: "yes or no",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env[0] != "" {
				t.Setenv(tc.env[0], tc.env[1])
			}
			_, err := config.Load(write(t, tc.file), tc.flags)
			if err == nil {
				t.Fatalf("accepted %q", tc.file+tc.env[1])
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A refusal has to say where the value came from, or an operator goes looking
// in the file for something the environment set.
func TestRefusalNamesTheSource(t *testing.T) {
	t.Setenv("WEG_LOG_LEVEL", "loud")

	_, err := config.Load(write(t, ""), config.Flags{})
	if err == nil {
		t.Fatal("accepted a level that is not one")
	}
	if !strings.Contains(err.Error(), string(config.FromEnvironment)) {
		t.Errorf("error = %v, want it to say the environment set it", err)
	}
}

// The library converts YAML to JSON and decodes that, so its own messages are
// about JSON and about Go types: for a file with no JSON in it and a reader
// with no Go in front of them.
func TestErrorsAreAboutTheFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
		want string
		gone []string
	}{
		{
			name: "a misspelt setting",
			file: "dns:\n  lisen: \":53\"\n",
			want: `unknown setting "lisen"`,
			gone: []string{"JSON", "field"},
		},
		{
			name: "a value of the wrong shape",
			file: "dns:\n  udpResponseSize: \"big\"\n",
			want: "dns.udpResponseSize: cannot be string",
			gone: []string{"JSON", "Go struct", "uint16"},
		},
		{
			name: "a section of the wrong shape",
			file: "dns: [1, 2]\n",
			want: "dns: cannot be array",
			gone: []string{"JSON", "config.DNSFile"},
		},
		{
			name: "a file that is not YAML at all",
			file: "dns:\n  listen: \":53\"\n   nonsense\n",
			want: "line 2",
			gone: []string{"JSON", "yaml:"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := write(t, tc.file)
			_, err := config.Load(path, config.Flags{})
			if err == nil {
				t.Fatalf("accepted %q", tc.file)
			}
			// Without the path in front: it ends in .yaml, which every check
			// below would otherwise match against.
			msg := strings.TrimPrefix(err.Error(), path+": ")
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q, want it to say %q", msg, tc.want)
			}
			for _, gone := range tc.gone {
				if strings.Contains(msg, gone) {
					t.Errorf("error = %q, still mentions %q", msg, gone)
				}
			}
		})
	}
}

// A client on the server's own machine should not have to repeat a port the
// file already names.
func TestLocalAPIAddress(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
		ok   bool
	}{
		{"a plain address", "api:\n  listen: \"127.0.0.1:9000\"\n", "127.0.0.1:9000", true},
		// A listening address is not a destination: "every interface" is
		// where the socket is bound, and the one to knock on from here is
		// loopback.
		{"every interface", "api:\n  listen: \"0.0.0.0:9000\"\n", "127.0.0.1:9000", true},
		{"every interface, v6", "api:\n  listen: \"[::]:9000\"\n", "127.0.0.1:9000", true},
		{"a port on its own", "api:\n  listen: \":9000\"\n", "127.0.0.1:9000", true},
		{"a specific interface is left alone", "api:\n  listen: \"10.0.0.5:9000\"\n", "10.0.0.5:9000", true},
		{"a file that does not say", "dns:\n  listen: \":53\"\n", "", false},
		// Given up on quietly: this is a convenience for a client, and the
		// process the file is actually for refuses to start and says why.
		{"a file that is nonsense", "api: [1,2]\n", "", false},
		{"an address that is not one", "api:\n  listen: \"nonsense\"\n", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.PathEnv, write(t, tc.file))

			got, ok := config.LocalAPIAddress()
			if ok != tc.ok {
				t.Fatalf("found = %v, want %v (got %q)", ok, tc.ok, got)
			}
			if got != tc.want {
				t.Errorf("address = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLocalAPIAddressWithoutAFile(t *testing.T) {
	t.Setenv(config.PathEnv, filepath.Join(t.TempDir(), "absent.yaml"))

	if got, ok := config.LocalAPIAddress(); ok {
		t.Errorf("address = %q from a file that is not there", got)
	}
}

// The interface is served unless somebody says otherwise, and "otherwise" is
// spelled the way a person would write it in a shell rather than only the way
// Go's own parser accepts (docs/decisions/ D16).
func TestAPIUI(t *testing.T) {
	t.Run("on by default", func(t *testing.T) {
		cfg, err := config.Load(write(t, ""), config.Flags{})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !cfg.APIUI.Value || cfg.APIUI.Source != config.FromDefault {
			t.Errorf("ui = %+v, want true from the default", cfg.APIUI)
		}
	})

	t.Run("a flag beats the file", func(t *testing.T) {
		on := true
		cfg, err := config.Load(write(t, "api:\n  ui: false\n"), config.Flags{APIUI: &on})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !cfg.APIUI.Value || cfg.APIUI.Source != config.FromFlag {
			t.Errorf("ui = %+v, want true from the flag", cfg.APIUI)
		}
	})

	for _, spelling := range []string{"0", "f", "false", "n", "no", "off", "OFF", " no "} {
		t.Run("the environment says "+spelling, func(t *testing.T) {
			t.Setenv("WEG_API_UI", spelling)
			cfg, err := config.Load(write(t, ""), config.Flags{})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.APIUI.Value || cfg.APIUI.Source != config.FromEnvironment {
				t.Errorf("ui = %+v, want false from the environment", cfg.APIUI)
			}
		})
	}
}
