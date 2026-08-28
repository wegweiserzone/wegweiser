package metrics_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/wegweiserzone/wegweiser/internal/metrics"
)

// exposition is a trimmed sample of what /api/v1/metrics answers with.
const exposition = `
# HELP weg_dns_queries_total Queries answered, by transport, question type and response code.
# TYPE weg_dns_queries_total counter
weg_dns_queries_total{rcode="NOERROR",transport="udp",type="A"} 40
weg_dns_queries_total{rcode="NOERROR",transport="tcp",type="A"} 2
weg_dns_queries_total{rcode="NXDOMAIN",transport="udp",type="AAAA"} 8
# HELP weg_dns_queries_dropped_total Queries that got no response at all, by transport.
# TYPE weg_dns_queries_dropped_total counter
weg_dns_queries_dropped_total{transport="udp"} 3
# HELP weg_dns_query_duration_seconds How long answering took.
# TYPE weg_dns_query_duration_seconds histogram
weg_dns_query_duration_seconds_bucket{le="0.0001"} 30
weg_dns_query_duration_seconds_bucket{le="0.001"} 45
weg_dns_query_duration_seconds_bucket{le="0.01"} 50
weg_dns_query_duration_seconds_bucket{le="+Inf"} 50
weg_dns_query_duration_seconds_sum 0.01
weg_dns_query_duration_seconds_count 50
# HELP weg_snapshot_zones Zones in the snapshot being answered from.
# TYPE weg_snapshot_zones gauge
weg_snapshot_zones 4
# HELP weg_snapshot_records Records in the snapshot being answered from.
# TYPE weg_snapshot_records gauge
weg_snapshot_records 1200
`

func TestSummarise(t *testing.T) {
	t.Parallel()

	got, err := metrics.Summarise(strings.NewReader(exposition))
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}

	if got.Queries != 50 {
		t.Errorf("queries = %d, want every labelled counter added up", got.Queries)
	}
	if got.UDP != 48 || got.TCP != 2 {
		t.Errorf("udp/tcp = %d/%d, want the transport label split", got.UDP, got.TCP)
	}
	if got.Dropped != 3 {
		t.Errorf("dropped = %d, want 3", got.Dropped)
	}
	if got.Zones != 4 || got.Records != 1200 {
		t.Errorf("snapshot = %d zones, %d records, want 4 and 1200", got.Zones, got.Records)
	}

	// Biggest first, so the thing worth looking at is the first line printed.
	if len(got.ByRcode) != 2 || got.ByRcode[0].Label != "NOERROR" || got.ByRcode[0].Count != 42 {
		t.Errorf("by rcode = %+v, want NOERROR at 42 first", got.ByRcode)
	}

	// The histogram is cumulative on the wire and a distribution here.
	want := []metrics.Bucket{
		{Bound: 0.0001, Count: 30},
		{Bound: 0.001, Count: 15},
		{Bound: 0.01, Count: 5},
		{Bound: math.Inf(1), Count: 0},
	}
	if len(got.Latency) != len(want) {
		t.Fatalf("latency = %+v, want %d buckets", got.Latency, len(want))
	}
	for i := range want {
		if got.Latency[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, got.Latency[i], want[i])
		}
	}

	if got.WithinTarget != 0.9 {
		t.Errorf("within a millisecond = %v, want 45 of 50 (D12)", got.WithinTarget)
	}
}

// A server nobody has asked anything reports no share rather than none of them,
// which are different things and would print the same way.
func TestSummariseSaysNothingAskedYet(t *testing.T) {
	t.Parallel()

	got, err := metrics.Summarise(strings.NewReader("# nothing here\n"))
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if got.Queries != 0 {
		t.Errorf("queries = %d, want 0", got.Queries)
	}
	if got.WithinTarget >= 0 {
		t.Errorf("within target = %v, want a negative marker for 'nothing asked yet'",
			got.WithinTarget)
	}
}

// A summary has to survive --output json. The last bucket has no upper bound,
// JSON has no infinity, and encoding/json refuses rather than guessing: without
// the marshaller this fails for every server that has answered anything.
func TestSummaryEncodesAsJSON(t *testing.T) {
	t.Parallel()

	got, err := metrics.Summarise(strings.NewReader(exposition))
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var back struct {
		Latency []struct {
			Bound *float64 `json:"bound"`
			Count uint64   `json:"count"`
		} `json:"latency"`
	}
	if uerr := json.Unmarshal(raw, &back); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if len(back.Latency) == 0 {
		t.Fatal("no latency buckets survived the round trip")
	}
	if last := back.Latency[len(back.Latency)-1]; last.Bound != nil {
		t.Errorf("the last bucket's bound is %v, want null for 'no upper bound'", *last.Bound)
	}
	for _, b := range back.Latency[:len(back.Latency)-1] {
		if b.Bound == nil {
			t.Error("a bucket that has an upper bound encoded as null")
		}
	}
}
