package dns

import (
	"fmt"
	"slices"
	"testing"
	"time"

	wire "github.com/miekg/dns"

	"github.com/wegweiserzone/wegweiser/internal/zone"
)

// ring is the keys a test server holds.
func ring(keys ...TSIGKey) Keyring {
	out := make(Keyring, len(keys))
	for _, k := range keys {
		out[k.Name] = k
	}
	return out
}

// testKey is one key, with a secret long enough for its algorithm
// (RFC 8945 §8).
func testKey(name string, alg zone.TSIGAlgorithm) TSIGKey {
	secret := make([]byte, alg.SecretBytes())
	for i := range secret {
		secret[i] = byte(i * 7)
	}
	return TSIGKey{Name: zone.MustParseName(name), Algorithm: alg, Secret: secret}
}

// signedTransfer sends one transfer request signed with key and reads the whole
// answer back, verifying every message the way a secondary does.
//
// at is the moment the request claims to have been signed. The zero time means
// now, and anything else is how the clock refusal below is provoked.
func signedTransfer(t *testing.T, addr, apex string, key TSIGKey, at time.Time) []*wire.Msg {
	t.Helper()

	if at.IsZero() {
		at = time.Now()
	}
	m := new(wire.Msg)
	m.SetAxfr(apex)
	m.Id = queryID
	m.SetTsig(key.Name.String(), key.Algorithm.String(), tsigFudge, at.Unix())
	query, requestMAC, err := wire.TsigGenerate(m, key.base64Secret(), "", false)
	if err != nil {
		t.Fatalf("sign the request: %v", err)
	}

	conn := dialTCP(t, addr)
	writeFramed(t, conn, query)

	var (
		msgs []*wire.Msg
		soas int
		prev = requestMAC
	)
	for {
		raw := readFramed(t, conn)
		m := parse(t, raw)
		msgs = append(msgs, m)

		// Every message of an answer to a signed request is signed, and each
		// after the first covers the one before it (RFC 8945 §5.3.1).
		if m.IsTsig() != nil && m.IsTsig().Error == 0 {
			timersOnly := len(msgs) > 1
			if verr := wire.TsigVerify(raw, key.base64Secret(), prev, timersOnly); verr != nil {
				t.Errorf("message %d does not verify: %v", len(msgs), verr)
			}
			prev = m.IsTsig().MAC
		}

		if m.Rcode != wire.RcodeSuccess || len(m.Answer) == 0 {
			return msgs
		}
		for _, rr := range m.Answer {
			if _, ok := rr.(*wire.SOA); ok {
				soas++
			}
		}
		if soas >= 2 {
			return msgs
		}
	}
}

// tsigOf is the TSIG record on a message, or a failure saying there was none.
func tsigOf(t *testing.T, m *wire.Msg) *wire.TSIG {
	t.Helper()
	rr := m.IsTsig()
	if rr == nil {
		t.Fatalf("the message carries no TSIG: %v", m)
	}
	return rr
}

func TestTransferToAKeyFromAnyAddress(t *testing.T) {
	t.Parallel()

	// The point of a key: the address is not on any list, and the transfer is
	// served anyway (D28).
	key := testKey("secondary.example.com.", zone.HMACSHA256)
	snap := resolveFixture(t)
	s := startServer(t, snap, Config{
		Transfers: Allow{Keys: []zone.Name{key.Name}},
		Keys:      ring(key),
	})

	msgs := signedTransfer(t, s.Addr().String(), "example.com.", key, time.Time{})
	got := transferred(msgs)
	if want := snap.zoneAt(zone.MustParseName("example.com.")).count + 1; len(got) != want {
		t.Fatalf("the transfer carried %d records, want %d", len(got), want)
	}
	for i, m := range msgs {
		if m.IsTsig() == nil {
			t.Errorf("message %d of the transfer is unsigned", i)
		}
	}
}

func TestAnUnsignedRequestStillNeedsTheAddressList(t *testing.T) {
	t.Parallel()

	key := testKey("secondary.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{
		Transfers: Allow{Keys: []zone.Name{key.Name}},
		Keys:      ring(key),
	})

	// Naming a key does not open the door to everybody; an unsigned request
	// from the loopback is still refused.
	msgs := askAXFR(t, s.Addr().String(), "example.com.")
	if msgs[0].Rcode != wire.RcodeRefused {
		t.Errorf("rcode is %s, want REFUSED", wire.RcodeToString[msgs[0].Rcode])
	}
	if msgs[0].IsTsig() != nil {
		t.Error("an answer to an unsigned request carries a TSIG")
	}
}

func TestASignedRequestIsRefusedWhenTheKeyIsNotOnTheList(t *testing.T) {
	t.Parallel()

	// The signature verifies, so this is not a TSIG failure. It is the
	// ordinary refusal: this key may not transfer.
	key := testKey("stranger.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{
		Transfers: Allow{Keys: []zone.Name{zone.MustParseName("secondary.example.com.")}},
		Keys:      ring(key),
	})

	msgs := signedTransfer(t, s.Addr().String(), "example.com.", key, time.Time{})
	if msgs[0].Rcode != wire.RcodeRefused {
		t.Errorf("rcode is %s, want REFUSED", wire.RcodeToString[msgs[0].Rcode])
	}
}

func TestARequestSignedWithAKeyThisServerDoesNotHold(t *testing.T) {
	t.Parallel()

	key := testKey("stranger.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{Transfers: loopback})

	msgs := signedTransfer(t, s.Addr().String(), "example.com.", key, time.Time{})
	m := msgs[0]
	if m.Rcode != wire.RcodeNotAuth {
		t.Errorf("rcode is %s, want NOTAUTH (RFC 8945 §5.2.1)", wire.RcodeToString[m.Rcode])
	}
	rr := tsigOf(t, m)
	if rr.Error != wire.RcodeBadKey {
		t.Errorf("TSIG error is %d, want BADKEY", rr.Error)
	}
	// §5.2.1: the refusal goes back unsigned, because there is nothing to sign
	// it with.
	if rr.MAC != "" {
		t.Errorf("the refusal carries a MAC of %q, want none", rr.MAC)
	}
	// Unsigned is not the same as blank. A client reading back a zero where it
	// sent a time reports that the clocks disagree, which is not what happened.
	if rr.TimeSigned == 0 {
		t.Error("the refusal echoes no time signed, which reads as a clock problem")
	}
}

func TestARequestWhoseSignatureDoesNotVerify(t *testing.T) {
	t.Parallel()

	held := testKey("secondary.example.com.", zone.HMACSHA256)
	// Same name, different secret: this is what an operator who typed one of
	// the two ends wrong produces.
	wrong := held
	wrong.Secret = append([]byte("wrong"), held.Secret[5:]...)

	s := startServer(t, resolveFixture(t), Config{
		Transfers: Allow{Keys: []zone.Name{held.Name}},
		Keys:      ring(held),
	})

	msgs := signedTransfer(t, s.Addr().String(), "example.com.", wrong, time.Time{})
	m := msgs[0]
	if m.Rcode != wire.RcodeNotAuth {
		t.Errorf("rcode is %s, want NOTAUTH", wire.RcodeToString[m.Rcode])
	}
	rr := tsigOf(t, m)
	if rr.Error != wire.RcodeBadSig {
		t.Errorf("TSIG error is %d, want BADSIG (RFC 8945 §5.2.2)", rr.Error)
	}
	if rr.MAC != "" {
		t.Errorf("the refusal carries a MAC of %q, want none", rr.MAC)
	}
	if len(m.Answer) != 0 {
		t.Errorf("a refused request was answered with %d records", len(m.Answer))
	}
}

func TestARequestSignedTooLongAgo(t *testing.T) {
	t.Parallel()

	key := testKey("secondary.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{
		Transfers: Allow{Keys: []zone.Name{key.Name}},
		Keys:      ring(key),
	})

	// Well outside the fudge window in either direction.
	stale := time.Now().Add(-2 * tsigFudge * time.Second)
	msgs := signedTransfer(t, s.Addr().String(), "example.com.", key, stale)

	m := msgs[0]
	if m.Rcode != wire.RcodeNotAuth {
		t.Errorf("rcode is %s, want NOTAUTH", wire.RcodeToString[m.Rcode])
	}
	rr := tsigOf(t, m)
	if rr.Error != wire.RcodeBadTime {
		t.Errorf("TSIG error is %d, want BADTIME (RFC 8945 §5.2.3)", rr.Error)
	}
	// Unlike the other two this one is signed, and carries this server's clock,
	// which is what lets a client see how far apart the two are.
	if rr.MAC == "" {
		t.Error("the BADTIME refusal is unsigned; a client cannot trust the time in it")
	}
	if rr.OtherLen != 6 || rr.OtherData == "" {
		t.Errorf("other data is %d octets (%q), want six holding this server's clock",
			rr.OtherLen, rr.OtherData)
	}
}

func TestARequestSignedWithTheWrongAlgorithm(t *testing.T) {
	t.Parallel()

	held := testKey("secondary.example.com.", zone.HMACSHA256)
	other := testKey("secondary.example.com.", zone.HMACSHA512)

	s := startServer(t, resolveFixture(t), Config{
		Transfers: Allow{Keys: []zone.Name{held.Name}},
		Keys:      ring(held),
	})

	msgs := signedTransfer(t, s.Addr().String(), "example.com.", other, time.Time{})
	if rr := tsigOf(t, msgs[0]); rr.Error != wire.RcodeBadKey {
		t.Errorf("TSIG error is %d, want BADKEY: the key does not sign with that", rr.Error)
	}
}

func TestAServerWithNoKeysRefusesASignedRequest(t *testing.T) {
	t.Parallel()

	key := testKey("secondary.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{Transfers: loopback})

	msgs := signedTransfer(t, s.Addr().String(), "example.com.", key, time.Time{})
	if rr := tsigOf(t, msgs[0]); rr.Error != wire.RcodeBadKey {
		t.Errorf("TSIG error is %d, want BADKEY", rr.Error)
	}
}

func TestASignedTransferSpansMessages(t *testing.T) {
	t.Parallel()

	// The chain of MACs is what a multi-message answer needs, and it only
	// shows up once there is more than one message. signedTransfer verifies
	// each one against the last, so reaching the end is the assertion.
	key := testKey("secondary.example.com.", zone.HMACSHA256)
	z := newZone(t, "big.example.")
	lines := make([]string, 0, 4000)
	for i := range 4000 {
		lines = append(lines, hostLine(i))
	}
	snap := build(t, z, lines...)

	s := startServer(t, snap, Config{
		Transfers: Allow{Keys: []zone.Name{key.Name}},
		Keys:      ring(key),
	})

	msgs := signedTransfer(t, s.Addr().String(), "big.example.", key, time.Time{})
	if len(msgs) < 2 {
		t.Fatalf("the transfer fitted in %d message; this proves nothing about the chain", len(msgs))
	}
	if got, want := len(transferred(msgs)), snap.zoneAt(z.Name).count+1; got != want {
		t.Errorf("the transfer carried %d records, want %d", got, want)
	}
}

// hostLine is one A record, numbered so that four thousand of them do not fit
// in one message.
func hostLine(i int) string {
	return fmt.Sprintf("host%d.big.example. 300 IN A 192.0.2.%d", i, i%256)
}

// signedQuery asks one ordinary question, signed, and reads the answer back.
func signedQuery(t *testing.T, addr, name string, typ zone.RRType, key TSIGKey) *wire.Msg {
	t.Helper()

	m := new(wire.Msg)
	m.SetQuestion(name, uint16(typ))
	m.Id = queryID
	m.SetTsig(key.Name.String(), key.Algorithm.String(), tsigFudge, time.Now().Unix())
	query, requestMAC, err := wire.TsigGenerate(m, key.base64Secret(), "", false)
	if err != nil {
		t.Fatalf("sign the query: %v", err)
	}

	conn := dialTCP(t, addr)
	writeFramed(t, conn, query)
	raw := readFramed(t, conn)
	got := parse(t, raw)

	// The way a secondary checks it, which is the whole point: BIND discards
	// an answer to a signed query that comes back unsigned.
	//
	// On a copy, because verifying rewrites the header count in place and the
	// message would then read as though it had never carried a signature.
	if verr := wire.TsigVerify(slices.Clone(raw), key.base64Secret(), requestMAC, false); verr != nil {
		t.Errorf("the answer does not verify: %v", verr)
	}
	return got
}

// An ordinary query, not a transfer. BIND signs the SOA refresh it sends before
// asking for a zone whenever a key is configured for the primary, and discards
// an unsigned answer to it, so a secondary configured with a key never reaches
// the transfer at all. RFC 8945 §5.3 requires the response to be signed.
func TestASignedQueryIsAnsweredSigned(t *testing.T) {
	t.Parallel()

	key := testKey("secondary.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{Keys: ring(key)})

	m := signedQuery(t, s.Addr().String(), "example.com.", zone.TypeSOA, key)
	if m.Rcode != wire.RcodeSuccess {
		t.Errorf("rcode is %s, want NOERROR", wire.RcodeToString[m.Rcode])
	}
	if m.IsTsig() == nil {
		t.Fatal("the answer is unsigned")
	}
	if len(m.Answer) != 1 {
		t.Fatalf("it carries %d records, want the start of authority", len(m.Answer))
	}
	if _, ok := m.Answer[0].(*wire.SOA); !ok {
		t.Errorf("it answers with %s", m.Answer[0])
	}
}

// Signing a query grants nothing on the query path. It is answered the same
// way an unsigned one is, and the signature only says who asked.
func TestASignedQueryIsAnsweredLikeAnyOther(t *testing.T) {
	t.Parallel()

	key := testKey("secondary.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{Keys: ring(key)})

	m := signedQuery(t, s.Addr().String(), "www.example.invalid.", zone.TypeA, key)
	if m.Rcode != wire.RcodeRefused {
		t.Errorf("rcode is %s, want REFUSED for a zone this server does not hold",
			wire.RcodeToString[m.Rcode])
	}
	if m.IsTsig() == nil {
		t.Error("the refusal is unsigned")
	}
}

func TestAQueryWhoseSignatureDoesNotHold(t *testing.T) {
	t.Parallel()

	held := testKey("secondary.example.com.", zone.HMACSHA256)
	wrong := held
	wrong.Secret = append([]byte("wrong"), held.Secret[5:]...)
	s := startServer(t, resolveFixture(t), Config{Keys: ring(held)})

	m := new(wire.Msg)
	m.SetQuestion("www.example.com.", uint16(zone.TypeA))
	m.Id = queryID
	m.SetTsig(wrong.Name.String(), wrong.Algorithm.String(), tsigFudge, time.Now().Unix())
	query, _, err := wire.TsigGenerate(m, wrong.base64Secret(), "", false)
	if err != nil {
		t.Fatalf("sign the query: %v", err)
	}

	conn := dialTCP(t, addrOf(t, s))
	writeFramed(t, conn, query)
	got := parse(t, readFramed(t, conn))

	if got.Rcode != wire.RcodeNotAuth {
		t.Errorf("rcode is %s, want NOTAUTH", wire.RcodeToString[got.Rcode])
	}
	if rr := tsigOf(t, got); rr.Error != wire.RcodeBadSig {
		t.Errorf("TSIG error is %d, want BADSIG", rr.Error)
	}
	// Nothing is resolved for a request this server cannot attribute.
	if len(got.Answer) != 0 {
		t.Errorf("a refused query was answered with %d records", len(got.Answer))
	}
}

// addrOf is the address a test server is listening on.
func addrOf(t *testing.T, s *Server) string {
	t.Helper()
	return s.Addr().String()
}

// An unsigned query is what almost every query is, and nothing about it
// changes.
func TestAnUnsignedQueryIsUntouched(t *testing.T) {
	t.Parallel()

	key := testKey("secondary.example.com.", zone.HMACSHA256)
	s := startServer(t, resolveFixture(t), Config{Keys: ring(key)})

	conn := dialTCP(t, s.Addr().String())
	writeFramed(t, conn, packQuery(t, "www.example.com.", zone.TypeA))
	m := parse(t, readFramed(t, conn))

	if m.Rcode != wire.RcodeSuccess {
		t.Errorf("rcode is %s, want NOERROR", wire.RcodeToString[m.Rcode])
	}
	if m.IsTsig() != nil {
		t.Error("an unsigned query was answered with a signature")
	}
}
