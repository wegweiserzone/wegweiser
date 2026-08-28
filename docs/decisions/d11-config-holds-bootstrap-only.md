# D11 — The config file holds bootstrap settings only

The file contains listen addresses, store DSN, TLS material and log level. Everything else
(zones, records, reverse policy, tokens, retention) lives in the database and is therefore
reachable through the API.

Precedence: flags → environment → file.

Invariant 1 says no feature exists in only one client. A setting that lives only in a file is
a feature that exists only for whoever can SSH to the box.

**Accepted consequence:** there is no way to configure a zone without a running server. Some
operators dislike that. The answer for them is `weg apply -f zones.yaml` against the YAML
export, which is a better GitOps story than a hand-edited config file, because it round-trips
and it validates.
