// Package cli assembles the weg command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// Exit codes returned by [Execute]. They follow the convention that 1 is a
// runtime failure and 2 is a usage error, so scripts can tell "the server said
// no" apart from "you typed it wrong".
const (
	// ExitOK indicates success.
	ExitOK = 0
	// ExitError indicates the command ran but failed.
	ExitError = 1
	// ExitUsage indicates the command line itself was invalid.
	ExitUsage = 2
	// ExitCancelled indicates the command was interrupted, following the shell
	// convention of 128 plus the signal number for SIGINT.
	ExitCancelled = 130
)

// options holds the settings shared by every command. It is threaded through
// the command tree explicitly rather than kept in package state, so that two
// tests can run concurrently without interfering.
type options struct {
	format output.Format
	stdout io.Writer
	stderr io.Writer
	// stdin is where a command reads a file from when it is handed one on a
	// pipe. Nil takes the process's own, which is what the binary does; a test
	// supplies its own so that reading from a pipe is testable.
	stdin   io.Reader
	printer *output.Printer
}

// Printer returns the configured output printer. It is only valid after flag
// parsing, so commands must call it from RunE and not from init.
func (o *options) Printer() *output.Printer {
	if o.printer == nil {
		o.printer = output.New(o.stdout, o.stderr, o.format)
	}
	return o.printer
}

// usageError marks an error as a command-line mistake rather than a runtime
// failure, so [Execute] can show usage and return [ExitUsage].
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// usageArgs wraps a positional-argument validator so that a rejected command
// line is reported as a usage error. Cobra's validators return plain errors,
// which would otherwise be indistinguishable from a failed API call.
func usageArgs(fn cobra.PositionalArgs) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if err := fn(c, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// Execute builds the command tree, runs it against args, and returns the
// process exit code. args excludes the program name.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts := &options{stdout: stdout, stderr: stderr}
	root := newRootCommand(opts)

	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetContext(ctx)

	// ExecuteC, rather than Execute, so that usage can be printed for the
	// command that actually failed instead of always for the root.
	failed, err := root.ExecuteC()
	switch {
	case err == nil:
		return ExitOK

	case errors.Is(err, context.Canceled):
		// Interrupted by a signal. 130 is the shell convention for SIGINT and
		// is what a script checking $? expects after Ctrl-C.
		fmt.Fprintln(stderr, "weg: cancelled")
		return ExitCancelled

	default:
		fmt.Fprintf(stderr, "weg: %v\n", err)

		var ue usageError
		if errors.As(err, &ue) {
			if failed == nil {
				failed = root
			}
			fmt.Fprintln(stderr)
			fmt.Fprint(stderr, failed.UsageString())
			return ExitUsage
		}
		return ExitError
	}
}

// newRootCommand constructs the top-level weg command and attaches every
// subcommand.
func newRootCommand(opts *options) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "weg",
		Short: "Wegweiser DNS server and client",
		Long: "weg operates a Wegweiser DNS server: it runs the server, manages\n" +
			"zones and records, and inspects a running instance.\n\n" +
			"Every command supports --output json and --output yaml, so anything\n" +
			"you can read you can also script.",

		// Cobra would otherwise print a full usage dump after every runtime
		// error, burying the actual message. Usage is printed deliberately by
		// Execute, and only for errors that are about the command line.
		SilenceUsage:  true,
		SilenceErrors: true,

		// An unknown subcommand lands here as a positional argument.
		Args: usageArgs(cobra.NoArgs),

		// Resolve global flags once, before any subcommand runs.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			f, err := output.ParseFormat(format)
			if err != nil {
				return usageError{err}
			}
			opts.format = f
			opts.printer = nil // rebuild with the resolved format
			return nil
		},

		// With no subcommand, show help rather than succeeding silently.
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}

	// Malformed flags are a usage error, not a runtime failure. Set on the
	// root, which Cobra propagates to every subcommand.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageError{err}
	})

	cmd.PersistentFlags().StringVarP(&format, "output", "o", string(output.FormatText),
		"output format: "+joinFormats())

	// Registration can only fail if the flag name does not exist, which is a
	// wiring bug rather than a runtime condition. Failing loudly at startup
	// beats shipping a binary with silently broken completion.
	if err := cmd.RegisterFlagCompletionFunc("output",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return output.Formats(), cobra.ShellCompDirectiveNoFileComp
		}); err != nil {
		panic(fmt.Sprintf("cli: register completion for --output: %v", err))
	}

	cmd.AddCommand(newZoneCommand(opts))
	cmd.AddCommand(newRecordCommand(opts))
	cmd.AddCommand(newHistoryCommand(opts))
	cmd.AddCommand(newTokenCommand(opts))
	cmd.AddCommand(newQueryCommand(opts))
	cmd.AddCommand(newServeCommand(opts))
	cmd.AddCommand(newConfigCommand(opts))
	cmd.AddCommand(newSettingsCommand(opts))
	cmd.AddCommand(newStatusCommand(opts))
	cmd.AddCommand(newHealthCommand(opts))
	cmd.AddCommand(newVersionCommand(opts))

	return cmd
}

// joinFormats renders the supported formats for flag help.
func joinFormats() string {
	fs := output.Formats()
	s := ""
	for i, f := range fs {
		if i > 0 {
			s += ", "
		}
		s += f
	}
	return s
}

// Main is the entry point used by cmd/weg. It wires the real standard streams
// and returns the exit code for the caller to pass to os.Exit.
func Main(ctx context.Context) int {
	return Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
