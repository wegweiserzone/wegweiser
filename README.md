<div align="center">

# Wegweiser

**An authoritative DNS server with a web interface you don't need a manual for.**

[![CI](https://github.com/wegweiserzone/wegweiser/actions/workflows/ci.yml/badge.svg)](https://github.com/wegweiserzone/wegweiser/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/wegweiserzone/wegweiser.svg)](https://pkg.go.dev/github.com/wegweiserzone/wegweiser)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)

</div>

---

> [!WARNING]
> **Pre-alpha. There is no release yet, and no upgrade path between commits.**
> It answers queries and most of the v0.1 feature set is in place, but none of it has run in
> production and the on-disk format may still change without a migration. Follow the
> [changelog](CHANGELOG.md).

Wegweiser is a single static binary that runs an authoritative DNS server. It is a side
project, written by one person who got tired of editing zonefiles by hand. *Wegweiser* is
German for signpost.

## What it does today

Start the binary, open the web interface, have a working zone in five minutes, without
learning zonefile syntax first.

**Reverse zones manage themselves.** Add an `A` record, get the matching `PTR` in the right
reverse zone. IPv6 nibble zones and RFC 2317 classless delegation included. Delete it, the
`PTR` goes too. Conflicts are shown, never silently overwritten.

**Every change is reversible.** Each edit is a journal event, so zone history, a diff of any
change, an audit trail and rollback to an earlier state all come from one mechanism.

**Two clients, one API.** A dense, keyboard-driven web interface with a command palette and
a live query stream, and a CLI that reaches everything the interface does.

Also here: authoritative UDP and TCP with EDNS0, zonefile import and export, SQLite
persistence, token-authenticated REST API, Prometheus metrics. Single node.

## What it does not do

Not built, and not in v0.1: clustering, `weg tui`, DNSSEC, recursion and caching, outbound
AXFR/IXFR, DoT/DoH/DoQ, PostgreSQL, split-horizon views. Some of these are planned and the
journal and `Store` interface are shaped to fit them later; none of them exists today.

Nothing here has run in production, and the numbers in
[docs/decisions.md](docs/decisions.md) D12 are targets rather than measurements of a real
deployment.

## Building

Requires Go 1.26.5 or newer. No cgo, no C toolchain.

```console
$ git clone https://github.com/wegweiserzone/wegweiser
$ cd wegweiser
$ make build
$ ./bin/weg version
```

```console
$ make check     # everything CI runs: tidy, format, vet, lint, tests
$ make test      # tests with the race detector
$ make help      # all targets
```

## Documentation

| Document | What it covers |
| --- | --- |
| [Decisions](docs/decisions.md) | Settled design questions and the reasoning |
| [ADRs](docs/adr/) | Architecture decision records |
| [Conventions](docs/conventions.md) | Product thesis, invariants, scope fence, the design bar |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Contributions are certified with a `Signed-off-by`
line ([DCO](https://developercertificate.org/)); there is no CLA.

Security issues: please follow [SECURITY.md](SECURITY.md) rather than opening a public issue.

## License

[GNU Affero General Public License v3.0](LICENSE).

Chosen deliberately for a network-facing server: if you run a modified Wegweiser as a
service, its users are entitled to the source. Note that this is more restrictive than
PowerDNS (GPLv2), Knot DNS (GPLv3) and CoreDNS (Apache-2.0).
