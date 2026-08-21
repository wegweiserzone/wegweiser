#!/usr/bin/env bash
#
# Bring up a Wegweiser with something in it, to look at.
#
# It is a demonstration and not a fixture: the data is what a small network
# actually looks like, including the reverse zones that fill themselves, so
# what the interface shows is what a real one would.
#
# Nothing here touches a system directory. The database goes to a temporary
# directory and the ports are unprivileged, so it needs no capability and no
# root.

set -euo pipefail

DIR=${WEG_DEMO_DIR:-/tmp/weg-demo}
# Not 5353: mDNS holds that on most desktops, and 53 would need
# CAP_NET_BIND_SERVICE, which a demonstration has no business asking for.
DNS=${WEG_DEMO_DNS:-127.0.0.1:5300}
API=${WEG_DEMO_API:-127.0.0.1:8053}

usage() {
  cat <<USAGE
usage: scripts/demo.sh [start|stop]

  start   build weg, run it on unprivileged ports, and fill it (default)
  stop    stop the demo and remove its database

environment:
  WEG_DEMO_DIR   where the database goes    (default $DIR)
  WEG_DEMO_DNS   address for queries        (default $DNS)
  WEG_DEMO_API   address for the API and UI (default $API)
USAGE
}

stop() {
  if [[ -f "$DIR/pid" ]] && kill -0 "$(cat "$DIR/pid")" 2>/dev/null; then
    kill "$(cat "$DIR/pid")"
    # Give it a moment to release the sockets, so an immediate restart does
    # not fail on an address that is still winding down.
    for _ in $(seq 20); do
      kill -0 "$(cat "$DIR/pid")" 2>/dev/null || break
      sleep 0.1
    done
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

start() {
  stop
  mkdir -p "$DIR"

  local where
  for where in "$DNS" "$API"; do
    if taken "$where"; then
      echo "something is already listening on $where." >&2
      echo "stop it, or choose another: WEG_DEMO_DNS=127.0.0.1:5301 make demo" >&2
      exit 1
    fi
  done

  make --no-print-directory build

  ./bin/weg serve --listen "$DNS" --api-listen "$API" --db "$DIR/weg.db" >"$DIR/log" 2>&1 &
  echo $! >"$DIR/pid"

  for _ in $(seq 50); do
    grep -q 'the API is on' "$DIR/log" && break
    sleep 0.1
  done
  if ! grep -q 'the API is on' "$DIR/log"; then
    echo "the server did not start:" >&2
    cat "$DIR/log" >&2
    exit 1
  fi

  export WEG_SERVER="http://$API"
  WEG_TOKEN=$(grep -oE 'weg_[A-Za-z0-9_-]+' "$DIR/log" | head -1)
  export WEG_TOKEN

  weg() { ./bin/weg "$@" >/dev/null; }

  # A forward zone and the two reverse zones it will write into. Creating the
  # reverse zones is deliberate: they are offered, never conjured (D6).
  weg zone create example.com --ttl 3600
  weg zone create internal.lan --ttl 300
  weg zone create staging.example.com --ttl 60
  weg zone create 0.168.192.in-addr.arpa
  weg zone create 8.b.d.0.1.0.0.2.ip6.arpa

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


  local host=${DNS%:*} port=${DNS##*:}
  cat <<READY

  open    http://$API/
  token   $WEG_TOKEN

  queries dig +short @$host -p $port www.example.com
          dig +short @$host -p $port -x 192.168.0.25    # the PTR nobody wrote

  cli     export WEG_SERVER=http://$API WEG_TOKEN=$WEG_TOKEN
          ./bin/weg zone list

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
