#!/bin/sh
# Opens the ports wiser-dashboard needs, to this machine's local network only:
#   TCP 8080  the dashboard and Home Assistant
#   UDP 5540  Matter (Google Home, Apple Home, Alexa controlling the bridge)
#   UDP 5353  mDNS (those apps finding the bridge)
#
#   sudo ./open-firewall.sh                  find the local network(s) and open the ports
#   ./open-firewall.sh --dry-run             only show what it would do
#   sudo ./open-firewall.sh 10.0.0.0/24      use these networks instead of the detected ones
#
# Works with ufw, firewalld and plain nftables. Running it again changes nothing.
# Other ports: WISER_PORT=9090 WISER_MATTER_PORT=5541 sudo -E ./open-firewall.sh
set -eu

PORT=${WISER_PORT:-8080}
MATTER_PORT=${WISER_MATTER_PORT:-5540}
TAG=wiser-dashboard
# Matter also talks over IPv6; these are local-only address ranges.
V6_LOCAL="fe80::/10 fc00::/7"

dry=false
nets=""
for a in "$@"; do
	case "$a" in
	--dry-run | -n) dry=true ;;
	-h | --help)
		sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*/*) nets="$nets $a" ;;
	*)
		echo "unknown argument: $a (try --help)" >&2
		exit 2
		;;
	esac
done

run() {
	echo "  + $*"
	$dry || "$@"
}

# Local IPv4 networks: the kernel's routes for directly connected private
# networks, skipping container, VPN and virtual-machine interfaces.
private() {
	case "$1" in
	10.* | 192.168.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].*) return 0 ;;
	esac
	return 1
}
if [ -z "$nets" ]; then
	if ! command -v ip >/dev/null; then
		echo "Can't find the 'ip' command to detect the local network; pass it, e.g. $0 192.168.1.0/24" >&2
		exit 1
	fi
	for n in $(ip -4 route show proto kernel scope link |
		awk '$1 ~ /\// && $3 !~ /^(docker|br-|veth|virbr|lxc|cni|flannel|tailscale|zt|wg|tun|tap|lo)/ {print $1}' | sort -u); do
		if private "$n"; then nets="$nets $n"; fi
	done
fi
if [ -z "$nets" ]; then
	echo "No local private network found. Pass yours, e.g.: sudo $0 192.168.1.0/24" >&2
	exit 1
fi
echo "Local network:$nets (plus local IPv6)"

if ! $dry && [ "$(id -u)" != 0 ]; then
	echo "Run it with sudo (or add --dry-run to just see the commands)." >&2
	exit 1
fi

# ---------------------------------------------------------------- ufw
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
	echo "Firewall: ufw"
	for n in $nets; do
		run ufw allow from "$n" to any port "$PORT" proto tcp comment "$TAG"
		run ufw allow from "$n" to any port "$MATTER_PORT" proto udp comment "$TAG matter"
		run ufw allow from "$n" to any port 5353 proto udp comment "$TAG mdns"
	done
	for n in $V6_LOCAL; do
		run ufw allow from "$n" to any port "$MATTER_PORT" proto udp comment "$TAG matter"
		run ufw allow from "$n" to any port 5353 proto udp comment "$TAG mdns"
	done
	echo "Done."
	exit 0
fi

# ---------------------------------------------------------------- firewalld
if command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
	echo "Firewall: firewalld"
	if ! firewall-cmd --permanent --get-zones | tr ' ' '\n' | grep -qx "$TAG"; then
		run firewall-cmd --permanent --new-zone="$TAG"
	fi
	# firewalld won't take ports and services in one command.
	open_zone() {
		run firewall-cmd --permanent --zone="$1" --add-port="$PORT/tcp" --add-port="$MATTER_PORT/udp"
		run firewall-cmd --permanent --zone="$1" --add-service=mdns
	}
	for n in $nets $V6_LOCAL; do
		# Prints "no zone" and fails when the network isn't in one.
		zone=$(firewall-cmd --permanent --get-zone-of-source="$n" 2>/dev/null) || zone=""
		if [ -z "$zone" ]; then
			run firewall-cmd --permanent --zone="$TAG" --add-source="$n"
		elif [ "$zone" != "$TAG" ]; then
			# Already handled by another zone: open the ports there instead.
			echo "  ($n belongs to zone $zone; opening the ports there)"
			open_zone "$zone"
		fi
	done
	open_zone "$TAG"
	run firewall-cmd --reload
	echo "Done."
	exit 0
fi

# ---------------------------------------------------------------- nftables
if command -v nft >/dev/null && nft list chain inet filter input >/dev/null 2>&1; then
	echo "Firewall: nftables (inet filter input)"
	if nft list chain inet filter input | grep -q "\"$TAG\""; then
		echo "The rules are already there. Done."
		exit 0
	fi
	set4=$(echo $nets | sed 's/ /, /g')
	set6=$(echo $V6_LOCAL | sed 's/ /, /g')
	run nft insert rule inet filter input ip saddr "{ $set4 }" tcp dport "$PORT" accept comment "\"$TAG\""
	run nft insert rule inet filter input ip saddr "{ $set4 }" udp dport "{ 5353, $MATTER_PORT }" accept comment "\"$TAG\""
	run nft insert rule inet filter input ip6 saddr "{ $set6 }" udp dport "{ 5353, $MATTER_PORT }" accept comment "\"$TAG\""
	echo "Done. These nftables rules last until reboot: to keep them, save the ruleset"
	echo "(e.g. sudo sh -c 'nft list ruleset > /etc/nftables.conf') or add them to /etc/nftables.conf."
	exit 0
fi

echo "No active firewall found (ufw, firewalld or nftables): nothing to open."
