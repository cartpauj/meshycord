#!/bin/sh
# MeshyCord installer and updater.
#
#   curl -fsSL https://raw.githubusercontent.com/cartpauj/meshycord/main/install.sh | sudo sh
#
# Picks the right package for this machine, checks it against the release's
# SHA256SUMS, and installs it. Running it again upgrades in place: the database
# in /var/lib/meshycord is package configuration as far as dpkg and rpm are
# concerned, so settings, links and history survive.
#
# Environment:
#   MESHYCORD_VERSION   tag to install, e.g. v0.0.2. Default: the latest release.
#   MESHYCORD_METHOD    force deb | rpm | tar instead of detecting.
#   MESHYCORD_DRY_RUN   1 to print what would happen and stop.
#
# POSIX sh on purpose. This is the first thing that runs on a fresh Pi, before
# anyone has installed anything, so it assumes only sh, uname, and one of
# curl/wget.

set -eu

REPO='cartpauj/meshycord'
API="https://api.github.com/repos/${REPO}"

VERSION="${MESHYCORD_VERSION:-}"
METHOD="${MESHYCORD_METHOD:-}"
DRY_RUN="${MESHYCORD_DRY_RUN:-0}"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- fetching ---------------------------------------------------------------

if command -v curl >/dev/null 2>&1; then
	fetch()    { curl -fsSL "$1"; }
	download() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch()    { wget -qO- "$1"; }
	download() { wget -q --tries=3 -O "$2" "$1"; }
else
	die 'neither curl nor wget is installed'
fi

# --- what are we running on -------------------------------------------------
#
# ARMv6 and ARMv7 both report as armhf to dpkg, so the two .debs differ only by
# filename. Getting this wrong is not subtle: an ARMv7 binary on a Pi Zero dies
# with an illegal instruction.

machine="$(uname -m)"
case "$machine" in
x86_64 | amd64)  deb='_amd64.deb'         ; rpm='.x86_64.rpm'  ; tar='_linux_amd64.tar.gz' ;;
aarch64 | arm64) deb='_arm64.deb'         ; rpm='.aarch64.rpm' ; tar='_linux_arm64.tar.gz' ;;
armv7l | armv7)  deb='_armhf.deb'         ; rpm='.armv7hl.rpm' ; tar='_linux_armv7.tar.gz' ;;
armv6l | armv6)  deb='_armhf-armv6.deb'   ; rpm=''             ; tar='_linux_armv6.tar.gz' ;;
i386 | i486 | i586 | i686)
                 deb='_i386.deb'          ; rpm='.i686.rpm'    ; tar='_linux_i386.tar.gz' ;;
*) die "unsupported architecture: $machine (builds exist for x86_64, aarch64, armv7l, armv6l, i686)" ;;
esac

# armv7l on a 64-bit kernel running a 32-bit userland is fine — the armhf build
# is what that wants. But aarch64 with a 32-bit userland would break, so check
# what dpkg itself believes when it is available.
if [ "$machine" = 'aarch64' ] && command -v dpkg >/dev/null 2>&1; then
	case "$(dpkg --print-architecture 2>/dev/null || true)" in
	armhf)
		warn 'note: 64-bit kernel with a 32-bit userland; using the armhf build'
		deb='_armhf.deb'; rpm='.armv7hl.rpm'; tar='_linux_armv7.tar.gz'
		;;
	esac
fi

if [ -z "$METHOD" ]; then
	if command -v dpkg >/dev/null 2>&1; then
		METHOD='deb'
	elif command -v rpm >/dev/null 2>&1; then
		METHOD='rpm'
	else
		METHOD='tar'
	fi
fi
if [ "$METHOD" = 'rpm' ] && [ -z "$rpm" ]; then
	warn 'note: there is no ARMv6 .rpm; falling back to the tarball'
	METHOD='tar'
fi

case "$METHOD" in
deb) want="$deb" ;;
rpm) want="$rpm" ;;
tar) want="$tar" ;;
*)   die "MESHYCORD_METHOD must be deb, rpm or tar (got '$METHOD')" ;;
esac

# --- which release ----------------------------------------------------------

if [ -n "$VERSION" ]; then
	release_url="${API}/releases/tags/${VERSION}"
else
	release_url="${API}/releases/latest"
fi
release_json="$(fetch "$release_url")" ||
	die "could not read the release list from GitHub (${release_url})"

# Assets are matched by suffix rather than by a filename built here, so a change
# in how packages are named cannot silently produce a 404.
asset_url="$(printf '%s' "$release_json" |
	tr ',' '\n' | grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*"' |
	sed 's/.*"\(https[^"]*\)"/\1/' | grep -- "${want}$" | head -n 1)" || true
sums_url="$(printf '%s' "$release_json" |
	tr ',' '\n' | grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*"' |
	sed 's/.*"\(https[^"]*\)"/\1/' | grep -- '/SHA256SUMS$' | head -n 1)" || true

tag="$(printf '%s' "$release_json" | tr ',' '\n' |
	grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' |
	sed 's/.*"\([^"]*\)"$/\1/' | head -n 1)"

[ -n "${asset_url:-}" ] ||
	die "release ${tag:-?} has no asset ending in ${want}"

asset="$(basename "$asset_url")"
say "MeshyCord ${tag}"
say "  machine : ${machine}"
say "  package : ${asset}  (${METHOD})"

if [ "$DRY_RUN" = '1' ]; then
	say '  dry run; nothing installed'
	exit 0
fi

# --- root -------------------------------------------------------------------

if [ "$(id -u)" -ne 0 ]; then
	# shellcheck disable=SC2016  # the backticks are prose, not a substitution
	die 'this needs root: pipe it into `sudo sh`, or run `sudo sh install.sh`'
fi

# --- download and verify ----------------------------------------------------

tmp="$(mktemp -d)"
# shellcheck disable=SC2064  # $tmp must expand now, not at trap time
trap "rm -rf '$tmp'" EXIT INT TERM

say "  fetching ${asset}"
download "$asset_url" "${tmp}/${asset}"

if [ -n "${sums_url:-}" ] && command -v sha256sum >/dev/null 2>&1; then
	download "$sums_url" "${tmp}/SHA256SUMS"
	# SHA256SUMS covers every asset in the release; check only ours, and insist
	# that a line for it actually exists. `sha256sum -c` on a filtered file that
	# came out empty exits non-zero, but with a confusing message, so be explicit.
	if ! grep -F " ${asset}" "${tmp}/SHA256SUMS" > "${tmp}/expected"; then
		die "SHA256SUMS has no entry for ${asset}"
	fi
	( cd "$tmp" && sha256sum -c expected >/dev/null ) ||
		die "checksum mismatch on ${asset} — download corrupted, or the release was altered"
	say '  checksum ok'
else
	warn '  warning: skipping checksum verification (no SHA256SUMS or no sha256sum)'
fi

# --- install ----------------------------------------------------------------

case "$METHOD" in
deb)
	say '  installing with dpkg'
	dpkg -i "${tmp}/${asset}" || {
		warn '  dpkg reported missing dependencies; asking apt to resolve them'
		apt-get -y -f install
	}
	;;
rpm)
	say '  installing with rpm'
	# -U upgrades or installs. --replacepkgs lets a reinstall of the same
	# version succeed, which is what re-running this script does.
	rpm -Uvh --replacepkgs "${tmp}/${asset}"
	;;
tar)
	say '  installing from the tarball'
	tar -xzf "${tmp}/${asset}" -C "$tmp" meshycord
	install -m 0755 -o root -g root "${tmp}/meshycord" /usr/bin/meshycord

	# The tarball is the binary alone, so the unit comes from the same tag —
	# never from main, which may describe a unit this binary does not match.
	if [ ! -f /etc/systemd/system/meshycord.service ] &&
		[ ! -f /usr/lib/systemd/system/meshycord.service ]; then
		unit="https://raw.githubusercontent.com/${REPO}/${tag}/deploy/meshycord.service"
		if download "$unit" "${tmp}/meshycord.service"; then
			install -d -m 0700 /var/lib/meshycord
			install -m 0644 "${tmp}/meshycord.service" /etc/systemd/system/meshycord.service
			command -v systemctl >/dev/null 2>&1 && {
				systemctl daemon-reload
				systemctl enable meshycord
			}
		else
			warn '  could not fetch the systemd unit; the binary is installed and can be run by hand'
		fi
	fi
	;;
esac

# --- report -----------------------------------------------------------------

if command -v systemctl >/dev/null 2>&1; then
	systemctl restart meshycord 2>/dev/null || systemctl start meshycord 2>/dev/null || true
fi

say ''
say "Installed: $(meshycord -version 2>/dev/null || echo "${tag}")"

if command -v systemctl >/dev/null 2>&1 &&
	[ "$(systemctl is-active meshycord 2>/dev/null || true)" = 'active' ]; then
	# The listen address is a setting, so read the port from the running unit
	# rather than asserting the default at someone whose port is not 9150.
	port="$(systemctl show -p ExecStart --value meshycord 2>/dev/null |
		sed -n 's/.*-listen[= ]*[^ ]*:\([0-9]\{1,5\}\).*/\1/p' | head -n 1)"
	say "Console  : http://$(hostname -I 2>/dev/null | awk '{print $1}'):${port:-9150}"
	say "Sign in  : admin / admin  <- change this first, it is the same on every install"
	say ''
	say 'Then set the Discord bot token and your server id on the Settings page.'
else
	say ''
	say 'The service is not running yet. Check: journalctl -u meshycord -n 30'
fi
