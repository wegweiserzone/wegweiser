package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/buildinfo"
)

// newVersionCommand reports the version of this binary.
func newVersionCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long: "Print the version of this weg binary.\n\n" +
			"Use --output json when reporting a bug, so the build details come\n" +
			"along unambiguously.",
		Args:                  usageArgs(cobra.NoArgs),
		DisableFlagsInUseLine: true,
		Example: "  weg version\n" +
			"  weg version --output json",

		RunE: func(_ *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			return opts.Printer().Print(info, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, info.String())
				return err
			})
		},
	}
}
