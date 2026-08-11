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

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true

    if systemctl is-active --quiet meshycord.service 2>/dev/null; then
        # An upgrade. Restart so the new binary takes over; settings live in
        # the database, so nothing is lost.
        echo "Restarting meshycord..."
        systemctl restart meshycord.service >/dev/null 2>&1 || true
    else
        systemctl enable meshycord.service >/dev/null 2>&1 || true
        systemctl start meshycord.service >/dev/null 2>&1 || true

        cat <<'BANNER'

  MeshyCord is installed and running.

  Finish setup in the web console:

      http://<this-machine>:9150

      username: admin
      password: admin

  CHANGE THAT PASSWORD. It is the same on every install and it is printed in
  the README, so until you change it anyone who can reach this machine can
  read your message history and your Discord bot token. The console says so
  on every page until you do. Settings page, or from the shell:

      sudo meshycord -db /var/lib/meshycord/db.sqlite -set-password 'something long'

  You will also need a Discord bot token and your server (guild) ID.

  Plug the MeshCore node in over USB and it is found automatically. To see
  which serial devices are visible:

      meshycord -list-ports

  Logs:    journalctl -u meshycord -f
  Status:  systemctl status meshycord

BANNER
    fi
fi

exit 0
