// Package config assembles the settings a Wegweiser process starts with.
//
// It holds bootstrap settings and nothing else: listen addresses, where the
// database is, and how loudly to log. Everything an operator manages (zones,
// records, tokens, the reverse conflict policy) lives in the database and is
// reachable through the API, because architecture invariant 1 says no feature
// exists in only one client, and a setting that lives only in a file is a
// feature that exists only for whoever can log in to the machine
// (docs/decisions.md D11).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// DefaultPath is where the file lives when nothing says otherwise. It matches
// the packaging: the unit and the container both put it there.
const DefaultPath = "/etc/wegweiser/config.yaml"

// PathEnv names the environment variable that moves the file.
const PathEnv = "WEG_CONFIG"

// Source is where a setting's value came from.
type Source string

// The four places a setting can come from, strongest first.
const (
	FromFlag        Source = "flag"
	FromEnvironment Source = "environment"
	FromFile        Source = "file"
	FromDefault     Source = "default"
)

// File is the document on disk.
type File struct {
	DNS      DNSFile      `json:"dns,omitempty"`
	API      APIFile      `json:"api,omitempty"`
	Database DatabaseFile `json:"database,omitempty"`
	Log      LogFile      `json:"log,omitempty"`
}

// DNSFile is what the query path takes.
type DNSFile struct {
	Listen          *string `json:"listen,omitempty"`
	UDPResponseSize *uint16 `json:"udpResponseSize,omitempty"`
	MaxTCPClients   *int    `json:"maxTCPClients,omitempty"`
}

// APIFile is what the control plane takes.
type APIFile struct {
	Listen *string `json:"listen,omitempty"`
	UI     *bool   `json:"ui,omitempty"`
}

// DatabaseFile is where the store lives.
type DatabaseFile struct {
	Path *string `json:"path,omitempty"`
}

// LogFile is how loudly to report.
type LogFile struct {
	Level *string `json:"level,omitempty"`
}

// Value is one setting, and where it came from.
type Value[T any] struct {
	Value  T
	Source Source
}

// Config is the settings a process starts with.
type Config struct {
	// Path is the file that was read, empty when there was none.
	Path string

	DNSListen       Value[string]
	APIListen       Value[string]
	APIUI           Value[bool]
	Database        Value[string]
	UDPResponseSize Value[uint16]
	MaxTCPClients   Value[int]
	LogLevel        Value[string]
}

// Defaults are what a process runs with when nothing says otherwise.
var Defaults = struct {
	DNSListen       string
	APIListen       string
	APIUI           bool
	Database        string
	UDPResponseSize uint16
	MaxTCPClients   int
	LogLevel        string
}{
	// The port RFC 1035 §4.2 assigns, on every address the host has. Reaching
	// it without root is what CAP_NET_BIND_SERVICE is for (invariant 7).
	DNSListen: ":53",
	// Loopback, because the API can change every zone this server answers
	// for: exposing it to a network is a decision an operator makes on
	// purpose rather than one they inherit from a default.
	APIListen: "127.0.0.1:8053",
	// The interface is served. It is what makes a zone reachable in five
	// minutes without reading a manual, and switching it off is a decision an
	// operator makes rather than one they have to undo (docs/decisions.md D16).
	APIUI: true,
	// A relative path, so that trying the server out does not require a
	// writable directory under /var. The unit passes an absolute one.
	Database:        "wegweiser.db",
	UDPResponseSize: 1232,
	// Zero means the query path's own default rather than no bound: what "no
	// bound at all" looks like is a negative number, said on purpose.
	MaxTCPClients: 0,
	LogLevel:      "info",
}

// LogLevels are the levels [Config.LogLevel] accepts.
var LogLevels = []string{"debug", "info", "warn", "error"}

// Flags are the settings a command line supplied. A nil field is one the
// command line did not mention.
type Flags struct {
	DNSListen       *string
	APIListen       *string
	APIUI           *bool
	Database        *string
	UDPResponseSize *uint16
	MaxTCPClients   *int
	LogLevel        *string
}

// Load reads the file and lays the environment and the flags over it.
func Load(path string, flags Flags) (*Config, error) {
	explicit := path != ""
	if !explicit {
		path = os.Getenv(PathEnv)
		explicit = path != ""
	}
	if !explicit {
		path = DefaultPath
	}

	file, read, err := readFile(path, explicit)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	if read {
		cfg.Path = path
	}

	cfg.DNSListen = resolve(flags.DNSListen, "WEG_LISTEN", file.DNS.Listen,
		Defaults.DNSListen, parseString)
	cfg.APIListen = resolve(flags.APIListen, "WEG_API_LISTEN", file.API.Listen,
		Defaults.APIListen, parseString)
	cfg.Database = resolve(flags.Database, "WEG_DATABASE", file.Database.Path,
		Defaults.Database, parseString)
	cfg.LogLevel = resolve(flags.LogLevel, "WEG_LOG_LEVEL", file.Log.Level,
		Defaults.LogLevel, parseString)

	var errs []error
	cfg.APIUI, err = resolveErr(flags.APIUI, "WEG_API_UI", file.API.UI,
		Defaults.APIUI, parseBool)
	errs = append(errs, err)
	cfg.UDPResponseSize, err = resolveErr(flags.UDPResponseSize, "WEG_UDP_RESPONSE_SIZE",
		file.DNS.UDPResponseSize, Defaults.UDPResponseSize, parseUint16)
	errs = append(errs, err)
	cfg.MaxTCPClients, err = resolveErr(flags.MaxTCPClients, "WEG_MAX_TCP_CLIENTS",
		file.DNS.MaxTCPClients, Defaults.MaxTCPClients, parseInt)
	errs = append(errs, err)

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readFile reads the document, and reports whether there was one.
func readFile(path string, explicit bool) (File, bool, error) {
	var file File

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// The ordinary case for a server nobody has configured.
		return file, false, nil
	case err != nil:
		return file, false, fmt.Errorf("read the configuration at %s: %w", path, err)
	}

	// Strict: a key this version does not know is a setting somebody believes
	// is in force. Silently ignoring it is how a typo becomes an outage
	// nobody can find.
	if err := yaml.UnmarshalStrict(raw, &file); err != nil {
		return file, false, fmt.Errorf("%s: %s", path, cleanError(err))
	}
	return file, true, nil
}

// cleanError rewrites what the YAML library says into something about the file
// the person is holding.
func cleanError(err error) string {
	msg := err.Error()
	for _, prefix := range []string{
		"error unmarshaling JSON: ",
		"while decoding JSON: ",
		"json: ",
		"error converting YAML to JSON: ",
		"yaml: ",
	} {
		msg = strings.ReplaceAll(msg, prefix, "")
	}

	if field, ok := quotedAfter(msg, "unknown field "); ok {
		return fmt.Sprintf("unknown setting %q; `weg config show` lists the ones there are", field)
	}

	// "cannot unmarshal X into Go struct field A.b.c of type T": the setting
	// is the path, and the type is Go's name for it, which means nothing here.
	if at := strings.Index(msg, " into Go struct field "); at >= 0 {
		what := strings.TrimPrefix(msg[:at], "cannot unmarshal ")
		rest := msg[at+len(" into Go struct field "):]
		path, _, _ := strings.Cut(rest, " of type ")
		if _, setting, found := strings.Cut(path, "."); found {
			return fmt.Sprintf("%s: cannot be %s", setting, what)
		}
	}
	return msg
}

// quotedAfter reads the quoted word following marker.
func quotedAfter(msg, marker string) (string, bool) {
	at := strings.Index(msg, marker)
	if at < 0 {
		return "", false
	}
	rest := msg[at+len(marker):]
	if !strings.HasPrefix(rest, `"`) {
		return "", false
	}
	word, _, found := strings.Cut(rest[1:], `"`)
	return word, found
}

// resolve picks the strongest source that has something to say.
func resolve[T any](
	flag *T, env string, file *T, fallback T, parse func(string) (T, error),
) Value[T] {
	v, err := resolveErr(flag, env, file, fallback, parse)
	if err != nil {
		// Only the parsers can fail, and parseString cannot.
		return Value[T]{Value: fallback, Source: FromDefault}
	}
	return v
}

// resolveErr is resolve for a setting whose environment form has to be parsed.
func resolveErr[T any](
	flag *T, env string, file *T, fallback T, parse func(string) (T, error),
) (Value[T], error) {
	if flag != nil {
		return Value[T]{Value: *flag, Source: FromFlag}, nil
	}
	if raw, ok := os.LookupEnv(env); ok && raw != "" {
		v, err := parse(raw)
		if err != nil {
			return Value[T]{}, fmt.Errorf("$%s: %w", env, err)
		}
		return Value[T]{Value: v, Source: FromEnvironment}, nil
	}
	if file != nil {
		return Value[T]{Value: *file, Source: FromFile}, nil
	}
	return Value[T]{Value: fallback, Source: FromDefault}, nil
}

func parseString(s string) (string, error) { return s, nil }

func parseUint16(s string) (uint16, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number between 0 and 65535", s)
	}
	return uint16(v), nil
}

// parseBool takes what a person would write in a shell rather than only what
// Go's own parser accepts, so that WEG_API_UI=no does what it plainly says.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%q is not yes or no", s)
}

func parseInt(s string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

// validate refuses what would start a server nobody meant.
//
// It checks the settings this package owns; a listen address is left to the
// socket, which reports what is actually wrong with it far better than a
// pattern here could.
func (c *Config) validate() error {
	if !slices.Contains(LogLevels, c.LogLevel.Value) {
		return fmt.Errorf("log level %q is not one of %s (from the %s)",
			c.LogLevel.Value, strings.Join(LogLevels, ", "), c.LogLevel.Source)
	}
	if c.Database.Value == "" {
		return fmt.Errorf("the database path is empty (from the %s)", c.Database.Source)
	}
	// Whether the path can be opened is not asked here. It is a fact about the
	// machine rather than about the file, the store already reports it and
	// says what to check, and asking would make `weg config show`, which
	// opens nothing, fail on a file that is right for the host it is meant
	// for.
	return nil
}

// LocalAPIAddress is where the API on this machine listens, as far as the
// configuration file says, and whether the file said anything.
func LocalAPIAddress() (string, bool) {
	path := os.Getenv(PathEnv)
	if path == "" {
		path = DefaultPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}

	var file File
	if uerr := yaml.Unmarshal(raw, &file); uerr != nil || file.API.Listen == nil {
		return "", false
	}

	host, port, err := net.SplitHostPort(*file.API.Listen)
	if err != nil {
		return "", false
	}
	// A listening address is not a destination. "every interface" is where the
	// socket is bound, and the one to knock on from this machine is loopback.
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), true
}
