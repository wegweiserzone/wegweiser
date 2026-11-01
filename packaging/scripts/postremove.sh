#!/bin/sh
# Runs after the package's files have gone. dpkg passes "remove", "purge" or
# "upgrade"; rpm passes 0 on removal and 1 on upgrade, and has no purge.
#
# The database is never touched, purge included: it is the zones, their whole
# history and the keys, and no package manager gets to decide that those go
# (docs/decisions/d54-installed-from-packages.md).
set -e

if [ "$1" = purge ]; then
	rm -f /etc/wegweiser/config.yaml
	rmdir /etc/wegweiser 2>/dev/null || true
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
