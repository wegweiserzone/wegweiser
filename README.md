<div align="center">

# Wegweiser

**An authoritative DNS server with a web interface you don't need a manual for.**

[![CI](https://github.com/wegweiserzone/wegweiser/actions/workflows/ci.yml/badge.svg)](https://github.com/wegweiserzone/wegweiser/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/wegweiserzone/wegweiser.svg)](https://pkg.go.dev/github.com/wegweiserzone/wegweiser)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)

</div>

---

Wegweiser is a single static binary that runs an authoritative DNS server. Start it, open the
web interface, and have a working zone in five minutes without learning zonefile syntax
first. It is a side project, written by one person who got tired of editing zonefiles by
hand. *Wegweiser* is German for signpost.

Versions are semantic, and the [changelog](CHANGELOG.md) carries the compatibility note for
each release. Migrations run at startup and only ever go forward, so a database written by a
newer build is refused rather than downgraded and the way back is a backup.

## What it does

**Reverse zones manage themselves.** Add an `A` record and the matching `PTR` appears in the
reverse zone responsible for it, IPv6 nibble zones and RFC 2317 classless delegation
included. Delete the record and the `PTR` goes with it. A conflict is shown, never silently
overwritten.

**Every change is reversible.** Each edit is a journal event, so zone history, a diff of any
change, an audit trail and rollback to an earlier state all come out of one mechanism.

**Two clients, one API.** A dense, keyboard-driven web interface with a command palette and a
live query stream, and a CLI that reaches everything the interface does. Neither of them
touches the database: both are clients of the same REST API, and so is anything you write
yourself.

Also here: authoritative UDP and TCP with EDNS0, zonefile import and export, outbound zone
transfer to the secondaries you name, signed with TSIG and announced with NOTIFY, SQLite
persistence, token authentication, Prometheus metrics. Single node.

## What it does not do

**It does not resolve.** No recursion, no forwarding, no cache. That one is settled rather
than pending ([D17](docs/decisions/d17-no-recursion.md)), and a name outside
its zones is answered with REFUSED. A network that wants both its own zones and the internet
runs a resolver, hands *that* out over DHCP, and points a stub zone at Wegweiser. Both fit on
one machine, on different ports.

Everything else it does not do yet is on the scope fence in
[docs/conventions.md](docs/conventions.md), which says what is in, what is out, and what each
pending item is waiting on. The fence moves with the code rather than with a tag, so it is
the list to trust.

## Getting started

### Install

Every release carries a static binary for linux/amd64 and linux/arm64, the licence beside it,
and a `checksums.txt` over the lot. Take the archive for your architecture and that checksum
file from the [latest release](https://github.com/wegweiserzone/wegweiser/releases/latest),
then:

```console
$ sha256sum -c --ignore-missing checksums.txt
$ tar xzf weg_*_linux_amd64.tar.gz
$ ./weg version
```

Or as a container, which is `scratch` with the one binary in it:

```console
$ podman run --rm --cap-add=NET_BIND_SERVICE -p 53:53/udp -p 53:53/tcp -p 8053:8053 \
    -v weg:/var/lib/wegweiser ghcr.io/wegweiserzone/wegweiser:latest
```

`--cap-add=NET_BIND_SERVICE` is what binding port 53 needs; the server never wants root.

### First start

Run it somewhere writable, on ports that need no capability at all:

```console
$ ./weg serve --listen 127.0.0.1:5300 --api-listen 127.0.0.1:8053 --db ./weg.db
weg is answering on 127.0.0.1:5300 — 0 zones, 0 records from ./weg.db
the API is on http://127.0.0.1:8053
weg: this is the first start. The administrator token is shown once:

    weg_...

Store it now; only its hash is kept.
```

Open the API address in a browser and paste the token there, or hand it to the CLI:

```console
$ export WEG_SERVER=http://127.0.0.1:8053 WEG_TOKEN=weg_...
$ weg zone create example.com
$ weg zone create 192.168.0.0/24        # becomes 0.168.192.in-addr.arpa
$ weg record add example.com www A 192.168.0.10
added www.example.com. 3600 IN A 192.168.0.10
  generated 10.0.168.192.in-addr.arpa. 3600 IN PTR www.example.com.
```

Nobody wrote that second line, and it is on the wire:

```console
$ dig +short @127.0.0.1 -p 5300 www.example.com
192.168.0.10
$ dig +short @127.0.0.1 -p 5300 -x 192.168.0.10
www.example.com.
```

The reverse zone had to exist first. Wegweiser fills the reverse zones it holds; it does not
conjure them behind your back.

To see a fuller instance without installing one, `make demo` builds the binary, starts it on
unprivileged ports and fills it with what a small network actually looks like, reverse zones
and all. `make demo-stop` takes it away again.

### Running it for real

Bootstrap settings come from a config file, the environment or a flag, in that order of
precedence. [docs/wegweiser.example.yaml](docs/wegweiser.example.yaml) documents every one of
them, the packaging looks for it at `/etc/wegweiser/config.yaml`, and `weg config show`
prints what a server would start with and where each value came from. Everything else lives
in the database and is reachable through the API.

The API listens on loopback by default, because it can change every zone this server answers
for. Putting TLS and a reverse proxy in front of it is the intended way to expose it. A
sandboxed systemd unit is in [packaging/systemd](packaging/systemd/wegweiser.service).

## Building

Needs the Go version in [go.mod](go.mod), or newer. No cgo, no C toolchain.

```console
$ git clone https://github.com/wegweiserzone/wegweiser
$ cd wegweiser
$ make build
$ ./bin/weg version
```

```console
$ make check     # the gate: format, tidy, vet, lint, tests, web interface
$ make test      # tests with the race detector
$ make help      # all targets
```

## Documentation

| Document | What it covers |
| --- | --- |
| [Conventions](docs/conventions.md) | Product thesis, architecture invariants, scope fence, the design bar |
| [Decisions](docs/decisions/) | Every settled question, one record each, and the reasoning |
| [Configuration](docs/wegweiser.example.yaml) | Every bootstrap setting, and what it is for |
| [Changelog](CHANGELOG.md) | What each release moved |

## How this is written

Parts of this repository were written with an AI assistant, and I would rather say so than
let you work it out. Not all of it. The direction is mine, so is every call recorded in
[docs/decisions/](docs/decisions/), and so is what goes in and what stays out. What I
hand over is the writing: the documentation and the decision records are drafted from a
paragraph I write first, and some of the code is too. I read what comes back, and the test
suite is what keeps it honest: the race detector, fuzz targets on the wire parser, a browser
suite over the web interface, and a linter that enforces the architecture mechanically. All
of it runs on every push, and it is the same gate the code I typed myself has to pass.

Two reasons. I am one person doing this in my spare time, and it saves an enormous amount of
it. And it gets me to a working answer on things I would otherwise have spent a fortnight
fighting through, which is time I would rather put into the parts that are actually hard to
get right.

That is settled, and not an invitation to argue about it. If it puts you off, fair enough,
though the issue tracker is not the place to tell me. If the software is useful to you
anyway, you are very welcome here.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Contributions are certified with a `Signed-off-by`
line ([DCO](https://developercertificate.org/)); there is no CLA.

Security issues: please follow [SECURITY.md](SECURITY.md) rather than opening a public issue.

## License

[GNU Affero General Public License v3.0](LICENSE).

Chosen deliberately for a network-facing server: if you run a modified Wegweiser as a
service, its users are entitled to the source. Note that this is more restrictive than
PowerDNS (GPLv2), Knot DNS (GPLv3) and CoreDNS (Apache-2.0).
