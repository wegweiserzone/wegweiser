#!/bin/sh
# Runs once the package's files are in place. dpkg passes "configure" and the
# version configured before, which is empty on a first installation; rpm passes
# 1 on a first installation and 2 or more on an upgrade.
#
# The service is never started from here. On Ubuntu and Fedora the default
# ":53" cannot be bound beside systemd-resolved, and the configuration is the
# administrator's to write first (docs/decisions/d54-installed-from-packages.md).
set -e

case "$1" in
configure)
	if [ -n "${2:-}" ]; then upgrade=yes; else upgrade=no; fi
	;;
[0-9]*)
	if [ "$1" -ge 2 ]; then upgrade=yes; else upgrade=no; fi
	;;
*)
	exit 0
	;;
esac

config=/etc/wegweiser/config.yaml

# Written once, and never owned by the package. Mode 0600 because it can hold
# the cluster's secret; the unit hands it to the service as a credential.
if [ ! -e "$config" ]; then
	install -d -m 0755 /etc/wegweiser
	install -m 0600 -o root -g root /usr/share/doc/wegweiser/wegweiser.example.yaml "$config"
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	# A server that was running runs the new binary; one that was stopped
	# stays stopped. One that does not come back is reported, not fatal: a
	# failing script would leave the package manager stuck for everything else
	# on the machine.
	if [ "$upgrade" = yes ] && ! systemctl try-restart wegweiser.service; then
		echo "wegweiser did not restart; journalctl -u wegweiser says why" >&2
	fi
fi

if [ "$upgrade" = no ]; then
	echo "Edit $config, then start the server: systemctl enable --now wegweiser"
fi
