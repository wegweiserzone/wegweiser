# D5 — Named tokens with scopes; the GUI uses a session cookie

**Tokens.** Named API tokens with `read`, `write` and `admin` scopes. One bootstrap `admin`
token is generated on first start and printed to stdout exactly once. Tokens are 256 bits
from `crypto/rand`, shown in full only at creation, stored as SHA-256
(`internal/api/auth.go`).

No users, no passwords and no OIDC. Multi-user authentication is a distinct piece of
work (password storage, reset flows, group mapping) and does not belong in the same release
as the DNS core. The schema does not preclude it.

**Browser sessions.** The GUI does not hold a long-lived token. It posts the token once to
`POST /api/v1/auth/session` and receives an `httpOnly`, `Secure`, `SameSite=Strict` session
cookie. `localStorage` is readable by any script that gets injected; an `httpOnly` cookie is
not, which is the whole point.

**CSRF.** `SameSite=Strict` blocks cross-site form posts, but it is one layer. State-changing
requests authenticated by cookie must also carry an `X-Wegweiser-CSRF` header matching a
non-`httpOnly` companion cookie (double-submit). Requests authenticated by bearer token skip
the check: CSRF requires ambient credentials, and a bearer token is not ambient.

**Transport.** A session is refused over plain HTTP from anywhere but the machine the browser
runs on. The two silent alternatives are both worse: a cookie without `Secure` is one the
network can read, and a cookie with `Secure` is one the browser never sends back, which looks
like a login that does nothing and cannot be debugged from the page. Loopback stays allowed,
because nothing leaves the host and browsers treat the origin as trustworthy for that reason.
A bearer token works over any transport and is what a program should use.

**Where sessions live.** In memory, for now. A restart therefore ends every session and the
interface asks for the token again. A session is not zone data; it does not belong in the
journal, and persisting it would cost a table, four store methods and a migration for state
that is worth nothing after twelve hours. The seam is `sessionStore` in `internal/api`; a
store-backed one replaces it without the rest of the package noticing. Revisit when the
cluster arrives, because a session pinned to one node is a session lost at every failover.

**Obligation.** Token comparison is constant-time. Unknown, revoked and expired tokens return
the same error, so an attacker cannot distinguish them. Failed authentication is rate-limited
per source address. The CSRF header is compared against the value the server holds for the
session, not against the cookie the request carried: double-submit compares two things the
client sent because a stateless server has nothing better, and this one does.
