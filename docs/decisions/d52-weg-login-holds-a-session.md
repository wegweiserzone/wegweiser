# D52 — `weg login` holds the session a browser would, and a provider's answer reaches it over loopback

- Decided: 2026-10-25

## Context

[D51](d51-accounts-come-from-an-identity-provider.md) lets a person sign in through a
directory or an OpenID Connect provider, and the sign-in page offers both. The CLI takes a
token and nothing else, so somebody whose account lives in FreeIPA can use the interface and
not `weg`. Architecture invariant 1 rules that out.

Two things are missing. The CLI has nowhere to keep what a sign-in hands it. And an OpenID
Connect sign-in ends at the server's callback, which sets the session as a cookie in the
browser: the browser holds it, and the terminal that asked for it never sees it.

## Decision

**The CLI holds the same session a browser does.** Nothing new is minted. `weg login` ends
with the sealed session value and its CSRF value, and every later command sends them back as
the `weg_session` cookie and the `X-Wegweiser-CSRF` header, which the server already checks.
Signing in still writes nothing, so it works while the cluster is read-only, and the session
ends when a browser's would.

**It is kept in a file only its owner can read.** The file sits in the user's configuration
directory, mode 0600 in a directory of 0700, and holds one session per server address. A
token given by `--token` or `$WEG_TOKEN` beats a stored session, because what the person
typed for this command is what they meant. `weg logout` ends the session on the server, as
signing out of the interface does, and removes it from the file whether or not the server
could record the end.

**A directory's password is asked for in the terminal, without echo,** and posted to
`/auth/ldap/{providerId}`. It is never read from a flag or the environment. The CLI refuses
before sending anything when the address is plain HTTP to another machine: the server
refuses that too, but only once the password has crossed the network.

**An OpenID Connect sign-in returns to the CLI through the server's own callback.** The CLI
listens on a free port of 127.0.0.1, makes a key pair for this one sign-in, and opens the
browser at `/auth/oidc/{providerId}/start` with the port and the public key, which the start
seals into the flow cookie beside the rest. The provider returns to the redirect URI the
server registered, as it does for the interface. Where the flow names a port, the callback
does not set cookies: it sends the browser on to `http://127.0.0.1:{port}/` with the session
and its CSRF value sealed to that key by HPKE (RFC 9180). A failure goes the same way, so the
terminal says why.

Going through the server, rather than having the provider redirect to the CLI, decides three
things. The provider is left alone: no loopback redirect URIs to register, which every
provider handles differently, and the client secret stays where the code exchange needs it.
The server builds the loopback address from a port number, so the callback cannot be made to
redirect anywhere else. And what lands in the browser's history, which browsers sync to
accounts elsewhere, is a value only the CLI's key opens, a key that lives as long as the
command.

## Consequences

The browser starts at the address the CLI uses for the server, so that address needs a
redirect URI at the provider, as the interface's does. A session is stored per address, so
signing in at one member does not carry to the address of another, though the session would
work there.

**Approving a sign-in by code, for a terminal with no browser beside it, is not built.** It
has a problem the loopback does not. Between the CLI showing a code and the person approving
it elsewhere, a pending approval has to be kept where both the CLI's polling and the
approving browser reach it, and in a cluster those can be two members. Replicating it makes
every such sign-in a write that needs quorum, which D51 kept signing in free of. Keeping it
on one member binds both halves to that member, which a load balancer does not respect and a
restart forgets. The device authorization grant of RFC 8628 avoids both, because the pending
state is kept by the provider and the server can hand the CLI the device code sealed under the
session key; it reaches only providers that offer the grant to this client. Until one of
these is chosen, a directory works over SSH as it is, a token works anywhere, and the
loopback works through `ssh -L` when `weg login --no-browser --port` prints the address
instead of opening it and listens on a port fixed in advance.

## Where this stands

Built as described. The server's half is in `internal/api/oidc.go`, the seal in
`internal/api/handoff`, which both halves import, and the CLI's in `internal/cli/login.go`
and `internal/cli/sessions.go`. Approving a sign-in by code is not built.
