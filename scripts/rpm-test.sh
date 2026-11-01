#!/bin/sh
# Installs, upgrades and removes the rpm package, and checks what D54 promises
# about the files. Meant for a throwaway container of an rpm distribution,
# which has no running systemd: what the unit does is scripts/systemd-test.sh's
# to check, under dpkg.
#
#   scripts/rpm-test.sh dist/wegweiser-0.0.1-1.x86_64.rpm dist/wegweiser-0.0.2-1.x86_64.rpm
set -eu

old=$1
new=$2
config=/etc/wegweiser/config.yaml
example=/usr/share/doc/wegweiser/wegweiser.example.yaml

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

sum() { sha256sum "$1" | cut -d' ' -f1; }

echo "--- a configuration already there survives installation and upgrade"
install -d -m 0755 /etc/wegweiser
(umask 077 && printf 'dns:\n  listen: "127.0.0.1:53"\n' >"$config")
before=$(sum "$config")
rpm -i "$old"
[ "$(sum "$config")" = "$before" ] || fail "the installation changed $config"
rpm -U "$new"
[ "$(sum "$config")" = "$before" ] || fail "the upgrade changed $config"
echo ok

echo "--- the files are where they belong"
weg version
for f in /usr/bin/weg /usr/lib/systemd/system/wegweiser.service "$example" \
	/usr/share/bash-completion/completions/weg /usr/share/zsh/site-functions/_weg \
	/usr/share/fish/vendor_completions.d/weg.fish; do
	[ -f "$f" ] || fail "$f is missing"
done
echo ok

echo "--- removing keeps the configuration"
rpm -e wegweiser
[ -f "$config" ] || fail "removing took $config"
echo ok

echo "--- a first installation writes the example as the configuration, mode 0600"
rm -rf /etc/wegweiser
rpm -i "$new"
[ "$(sum "$config")" = "$(sum "$example")" ] || fail "$config is not the example"
[ "$(stat -c '%a %U' "$config")" = "600 root" ] || fail "$config is $(stat -c '%a %U' "$config")"
echo ok
