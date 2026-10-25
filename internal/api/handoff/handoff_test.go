package handoff

import (
	"testing"
	"time"
)

func TestASessionOpensUnderTheKeyItWasSealedTo(t *testing.T) {
	t.Parallel()
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	want := Session{Value: "v", CSRF: "c", Expires: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	sealed, err := Seal(k.Public(), &want)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := k.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if *got != want {
		t.Errorf("opened %+v, want %+v", *got, want)
	}

	other, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Error("another sign-in's key opened the session")
	}
}

func TestAKeyThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()
	for _, public := range []string{"", "not base64!", "AAAA"} {
		if ValidPublic(public) {
			t.Errorf("ValidPublic(%q) = true", public)
		}
		if _, err := Seal(public, &Session{}); err == nil {
			t.Errorf("Seal to %q succeeded", public)
		}
	}
}
