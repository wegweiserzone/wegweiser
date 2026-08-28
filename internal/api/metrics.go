package api

import (
	"bytes"
	"context"
	"net/http"
	"strconv"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/metrics"
)

// GetMetrics writes the current values in the Prometheus text exposition
// format.
func (s *Server) GetMetrics(
	_ context.Context, _ gen.GetMetricsRequestObject,
) (gen.GetMetricsResponseObject, error) {
	// Gathered into a buffer rather than streamed, so that a collector which
	// fails becomes a problem response instead of a truncated body behind a
	// 200 that has already been sent. The whole exposition is tens of
	// kilobytes.
	var buf bytes.Buffer
	if _, err := s.metrics.WriteTo(&buf); err != nil {
		return nil, internal(err)
	}
	return exposition(buf.Bytes()), nil
}

// exposition is the response the generated one cannot be: a scraper reads which
// version of the format it is looking at out of the Content-Type, and OpenAPI
// has nowhere to write a media type's parameters.
type exposition []byte

func (e exposition) VisitGetMetricsResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", metrics.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(e)))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(e)
	return err
}
