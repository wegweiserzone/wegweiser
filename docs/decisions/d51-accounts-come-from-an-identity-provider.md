# D51 — Accounts come from an identity provider, and a session is signed rather than stored

- Decided: 2026-10-24
- Amends: [D5](d05-tokens-and-sessions.md), [D32](d32-what-else-the-cluster-replicates.md),
  [D40](d40-a-write-reaches-the-leader.md)

## Context

[D5](d05-tokens-and-sessions.md) gave the interface one way in: paste a token. That is fine
for one operator and wrong for a team, where a token passed around is a credential nobody
can take back from one person without taking it from all of them, and a commit reading
`admin` says nothing about who made it.

The places people already keep their accounts are FreeIPA, Active Directory and OpenLDAP on
one side, and Authentik, Keycloak, Entra and their kin on the other. The first speak LDAP,
the second OpenID Connect, and most of the second can also be put behind LDAP.

D5 kept sessions in memory and asked for that to be revisited with the cluster;
[D32](d32-what-else-the-cluster-replicates.md) kept them there, because a session through
Raft makes a login a consensus round that fails without quorum. The cost was a session that
holds on one member only. With people signing in through a provider that cost grows: behind
anything that spreads requests, a login works on one request and not the next.

## Decision

**A person signs in through a provider, and this server keeps no passwords.** A provider is
OpenID Connect, by the authorization code flow with PKCE, or LDAP, by binding as the person
over LDAPS or StartTLS and never in the clear. Several may be configured at once, and the
sign-in page offers each of them beside the token. A password typed for LDAP is passed to the
directory and forgotten. There are no local accounts, so there is no password storage, no
reset flow and nothing here to brute-force offline. SAML is not offered: every provider named
above speaks OpenID Connect, and SAML would bring XML signature validation, with its own long
record of flaws, for nobody who lacks the alternative.

**Groups map to the three scopes tokens already carry.** A provider's settings say which of
its groups is `read`, which `write` and which `admin`, and a person holds the strongest scope
any of their groups maps to. A person in no mapped group is refused, however well they
authenticated. One permission model serves tokens and people, and the API checks the same
thing for both. A scope covers every zone; a mapping that names zones fits the same place,
and is not built.

**Providers are settings, replicated as D32 replicates the rest.** A provider is configured
through the API like the transfer list, its client secret or bind password held in the
database like a TSIG secret ([D28](d28-tsig.md)), and none of it lives in the file
([D11](d11-config-holds-bootstrap-only.md)). An OpenID Connect provider lists the redirect
URIs it has registered, one per member, and a sign-in returns to the address the browser
used only when it is one of them, so a forged `Host` cannot send a code elsewhere.

**Signing in writes nothing.** No user record is made at sign-in, so signing in needs no
quorum and works while [D10](d10-quorum-loss-is-read-only.md) holds the cluster read-only,
which is when somebody most needs to look.

**A session is a signed cookie, not an entry in memory.** It carries who signed in, through
which provider, the scope they hold and when it ends, sealed under a session key the
cluster mints once and replicates, rotated the way the cookie secret is, with the previous
key still accepted, so that a rotation ends no session. Every member can read every session,
and a restart or a failover ends none. The CSRF value is derived from the session under the
same key, so the server still compares the header against a value it holds rather than
against a cookie the request brought. A session opened with a token is checked against that
token on every request, so revoking the token ends its sessions everywhere.

**Taking a session back is a write.** Signing out clears the browser's cookies and, where
the cluster takes writes, records that one session as ended. Ending every session of one
person is an administrator's act. Both are replicated, both need quorum, and a record of
either is dropped once the sessions it names would have ended anyway, so the list never
grows past what twelve hours can hold. A person removed from a group upstream keeps what
they had until their session ends, at most twelve hours, unless an administrator ends it
sooner.

**A commit names the person.** The journal's actor is the person's name and the provider
they came through, as it is the token's name for a token.

## Consequences

A provider that is down stops people signing in through it and nobody else: tokens and the
other providers go on working, and the bootstrap token stays the way in when every provider
is gone.

Signing out under quorum loss clears the browser and cannot do more. A copy of that cookie
taken beforehand works until it expires, which D5 had ruled out for a single node. That is
the price of a session every member can read without asking the others.

Two dependencies arrive: `github.com/coreos/go-oidc/v3` with `golang.org/x/oauth2`, and
`github.com/go-ldap/ldap/v3`. Both protocols are full of edges a hand-written client gets
wrong, and both libraries are what most Go programs speaking them use.

## Where this stands

Sessions are built as described, for the token sessions that exist: sealed in
`internal/api/session.go` under the `session_keys` setting, rotated with the cookie secret's
loop, and ended through the `ended_sessions` setting, which a member that does not lead hands
to the leader. Providers and their group mappings are held in the `identity_providers`
setting and managed through `/identity-providers`. Signing in through an OpenID Connect
provider is built in `internal/api/oidc.go`, the flow sealed in a cookie of its own under the
session key. Signing in through a directory is built in `internal/api/ldap.go`, and the
sign-in page offers both. `weg login` signs in through either from the CLI
([D52](d52-weg-login-holds-a-session.md)). Managing the providers in the interface, and ending every session
of one person, are not built.
