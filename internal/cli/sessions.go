package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The cookie and the header a session travels in. They are repeated from
// internal/api rather than imported, as basePath is: a client should not need
// the server to know where to knock.
const (
	sessionCookie = "weg_session"
	csrfCookie    = "weg_csrf"
	csrfHeader    = "X-Wegweiser-CSRF"
)

// sessionsFile is where weg login keeps its sessions, below the directory
// weg keeps its files in.
const sessionsFile = "sessions.json"

// heldSession is a session weg login opened, as the file keeps it.
type heldSession struct {
	// Value is the sealed session, sent back as the cookie, and CSRF the
	// value a write repeats in the header. Both are the server's and are
	// carried, never read.
	Value string `json:"session"`
	CSRF  string `json:"csrf"`

	// Name is who signed in, for weg logout to say whom it signed out.
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
}

// attach puts the session on a request, as a browser would send it.
func (h *heldSession) attach(r *http.Request) {
	//nolint:gosec // G124: a cookie sent back, not set; its attributes were the server's to give
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: h.Value})
	r.Header.Set(csrfHeader, h.CSRF)
}

// heldSessions is the file: one session per server address.
type heldSessions struct {
	Servers map[string]heldSession `json:"servers"`
}

// sessionsPath is the file the sessions are kept in.
func (o *options) sessionsPath() (string, error) {
	dir := ""
	if o != nil {
		dir = o.configDir
	}
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("no directory to keep a session in: %w", err)
		}
		dir = filepath.Join(base, "wegweiser")
	}
	return filepath.Join(dir, sessionsFile), nil
}

// readSessions reads the file, which is empty when there is none.
func (o *options) readSessions() (heldSessions, string, error) {
	held := heldSessions{Servers: map[string]heldSession{}}
	path, err := o.sessionsPath()
	if err != nil {
		return held, "", err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return held, path, nil
	}
	if err != nil {
		return held, path, fmt.Errorf("read the sessions weg login kept: %w", err)
	}
	if err := json.Unmarshal(raw, &held); err != nil {
		return held, path, fmt.Errorf("%s is not a file weg login wrote: %w", path, err)
	}
	if held.Servers == nil {
		held.Servers = map[string]heldSession{}
	}
	return held, path, nil
}

// writeSessions replaces the file with what is held, less what has run out.
// The directory is the owner's alone and the file readable by nobody else,
// because what it holds signs in as the person (D52). It is written beside
// itself and renamed into place, so that a reader never finds half of it.
func writeSessions(path string, held heldSessions, now time.Time) error {
	for server, h := range held.Servers {
		if !now.Before(h.Expires) {
			delete(held.Servers, server)
		}
	}
	raw, err := json.MarshalIndent(held, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if merr := os.MkdirAll(dir, 0o700); merr != nil {
		return fmt.Errorf("make the directory to keep the session in: %w", merr)
	}
	tmp, err := os.CreateTemp(dir, sessionsFile+".*")
	if err != nil {
		return fmt.Errorf("keep the session: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("keep the session: %w", err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("keep the session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keep the session: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("keep the session: %w", err)
	}
	return nil
}

// sessionAt is the session weg login opened at this address, if it is
// still good.
func (o *options) sessionAt(server string, now time.Time) (*heldSession, error) {
	held, _, err := o.readSessions()
	if err != nil {
		return nil, err
	}
	h, ok := held.Servers[server]
	switch {
	case !ok:
		return nil, usageError{fmt.Errorf(
			"not signed in at %s, and no API token: run weg login, pass --token, or put a "+
				"token in $%s. The first start of weg serve prints one", server, tokenEnv)}
	case !now.Before(h.Expires):
		return nil, usageError{fmt.Errorf(
			"the session weg login opened at %s ended at %s; run weg login again",
			server, h.Expires.Local().Format(time.DateTime))}
	}
	return &h, nil
}
