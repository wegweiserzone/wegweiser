package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/cli/output"
)

// healthTimeout bounds the check. A health check that hangs is a health check
// that a supervisor cannot act on, and a server this slow to answer one
// request is not healthy whatever it would eventually have said.
const healthTimeout = 5 * time.Second

// serverHealth is what the check reports.
type serverHealth struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Zones   int    `json:"zones"`
	Records int    `json:"records"`
}

func newHealthCommand(opts *options) *cobra.Command {
	var f clientFlags

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Ask a server whether it is fit to answer",
		Long: "Ask a running server whether it is serving.\n\n" +
			"Serving means a snapshot has been published: a process that is up\n" +
			"with nothing loaded answers REFUSED for zones it is supposed to hold,\n" +
			"and a supervisor told \"ready\" by such a process would send it traffic\n" +
			"it cannot answer.\n\n" +
			"It needs no token (a load balancer and a container runtime have\n" +
			"nowhere to put one) and exits non-zero when the answer is anything\n" +
			"but serving, which is what makes it usable as a probe.",
		Args: usageArgs(cobra.NoArgs),
		Example: "  weg health\n" +
			"  weg health --server http://10.0.0.5:8053\n" +
			"  weg health --output json",

		RunE: func(c *cobra.Command, _ []string) error {
			return runHealth(c.Context(), opts, &f)
		},
	}
	f.register(cmd)
	return cmd
}

func runHealth(ctx context.Context, opts *options, f *clientFlags) error {
	client, err := f.openClient(healthTimeout)
	if err != nil {
		return err
	}

	resp, err := client.GetHealthWithResponse(ctx)
	if err != nil {
		return reachable(err, f.server)
	}
	if resp.JSON200 == nil {
		// A server that is up and not ready answers 503 with a problem
		// document, which says more than a status code would.
		if resp.HTTPResponse.StatusCode == http.StatusServiceUnavailable {
			return fmt.Errorf("the server is not serving: %w",
				apiError(resp.HTTPResponse.StatusCode, resp.Body))
		}
		return apiError(resp.HTTPResponse.StatusCode, resp.Body)
	}
	h := resp.JSON200

	got := serverHealth{
		Status:  string(h.Status),
		Version: h.Version,
		Zones:   h.Zones,
		Records: h.Records,
	}
	p := opts.Printer()
	return p.Print(got, func(w io.Writer) error {
		colour := output.ColorGreen
		if got.Status != string(gen.Serving) {
			colour = output.ColorYellow
		}
		_, werr := fmt.Fprintf(w, "%s — %d zones, %d records, %s\n",
			p.Paint(colour, got.Status), got.Zones, got.Records, got.Version)
		return werr
	})
}
