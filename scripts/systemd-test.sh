#!/bin/sh
# Installs weg the way the deployment page does, under a running systemd, and
# checks that the unit starts it with a configuration only root can read.
#
#   sudo scripts/systemd-test.sh [path/to/weg]
#
# Needs root and a running systemd, which CI's runner has. It refuses to run
# on a machine that already has weg installed, because cleaning up afterwards
# means deleting the state directory, and on such a machine that is a database.
set -eu

bin=${1:-bin/weg}
unit=packaging/systemd/wegweiser.service

for path in /usr/bin/weg /etc/wegweiser /etc/systemd/system/wegweiser.service \
	/var/lib/wegweiser /var/lib/private/wegweiser; do
	if [ -e "$path" ]; then
		echo "systemd-test: $path exists; this is for a machine without weg on it" >&2
		exit 2
	fi
done

fail() {
	echo "FAIL: $*" >&2
	journalctl -u wegweiser --no-pager -n 30 >&2 || true
	exit 1
}

cleanup() {
	systemctl stop wegweiser 2>/dev/null || true
	systemctl reset-failed wegweiser 2>/dev/null || true
	rm -rf /usr/bin/weg /etc/systemd/system/wegweiser.service /etc/wegweiser \
		/var/lib/wegweiser /var/lib/private/wegweiser
	systemctl daemon-reload
}
trap cleanup EXIT

healthy() {
	i=0
	while [ "$i" -lt 20 ]; do
		weg health >/dev/null 2>&1 && return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}

install -m 0755 "$bin" /usr/bin/weg
install -m 0644 "$unit" /etc/systemd/system/wegweiser.service
install -d -m 0755 /etc/wegweiser
# 127.0.0.1 rather than every address: systemd-resolved holds 127.0.0.53:53 on
# Ubuntu, and the default ":53" cannot be bound beside it. The database goes
# where the example configuration puts it.
(umask 077 && printf '%s\n' 'dns:' '  listen: "127.0.0.1:53"' 'database:' \
	'  path: "/var/lib/wegweiser/wegweiser.db"' >/etc/wegweiser/config.yaml)
systemctl daemon-reload

echo "--- a configuration of mode 0600 is read"
systemctl start wegweiser || fail "the unit did not start"
healthy || fail "weg health did not answer within 20 seconds"
echo ok

echo "--- a missing configuration stops the start rather than running on defaults"
systemctl stop wegweiser
mv /etc/wegweiser/config.yaml /etc/wegweiser/config.yaml.away
if systemctl start wegweiser 2>/dev/null; then
	fail "the unit started without a configuration file"
fi
mv /etc/wegweiser/config.yaml.away /etc/wegweiser/config.yaml
echo ok

echo "--- exposure"
score=$(systemd-analyze security --offline=true "$unit" | tail -1 |
	awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+\.[0-9]+$/) { print $i; exit } }')
echo "systemd-analyze security: $score"
awk -v s="$score" 'BEGIN { exit !(s != "" && s <= 1.4) }' ||
	fail "the unit scores $score, above the 1.4 it had"
echo ok
