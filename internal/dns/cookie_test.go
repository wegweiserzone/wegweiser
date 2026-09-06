package dns

import (
	"encoding/hex"
	"net/netip"
	"testing"
)

// TestServerCookie checks the construction against the test vectors in
// RFC 9018 Appendix A.
//
// They are worth having in full: between them they cover an IPv4 client, an
// IPv6 one, a cookie handed out fresh and one renewed after the secret was
// rolled over, which is every path the construction has.
func TestServerCookie(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		client string
		ip     string
		at     uint32
		want   string
	}{
		{
			name:   "A.1, learning a new server cookie",
			secret: "e5e973e5a6b2a43f48e7dc849e37bfcf",
			client: "2464c4abcf10c957",
			ip:     "198.51.100.100",
			at:     1559731985,
			want:   "010000005cf79f111f8130c3eee29480",
		},
		{
			name:   "A.2, the same client renewing it",
			secret: "e5e973e5a6b2a43f48e7dc849e37bfcf",
			client: "2464c4abcf10c957",
			ip:     "198.51.100.100",
			at:     1559734385,
			want:   "010000005cf7a871d4a564a1442aca77",
		},
		{
			name:   "A.3, another client renewing it",
			secret: "e5e973e5a6b2a43f48e7dc849e37bfcf",
			client: "fc93fc62807ddb86",
			ip:     "203.0.113.203",
			at:     1559734700,
			want:   "010000005cf7a9acf73a7810aca2381e",
		},
		{
			name:   "A.4, an IPv6 client after the secret rolled over",
			secret: "445536bcd2513298075a5d379663c962",
			client: "22681ab97d52c298",
			ip:     "2001:db8:220:1:59de:d0f4:8769:82b8",
			at:     1559741961,
			want:   "010000005cf7c609a6bb79d16625507a",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secret := mustSecret(t, c.secret)
			got := secret.serverCookie(mustHex(t, c.client), mustAddr(t, c.ip), c.at)

			if hex.EncodeToString(got[:]) != c.want {
				t.Errorf("server cookie = %x, want %s", got, c.want)
			}
		})
	}
}

// TestServerCookieMatches checks the cookies those vectors have the client
// returning, which is the direction the query path runs in.
func TestServerCookieMatches(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		ip     string
		cookie string
		want   bool
	}{
		{
			name:   "A.3, reserved octets the client set are part of the hash",
			secret: "e5e973e5a6b2a43f48e7dc849e37bfcf",
			ip:     "203.0.113.203",
			cookie: "fc93fc62807ddb8601abcdef5cf78f71a314227b6679ebf5",
			want:   true,
		},
		{
			name:   "A.4, the cookie the old secret produced",
			secret: "dd3bdf9344b678b185a6f5cb60fca715",
			ip:     "2001:db8:220:1:59de:d0f4:8769:82b8",
			cookie: "22681ab97d52c298010000005cf7c57926556bd0934c72f8",
			want:   true,
		},
		{
			name:   "A.4, and not the cookie the new one would have",
			secret: "445536bcd2513298075a5d379663c962",
			ip:     "2001:db8:220:1:59de:d0f4:8769:82b8",
			cookie: "22681ab97d52c298010000005cf7c57926556bd0934c72f8",
			want:   false,
		},
		{
			name:   "the same cookie from another address is not ours",
			secret: "e5e973e5a6b2a43f48e7dc849e37bfcf",
			ip:     "203.0.113.204",
			cookie: "fc93fc62807ddb8601abcdef5cf78f71a314227b6679ebf5",
			want:   false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secret := mustSecret(t, c.secret)
			client, server, ok := splitCookie(mustHex(t, c.cookie))
			if !ok {
				t.Fatalf("cookie %s did not split", c.cookie)
			}

			if got := secret.matches(client, server, mustAddr(t, c.ip)); got != c.want {
				t.Errorf("matches = %t, want %t", got, c.want)
			}
		})
	}
}

// TestSplitCookie covers the lengths RFC 7873 §5.2.2 allows and the ones it
// answers with FORMERR.
func TestSplitCookie(t *testing.T) {
	cases := []struct {
		len    int
		ok     bool
		server int
	}{
		{len: 0, ok: false},
		{len: 7, ok: false},
		{len: 8, ok: true, server: 0},
		{len: 9, ok: false},
		{len: 15, ok: false},
		{len: 16, ok: true, server: 8},
		{len: 24, ok: true, server: 16},
		{len: 40, ok: true, server: 32},
		{len: 41, ok: false},
	}

	for _, c := range cases {
		client, server, ok := splitCookie(make([]byte, c.len))
		switch {
		case ok != c.ok:
			t.Errorf("a cookie of %d octets: ok = %t, want %t", c.len, ok, c.ok)
		case !ok:
		case len(client) != clientCookieLen:
			t.Errorf("a cookie of %d octets: client half is %d octets", c.len, len(client))
		case len(server) != c.server:
			t.Errorf("a cookie of %d octets: server half is %d octets, want %d",
				c.len, len(server), c.server)
		}
	}
}

// TestCookieAge covers the window of RFC 9018 §4.3, including the wrap the
// modulo-2**32 timestamp makes inevitable.
func TestCookieAge(t *testing.T) {
	const now = 1559731985

	cases := []struct {
		name   string
		ts     uint32
		at     uint32
		usable bool
		worn   bool
	}{
		{name: "stamped this second", ts: now, at: now, usable: true},
		{name: "half an hour old", ts: now - 1800, at: now, usable: true, worn: true},
		{name: "an hour old, the last second it holds", ts: now - 3600, at: now, usable: true, worn: true},
		{name: "an hour and a second old", ts: now - 3601, at: now, worn: true},
		{name: "five minutes ahead, the skew allowance", ts: now + 300, at: now, usable: true},
		{name: "five minutes and a second ahead", ts: now + 301, at: now},
		{
			// Stamped before the timestamp space ran out and read after it
			// wrapped, which is an hour apart and not four billion seconds.
			name: "an hour old across the wrap",
			ts:   4294964696, at: 1000, usable: true, worn: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cookieUsable(c.ts, c.at); got != c.usable {
				t.Errorf("cookieUsable = %t, want %t", got, c.usable)
			}
			if got := cookieWorn(c.ts, c.at); got != c.worn {
				t.Errorf("cookieWorn = %t, want %t", got, c.worn)
			}
		})
	}
}

func mustSecret(t *testing.T, s string) cookieSecret {
	t.Helper()
	b := mustHex(t, s)
	if len(b) != len(cookieSecret{}) {
		t.Fatalf("a secret of %d octets", len(b))
	}
	return cookieSecret(b)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}
