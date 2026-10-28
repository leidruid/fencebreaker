#!/bin/sh
# Local end-to-end smoke of fencebreakerd against the real nftables/netlink
# stack. Requires root and the nft/ip binaries. Creates and removes table
# inet fencebreaker and ip rules in the priority range 11000-12000.
#
# Usage: tests/e2e/local_smoke.sh [WORKDIR]
set -eu

WORK="${1:-/tmp/fencebreaker-smoke}"
BIN="$(cd "$(dirname "$0")/../.." && pwd)/daemon/fencebreakerd"
[ -x "$BIN" ] || { echo "build daemon first: (cd daemon && go build -o fencebreakerd ./cmd/fencebreakerd)"; exit 1; }

if [ "$(id -u)" != 0 ]; then
    echo "re-running as root..."
    exec sudo "$0" "$WORK"
fi

mkdir -p "$WORK/run" "$WORK/lists"

# tiny valid geoip.dat with RU = 45.135.240.0/22
printf '\x0a\x0e\x0a\x02RU\x12\x08\x0a\x04\x2d\x87\xf0\x00\x10\x16' > "$WORK/geoip.dat"

cat > "$WORK/network" <<'EOF'
config interface 'lan'
	option device 'eth0'
	option proto 'dhcp'
EOF

cat > "$WORK/fencebreaker.conf" <<'EOF'
config fencebreaker 'global'
	option enabled '1'
	option coredns_port '10530'

config tenant
	option id 'smoke'
	option name 'Smoke'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'eth0'
	option routing_mode 'select'
	list include_subnets '1.2.3.0/24'
	list include_geoip 'ru'
	list networks '192.168.77.0/24'
	list full_hosts '192.168.77.5'
	list excluded_hosts '192.168.77.6'
	list dns_upstream '1.1.1.1'

config list
	option name 'bl'
	option url 'http://127.0.0.1:1/none.txt'
	option refresh '0'
EOF

cleanup() {
    "$BIN" -config "$WORK/fencebreaker.conf" -network "$WORK/network" -rundir "$WORK/run" -listsdir "$WORK/lists" -geodata "$WORK/geoip.dat" -version >/dev/null 2>&1 || true
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
    nft delete table inet fencebreaker 2>/dev/null || true
    ip rule del pref 11010 2>/dev/null || true
    ip route flush table 101 2>/dev/null || true
}
trap cleanup EXIT INT TERM

"$BIN" \
    -config "$WORK/fencebreaker.conf" \
    -network "$WORK/network" \
    -rundir "$WORK/run" \
    -listsdir "$WORK/lists" \
    -geodata "$WORK/geoip.dat" &
PID=$!

# wait for the first apply
for i in $(seq 1 50); do
    [ -s "$WORK/run/dns_state.json" ] && break
    sleep 0.2
done

fail=0
check() {
    if eval "$2"; then
        echo "PASS: $1"
    else
        echo "FAIL: $1"
        fail=1
    fi
}

NFT=$(nft -list table inet fencebreaker 2>/dev/null || nft list table inet fencebreaker)
check "nft table exists"            "[ -n \"\$NFT\" ]"
check "tenant src set populated"    "echo \"\$NFT\" | grep -q '192.168.77.0/24'"
check "geoip zone loaded"           "echo \"\$NFT\" | grep -q '45.135.240.0/22'"
check "static include present"      "echo \"\$NFT\" | grep -q '1.2.3.0/24'"
check "dns hijack redirect rule"    "echo \"\$NFT\" | grep -q 'redirect to :10530'"
check "hosts full mark rule"        "echo \"\$NFT\" | grep -qE 'ip saddr @t1_hosts_full meta mark set 0x0*1000'"
check "dns set has timeout flags"   "echo \"\$NFT\" | grep -q 't1_inc_dns'"
check "ip rule installed"           "ip rule show | grep -q '11010'"
check "dns_state.json written"      "grep -q '\"generation\"' \"$WORK/run/dns_state.json\""
check "dns_state tenant mark"       "grep -q '\"mark\": 4096' \"$WORK/run/dns_state.json\""
check "status.json written"         "grep -q '\"tenants\"' \"$WORK/run/status.json\""
check "Corefile generated"          "grep -q 'fbtenant' \"$WORK/run/Corefile\""

ip route show table 101 | sed 's/^/  table101: /'

exit $fail
