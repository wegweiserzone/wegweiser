// Command weg runs and operates a Wegweiser DNS server.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/wegweiserzone/wegweiser/internal/cli"
)

func main() {
	// os.Exit is confined to this line so that run can use defer.
	os.Exit(run())
}

// run sets up signal handling and executes the command tree, returning the
// process exit code.
func run() int {
	// A first signal cancels the context so commands can shut down cleanly. A
	// second one is left to the default handler, which terminates immediately:
	// an operator pressing Ctrl-C twice means it, and a server that cannot be
	// stopped is worse than one that stops abruptly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Main(ctx)
}
