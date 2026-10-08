#!/usr/bin/env python3
"""Exercise the actual binary against two local SOCKS5 DNS outbounds.

No external DNS, network service, Python dependency, or privileged port is used.
The reserved resolver addresses must arrive unchanged at the selected outbound.
"""

import argparse
import contextlib
import ipaddress
import json
import pathlib
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
import time
import urllib.request


def read_exact(conn, size):
    data = bytearray()
    while len(data) < size:
        part = conn.recv(size - len(data))
        if not part:
            raise EOFError("connection closed")
        data.extend(part)
    return bytes(data)


def encode_address(host, port):
    addr = ipaddress.ip_address(host)
    return bytes([1 if addr.version == 4 else 4]) + addr.packed + struct.pack("!H", port)


def read_address(conn):
    kind = read_exact(conn, 1)[0]
    if kind not in (1, 4):
        raise AssertionError("expected a literal resolver IP")
    host = str(ipaddress.ip_address(read_exact(conn, 4 if kind == 1 else 16)))
    return host, struct.unpack("!H", read_exact(conn, 2))[0]


def decode_packet(packet):
    assert packet[:3] == b"\x00\x00\x00", "invalid SOCKS5 UDP envelope"
    kind = packet[3]
    assert kind in (1, 4)
    size = 4 if kind == 1 else 16
    host = str(ipaddress.ip_address(packet[4 : 4 + size]))
    port = struct.unpack("!H", packet[4 + size : 6 + size])[0]
    return (host, port), packet[6 + size :]


def question(name, transaction=1234):
    wire_name = b"".join(bytes([len(label)]) + label.encode("ascii") for label in name.split(".")) + b"\x00"
    return struct.pack("!6H", transaction, 0x0100, 1, 0, 0, 0) + wire_name + struct.pack("!2H", 1, 1)


def question_name(wire):
    labels, offset = [], 12
    while wire[offset]:
        size = wire[offset]
        labels.append(wire[offset + 1 : offset + size + 1].decode("ascii"))
        offset += size + 1
    return ".".join(labels)


def make_answer(wire, answer):
    header = struct.pack("!6H", struct.unpack("!H", wire[:2])[0], 0x8180, 1, 1, 0, 0)
    rr = b"\xc0\x0c" + struct.pack("!HHIH", 1, 1, 30, 4) + ipaddress.ip_address(answer).packed
    return header + wire[12:] + rr


def frame(wire):
    return struct.pack("!H", len(wire)) + wire


def read_frame(conn):
    return read_exact(conn, struct.unpack("!H", read_exact(conn, 2))[0])


def check_answer(response, query, answer):
    assert response[:2] == query[:2], "transaction ID changed"
    assert question_name(response) == question_name(query), "question changed"
    assert response[3] & 15 == 0, "unexpected DNS response code"
    assert response[-4:] == ipaddress.ip_address(answer).packed, "wrong outbound selected"


class MockSOCKS(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self, answer, records, lock):
        self.answer, self.records, self.record_lock = answer, records, lock
        self.connections = 0
        self.held_queries = {}
        super().__init__(("127.0.0.1", 0), MockHandler)

    @contextlib.contextmanager
    def hold(self, name, network):
        gate = threading.Event()
        key = (name.lower(), network)
        with self.record_lock:
            self.held_queries[key] = gate
        try:
            yield gate
        finally:
            gate.set()
            with self.record_lock:
                self.held_queries.pop(key, None)

    def respond(self, wire, target, network):
        assert target[1] == 53, "non-DNS destination reached outbound"
        name = question_name(wire)
        with self.record_lock:
            self.records.append((self.answer, name, target, network))
            gate = self.held_queries.get((name.lower(), network))
        if gate is not None and not gate.wait(8):
            raise TimeoutError("test did not release the held DNS response")
        return make_answer(wire, self.answer)


class MockHandler(socketserver.BaseRequestHandler):
    def handle(self):
        conn = self.request
        conn.settimeout(8)
        with self.server.record_lock:
            self.server.connections += 1
        try:
            assert read_exact(conn, 1) == b"\x05"
            methods = read_exact(conn, read_exact(conn, 1)[0])
            assert 0 in methods
            conn.sendall(b"\x05\x00")
            version, command, reserved = read_exact(conn, 3)
            assert version == 5 and reserved == 0
            target = read_address(conn)
            if command == 1:
                conn.sendall(b"\x05\x00\x00" + encode_address("127.0.0.1", 0))
                wire = read_frame(conn)
                conn.sendall(frame(self.server.respond(wire, target, "tcp")))
            elif command == 3:
                with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                    udp.bind(("127.0.0.1", 0))
                    udp.settimeout(8)
                    conn.sendall(b"\x05\x00\x00" + encode_address(*udp.getsockname()))
                    packet, peer = udp.recvfrom(65535)
                    target, wire = decode_packet(packet)
                    response = self.server.respond(wire, target, "udp")
                    udp.sendto(b"\x00\x00\x00" + encode_address(*target) + response, peer)
                    with contextlib.suppress(EOFError, OSError):
                        while conn.recv(1024):
                            pass
            else:
                raise AssertionError("unexpected SOCKS command")
        except (EOFError, OSError):
            return


def unused_port():
    with socket.socket() as conn:
        conn.bind(("127.0.0.1", 0))
        return conn.getsockname()[1]


def connect(port):
    return socket.create_connection(("127.0.0.1", port), timeout=3)


def socks_request(port, target, command=1):
    conn = connect(port)
    conn.sendall(b"\x05\x01\x00")
    assert read_exact(conn, 2) == b"\x05\x00"
    conn.sendall(bytes([5, command, 0]) + encode_address(*target))
    version, reply, reserved = read_exact(conn, 3)
    assert version == 5 and reserved == 0
    bound = read_address(conn)
    return conn, reply, bound


def socks4_request(port, target, use_4a=False):
    conn = connect(port)
    host, target_port = target
    address = b"\x00\x00\x00\x01" if use_4a else ipaddress.IPv4Address(host).packed
    request = b"\x04\x01" + struct.pack("!H", target_port) + address + b"\x00"
    if use_4a:
        request += host.encode("ascii") + b"\x00"
    conn.sendall(request)
    response = read_exact(conn, 8)
    assert response[0] == 0
    return conn, response[1]


def http_request(port, target, first=b""):
    conn = connect(port)
    host, target_port = target
    authority = f"[{host}]:{target_port}" if ":" in host else f"{host}:{target_port}"
    conn.sendall(f"CONNECT {authority} HTTP/1.1\r\nHost: {authority}\r\n\r\n".encode() + first)
    response = bytearray()
    while not response.endswith(b"\r\n\r\n"):
        response.extend(read_exact(conn, 1))
        assert len(response) <= 8192
    return conn, int(response.split()[1])


def api_request(port, path="/configs", body=None, method="GET"):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", data=data,
                                 headers={"Content-Type": "application/json"}, method=method)
    with urllib.request.urlopen(req, timeout=10) as response:
        payload = response.read()
        return json.loads(payload) if payload else None


def expect_port_closed(port):
    try:
        with connect(port):
            raise AssertionError(f"disabled DNS proxy still accepts TCP on {port}")
    except ConnectionRefusedError:
        pass


def wait_dns_connections(api_port, host=None):
    deadline = time.monotonic() + 2
    while True:
        snapshot = api_request(api_port, "/connections")
        entries = [entry for entry in snapshot.get("connections") or []
                   if entry["metadata"].get("inboundName") == "DEFAULT-DNS-PROXY"]
        if host is None and not entries:
            return snapshot, None
        if host is not None:
            matching = [entry for entry in entries
                        if entry["metadata"].get("host") == host and entry["upload"] > 0]
            if len(matching) == 1:
                return snapshot, matching[0]
        if time.monotonic() >= deadline:
            raise AssertionError(f"unexpected dashboard connections for {host!r}: {entries}")
        time.sleep(0.01)


def check_dashboard_connections(dns_port, api_port, resolver, upstream):
    # Hold only the mock resolver's replies so the standard activity snapshot
    # can deterministically observe these otherwise very short DNS exchanges.
    for network in ("tcp", "udp"):
        for close_from_dashboard in (False, True):
            host = "cancel.example" if close_from_dashboard else "visible.example"
            query = question(host.upper(), 201 if network == "tcp" else 202)
            before, _ = wait_dns_connections(api_port)
            with upstream.hold(host, network) as release, contextlib.ExitStack() as stack:
                if network == "tcp":
                    client, reply, _ = socks_request(dns_port, resolver)
                    stack.enter_context(client)
                    assert reply == 0
                    client.sendall(frame(query))
                else:
                    client = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
                    client.bind(("127.0.0.1", 0))
                    client.settimeout(3)
                    client.sendto(b"\x00\x00\x00" + encode_address(*resolver) + query,
                                  ("127.0.0.1", dns_port))

                during, entry = wait_dns_connections(api_port, host)
                metadata = entry["metadata"]
                assert metadata["network"] == network and metadata["type"] == "Socks5"
                assert metadata["sourceIP"] == "127.0.0.1"
                assert int(metadata["sourcePort"]) == client.getsockname()[1]
                assert metadata["destinationIP"] == resolver[0]
                assert int(metadata["destinationPort"]) == 53
                assert int(metadata["inboundPort"]) == dns_port
                assert entry["rule"] == "Domain" and entry["rulePayload"] == host
                assert entry["chains"] == ["B", "Chosen", "DNSOuter"], entry["chains"]
                assert len(entry["providerChains"]) == len(entry["chains"])
                uploaded = len(query) + (2 if network == "tcp" else 0)
                assert entry["upload"] == uploaded and entry["download"] == 0
                assert during["uploadTotal"] - before["uploadTotal"] == uploaded

                if close_from_dashboard:
                    api_request(api_port, f"/connections/{entry['id']}", method="DELETE")
                    if network == "tcp":
                        client.settimeout(1)
                        assert client.recv(1) == b"", "dashboard close did not interrupt TCP DNS"
                    else:
                        client.settimeout(0.2)
                        try:
                            client.recvfrom(65535)
                            raise AssertionError("closed UDP DNS query still returned a response")
                        except socket.timeout:
                            pass
                    after, _ = wait_dns_connections(api_port)
                    assert after["downloadTotal"] == before["downloadTotal"]
                    release.set()
                else:
                    release.set()
                    if network == "tcp":
                        response = read_frame(client)
                    else:
                        _, response = decode_packet(client.recvfrom(65535)[0])
                    check_answer(response, query, upstream.answer)
                    after, _ = wait_dns_connections(api_port)
                    downloaded = len(response) + (2 if network == "tcp" else 0)
                    assert after["downloadTotal"] - before["downloadTotal"] == downloaded
                assert after["uploadTotal"] - before["uploadTotal"] == uploaded
    print("PASS dashboard connections: TCP/UDP QNAME, resolver, source, rule and nested group chains; exact traffic totals; completion cleanup and API close")


def run(binary):
    records, record_lock = [], threading.Lock()
    upstream_a = MockSOCKS("198.51.100.11", records, record_lock)
    upstream_b = MockSOCKS("198.51.100.22", records, record_lock)
    for upstream in (upstream_a, upstream_b):
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
    dns_port, api_port, mixed_port = unused_port(), unused_port(), unused_port()
    resolver = ("203.0.113.53", 53)
    resolver_v6 = ("2001:db8::53", 53)
    config = f"""mode: rule
log-level: debug
external-controller: 127.0.0.1:{api_port}
mixed-port: {mixed_port}
dns-proxy-port: {dns_port}
allow-lan: false
# Like mixed-port, allow-lan: false overrides this non-local bind address.
bind-address: 192.0.2.1
dns:
  enable: false
proxies:
  - name: A
    type: socks5
    server: 127.0.0.1
    port: {upstream_a.server_address[1]}
    udp: true
  - name: B
    type: socks5
    server: 127.0.0.1
    port: {upstream_b.server_address[1]}
    udp: true
proxy-groups:
  - name: Chosen
    type: select
    proxies: [A, B]
  - name: DNSOuter
    type: select
    proxies: [Chosen]
rules:
  - DOMAIN,first.example,A
  - DOMAIN,second.example,B
  - DOMAIN,selector.example,Chosen
  - DOMAIN,visible.example,DNSOuter
  - DOMAIN,cancel.example,DNSOuter
  - DOMAIN,blocked.example,REJECT
  - DOMAIN,drop.example,REJECT-DROP
  - MATCH,A
"""
    try:
        with tempfile.TemporaryDirectory(prefix="dns-proxy-e2e-") as tmp:
            config_dir = pathlib.Path(tmp)
            config_file = config_dir / "config.yaml"
            config_file.write_text(config, encoding="utf-8")
            subprocess.run([str(binary), "-t", "-d", str(config_dir), "-f", str(config_file)], check=True, timeout=30, capture_output=True)
            with (config_dir / "mihomo.log").open("w+") as log:
                proc = subprocess.Popen([str(binary), "-d", str(config_dir), "-f", str(config_file)], stdout=log, stderr=subprocess.STDOUT)
                try:
                    deadline = time.monotonic() + 15
                    while True:
                        if proc.poll() is not None:
                            raise AssertionError("binary exited during startup")
                        try:
                            with connect(dns_port):
                                break
                        except OSError:
                            if time.monotonic() >= deadline:
                                raise AssertionError("listener failed to start")
                            time.sleep(0.05)

                    general = api_request(api_port)
                    assert general["dns-proxy-port"] == dns_port
                    assert general["mixed-port"] == mixed_port
                    with connect(mixed_port):
                        pass

                    q1, q2 = question("first.example", 101), question("second.example", 102)
                    conn, status = http_request(dns_port, resolver, frame(q1))
                    with conn:
                        assert status == 200
                        check_answer(read_frame(conn), q1, upstream_a.answer)
                        conn.sendall(frame(q2))
                        check_answer(read_frame(conn), q2, upstream_b.answer)
                        chosen = question("selector.example", 103)
                        conn.sendall(frame(chosen))
                        check_answer(read_frame(conn), chosen, upstream_a.answer)
                        api_request(api_port, "/proxies/Chosen", {"name": "B"}, "PUT")
                        conn.sendall(frame(chosen))
                        check_answer(read_frame(conn), chosen, upstream_b.answer)
                    print("PASS HTTP CONNECT: pipelined first frame, per-question routes, live selector change")

                    conn, reply, _ = socks_request(dns_port, resolver_v6)
                    with conn:
                        assert reply == 0
                        for query, expected in ((q1, upstream_a.answer), (q2, upstream_b.answer)):
                            conn.sendall(frame(query))
                            check_answer(read_frame(conn), query, expected)
                    print("PASS SOCKS5 CONNECT: persistent TCP and unchanged IPv6 resolver target")

                    for use_4a, target in ((False, resolver), (True, resolver), (True, resolver_v6)):
                        conn, reply = socks4_request(dns_port, target, use_4a)
                        with conn:
                            assert reply == 90, "SOCKS4/4a DNS CONNECT rejected"
                            for query, expected in ((q1, upstream_a.answer), (q2, upstream_b.answer)):
                                conn.sendall(frame(query))
                                check_answer(read_frame(conn), query, expected)
                    print("PASS SOCKS4/4a: persistent TCP, per-question routes, literal IPv4/IPv6 resolver targets")

                    # SmartDNS sends an unspecified client IP with the resolver
                    # port before binding its own ephemeral UDP source port.
                    conn, reply, relay = socks_request(dns_port, ("0.0.0.0", 53), command=3)
                    with conn, socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                        assert reply == 0
                        udp.bind(("127.0.0.1", 0))
                        udp.settimeout(3)
                        for query, expected, target in ((q1, upstream_a.answer, resolver), (q2, upstream_b.answer, resolver), (q2, upstream_b.answer, resolver_v6)):
                            udp.sendto(b"\x00\x00\x00" + encode_address(*target) + query, relay)
                            packet, _ = udp.recvfrom(65535)
                            original_target, answer = decode_packet(packet)
                            assert original_target == target, "resolver destination was changed"
                            check_answer(answer, query, expected)
                        with record_lock:
                            before = len(records)
                            connections_before = upstream_a.connections + upstream_b.connections
                        udp.settimeout(0.2)
                        for packet in (
                            b"\x00\x00\x00" + encode_address(resolver[0], 443) + q1,
                            b"\x00\x00\x01" + encode_address(*resolver) + q1,
                            b"\x00\x00\x00" + encode_address(*resolver) + b"\x00" * 12,
                            b"\x00\x00\x00" + encode_address(*resolver) + question("drop.example"),
                        ):
                            udp.sendto(packet, relay)
                            try:
                                udp.recvfrom(65535)
                                raise AssertionError("forbidden UDP packet received a reply")
                            except socket.timeout:
                                pass
                        with record_lock:
                            assert len(records) == before, "forbidden UDP packet reached an outbound"
                            assert upstream_a.connections + upstream_b.connections == connections_before, "forbidden UDP packet dialed an outbound"
                        blocked = question("blocked.example")
                        udp.settimeout(3)
                        udp.sendto(b"\x00\x00\x00" + encode_address(*resolver) + blocked, relay)
                        _, response = decode_packet(udp.recvfrom(65535)[0])
                        assert response[3] & 15 == 5, "REJECT did not return DNS REFUSED"
                    print("PASS SOCKS5 UDP: SmartDNS ASSOCIATE, per-datagram routes, IPv4/IPv6 targets, drop/REFUSED, malformed and fragmented packets")

                    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                        udp.bind(("127.0.0.1", 0))
                        udp.settimeout(3)
                        for query, expected, target in ((q1, upstream_a.answer, resolver), (q2, upstream_b.answer, resolver_v6)):
                            udp.sendto(b"\x00\x00\x00" + encode_address(*target) + query, ("127.0.0.1", dns_port))
                            packet, source = udp.recvfrom(65535)
                            original_target, response = decode_packet(packet)
                            assert source[1] == dns_port, "fixed UDP entry did not reply from configured port"
                            assert original_target == target
                            check_answer(response, query, expected)
                        with record_lock:
                            connections_before = upstream_a.connections + upstream_b.connections
                        udp.settimeout(0.2)
                        for packet in (
                            b"\x00\x00\x00" + encode_address(resolver[0], 22) + q1,
                            b"\x00\x00\x00" + encode_address(*resolver) + b"\x00" * 12,
                            q1,  # Bare DNS is not SOCKS5 UDP, even on this port.
                        ):
                            udp.sendto(packet, ("127.0.0.1", dns_port))
                            try:
                                udp.recvfrom(65535)
                                raise AssertionError("forbidden fixed-port UDP packet received a reply")
                            except socket.timeout:
                                pass
                        with record_lock:
                            assert upstream_a.connections + upstream_b.connections == connections_before, "forbidden fixed-port UDP dialed an outbound"
                    print("PASS same-port SOCKS5 UDP: independent routes, unchanged targets, non-53/malformed/bare DNS discarded without dialing")

                    with record_lock:
                        before = len(records)
                        connections_before = upstream_a.connections + upstream_b.connections
                    for forbidden_port in (22, 443, 853):
                        target = (resolver[0], forbidden_port)
                        conn, status = http_request(dns_port, target)
                        with conn:
                            assert status == 403
                        conn, reply, _ = socks_request(dns_port, target)
                        with conn:
                            assert reply == 2, "SOCKS5 non-53 CONNECT accepted"
                        for use_4a in (False, True):
                            conn, reply = socks4_request(dns_port, target, use_4a)
                            with conn:
                                assert reply == 91, "SOCKS4/4a non-53 CONNECT accepted"
                    with connect(dns_port) as conn:
                        conn.sendall(b"GET http://example.invalid/ HTTP/1.1\r\nHost: example.invalid\r\n\r\n")
                        assert b" 405 " in conn.recv(1024)
                    # A successful CONNECT to port 53 is not an opaque tunnel.
                    # Non-DNS bytes must close it before an upstream connection.
                    for protocol in ("http", "socks5", "socks4", "socks4a"):
                        if protocol == "http":
                            conn, reply = http_request(dns_port, resolver)
                            assert reply == 200
                        elif protocol == "socks5":
                            conn, reply, _ = socks_request(dns_port, resolver)
                            assert reply == 0
                        else:
                            conn, reply = socks4_request(dns_port, resolver, protocol == "socks4a")
                            assert reply == 90
                        with conn:
                            conn.sendall(frame(b"GET /dns-query HTTP/1.1\r\n\r\n"))
                            assert conn.recv(1) == b"", f"{protocol} forwarded non-DNS traffic on port 53"
                    with record_lock:
                        assert len(records) == before, "forbidden request reached an outbound"
                        assert upstream_a.connections + upstream_b.connections == connections_before, "forbidden TCP request dialed an outbound"
                        assert all(target in (resolver, resolver_v6) for _, _, target, _ in records)
                    print("PASS all TCP protocols: non-53 destinations and non-DNS payloads rejected before any outbound dial")

                    check_dashboard_connections(dns_port, api_port, resolver, upstream_b)

                    # The new field follows the same live configuration paths as
                    # mixed-port, and closing it also closes existing sessions.
                    held, reply, _ = socks_request(dns_port, resolver)
                    assert reply == 0
                    api_request(api_port, body={"dns-proxy-port": 0}, method="PATCH")
                    with held:
                        assert held.recv(1) == b"", "disabled port left an active DNS connection open"
                    expect_port_closed(dns_port)
                    assert api_request(api_port)["dns-proxy-port"] == 0
                    with connect(mixed_port):
                        pass

                    moved_port = unused_port()
                    api_request(api_port, body={"dns-proxy-port": moved_port}, method="PATCH")
                    assert api_request(api_port)["dns-proxy-port"] == moved_port
                    conn, reply, _ = socks_request(moved_port, resolver)
                    with conn:
                        assert reply == 0
                        conn.sendall(frame(q2))
                        check_answer(read_frame(conn), q2, upstream_b.answer)
                    expect_port_closed(dns_port)

                    # A full forced reload exercises RawConfig -> General ->
                    # executor, including a missing field disabling the port.
                    without_dns_port = config.replace(f"dns-proxy-port: {dns_port}\n", "")
                    api_request(api_port, "/configs?force=true", {"payload": without_dns_port}, "PUT")
                    expect_port_closed(moved_port)
                    assert api_request(api_port)["dns-proxy-port"] == 0
                    api_request(api_port, "/configs?force=true", {"payload": config}, "PUT")
                    assert api_request(api_port)["dns-proxy-port"] == dns_port
                    conn, status = http_request(dns_port, resolver, frame(q1))
                    with conn:
                        assert status == 200
                        check_answer(read_frame(conn), q1, upstream_a.answer)
                    print("PASS top-level config: mixed coexists; GET/PATCH close, reopen and move; full reload handles omitted and configured ports")
                    print(f"PASS actual binary end-to-end: {len(records)} independently routed DNS exchanges")
                except BaseException:
                    log.flush()
                    log.seek(0)
                    print(log.read())
                    raise
                finally:
                    proc.terminate()
                    try:
                        proc.wait(timeout=8)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait(timeout=3)
    finally:
        for upstream in (upstream_a, upstream_b):
            upstream.shutdown()
            upstream.server_close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=pathlib.Path)
    run(parser.parse_args().binary.resolve(strict=True))
