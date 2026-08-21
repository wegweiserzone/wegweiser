# ADR 0005 — internal/zone depends on the wire library

- Status: accepted
- Date: 2026-08-16

## Context

`internal/zone` is specified as pure model: no persistence, no transport, so that
validation and the reverse-mapping rules stay exhaustively testable without a server.
`depguard` enforces that by denying `net/http`, `internal/store`, `internal/journal` and
`internal/api`.

The model still needs the DNS vocabulary. Record types and classes have IANA numbers and
mnemonics, unknown types need the `TYPE<n>` form of RFC 3597 §5, and, most expensively,
RDATA has to be parsed, validated and re-rendered in a canonical presentation form for
every record type we accept, as ADR 0001 requires.

`github.com/miekg/dns` already carries all of it, and it is the fixed choice for the wire
protocol.

## Decision

`internal/zone` may import `github.com/miekg/dns`, and does so for the type and class
tables and for RDATA parsing and printing.

It may not import anything from `internal/`, `net/http`, or the wire library's *server*
surface. The dependency is on the library as a DNS codec and vocabulary, not as a server.

## Rationale

The alternative is writing our own RDATA parser and printer. That is several dozen record
types, each with its own presentation syntax, plus the RFC 3597 unknown-type form: weeks of
work whose only possible outcome is to be slightly worse than a library that has been
carrying production traffic for a decade. Re-implementing it would be the opposite of the
project's own rule about checking behaviour against the RFC rather than against intuition:
we would be re-deriving from the RFC what someone else already derived and had corrected by
users.

"Pure model" was never meant as "zero dependencies". It means the package holds no state,
opens no connections, and can be tested by calling functions with values. Importing a codec
does not change any of that. What the rule is actually protecting against is a model that
cannot be constructed without a database or a socket, and that protection is unaffected.

Numeric constants are declared as `RRType(dns.TypeA)` rather than as literals, so the
package has no magic numbers and cannot drift from the registry through a typo.

## Consequences

- The type and class mnemonic tables are the wire library's. Two of its entries, "None" for
  type 0 and "Reserved" for type 65535, are placeholders rather than usable mnemonics and
  would not parse back. `RRType.String` screens entries by shape (upper-case letters,
  digits and hyphens) so those two render in the RFC 3597 form instead. A round-trip test
  over all 65536 values guards it.
- A future change of wire library would touch `internal/zone`, not only `internal/dns`. The
  surface used is small and mechanical, so this is a contained risk rather than a lock-in.
- The `depguard` rule for `internal/zone` stays as it is: it denies transport and the
  internal packages, and says nothing about the wire library.
