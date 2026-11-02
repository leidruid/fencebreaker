#!/bin/sh
# End-to-end smoke of the fbtenant CoreDNS plugin: builds nothing itself, it
# needs a coredns binary compiled WITH the fbtenant plugin. Runs it against a
# real nftables table, sends queries with dig and verifies:
#   - learned IPs land in the tenant's inc_dns timeout set;
#   - excluded-domain answers land in exc_dns;
#   - repeated queries do NOT re-add live elements (mirror duplicates);
#   - select-strict: domains outside include/exclude land in exc_dns;
#   - plugin_status.json carries the counters.
#
# The upstream is a second instance of the same binary serving a fixed hosts
# zone on loopback: no live internet, no shared/short-TTL real records, the
# answers are identical on every run.
#
# Usage: tests/e2e/dns_smoke.sh [COREDNS_BIN] [WORKDIR]
set -eu

COREDNS="${1:-/tmp/fencebreaker-dns-smoke/coredns-fb}"
WORK="${2:-/tmp/fencebreaker-dns-smoke}"
PORT=10530
UPSTREAM_PORT=10531

[ -x "$COREDNS" ] || { echo "coredns binary not found: $COREDNS (build one with the fbtenant plugin first)"; exit 1; }
command -v dig >/dev/null || { echo "dig is required"; exit 1; }

if [ "$(id -u)" != 0 ]; then
    echo "re-running as root..."
    exec sudo "$0" "$COREDNS" "$WORK"
fi

mkdir -p "$WORK/run"

cat > "$WORK/run/dns_state.json" <<EOF
{
  "version": 1,
  "generation": 1,
  "listen_port": $PORT,
  "resync_interval_sec": 2,
  "ttl_min_sec": 5,
  "ttl_max_sec": 60,
  "refresh_on_remain_pct": 0,
  "max_set_size": 100,
  "default_upstreams": ["127.0.0.1:$UPSTREAM_PORT"],
  "nft_table": "fencebreaker",
  "tenants": [
    {
      "id": "smoke",
      "mark": 4096,
      "src_nets": ["127.0.0.0/8"],
      "hosts_full": [],
      "sets": {"inc_dns": "t1_inc_dns", "exc_dns": "t1_exc_dns"},
      "learning_mode": "select-strict",
      "include_domains": ["example.com"],
      "exclude_domains": ["wikipedia.org"],
      "upstreams": ["127.0.0.1:$UPSTREAM_PORT"],
      "dns_via_tunnel": false,
      "records": [],
      "forwards": []
    }
  ]
}
EOF

cat > "$WORK/Corefile" <<EOF
.:$PORT {
    fbtenant {
        state $WORK/run/dns_state.json
    }
    errors
}
EOF

# Deterministic upstream: fixed answers, stable TTL (hosts plugin: 3600s).
cat > "$WORK/upstream-Corefile" <<EOF
.:$UPSTREAM_PORT {
    hosts {
        93.184.216.34 example.com
        93.184.216.35 www.example.com
        185.15.59.224 www.wikipedia.org
        8.6.112.0 example.net
        fallthrough
    }
    errors
}
EOF

cleanup() {
    kill "$PID" "$UPID" 2>/dev/null || true
    wait "$PID" "$UPID" 2>/dev/null || true
    nft delete table inet fencebreaker 2>/dev/null || true
}
trap cleanup EXIT INT TERM

nft add table inet fencebreaker
nft add set inet fencebreaker t1_inc_dns '{ type ipv4_addr; flags timeout; timeout 60s; }'
nft add set inet fencebreaker t1_exc_dns '{ type ipv4_addr; flags timeout; timeout 60s; }'

"$COREDNS" -conf "$WORK/upstream-Corefile" >"$WORK/upstream.log" 2>&1 &
UPID=$!
"$COREDNS" -conf "$WORK/Corefile" >/tmp/dnssmoke-coredns.log 2>&1 &
PID=$!

for i in $(seq 1 50); do
    if dig +time=1 +tries=1 -p "$PORT" @127.0.0.1 example.com >/dev/null 2>&1; then break; fi
    sleep 0.2
done

fail=0
check() {
    if [ "$2" = "0" ]; then
        echo "PASS: $1"
    else
        echo "FAIL: $1"
        fail=1
    fi
}

# nft -j prints the whole document on ONE line, so element counting must
# count occurrences (grep -o), not matching lines (grep -c).
setelems() {
    nft -j list set inet fencebreaker "$1" | grep -o '"elem"' | wc -l
}

# 1. include-domain resolves and is learned into inc_dns
dig +time=3 +tries=1 -p "$PORT" @127.0.0.1 example.com A >/dev/null
sleep 1
n=$(setelems t1_inc_dns || true)
check "example.com answer learned into inc_dns" "$([ "${n:-0}" -ge 1 ] && echo 0 || echo 1)"

# 2. repeated queries skip duplicates (no re-add of live elements)
inc_before=$(setelems t1_inc_dns)
dig +time=3 +tries=1 -p "$PORT" @127.0.0.1 example.com A >/dev/null
dig +time=3 +tries=1 -p "$PORT" @127.0.0.1 www.example.com A >/dev/null
sleep 1
inc_after=$(setelems t1_inc_dns)
check "duplicate answers did not double elements ($inc_before->$inc_after)" "$([ "${inc_after:-0}" -le "$((inc_before + 2))" ] && echo 0 || echo 1)"

# 3. excluded domain is learned into exc_dns
dig +time=3 +tries=1 -p "$PORT" @127.0.0.1 www.wikipedia.org A >/dev/null
sleep 1
n=$(setelems t1_exc_dns)
check "wikipedia.org learned into exc_dns" "$([ "${n:-0}" -ge 1 ] && echo 0 || echo 1)"

# 4. non-include domain in select-strict mode is NOT learned into inc_dns
inc_wiki=$(nft -j list set inet fencebreaker t1_inc_dns | grep -o wikipedia | wc -l)
check "wikipedia not in inc_dns" "$([ "${inc_wiki:-0}" = "0" ] && echo 0 || echo 1)"

# 4b. select-strict: a domain outside include/exclude is classified into
# exc_dns (unclassified destinations would be dropped by the ruleset).
# Poll for a few seconds: the answer itself may need retries on lossy loopback.
exc_before=$(setelems t1_exc_dns)
learned=1
for i in 1 2 3 4 5; do
    dig +time=3 +tries=1 -p "$PORT" @127.0.0.1 example.net A >/dev/null || true
    sleep 1
    if [ "$(setelems t1_exc_dns)" -gt "${exc_before:-0}" ]; then
        learned=0
        break
    fi
done
exc_after=$(setelems t1_exc_dns)
check "select-strict: non-include domain lands in exc_dns ($exc_before->$exc_after)" "$learned"

# 5. plugin_status.json counters
sleep 6
grep -q '"duplicates_skipped"' "$WORK/run/plugin_status.json"
check "plugin_status.json has counters" "$?"
dup=$(grep -o '"duplicates_skipped": [0-9]*' "$WORK/run/plugin_status.json" | head -1 | grep -o '[0-9]*$')
check "duplicates_skipped > 0 (got $dup)" "$([ "${dup:-0}" -ge 1 ] && echo 0 || echo 1)"

exit $fail
