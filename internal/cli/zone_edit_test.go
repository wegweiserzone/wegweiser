package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// Changing a zone is reachable from the command line at all.
//
// The API has had PATCH /zones/{zoneId} since the beginning and only the web
// interface used it, which is architecture invariant 1 broken: no feature
// exists in only one client.
func TestZoneUpdate(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com", "--ttl", "3600")

	t.Run("with nothing to change it says so", func(t *testing.T) {
		code, _, errOut := run(t, srv, "zone", "update", "example.com")
		if code == ExitOK {
			t.Fatal("an empty change was accepted")
		}
		if !strings.Contains(errOut, "nothing to change") {
			t.Errorf("stderr = %q, want it to say what to pass", errOut)
		}
	})

	t.Run("one commit, one serial", func(t *testing.T) {
		out := mustRun(t, srv, "zone", "update", "example.com", "--ttl", "300")
		if !strings.Contains(out, "1 → 2") {
			t.Errorf("output = %q, want the serial to advance by exactly one (D2)", out)
		}

		shown := mustRun(t, srv, "zone", "show", "example.com")
		if !strings.Contains(shown, "300") {
			t.Errorf("the zone still reads %q, want the new default TTL", shown)
		}
	})

	t.Run("an address becomes a mailbox", func(t *testing.T) {
		mustRun(t, srv, "zone", "update", "example.com", "--email", "first.last@example.com")

		shown := mustRun(t, srv, "zone", "show", "example.com")
		// RFC 1035 §3.3.13: the @ becomes a dot and the dot inside the local
		// part is escaped, or this would claim a mailbox in a host called
		// "first".
		if !strings.Contains(shown, `first\.last.example.com.`) {
			t.Errorf("mailbox not escaped:\n%s", shown)
		}
	})

	t.Run("what is not passed is left alone", func(t *testing.T) {
		mustRun(t, srv, "zone", "update", "example.com", "--comment", "the main zone")

		shown := mustRun(t, srv, "zone", "show", "example.com")
		if !strings.Contains(shown, "the main zone") {
			t.Errorf("the comment did not stick:\n%s", shown)
		}
		if !strings.Contains(shown, "300") {
			t.Errorf("the default TTL was lost by a change that did not mention it:\n%s", shown)
		}
	})
}

// The setting has three states and "server" is not "off": it puts the zone
// back on the server-wide default, so changing that default reaches this zone
// again. A boolean flag could only offer two of the three.
func TestZoneUpdateAutoReverse(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com")

	for _, tc := range []struct {
		given string
		want  string
	}{
		{"on", "on"},
		{"off", "off"},
		{"server", "follows the server"},
	} {
		t.Run(tc.given, func(t *testing.T) {
			mustRun(t, srv, "zone", "update", "example.com", "--auto-reverse", tc.given)

			shown := mustRun(t, srv, "zone", "show", "example.com")
			if !strings.Contains(shown, tc.want) {
				t.Errorf("--auto-reverse %s left the zone reading:\n%s\nwant %q",
					tc.given, shown, tc.want)
			}
		})
	}

	t.Run("anything else is refused with the three words", func(t *testing.T) {
		code, _, errOut := run(t, srv, "zone", "update", "example.com", "--auto-reverse", "maybe")
		if code == ExitOK {
			t.Fatal("\"maybe\" was accepted")
		}
		for _, word := range []string{"on", "off", "server"} {
			if !strings.Contains(errOut, word) {
				t.Errorf("stderr = %q, want it to name %q", errOut, word)
			}
		}
	})
}

func TestZoneEnableDisable(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com")

	out := mustRun(t, srv, "zone", "disable", "example.com")
	if !strings.Contains(out, "disabled example.com.") {
		t.Errorf("output = %q, want it to say what it did", out)
	}

	listed := mustRun(t, srv, "zone", "list")
	if !strings.Contains(listed, "disabled") {
		t.Errorf("the listing does not show it:\n%s", listed)
	}

	// Reversible by running the opposite command, with the records untouched.
	mustRun(t, srv, "zone", "enable", "example.com")
	shown := mustRun(t, srv, "zone", "show", "example.com")
	if !strings.Contains(shown, "enabled") {
		t.Errorf("the zone did not come back:\n%s", shown)
	}
}

func TestZoneUpdateJSON(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com")

	out := mustRun(t, srv, "zone", "update", "example.com", "--ttl", "60", "--output", "json")

	var got struct {
		Zone       string `json:"zone"`
		Serial     int64  `json:"serial"`
		SerialFrom int64  `json:"serialFrom"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Zone != "example.com." || got.Status != "enabled" {
		t.Errorf("got %+v, want the zone and its status", got)
	}
	if got.Serial != got.SerialFrom+1 {
		t.Errorf("serial %d → %d, want exactly one step (D2)", got.SerialFrom, got.Serial)
	}
}
