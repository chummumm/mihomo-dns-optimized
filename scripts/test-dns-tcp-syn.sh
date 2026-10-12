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
trace_pids=()
traces_printed=false

print_traces() {
  local trace_log
  [[ "$traces_printed" == false ]] || return 0
  traces_printed=true
  for trace_log in "$fixture_dir"/trace-*.log; do
    [[ -f "$trace_log" ]] || continue
    cat "$trace_log"
  done
}

cleanup() {
  local status=$? trace_pid
  trap - EXIT INT TERM
  for trace_pid in "${trace_pids[@]}"; do
    kill "$trace_pid" 2>/dev/null || true
    wait "$trace_pid" 2>/dev/null || true
  done
  print_traces
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
# Do not make a one-second probe depend on first-use ARP/NDP or IPv6 multicast
# setup in these freshly created namespaces. The fixture has exactly two peers.
client_mac=02:6d:69:68:6f:01
peer_mac=02:6d:69:68:6f:02
ip -n "$client_namespace" link set syn-client address "$client_mac"
ip -n "$peer_namespace" link set syn-peer address "$peer_mac"
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
ip -n "$client_namespace" neigh replace 192.0.2.2 lladdr "$peer_mac" nud permanent dev syn-client
ip -n "$peer_namespace" neigh replace 192.0.2.1 lladdr "$client_mac" nud permanent dev syn-peer
ip -n "$client_namespace" -6 neigh replace fd00:6d69:686f::2 lladdr "$peer_mac" nud permanent dev syn-client
ip -n "$peer_namespace" -6 neigh replace fd00:6d69:686f::1 lladdr "$client_mac" nud permanent dev syn-peer

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

# Keep a bounded trace on each side so a failure distinguishes a missing packet
# from a rejected TCP reply. Only fixture ports and IPv6 neighbor/MLD control
# messages are logged; TCP checksums are independently computed from wire bytes.
cat >"$fixture_dir/trace.py" <<'PY'
import pathlib
import socket
import struct
import sys
import time

interface, ready_path, stop_path = sys.argv[1:]
ready, stop = pathlib.Path(ready_path), pathlib.Path(stop_path)


def checksum(data):
    if len(data) % 2:
        data += b"\x00"
    total = sum(struct.unpack("!%dH" % (len(data) // 2), data))
    while total >> 16:
        total = (total & 0xffff) + (total >> 16)
    return (~total) & 0xffff


def describe(frame):
    if len(frame) < 14:
        return None
    ethernet_type = struct.unpack_from("!H", frame, 12)[0]
    packet = frame[14:]
    if ethernet_type == 0x0800 and len(packet) >= 20:
        header_length = (packet[0] & 15) * 4
        total_length = struct.unpack_from("!H", packet, 2)[0]
        if header_length < 20 or total_length > len(packet) or packet[9] != 6:
            return None
        source, destination = packet[12:16], packet[16:20]
        segment = packet[header_length:total_length]
        pseudo = source + destination + struct.pack("!BBH", 0, 6, len(segment))
        family = socket.AF_INET
    elif ethernet_type == 0x86dd and len(packet) >= 40:
        total_length = 40 + struct.unpack_from("!H", packet, 4)[0]
        if total_length > len(packet):
            return None
        source, destination = packet[8:24], packet[24:40]
        protocol, offset = packet[6], 40
        while protocol in (0, 43, 60) and offset + 2 <= total_length:
            protocol, extension_length = packet[offset], (packet[offset + 1] + 1) * 8
            offset += extension_length
        if offset >= total_length:
            return None
        if protocol == 58 and packet[offset] in (130, 131, 132, 135, 136, 143):
            return "IPv6 control type=%d %s -> %s" % (
                packet[offset], socket.inet_ntop(socket.AF_INET6, source),
                socket.inet_ntop(socket.AF_INET6, destination))
        if protocol != 6:
            return None
        segment = packet[offset:total_length]
        pseudo = source + destination + struct.pack("!I3xB", len(segment), 6)
        family = socket.AF_INET6
    else:
        return None
    if len(segment) < 20:
        return None
    source_port, destination_port, sequence, acknowledgment = struct.unpack_from("!HHII", segment)
    if source_port not in (16080, 16081) and destination_port not in (16080, 16081):
        return None
    flags = segment[13]
    names = [name for bit, name in ((1, "FIN"), (2, "SYN"), (4, "RST"),
             (8, "PSH"), (16, "ACK"), (32, "URG"), (64, "ECE"), (128, "CWR")) if flags & bit]
    result = checksum(pseudo + segment)
    return "IPv%d [%s]:%d -> [%s]:%d flags=%s seq=%08x ack=%08x bytes=%d checksum=%s" % (
        4 if family == socket.AF_INET else 6,
        socket.inet_ntop(family, source), source_port,
        socket.inet_ntop(family, destination), destination_port,
        "+".join(names), sequence, acknowledgment, len(segment),
        "valid" if result == 0 else "INVALID(%04x)" % result)


with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3)) as capture:
    capture.bind((interface, 0))
    capture.settimeout(0.05)
    ready.write_text("ready\n")
    deadline, count = time.monotonic() + 15, 0
    while True:
        if time.monotonic() >= deadline:
            raise RuntimeError("fixture packet trace exceeded its 15-second limit")
        try:
            frame, address = capture.recvfrom(65535)
        except socket.timeout:
            if stop.exists():
                break
            continue
        detail = describe(frame)
        if detail is None:
            continue
        direction = "OUT" if address[2] == socket.PACKET_OUTGOING else "IN"
        print("TCP SYN trace %.6f %s %s %s" % (time.monotonic(), interface, direction, detail), flush=True)
        count += 1
        if count >= 128:
            raise RuntimeError("fixture packet trace exceeded its 128-frame limit")
    print("TCP SYN trace %s: %d matching frames captured" % (interface, count), flush=True)
PY
for side in client peer; do
  namespace="mihomo-syn-$side-$fixture_id"
  ip netns exec "$namespace" python3 "$fixture_dir/trace.py" "syn-$side" \
    "$fixture_dir/trace-$side.ready" "$fixture_dir/stop" >"$fixture_dir/trace-$side.log" 2>&1 &
  trace_pids+=("$!")
done
for ((attempt = 0; attempt < 250; attempt++)); do
  [[ -f "$fixture_dir/trace-client.ready" && -f "$fixture_dir/trace-peer.ready" ]] && break
  for trace_pid in "${trace_pids[@]}"; do
    kill -0 "$trace_pid" 2>/dev/null || fail 'fixture packet trace failed before becoming ready'
  done
  sleep 0.02
done
[[ -f "$fixture_dir/trace-client.ready" && -f "$fixture_dir/trace-peer.ready" ]] ||
  fail 'fixture packet traces did not become ready within five seconds'

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

test_status=0
ip netns exec "$client_namespace" env \
  MIHOMO_TCP_SYN_INTEGRATION=1 \
  MIHOMO_TCP_SYN_TEST_INTERFACE=syn-client \
  MIHOMO_TCP_SYN_TEST_IPV4=192.0.2.2:16080 \
  MIHOMO_TCP_SYN_TEST_IPV4_CLOSED=192.0.2.2:16081 \
  MIHOMO_TCP_SYN_TEST_IPV6='[fd00:6d69:686f::2]:16080' \
  MIHOMO_TCP_SYN_TEST_IPV6_CLOSED='[fd00:6d69:686f::2]:16081' \
  "$test_binary" -test.v -test.timeout 1m \
  -test.run '^TestTCPSYN(KernelRepliesWithoutOpeningTCPConnection|PortReservationIsExclusiveAndReleased|RejectsCanceledAndWrongFamilyProbes)$' || test_status=$?

touch "$fixture_dir/stop"
for trace_pid in "${trace_pids[@]}"; do
  wait "$trace_pid" || test_status=1
done
trace_pids=()
print_traces
if ! wait "$peer_pid"; then
  peer_pid=
  cat "$fixture_dir/peer.log" >&2
  fail 'fixture peer detected an unexpected completed TCP connection'
fi
peer_pid=
[[ "$test_status" == 0 ]] || exit "$test_status"
printf 'TCP SYN integration: IPv4/IPv6 SYN-ACK and RST-ACK verified with full wire checksums.\n'
