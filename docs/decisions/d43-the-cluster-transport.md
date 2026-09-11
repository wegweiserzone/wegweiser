# D43 — The cluster transport is TLS, and the secret is proved inside it

- Decided: 2026-09-10
- Amends: [D24](d24-what-the-cluster-replicates.md)
- Amended by: [D44](d44-starting-and-joining.md)

## Context

D24 settled that the Raft transport authenticates with a shared secret read from the
configuration file, and named mutual TLS with a cluster CA as the better arrangement that
could replace it later. It said nothing about whether anybody else can read what travels.

Somebody can, unless something stops them. A batch carries TSIG secrets
([D32](d32-what-else-the-cluster-replicates.md)), the cookie secret travels beside them as a
server setting, a log snapshot carries all of it at once, and a forwarded write carries an
assertion about who the caller is ([D40](d40-a-write-reaches-the-leader.md)). A transport
that authenticates and then speaks in the clear hands every one of those to whoever sits on
the path. Telling operators to keep the cluster on a private network is a condition most of
them will not know they have broken.

The transport is also the one thing `wegwitness` has to agree with this server about
([D39](d39-the-witness.md)). It is a contract between two repositories, and it has to be
written down precisely enough for a second implementation.

## Decision

**TLS 1.3, with a certificate each member makes for itself at start and never writes
down.** Nothing verifies that certificate, and nothing has to. It exists so that TLS can
agree a key; what it would otherwise vouch for is established by the step below. There is no
CA, nothing to distribute and nothing to renew.

**The secret is proved inside the channel, bound to it.** Once TLS is up, both ends draw 32
octets of keying material from the session under a label of this protocol's own: the
exporter of RFC 5705, which RFC 9266 uses for channel binding. The side that dialled sends an
HMAC-SHA-256, keyed with the secret, over a label naming its role and that material. The side
that answered checks it, and only then sends its own under the other role's label.

Keying material differs for every TLS session, so a proof is worthless anywhere but on the
connection it was made for. A party in the middle that terminates TLS towards both ends holds
two sessions and cannot pass a proof from one to the other. The roles keep a proof from being
reflected back at whoever sent it. The side that answered speaks second, so a stranger who
connects without the secret learns only that a TLS server is listening.

**The protocol and its version are named in ALPN, and a peer that does not name the same
one is refused**, including one that names none at all. Two builds that cannot talk to each
other then fail at the handshake and say so, rather than halfway into a stream neither of
them understands.

**After the proof, one octet says what the stream is**: Raft's own traffic, or a write
forwarded to the leader. Both use the one port, so a member has one address to advertise
and one opening in a firewall. The default port is 8054, one above the API's.

The exact labels and octets are constants in `internal/cluster`, and that is what a second
implementation reads. They are not repeated here, because a copy is a second thing to keep
correct.

**The secret proves membership, not identity.** Whoever holds it can speak as any member.
Which member is which is Raft's configuration ([D42](d42-membership-lives-in-the-log.md)),
and a secret that leaks is replaced for the whole cluster at once. That was D24's trade, and
this record keeps it rather than reopening it.

**The secret is at least 256 bits, and a shorter one is refused at start.** A member that
dials an impostor hands it a proof, and a proof is an offline test of any guess at the
secret. Against 256 random bits that is harmless. Against a word somebody chose it is fatal.

## Consequences

Mutual TLS with a cluster CA stays what D24 said it was: better, and a replacement for this
layer that nothing above it would notice. What changes when it arrives is where a certificate
comes from and whether the peer's is checked. The stream kinds, and everything behind them,
stay as they are.

The linter's objection to a TLS client that checks no certificate is right in general and
wrong here, and the code says why at the one place it is switched off.

`wegwitness` implements the same handshake with its own `crypto/tls`. It never forwards a
write, and it refuses one sent to it, having nothing to plan with.
