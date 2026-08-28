// Package api is the HTTP interface every Wegweiser client speaks.
//
// The web interface and the weg command line are both clients of it, and
// neither reaches the database any other way (architecture invariant 1). What
// the API accepts is described in openapi.yaml, which is the source of truth:
// the models, the server interface and the client in gen/ are all generated
// from it, so an endpoint that is not written down there does not exist.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/buildinfo"
	"github.com/wegweiserzone/wegweiser/internal/dns"
	"github.com/wegweiserzone/wegweiser/internal/metrics"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/stream"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// basePath is where the API is mounted. It carries its version, so that a
// second one can exist beside it rather than instead of it.
const basePath = "/api/v1"

// healthPath is the one endpoint that needs no credential.
const healthPath = "/healthz"

// requestTimeout bounds how long a request may occupy a handler. Every read
// this API makes is one indexed query, so a request that reaches this has hit
// something pathological rather than something slow.
const requestTimeout = 30 * time.Second

// maxRequestBody is the largest body accepted. A record is a line of text and a
// zone is a handful of fields; anything larger is a mistake or an attempt to
// make the server hold something for a while.
const maxRequestBody = 1 << 20 // 1 MiB

// maxImportBody is what a zonefile may be. A record is a line, so a zone of a
// hundred thousand records is a few megabytes and the ordinary limit would
// refuse every migration worth making. It is still a limit: what the file
// *becomes* is bounded separately, because thirty octets of $GENERATE expand
// to millions of records.
const maxImportBody = 32 << 20 // 32 MiB

// Snapshots is the data plane, as far as the API needs to know about it: it
// reports what is being answered from, and it takes what should be answered
// from next. A *dns.Server is one.
//
// It is an interface so that the API can be tested without a socket, and so
// that the coupling stays the one pointer architecture invariant 2 allows.
type Snapshots interface {
	Snapshot() *dns.Snapshot
	SetSnapshot(*dns.Snapshot)
}

// TransferList is the data plane's view of who may pull a whole zone. A
// *dns.Server is one.
type TransferList interface {
	SetTransfers(dns.Transfers)
}

// Keyring is the query path's copy of the TSIG keys. A key has to reach the
// server that verifies signatures with it, or a secondary configured with one
// created a moment ago would be refused until the next restart.
type Keyring interface {
	SetKeys(dns.Keyring)
}

// Notifier tells the secondaries that a zone has a new version, and holds the
// list of who they are. A *dns.Notifier is one.
type Notifier interface {
	Notify(snap *dns.Snapshot, apex zone.Name)
	SetTargets(targets []dns.NotifyTarget)
}

// Config is what a [Server] needs.
type Config struct {
	// Store is the source of truth. It is read directly and written only
	// through the applier.
	Store store.Store

	// Applier is the write path. Every change goes through it, so that no
	// write bypasses the journal (architecture invariant 4).
	Applier *apply.Applier

	// Snapshots is the data plane whose view is republished after every write.
	// A nil one means the API is running without a query path, which is what a
	// test does; writes then change the database and nothing else.
	Snapshots Snapshots

	// Transfers is the query path's list of who may pull a whole zone. The
	// setting lives in the database, so a change has to reach the server that
	// enforces it; a *dns.Server is one. May be nil, which is what a test that
	// runs without a query path passes.
	Transfers TransferList

	// Keyring is where the TSIG keys are published after one is created or
	// withdrawn. May be nil, which is what a test without a query path passes.
	Keyring Keyring

	// Notifier tells the secondaries a zone changed, once the snapshot saying
	// so has been published (docs/decisions/d27-notify.md). May be nil, and then
	// nobody is told and a secondary waits out its refresh timer.
	Notifier Notifier

	// Metrics is what /metrics exports. It is required rather than optional:
	// an endpoint that answers with an empty registry when the process forgot
	// to build one would report a server that answers no queries, which is
	// worse than no endpoint at all.
	Metrics *metrics.Metrics

	// Stream is what the live query stream subscribes to. Required for the
	// same reason as Metrics: an endpoint that opens a stream nothing feeds
	// looks like a server nobody is querying.
	Stream *stream.Hub

	// UI decides whether the embedded web interface is served alongside the
	// API. False serves only the API and answers everything else with a
	// problem document saying so (docs/decisions/ D16).
	UI bool

	// OnError is called for failures the client is not told the detail of, so
	// that they reach an operator instead of nobody. It may be nil.
	OnError func(error)

	// Now supplies the current time. Nil picks [time.Now]; tests set it.
	Now func() time.Time
}

// Server implements the generated API.
type Server struct {
	store     store.Store
	applier   *apply.Applier
	snapshots Snapshots
	transfers TransferList
	keyring   Keyring
	notifier  Notifier
	metrics   *metrics.Metrics
	stream    *stream.Hub
	onError   func(error)
	now       func() time.Time

	limiter  *authLimiter
	sessions *sessionStore

	// ui is the embedded web interface, or nil when it is switched off.
	ui http.Handler

	// tokenUse collects which tokens have authenticated a request, written
	// back in one transaction rather than one per request. See lastused.go.
	tokenUse  *tokenUse
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// compile-time proof that every operation in the spec has an implementation.
var _ gen.StrictServerInterface = (*Server)(nil)

// New returns a server and the handler that serves it.
func New(cfg Config) (*Server, http.Handler, error) {
	if cfg.Store == nil {
		return nil, nil, errors.New("api: no store given")
	}
	if cfg.Applier == nil {
		return nil, nil, errors.New("api: no applier given")
	}
	if cfg.Metrics == nil {
		return nil, nil, errors.New("api: no metrics given")
	}
	if cfg.Stream == nil {
		return nil, nil, errors.New("api: no query stream given")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	s := &Server{
		store:     cfg.Store,
		applier:   cfg.Applier,
		snapshots: cfg.Snapshots,
		transfers: cfg.Transfers,
		keyring:   cfg.Keyring,
		notifier:  cfg.Notifier,
		metrics:   cfg.Metrics,
		stream:    cfg.Stream,
		onError:   cfg.OnError,
		now:       cfg.Now,
		limiter:   newAuthLimiter(),
		sessions:  newSessionStore(),
		tokenUse:  newTokenUse(),
		done:      make(chan struct{}),
	}
	if cfg.UI {
		ui, err := newWebUI()
		if err != nil {
			return nil, nil, err
		}
		s.ui = ui
	}

	s.wg.Add(1)
	go s.recordUse()

	handler := s.handler()
	return s, handler, nil
}

// handler builds what the listener serves.
//
// Two things live behind one socket: the API under its versioned prefix, and
// the web interface under everything else. They are siblings rather than
// nested, because the interface must load before anybody has authenticated —
// the login screen is the thing that gets you a credential, and the API's
// middleware chain demands one on every request it sees.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(basePath+"/", s.apiHandler())
	if s.ui != nil {
		mux.Handle("/", s.ui)
	} else {
		mux.HandleFunc("/", uiOff)
	}
	return mux
}

// apiHandler builds the API router.
func (s *Server) apiHandler() http.Handler {
	r := chi.NewRouter()
	// No RealIP here on purpose. It rewrites RemoteAddr from headers the
	// client sets, which would hand the rate limiter of D5 to whoever it is
	// meant to limit. A deployment behind a proxy needs a configured trust
	// list, which is an operator's decision rather than a default.
	r.Use(
		middleware.RequestID,
		s.recoverer,
		boundedTime,
		limitBody,
		withFacts,
		s.authenticator,
	)

	strict := gen.NewStrictHandlerWithOptions(s, nil, gen.StrictHTTPServerOptions{
		// A body that does not match the spec is the client's mistake, and an
		// answer it cannot decode is not an improvement on the one it sent.
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, r, badRequest("%v", err))
		},
		// An error out of a handler is classified before it is answered with.
		// Wrapping every one of them as internal would turn "no such zone"
		// into a server fault, and bury the one case that really is one.
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			if asProblem(err).cause != nil {
				s.report(err)
			}
			writeProblem(w, r, err)
		},
	})

	return gen.HandlerWithOptions(strict, gen.ChiServerOptions{
		BaseURL:    basePath,
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeProblem(w, r, badRequest("%v", err))
		},
	})
}

// recoverer turns a panic in a handler into a failed request rather than a
// dead process. The API is the control plane: losing it takes the operator's
// only way to fix whatever caused the panic.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec) // the server's own signal, not a failure
				}
				err := fmt.Errorf("api: panic in %s %s: %v", r.Method, r.URL.Path, rec)
				s.report(err)
				writeProblem(w, r, internal(err))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// boundedTime caps how long a request may occupy a handler.
func boundedTime(next http.Handler) http.Handler {
	bounded := middleware.Timeout(requestTimeout)(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == streamPath {
			next.ServeHTTP(w, r)
			return
		}
		bounded.ServeHTTP(w, r)
	})
}

// limitBody caps what a request may send, before anything tries to read it.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxRequestBody)
		if r.URL.Path == importPath {
			limit = maxImportBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// report hands a failure to the operator, if anybody is listening.
func (s *Server) report(err error) {
	if s.onError != nil && err != nil {
		s.onError(err)
	}
}

// GetHealth reports whether the server is fit to answer queries.
func (s *Server) GetHealth(_ context.Context, _ gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	info := buildinfo.Get()

	var snap *dns.Snapshot
	if s.snapshots != nil {
		snap = s.snapshots.Snapshot()
	}
	if snap == nil {
		detail := "no snapshot has been published, so there is nothing to answer from yet"
		return gen.GetHealth503ApplicationProblemPlusJSONResponse{
			Type:   typeInternal,
			Title:  "Not ready",
			Status: http.StatusServiceUnavailable,
			Detail: &detail,
		}, nil
	}

	return gen.GetHealth200JSONResponse{
		Status:  gen.Serving,
		Version: info.Version,
		Zones:   snap.Zones(),
		Records: snap.Records(),
	}, nil
}
