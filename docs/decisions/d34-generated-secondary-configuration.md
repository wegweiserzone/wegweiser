# D34 — The secondary's configuration is generated, not installed

- Decided: 2026-08-29

## Context

Setting up the second nameserver ends with a step this server cannot help with today. The
documentation hands the reader a BIND block and a Knot block to copy, with the primary's
address written in as an example and the key's secret left as a placeholder. Everything the
reader then fills in is something this server already holds: the zones, the reverse zones
among them, the key's name and algorithm, and the secret itself, which unlike an API token
can be read back ([D28](d28-tsig.md)).

Two of those are easy to get wrong in a way that stays quiet. Knot ignores a notification
without an `acl` naming the primary, so the zone is correct and the news takes an hour, and
nothing anywhere says why. The algorithms are stored in domain name syntax with the trailing
dot RFC 8945 gives them, and both servers want them without it.

It is also where the product thesis stops holding. Getting to a working pair of nameservers
means opening somebody else's manual, and that is the one thing the thesis promises it will
not.

## Decision

**The server renders the configuration and both clients ask for it.** Architecture invariant
1 settles where the templates live: putting them in the CLI would mean writing them a second
time in TypeScript for the interface, and two copies of another program's syntax drift apart
quietly.

**It writes a file. It does not install one.** Configuring the secondary would mean
credentials for a machine this one does not own, a way to restart a service there, and the
file layout of every distribution. It would also invert the trust direction: a compromised
Wegweiser hands over the zones today, and would hand over the secondaries as well. The
output being an ordinary file is what makes it composable with the tools that do own that
machine.

**The primary's address is given, never guessed.** This server does not know which of its
addresses the world reaches it on, and a hidden primary, which is the arrangement transfer
was built for, is named by no NS record to ask. A wrong address here produces a
configuration that looks right and fetches nothing.

**BIND and Knot.** They are the two the documentation already covers, and the shape it gives
them was checked against running servers. NSD is a template away when somebody can run one.
PowerDNS is not, and the reason is not effort: zones there live in a backend rather than a
file, so the equivalent is a list of `pdnsutil` commands, which is a different thing to
generate and to be wrong about.

**The output is deterministic.** No timestamp and no version stamp, so that regenerating it
under configuration management produces no diff when nothing has changed. The header names
the command that made it.

**It reports what will not work before it is deployed.** The facts needed to render are the
same ones that answer whether the arrangement can function at all: an empty transfer list, a
key that was created but never put on that list, a secondary absent from the notify list.
Each of those produces a configuration that is syntactically perfect. They are warnings in
the sense [D31](d31-what-a-zone-check-reports.md) gives the word, and they go to standard
error so that standard output stays a file.

**Checking the syntax is delegated.** `named-checkconf` and its equivalents belong to the
software being configured, and depending on foreign binaries would put them in the test
matrix. The commands are documented instead, which is what `weg zone export` already does
with `named-checkzone`.

## Consequences

**The documentation shrinks rather than grows.** The hand-written blocks stop being
instructions and become an illustration of what the command prints.

**The output carries a secret in clear.** That is the point of it, and the endpoint needs the
`admin` scope for the same reason reading a key's secret does. The file is a credential and
the command's help says so.

**Foreign syntax is a maintenance obligation, and it is bounded on purpose.** What is
generated is the smallest subset that does the job: a key, a remote or server, and one stanza
per zone. Those have been stable in both programs for years, and CI cannot run either
program, so the defence is a small target rather than a test.

**`weg secondary` is a client-side noun.** It names the other end of a transfer this server
serves. Should being a secondary ever come inside the scope fence, a zone received from
elsewhere is a zone, and it belongs under `weg zone` with a kind of its own rather than here.

**Asking the secondary whether it worked is not this record.** A serial probe against the
configured targets, and the lag metric it would feed, is a separate feature with its own
questions about timeouts and about who does the asking. Nothing here forecloses it.
