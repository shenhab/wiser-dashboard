#!/bin/sh
# Installs or upgrades Wiser Dashboard on Linux and macOS, from the latest release.
#
#   curl -fsSL https://raw.githubusercontent.com/shenhab/wiser-dashboard/main/install.sh | sh
#
# Options go after "sh -s --", e.g. ... | sh -s -- --version v1.3.1
#   --version vX.Y.Z   install this release instead of the latest
#   --hub IP           the hub's address (otherwise it's found automatically)
#   --port N           serve the dashboard on this port (default 8080)
#   --no-service       don't set it up to start automatically; just install and start it
#   --no-firewall      don't open the firewall (Linux)
#   --uninstall        remove it (settings and history are kept unless you add --purge)
# The hub secret can be given as WISER_SECRET=... (kept out of the command line);
# otherwise the dashboard asks for it in the browser.
#
# Linux with systemd: a locked-down system service (as root, via sudo if needed).
# macOS: a login item (LaunchAgent) for your user. Elsewhere: a per-user install.
# Running it again upgrades in place, keeping settings, history, pairings and tokens.
set -eu

REPO="shenhab/wiser-dashboard"
# The release signing keys (internal/releasekeys), as OpenSSL public keys.
RELEASE_KEYS="MCowBQYDK2VwAyEA9JzdnhnMFHPU2Buxd5LDtzbAPMRVTVW8ZQZ+tDDJgXw= MCowBQYDK2VwAyEAbs4Cabad/CD+lNOC7GWeFo9KbAcNOL1ZnnhMC8OW1FE="
VERSION="${WISER_VERSION:-}"
HUB=""
PORT=8080
SERVICE=1
FIREWALL=1
UNINSTALL=0
PURGE=0

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[31merror:\033[0m %s\n' "$*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) VERSION="${2:?--version needs a value}"; shift ;;
	--hub) HUB="${2:?--hub needs an address}"; shift ;;
	--port) PORT="${2:?--port needs a number}"; shift ;;
	--no-service) SERVICE=0 ;;
	--no-firewall) FIREWALL=0 ;;
	--uninstall) UNINSTALL=1 ;;
	--purge) PURGE=1 ;;
	-h | --help)
		cat <<'EOF2'
Installs or upgrades Wiser Dashboard from the latest release.
  curl -fsSL https://raw.githubusercontent.com/shenhab/wiser-dashboard/main/install.sh | sh -s -- [options]
  --version vX.Y.Z   install this release instead of the latest
  --hub IP           the hub's address (otherwise it's found automatically)
  --port N           serve the dashboard on this port (default 8080)
  --no-service       don't start it automatically at boot/login
  --no-firewall      don't open the firewall (Linux)
  --uninstall        remove it (add --purge to also delete settings and history)
Give the hub secret as WISER_SECRET=... if you want; otherwise the dashboard asks for it.
EOF2
		exit 0 ;;
	*) die "unknown option: $1 (see the top of install.sh)" ;;
	esac
	shift
done
case "$PORT" in '' | *[!0-9]*) die "--port must be a number" ;; esac

have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- where it goes

OS=$(uname -s)
SYSTEMD=0
if [ "$OS" = Linux ] && [ "$SERVICE" = 1 ] && have systemctl && [ -d /run/systemd/system ]; then
	SYSTEMD=1
fi

SUDO=""
if [ "$SYSTEMD" = 1 ] && [ "$(id -u)" != 0 ]; then
	have sudo || die "installing the service needs root: run as root, or use --no-service for a per-user install"
	SUDO="sudo"
fi

if [ "$SYSTEMD" = 1 ]; then
	BIN=/usr/local/bin/wiser-dashboard
	STATE=/var/lib/wiser-dashboard
	MODE="system service"
elif [ "$OS" = Darwin ]; then
	BIN="$HOME/.local/bin/wiser-dashboard"
	STATE="$HOME/Library/Application Support/wiser-dashboard"
	PLIST="$HOME/Library/LaunchAgents/io.github.shenhab.wiser-dashboard.plist"
	LOG="$HOME/Library/Logs/wiser-dashboard.log"
	MODE="login item"
	[ "$SERVICE" = 1 ] || MODE="per-user install"
else
	BIN="$HOME/.local/bin/wiser-dashboard"
	STATE="${XDG_CONFIG_HOME:-$HOME/.config}/wiser-dashboard"
	LOG="${XDG_STATE_HOME:-$HOME/.local/state}/wiser-dashboard.log"
	MODE="per-user install"
fi
UNIT=/etc/systemd/system/wiser-dashboard.service
DROPIN=/etc/systemd/system/wiser-dashboard.service.d/10-install.conf
UPDATE_UNITS="wiser-dashboard-update.path wiser-dashboard-update.service"

# stop_running stops whatever this installer started before (service, login
# item or background copy), so the binary can be replaced.
stop_running() {
	if [ "$SYSTEMD" = 1 ]; then
		$SUDO systemctl stop wiser-dashboard 2>/dev/null || true
	elif [ "$OS" = Darwin ] && [ -f "${PLIST:-/nonexistent}" ]; then
		launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
	fi
	if [ -f "$STATE/wiser-dashboard.pid" ] && [ "$SYSTEMD" = 0 ]; then
		kill "$(cat "$STATE/wiser-dashboard.pid")" 2>/dev/null || true
		rm -f "$STATE/wiser-dashboard.pid"
	fi
	if have pkill; then
		pkill -x wiser-dashboard -U "$(id -u)" 2>/dev/null || true
	fi
}

# ---------------------------------------------------------------- uninstall

if [ "$UNINSTALL" = 1 ]; then
	say "Removing Wiser Dashboard ($MODE)"
	stop_running
	if [ "$SYSTEMD" = 1 ]; then
		$SUDO systemctl disable wiser-dashboard wiser-dashboard-update.path 2>/dev/null || true
		$SUDO rm -f "$UNIT" "$DROPIN"
		for u in $UPDATE_UNITS; do $SUDO rm -f "/etc/systemd/system/$u"; done
		$SUDO rmdir /etc/systemd/system/wiser-dashboard.service.d 2>/dev/null || true
		$SUDO systemctl daemon-reload
		$SUDO rm -f "$BIN"
		if [ "$PURGE" = 1 ]; then
			$SUDO rm -rf "$STATE" /var/lib/private/wiser-dashboard /etc/wiser-dashboard.secret /etc/wiser-dashboard.env
			$SUDO userdel wiser-dashboard 2>/dev/null || true
		fi
	else
		[ "$OS" = Darwin ] && rm -f "$PLIST"
		rm -f "$BIN"
		[ "$PURGE" = 1 ] && rm -rf "$STATE"
	fi
	if [ "$PURGE" = 1 ]; then say "Removed, including settings and history."; else say "Removed. Settings and history are kept in $STATE (add --purge to delete them)."; fi
	exit 0
fi

# ---------------------------------------------------------------- which build

ARCH=$(uname -m)
case "$OS/$ARCH" in
Linux/x86_64 | Linux/amd64) PLATFORM=linux-amd64 ;;
Linux/aarch64 | Linux/arm64)
	# 32-bit Raspberry Pi OS can run a 64-bit kernel; its programs are still 32-bit.
	if [ "$(getconf LONG_BIT 2>/dev/null || echo 64)" = 32 ]; then PLATFORM=linux-armv7; else PLATFORM=linux-arm64; fi ;;
Linux/armv7* | Linux/armv8l) PLATFORM=linux-armv7 ;;
Linux/armv6*) die "ARMv6 (Raspberry Pi 1 and Zero) isn't supported; it needs an ARMv7 or 64-bit board" ;;
Darwin/arm64) PLATFORM=darwin-arm64 ;;
Darwin/x86_64)
	# A shell running under Rosetta on Apple silicon reports x86_64.
	if [ "$(sysctl -in hw.optional.arm64 2>/dev/null)" = 1 ]; then PLATFORM=darwin-arm64; else PLATFORM=darwin-amd64; fi ;;
*) die "no build for $OS on $ARCH; see https://github.com/$REPO#build-and-release to build it yourself" ;;
esac

if have curl; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
	fetch_text() { curl -fsSL --retry 3 "$1"; }
elif have wget; then
	fetch() { wget -q -O "$2" "$1"; }
	fetch_text() { wget -q -O - "$1"; }
else
	die "needs curl or wget"
fi
have tar || die "needs tar"
if have sha256sum; then sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif have shasum; then sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else die "needs sha256sum or shasum to check the download"; fi

if [ -z "$VERSION" ]; then
	# The releases/latest page redirects to the newest tag; unlike GitHub's API
	# it isn't limited to 60 requests an hour per address.
	if have curl; then
		VERSION=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null | sed -n 's|.*/tag/||p')
	fi
	[ -n "$VERSION" ] ||
		VERSION=$(fetch_text "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
	[ -n "$VERSION" ] || die "couldn't find the latest release (GitHub may be rate-limiting; try again, or pass --version)"
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

NAME="wiser-dashboard-$VERSION-$PLATFORM"
URL="https://github.com/$REPO/releases/download/$VERSION"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

say "Downloading Wiser Dashboard $VERSION for $PLATFORM"
fetch "$URL/$NAME.tar.gz" "$TMP/$NAME.tar.gz" || die "download failed: $URL/$NAME.tar.gz"
fetch "$URL/SHA256SUMS.txt" "$TMP/SHA256SUMS.txt" || die "couldn't download the checksums for $VERSION"
# The checksums are signed with the project's release key. Verified with
# OpenSSL 3 where it's available (most Linux); the dashboard itself checks the
# signature of every later update on every system.
if fetch "$URL/SHA256SUMS.txt.sig" "$TMP/SHA256SUMS.txt.sig" 2>/dev/null; then
	if have openssl && openssl version 2>/dev/null | grep -q '^OpenSSL [3-9]'; then
		openssl base64 -d -A -in "$TMP/SHA256SUMS.txt.sig" -out "$TMP/sig.bin" 2>/dev/null || die "the release signature is malformed"
		SIGNED=0
		for k in $RELEASE_KEYS; do
			printf -- '-----BEGIN PUBLIC KEY-----\n%s\n-----END PUBLIC KEY-----\n' "$k" >"$TMP/key.pem"
			if openssl pkeyutl -verify -pubin -inkey "$TMP/key.pem" -rawin -in "$TMP/SHA256SUMS.txt" -sigfile "$TMP/sig.bin" >/dev/null 2>&1; then
				SIGNED=1
				break
			fi
		done
		[ "$SIGNED" = 1 ] || die "the release signature doesn't match this project's signing keys: not installing it"
		say "Signature verified"
	else
		warn "can't check the release signature here (needs OpenSSL 3); relying on the checksum. Updates are signature-checked by the dashboard."
	fi
else
	warn "release $VERSION isn't signed (releases before v1.5.0 aren't); relying on the checksum"
fi
WANT=$(grep " $NAME.tar.gz\$" "$TMP/SHA256SUMS.txt" | cut -d' ' -f1)
[ -n "$WANT" ] || die "$NAME.tar.gz isn't listed in the release's checksums"
[ "$(sha256 "$TMP/$NAME.tar.gz")" = "$WANT" ] || die "checksum mismatch: the download is damaged or was tampered with"
say "Checksum verified"
tar -xzf "$TMP/$NAME.tar.gz" -C "$TMP"
PKG="$TMP/$NAME"
"$PKG/wiser-dashboard" -version >/dev/null 2>&1 || die "the downloaded program doesn't run on this system"

# ---------------------------------------------------------------- port check

dashboard_up() {
	if have curl; then curl -fsS -m 3 -o /dev/null "http://127.0.0.1:$PORT/api/state" 2>/dev/null
	else wget -q -T 3 -O /dev/null "http://127.0.0.1:$PORT/api/state" 2>/dev/null; fi
}

stop_running
sleep 1
if dashboard_up; then
	die "something is still answering on port $PORT, probably a copy started by hand (or as another user): stop it (Ctrl+C in its terminal, or 'sudo pkill -x wiser-dashboard'), then run this again"
fi

FLAGS="-listen :$PORT"
[ -n "$HUB" ] && FLAGS="$FLAGS -hub $HUB"

# ---------------------------------------------------------------- install: Linux service

if [ "$SYSTEMD" = 1 ]; then
	say "Installing the system service (you may be asked for your password)"
	if ! id wiser-dashboard >/dev/null 2>&1; then
		NOLOGIN=$(command -v nologin || echo /usr/sbin/nologin)
		$SUDO useradd --system --no-create-home --shell "$NOLOGIN" wiser-dashboard
	fi
	$SUDO install -m 755 "$PKG/wiser-dashboard" "$BIN"
	$SUDO install -m 644 "$PKG/wiser-dashboard.service" "$UNIT"
	# The update helper (in releases from v1.4.0): lets the dashboard update
	# itself without the service being allowed to change its own program.
	HELPER=0
	if [ -f "$PKG/wiser-dashboard-update.path" ]; then
		for u in $UPDATE_UNITS; do $SUDO install -m 644 "$PKG/$u" "/etc/systemd/system/$u"; done
		HELPER=1
	fi
	if [ -n "${WISER_SECRET:-}" ]; then
		printf '%s\n' "$WISER_SECRET" | $SUDO sh -c 'umask 077; cat > /etc/wiser-dashboard.secret'
	fi
	SECRET_FLAG=""
	if $SUDO test -s /etc/wiser-dashboard.secret; then
		$SUDO chgrp wiser-dashboard /etc/wiser-dashboard.secret
		$SUDO chmod 640 /etc/wiser-dashboard.secret
		SECRET_FLAG=" -secret-file /etc/wiser-dashboard.secret"
	fi
	# The installer's choices live in a drop-in, so later unit updates keep them.
	$SUDO mkdir -p "$(dirname "$DROPIN")"
	# shellcheck disable=SC2016 # ${STATE_DIRECTORY} is for systemd to expand
	printf '[Service]\nExecStart=\nExecStart=%s %s -config ${STATE_DIRECTORY}/config.json%s\n' "$BIN" "$FLAGS" "$SECRET_FLAG" |
		$SUDO tee "$DROPIN" >/dev/null
	# Pick up a hand-run copy's settings, history and pairing the first time.
	if ! $SUDO test -e "$STATE/config.json"; then
		for old in /root/.config/wiser-dashboard "$HOME/.config/wiser-dashboard"; do
			if $SUDO test -f "$old/config.json"; then
				say "Carrying over settings, history and pairing from $old"
				$SUDO mkdir -p "$STATE"
				$SUDO cp -a "$old/." "$STATE/"
				$SUDO chown -R wiser-dashboard:wiser-dashboard "$STATE"
				break
			fi
		done
	fi
	$SUDO systemctl daemon-reload
	$SUDO systemctl enable --quiet wiser-dashboard
	$SUDO systemctl restart wiser-dashboard
	[ "$HELPER" = 1 ] && $SUDO systemctl enable --quiet --now wiser-dashboard-update.path
	if [ "$FIREWALL" = 1 ] && [ -f "$PKG/open-firewall.sh" ]; then
		if $SUDO env WISER_PORT="$PORT" sh "$PKG/open-firewall.sh" >/dev/null 2>&1; then
			say "Firewall: opened the ports to your local network (if a firewall is active)"
		else
			warn "couldn't open the firewall automatically; see https://github.com/$REPO#firewall"
		fi
	fi
	show_log() { $SUDO journalctl -u wiser-dashboard -n 30 --no-pager -o cat >&2 || true; }
	HOW="It starts at boot. Log: journalctl -u wiser-dashboard -f"

# ---------------------------------------------------------------- install: macOS / per-user

else
	mkdir -p "$(dirname "$BIN")" "$STATE" "$(dirname "$LOG")"
	install -m 755 "$PKG/wiser-dashboard" "$BIN"
	# A secret given here goes into the dashboard's own config (only you can read
	# it), never onto a command line other users could see.
	if [ -n "${WISER_SECRET:-}" ]; then
		if [ -f "$STATE/config.json" ]; then
			warn "already set up: WISER_SECRET ignored; change the secret on the dashboard's setup page"
		else
			case "$WISER_SECRET" in *[!A-Za-z0-9+/=_.-]*) die "WISER_SECRET has unexpected characters" ;; esac
			(umask 077; printf '{\n  "secret": "%s"\n}\n' "$WISER_SECRET" >"$STATE/config.json")
		fi
	fi
	# Musl systems (Alpine) can't run the Matter bridge's runtime.
	if [ "$OS" = Linux ] && have ldd && ldd --version 2>&1 | grep -qi musl; then FLAGS="$FLAGS -matter=false"; fi
	STARTED=0
	if [ "$OS" = Darwin ] && [ "$SERVICE" = 1 ]; then
		mkdir -p "$(dirname "$PLIST")"
		{
			echo '<?xml version="1.0" encoding="UTF-8"?>'
			echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
			echo '<plist version="1.0"><dict>'
			echo '<key>Label</key><string>io.github.shenhab.wiser-dashboard</string>'
			echo '<key>ProgramArguments</key><array>'
			echo "<string>$BIN</string>"
			for f in $FLAGS; do echo "<string>$f</string>"; done
			echo '</array>'
			echo '<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>'
			echo "<key>StandardOutPath</key><string>$LOG</string><key>StandardErrorPath</key><string>$LOG</string>"
			echo '</dict></plist>'
		} >"$PLIST"
		chmod 600 "$PLIST"
		if launchctl bootstrap "gui/$(id -u)" "$PLIST" 2>/dev/null; then
			STARTED=1
			HOW="It starts when you log in. Log: $LOG"
		else
			warn "couldn't register the login item (no desktop session?); starting it in the background instead"
			MODE="per-user install"
		fi
	fi
	if [ "$STARTED" = 0 ]; then
		# shellcheck disable=SC2086
		nohup "$BIN" $FLAGS >>"$LOG" 2>&1 </dev/null &
		echo $! >"$STATE/wiser-dashboard.pid"
		HOW="It's running in the background until you log out or reboot; run $BIN to start it again. Log: $LOG"
	fi
	show_log() { tail -n 30 "$LOG" >&2 2>/dev/null || true; }
	case ":$PATH:" in *":$(dirname "$BIN"):"*) ;; *) warn "$(dirname "$BIN") isn't on your PATH; run it as $BIN" ;; esac
fi

# ---------------------------------------------------------------- check it really runs

say "Waiting for the dashboard to start"
i=0
until dashboard_up; do
	i=$((i + 1))
	if [ "$i" -ge 40 ]; then
		show_log
		die "the dashboard didn't start within 40 seconds; its log is above"
	fi
	sleep 1
done

IP=""
if have ip; then IP=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
elif [ "$OS" = Darwin ]; then IP=$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true); fi

echo
say "Wiser Dashboard $VERSION is installed and running ($MODE)."
echo "    Open:   http://localhost:$PORT${IP:+   or from other devices: http://$IP:$PORT}"
echo "    $HOW"
echo "    Upgrade: run the same command again. Remove: add '-s -- --uninstall' to it."
