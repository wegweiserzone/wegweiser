# D54 — Wegweiser is installed from packages, out of a repository of its own

- Decided: 2026-11-01

## Context

A release is two archives, a `checksums.txt` and a container image. Installing from them is
five commands copied off a web page, and updating is the same five again, by hand, on every
machine. The checksums come from the same place as the archives, so they show that a
download is intact and say nothing about where it came from.

The conventions fix what is delivered: one static binary, a container image, a systemd
unit. Anything that installs or updates them is not on that list, so it starts here.

Three shapes were weighed: a `weg install` and `weg upgrade` that know their own version, a
shell script piped from the website, and packages for the distributions' own managers.

## Decision

**Wegweiser and `wegwitness` are delivered as deb and rpm packages from a repository the
project runs, at `pkg.wegweiser.zone`.** The archives and the image stay. A package manager
already does what an installer would have to write again: it checks a signature, knows which
files are its own, can be run twice, removes what it put there, and goes through whatever
proxy or mirror the machine already uses. A subcommand or a script would be a second
implementation of exactly the parts that must not be wrong, written beside the one the
administrator's system already trusts.

The distributions' own archives were the other place to be. Debian wants every Go dependency
packaged on its own, which takes months and then sets the pace of every release. A hosted
service would hold the signing key, which is the one part of this worth guarding.

**The repository is derived from the releases.** Each publish rebuilds it whole from the
GitHub releases of both projects, so it holds no state of its own that could be lost, and it
keeps every version, so a new member can be given the one its cluster runs. A pre-release is
left out.

**One OpenPGP key signs it, and it never leaves the workflow that publishes.** It is RSA-4096,
because the EdDSA support in RHEL 9's rpm arrived in a later update and an older system
refuses the signature. It has no expiry, because a key that expires stops `apt update` on
every machine whose owner did not fetch it again in time; if the key is lost, a revocation
certificate kept offline withdraws it. The key is a secret in a protected environment, and
the workflow runs only when the maintainer approves it. The release workflows never see it.
Its fingerprint is printed in the README and the documentation, so that it can be checked
somewhere other than where it was downloaded.

**The package does not decide what the administrator decides.** It does not start the
service on installation. On Ubuntu and Fedora, systemd-resolved holds `127.0.0.53:53`, and
the default `:53` cannot be bound beside it, so a service started there fails at once. It
does not own the configuration file either. It writes one from the example when there is
none, mode 0600, and leaves it alone afterwards: as a conffile, dpkg would ask about it on
every release whose example changed by a comment, and on an installation over a file that
was already there. It never removes the database, on purge included.

**The unit reads the configuration as a systemd credential.** With `DynamicUser=yes` there is
no user to own a file of mode 0600, so a configuration the service can read is one every
account on the host can read, the cluster secret in it included. `LoadCredential=` has
systemd read the file as root and hand it to the service alone.

**An upgrade's risk is the administrator's.** The package restarts a running service and
does nothing else. It takes no backup, does not roll back, and does not order the members of
a cluster. dpkg cannot roll back, and a maintainer script that fails leaves the package
manager stuck for everything else on the machine. The order cannot be checked either,
because no member says which version it runs (`weg cluster status` carries none), and
coordinating members from one machine is installing on another, which the scope fence rules
out for good.

The administrator is told instead. The changelog says so under its own heading when a
release migrates the database, because an older build refuses the database afterwards and
the way back is a backup taken before. It says so too when a release changes what a batch
is, because then the order of a rolling upgrade matters
([D29](d29-a-node-that-cannot-apply.md)).

## Consequences

The delivery line among the conventions' fixed decisions gains the packages and the
repository.

The unit already required systemd 247 or later, and so do the packages: Debian 12, Ubuntu
22.04, RHEL 9 and their successors, current Fedora and openSUSE Leap 15.6.

A cluster member is held at its version and upgraded on purpose, one member at a time. The
documentation says how, and says that an unattended upgrade reaches all members in the same
hour.

A unit installed by hand under `/etc/systemd/system` takes precedence over the packaged one
in `/usr/lib/systemd/system`. Moving to the package means removing it, or the old unit keeps
running unnoticed until the day the two differ.

The repository's index carries no `Valid-Until`. Setting one means signing again on a
schedule, which needs the key without anybody approving it. So a mirror, or somebody between
it and the client, can keep serving an older index that is still correctly signed, and hold
a machine at an older release without it noticing.

## Where this stands

Not built.
