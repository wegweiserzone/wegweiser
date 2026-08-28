package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecordAddListDelete(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com", "--ttl", "300")

	t.Run("an empty zone says what to do about it", func(t *testing.T) {
		out := mustRun(t, srv, "record", "list", "example.com", "--type", "A")
		if !strings.Contains(out, "weg record add") {
			t.Errorf("output = %q, want an empty state that says what comes next", out)
		}
	})

	// A name with no trailing dot is relative to the zone, the way a zonefile
	// writes it. The API completes nothing, which is what stops
	// www.example.com from becoming www.example.com.example.com.
	out := mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")
	if !strings.Contains(out, "added www.example.com. 300 IN A 192.0.2.10") {
		t.Fatalf("output = %q, want the record as it was stored", out)
	}

	t.Run("@ is the zone itself, and data is the rest of the line", func(t *testing.T) {
		out := mustRun(t, srv, "record", "add", "example.com", "@", "MX", "10", "mail.example.com.")
		if !strings.Contains(out, "added example.com. 300 IN MX 10 mail.example.com.") {
			t.Errorf("output = %q, want the apex and the unquoted data", out)
		}
	})

	t.Run("the reverse zone that would be needed is offered", func(t *testing.T) {
		out := mustRun(t, srv, "record", "add", "example.com", "far", "A", "198.51.100.1")
		if !strings.Contains(out, "no reverse zone covers 198.51.100.1") ||
			!strings.Contains(out, "100.51.198.in-addr.arpa.") {
			t.Errorf("output = %q, want the reverse zone offered (D6)", out)
		}
	})

	t.Run("a generated record is marked as one", func(t *testing.T) {
		mustRun(t, srv, "zone", "create", "2.0.192.in-addr.arpa")
		mustRun(t, srv, "record", "add", "example.com", "ptr", "A", "192.0.2.99")

		out := mustRun(t, srv, "record", "list", "2.0.192.in-addr.arpa")
		if !strings.Contains(out, "99.2.0.192.in-addr.arpa.") || !strings.Contains(out, "(generated)") {
			t.Errorf("output = %q, want the PTR marked as generated", out)
		}
	})

	t.Run("the listing lines up", func(t *testing.T) {
		out := mustRun(t, srv, "record", "list", "example.com")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "DATA") {
			t.Fatalf("header = %q, want the column titles", lines[0])
		}
		// Every row's type column starts in the same place, which is what a
		// coloured cell anywhere but the end would break.
		at := strings.Index(lines[0], "TYPE")
		for _, l := range lines[1:] {
			if len(l) < at || strings.TrimSpace(l[:at]) == "" {
				t.Errorf("row does not fill the columns before TYPE: %q", l)
			}
		}
	})

	t.Run("--output json is the same thing for a script", func(t *testing.T) {
		out := mustRun(t, srv, "record", "list", "example.com", "--type", "A", "--output", "json")
		var records []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &records); err != nil {
			t.Fatalf("decode: %v\n%s", err, out)
		}
		if len(records) == 0 {
			t.Fatal("no records decoded")
		}
		for _, r := range records {
			if r.Type != "A" {
				t.Errorf("record = %+v, want only A records", r)
			}
		}
	})

	// An RRset is several records with one name and type. Deleting by name and
	// type alone cannot know which was meant, and picking one is wrong half
	// the time.
	t.Run("an ambiguous deletion is refused and says why", func(t *testing.T) {
		mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.11")

		code, _, errOut := run(t, srv, "record", "delete", "example.com", "www", "A", "--yes")
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		for _, want := range []string{"2 A records", "192.0.2.10", "192.0.2.11"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr does not offer %q:\n%s", want, errOut)
			}
		}
	})

	t.Run("naming the data deletes exactly that one", func(t *testing.T) {
		out := mustRun(t, srv, "record", "delete", "example.com", "www", "A", "192.0.2.11", "--yes")
		if !strings.Contains(out, "deleted www.example.com. 300 IN A 192.0.2.11") {
			t.Errorf("output = %q, want the record that went", out)
		}

		left := mustRun(t, srv, "record", "list", "example.com", "--name", "www")
		if strings.Contains(left, "192.0.2.11") || !strings.Contains(left, "192.0.2.10") {
			t.Errorf("the wrong record went:\n%s", left)
		}
	})

	t.Run("a record that is not there says so", func(t *testing.T) {
		code, _, errOut := run(t, srv, "record", "delete", "example.com", "www", "SRV", "--yes")
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if !strings.Contains(errOut, "no SRV record") {
			t.Errorf("stderr = %q, want it to say what was not found", errOut)
		}
	})
}

// The shell takes the quotes off, and a TXT record's data is one or more
// character-strings separated by whitespace (RFC 1035 §3.3.14). Without this,
// `TXT "v=spf1 -all"` is stored as two strings and is not the SPF record
// anybody meant.
func TestQuoteText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  string
		in   string
		want string
	}{
		{"spf keeps its spaces", "TXT", "v=spf1 -all", `"v=spf1 -all"`},
		{"one word is quoted too, so the rule has no exceptions", "TXT", "hello", `"hello"`},
		{"already quoted is left alone", "TXT", `"one" "two"`, `"one" "two"`},
		{"a quote inside is escaped", "TXT", `say "hi"`, `"say \"hi\""`},
		{"a backslash inside is escaped", "TXT", `a\b`, `"a\\b"`},
		{"spf records too", "SPF", "v=spf1 -all", `"v=spf1 -all"`},
		// Everything else either has no character-string or has structure that
		// quoting the whole line would destroy.
		{"an address is not text", "A", "192.0.2.10", "192.0.2.10"},
		{"an MX has a preference in front", "MX", "10 mail.example.com.", "10 mail.example.com."},
		{"HINFO is two strings, not one", "HINFO", `"amd64" "linux"`, `"amd64" "linux"`},
		{"CAA has a tag before its value", "CAA", `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := quoteText(tc.typ, tc.in); got != tc.want {
				t.Errorf("quoteText(%s, %q) = %q, want %q", tc.typ, tc.in, got, tc.want)
			}
		})
	}
}

func TestRecordUpdateAndSwitch(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com", "--ttl", "300")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")

	t.Run("changing the TTL keeps the record", func(t *testing.T) {
		out := mustRun(t, srv, "record", "update", "example.com", "www", "A", "--ttl", "60")
		// Both halves: "updated" without the old value leaves a person unable
		// to tell a change from a no-op.
		if !strings.Contains(out, "updated www.example.com. 60 IN A 192.0.2.10") ||
			!strings.Contains(out, "from www.example.com. 300 IN A 192.0.2.10") {
			t.Errorf("output = %q, want what it is now and what it was", out)
		}
	})

	t.Run("changing the data", func(t *testing.T) {
		out := mustRun(t, srv, "record", "update", "example.com", "www", "A", "--data", "192.0.2.99")
		if !strings.Contains(out, "60 IN A 192.0.2.99") {
			t.Errorf("output = %q, want the new data", out)
		}
	})

	t.Run("nothing to change is the user's mistake", func(t *testing.T) {
		code, _, errOut := run(t, srv, "record", "update", "example.com", "www", "A")
		if code != ExitUsage {
			t.Errorf("exit code = %d, want %d", code, ExitUsage)
		}
		if !strings.Contains(errOut, "--ttl") {
			t.Errorf("stderr = %q, want it to say what can be changed", errOut)
		}
	})

	t.Run("disable and enable are reversible", func(t *testing.T) {
		out := mustRun(t, srv, "record", "disable", "example.com", "www", "A")
		if !strings.Contains(out, "disabled www.example.com.") {
			t.Errorf("output = %q, want it disabled", out)
		}
		if listed := mustRun(t, srv, "record", "list", "example.com", "--name", "www"); !strings.Contains(listed, "(disabled)") {
			t.Errorf("listing = %q, want the record marked disabled", listed)
		}

		out = mustRun(t, srv, "record", "enable", "example.com", "www", "A")
		if !strings.Contains(out, "enabled www.example.com.") {
			t.Errorf("output = %q, want it enabled", out)
		}
		if listed := mustRun(t, srv, "record", "list", "example.com", "--name", "www"); strings.Contains(listed, "(disabled)") {
			t.Errorf("listing = %q, want the mark gone", listed)
		}
	})
}

// D4: a generated record follows the one it came from, and editing it means
// taking it over first.
func TestRecordDetach(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com", "--ttl", "300")
	mustRun(t, srv, "zone", "create", "2.0.192.in-addr.arpa")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")

	t.Run("editing a generated record is refused, and says how", func(t *testing.T) {
		code, _, errOut := run(t, srv, "record", "update", "2.0.192.in-addr.arpa", "10", "PTR", "--ttl", "60")
		if code != ExitError {
			t.Errorf("exit code = %d, want %d", code, ExitError)
		}
		if !strings.Contains(errOut, "detach") {
			t.Errorf("stderr = %q, want it to name the way out", errOut)
		}
	})

	t.Run("detaching hands it over", func(t *testing.T) {
		out := mustRun(t, srv, "record", "detach", "2.0.192.in-addr.arpa", "10", "PTR")
		if !strings.Contains(out, "detached 10.2.0.192.in-addr.arpa.") {
			t.Errorf("output = %q, want the record handed over", out)
		}
		if listed := mustRun(t, srv, "record", "list", "2.0.192.in-addr.arpa"); strings.Contains(listed, "(generated)") {
			t.Errorf("listing = %q, want the mark gone", listed)
		}
		// And now it can be edited.
		if got := mustRun(t, srv, "record", "update", "2.0.192.in-addr.arpa", "10", "PTR", "--ttl", "60"); !strings.Contains(got, "60 IN PTR") {
			t.Errorf("output = %q, want the edit to have gone through", got)
		}
	})

	t.Run("detaching a record that was already yours says so", func(t *testing.T) {
		out := mustRun(t, srv, "record", "detach", "example.com", "www", "A")
		if !strings.Contains(out, "already yours") {
			t.Errorf("output = %q, want it to say nothing happened", out)
		}
	})
}

// Searching a zone: what backs the filter above a record listing.
func TestRecordListSearch(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	mustRun(t, srv, "zone", "create", "example.com")
	mustRun(t, srv, "record", "add", "example.com", "www", "A", "192.0.2.10")
	mustRun(t, srv, "record", "add", "example.com", "mail", "A", "192.0.2.25")
	mustRun(t, srv, "record", "add", "example.com", "_dmarc", "TXT", "v=DMARC1; p=none")

	t.Run("by data", func(t *testing.T) {
		out := mustRun(t, srv, "record", "list", "example.com", "--search", "192.0.2.10")
		if !strings.Contains(out, "www.example.com.") {
			t.Errorf("output = %q, want the record holding that address", out)
		}
		if strings.Contains(out, "mail.example.com.") {
			t.Errorf("output = %q, want only the matching record", out)
		}
	})

	t.Run("by name, case-insensitively", func(t *testing.T) {
		out := mustRun(t, srv, "record", "list", "example.com", "--search", "DMARC")
		if !strings.Contains(out, "_dmarc.example.com.") {
			t.Errorf("output = %q, want the record whose name contains it", out)
		}
		if strings.Contains(out, "www.example.com.") {
			t.Errorf("output = %q, want only the matching record", out)
		}
	})

	t.Run("nothing matching says so", func(t *testing.T) {
		out := mustRun(t, srv, "record", "list", "example.com", "--search", "203.0.113.9")
		if !strings.Contains(out, "no record matching") {
			t.Errorf("output = %q, want an empty state that says why", out)
		}
	})
}
