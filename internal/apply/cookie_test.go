package apply_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wegweiserzone/wegweiser/internal/apply"
	"github.com/wegweiserzone/wegweiser/internal/store"
	"github.com/wegweiserzone/wegweiser/internal/zone"
)

func TestCookieSecretsRoundTripThroughTheStore(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	var got apply.CookieSecrets
	read := func() {
		t.Helper()
		if err := f.s.View(t.Context(), func(r store.Reader) error {
			var verr error
			got, verr = apply.StoredCookieSecrets(t.Context(), r)
			return verr
		}); err != nil {
			t.Fatalf("StoredCookieSecrets: %v", err)
		}
	}

	// A server that has never answered a query has no secret. It is minted on
	// the way up, not at build time, so there is nothing to read yet.
	read()
	if !got.IsZero() {
		t.Fatalf("a fresh database already holds a cookie secret")
	}

	at := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	want, err := apply.NewCookieSecrets(at)
	if err != nil {
		t.Fatalf("NewCookieSecrets: %v", err)
	}
	change, cerr := apply.CookieSecretsChange(want)
	if cerr != nil {
		t.Fatalf("CookieSecretsChange: %v", cerr)
	}
	setSetting(t, f, change)

	read()
	if got.Current != want.Current {
		t.Errorf("stored %x, read back %x", want.Current, got.Current)
	}
	if !got.Previous.IsZero() {
		t.Errorf("a server that never rotated has a previous secret %x", got.Previous)
	}
	if !got.RotatedAt.Equal(at) {
		t.Errorf("minted at %s, read back %s", at, got.RotatedAt)
	}

	// Rotating keeps the secret every cookie in flight was built under, or
	// every client holding one pays a round trip at the same moment.
	rotated, rerr := want.Rotate(at.Add(time.Hour))
	if rerr != nil {
		t.Fatalf("Rotate: %v", rerr)
	}
	change, cerr = apply.CookieSecretsChange(rotated)
	if cerr != nil {
		t.Fatalf("CookieSecretsChange: %v", cerr)
	}
	setSetting(t, f, change)

	read()
	if got.Current != rotated.Current || got.Previous != want.Current {
		t.Errorf("after rotating: current %x previous %x, want %x and %x",
			got.Current, got.Previous, rotated.Current, want.Current)
	}
	if got.Current == got.Previous {
		t.Error("rotating minted the secret it replaced")
	}
}

func TestCookieSecretsAreRefusedWhenUnreadable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		bad  string
	}{
		{name: "not hex", raw: `{"current":"zzzz"}`, bad: "not hex"},
		{
			name: "the wrong length hashes to something, which is worse",
			raw:  `{"current":"e5e973e5a6b2a43f48e7dc84"}`,
			bad:  "12 octets",
		},
		{name: "nothing current", raw: `{"previous":""}`, bad: "no current secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			setSetting(t, f, apply.SettingChange{
				Key: apply.CookieSecretSetting, Value: []byte(tt.raw),
			})

			err := f.s.View(t.Context(), func(r store.Reader) error {
				_, verr := apply.StoredCookieSecrets(t.Context(), r)
				return verr
			})
			if err == nil {
				t.Fatalf("%s was read as a usable secret", tt.raw)
			}
			if !strings.Contains(err.Error(), tt.bad) {
				t.Errorf("the refusal says %q, which does not mention %q", err, tt.bad)
			}
		})
	}
}

func TestACookieSecretIsWrittenAsHex(t *testing.T) {
	t.Parallel()

	// The vector from RFC 9018 Appendix A, so that a secret in the database
	// reads the way the specification writes one.
	const secret = "e5e973e5a6b2a43f48e7dc849e37bfcf"

	var s apply.CookieSecret
	if err := json.Unmarshal([]byte(`"`+secret+`"`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `"`+secret+`"` {
		t.Errorf("wrote %s, want %q", raw, secret)
	}

	err = json.Unmarshal([]byte(`"e5e973"`), &s)
	if !errors.Is(err, zone.ErrInvalid) {
		t.Errorf("a short secret gave %v, want an invalid-value error", err)
	}
}
