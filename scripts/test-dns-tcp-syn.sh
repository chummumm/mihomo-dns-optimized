#!/usr/bin/env bash
# Run the precompiled DNS tests with real, fully checksummed IPv4/IPv6 packets.
# Linux loopback has fixed checksum offload on many kernels; a veth pair with
# TX offload disabled provides a wire-equivalent fixture without changing the
# host's interfaces, routes, firewall, or offload settings.
set -euo pipefail
export LC_ALL=C

fail() {
  printf 'TCP SYN integration: %s\n' "$*" >&2
  exit 1
}

[[ $# == 1 ]] || fail "usage: $0 <precompiled-dns-test-binary>"
[[ $EUID == 0 ]] || fail 'requires root with network namespace and raw socket capabilities'
for command in ip ethtool python3 sysctl realpath mktemp; do
  command -v "$command" >/dev/null || fail "required command is missing: $command"
done
test_binary=$(realpath -- "$1")
[[ -x "$test_binary" ]] || fail "test binary is not executable: $test_binary"

fixture_dir=$(mktemp -d "${TMPDIR:-/tmp}/mihomo-tcp-syn.XXXXXXXX")
fixture_id=${fixture_dir##*.}
client_namespace="mihomo-syn-client-$fixture_id"
peer_namespace="mihomo-syn-peer-$fixture_id"
client_created=false
peer_created=false
peer_pid=

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "$peer_pid" ]]; then
    kill "$peer_pid" 2>/dev/null || true
    wait "$peer_pid" 2>/dev/null || true
  fi
  if [[ "$peer_created" == true ]]; then
    ip netns delete "$peer_namespace" || status=1
  fi
  if [[ "$client_created" == true ]]; then
    ip netns delete "$client_namespace" || status=1
  fi
  rm -rf -- "$fixture_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ip netns add "$client_namespace" || fail 'cannot create isolated client namespace'
client_created=true
ip netns add "$peer_namespace" || fail 'cannot create isolated peer namespace'
peer_created=true
ip -n "$client_namespace" link add syn-client type veth peer name syn-peer netns "$peer_namespace"
for namespace in "$client_namespace" "$peer_namespace"; do
  ip netns exec "$namespace" sysctl -q -w net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0
  ip -n "$namespace" link set lo up
done
ip -n "$client_namespace" addr add 192.0.2.1/24 dev syn-client
ip -n "$peer_namespace" addr add 192.0.2.2/24 dev syn-peer
ip -n "$client_namespace" -6 addr add fd00:6d69:686f::1/64 dev syn-client nodad
ip -n "$peer_namespace" -6 addr add fd00:6d69:686f::2/64 dev syn-peer nodad
ip -n "$client_namespace" link set syn-client up
ip -n "$peer_namespace" link set syn-peer up

disable_tx_offload() {
  local namespace=$1 interface=$2 features
  ip netns exec "$namespace" ethtool -K "$interface" tx off ||
    fail "cannot disable TX checksum offload on fixture interface $interface"
  features=$(ip netns exec "$namespace" ethtool -k "$interface") ||
    fail "cannot read checksum offload state for $interface"
  if ! grep -Eq '^tx-checksumming: off([[:space:]]|$)' <<< "$features"; then
    printf '%s\n' "$features" >&2
    fail "TX checksum offload remains enabled on fixture interface $interface"
  fi
}
disable_tx_offload "$client_namespace" syn-client
disable_tx_offload "$peer_namespace" syn-peer

# The peer never accepts a completed handshake. Any accepted connection means
# tcp-syn accidentally used a connect probe; that makes this required gate fail.
ip netns exec "$peer_namespace" python3 - "$fixture_dir/ready" "$fixture_dir/stop" >"$fixture_dir/peer.log" 2>&1 <<'PY' &
import pathlib
import selectors
import socket
import sys

ready, stop = map(pathlib.Path, sys.argv[1:])
listeners = []
selector = selectors.DefaultSelector()
try:
    for family, address in ((socket.AF_INET, "192.0.2.2"), (socket.AF_INET6, "fd00:6d69:686f::2")):
        listener = socket.socket(family, socket.SOCK_STREAM)
        listeners.append(listener)
        if family == socket.AF_INET6:
            listener.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        listener.bind((address, 16080))
        listener.listen(8)
        listener.setblocking(False)
        selector.register(listener, selectors.EVENT_READ)
    ready.write_text("ready\n")
    while True:
        events = selector.select(timeout=0.02)
        for event, _ in events:
            connection, _ = event.fileobj.accept()
            connection.close()
            raise RuntimeError("tcp-syn completed a TCP handshake instead of sending a SYN probe")
        if stop.exists():
            break
finally:
    selector.close()
    for listener in listeners:
        listener.close()
PY
peer_pid=$!
for ((attempt = 0; attempt < 250; attempt++)); do
  [[ -f "$fixture_dir/ready" ]] && break
  if ! kill -0 "$peer_pid" 2>/dev/null; then
    cat "$fixture_dir/peer.log" >&2
    fail 'IPv4/IPv6 fixture peer failed before becoming ready'
  fi
  sleep 0.02
done
[[ -f "$fixture_dir/ready" ]] || fail 'IPv4/IPv6 fixture peer did not become ready within five seconds'

ip netns exec "$client_namespace" env \
  MIHOMO_TCP_SYN_INTEGRATION=1 \
  MIHOMO_TCP_SYN_TEST_INTERFACE=syn-client \
  MIHOMO_TCP_SYN_TEST_IPV4=192.0.2.2:16080 \
  MIHOMO_TCP_SYN_TEST_IPV4_CLOSED=192.0.2.2:16081 \
  MIHOMO_TCP_SYN_TEST_IPV6='[fd00:6d69:686f::2]:16080' \
  MIHOMO_TCP_SYN_TEST_IPV6_CLOSED='[fd00:6d69:686f::2]:16081' \
  "$test_binary" -test.v -test.timeout 1m \
  -test.run '^TestTCPSYN(KernelRepliesWithoutOpeningTCPConnection|PortReservationIsExclusiveAndReleased|RejectsCanceledAndWrongFamilyProbes)$'

touch "$fixture_dir/stop"
if ! wait "$peer_pid"; then
  peer_pid=
  cat "$fixture_dir/peer.log" >&2
  fail 'fixture peer detected an unexpected completed TCP connection'
fi
peer_pid=
printf 'TCP SYN integration: IPv4/IPv6 SYN-ACK and RST-ACK verified with full wire checksums.\n'
