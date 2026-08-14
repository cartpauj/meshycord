#!/bin/sh
# Runs after install or upgrade, on both .deb and .rpm.
set -e

STATE_DIR=/var/lib/meshycord

# The database holds the Discord bot token and every room-server password.
# Lock the directory down before anything can be written into it.
mkdir -p "$STATE_DIR"
chmod 0700 "$STATE_DIR"
chown root:root "$STATE_DIR" 2>/dev/null || true
if [ -f "$STATE_DIR/db.sqlite" ]; then
    chmod 0600 "$STATE_DIR/db.sqlite" 2>/dev/null || true
fi

# Fresh install, or an upgrade?
#
# From the DATABASE, not from whether the service is running. That was the old
# test and it was wrong every time: prerm stops the service before this script
# runs, so an upgrade always looked like a first install and was greeted with
# "username: admin / password: admin" — which reads as though the password had
# just been reset.
#
# dpkg and rpm both pass an argument saying which case this is, but they say it
# differently, and this script is shared. The database is the same answer in
# both packagers and it is the thing the banner is actually about.
fresh=1
if [ -f "$STATE_DIR/db.sqlite" ]; then
    fresh=0
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true

    # Unconditional, both of them. prerm DISABLED the unit on its way past — it
    # cannot tell an upgrade from a removal either — so enabling here is what
    # keeps an upgraded install starting at boot.
    systemctl enable meshycord.service >/dev/null 2>&1 || true
    systemctl restart meshycord.service >/dev/null 2>&1 ||
        systemctl start meshycord.service >/dev/null 2>&1 || true

    # install.sh prints its own report, with the real address and port read from
    # the unit rather than a placeholder, so it sets this to avoid saying it all
    # twice. Anyone installing the package directly gets the banner.
    if [ "$fresh" -eq 1 ] && [ "${MESHYCORD_QUIET:-0}" != 1 ]; then
        cat <<'BANNER'

  MeshyCord is installed and running. Finish setup in the web console:

      http://<this-machine>:9150
      username: admin    password: admin    <- change this first

  It is the same password on every install and it is published in the README, so
  until you change it anyone who can reach this machine can read your message
  history and your Discord bot token. The console says so on every page.

  Help: https://github.com/cartpauj/meshycord

BANNER
    fi
fi

exit 0
