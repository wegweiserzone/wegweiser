# D28 — TSIG: a transfer is granted to a key, not to an address

- Decided: 2026-08-25

## Context

D26 put a zone transfer behind a list of addresses and said what that list cannot do:
tell two hosts behind one NAT apart, or authenticate a secondary somebody else runs. TSIG
(RFC 8945) is what it named as the next step, and the list was shaped so a key could become
an entry in it without the shape changing.

Three things decide what this costs.

**A TSIG secret cannot be stored the way an API token is.** D5 keeps a token's SHA-256 and
shows the token once, so a stolen database yields nothing anybody can log in with. Verifying
a TSIG signature means recomputing the MAC, which needs the secret itself. There is no
arrangement in which this database holds a usable key and cannot hand it over.

**Signing a transfer is stateful across its messages.** RFC 8945 §5.3.1 requires the first
and last envelope of a multi-message response to be signed and permits at most 99 unsigned in
between, and §5.3 makes each later digest depend on the MAC of the previous signed message.
The transfer path packs each message just before writing it, so that is where a signature
belongs, and the writer has to carry the running MAC from one to the next.

**The wire library already does the arithmetic.** `TsigGenerate` and `TsigVerify` work on
packed bytes, which is exactly what `internal/dns` produces on the way out and reads on the
way in. Nothing here has to implement HMAC or the digest layout.

## Decision

**A key is a name, an algorithm and a secret**, in a table of its own with an identifier like
everything else. The name is a domain name (RFC 8945 §4.2) and is compared case-insensitively
(§9), which is how this server compares every other name already (RFC 4343).

**The secret is stored so that it can be read back, and the documentation says so.** It is the
one thing in this database that is not a hash of a secret, and an operator deciding how to
back the file up needs to know that before they copy it somewhere convenient. The file mode
and the backup are the controls here, not the schema.

**A secret is generated rather than typed.** At least as long as the hash output (RFC 8945
§8), from the system's entropy source. It can still be pasted in for a secondary that already
has a key, because the other end is often not ours to choose.

**The secret is never in a listing.** Reading it back is a request of its own. That is not a
security boundary, since the database holds it either way; it keeps a key off a screen
somebody did not mean to put it on.

**hmac-sha256 by default, with hmac-sha384 and hmac-sha512 offered. hmac-sha1 and HMAC-MD5 are
not.** This is a deliberate departure from the MUST-implement in RFC 8945 §6, and it is worth
naming rather than burying. The same table calls hmac-sha1 NOT RECOMMENDED and HMAC-MD5 MUST
NOT be used; this product has no installed base to stay compatible with; and the algorithm is
one line to change at both ends. A secondary that can only do hmac-sha1 is an old secondary,
and the address list still serves it.

**An entry in the transfer list is a prefix or a key, and a request needs one of them.** A key
entry matches a request signed by that key that verifies, from any address at all, which is
the whole point of having one. Requiring both an address and a key is a narrower entry that
can be added later without changing what the existing ones mean.

**Every message of a transfer is signed.** The ninety-nine the RFC permits are a concession to
old clients, not a budget to spend. One HMAC per sixty kilobytes is not worth the state
machine that saving it would need.

**A refusal says which of the three failures it is.** An unknown key name and a signature that
does not verify are both NOTAUTH with BADKEY and BADSIG, and both go back unsigned (RFC 8945
§5.2.1, §5.2.2). A time outside the fudge window is NOTAUTH with BADTIME, signed, carrying
this server's clock in the Other Data field (§5.2.3), because a clock difference is the one of
the three the client can diagnose from the answer it gets.

**A notification is signed when the secondary it goes to names a key.** An entry in the notify
list gains an optional key, so that a secondary configured to insist on TSIG hears the news as
well as being able to fetch it.

**Revoking a key clears its secret.** A token survives revocation because what survives is a
hash, and the audit log still wants the name behind a change. Here the row can keep the name
and the dates without keeping the material, and a revoked key whose secret is still in the
file is a liability with no reader. The name is then free again, so rotating a key an operator
has already configured on the other end does not mean renaming it there.

**Keys are server-wide.** A key that may transfer may transfer every zone, which is what the
address list already grants. Per-zone entries are a narrowing, and narrowing later breaks
nobody.

## Rationale

Encrypting the secret at rest was the obvious counter-proposal and it is a separate decision
rather than a rider on this one. The encryption key has to come from outside the database,
which means a file or an environment variable, which puts a secret in a second place and adds
a way for an operator to lock themselves out of their own zones. It is a real improvement and
it deserves its own argument.

The algorithm list is where this deviates from the specification on purpose. Implementing
hmac-sha1 to satisfy a MUST, and then telling every operator not to use the thing we
implemented, is worse than not offering it: it is a footgun with a manual page. The RFC's own
table already says as much, and the address list remains for the secondary that cannot do
better.

## Consequences

Accepted knowingly: **the database file becomes as sensitive as the zones plus the ability to
take a copy of them.** What it held until now was token hashes, which survive being read: D5
stores a SHA-256 and nothing anybody can authenticate with. A TSIG secret does not survive
being read. The installation documentation has to say what mode the file carries and what
that means for backups, and it is the first thing in this product where
losing a file is worse than losing a service.

Key management is three operations that by invariant 1 exist in the API, the CLI and the
interface: create, list, revoke. That is more surface than either of the two lists before it,
and it is the reason this is its own piece of work rather than another setting.

`transferWriter` gains a signer and stops being a thing that only counts octets. The signing
state belongs to one transfer, so it is created per transfer rather than held on the server.

A notification is signed on the way out and its answer is not verified on the way back. A
secondary that does not sign its response would otherwise never be acknowledged and would be
told six times, and what an unverified acknowledgement costs is that somebody who can guess
the message identifier and spoof the address stops one retransmission. That is the same
exposure the notification had before it was signed at all.

**Corrected after testing against BIND 9.20.** This record said a signature would be verified on
a transfer request and nowhere else, on the reasoning that nobody signs ordinary queries. That
is wrong. BIND signs the SOA refresh it sends before asking for a zone, whenever a key is
configured for the primary, and discards an unsigned answer to it with `expected a TSIG or
SIG(0)`. Both ways of writing that configuration do it, so a secondary using TSIG never
reached the transfer at all. RFC 8945 §5.3 requires a signed request to be answered signed,
and this server now does, which is what makes the arrangement work rather than a nicety.

The keys are therefore published to the query path rather than read from the database when one
is named, the way the transfer list already was. A signed query costs a map lookup and an HMAC;
an unsigned one, which is almost all of them, costs a nil check.

An address entry and a key entry can both be present, and a request that matches either is
served. An operator who adds a key and forgets to remove the prefix has not tightened
anything, and the interface should show the two lists as one answer to one question rather
than as two independent switches.
