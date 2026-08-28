package api

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// dist is the built web interface.
//
// It is committed rather than produced during the build, so that `go build`
// and `go install .../cmd/weg@latest` need no Node installed and still yield a
// binary with an interface in it: the same reasoning that applies to the
// generated API code in gen/ (docs/decisions/ D16). `make web` rebuilds it
// and `make web-check` fails when the committed copy has drifted.
//
// The `all:` prefix is load-bearing: without it embed skips names beginning
// with an underscore, so everything under _app would be left out and what
// shipped would be a document referring to files that are not there.
//
//go:embed all:dist
var dist embed.FS

// immutablePrefix is where the build puts the files it names by their content.
// A different byte produces a different name, so those may be held forever;
// everything else has a stable name and must be revalidated.
const immutablePrefix = "_app/immutable/"

// uiCacheImmutable is a year, which is the longest any implementation honours.
const uiCacheImmutable = "public, max-age=31536000, immutable"

// webUI serves the single-page interface out of the embedded filesystem.
type webUI struct {
	files fs.FS
	serve http.Handler
	index []byte
	etag  string
}

// newWebUI prepares the interface for serving.
func newWebUI() (*webUI, error) {
	files, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("api: open the embedded web interface: %w", err)
	}

	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, fmt.Errorf("api: the web interface was not built, run `make web`: %w", err)
	}

	// The document is not named by its content the way the assets are, so it
	// carries a validator of its own and a browser revalidates it on every
	// load. That is one conditional request, and it is what keeps a cached
	// document from naming assets a newer build no longer has.
	sum := sha256.Sum256(index)

	return &webUI{
		files: files,
		serve: http.FileServerFS(files),
		index: index,
		etag:  `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`,
	}, nil
}

func (u *webUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	// The document's own policy is a meta element, and frame-ancestors is
	// ignored there, so this is the only place it can be said at all.
	h.Set("X-Frame-Options", "DENY")
	// A page that can change every zone this server answers for has no reason
	// to tell anybody where its users came from.
	h.Set("Referrer-Policy", "no-referrer")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		writeProblem(w, r, &apiError{
			status: http.StatusMethodNotAllowed,
			kind:   typeNotFound,
			title:  "Not allowed",
			detail: "the web interface is read with GET; everything that changes something is under " + basePath,
		})
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	if u.holds(name) {
		if strings.HasPrefix(name, immutablePrefix) {
			h.Set("Cache-Control", uiCacheImmutable)
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		u.serve.ServeHTTP(w, r)
		return
	}

	// An asset that is not here is a broken build, not a route.
	if strings.HasPrefix(name, "_app/") {
		http.NotFound(w, r)
		return
	}

	u.document(w, r)
}

// holds reports whether the build contains this exact file.
func (u *webUI) holds(name string) bool {
	if name == "" || name == "." {
		return false
	}
	info, err := fs.Stat(u.files, name)
	return err == nil && info.Mode().IsRegular()
}

// document answers with the fallback page, which is what every route the
// client-side router owns resolves to.
func (u *webUI) document(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", u.etag)
	// A zero time leaves Last-Modified off: the embedded files have no
	// meaningful one, and a fabricated date is worse than none. The ETag is
	// what conditional requests are answered from.
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(u.index))
}

// uiOff answers everything outside the API when the interface is switched off.
func uiOff(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, &apiError{
		status: http.StatusNotFound,
		kind:   typeNotFound,
		title:  "The web interface is switched off",
		detail: "this server was started with api.ui set to false, so only the API is served; it is at " + basePath,
	})
}
