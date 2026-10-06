#!/usr/bin/env bash
#
# Bring up a Wegweiser with something in it, to look at.
#
# It is a demonstration and not a fixture: the data is what a small network
# actually looks like, including the reverse zones that fill themselves, so
# what the interface shows is what a real one would. By default it is three
# servers in one cluster, because a cluster is what a real deployment of more
# than one server is, and the Cluster page has nothing to show on a single one.
#
# Nothing here touches a system directory. The databases go to a temporary
# directory and the ports are unprivileged, so it needs no capability and no
# root.

set -euo pipefail

DIR=${WEG_DEMO_DIR:-/tmp/weg-demo}
# Not 5353: mDNS holds that on most desktops, and 53 would need
# CAP_NET_BIND_SERVICE, which a demonstration has no business asking for.
DNS=${WEG_DEMO_DNS:-127.0.0.1:5300}
API=${WEG_DEMO_API:-127.0.0.1:8053}
NODES=${WEG_DEMO_NODES:-3}

usage() {
  cat <<USAGE
usage: scripts/demo.sh [start|stop]

  start   build weg, run it on unprivileged ports, and fill it (default)
  stop    stop the demo and remove its databases

environment:
  WEG_DEMO_DIR     where the databases go                   (default $DIR)
  WEG_DEMO_DNS     address for queries to the first server  (default $DNS)
  WEG_DEMO_API     address for its API and UI               (default $API)
  WEG_DEMO_NODES   how many servers: 1, or a cluster of 3   (default $NODES)

The other servers of a cluster take the next ports: queries one up from the
first server's, the API ten up, and the cluster port one above each API.
USAGE
}

# Addresses for server n, counting from 1.
dns_of() { echo "${DNS%:*}:$((${DNS##*:} + $1 - 1))"; }
api_of() { echo "${API%:*}:$((${API##*:} + 10 * ($1 - 1)))"; }
cluster_of() { echo "${API%:*}:$((${API##*:} + 10 * ($1 - 1) + 1))"; }

stop() {
  local pidfile pid stopped=0
  for pidfile in "$DIR"/ns*/pid; do
    [[ -f "$pidfile" ]] || continue
    pid=$(cat "$pidfile")
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid"
      stopped=1
    fi
  done
  # Give them a moment to release the sockets, so an immediate restart does
  # not fail on an address that is still winding down.
  for pidfile in "$DIR"/ns*/pid; do
    [[ -f "$pidfile" ]] || continue
    for _ in $(seq 20); do
      kill -0 "$(cat "$pidfile")" 2>/dev/null || break
      sleep 0.1
    done
  done
  if ((stopped)); then
    echo "stopped the demo"
  fi
  rm -rf "$DIR"
}

# taken reports whether something is already listening, so the failure is a
# sentence rather than a bind error out of the middle of a startup.
taken() {
  local addr=$1
  (exec 3<>"/dev/tcp/${addr%:*}/${addr##*:}") 2>/dev/null && exec 3<&- && return 0
  return 1
}

# serve starts server n, joining the first when it is not the first, and waits
# until it answers. A server that joins answers only once the cluster's data
# has reached it, so the wait covers that too.
serve() {
  local n=$1 here="$DIR/ns$1"
  mkdir -p "$here"
  local args=(serve --listen "$(dns_of "$n")" --api-listen "$(api_of "$n")" --db "$here/weg.db")
  if ((NODES > 1)); then
    cat >"$here/config.yaml" <<CONFIG
cluster:
  id: "ns$n"
  listen: "$(cluster_of "$n")"
  advertise: "$(cluster_of "$n")"
  secret: "$SECRET"
CONFIG
    args+=(--config "$here/config.yaml")
    if ((n > 1)); then
      args+=(--join "$(cluster_of 1)")
    fi
  fi

  ./bin/weg "${args[@]}" >"$here/log" 2>&1 &
  echo $! >"$here/pid"

  for _ in $(seq 300); do
    grep -q 'the API is on' "$here/log" && return 0
    kill -0 "$(cat "$here/pid")" 2>/dev/null || break
    sleep 0.1
  done
  echo "ns$n did not start:" >&2
  cat "$here/log" >&2
  exit 1
}

start() {
  stop
  case "$NODES" in
    1 | 3) ;;
    *)
      echo "WEG_DEMO_NODES is $NODES; the demo runs 1 server, or a cluster of 3." >&2
      echo "Two servers make no honest cluster: docs/decisions/d25-cluster-shape.md says why." >&2
      exit 2
      ;;
  esac
  mkdir -p "$DIR"

  local n where addrs
  for n in $(seq "$NODES"); do
    addrs=("$(dns_of "$n")" "$(api_of "$n")")
    if ((NODES > 1)); then
      addrs+=("$(cluster_of "$n")")
    fi
    for where in "${addrs[@]}"; do
      if taken "$where"; then
        echo "something is already listening on $where." >&2
        echo "stop it, or choose other ports: WEG_DEMO_DNS=127.0.0.1:5310 WEG_DEMO_API=127.0.0.1:8153 make demo" >&2
        exit 1
      fi
    done
  done

  make --no-print-directory build

  # The cluster's shared secret. It lives as long as the demonstration does.
  SECRET=$(head -c 32 /dev/urandom | base64)

  serve 1

  export WEG_SERVER="http://$(api_of 1)"
  WEG_TOKEN=$(grep -oE 'weg_[A-Za-z0-9_-]+' "$DIR/ns1/log" | head -1)
  export WEG_TOKEN

  # The advisory notes the CLI writes to standard error are for somebody
  # setting a server up rather than for somebody watching one fill itself, so
  # they go to the log. A failure goes there too, and is then shown: `set -e`
  # would otherwise end the demonstration without saying what stopped it.
  weg() {
    if ! ./bin/weg "$@" >/dev/null 2>>"$DIR/ns1/log"; then
      echo "the demo stopped at: weg $*" >&2
      tail -3 "$DIR/ns1/log" >&2
      exit 1
    fi
  }

  # --- the cluster ----------------------------------------------------------
  # The first server starts the cluster, and the other two join it from their
  # first start, with empty databases (D44). Everything after this goes
  # through the cluster's log and reaches all three.
  if ((NODES > 1)); then
    weg cluster init
    serve 2
    serve 3
  fi

  local host=${DNS%:*} port=${DNS##*:}

  # --- the network --------------------------------------------------------
  # A forward zone and the reverse zones it writes into. The reverse zones are
  # created rather than conjured: they are offered, never invented (D6).
  weg zone create example.com --ttl 3600
  weg zone create internal.lan --ttl 300
  weg zone create staging.example.com --ttl 60
  weg zone create 0.168.192.in-addr.arpa
  weg zone create 8.b.d.0.1.0.0.2.ip6.arpa

  # A slice of that /24, delegated the way RFC 2317 describes. A host inside it
  # gets its reverse entry here, and the parent gets the CNAME pointing at it,
  # without anybody writing either one (D7).
  weg zone create 192.168.0.192/26

  # example.com answers for its own name server, so its check comes back clean.
  # staging is left without one on purpose: it is the zone with something to
  # find.
  weg record add example.com ns1 A 192.168.0.2

  weg record add example.com @      A     192.168.0.10
  weg record add example.com @      AAAA  2001:db8::10
  weg record add example.com @      MX    "10 mail.example.com."
  weg record add example.com @      TXT   "v=spf1 mx -all"
  weg record add example.com _dmarc TXT   "v=DMARC1; p=quarantine"
  weg record add example.com www    CNAME example.com.
  weg record add example.com mail   A     192.168.0.25
  weg record add example.com mail   AAAA  2001:db8::25
  weg record add example.com vpn    A     192.168.0.99 --ttl 300
  weg record add example.com '*.dev' A    192.168.0.50 --ttl 300

  weg record add internal.lan nas A 192.168.0.71
  weg record add internal.lan pbx A 192.168.0.80
  weg record add internal.lan cam A 192.168.0.200

  # --- what makes it worth looking at -------------------------------------
  # A second name on one address. The record is written and the reverse entry
  # is not, and both the write and the zone check say so rather than one name
  # quietly taking the address from the other (D3).
  weg record add example.com smtp A 192.168.0.25

  # A change, and the regret. The rollback writes forward to a new serial
  # rather than rewinding to an old one, because a secondary that has already
  # seen serial 14 will never ask for it again (D2). The history screen is
  # where this reads.
  local before
  before=$(./bin/weg zone show example.com | awk '/^serial/{print $2}')
  weg record update example.com vpn A 192.168.0.99 --data 192.168.0.250 \
    --comment "move the vpn endpoint"
  weg zone rollback example.com "$before" --yes --comment "it was not the vpn"

  # --- the other end of a transfer ----------------------------------------
  # A key, the transfer list it has to reach to grant anything, and the list of
  # who is told when a zone changes. Nobody is on either list until somebody is
  # named (D26, D27), which is why a demonstration has to name one.
  weg tsig create ns2.example.com.
  weg settings set --transfer-allow "key:ns2.example.com." --notify 192.168.0.53

  # A token that may read and nothing else, so the tokens screen shows what
  # scopes are for.
  weg token create reader --scope read

  # --- something to have answered -----------------------------------------
  # Without this the first screen a demonstration opens is empty. A spread
  # rather than a flood: two transports, several types, and the three response
  # codes an authoritative server has to tell apart.
  if command -v dig >/dev/null; then
    ask() { dig +tries=1 +timeout=1 "@$host" -p "$port" "$@" >/dev/null 2>&1 || true; }
    ask www.example.com A
    ask example.com MX
    ask example.com TXT
    ask mail.example.com AAAA
    ask nas.internal.lan A
    ask anything.dev.example.com A
    ask example.com ANY
    ask example.com AXFR +tcp
    ask -x 192.168.0.25
    ask -x 192.168.0.200
    ask -x 2001:db8::10
    # A name in a zone this server holds, and one in a zone it does not:
    # NXDOMAIN and REFUSED are different answers and the overview separates
    # them (D17).
    ask nope.example.com A
    ask notmine.test A
  fi

  cat <<READY

  open    http://$(api_of 1)/
  token   $WEG_TOKEN

  The zones a small network has, and the reverse entries nobody wrote.

    Zones          192.168.0.200 reverses inside 192/26.0.168.192.in-addr.arpa.,
                   and the /24 above it holds the CNAME that leads there. The
                   other hosts reverse in the /24 directly.
    example.com    check it: smtp and mail share an address, so one of them has
                   the reverse entry and the check says which.
    History        an edit and the rollback that undid it, both written
                   forward. Open the rollback and diff it.
    Secondaries    192.168.0.53 is on the notify list and nobody runs it, so
                   every zone reads unasked. Set one up writes the file BIND or
                   Knot would need.
    Overview       what the queries below did.
READY

  if ((NODES > 1)); then
    cat <<CLUSTER
    Cluster        three servers, ns1 leading, every one of them current. All
                   of the above went in through ns1 and reached the other two
                   through the cluster's log.

  servers ns1     http://$(api_of 1)/   queries on $(dns_of 1)
          ns2     http://$(api_of 2)/   queries on $(dns_of 2)
          ns3     http://$(api_of 3)/   queries on $(dns_of 3)
          The token works on every one; a write sent to any of them reaches
          the leader. Stop one and its row on the Cluster page reads not
          reached, while the other two go on taking writes:
          kill \$(cat $DIR/ns3/pid)
CLUSTER
  fi

  # A single server has no cluster to show, and says so with a 404.
  local third="./bin/weg secondary status"
  if ((NODES > 1)); then
    third="./bin/weg cluster status"
  fi

  cat <<READY

  queries dig +short @$host -p $port www.example.com
          dig +short @$host -p $port -x 192.168.0.25    # the PTR nobody wrote
          dig +short @$host -p $port -x 192.168.0.200   # and one through a CNAME

  cli     export WEG_SERVER=http://$(api_of 1) WEG_TOKEN=$WEG_TOKEN
          ./bin/weg zone list
          $third
          ./bin/weg zone check example.com --reverse

  stop    make demo-stop
READY
}

case "${1:-start}" in
  start) start ;;
  stop) stop ;;
  -h | --help | help) usage ;;
  *)
    usage >&2
    exit 2
    ;;
esac
