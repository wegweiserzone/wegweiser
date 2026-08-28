package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Summary is what the query path has been doing, read back out of the
// exposition format.
//
// It lives here rather than in the client that renders it because `depguard`
// keeps the Prometheus packages to this one importer (docs/decisions/ D15),
// and because the web interface summarises the same text in TypeScript: one
// vocabulary, so both clients say the same thing about the same server.
type Summary struct {
	Queries   uint64  `json:"queries"`
	ByRcode   []Count `json:"byRcode"`
	ByType    []Count `json:"byType"`
	UDP       uint64  `json:"udp"`
	TCP       uint64  `json:"tcp"`
	Dropped   uint64  `json:"dropped"`
	Truncated uint64  `json:"truncated"`

	// WithinTarget is the share of answers inside a millisecond, which is what
	// docs/decisions/ D12 asks for. Negative when nothing has been asked yet,
	// so a caller can tell "none yet" from "none of them".
	WithinTarget float64  `json:"withinTarget"`
	Latency      []Bucket `json:"latency"`

	Zones      int        `json:"zones"`
	Records    int        `json:"records"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	SnapshotAt *time.Time `json:"snapshotAt,omitempty"`
}

// Count is one labelled total, as a response code or a question type.
type Count struct {
	Label string `json:"label"`
	Count uint64 `json:"count"`
}

// Bucket is one latency bucket, already differenced out of the cumulative
// histogram so it reads as a distribution.
type Bucket struct {
	// Bound is the upper edge in seconds, and +Inf for the last one, which
	// holds everything slower than the widest bucket.
	Bound float64 `json:"bound"`
	Count uint64  `json:"count"`
}

// MarshalJSON writes the unbounded last bucket as null.
//
// JSON has no infinity and encoding/json refuses to guess, so a summary
// carrying one would fail to encode at all. Null is what a consumer can read as
// "no upper bound"; a number here would be a lie about where the bucket ends.
func (b Bucket) MarshalJSON() ([]byte, error) {
	bound := "null"
	if !math.IsInf(b.Bound, 1) {
		bound = strconv.FormatFloat(b.Bound, 'g', -1, 64)
	}
	return []byte(fmt.Sprintf(`{"bound":%s,"count":%d}`, bound, b.Count)), nil
}

// Summarise reads the exposition format and reports what it says.
func Summarise(r io.Reader) (*Summary, error) {
	// The zero TextParser is documented as invalid, and panics rather than
	// failing when it is used. UTF-8 is the library's own default and accepts
	// everything this server exports.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, fmt.Errorf("metrics: read the exposition format: %w", err)
	}

	s := &Summary{WithinTarget: -1}
	byRcode := map[string]uint64{}
	byType := map[string]uint64{}

	for _, m := range families[namespace+"_dns_queries_total"].GetMetric() {
		n := uint64(m.GetCounter().GetValue())
		s.Queries += n
		for _, l := range m.GetLabel() {
			switch l.GetName() {
			case "rcode":
				byRcode[l.GetValue()] += n
			case "type":
				byType[l.GetValue()] += n
			case "transport":
				if l.GetValue() == "tcp" {
					s.TCP += n
				} else {
					s.UDP += n
				}
			}
		}
	}
	s.ByRcode = ranked(byRcode)
	s.ByType = ranked(byType)

	s.Dropped = totalOf(families, namespace+"_dns_queries_dropped_total")
	s.Truncated = totalOf(families, namespace+"_dns_responses_truncated_total")
	s.Zones = int(gaugeOf(families, namespace+"_snapshot_zones"))
	s.Records = int(gaugeOf(families, namespace+"_snapshot_records"))
	s.StartedAt = momentOf(families, namespace+"_process_start_timestamp_seconds")
	s.SnapshotAt = momentOf(families, namespace+"_snapshot_published_timestamp_seconds")

	s.Latency, s.WithinTarget = histogram(families[namespace+"_dns_query_duration_seconds"])
	return s, nil
}

// ranked orders the labels by how often they were seen, then by name so that
// two runs of the same data print the same way.
func ranked(m map[string]uint64) []Count {
	out := make([]Count, 0, len(m))
	for label, n := range m {
		out = append(out, Count{Label: label, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// histogram differences the cumulative buckets and reports the share that fell
// inside a millisecond.
func histogram(family *dto.MetricFamily) (buckets []Bucket, withinTarget float64) {
	cumulative := map[float64]uint64{}
	var counted uint64
	for _, m := range family.GetMetric() {
		h := m.GetHistogram()
		counted += h.GetSampleCount()
		for _, b := range h.GetBucket() {
			cumulative[b.GetUpperBound()] += b.GetCumulativeCount()
		}
	}
	if counted == 0 {
		return nil, -1
	}

	bounds := make([]float64, 0, len(cumulative))
	for b := range cumulative {
		bounds = append(bounds, b)
	}
	sort.Float64s(bounds)

	out := make([]Bucket, 0, len(bounds)+1)
	var previous uint64
	for _, bound := range bounds {
		under := cumulative[bound]
		// A cumulative count that went backwards would be a broken exposition;
		// clamping keeps one bad line from producing a negative bucket.
		var n uint64
		if under > previous {
			n = under - previous
		}
		out = append(out, Bucket{Bound: bound, Count: n})
		previous = under
	}
	if counted > previous {
		out = append(out, Bucket{Bound: math.Inf(1), Count: counted - previous})
	}
	return out, float64(cumulative[0.001]) / float64(counted)
}

func totalOf(f map[string]*dto.MetricFamily, name string) uint64 {
	var n uint64
	for _, m := range f[name].GetMetric() {
		n += uint64(m.GetCounter().GetValue())
	}
	return n
}

func gaugeOf(f map[string]*dto.MetricFamily, name string) float64 {
	for _, m := range f[name].GetMetric() {
		return m.GetGauge().GetValue()
	}
	return 0
}

// momentOf reads a gauge holding seconds since the epoch. Zero means the server
// has not set it, which is not the same as the epoch.
func momentOf(f map[string]*dto.MetricFamily, name string) *time.Time {
	secs := gaugeOf(f, name)
	if secs <= 0 {
		return nil
	}
	whole, frac := math.Modf(secs)
	t := time.Unix(int64(whole), int64(frac*float64(time.Second))).UTC()
	return &t
}
