package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/metrics"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/store/sqlite"
	"github.com/wegweiserzone/wegweiser/internal/stream"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// snapshots is a stand-in for the data plane: it holds what the API publishes,
// so a test can check that a write reached the query path without a socket.
type snapshots struct {
	current *dns.Snapshot
}

func (s *snapshots) Snapshot() *dns.Snapshot     { return s.current }
func (s *snapshots) SetSnapshot(n *dns.Snapshot) { s.current = n }

// harness is a server, the database behind it, and a token that may use it.
type harness struct {
	t      *testing.T
	api    *Server
	http   *httptest.Server
	store  store.Store
	snaps  *snapshots
	stream *stream.Hub
	token  string
}

// newHarness brings up an API over a fresh database, with one admin token.
func newHarness(t *testing.T, tweak ...func(*Config)) *harness {
	t.Helper()

	st, err := sqlite.Open(t.Context(), sqlite.Options{
		Path: filepath.Join(t.TempDir(), "weg.db"),
	})
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("close the database: %v", cerr)
		}
	})
	if merr := st.Migrate(t.Context()); merr != nil {
		t.Fatalf("migrate: %v", merr)
	}

	applier, err := apply.New(st, apply.Options{})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}

	secret, err := EnsureBootstrapToken(t.Context(), st, time.Now())
	if err != nil {
		t.Fatalf("mint the bootstrap token: %v", err)
	}

	snaps := &snapshots{}
	var empty *dns.Snapshot
	if verr := st.View(t.Context(), func(r store.Reader) error {
		var berr error
		empty, berr = dns.Rebuild(t.Context(), r)
		return berr
	}); verr != nil {
		t.Fatalf("build the first snapshot: %v", verr)
	}
	snaps.SetSnapshot(empty)

	// A small bound, so that the refusal past it is reachable in a test
	// without opening sixteen connections. No test here opens more than two
	// watchers at once.
	hub := stream.NewHub(stream.Options{MaxWatchers: 3})
	cfg := Config{
		Store:     st,
		Applier:   applier,
		Snapshots: snaps,
		Metrics:   metrics.New(),
		Stream:    hub,
		OnError:   func(err error) { t.Errorf("the server reported a fault: %v", err) },
	}
	for _, apply := range tweak {
		apply(&cfg)
	}

	srvAPI, handler, err := New(cfg)
	if err != nil {
		t.Fatalf("build the API: %v", err)
	}
	t.Cleanup(func() {
		if cerr := srvAPI.Close(); cerr != nil {
			t.Errorf("close the API: %v", cerr)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &harness{
		t: t, api: srvAPI, http: srv,
		store: st, snaps: snaps, stream: hub, token: secret,
	}
}

// do sends a request with the harness's token unless told otherwise.
func (h *harness) do(method, path string, body any, opts ...func(*http.Request)) *http.Response {
	h.t.Helper()

	var r io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode the request body: %v", err)
		}
		r = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(h.t.Context(), method, h.http.URL+basePath+path, r)
	if err != nil {
		h.t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}

	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decode reads a response body into v, failing the test on a wrong status.
func (h *harness) decode(resp *http.Response, want int, v any) {
	h.t.Helper()

	if resp.StatusCode != want {
		body, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			h.t.Fatalf("status = %d, want %d; the body could not be read: %v",
				resp.StatusCode, want, rerr)
		}
		h.t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, want, body)
	}
	if v == nil {
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		h.t.Fatalf("decode the response: %v", err)
	}
}

// createZone adds a zone through the API and returns it.
func (h *harness) createZone(name string) gen.Zone {
	h.t.Helper()

	var z gen.Zone
	h.decode(h.do(http.MethodPost, "/zones", gen.CreateZone{Name: name}), http.StatusCreated, &z)
	return z
}

// createRecord adds a record through the API and returns what that caused.
func (h *harness) createRecord(zid string, in gen.CreateRecord) gen.RecordWritten {
	h.t.Helper()

	var out gen.RecordWritten
	h.decode(h.do(http.MethodPost, "/zones/"+zid+"/records", in), http.StatusCreated, &out)
	return out
}

// answers returns what the published snapshot answers a question with.
func (h *harness) answers(name string, typ zone.RRType) []string {
	h.t.Helper()

	var a dns.Answer
	h.snaps.Snapshot().Resolve(dns.Question{
		Name: zone.MustParseName(name), Class: zone.ClassIN, Type: typ,
	}, &a)

	out := make([]string, 0, len(a.Answer))
	for _, rr := range a.Answer {
		out = append(out, rr.String())
	}
	return out
}

func TestZoneLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	z := h.createZone("example.com.")
	if z.Name != "example.com." || z.Kind != gen.Forward {
		t.Errorf("created %+v, want a forward zone named example.com.", z)
	}
	if z.Soa.PrimaryNs != "ns1.example.com." {
		t.Errorf("primary name server = %q, want the derived default", z.Soa.PrimaryNs)
	}

	t.Run("it is in the listing", func(t *testing.T) {
		var page gen.ZonePage
		h.decode(h.do(http.MethodGet, "/zones", nil), http.StatusOK, &page)
		if len(page.Items) != 1 || page.Items[0].Id != z.Id {
			t.Errorf("listing = %+v, want the one zone that was created", page.Items)
		}
	})

	t.Run("it can be read back", func(t *testing.T) {
		var got gen.Zone
		h.decode(h.do(http.MethodGet, "/zones/"+z.Id, nil), http.StatusOK, &got)
		if got.Id != z.Id {
			t.Errorf("read back %q, want %q", got.Id, z.Id)
		}
	})

	t.Run("the query path answers for it straight away", func(t *testing.T) {
		// The whole point of republishing: a zone created over HTTP is one the
		// DNS side answers for, without a restart and without a rebuild.
		if got := h.snaps.Snapshot().Zones(); got != 1 {
			t.Errorf("the snapshot holds %d zones, want the new one", got)
		}
	})

	t.Run("a second zone with the same name is a conflict", func(t *testing.T) {
		resp := h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "example.com."})
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("status = %d, want 409", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, problemContentType) {
			t.Errorf("content type = %q, want a problem document", ct)
		}
	})

	t.Run("deleting it takes it off the wire too", func(t *testing.T) {
		h.decode(h.do(http.MethodDelete, "/zones/"+z.Id, nil), http.StatusNoContent, nil)

		if got := h.snaps.Snapshot().Zones(); got != 0 {
			t.Errorf("the snapshot still holds %d zones after the delete", got)
		}
		resp := h.do(http.MethodGet, "/zones/"+z.Id, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d after the delete, want 404", resp.StatusCode)
		}
	})
}

func TestRecordLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	z := h.createZone("example.com.")

	var created gen.RecordWritten
	h.decode(h.do(http.MethodPost, "/zones/"+z.Id+"/records", gen.CreateRecord{
		Name: "www.example.com.",
		Type: "A",
		Data: "192.0.2.10",
	}), http.StatusCreated, &created)

	if created.Record.Name != "www.example.com." || created.Record.Data != "192.0.2.10" {
		t.Errorf("created %+v, want the record that was asked for", created.Record)
	}
	if created.Record.Ttl != z.DefaultTtl {
		t.Errorf("ttl = %d, want the zone's default of %d", created.Record.Ttl, z.DefaultTtl)
	}
	// The stamps belong to the store. Answering with the record as it was sent
	// would report a zero time for a row that has a real one.
	if created.Record.CreatedAt.IsZero() || created.Record.UpdatedAt.IsZero() {
		t.Errorf("created %v / updated %v, want the times the store wrote",
			created.Record.CreatedAt, created.Record.UpdatedAt)
	}

	t.Run("no reverse zone covers it, and that is said rather than swallowed", func(t *testing.T) {
		// docs/decisions.md D6: the hint is structured data, not prose.
		if created.MissingZones == nil || len(*created.MissingZones) != 1 {
			t.Fatalf("missingZones = %v, want the reverse zone that would be needed",
				created.MissingZones)
		}
		if got := (*created.MissingZones)[0].ZoneName; got != "2.0.192.in-addr.arpa." {
			t.Errorf("suggested zone = %q, want the /24 covering the address", got)
		}
	})

	t.Run("the query path answers for it", func(t *testing.T) {
		var a dns.Answer
		h.snaps.Snapshot().Resolve(dns.Question{
			Name:  zone.MustParseName("www.example.com."),
			Class: zone.ClassIN,
			Type:  zone.TypeA,
		}, &a)
		if len(a.Answer) != 1 {
			t.Errorf("the snapshot answers %d records, want the one just added", len(a.Answer))
		}
	})

	t.Run("it is in the zone's listing", func(t *testing.T) {
		var page gen.RecordPage
		h.decode(h.do(http.MethodGet, "/zones/"+z.Id+"/records", nil), http.StatusOK, &page)
		// The apex NS the zone was created with, plus this one.
		if len(page.Items) != 2 {
			t.Errorf("listing holds %d records, want the apex NS and the new one", len(page.Items))
		}
	})

	t.Run("deleting it removes it from the wire", func(t *testing.T) {
		h.decode(h.do(http.MethodDelete, "/records/"+created.Record.Id, nil),
			http.StatusNoContent, nil)

		var a dns.Answer
		h.snaps.Snapshot().Resolve(dns.Question{
			Name: zone.MustParseName("www.example.com."), Class: zone.ClassIN, Type: zone.TypeA,
		}, &a)
		if len(a.Answer) != 0 {
			t.Errorf("the snapshot still answers %d records after the delete", len(a.Answer))
		}
	})

	t.Run("a record outside the zone is refused", func(t *testing.T) {
		resp := h.do(http.MethodPost, "/zones/"+z.Id+"/records", gen.CreateRecord{
			Name: "www.example.net.", Type: "A", Data: "192.0.2.1",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("a relative name is refused with the absolute one", func(t *testing.T) {
		// Never completed with the apex: completing it is what turns
		// "www.example.com" into "www.example.com.example.com." in a zonefile.
		// Somebody who typed a relative name is told what they meant instead
		// of getting a record where they cannot see it.
		var p gen.Problem
		h.decode(h.do(http.MethodPost, "/zones/"+z.Id+"/records", gen.CreateRecord{
			Name: "api", Type: "A", Data: "192.0.2.1",
		}), http.StatusBadRequest, &p)

		if p.Detail == nil || !strings.Contains(*p.Detail, "api.example.com.") {
			t.Errorf("detail = %v, want it to name the absolute form", p.Detail)
		}
	})

	t.Run("data that is not that type is refused with a reason", func(t *testing.T) {
		resp := h.do(http.MethodPost, "/zones/"+z.Id+"/records", gen.CreateRecord{
			Name: "bad.example.com.", Type: "A", Data: "not-an-address",
		})
		var p gen.Problem
		h.decode(resp, http.StatusUnprocessableEntity, &p)
		if p.Detail == nil || *p.Detail == "" {
			t.Error("the problem document says nothing about what was wrong")
		}
	})
}

// TestAuth covers docs/decisions.md D5 from the outside.
func TestAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	noToken := func(r *http.Request) { r.Header.Del("Authorization") }
	badToken := func(r *http.Request) { r.Header.Set("Authorization", "Bearer weg_nonsense") }

	t.Run("health needs no token", func(t *testing.T) {
		resp := h.do(http.MethodGet, healthPath, nil, noToken)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("everything else does", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/zones", nil, noToken)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got == "" {
			t.Error("a 401 without WWW-Authenticate leaves a client guessing how to authenticate")
		}
	})

	t.Run("a token that is not ours is refused", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/zones", nil, badToken)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("a read token cannot write", func(t *testing.T) {
		secret, tok, err := MintToken("reader", []Scope{ScopeRead}, time.Now())
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if err := h.store.Update(t.Context(), func(tx store.Tx) error {
			return tx.CreateToken(t.Context(), &tok)
		}); err != nil {
			t.Fatalf("store the token: %v", err)
		}
		asReader := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }

		if resp := h.do(http.MethodGet, "/zones", nil, asReader); resp.StatusCode != http.StatusOK {
			t.Errorf("reading with a read token: status = %d, want 200", resp.StatusCode)
		}
		resp := h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "nope.example."}, asReader)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("writing with a read token: status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("a revoked token stops working", func(t *testing.T) {
		secret, tok, err := MintToken("temporary", []Scope{ScopeAdmin}, time.Now())
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if err := h.store.Update(t.Context(), func(tx store.Tx) error {
			return tx.CreateToken(t.Context(), &tok)
		}); err != nil {
			t.Fatalf("store the token: %v", err)
		}
		asTemp := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }

		if resp := h.do(http.MethodGet, "/zones", nil, asTemp); resp.StatusCode != http.StatusOK {
			t.Fatalf("the token does not work before revocation: %d", resp.StatusCode)
		}
		if err := h.store.Update(t.Context(), func(tx store.Tx) error {
			return tx.RevokeToken(t.Context(), tok.ID, time.Now())
		}); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if resp := h.do(http.MethodGet, "/zones", nil, asTemp); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a revoked token still works: status = %d", resp.StatusCode)
		}
	})
}

// TestAuthRateLimit checks that a wrong token cannot be guessed at indefinitely
// (docs/decisions.md D5).
func TestAuthRateLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bad := func(r *http.Request) { r.Header.Set("Authorization", "Bearer weg_wrong") }

	var limited bool
	for range authBurst + 5 {
		if h.do(http.MethodGet, "/zones", nil, bad).StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("a wrong token can be tried without limit")
	}

	// The limit gates before the token is looked up, so an address that has
	// been guessing is refused whatever it presents next. That is the point —
	// the work of a lookup is what is being bounded, and it is also the cost:
	// one bad client behind a shared address takes the others with it until
	// the bucket refills, which takes seconds.
	if resp := h.do(http.MethodGet, "/zones", nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d for a valid token from a limited address, want 429",
			resp.StatusCode)
	}
}

// TestNotFound checks that an identifier for something that is not there is a
// 404 with a problem document rather than a 500.
func TestNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for _, path := range []string{"/zones/01ARZ3NDEKTSV4RRFFQ69G5FAV", "/records/01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		var p gen.Problem
		h.decode(h.do(http.MethodGet, path, nil), http.StatusNotFound, &p)
		if p.Type != typeNotFound || p.Status != http.StatusNotFound {
			t.Errorf("%s: problem = %+v, want a not-found document", path, p)
		}
	}
}

// TestHealthWithoutSnapshot pins what "serving" means. A process that is
// listening with nothing loaded answers REFUSED for zones it is supposed to
// hold, so telling a load balancer it is ready would send it traffic it cannot
// answer.
func TestHealthWithoutSnapshot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.snaps.SetSnapshot(nil)

	resp := h.do(http.MethodGet, healthPath, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d without a snapshot, want 503", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, problemContentType) {
		t.Errorf("content type = %q, want a problem document", ct)
	}
}

// TestCreateZoneWithSOA covers the half of soaFor that takes what the client
// sent, including the range check that keeps a number which is not a TTL from
// being truncated into one.
func TestCreateZoneWithSOA(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	t.Run("the supplied parameters are used", func(t *testing.T) {
		var z gen.Zone
		h.decode(h.do(http.MethodPost, "/zones", gen.CreateZone{
			Name:       "example.org.",
			DefaultTtl: ptr(int64(600)),
			Soa: &gen.SOAInput{
				PrimaryNs: ptr("ns.elsewhere.example."),
				Mailbox:   ptr("dns.elsewhere.example."),
				Refresh:   ptr(int64(7200)), Retry: ptr(int64(1800)),
				Expire: ptr(int64(604800)), Minimum: ptr(int64(300)), Ttl: ptr(int64(900)),
			},
		}), http.StatusCreated, &z)

		if z.Soa.PrimaryNs != "ns.elsewhere.example." || z.Soa.Refresh != 7200 {
			t.Errorf("soa = %+v, want the parameters that were sent", z.Soa)
		}
		if z.DefaultTtl != 600 {
			t.Errorf("default ttl = %d, want 600", z.DefaultTtl)
		}
		// D2: the serial is the journal's, not the client's. A new zone starts
		// where a new zone starts.
		if z.Soa.Serial == nil || *z.Soa.Serial != 1 {
			t.Errorf("serial = %v, want a new zone to start at 1", z.Soa.Serial)
		}
	})

	// The point of the partial shape: a client with an opinion about one field
	// should not have to send five it has none about, and the five it left out
	// have to come out as this server's defaults rather than as zero, an SOA
	// with a refresh of zero is a zone secondaries never refresh.
	t.Run("what is not sent comes from the defaults", func(t *testing.T) {
		var z gen.Zone
		h.decode(h.do(http.MethodPost, "/zones", gen.CreateZone{
			Name: "partial.example.",
			Soa:  &gen.SOAInput{PrimaryNs: ptr("ns.elsewhere.example.")},
		}), http.StatusCreated, &z)

		if z.Soa.PrimaryNs != "ns.elsewhere.example." {
			t.Errorf("primaryNs = %q, want the one that was sent", z.Soa.PrimaryNs)
		}
		if z.Soa.Mailbox != "hostmaster.partial.example." {
			t.Errorf("mailbox = %q, want the default under the zone's own apex", z.Soa.Mailbox)
		}
		if z.Soa.Refresh == 0 || z.Soa.Retry == 0 || z.Soa.Expire == 0 || z.Soa.Minimum == 0 {
			t.Errorf("soa = %+v, want the timers filled from the defaults", z.Soa)
		}
	})

	t.Run("a number that is not a TTL is refused rather than truncated", func(t *testing.T) {
		var p gen.Problem
		h.decode(h.do(http.MethodPost, "/zones", gen.CreateZone{
			Name:       "toobig.example.",
			DefaultTtl: ptr(int64(1) << 40),
		}), http.StatusBadRequest, &p)

		if p.Detail == nil || !strings.Contains(*p.Detail, "TTL") {
			t.Errorf("detail = %v, want it to name the field that was wrong", p.Detail)
		}
	})
}

// TestReverseConflict covers docs/decisions.md D3 through the API: a second
// name for one address does not take the reverse entry away from the first, and
// the client is told rather than left to wonder.
func TestReverseConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	forward := h.createZone("example.com.")
	h.createZone("2.0.192.in-addr.arpa.")

	var first gen.RecordWritten
	h.decode(h.do(http.MethodPost, "/zones/"+forward.Id+"/records", gen.CreateRecord{
		Name: "www.example.com.", Type: "A", Data: "192.0.2.10",
	}), http.StatusCreated, &first)

	if first.Generated == nil || len(*first.Generated) != 1 {
		t.Fatalf("generated = %v, want the PTR the address caused", first.Generated)
	}
	if got := (*first.Generated)[0]; got.Type != "PTR" || got.Data != "www.example.com." {
		t.Errorf("generated %+v, want a PTR to the name that was added", got)
	}

	var second gen.RecordWritten
	h.decode(h.do(http.MethodPost, "/zones/"+forward.Id+"/records", gen.CreateRecord{
		Name: "mail.example.com.", Type: "A", Data: "192.0.2.10",
	}), http.StatusCreated, &second)

	if second.Conflicts == nil || len(*second.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want the address to report that it already answers",
			second.Conflicts)
	}
	c := (*second.Conflicts)[0]
	if c.ExistingName != "www.example.com." || c.RequestedName != "mail.example.com." {
		t.Errorf("conflict = %+v, want it to name both sides", c)
	}
	// First wins: the record was still added, and the reverse entry was not
	// taken away from the name that had it.
	if second.Record.Id == "" {
		t.Error("the record was not added, and a conflict is not a refusal")
	}
}

// TestBootstrapTokenIsMintedOnce checks that a second start does not hand out a
// second administrator token to whoever happens to read the log.
func TestBootstrapTokenIsMintedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	again, err := EnsureBootstrapToken(t.Context(), h.store, time.Now())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if again != "" {
		t.Errorf("a second token was minted: %q", again)
	}
}

// TestRecovererKeepsTheServerUp checks that a panic in a handler costs the
// request and nothing else. The API is the control plane: losing it takes away
// the operator's only way to fix whatever caused the panic.
func TestRecovererKeepsTheServerUp(t *testing.T) {
	t.Parallel()

	var reported error
	s := &Server{onError: func(err error) { reported = err }}
	handler := s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something went very wrong")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/zones", http.NoBody))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if reported == nil {
		t.Error("the panic was swallowed instead of reaching the operator")
	}
	if strings.Contains(rec.Body.String(), "something went very wrong") {
		t.Error("the response repeats what went wrong inside, which is not a client's business")
	}
}

func TestUpdateZone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	z := h.createZone("example.com.")

	var got gen.Zone
	h.decode(h.do(http.MethodPatch, "/zones/"+z.Id, gen.UpdateZone{
		DefaultTtl: ptr(int64(300)),
		Comment:    ptr("the customer zone"),
	}), http.StatusOK, &got)

	if got.DefaultTtl != 300 || deref(got.Comment, "") != "the customer zone" {
		t.Errorf("updated %+v, want the default TTL and the comment that were sent", got)
	}
	t.Run("what was not sent is left alone", func(t *testing.T) {
		if got.Soa.PrimaryNs != z.Soa.PrimaryNs || got.Name != z.Name {
			t.Errorf("updated %+v, want everything else as it was in %+v", got, z)
		}
	})
	t.Run("every change advances the serial by exactly one", func(t *testing.T) {
		// Even one that is invisible on the wire, such as this comment. The
		// serial is what orders the journal (journal_commits is unique on
		// (zone_id, serial_to)) so a commit that did not advance it would
		// have nowhere to sit. Advancing costs a secondary one transfer of a
		// zone that did not change; not advancing costs the ordering that
		// rollback and IXFR are built on.
		if deref(got.Soa.Serial, 0) != deref(z.Soa.Serial, 0)+1 {
			t.Errorf("serial went from %d to %d, want exactly one step (D2)",
				deref(z.Soa.Serial, 0), deref(got.Soa.Serial, 0))
		}
	})

	t.Run("a change to nothing writes nothing", func(t *testing.T) {
		var after gen.Zone
		h.decode(h.do(http.MethodPatch, "/zones/"+z.Id, gen.UpdateZone{
			DefaultTtl: ptr(int64(300)),
		}), http.StatusOK, &after)

		if deref(after.Soa.Serial, 0) != deref(got.Soa.Serial, 0) {
			t.Errorf("serial went from %d to %d for a request that changed nothing",
				deref(got.Soa.Serial, 0), deref(after.Soa.Serial, 0))
		}
	})

	t.Run("the SOA timers can be changed", func(t *testing.T) {
		var after gen.Zone
		h.decode(h.do(http.MethodPatch, "/zones/"+z.Id, gen.UpdateZone{
			Soa: &gen.SOAInput{Refresh: ptr(int64(7200))},
		}), http.StatusOK, &after)

		if after.Soa.Refresh != 7200 {
			t.Errorf("refresh = %d, want the 7200 that was sent", after.Soa.Refresh)
		}
		if after.Soa.Retry != z.Soa.Retry {
			t.Errorf("retry = %d, want the timer that was not mentioned left alone",
				after.Soa.Retry)
		}
	})

	t.Run("automatic reverse management has three states", func(t *testing.T) {
		// Absent leaves it, null puts the zone back on the server-wide
		// setting, and a value overrides it. A two-state field could not say
		// the middle one, which is why this one is nullable.
		send := func(body gen.UpdateZone) *bool {
			t.Helper()
			var out gen.Zone
			h.decode(h.do(http.MethodPatch, "/zones/"+z.Id, body), http.StatusOK, &out)
			return out.AutoReverse
		}

		var off gen.UpdateZone
		off.AutoReverse.Set(false)
		if v := send(off); v == nil || *v {
			t.Errorf("autoReverse = %v after false, want it overridden", v)
		}

		if v := send(gen.UpdateZone{Comment: ptr("unrelated")}); v == nil || *v {
			t.Errorf("autoReverse = %v after an unrelated edit, want it left alone", v)
		}

		var inherit gen.UpdateZone
		inherit.AutoReverse.SetNull()
		if v := send(inherit); v != nil {
			t.Errorf("autoReverse = %v after null, want it back on the global setting", *v)
		}
	})

	t.Run("an impossible TTL is refused rather than truncated", func(t *testing.T) {
		resp := h.do(http.MethodPatch, "/zones/"+z.Id, gen.UpdateZone{
			DefaultTtl: ptr(int64(1) << 40),
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestUpdateRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	z := h.createZone("example.com.")
	h.createZone("2.0.192.in-addr.arpa.")

	created := h.createRecord(z.Id, gen.CreateRecord{
		Name: "www.example.com.", Type: "A", Data: "192.0.2.10", Comment: ptr("the web server"),
	})

	var got gen.RecordWritten
	h.decode(h.do(http.MethodPatch, "/records/"+created.Record.Id, gen.UpdateRecord{
		Data: ptr("192.0.2.11"),
		Ttl:  ptr(int64(60)),
	}), http.StatusOK, &got)

	if got.Record.Id != created.Record.Id {
		t.Errorf("id = %q, want the record to keep its identity across an edit", got.Record.Id)
	}
	if got.Record.Data != "192.0.2.11" || got.Record.Ttl != 60 {
		t.Errorf("updated %+v, want the data and the TTL that were sent", got.Record)
	}
	if deref(got.Record.Comment, "") != "the web server" {
		t.Errorf("comment = %v, want an edit that did not mention it to keep it",
			got.Record.Comment)
	}

	t.Run("the reverse entry follows the address", func(t *testing.T) {
		if got.Generated == nil || len(*got.Generated) != 1 {
			t.Fatalf("generated = %v, want the PTR to have moved with the record",
				got.Generated)
		}
		if name := (*got.Generated)[0].Name; name != "11.2.0.192.in-addr.arpa." {
			t.Errorf("the PTR sits at %q, want it at the new address", name)
		}
	})

	t.Run("the query path answers with the new data", func(t *testing.T) {
		got := h.answers("www.example.com.", zone.TypeA)
		if len(got) != 1 || !strings.HasSuffix(got[0], "192.0.2.11") {
			t.Errorf("the snapshot answers %v, want the address the edit set", got)
		}
	})

	t.Run("a generated record is refused, and can be taken over", func(t *testing.T) {
		ptrID := (*got.Generated)[0].Id

		resp := h.do(http.MethodPatch, "/records/"+ptrID, gen.UpdateRecord{
			Data: ptr("elsewhere.example.com."),
		})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want a generated record to be refused (D4)", resp.StatusCode)
		}

		var detached gen.RecordWritten
		h.decode(h.do(http.MethodPost, "/records/"+ptrID+"/detach", nil), http.StatusOK, &detached)
		if detached.Record.ManagedBy != nil {
			t.Errorf("managedBy = %v after detaching, want it gone", detached.Record.ManagedBy)
		}

		var edited gen.RecordWritten
		h.decode(h.do(http.MethodPatch, "/records/"+ptrID, gen.UpdateRecord{
			Data: ptr("elsewhere.example.com."),
		}), http.StatusOK, &edited)
		if edited.Record.Data != "elsewhere.example.com." {
			t.Errorf("data = %q, want the detached record to be editable", edited.Record.Data)
		}
	})

	t.Run("detaching an authored record is not an error", func(t *testing.T) {
		var again gen.RecordWritten
		h.decode(h.do(http.MethodPost, "/records/"+created.Record.Id+"/detach", nil),
			http.StatusOK, &again)
		if again.Record.Id != created.Record.Id {
			t.Errorf("detach returned %+v, want the record unchanged", again.Record)
		}
	})

	t.Run("data that does not parse as the new type is refused", func(t *testing.T) {
		// Changing the type alone re-reads the same text as a different
		// record. 192.0.2.11 is not an AAAA, and that has to be caught here
		// rather than stored and discovered on the wire.
		resp := h.do(http.MethodPatch, "/records/"+created.Record.Id, gen.UpdateRecord{
			Type: ptr("AAAA"),
		})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", resp.StatusCode)
		}
	})

	t.Run("a name outside the zone is refused", func(t *testing.T) {
		resp := h.do(http.MethodPatch, "/records/"+created.Record.Id, gen.UpdateRecord{
			Name: ptr("www.example.org."),
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestReplaceRRsets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	z := h.createZone("example.com.")

	var first gen.RRsetsWritten
	h.decode(h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{
		Rrsets: []gen.RRset{{
			Name: "www.example.com.",
			Type: "A",
			Ttl:  ptr(int64(300)),
			Records: []gen.RRsetMember{
				{Data: "192.0.2.10", Comment: ptr("the first one")},
				{Data: "192.0.2.11"},
			},
		}},
	}), http.StatusOK, &first)

	if len(first.Records) != 2 {
		t.Fatalf("the set holds %d records, want the two that were sent", len(first.Records))
	}
	for _, r := range first.Records {
		if r.Ttl != 300 {
			t.Errorf("%s has TTL %d, want the one the set carries (RFC 2181 §5.2)", r.Data, r.Ttl)
		}
	}

	t.Run("the query path answers with both", func(t *testing.T) {
		if got := h.answers("www.example.com.", zone.TypeA); len(got) != 2 {
			t.Errorf("the snapshot answers %v, want both members", got)
		}
	})

	t.Run("a member that stays keeps its identity", func(t *testing.T) {
		// The point of sending the set rather than a list of changes: the
		// server works out the difference, so the record that did not change
		// is not removed and written again, and keeps its comment with it.
		kept := recordWithData(t, first.Records, "192.0.2.10")

		var second gen.RRsetsWritten
		h.decode(h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{
			Rrsets: []gen.RRset{{
				Name: "www.example.com.",
				Type: "A",
				Ttl:  ptr(int64(300)),
				Records: []gen.RRsetMember{
					{Data: "192.0.2.10", Comment: ptr("the first one")},
					{Data: "192.0.2.12"},
				},
			}},
		}), http.StatusOK, &second)

		again := recordWithData(t, second.Records, "192.0.2.10")
		if again.Id != kept.Id {
			t.Errorf("the unchanged member is now %q, was %q", again.Id, kept.Id)
		}
		if deref(again.Comment, "") != "the first one" {
			t.Errorf("comment = %v, want it to have survived the edit", again.Comment)
		}
		if got := h.answers("www.example.com.", zone.TypeA); len(got) != 2 {
			t.Errorf("the snapshot answers %v, want the set as it was sent", got)
		}
	})

	t.Run("several sets in one request are one commit", func(t *testing.T) {
		// One user action, one line in the history, one point to roll back to.
		before := h.commitCount(z.Id)

		var out gen.RRsetsWritten
		h.decode(h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{
			Comment: ptr("a page of edits"),
			Rrsets: []gen.RRset{
				{Name: "a.example.com.", Type: "A", Records: []gen.RRsetMember{{Data: "192.0.2.1"}}},
				{Name: "b.example.com.", Type: "A", Records: []gen.RRsetMember{{Data: "192.0.2.2"}}},
			},
		}), http.StatusOK, &out)

		if len(out.Records) != 2 {
			t.Errorf("wrote %d records, want one per set", len(out.Records))
		}
		if got := h.commitCount(z.Id) - before; got != 1 {
			t.Errorf("the request produced %d commits, want exactly one", got)
		}
	})

	t.Run("an empty set removes it", func(t *testing.T) {
		var out gen.RRsetsWritten
		h.decode(h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{
			Rrsets: []gen.RRset{{Name: "www.example.com.", Type: "A", Records: nil}},
		}), http.StatusOK, &out)

		if len(out.Records) != 0 {
			t.Errorf("the set still holds %d records", len(out.Records))
		}
		if got := h.answers("www.example.com.", zone.TypeA); len(got) != 0 {
			t.Errorf("the snapshot still answers %v", got)
		}
	})

	t.Run("two canonical names at one name are refused", func(t *testing.T) {
		// RFC 2181 §10.1: a name has one canonical name, so a second CNAME is
		// not an alternative but a contradiction.
		resp := h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{
			Rrsets: []gen.RRset{{
				Name: "alias.example.com.",
				Type: "CNAME",
				Records: []gen.RRsetMember{
					{Data: "one.example.com."},
					{Data: "two.example.com."},
				},
			}},
		})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", resp.StatusCode)
		}
	})

	t.Run("a set outside the zone is refused", func(t *testing.T) {
		resp := h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{
			Rrsets: []gen.RRset{{
				Name: "www.example.org.", Type: "A",
				Records: []gen.RRsetMember{{Data: "192.0.2.1"}},
			}},
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("a request with no sets is refused", func(t *testing.T) {
		resp := h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestReplaceRRsetsGeneratesReverse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	z := h.createZone("example.com.")
	h.createZone("2.0.192.in-addr.arpa.")

	var out gen.RRsetsWritten
	h.decode(h.do(http.MethodPut, "/zones/"+z.Id+"/rrsets", gen.ReplaceRRsets{
		Rrsets: []gen.RRset{{
			Name: "www.example.com.", Type: "A",
			Records: []gen.RRsetMember{{Data: "192.0.2.10"}},
		}},
	}), http.StatusOK, &out)

	if out.Generated == nil || len(*out.Generated) != 1 {
		t.Fatalf("generated = %v, want the PTR the address caused", out.Generated)
	}
	if got := (*out.Generated)[0]; got.Name != "10.2.0.192.in-addr.arpa." || got.Data != "www.example.com." {
		t.Errorf("generated %+v, want the PTR for the address that was written", got)
	}
}

// recordWithData picks the one member of a set carrying the given data.
func recordWithData(t *testing.T, recs []gen.Record, data string) gen.Record {
	t.Helper()
	for i := range recs {
		if recs[i].Data == data {
			return recs[i]
		}
	}
	t.Fatalf("no record with data %q in %+v", data, recs)
	return gen.Record{}
}

// commitCount is how many commits a zone's history holds.
func (h *harness) commitCount(zid string) int {
	h.t.Helper()

	var n int
	if err := h.store.View(h.t.Context(), func(r store.Reader) error {
		page, perr := r.ListCommits(h.t.Context(), store.CommitFilter{
			ZoneID: zone.ZoneID(zid),
			Paging: store.Paging{Limit: store.MaxLimit},
		})
		n = len(page.Items)
		return perr
	}); err != nil {
		h.t.Fatalf("read the history: %v", err)
	}
	return n
}

func TestHistory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	z := h.createZone("example.com.")

	created := h.createRecord(z.Id, gen.CreateRecord{
		Name: "www.example.com.", Type: "A", Data: "192.0.2.10",
	})

	var page gen.CommitPage
	h.decode(h.do(http.MethodGet, "/commits?zoneId="+z.Id, nil), http.StatusOK, &page)

	if len(page.Items) != 2 {
		t.Fatalf("history holds %d commits, want the zone and the record", len(page.Items))
	}
	newest, oldest := page.Items[0], page.Items[1]
	if newest.Kind != gen.CommitKindEdit || oldest.Kind != gen.CommitKindZoneCreate {
		t.Errorf("history reads %s then %s, want newest first", newest.Kind, oldest.Kind)
	}
	if newest.SerialFrom != oldest.SerialTo || newest.SerialTo != newest.SerialFrom+1 {
		t.Errorf("serials %d→%d then %d→%d, want each commit to take exactly one step",
			oldest.SerialFrom, oldest.SerialTo, newest.SerialFrom, newest.SerialTo)
	}
	if newest.Events != nil {
		t.Error("a listing carries the events, which is what makes a long history expensive")
	}

	t.Run("one commit carries what it changed", func(t *testing.T) {
		var c gen.Commit
		h.decode(h.do(http.MethodGet, "/commits/"+newest.Id, nil), http.StatusOK, &c)

		if c.Events == nil || len(*c.Events) != 1 {
			t.Fatalf("events = %v, want the record that was added", c.Events)
		}
		e := (*c.Events)[0]
		if e.Op != gen.Add || e.Name != "www.example.com." || e.Data != "192.0.2.10" {
			t.Errorf("event = %+v, want the addition in full", e)
		}
	})

	t.Run("a deletion carries the record it removed", func(t *testing.T) {
		// RFC 1995 §2 lists deleted records in full rather than by name, and
		// a rollback that has to put one back needs the same thing.
		h.decode(h.do(http.MethodDelete, "/records/"+created.Record.Id, nil),
			http.StatusNoContent, nil)

		var after gen.CommitPage
		h.decode(h.do(http.MethodGet, "/commits?zoneId="+z.Id+"&limit=1", nil),
			http.StatusOK, &after)

		var c gen.Commit
		h.decode(h.do(http.MethodGet, "/commits/"+after.Items[0].Id, nil), http.StatusOK, &c)
		if c.Events == nil || len(*c.Events) != 1 {
			t.Fatalf("events = %v, want the removal", c.Events)
		}
		if e := (*c.Events)[0]; e.Op != gen.Del || e.Data != "192.0.2.10" {
			t.Errorf("event = %+v, want the removal to carry the data in full", e)
		}
	})

	t.Run("it says who made the change", func(t *testing.T) {
		if newest.Source != gen.CommitSourceApi {
			t.Errorf("source = %q, want the interface it arrived through", newest.Source)
		}
		if deref(newest.Actor, "") == "" {
			t.Error("actor is empty, want the token that authenticated the request")
		}
	})

	t.Run("it can be narrowed to a kind", func(t *testing.T) {
		var only gen.CommitPage
		h.decode(h.do(http.MethodGet, "/commits?zoneId="+z.Id+"&kind=zone_create", nil),
			http.StatusOK, &only)
		if len(only.Items) != 1 || only.Items[0].Kind != gen.CommitKindZoneCreate {
			t.Errorf("filtered history = %+v, want only the zone's first commit", only.Items)
		}
	})

	t.Run("the history of a deleted zone survives it", func(t *testing.T) {
		// A commit outlives the zone it describes: the last thing that happens
		// to a zone is that someone deletes it, and "who removed example.com,
		// and when" is exactly the question the history is here to answer.
		h.decode(h.do(http.MethodDelete, "/zones/"+z.Id, nil), http.StatusNoContent, nil)

		var gone gen.CommitPage
		h.decode(h.do(http.MethodGet, "/commits?zoneId="+z.Id, nil), http.StatusOK, &gone)
		if len(gone.Items) == 0 {
			t.Fatal("the history went with the zone")
		}
		if gone.Items[0].Kind != gen.CommitKindZoneDelete || gone.Items[0].ZoneName != "example.com." {
			t.Errorf("last commit = %+v, want the deletion under the name the zone had",
				gone.Items[0])
		}
	})

	t.Run("an unknown commit is a 404", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/commits/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func TestTokens(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	var listed []gen.Token
	h.decode(h.do(http.MethodGet, "/tokens", nil), http.StatusOK, &listed)
	if len(listed) != 1 || listed[0].Name != BootstrapName {
		t.Fatalf("tokens = %+v, want the one minted on first start", listed)
	}
	if listed[0].Prefix == "" || strings.Contains(h.token, listed[0].Prefix[len(TokenPrefix):]) == false {
		t.Errorf("prefix = %q, want the leading characters of the secret", listed[0].Prefix)
	}

	var minted gen.TokenCreated
	h.decode(h.do(http.MethodPost, "/tokens", gen.CreateToken{
		Name:   "the deploy pipeline",
		Scopes: []gen.Scope{gen.Write},
	}), http.StatusCreated, &minted)

	if !strings.HasPrefix(minted.Secret, TokenPrefix) {
		t.Errorf("secret = %q, want it marked as one of ours", minted.Secret)
	}

	t.Run("the new token works, within its scope", func(t *testing.T) {
		withNew := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+minted.Secret) }

		if resp := h.do(http.MethodGet, "/zones", nil, withNew); resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d for a read with a write token, want 200", resp.StatusCode)
		}
		resp := h.do(http.MethodPost, "/tokens", gen.CreateToken{
			Name: "escalation", Scopes: []gen.Scope{gen.Admin},
		}, withNew)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want a write token to be refused an admin token", resp.StatusCode)
		}
	})

	t.Run("the secret is never shown again", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/tokens", nil)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read the listing: %v", err)
		}
		if strings.Contains(string(body), minted.Secret) {
			t.Error("the listing carries a secret")
		}
	})

	t.Run("revoking it takes it out of use", func(t *testing.T) {
		h.decode(h.do(http.MethodDelete, "/tokens/"+minted.Token.Id, nil),
			http.StatusNoContent, nil)

		resp := h.do(http.MethodGet, "/zones", nil, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+minted.Secret)
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d for a revoked token, want 401", resp.StatusCode)
		}

		var after []gen.Token
		h.decode(h.do(http.MethodGet, "/tokens", nil), http.StatusOK, &after)
		revoked := tokenNamed(t, after, "the deploy pipeline")
		if revoked.RevokedAt == nil {
			t.Error("the revoked token is listed as still usable")
		}
	})

	t.Run("an expiry in the past is refused", func(t *testing.T) {
		past := time.Now().Add(-time.Hour)
		resp := h.do(http.MethodPost, "/tokens", gen.CreateToken{
			Name: "already over", Scopes: []gen.Scope{gen.Read}, ExpiresAt: &past,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("an unknown token is a 404", func(t *testing.T) {
		resp := h.do(http.MethodDelete, "/tokens/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// tokenNamed picks one token out of a listing by its name.
func tokenNamed(t *testing.T, toks []gen.Token, name string) gen.Token {
	t.Helper()
	for i := range toks {
		if toks[i].Name == name {
			return toks[i]
		}
	}
	t.Fatalf("no token named %q in %+v", name, toks)
	return gen.Token{}
}

// TestTokenLockout has its own harness, because the last thing it does is take
// away the credential the harness authenticates with.
func TestTokenLockout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	var listed []gen.Token
	h.decode(h.do(http.MethodGet, "/tokens", nil), http.StatusOK, &listed)
	bootstrap := listed[0].Id

	// Revoked tokens are kept, so a server whose every token is revoked does
	// not mint a new bootstrap token on the next start. It stays locked, and
	// the only way back in is editing the database by hand.
	resp := h.do(http.MethodDelete, "/tokens/"+bootstrap, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want the last admin token to be refused", resp.StatusCode)
	}

	var second gen.TokenCreated
	h.decode(h.do(http.MethodPost, "/tokens", gen.CreateToken{
		Name: "the second administrator", Scopes: []gen.Scope{gen.Admin},
	}), http.StatusCreated, &second)

	// With another way in, the first one may go.
	h.decode(h.do(http.MethodDelete, "/tokens/"+bootstrap, nil), http.StatusNoContent, nil)

	withSecond := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+second.Secret) }
	if got := h.do(http.MethodGet, "/tokens", nil, withSecond); got.StatusCode != http.StatusOK {
		t.Errorf("status = %d for the remaining administrator, want 200", got.StatusCode)
	}
}

// TestSession covers the browser half of docs/decisions.md D5.
func TestSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	resp := h.do(http.MethodPost, sessionPath, gen.CreateSession{Token: h.token},
		func(r *http.Request) { r.Header.Del("Authorization") })

	var opened gen.Session
	h.decode(resp, http.StatusCreated, &opened)
	if opened.Name != BootstrapName || opened.ExpiresAt == nil {
		t.Errorf("session = %+v, want it to name the token and say when it ends", opened)
	}

	sessionValue, csrfValue := cookiesOf(t, resp)
	if sessionValue == "" || csrfValue == "" {
		t.Fatalf("cookies = %q / %q, want both", sessionValue, csrfValue)
	}

	// Authenticates with the session and nothing else.
	withSession := func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionValue})
	}
	withCSRF := func(r *http.Request) {
		withSession(r)
		r.Header.Set(csrfHeader, csrfValue)
	}

	t.Run("the credential cookie is not readable by the page", func(t *testing.T) {
		// The whole reason the interface does not keep the token in
		// localStorage: a script injected into the page cannot read this.
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookie && !c.HttpOnly {
				t.Error("the session cookie is readable by script")
			}
			if c.Name == csrfCookie && c.HttpOnly {
				t.Error("the CSRF cookie is not readable, so the page cannot send it back")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("%s is SameSite=%v, want Strict", c.Name, c.SameSite)
			}
		}
	})

	t.Run("a read works with the cookie alone", func(t *testing.T) {
		if got := h.do(http.MethodGet, "/zones", nil, withSession); got.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", got.StatusCode)
		}
	})

	t.Run("a write without the header is refused", func(t *testing.T) {
		got := h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "csrf.example."}, withSession)
		if got.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got.StatusCode)
		}
	})

	t.Run("a write with the wrong header is refused", func(t *testing.T) {
		got := h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "csrf.example."},
			func(r *http.Request) {
				withSession(r)
				r.Header.Set(csrfHeader, "not the value")
			})
		if got.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got.StatusCode)
		}
	})

	t.Run("a write with the header works", func(t *testing.T) {
		var z gen.Zone
		h.decode(h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "session.example."}, withCSRF),
			http.StatusCreated, &z)
		if z.Name != "session.example." {
			t.Errorf("created %q, want the zone the session asked for", z.Name)
		}
	})

	t.Run("a bearer token needs no header", func(t *testing.T) {
		// CSRF needs ambient credentials, and a bearer token is not ambient:
		// a cross-site request cannot make a browser attach one.
		got := h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "bearer.example."})
		if got.StatusCode != http.StatusCreated {
			t.Errorf("status = %d, want a bearer write to go through without the header",
				got.StatusCode)
		}
	})

	t.Run("it says who it is", func(t *testing.T) {
		var who gen.Session
		h.decode(h.do(http.MethodGet, sessionPath, nil, withSession), http.StatusOK, &who)
		if who.Name != BootstrapName || who.ExpiresAt == nil {
			t.Errorf("session = %+v, want the session's own name and expiry", who)
		}

		var bearer gen.Session
		h.decode(h.do(http.MethodGet, sessionPath, nil), http.StatusOK, &bearer)
		if bearer.ExpiresAt != nil {
			t.Error("a bearer request reports a session expiry it does not have")
		}
	})

	t.Run("ending it stops the cookie working", func(t *testing.T) {
		out := h.do(http.MethodDelete, sessionPath, nil, withCSRF)
		if out.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", out.StatusCode)
		}
		for _, c := range out.Cookies() {
			if c.MaxAge >= 0 {
				t.Errorf("%s was not cleared", c.Name)
			}
		}
		// Forgotten here, not merely cleared in the browser, so a copy of the
		// cookie taken beforehand is worth nothing either.
		if got := h.do(http.MethodGet, "/zones", nil, withSession); got.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d for the ended session, want 401", got.StatusCode)
		}
	})

	t.Run("a bad token opens no session", func(t *testing.T) {
		got := h.do(http.MethodPost, sessionPath, gen.CreateSession{Token: "weg_nonsense"},
			func(r *http.Request) { r.Header.Del("Authorization") })
		if got.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got.StatusCode)
		}
		if len(got.Cookies()) != 0 {
			t.Errorf("a refused login set %d cookies", len(got.Cookies()))
		}
	})
}

func TestSessionScopes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	var minted gen.TokenCreated
	h.decode(h.do(http.MethodPost, "/tokens", gen.CreateToken{
		Name: "a reader", Scopes: []gen.Scope{gen.Read},
	}), http.StatusCreated, &minted)

	resp := h.do(http.MethodPost, sessionPath, gen.CreateSession{Token: minted.Secret},
		func(r *http.Request) { r.Header.Del("Authorization") })
	var opened gen.Session
	h.decode(resp, http.StatusCreated, &opened)

	if len(opened.Scopes) != 1 || opened.Scopes[0] != gen.Read {
		t.Errorf("scopes = %v, want the ones the token carried", opened.Scopes)
	}

	sessionValue, csrfValue := cookiesOf(t, resp)
	withCSRF := func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionValue})
		r.Header.Set(csrfHeader, csrfValue)
	}

	t.Run("the session may not do what the token may not", func(t *testing.T) {
		got := h.do(http.MethodPost, "/zones", gen.CreateZone{Name: "readonly.example."}, withCSRF)
		if got.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want a read session to be refused a write", got.StatusCode)
		}
	})

	t.Run("but it may always end itself", func(t *testing.T) {
		// A read-only token that could not log out would be one that cannot
		// stop being logged in.
		if got := h.do(http.MethodDelete, sessionPath, nil, withCSRF); got.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", got.StatusCode)
		}
	})
}

// cookiesOf pulls the session and CSRF values out of a login response.
func cookiesOf(t *testing.T, resp *http.Response) (sessionValue, csrfValue string) {
	t.Helper()
	for _, c := range resp.Cookies() {
		switch c.Name {
		case sessionCookie:
			sessionValue = c.Value
		case csrfCookie:
			csrfValue = c.Value
		}
	}
	return sessionValue, csrfValue
}

func TestSessionTransport(t *testing.T) {
	t.Parallel()

	// Checked here rather than through the server, because httptest binds on
	// loopback and there is no way to arrive from anywhere else.
	for _, tc := range []struct {
		name    string
		secure  bool
		from    string
		allowed bool
	}{
		{"over TLS", true, "203.0.113.7", true},
		{"over TLS from loopback", true, "127.0.0.1", true},
		{"plain HTTP from the same machine", false, "127.0.0.1", true},
		{"plain HTTP from the same machine over IPv6", false, "::1", true},
		{"plain HTTP from the network", false, "192.168.1.20", false},
		{"plain HTTP from a name that is not an address", false, "somewhere", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkTransport(tc.secure, tc.from)
			if tc.allowed && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !tc.allowed {
				if err == nil {
					t.Fatal("allowed, want a credential not to be put on the wire in the clear")
				}
				if asProblem(err).status != http.StatusForbidden {
					t.Errorf("status = %d, want 403", asProblem(err).status)
				}
			}
		})
	}
}

func TestRollbackZone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	z := h.createZone("example.com.")

	kept := h.createRecord(z.Id, gen.CreateRecord{
		Name: "www.example.com.", Type: "A", Data: "192.0.2.10", Comment: ptr("the web server"),
	})

	var atTarget gen.Zone
	h.decode(h.do(http.MethodGet, "/zones/"+z.Id, nil), http.StatusOK, &atTarget)
	target := deref(atTarget.Soa.Serial, 0)

	// Two changes to undo.
	added := h.createRecord(z.Id, gen.CreateRecord{
		Name: "mail.example.com.", Type: "A", Data: "192.0.2.20",
	})
	h.decode(h.do(http.MethodDelete, "/records/"+kept.Record.Id, nil), http.StatusNoContent, nil)

	var out gen.RollbackResult
	h.decode(h.do(http.MethodPost, "/zones/"+z.Id+"/rollback", gen.RollbackZone{
		Serial: target, Comment: ptr("undo the morning"),
	}), http.StatusOK, &out)

	if out.Commit == nil {
		t.Fatal("the rollback wrote no commit")
	}
	if out.Commit.Kind != gen.CommitKindRollback {
		t.Errorf("kind = %q, want a rollback", out.Commit.Kind)
	}
	if deref(out.Commit.RevertsTo, -1) != target {
		t.Errorf("revertsTo = %v, want %d", out.Commit.RevertsTo, target)
	}
	if out.Commit.SerialTo <= target {
		t.Errorf("serial went to %d, want it past %d: a rollback moves forward",
			out.Commit.SerialTo, target)
	}

	t.Run("the query path answers the restored state", func(t *testing.T) {
		if got := h.answers("www.example.com.", zone.TypeA); len(got) != 1 {
			t.Errorf("www answers %v, want the record that was restored", got)
		}
		if got := h.answers("mail.example.com.", zone.TypeA); len(got) != 0 {
			t.Errorf("mail still answers %v", got)
		}
	})

	t.Run("the record that was added is gone", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/records/"+added.Record.Id, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("rolling back again to where it now is changes nothing", func(t *testing.T) {
		var again gen.RollbackResult
		h.decode(h.do(http.MethodPost, "/zones/"+z.Id+"/rollback", gen.RollbackZone{
			Serial: out.Commit.SerialTo,
		}), http.StatusOK, &again)
		if again.Commit != nil {
			t.Errorf("a rollback to where the zone already is wrote %+v", again.Commit)
		}
	})

	t.Run("a serial the zone never had is refused", func(t *testing.T) {
		resp := h.do(http.MethodPost, "/zones/"+z.Id+"/rollback", gen.RollbackZone{Serial: 900000})
		if resp.StatusCode == http.StatusOK {
			t.Error("restored a state that was never recorded")
		}
	})

	t.Run("a number that is not a serial is refused", func(t *testing.T) {
		resp := h.do(http.MethodPost, "/zones/"+z.Id+"/rollback", gen.RollbackZone{Serial: 1 << 40})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

const importable = `$ORIGIN imported.example.
$TTL 3600
@	IN	SOA	ns1.imported.example. hostmaster.imported.example. (
		2026081801 7200 900 1209600 3600 )
@	IN	NS	ns1.imported.example.
ns1	IN	A	192.0.2.53
www	IN	A	192.0.2.10
www	IN	AAAA	2001:db8::10
sub	IN	NS	ns1.sub.imported.example.
ns1.sub	IN	A	192.0.2.54
buried.sub	IN	TXT	"never answered"
`

func TestImportAndExport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	var out gen.ZoneImported
	h.decode(h.doText(http.MethodPost, "/zones/import", importable), http.StatusCreated, &out)

	if out.Zone.Name != "imported.example." {
		t.Errorf("imported %q, want the zone the file's SOA names", out.Zone.Name)
	}

	t.Run("the serial it arrived with is the serial it starts at", func(t *testing.T) {
		// docs/decisions.md D2: restarting at 1 makes a migrated zone look
		// older than itself to every secondary that has seen it.
		if got := deref(out.Zone.Soa.Serial, 0); got != 2026081801 {
			t.Errorf("serial = %d, want the one in the file", got)
		}
	})

	t.Run("what could never be answered is reported, not refused", func(t *testing.T) {
		if out.Skipped == nil || len(*out.Skipped) != 1 {
			t.Fatalf("skipped = %v, want the record below the delegation", out.Skipped)
		}
		s := (*out.Skipped)[0]
		if !strings.Contains(s.Record, "buried.sub.imported.example.") ||
			!strings.Contains(s.Reason, "sub.imported.example.") {
			t.Errorf("skipped %+v, want the record and the delegation that buries it", s)
		}
	})

	t.Run("the query path answers for it straight away", func(t *testing.T) {
		if got := h.answers("www.imported.example.", zone.TypeA); len(got) != 1 {
			t.Errorf("the snapshot answers %v, want the imported record", got)
		}
	})

	t.Run("it exports back to something that reads the same", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/zones/"+out.Zone.Id+"/export", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/dns" {
			t.Errorf("content type = %q, want text/dns", ct)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read the export: %v", err)
		}

		for _, want := range []string{
			"$ORIGIN imported.example.",
			"imported.example.\t3600\tIN\tSOA",
			"www.imported.example.\t3600\tIN\tA\t192.0.2.10",
			"sub.imported.example.\t3600\tIN\tNS",
		} {
			if !strings.Contains(string(body), want) {
				t.Errorf("the export is missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(string(body), "buried") {
			t.Error("the export carries a record the import left out")
		}
	})

	t.Run("importing it a second time is refused", func(t *testing.T) {
		// A file is the complete contents of a zone, so this would be a
		// replacement, and doing it silently is the difference between gaining
		// a zone and losing one.
		resp := h.doText(http.MethodPost, "/zones/import", importable)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("a file that reads this server's filesystem is refused", func(t *testing.T) {
		resp := h.doText(http.MethodPost, "/zones/import",
			"$ORIGIN evil.example.\n"+
				"@ IN SOA ns1.evil.example. hostmaster.evil.example. 1 7200 900 1209600 3600\n"+
				"@ IN NS ns1.evil.example.\n$INCLUDE /etc/passwd\n")
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", resp.StatusCode)
		}
	})

	t.Run("a fragment with no SOA is refused", func(t *testing.T) {
		resp := h.doText(http.MethodPost, "/zones/import", "www.nowhere.example. IN A 192.0.2.1\n")
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", resp.StatusCode)
		}
	})
}

func TestExportRoundTripsThroughImport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	var first gen.ZoneImported
	h.decode(h.doText(http.MethodPost, "/zones/import", importable), http.StatusCreated, &first)

	exported, err := io.ReadAll(h.do(http.MethodGet, "/zones/"+first.Zone.Id+"/export", nil).Body)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}

	// Into a second server, so that what comes out of one goes into another —
	// which is the whole claim an export makes.
	other := newHarness(t)
	var second gen.ZoneImported
	other.decode(other.doText(http.MethodPost, "/zones/import", string(exported)),
		http.StatusCreated, &second)

	if second.Records != first.Records {
		t.Errorf("the round trip wrote %d records, started with %d", second.Records, first.Records)
	}
	if deref(second.Zone.Soa.Serial, 0) != deref(first.Zone.Soa.Serial, 0) {
		t.Errorf("the serial changed on the way through: %d then %d",
			deref(first.Zone.Soa.Serial, 0), deref(second.Zone.Soa.Serial, 0))
	}
	if second.Skipped != nil {
		t.Errorf("the export carried records the import then left out: %v", second.Skipped)
	}
}

// doText sends a body that is not JSON, which is what a zonefile is.
func (h *harness) doText(method, path, body string) *http.Response {
	h.t.Helper()

	req, err := http.NewRequestWithContext(h.t.Context(), method,
		h.http.URL+basePath+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "text/dns")

	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The metrics travel over the same API as everything else, so that there is
// one socket, one credential and one thing to expose. What a server is asked
// and how many zones it holds is operational detail; a scraper can carry a
// token, and Prometheus has had `authorization` in its scrape configuration
// for years.
func TestMetrics(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// What the numbers say is the metrics package's business and the wiring's;
	// what this endpoint owes is the format, the header and the credential.
	// Whether the counters actually move is checked where the query path and
	// the metrics are wired together, in TestServeExportsMetrics.
	t.Run("exports in the format a scraper reads", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/metrics", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		// A scraper reads which version of the exposition format it is looking
		// at out of this header, so it is not merely "text/plain".
		if got := resp.Header.Get("Content-Type"); got != metrics.ContentType {
			t.Errorf("Content-Type = %q, want %q", got, metrics.ContentType)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read the body: %v", err)
		}
		out := string(body)
		for _, want := range []string{
			"# TYPE weg_dns_query_duration_seconds histogram",
			"weg_snapshot_zones",
			"weg_build_info{",
			"go_goroutines",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the exposition does not mention %q", want)
			}
		}
	})

	t.Run("needs a token", func(t *testing.T) {
		noToken := func(r *http.Request) { r.Header.Del("Authorization") }
		resp := h.do(http.MethodGet, "/metrics", nil, noToken)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// sseReader reads Server-Sent Events off a live response.
type sseReader struct {
	t    *testing.T
	body io.ReadCloser
	scan *bufio.Scanner
}

// next reads one event, failing the test rather than hanging if none arrives.
func (r *sseReader) next() (name string, data []byte) {
	r.t.Helper()

	for r.scan.Scan() {
		line := r.scan.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = []byte(strings.TrimPrefix(line, "data: "))
		case line == "":
			if name != "" {
				return name, data
			}
		}
	}
	r.t.Fatalf("the stream ended before an event arrived: %v", r.scan.Err())
	return "", nil
}

// watch opens the query stream with the harness's token.
func (h *harness) watch(query string) *sseReader {
	h.t.Helper()

	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet,
		h.http.URL+basePath+"/queries/stream"+query, http.NoBody)
	if err != nil {
		h.t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)

	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatalf("open the stream: %v", err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		h.t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	return &sseReader{t: h.t, body: resp.Body, scan: bufio.NewScanner(resp.Body)}
}

// observed is one exchange for the stream to carry.
func observed(shape ...func(*dns.Event)) dns.Event {
	ev := dns.Event{
		At:        time.Now(),
		Latency:   250 * time.Microsecond,
		Client:    netip.MustParseAddrPort("192.0.2.50:41234"),
		Transport: dns.UDP,
		Name:      "WwW.example.com.",
		Type:      zone.TypeA,
		Class:     zone.ClassIN,
		Size:      100,
	}
	for _, s := range shape {
		s(&ev)
	}
	return ev
}

func TestStreamQueries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	t.Run("carries the exchanges as they are answered", func(t *testing.T) {
		w := h.watch("")

		// A watcher is told what it is being shown before it is shown
		// anything: on a quiet server the first exchange may be minutes away,
		// and until then a stream that said nothing would be one that could
		// not be told apart from a broken one.
		name, data := w.next()
		if name != "status" {
			t.Fatalf("the first event is %q, want status", name)
		}
		var st gen.StreamStatus
		if err := json.Unmarshal(data, &st); err != nil {
			t.Fatalf("decode the status: %v", err)
		}
		if st.Ratio != 1 {
			t.Errorf("ratio = %d before anything happened, want 1", st.Ratio)
		}

		h.stream.Observe(observed())

		name, data = w.next()
		if name != "query" {
			t.Fatalf("event = %q, want query", name)
		}
		var ev gen.QueryEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("decode the event: %v", err)
		}
		// The casing is the client's, not the server's: 0x20 encoding means a
		// real query is rarely lowercase, and somebody watching is looking for
		// the query they sent.
		if ev.Name != "WwW.example.com." || ev.Type != "A" || ev.Rcode != "NOERROR" {
			t.Errorf("event = %s %s %s, want the exchange that was observed",
				ev.Name, ev.Type, ev.Rcode)
		}
		if ev.Client != "192.0.2.50" || ev.Port == nil || *ev.Port != 41234 {
			t.Errorf("client = %s:%v, want 192.0.2.50:41234", ev.Client, ev.Port)
		}
		if ev.Transport != "udp" || ev.LatencyUs != 250 || ev.Size != 100 {
			t.Errorf("event = %s %dus %d octets, want udp 250us 100", ev.Transport, ev.LatencyUs, ev.Size)
		}
	})

	t.Run("filters server-side", func(t *testing.T) {
		w := h.watch("?name=wanted.example.com.&type=AAAA")
		if name, _ := w.next(); name != "status" {
			t.Fatalf("the first event is %q, want status", name)
		}

		// Three exchanges the filter excludes, one it does not. If the filter
		// were applied anywhere but before the buffer, the first of these
		// would arrive.
		h.stream.Observe(observed())
		h.stream.Observe(observed(func(e *dns.Event) { e.Name = "wanted.example.com." }))
		h.stream.Observe(observed(func(e *dns.Event) { e.Type = zone.TypeAAAA }))
		h.stream.Observe(observed(func(e *dns.Event) {
			e.Name = "wanted.example.com."
			e.Type = zone.TypeAAAA
		}))

		name, data := w.next()
		if name != "query" {
			t.Fatalf("event = %q, want query", name)
		}
		var ev gen.QueryEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("decode the event: %v", err)
		}
		if ev.Name != "wanted.example.com." || ev.Type != "AAAA" {
			t.Errorf("event = %s %s, want the only exchange matching both", ev.Name, ev.Type)
		}
	})

	t.Run("a filter it cannot read is the client's mistake", func(t *testing.T) {
		for _, q := range []string{
			"?type=NOTATYPE",
			"?rcode=NOTANRCODE",
			"?client=notanaddress",
			"?name=" + strings.Repeat("a", 300),
		} {
			resp := h.do(http.MethodGet, "/queries/stream"+q, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
			}
		}
	})

	t.Run("needs a token", func(t *testing.T) {
		noToken := func(r *http.Request) { r.Header.Del("Authorization") }
		resp := h.do(http.MethodGet, "/queries/stream", nil, noToken)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// Every watcher costs the query path a filter evaluation per query, so the
// bound is on the data plane rather than on this endpoint, which is why the
// answer past it says "come back", not "you did something wrong".
func TestStreamRefusesTooManyWatchers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	for range 3 {
		h.watch("")
	}

	resp := h.do(http.MethodGet, "/queries/stream", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var problem gen.Problem
	h.decode(resp, http.StatusServiceUnavailable, &problem)
	if problem.Type != typeUnavailable {
		t.Errorf("type = %q, want %q", problem.Type, typeUnavailable)
	}
}

// Under load the sampling ratio moves with every few queries, because it is
// worked out from how full the current second already is. A status message per
// change would be a flood of its own (measured against a real one, three
// hundred lines for a three-second flood) so they are throttled, except for
// the moment sampling starts.
func TestStreamStatusIsThrottled(t *testing.T) {
	t.Parallel()

	last := stream.Stats{Ratio: 1}

	// How long ago the last status went out, rather than when. The table is
	// built once and the subtests run later: an absolute "now" here means
	// "just now" turns into "a second ago" on a machine under load, and the
	// throttle then fires in a case that asked it not to.
	const (
		justNow = time.Duration(0)
		aWhile  = time.Hour
	)

	tests := []struct {
		name  string
		last  stream.Stats
		got   stream.Stats
		since time.Duration
		want  bool
	}{
		{"nothing changed", last, last, aWhile, false},
		{"sampling started", last, stream.Stats{Ratio: 2}, justNow, true},
		{"sampling stopped", stream.Stats{Ratio: 50}, last, justNow, true},
		{"the ratio moved, and it was just said", stream.Stats{Ratio: 50}, stream.Stats{Ratio: 51}, justNow, false},
		{"the ratio moved, and it has been a while", stream.Stats{Ratio: 50}, stream.Stats{Ratio: 51}, aWhile, true},
		{"something was dropped, and it has been a while", last, stream.Stats{Ratio: 1, Dropped: 3}, aWhile, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			said := time.Now().Add(-tc.since)
			if got := statusDue(tc.last, tc.got, said); got != tc.want {
				t.Errorf("statusDue = %v, want %v", got, tc.want)
			}
		})
	}
}

// D5 promised a "last used" for every token and nothing wrote it. The field is
// what an operator reads before revoking something, so a token in daily use
// reading "never" is not a missing feature but a wrong answer.
func TestTokenLastUsedIsRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// Two requests with the token, so that what is written is the later one
	// rather than one row per request.
	h.do(http.MethodGet, "/zones", nil)
	// Truncated because the store keeps timestamps as UnixMilli. Comparing a
	// nanosecond reading against a millisecond one fails whenever the request
	// lands in the same millisecond.
	before := time.Now().Truncate(time.Millisecond)
	h.do(http.MethodGet, "/zones", nil)

	// Nothing is written yet: the point of collecting them is that a request
	// does not pay for the write.
	if got := h.tokenNamed(t, "bootstrap").LastUsedAt; got != nil {
		t.Errorf("last used = %v before a flush, want it still pending", got)
	}

	h.api.flushTokenUse(t.Context())

	used := h.tokenNamed(t, "bootstrap").LastUsedAt
	if used == nil {
		t.Fatal("last used is still empty after a flush")
	}
	if used.Before(before) {
		t.Errorf("last used = %v, want the later of the two requests (after %v)", used, before)
	}
}

// tokenNamed reads one token out of the listing.
func (h *harness) tokenNamed(t *testing.T, name string) gen.Token {
	t.Helper()

	var tokens []gen.Token
	h.decode(h.do(http.MethodGet, "/tokens", nil), http.StatusOK, &tokens)
	for _, tok := range tokens {
		if tok.Name == name {
			return tok
		}
	}
	t.Fatalf("no token named %q", name)
	return gen.Token{}
}

// Opening the interface is not a failed authentication.
func TestUnauthenticatedReadsDoNotSpendTheLimiter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// Comfortably past authBurst.
	for i := range authBurst * 3 {
		resp := h.do(http.MethodGet, "/auth/session", nil, func(r *http.Request) {
			r.Header.Del("Authorization")
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("load %d: status %d, want 401", i, resp.StatusCode)
		}
	}

	// And the credential that was never wrong still works.
	resp := h.do(http.MethodPost, "/auth/session", map[string]string{"token": h.token},
		func(r *http.Request) { r.Header.Del("Authorization") })
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signing in afterwards: status %d, want 201", resp.StatusCode)
	}
}

// A token that is actually wrong still costs, which is what the limiter is for.
func TestRejectedTokensStillSpendTheLimiter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	wrong := func() int {
		resp := h.do(http.MethodGet, "/zones", nil, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+TokenPrefix+"nope")
		})
		return resp.StatusCode
	}

	var limited bool
	for range authBurst * 2 {
		if wrong() == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("guessing %d times was never refused", authBurst*2)
	}
}

// A network is a zone name a person should never have to work out.
//
// 192.168.0.0/16 is 168.192.in-addr.arpa, 192.0.2.0/25 is the classless form
// of RFC 2317, and an IPv6 prefix is nibbles written backwards. The server has
// always known how to derive all three; this is that knowledge reaching the
// API.
func TestCreateZoneFromANetwork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	tests := []struct {
		given string
		want  string
	}{
		{"192.168.0.0/24", "0.168.192.in-addr.arpa."},
		{"192.168.0.0/16", "168.192.in-addr.arpa."},
		{"10.0.0.0/8", "10.in-addr.arpa."},
		// RFC 2317 §4: a prefix that stops inside an octet.
		{"192.0.2.0/25", "0/25.2.0.192.in-addr.arpa."},
		{"2001:db8::/32", "8.b.d.0.1.0.0.2.ip6.arpa."},
		// Masked on the way in, so an address inside the network works too.
		{"192.168.7.99/24", "7.168.192.in-addr.arpa."},
	}

	for _, tc := range tests {
		t.Run(tc.given, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/zones", map[string]any{"name": tc.given})

			var got gen.Zone
			h.decode(resp, http.StatusCreated, &got)
			if got.Name != tc.want {
				t.Errorf("%s became %q, want %q", tc.given, got.Name, tc.want)
			}
			if got.Kind != gen.Reverse {
				t.Errorf("%s became a %s zone, want reverse", tc.given, got.Kind)
			}
		})
	}
}

// The two things that are nearly right, refused with what to do about them.
func TestCreateZoneFromANetworkRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	tests := []struct {
		name  string
		given string
		want  string
	}{
		{
			// ip6.arpa divides into nibbles and has nothing finer to offer.
			name:  "an IPv6 prefix off a nibble boundary",
			given: "2001:db8::/33",
			want:  "/32 or /36",
		},
		{
			// It parses as a name, all-numeric labels are legal, so without
			// catching it this would quietly make a forward zone.
			name:  "a bare address",
			given: "192.168.5.0",
			want:  "192.168.5.0/24",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/zones", map[string]any{"name": tc.given})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read the body: %v", err)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, body)
			}
		})
	}
}
