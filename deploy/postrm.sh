#!/bin/sh
# Runs after removal.
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

# /var/lib/meshycord is deliberately left in place. It holds the message
# history, the link table and the settings, and a package removal is not
# consent to delete those. To remove it:
#
#     sudo rm -rf /var/lib/meshycord
#
if [ -d /var/lib/meshycord ]; then
    echo "Settings and message history are kept in /var/lib/meshycord."
    echo "Remove it by hand if you want them gone."
fi

exit 0
