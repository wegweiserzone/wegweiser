package dns

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// TestServerCookie checks the construction against the test vectors in
// RFC 9018 Appendix A.
//
// They are worth having in full: between them they cover an IPv4 client, an
// IPv6 one, a cookie handed out fresh and one renewed after the secret was
// rolled over, which is every path the construction has.
// testClient is the address a test query arrives from where the test is about
// something else. A cookie is computed over the client address, so the query
// path needs one even when nothing in the test looks at it.
var testClient = netip.MustParseAddr("192.0.2.1")

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

func mustSecret(t *testing.T, s string) CookieSecret {
	t.Helper()
	b := mustHex(t, s)
	if len(b) != len(CookieSecret{}) {
		t.Fatalf("a secret of %d octets", len(b))
	}
	return CookieSecret(b)
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

// TestRespondIssuesACookie covers the exchange a client opens by sending eight
// octets of its own: it gets them back with sixteen of ours behind them, and
// what comes back is a cookie this server recognises (RFC 9018 §4.4).
func TestRespondIssuesACookie(t *testing.T) {
	t.Parallel()

	secrets := testSecrets(t)
	r := responderWithCookies(t, secrets)
	snap := resolveFixture(t)
	client := mustHex(t, "2464c4abcf10c957")

	got, _ := respond(t, r, snap,
		packQuery(t, "www.example.com.", zone.TypeA, withCookie(client)), UDP)

	if got.Rcode != wire.RcodeSuccess {
		t.Errorf("rcode = %s, want NOERROR", wire.RcodeToString[got.Rcode])
	}
	cookie := responseCookie(t, got)
	if len(cookie) != cookieLen {
		t.Fatalf("the response carries %d octets of cookie, want %d", len(cookie), cookieLen)
	}
	if !bytes.Equal(cookie[:clientCookieLen], client) {
		t.Errorf("the client half comes back as %x, want %x", cookie[:clientCookieLen], client)
	}
	if !secrets.knows(cookie[:clientCookieLen], cookie[clientCookieLen:], testClient) {
		t.Error("the server half is not one this server would recognise")
	}
}

// TestRespondKeepsAFreshCookie covers the ordinary case after that one. The
// client returns what it was given, and RFC 9018 §4.3 asks for a new one only
// once the old is half an hour old, so it keeps the one it has.
func TestRespondKeepsAFreshCookie(t *testing.T) {
	t.Parallel()

	secrets := testSecrets(t)
	r := responderWithCookies(t, secrets)
	snap := resolveFixture(t)

	client := mustHex(t, "2464c4abcf10c957")
	held := heldCookie(secrets.Current, client, testClient, 0)

	got, _ := respond(t, r, snap,
		packQuery(t, "www.example.com.", zone.TypeA, withCookie(held)), UDP)

	if cookie := responseCookie(t, got); !bytes.Equal(cookie, held) {
		t.Errorf("the response hands back %x, want the cookie the client held, %x", cookie, held)
	}
}

// TestRespondReplacesACookie covers the three ways a cookie stops being worth
// keeping: age, wear, and having been minted for somebody else.
func TestRespondReplacesACookie(t *testing.T) {
	t.Parallel()

	secrets := testSecrets(t)
	client := mustHex(t, "2464c4abcf10c957")

	tests := []struct {
		name string
		held []byte
	}{
		{
			name: "worn: past half an hour it is renewed although it still holds",
			held: heldCookie(secrets.Current, client, testClient, -cookieRenewAge-1),
		},
		{
			name: "expired: past an hour it is not taken at all",
			held: heldCookie(secrets.Current, client, testClient, -cookieMaxAge-1),
		},
		{
			name: "somebody else's: a cookie is bound to the address it was issued to",
			held: heldCookie(secrets.Current, client, netip.MustParseAddr("192.0.2.9"), 0),
		},
		{
			name: "a secret we no longer hold, and never did",
			held: heldCookie(CookieSecret{0xAA}, client, testClient, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := responderWithCookies(t, secrets)
			got, _ := respond(t, r, resolveFixture(t),
				packQuery(t, "www.example.com.", zone.TypeA, withCookie(tt.held)), UDP)

			cookie := responseCookie(t, got)
			if bytes.Equal(cookie, tt.held) {
				t.Fatalf("the cookie came back unchanged: %x", cookie)
			}
			if !secrets.knows(cookie[:clientCookieLen], cookie[clientCookieLen:], testClient) {
				t.Errorf("what came back is not a cookie of ours: %x", cookie)
			}
			// Whatever was wrong with the cookie, the query is still answered:
			// nothing is refused until the server is under load (D35, D37).
			if got.Rcode != wire.RcodeSuccess {
				t.Errorf("rcode = %s, want NOERROR", wire.RcodeToString[got.Rcode])
			}
		})
	}
}

// TestRespondTakesACookieFromThePreviousSecret covers a rotation: the client
// holds a cookie from before it and is not made to start over.
func TestRespondTakesACookieFromThePreviousSecret(t *testing.T) {
	t.Parallel()

	secrets := testSecrets(t)
	rotated := CookieSecrets{Current: CookieSecret{0x11, 0x22}, Previous: secrets.Current}
	client := mustHex(t, "2464c4abcf10c957")
	held := heldCookie(secrets.Current, client, testClient, 0)

	r := responderWithCookies(t, rotated)
	got, _ := respond(t, r, resolveFixture(t),
		packQuery(t, "www.example.com.", zone.TypeA, withCookie(held)), UDP)

	if cookie := responseCookie(t, got); !bytes.Equal(cookie, held) {
		t.Errorf("the cookie was replaced with %x although the old secret still holds", cookie)
	}
}

// TestRespondRefusesAMalformedCookie covers RFC 7873 §5.2.2: a cookie option
// of any other length is malformed, and FORMERR is the answer.
func TestRespondRefusesAMalformedCookie(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 7, 9, 15, 41} {
		r := responderWithCookies(t, testSecrets(t))
		got, _ := respond(t, r, resolveFixture(t),
			packQuery(t, "www.example.com.", zone.TypeA, withCookie(make([]byte, n))), UDP)

		if got.Rcode != wire.RcodeFormatError {
			t.Errorf("a cookie of %d octets: rcode = %s, want FORMERR",
				n, wire.RcodeToString[got.Rcode])
		}
		if len(got.Answer) != 0 {
			t.Errorf("a cookie of %d octets: the query was answered anyway", n)
		}
	}
}

// TestRespondWithoutACookie covers the two ways a response carries none: the
// query did not ask, or this server has no secret to answer with yet.
func TestRespondWithoutACookie(t *testing.T) {
	t.Parallel()

	snap := resolveFixture(t)

	t.Run("the query carried none", func(t *testing.T) {
		t.Parallel()

		r := responderWithCookies(t, testSecrets(t))
		got, _ := respond(t, r, snap,
			packQuery(t, "www.example.com.", zone.TypeA, withEDNS(4096, 0)), UDP)

		if opt := got.IsEdns0(); opt != nil && requestCookie(opt) != nil {
			t.Error("a cookie was volunteered to a client that did not ask for one")
		}
	})

	t.Run("no secret has been published", func(t *testing.T) {
		t.Parallel()

		r := NewResponder(DefaultLimits())
		got, _ := respond(t, r, snap, packQuery(t, "www.example.com.", zone.TypeA,
			withCookie(mustHex(t, "2464c4abcf10c957"))), UDP)

		if opt := got.IsEdns0(); opt != nil && requestCookie(opt) != nil {
			t.Error("a cookie was answered with before a secret existed")
		}
		if got.Rcode != wire.RcodeSuccess {
			t.Errorf("rcode = %s, want the query answered anyway", wire.RcodeToString[got.Rcode])
		}
	})
}

// testSecrets returns a published pair, using the secret RFC 9018 Appendix A
// works its vectors with.
func testSecrets(t *testing.T) CookieSecrets {
	t.Helper()
	return CookieSecrets{Current: mustSecret(t, "e5e973e5a6b2a43f48e7dc849e37bfcf")}
}

// responderWithCookies returns a responder holding secrets, the way the server
// hands them to the one it runs per reader.
func responderWithCookies(t *testing.T, secrets CookieSecrets) *Responder {
	t.Helper()

	r := NewResponder(DefaultLimits())
	holder := new(atomic.Pointer[cookieHolder])
	holder.Store(&cookieHolder{secrets: secrets})
	r.cookies = holder
	return r
}

// heldCookie is the cookie a client holds: its own eight octets and a Server
// Cookie computed offset seconds from now.
func heldCookie(secret CookieSecret, client []byte, ip netip.Addr, offset int) []byte {
	at := cookieNow(time.Now().Add(time.Duration(offset) * time.Second))
	server := secret.serverCookie(client, ip, at)

	out := make([]byte, 0, cookieLen)
	out = append(out, client...)
	return append(out, server[:]...)
}

// withCookie gives a query an OPT carrying a cookie option.
func withCookie(cookie []byte) func(*wire.Msg) {
	return func(m *wire.Msg) {
		opt := &wire.OPT{Hdr: wire.RR_Header{Name: ".", Rrtype: wire.TypeOPT}}
		opt.SetUDPSize(4096)
		opt.SetVersion(0)
		opt.Option = append(opt.Option,
			&wire.EDNS0_COOKIE{Code: wire.EDNS0COOKIE, Cookie: hex.EncodeToString(cookie)})
		m.Extra = append(m.Extra, opt)
	}
}

// responseCookie returns the cookie a response carries, failing the test where
// it carries none.
func responseCookie(t *testing.T, msg *wire.Msg) []byte {
	t.Helper()

	opt := msg.IsEdns0()
	if opt == nil {
		t.Fatal("the response carries no OPT, so it carries no cookie")
	}
	cookie := requestCookie(opt)
	if cookie == nil {
		t.Fatal("the response carries no cookie option")
	}
	raw, err := hex.DecodeString(cookie.Cookie)
	if err != nil {
		t.Fatalf("the cookie in the response is not hex: %v", err)
	}
	return raw
}
