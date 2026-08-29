package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/cli/output"
	"github.com/wegweiserzone/wegweiser/internal/config"
)

// newConfigCommand groups what a process would start with.
func newConfigCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Aliases: []string{"conf"},
		Short:   "Show the settings a server would start with",
		Long: "The bootstrap settings, and where each of them comes from.\n\n" +
			"The file holds these and nothing else. Zones, records, tokens and the\n" +
			"reverse conflict policy live in the database and are reachable through\n" +
			"the API, because a setting that lives only in a file is a feature that\n" +
			"exists only for whoever can log in to the machine\n" +
			"(docs/decisions/ D11).",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}

	cmd.AddCommand(newConfigShowCommand(opts))
	return cmd
}

// settingShown is one resolved setting.
type settingShown struct {
	Setting string `json:"setting"`
	Value   string `json:"value"`
	Source  string `json:"source"`
}

// configShown is what a server would start with.
type configShown struct {
	// File is the configuration file that was read, empty when there was none.
	File     string         `json:"file,omitempty"`
	Settings []settingShown `json:"settings"`
}

func newConfigShowCommand(opts *options) *cobra.Command {
	var f serveFlags

	cmd := &cobra.Command{
		Use:     "show",
		Aliases: []string{"get", "print"},
		Short:   "Print the settings and where each came from",
		Long: "Print the settings a server started with these arguments would use,\n" +
			"and which of the four sources each value came from: a flag, an\n" +
			"environment variable, the file, or the built-in default.\n\n" +
			"Every flag `weg serve` takes is accepted here, so the question \"why\n" +
			"is it listening there\" can be asked without starting anything.",
		Args: usageArgs(cobra.NoArgs),
		Example: "  weg config show\n" +
			"  weg config show --config ./wegweiser.yaml\n" +
			"  weg config show --listen 127.0.0.1:5353 --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := config.Load(f.config, f.asFlags(c))
			if err != nil {
				return err
			}
			return runConfigShow(opts, cfg)
		},
	}

	registerServeFlags(cmd, &f)
	return cmd
}

func runConfigShow(opts *options, cfg *config.Config) error {
	shown := configShown{
		File: cfg.Path,
		Settings: []settingShown{
			{"dns.listen", cfg.DNSListen.Value, string(cfg.DNSListen.Source)},
			{"dns.udpResponseSize", fmt.Sprint(cfg.UDPResponseSize.Value), string(cfg.UDPResponseSize.Source)},
			{"dns.maxTCPClients", fmt.Sprint(cfg.MaxTCPClients.Value), string(cfg.MaxTCPClients.Source)},
			{"dns.maxTransfers", fmt.Sprint(cfg.MaxTransfers.Value), string(cfg.MaxTransfers.Source)},
			{"api.listen", cfg.APIListen.Value, string(cfg.APIListen.Source)},
			{"api.ui", yesNo(cfg.APIUI.Value), string(cfg.APIUI.Source)},
			{"database.path", cfg.Database.Value, string(cfg.Database.Source)},
			{"log.level", cfg.LogLevel.Value, string(cfg.LogLevel.Source)},
		},
	}

	p := opts.Printer()
	return p.Print(shown, func(w io.Writer) error {
		if shown.File == "" {
			if _, err := fmt.Fprintf(w,
				"no configuration file; %s would be read if it existed\n\n",
				config.DefaultPath); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(w, "%s\n\n", shown.File); err != nil {
			return err
		}

		t := newTable(w, "SETTING", "VALUE", "FROM")
		for _, s := range shown.Settings {
			// A value nobody chose is dimmed, so what somebody did choose is
			// what the eye lands on.
			colour := output.ColorNone
			if s.Source == string(config.FromDefault) {
				colour = output.ColorDim
			}
			t.row(s.Setting, s.Value, p.Paint(colour, s.Source))
		}
		if err := t.flush(); err != nil {
			return err
		}

		_, err := fmt.Fprintf(w,
			"\na flag beats an environment variable, which beats the file, "+
				"which beats the default.\nthe variables are %s.\n",
			strings.Join(configEnvNames, ", "))
		return err
	})
}

// configEnvNames are the environment variables the loader reads, for the note
// under the table. They are listed rather than derived, because the loader
// takes them one at a time and a list built from nothing is a list that goes
// stale silently; this one goes stale next to the code that would change it.
var configEnvNames = []string{
	config.PathEnv, "WEG_LISTEN", "WEG_API_LISTEN", "WEG_API_UI", "WEG_DATABASE",
	"WEG_UDP_RESPONSE_SIZE", "WEG_MAX_TCP_CLIENTS", "WEG_MAX_TRANSFERS", "WEG_LOG_LEVEL",
}

// yesNo prints a switch the way the file spells it, so that what the table
// shows can be pasted back into the file unchanged.
func yesNo(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
