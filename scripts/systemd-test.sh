#!/bin/sh
# Installs, upgrades, removes and purges the deb package under a running
# systemd, and checks what D54 promises at each step.
#
#   make package VERSION=0.0.1 && make package VERSION=0.0.2
#   sudo scripts/systemd-test.sh dist/wegweiser_0.0.1_amd64.deb dist/wegweiser_0.0.2_amd64.deb
#
# The two packages must be different versions, the first older. Needs root
# and a running systemd, which CI's runner has. It refuses to run on a machine
# that already has weg on it, because cleaning up afterwards means deleting
# the state directory, and on such a machine that is a database.
set -eu

old=$1
new=$2
config=/etc/wegweiser/config.yaml
unit=/usr/lib/systemd/system/wegweiser.service
state=/var/lib/private/wegweiser

for path in /usr/bin/weg /etc/wegweiser /etc/systemd/system/wegweiser.service \
	"$unit" /var/lib/wegweiser "$state"; do
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
	dpkg --purge wegweiser >/dev/null 2>&1 || true
	systemctl reset-failed wegweiser 2>/dev/null || true
	rm -rf /etc/wegweiser /var/lib/wegweiser "$state"
}
trap cleanup EXIT

version_of() { dpkg-deb -f "$1" Version; }

# Waits for weg health to report the given version.
serving() {
	i=0
	while [ "$i" -lt 20 ]; do
		if weg health --output json 2>/dev/null | grep -q "\"version\": *\"$1\""; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	return 1
}

sum() { sha256sum "$1" | cut -d' ' -f1; }

echo "--- a configuration already there survives the first installation"
install -d -m 0755 /etc/wegweiser
# 127.0.0.1 rather than every address: systemd-resolved holds 127.0.0.53:53 on
# Ubuntu, and the default ":53" cannot be bound beside it.
(umask 077 && printf '%s\n' 'dns:' '  listen: "127.0.0.1:53"' 'database:' \
	'  path: "/var/lib/wegweiser/wegweiser.db"' >"$config")
before=$(sum "$config")
dpkg -i "$old" >/dev/null
[ "$(sum "$config")" = "$before" ] || fail "the installation changed $config"
echo ok

echo "--- installing starts nothing"
[ -f "$unit" ] || fail "no unit at $unit"
[ -f /usr/share/bash-completion/completions/weg ] || fail "no bash completion"
[ "$(systemctl is-active wegweiser || true)" = inactive ] || fail "the service is running"
[ "$(systemctl is-enabled wegweiser || true)" = disabled ] || fail "the service is enabled"
echo ok

echo "--- the unit starts it, with a configuration only root can read"
systemctl enable --now wegweiser
serving "$(version_of "$old")" || fail "weg health did not report $(version_of "$old")"
echo ok

echo "--- an upgrade restarts a running server on the new binary"
dpkg -i "$new" >/dev/null
serving "$(version_of "$new")" || fail "weg health did not report $(version_of "$new")"
[ "$(sum "$config")" = "$before" ] || fail "the upgrade changed $config"
echo ok

echo "--- an upgrade leaves a stopped server stopped"
systemctl stop wegweiser
dpkg -i "$new" >/dev/null
[ "$(systemctl is-active wegweiser || true)" = inactive ] || fail "the reinstallation started the service"
echo ok

echo "--- a missing configuration stops the start rather than running on defaults"
mv "$config" "$config.away"
if systemctl start wegweiser 2>/dev/null; then
	fail "the unit started without a configuration file"
fi
mv "$config.away" "$config"
echo ok

echo "--- a server that does not come back after an upgrade leaves dpkg working"
systemctl start wegweiser
serving "$(version_of "$new")" || fail "weg health did not report $(version_of "$new")"
mv "$config" "$config.away"
dpkg -i "$new" >/tmp/systemd-test-dpkg.log 2>&1 || {
	cat /tmp/systemd-test-dpkg.log >&2
	fail "dpkg failed because the restart did"
}
dpkg-query -W -f '${Status}' wegweiser | grep -q 'install ok installed' ||
	fail "the package is $(dpkg-query -W -f '${Status}' wegweiser)"
mv "$config.away" "$config"
echo ok

echo "--- removing keeps the configuration and the database"
dpkg -r wegweiser >/dev/null
[ ! -e "$unit" ] || fail "the unit is still there"
[ -f "$config" ] || fail "removing took $config"
[ -f "$state/wegweiser.db" ] || fail "removing took the database"
echo ok

echo "--- purging takes the configuration and keeps the database"
dpkg -i "$new" >/tmp/systemd-test-dpkg.log 2>&1
grep -q "enable --now" /tmp/systemd-test-dpkg.log ||
	fail "installing again after a removal does not say how to start it"
dpkg --purge wegweiser >/dev/null
[ ! -e /etc/wegweiser ] || fail "purging left /etc/wegweiser"
[ -f "$state/wegweiser.db" ] || fail "purging took the database"
echo ok

echo "--- a first installation writes the example as the configuration, mode 0600"
dpkg -i "$new" >/dev/null
cmp -s "$config" /usr/share/wegweiser/wegweiser.example.yaml || fail "$config is not the example"
[ "$(stat -c '%a %U' "$config")" = "600 root" ] || fail "$config is $(stat -c '%a %U' "$config")"
echo ok

echo "--- exposure"
score=$(systemd-analyze security --offline=true "$unit" | tail -1 |
	awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^[0-9]+\.[0-9]+$/) { print $i; exit } }')
echo "systemd-analyze security: $score"
awk -v s="$score" 'BEGIN { exit !(s != "" && s <= 1.4) }' ||
	fail "the unit scores $score, above the 1.4 it had"
echo ok
