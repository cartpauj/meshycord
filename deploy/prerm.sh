#!/bin/sh
# Runs before removal. Stop the service, but leave the state directory alone —
# removing a package should not destroy someone's message history.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop meshycord.service >/dev/null 2>&1 || true
    systemctl disable meshycord.service >/dev/null 2>&1 || true
fi

exit 0
