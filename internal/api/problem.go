package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/wegweiserzone/wegweiser/internal/api/gen"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// problemContentType is the media type RFC 9457 assigns. It is not
// application/json, and that matters: a client seeing it knows the body is a
// failure without having to guess from the status code alone.
const problemContentType = "application/problem+json"

// Problem types. They are URI references rather than free text so that a client
// can branch on one without matching English, and they are stable: the wording
// of a title may improve, the type may not change.
const (
	typeInvalid      = "/problems/invalid-request"
	typeNotFound     = "/problems/not-found"
	typeConflict     = "/problems/conflict"
	typeUnauthorized = "/problems/unauthorized"
	typeForbidden    = "/problems/forbidden"
	typeRateLimited  = "/problems/too-many-requests"
	typeUnavailable  = "/problems/unavailable"
	typeInternal     = "/problems/internal"
)

// apiError is a failure on its way to becoming a problem document.
type apiError struct {
	status int
	kind   string
	title  string
	detail string
	// cause is kept for the server's own error reporting and never reaches the
	// client, because an internal failure's text is not a client's business.
	cause error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.title, e.detail, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.title, e.detail)
}

func (e *apiError) Unwrap() error { return e.cause }

// badRequest reports something the client sent that cannot be used.
func badRequest(format string, args ...any) *apiError {
	return &apiError{
		status: http.StatusBadRequest,
		kind:   typeInvalid,
		title:  "The request could not be used",
		detail: fmt.Sprintf(format, args...),
	}
}

// conflict reports a write that collided with what is already stored.
func conflict(detail string) *apiError {
	return &apiError{
		status: http.StatusConflict,
		kind:   typeConflict,
		title:  "Conflict",
		detail: detail,
	}
}

// internal reports a failure that is the server's own.
func internal(err error) *apiError {
	return &apiError{
		status: http.StatusInternalServerError,
		kind:   typeInternal,
		title:  "The server could not carry out the request",
		detail: "something failed inside the server; the operator's log has it",
		cause:  err,
	}
}

// asProblem turns any error into the document that describes it.
func asProblem(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		return &apiError{
			status: http.StatusNotFound,
			kind:   typeNotFound,
			title:  "Not found",
			detail: err.Error(),
		}
	case errors.Is(err, store.ErrConflict):
		return conflict(err.Error())
	case errors.Is(err, zone.ErrInvalid):
		// The zone package validates what DNS itself requires, so its
		// rejections are about the request and are safe to quote in full.
		return &apiError{
			status: http.StatusUnprocessableEntity,
			kind:   typeInvalid,
			title:  "The request was understood but cannot be carried out",
			detail: err.Error(),
		}
	default:
		return internal(err)
	}
}

// document renders the problem in the shape the spec describes.
func (e *apiError) document(instance string) gen.Problem {
	p := gen.Problem{
		Type:   e.kind,
		Title:  e.title,
		Status: e.status,
	}
	if e.detail != "" {
		p.Detail = &e.detail
	}
	if instance != "" {
		p.Instance = &instance
	}
	return p
}

// writeProblem sends a failure to the client.
func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	ae := asProblem(err)

	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(ae.status)

	// A failed write means the client hung up mid-response. The status line is
	// already gone, so there is nothing left to say and nobody to say it to.
	if err := json.NewEncoder(w).Encode(ae.document(r.URL.Path)); err != nil {
		return
	}
}
