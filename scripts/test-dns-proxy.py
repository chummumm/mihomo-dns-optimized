#!/usr/bin/env python3
"""Exercise ordinary proxy forwarding and native DNS routing in the binary.

No external DNS, network service, Python dependency, or privileged port is used.
Proxy traffic, including DNS on port 53, must retain its target and payload.
Only queries sent to the built-in DNS listener receive per-question DNS routing.
"""

import argparse
import contextlib
import errno
import ipaddress
import json
import pathlib
import os
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
import time
import urllib.request
import urllib.error


RAW_TCP_TARGET = ("203.0.113.55", 53)
SERVER_GREETING = b"ordinary-server-first-on-port-53\r\n"


def read_exact(conn, size):
    data = bytearray()
    while len(data) < size:
        part = conn.recv(size - len(data))
        if not part:
            raise EOFError("connection closed")
        data.extend(part)
    return bytes(data)


def encode_address(host, port):
    try:
        addr = ipaddress.ip_address(host)
    except ValueError:
        encoded = host.encode("ascii")
        return b"\x03" + bytes([len(encoded)]) + encoded + struct.pack("!H", port)
    return bytes([1 if addr.version == 4 else 4]) + addr.packed + struct.pack("!H", port)


def read_address(conn):
    kind = read_exact(conn, 1)[0]
    if kind == 3:
        return read_exact(conn, read_exact(conn, 1)[0]).decode("ascii"), struct.unpack("!H", read_exact(conn, 2))[0]
    if kind not in (1, 4):
        raise AssertionError("expected a literal resolver IP")
    host = str(ipaddress.ip_address(read_exact(conn, 4 if kind == 1 else 16)))
    return host, struct.unpack("!H", read_exact(conn, 2))[0]


def decode_packet(packet):
    assert packet[:3] == b"\x00\x00\x00", "invalid SOCKS5 UDP envelope"
    kind = packet[3]
    if kind == 3:
        size = packet[4]
        host = packet[5 : 5 + size].decode("ascii")
        port = struct.unpack("!H", packet[5 + size : 7 + size])[0]
        return (host, port), packet[7 + size :]
    assert kind in (1, 4)
    size = 4 if kind == 1 else 16
    host = str(ipaddress.ip_address(packet[4 : 4 + size]))
    port = struct.unpack("!H", packet[4 + size : 6 + size])[0]
    return (host, port), packet[6 + size :]


def question(name, transaction=1234):
    wire_name = b"".join(bytes([len(label)]) + label.encode("ascii") for label in name.split(".")) + b"\x00"
    return struct.pack("!6H", transaction, 0x0100, 1, 0, 0, 0) + wire_name + struct.pack("!2H", 1, 1)


def malformed_queries(query):
    """Packets a permissive DNS unpacker can accept despite invalid structure."""
    cases = [b"GET /dns-query HTTP/1.1\r\n\r\n", query + b"\x00"]
    for offset in (6, 8, 10):
        packet = bytearray(query)
        struct.pack_into("!H", packet, offset, 1)
        cases.append(bytes(packet))  # One advertised RR, no record on the wire.
    opt = b"\x00" + struct.pack("!HHIH", 41, 1232, 0, 0)
    packet = bytearray(query)
    struct.pack_into("!H", packet, 10, 2)
    cases.append(bytes(packet) + opt + opt)
    return cases


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
        self.raw_records = []
        self.wire_records = []
        self.tcp_streams = []
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
        ipaddress.ip_address(target[0])  # The resolver hostname must be bootstrapped before DNS routing.
        name = question_name(wire)
        with self.record_lock:
            self.records.append((self.answer, name, target, network))
            gate = self.held_queries.get((name.lower(), network))
        if gate is not None and not gate.wait(8):
            raise TimeoutError("test did not release the held DNS response")
        error_code = {"formerr.example": 1, "servfail.example": 2, "refused.example": 5}.get(name)
        if error_code is not None:
            return struct.pack("!6H", struct.unpack("!H", wire[:2])[0], 0x8180 | error_code, 0, 0, 0, 0)
        if name == "truncated.example":
            return struct.pack("!6H", struct.unpack("!H", wire[:2])[0], 0x8380, 1, 0, 0, 0) + wire[12:]
        return make_answer(wire, self.answer)

    def respond_or_echo(self, wire, target, network):
        with self.record_lock:
            self.wire_records.append((target, network, wire))
        ordinary = False
        if target[1] == 53 and len(wire) >= 17:
            _, flags, questions, answers, authority, additional = struct.unpack("!6H", wire[:12])
            try:
                name = question_name(wire)
                ordinary = (flags & 0xFA0F == 0 and questions == 1 and
                            answers == authority == additional == 0 and
                            len(question(name)) == len(wire))
            except (IndexError, UnicodeDecodeError, ValueError):
                pass
        if ordinary:
            return self.respond(wire, target, network)
        with self.record_lock:
            self.raw_records.append((target, network, wire))
        return wire


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
                stream = (target, [])
                with self.server.record_lock:
                    self.server.tcp_streams.append(stream)
                conn.sendall(b"\x05\x00\x00" + encode_address("127.0.0.1", 0))
                if target == RAW_TCP_TARGET:
                    conn.sendall(SERVER_GREETING)
                while True:
                    if target[1] == 53 and target != RAW_TCP_TARGET:
                        wire = read_frame(conn)
                        with self.server.record_lock:
                            stream[1].append(wire)
                        conn.sendall(frame(self.server.respond_or_echo(wire, target, "tcp")))
                    else:
                        wire = conn.recv(65535)
                        if not wire:
                            return
                        with self.server.record_lock:
                            stream[1].append(wire)
                        conn.sendall(self.server.respond_or_echo(wire, target, "tcp"))
            elif command == 3:
                with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                    udp.bind(("127.0.0.1", 0))
                    udp.settimeout(8)
                    conn.sendall(b"\x05\x00\x00" + encode_address(*udp.getsockname()))
                    while True:
                        packet, peer = udp.recvfrom(65535)
                        target, wire = decode_packet(packet)
                        response = self.server.respond_or_echo(wire, target, "udp")
                        if response != wire and question_name(wire) == "retry.example":
                            wrong_id = struct.pack("!H", (struct.unpack("!H", wire[:2])[0] + 1) % 65536) + response[2:]
                            wrong_question = make_answer(question("mismatch.example", struct.unpack("!H", wire[:2])[0]), self.server.answer)
                            for unrelated in (wrong_id, wrong_question):
                                udp.sendto(b"\x00\x00\x00" + encode_address(*target) + unrelated, peer)
                        udp.sendto(b"\x00\x00\x00" + encode_address(*target) + response, peer)
            else:
                raise AssertionError("unexpected SOCKS command")
        except (EOFError, OSError):
            return


allocated_ports = set()


@contextlib.contextmanager
def reserve_local_port():
    # TCP's ephemeral allocator does not know about existing SOCKS UDP relays.
    # Hold both bindings while checking, and do not reuse an earlier returned
    # fixture port which its caller may not have started listening on yet.
    for _ in range(128):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as tcp, \
                socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
            tcp.bind(("127.0.0.1", 0))
            address = tcp.getsockname()
            if address[1] in allocated_ports:
                continue
            try:
                udp.bind(address)
            except OSError as error:
                if error.errno != errno.EADDRINUSE:
                    raise
                continue
            allocated_ports.add(address[1])
            yield tcp, udp
            return
    raise RuntimeError("could not allocate an unused TCP/UDP fixture port")


def unused_port():
    with reserve_local_port() as (tcp, _):
        return tcp.getsockname()[1]


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


def wait_proxy_connection(api_port, network, source_port, uploaded=0, downloaded=0, missing=False,
                          destination_port=53):
    deadline = time.monotonic() + 3
    while True:
        entries = [entry for entry in (api_request(api_port, "/connections").get("connections") or [])
                   if entry["metadata"].get("inboundName") in ("DEFAULT-MIXED", "DEFAULT-SOCKS")
                   and entry["metadata"].get("network") == network
                   and int(entry["metadata"].get("sourcePort", 0)) == source_port
                   and int(entry["metadata"].get("destinationPort", 0)) == destination_port]
        if missing and not entries:
            return None
        if not missing and len(entries) == 1 and entries[0]["upload"] >= uploaded and entries[0]["download"] >= downloaded:
            return entries[0]
        if time.monotonic() >= deadline:
            raise AssertionError(f"unexpected {network} proxy connections for source port {source_port}: {entries}")
        time.sleep(0.01)


def check_dashboard_connections(proxy_port, api_port, resolver, upstream):
    # Ordinary proxy connections describe the resolver destination, not an
    # inferred QNAME. They persist across requests and use the normal API close.
    for network in ("tcp", "udp"):
        for close_from_dashboard in (False, True):
            host = "cancel.example" if close_from_dashboard else "visible.example"
            query = question(host.upper(), 201 if network == "tcp" else 202)
            with upstream.hold(host, network) as release, contextlib.ExitStack() as stack:
                if network == "tcp":
                    client, reply, _ = socks_request(proxy_port, resolver)
                    stack.enter_context(client)
                    assert reply == 0
                    client.sendall(frame(query))
                else:
                    client = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
                    client.bind(("127.0.0.1", 0))
                    client.settimeout(3)
                    client.sendto(b"\x00\x00\x00" + encode_address(*resolver) + query,
                                  ("127.0.0.1", proxy_port))
                source_port = client.getsockname()[1]
                uploaded = len(query) + (2 if network == "tcp" else 0)
                entry = wait_proxy_connection(api_port, network, source_port, uploaded)
                metadata = entry["metadata"]
                assert metadata["type"] == "Socks5" and not metadata.get("host"), metadata
                assert metadata["sourceIP"] == "127.0.0.1"
                assert metadata["destinationIP"] == resolver[0]
                assert int(metadata["inboundPort"]) == proxy_port
                assert entry["rule"] == "IPCIDR" and entry["rulePayload"] == resolver[0] + "/32"
                assert entry["chains"] == ["A"], entry["chains"]
                assert entry["upload"] == uploaded and entry["download"] == 0

                if not close_from_dashboard:
                    release.set()
                    if network == "tcp":
                        response = read_frame(client)
                    else:
                        target, response = decode_packet(client.recvfrom(65535)[0])
                        assert target == resolver
                    check_answer(response, query, upstream.answer)
                    downloaded = len(response) + (2 if network == "tcp" else 0)
                    after = wait_proxy_connection(api_port, network, source_port, uploaded, downloaded)
                    assert after["id"] == entry["id"], "DNS reply replaced the ordinary connection"
                    assert after["upload"] == uploaded and after["download"] == downloaded

                api_request(api_port, f"/connections/{entry['id']}", method="DELETE")
                wait_proxy_connection(api_port, network, source_port, missing=True)
                release.set()
                if network == "tcp":
                    assert client.recv(1) == b"", "dashboard close did not interrupt TCP forwarding"
                elif close_from_dashboard:
                    client.settimeout(0.2)
                    try:
                        client.recvfrom(65535)
                        raise AssertionError("closed UDP connection still returned a response")
                    except socket.timeout:
                        pass
    print("PASS ordinary dashboard: resolver IP, no QNAME, destination rule, persistent TCP/UDP trackers, exact connection traffic and API close")


def check_reply_compatibility(dns_port, resolver, expected_answer):
    cases = [("formerr.example", 1), ("servfail.example", 2), ("refused.example", 5), ("truncated.example", 0)]

    def check(response, query, name, rcode):
        assert response[:2] == query[:2], "error response transaction ID changed"
        assert response[2] & 0x80 and response[3] & 15 == rcode
        if rcode:
            assert len(response) == 12 and response[4:] == b"\x00" * 8, "header-only error was rewritten"
        else:
            assert response[2] & 2 and question_name(response) == name, "TC response was not preserved"

    conn, reply, _ = socks_request(dns_port, resolver)
    with conn:
        assert reply == 0
        for index, (name, rcode) in enumerate(cases):
            query = question(name, 200 + index)
            conn.sendall(frame(query))
            check(read_frame(conn), query, name, rcode)

    conn, reply, relay = socks_request(dns_port, ("0.0.0.0", 53), command=3)
    with conn, socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
        assert reply == 0
        udp.bind(("127.0.0.1", 0))
        udp.settimeout(3)
        for index, (name, rcode) in enumerate(cases):
            query = question(name, 210 + index)
            udp.sendto(b"\x00\x00\x00" + encode_address(*resolver) + query, relay)
            target, response = decode_packet(udp.recvfrom(65535)[0])
            assert target == resolver
            check(response, query, name, rcode)
        query = question("retry.example", 220)
        udp.sendto(b"\x00\x00\x00" + encode_address(*resolver) + query, relay)
        answer = make_answer(query, expected_answer)
        wrong_id = struct.pack("!H", 221) + answer[2:]
        wrong_question = make_answer(question("mismatch.example", 220), expected_answer)
        for expected in (wrong_id, wrong_question, answer):
            target, response = decode_packet(udp.recvfrom(65535)[0])
            assert target == resolver and response == expected, "ordinary UDP relay filtered or rewrote a datagram"
    print("PASS response forwarding: unchanged TCP/UDP DNS errors and TC; UDP passes every response without transaction/question filtering")


def check_rule_control_actions(dns_port, expected_answer):
    # These are real parsed groups, including the upstream round-robin strategy.
    # PASS must not advance its cursor; neither control action needs UDP support.
    queries = [question(name, 230 + index) for index, name in enumerate(
        ("pass-control.example", "pass-control.example", "sub-control.example", "sub-control.example")
    )]
    with connect(dns_port) as conn:
        for query in queries:
            conn.sendall(frame(query))
            check_answer(read_frame(conn), query, expected_answer)
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
        udp.bind(("127.0.0.1", 0))
        udp.settimeout(3)
        for query in queries:
            udp.sendto(query, ("127.0.0.1", dns_port))
            check_answer(udp.recvfrom(65535)[0], query, expected_answer)
    print("PASS native rule controls: real round-robin PASS cursor unchanged; TCP/UDP PASS and SUB-RULE PASS-RULE continue past UDP-disabled control groups")


def check_builtin_dns(config, api_port, upstream_a, upstream_b):
    dns_port = unused_port()
    builtin_config = config.replace("  enable: false\n", f"""  enable: true
  listen: 127.0.0.1:{dns_port}
  enhanced-mode: redir-host
  use-hosts: false
  respect-rules: false
  nameserver: [203.0.113.53]
  # This unavailable old policy must be inactive when the switch is enabled.
  nameserver-policy:
    'second.example': [127.0.0.1:9]
""")
    builtin_config = builtin_config.replace("  - DOMAIN,first.example,A\n", f"""  - AND,((DOMAIN,builtin-udp.example),(IN-NAME,DNS),(IN-PORT,{dns_port}),(SRC-IP-CIDR,127.0.0.1/32),(DST-PORT,53),(NETWORK,udp)),B
  - AND,((DOMAIN,builtin-tcp.example),(IN-NAME,DNS),(IN-PORT,{dns_port}),(SRC-IP-CIDR,127.0.0.1/32),(DST-PORT,53),(NETWORK,tcp)),B
  - DOMAIN,first.example,A
""")
    api_request(api_port, "/configs", {"payload": builtin_config}, "PUT")
    deadline = time.monotonic() + 10
    while True:
        try:
            with connect(dns_port):
                break
        except OSError:
            if time.monotonic() >= deadline:
                raise AssertionError("built-in DNS server failed to start")
            time.sleep(0.05)

    with connect(dns_port) as tcp:
        for index, (name, answer) in enumerate((("first.example", upstream_a.answer),
                                               ("second.example", upstream_b.answer),
                                               ("builtin-tcp.example", upstream_b.answer))):
            query = question(name, 501 + index)
            tcp.sendall(frame(query))
            check_answer(read_frame(tcp), query, answer)

    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
        udp.bind(("127.0.0.1", 0))
        udp.settimeout(3)

        def exchange(query):
            udp.sendto(query, ("127.0.0.1", dns_port))
            return udp.recvfrom(65535)[0]

        for index, (name, answer) in enumerate((("first.example", upstream_a.answer),
                                               ("second.example", upstream_b.answer),
                                               ("builtin-udp.example", upstream_b.answer))):
            query = question(name, 511 + index)
            check_answer(exchange(query), query, answer)

        query = question("retry.example", 519)
        check_answer(exchange(query), query, upstream_a.answer)

        # Keep the same source socket so this is a real cache-key collision if
        # the selected leaf is missing from the resolver's route scope.
        for index, upstream in enumerate((upstream_a, upstream_b, upstream_b)):
            api_request(api_port, "/proxies/Chosen", {"name": "A" if upstream is upstream_a else "B"}, "PUT")
            query = question("selector.example", 520 + index)
            check_answer(exchange(query), query, upstream.answer)

        query = question("blocked.example", 530)
        response = exchange(query)
        assert response[:2] == query[:2] and response[3] & 15 == 5
        udp.settimeout(0.4)
        udp.sendto(question("drop.example", 531), ("127.0.0.1", dns_port))
        try:
            udp.recvfrom(65535)
            raise AssertionError("built-in REJECT-DROP replied instead of dropping")
        except socket.timeout:
            pass
        udp.settimeout(3)

        with upstream_b.hold("visible.example", "udp") as gate:
            query = question("visible.example", 540)
            udp.sendto(query, ("127.0.0.1", dns_port))
            deadline = time.monotonic() + 3
            while True:
                matches = [item for item in (api_request(api_port, "/connections").get("connections") or [])
                           if item["metadata"].get("inboundName") == "DNS" and
                           item["metadata"].get("host") == "visible.example"]
                if matches:
                    break
                if time.monotonic() >= deadline:
                    raise AssertionError("built-in DNS query missing from dashboard")
                time.sleep(0.03)
            metadata = matches[0]["metadata"]
            assert int(metadata["sourcePort"]) == udp.getsockname()[1]
            assert int(metadata["inboundPort"]) == dns_port
            assert int(metadata["destinationPort"]) == 53 and metadata["destinationIP"] == "203.0.113.53"
            assert matches[0]["chains"][:3] == ["B", "Chosen", "DNSOuter"]
            gate.set()
            check_answer(udp.recvfrom(65535)[0], query, upstream_b.answer)
    print("PASS built-in DNS TCP/UDP: QNAME routing, real IN/SRC/transport metadata, disabled nameserver-policy, selector-aware cache/ID, REJECT/DROP and dashboard")
    check_rule_control_actions(dns_port, upstream_b.answer)


@contextlib.contextmanager
def local_plain_dns(answer):
    """A local direct-pool endpoint, with ordinary DNS on an unprivileged port."""
    records, lock = [], threading.Lock()

    def respond(wire, network):
        with lock:
            records.append((question_name(wire), network))
        return make_answer(wire, answer)

    class TCPHandler(socketserver.BaseRequestHandler):
        def handle(self):
            self.request.settimeout(3)
            try:
                while True:
                    self.request.sendall(frame(respond(read_frame(self.request), "tcp")))
            except (EOFError, OSError):
                pass

    class UDPHandler(socketserver.BaseRequestHandler):
        def handle(self):
            wire, sock = self.request
            sock.sendto(respond(wire, "udp"), self.client_address)

    # Adopt the already bound sockets, so another ephemeral UDP allocation
    # cannot steal this fixture's port between checking it and starting it.
    with reserve_local_port() as (tcp_socket, udp_socket):
        address = tcp_socket.getsockname()
        with socketserver.ThreadingTCPServer(address, TCPHandler, bind_and_activate=False) as tcp, \
                socketserver.ThreadingUDPServer(address, UDPHandler, bind_and_activate=False) as udp:
            tcp.socket.close()
            tcp.socket = tcp_socket
            tcp.server_activate()
            udp.socket.close()
            udp.socket = udp_socket
            tcp.daemon_threads = True
            udp.daemon_threads = True
            for server in (tcp, udp):
                threading.Thread(target=server.serve_forever, daemon=True).start()
            try:
                yield tcp.server_address[1], records, lock
            finally:
                for server in (tcp, udp):
                    server.shutdown()


def check_native_dns_pools(config, api_port, upstream_a, upstream_b):
    # Native transports route their own questions on any configured port.
    # Real speed probes have separate Go tests, not this reserved-address fixture.
    direct_answer = "198.51.100.33"
    dns_port = unused_port()
    with local_plain_dns(direct_answer) as (direct_port, direct_records, direct_lock):
        for network in ("udp", "tcp"):
            endpoint = f"127.0.0.1:{direct_port}"
            if network == "tcp":
                endpoint = "tcp://" + endpoint
            native_config = config.replace("  enable: false\n", f"""  enable: true
  listen: 127.0.0.1:{dns_port}
  enhanced-mode: redir-host
  use-hosts: false
  respect-rules: false
  nameserver: [203.0.113.53]
  direct-nameserver: [{endpoint}]
  speed-check-mode: [none]
  speed-check-timeout: 1000
  speed-check-concurrency: 16
  prefetch-domain: true
  serve-expired: true
  serve-expired-ttl: 604800
  serve-expired-reply-ttl: 1
""")
            native_config = native_config.replace("  - name: Chosen\n    type: select\n    proxies: [A, B]\n",
                                                  "  - name: Chosen\n    type: select\n    proxies: [A, B, DIRECT]\n")
            native_config = native_config.replace("  - DOMAIN,first.example,A\n", """  - DOMAIN-SUFFIX,native-blocked.example,REJECT
  - AND,((SRC-IP-CIDR,127.0.0.2/32),(OR,(DOMAIN,source-upgrade.example),(DOMAIN-SUFFIX,source-git.example),(DOMAIN-SUFFIX,source-docker.example))),B
  - OR,((SRC-IP-CIDR,127.0.0.2/32),(SRC-IP-CIDR,127.0.0.3/32)),DIRECT
  - SRC-IP-CIDR,127.0.0.4/32,A
  - DOMAIN-SUFFIX,native-direct.example,DIRECT
  - DOMAIN-SUFFIX,native-proxy.example,B
  - DOMAIN-SUFFIX,native-selector.example,Chosen
  - DOMAIN,first.example,A
""")
            api_request(api_port, "/configs", {"payload": native_config}, "PUT")

            def seen(name):
                with direct_lock:
                    direct = [row for row in direct_records if row[0] == name]
                with upstream_a.record_lock:
                    proxied = [row for row in upstream_a.records if row[1] == name]
                assert all(transport == network for _, transport in direct), direct
                assert all(target == ("203.0.113.53", 53) for _, _, target, _ in proxied), proxied
                return len(direct), len(proxied)

            # TCP clients and UDP clients both enter the primary native
            # resolver; neither sends these queries through the mixed port.
            with connect(dns_port) as tcp:
                for index, (suffix, answer, counts) in enumerate((
                        ("native-direct.example", direct_answer, (1, 0)),
                        ("native-proxy.example", upstream_b.answer, (0, 1)))):
                    name = f"{network}.{suffix}"
                    query = question(name, 601 + index)
                    tcp.sendall(frame(query))
                    check_answer(read_frame(tcp), query, answer)
                    assert seen(name) == counts, (name, seen(name), counts)

            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                udp.bind(("127.0.0.1", 0))
                udp.settimeout(3)

                def exchange(query):
                    udp.sendto(query, ("127.0.0.1", dns_port))
                    return udp.recvfrom(65535)[0]

                # Same question/source socket, including returning to a prior
                # pool: each pool/leaf keeps its own cached answer and ID.
                # Two hits per scope also keep optional prefetch below its
                # threshold, so background work cannot alter these counters.
                name = f"{network}.native-selector.example"
                for index, (choice, answer, counts) in enumerate((
                        ("DIRECT", direct_answer, (1, 0)),
                        ("B", upstream_b.answer, (1, 1)),
                        ("B", upstream_b.answer, (1, 1)),
                        ("DIRECT", direct_answer, (1, 1)),
                        ("A", upstream_a.answer, (1, 2)),
                        ("A", upstream_a.answer, (1, 2)))):
                    api_request(api_port, "/proxies/Chosen", {"name": choice}, "PUT")
                    query = question(name, 611 + index)
                    check_answer(exchange(query), query, answer)
                    assert seen(name) == counts, (network, choice, seen(name), counts)

                name = f"{network}.native-blocked.example"
                query = question(name, 620)
                response = exchange(query)
                assert response[:2] == query[:2] and response[3] & 15 == 5
                assert seen(name) == (0, 0), "REJECT contacted a native DNS pool"

            # Preserve the user's rule shape: a source+domain exception first,
            # followed by a source-only DIRECT rule. Loopback aliases require
            # no extra interface, privilege, NAT or external DNS service.
            with contextlib.ExitStack() as stack:
                sources = {}
                source_port = 0
                for address in ("127.0.0.2", "127.0.0.3", "127.0.0.4"):
                    client = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
                    client.bind((address, source_port))
                    source_port = client.getsockname()[1]
                    client.settimeout(3)
                    sources[address] = client

                def source_exchange(address, name, transaction, answer):
                    client = sources[address]
                    query = question(name, transaction)
                    client.sendto(query, ("127.0.0.1", dns_port))
                    check_answer(client.recvfrom(65535)[0], query, answer)

                name = f"{network}.source-git.example"
                for index, (address, answer, counts) in enumerate((
                        ("127.0.0.2", upstream_b.answer, (0, 1)),
                        ("127.0.0.3", direct_answer, (1, 1)),
                        ("127.0.0.4", upstream_a.answer, (1, 2)),
                        ("127.0.0.2", upstream_b.answer, (1, 2)),
                        ("127.0.0.3", direct_answer, (1, 2)),
                        ("127.0.0.4", upstream_a.answer, (1, 2)))):
                    source_exchange(address, name, 630 + index, answer)
                    assert seen(name) == counts, ("source route/cache", address, seen(name), counts)

                # Same QNAME, leaf, client network and source port: only the
                # source IP differs, so neither source may reuse the other's
                # query cache even when both selected DIRECT.
                name = f"{network}.source-other.example"
                for index, (address, counts) in enumerate((
                        ("127.0.0.2", (1, 0)), ("127.0.0.3", (2, 0)),
                        ("127.0.0.2", (2, 0)), ("127.0.0.3", (2, 0)))):
                    source_exchange(address, name, 640 + index, direct_answer)
                    assert seen(name) == counts, ("source IP cache scope", address, seen(name), counts)
                name = f"source-{network}.native-blocked.example"
                query = question(name, 645)
                sources["127.0.0.2"].sendto(query, ("127.0.0.1", dns_port))
                response = sources["127.0.0.2"].recvfrom(65535)[0]
                assert response[:2] == query[:2] and response[3] & 15 == 5
                assert seen(name) == (0, 0), "blocklist lost priority to source DIRECT"

                for index, (address, answer) in enumerate((("127.0.0.2", upstream_b.answer),
                                                         ("127.0.0.3", direct_answer))):
                    tcp = stack.enter_context(socket.socket(socket.AF_INET, socket.SOCK_STREAM))
                    tcp.bind((address, 0))
                    tcp.settimeout(3)
                    tcp.connect(("127.0.0.1", dns_port))
                    name = f"tcp-{network}.source-git.example"
                    for repeated in range(2):
                        query = question(name, 650 + index * 2 + repeated)
                        tcp.sendall(frame(query))
                        check_answer(read_frame(tcp), query, answer)
                    assert seen(name) == ((0, 1) if index == 0 else (1, 1))
        with direct_lock:
            direct_count = len(direct_records)
    print("PASS native DNS first-query pools: DIRECT only uses local UDP/TCP upstream; proxy retains main IP:53; same-source DIRECT/proxy cache isolation and REJECT before both pools (automatic high-port native transport, no privileged port or speed probe)")
    print("PASS source rules: real TCP/UDP clients, source+domain exception before source DIRECT, same-port distinct-source cache isolation including the same leaf, and blocklist precedence")
    return direct_count


def check_redir_host_filter(api_port, proxy_port, upstream_a, upstream_b):
    # The two names have different documentation IPs, so this checks the basic
    # domain blacklist without imposing a shared-IP exclusion policy.
    blocked, allowed = "blocked.mapping.test", "allowed.mapping.test"
    blocked_ip, allowed_ip = "192.0.2.101", "192.0.2.102"
    dns_port, target_port = unused_port(), 18080
    with local_plain_dns(blocked_ip) as (blocked_port, blocked_records, blocked_lock), \
            local_plain_dns(allowed_ip) as (allowed_port, allowed_records, allowed_lock), \
            socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
        udp.bind(("127.0.0.1", 0))
        udp.settimeout(3)

        def apply(patterns, sniff=False):
            # A standalone rule list keeps the earlier port-53 fixture's IP
            # rules from hiding whether the inferred domain actually matched.
            mapping_config = f"""mode: rule
log-level: debug
external-controller: 127.0.0.1:{api_port}
mixed-port: {proxy_port}
allow-lan: false
bind-address: 127.0.0.1
find-process-mode: off
ipv6: false
dns-rule-routing: false
dns:
  enable: true
  listen: 127.0.0.1:{dns_port}
  enhanced-mode: redir-host
  redir-host-filter: {json.dumps(patterns)}
  use-hosts: false
  use-system-hosts: false
  respect-rules: false
  default-nameserver: [127.0.0.1:{blocked_port}]
  nameserver: [127.0.0.1:{blocked_port}]
  nameserver-policy:
    '{blocked}': [127.0.0.1:{blocked_port}]
    '{allowed}': [127.0.0.1:{allowed_port}]
  speed-check-mode: [none]
  prefetch-domain: false
  serve-expired: false
sniffer:
  enable: {str(sniff).lower()}
  parse-pure-ip: true
  force-dns-mapping: true
  override-destination: false
  sniff:
    HTTP:
      ports: [{target_port}]
proxies:
  - name: A
    type: socks5
    server: 127.0.0.1
    port: {upstream_a.server_address[1]}
  - name: B
    type: socks5
    server: 127.0.0.1
    port: {upstream_b.server_address[1]}
rules:
  - DOMAIN,{blocked},B
  - DOMAIN,{allowed},B
  - MATCH,A
"""
            api_request(api_port, "/configs", {"payload": mapping_config}, "PUT")
            assert api_request(api_port)["dns-rule-routing"] is False

        def exchange(name, answer, transaction):
            query = question(name, transaction)
            udp.sendto(query, ("127.0.0.1", dns_port))
            check_answer(udp.recvfrom(65535)[0], query, answer)

        def seen():
            with blocked_lock:
                assert all(name == blocked for name, _ in blocked_records), blocked_records
                blocked_count = len(blocked_records)
            with allowed_lock:
                assert all(name == allowed for name, _ in allowed_records), allowed_records
                allowed_count = len(allowed_records)
            return blocked_count, allowed_count

        def probe(label, destination, outbound, host="", sniff_host="", payload=None):
            if payload is None:
                payload = ("redir-host-filter:" + label).encode("ascii")
            upstream = upstream_b if outbound == "B" else upstream_a
            with upstream.record_lock:
                before = len(upstream.tcp_streams)
            conn, reply, _ = socks_request(proxy_port, (destination, target_port))
            with conn:
                assert reply == 0, (label, reply)
                conn.sendall(payload)
                assert read_exact(conn, len(payload)) == payload, (label, "payload changed")
                entry = wait_proxy_connection(api_port, "tcp", conn.getsockname()[1],
                                               len(payload), len(payload), destination_port=target_port)
                metadata = entry["metadata"]
                assert entry["chains"] == [outbound], (label, entry)
                assert (metadata.get("host") or "") == host, (label, metadata)
                assert (metadata.get("sniffHost") or "") == sniff_host, (label, metadata)
                if destination in (blocked_ip, allowed_ip):
                    assert metadata["destinationIP"] == destination, (label, metadata)
                if outbound == "B":
                    assert entry["rule"] == "Domain" and entry["rulePayload"] == (sniff_host or host), (label, entry)
                else:
                    assert entry["rule"] == "Match", (label, entry)
                with upstream.record_lock:
                    streams = [(target, b"".join(parts)) for target, parts in upstream.tcp_streams[before:]]
                target = (host or destination, target_port)
                assert (target, payload) in streams, (label, target, streams)

        apply([])
        exchange(blocked, blocked_ip, 701)
        exchange(allowed, allowed_ip, 702)
        assert seen() == (1, 1), seen()
        probe("warm-blocked", blocked_ip, "B", host=blocked)
        probe("warm-allowed", allowed_ip, "B", host=allowed)

        # No new DNS queries occur between adding the blacklist and these
        # connections: the results must come from the inherited mapping state.
        apply([blocked.upper() + "."])
        probe("reload-blocked", blocked_ip, "A")
        probe("reload-allowed", allowed_ip, "B", host=allowed)
        assert seen() == (1, 1), seen()
        exchange(blocked, blocked_ip, 703)
        exchange(blocked, blocked_ip, 704)
        assert seen() == (2, 1), ("DNS answer cache was not retained between queries", seen())
        probe("filtered-cold-and-warm-dns", blocked_ip, "A")
        probe("explicit-domain", blocked, "B", host=blocked)

        apply([blocked], sniff=True)
        http_payload = f"GET /mapping-filter HTTP/1.1\r\nHost: {blocked}\r\nConnection: keep-alive\r\n\r\n".encode("ascii")
        probe("http-sniff", blocked_ip, "B", sniff_host=blocked, payload=http_payload)
        assert seen() == (2, 1), ("ordinary proxy/sniffing unexpectedly queried DNS", seen())

        apply([])
        probe("removed-filter-before-dns", blocked_ip, "A")
        probe("allowed-still-inherited", allowed_ip, "B", host=allowed)
        exchange(blocked, blocked_ip, 705)
        probe("removed-filter-after-dns", blocked_ip, "B", host=blocked)
        assert seen() == (3, 1), seen()
        exchange_count = sum(seen())
    print("PASS redir-host filter: real config reload removes old blocked mappings and keeps allowed mappings; cold/warm DNS answers remain valid and cached; removing the filter restores mapping after a new query")
    print("PASS redir-host filter routing: pure IP follows MATCH, explicit SOCKS domain and HTTP sniffHost still match domain rules; override-destination=false preserves the IP and exact echoed HTTP payload")
    return exchange_count


def run(binary):
    records, record_lock = [], threading.Lock()
    upstream_a = MockSOCKS("198.51.100.11", records, record_lock)
    upstream_b = MockSOCKS("198.51.100.22", records, record_lock)
    for upstream in (upstream_a, upstream_b):
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
    mixed_port, api_port, fixed_port, scoped_port = [unused_port() for _ in range(4)]
    resolver = ("203.0.113.53", 53)
    resolver_v6 = ("2001:db8::53", 53)
    resolver_b = ("203.0.113.54", 53)
    process_name = pathlib.Path(os.readlink('/proc/self/exe')).name
    config = f"""mode: rule
log-level: debug
external-controller: 127.0.0.1:{api_port}
mixed-port: {mixed_port}
dns-rule-routing: true
allow-lan: false
bind-address: 192.0.2.1
find-process-mode: strict
hosts:
  resolver.bootstrap.test: 203.0.113.53
dns:
  enable: false
listeners:
  - name: FixedDNS
    type: mixed
    listen: 127.0.0.1
    port: {fixed_port}
    proxy: B
  - name: ScopedDNS
    type: mixed
    listen: 127.0.0.1
    port: {scoped_port}
    rule: ScopedRules
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
  - name: GLOBAL
    type: select
    proxies: [A, B]
  - name: Chosen
    type: select
    proxies: [A, B]
  - name: DNSOuter
    type: select
    proxies: [Chosen]
  - name: PassRoundRobin
    type: load-balance
    strategy: round-robin
    url: " "
    disable-udp: true
    proxies: [PASS, A]
  - name: PassRuleGroup
    type: select
    disable-udp: true
    proxies: [PASS-RULE]
sub-rules:
  DNSControl:
    - MATCH,PassRuleGroup
    - MATCH,B
  ScopedRules:
    - MATCH,B
rules:
  # Proxy connections use the resolver IP; native DNS must not mistake it
  # for the queried website IP when matching these same rules.
  - IP-CIDR,203.0.113.54/32,B,no-resolve
  - IP-CIDR,203.0.113.53/32,A,no-resolve
  - IP-CIDR6,2001:db8::53/128,A,no-resolve
  - NOT,((IP-CIDR,10.0.0.0/8)),A
  # Available DNS connection attributes remain usable in the normal order.
  - AND,((DOMAIN,inbound.example),(IN-NAME,DEFAULT-MIXED)),B
  - AND,((DOMAIN,source.example),(SRC-IP-CIDR,127.0.0.1/32)),B
  - AND,((DOMAIN,transport.example),(DST-PORT,53),(NETWORK,tcp)),B
  - AND,((DOMAIN,process.example),(PROCESS-NAME,{process_name})),B
  - DOMAIN,pass-control.example,PassRoundRobin
  - DOMAIN,pass-control.example,B
  - SUB-RULE,(DOMAIN,sub-control.example),DNSControl
  - DOMAIN,first.example,A
  - DOMAIN,second.example,B
  - DOMAIN,selector.example,Chosen
  - DOMAIN,visible.example,DNSOuter
  - DOMAIN,cancel.example,DNSOuter
  - DOMAIN,blocked.example,REJECT
  - DOMAIN,drop.example,REJECT-DROP
  - MATCH,A
"""

    def tcp_query(port, query, expected, target=resolver):
        conn, reply, _ = socks_request(port, target)
        with conn:
            assert reply == 0
            conn.sendall(frame(query))
            check_answer(read_frame(conn), query, expected)

    try:
        with tempfile.TemporaryDirectory(prefix="dns-rule-routing-e2e-") as tmp:
            config_dir = pathlib.Path(tmp)
            config_file = config_dir / "config.yaml"
            config_file.write_text(config, encoding="utf-8")
            subprocess.run([str(binary), "-t", "-d", str(config_dir), "-f", str(config_file)],
                           check=True, timeout=30, capture_output=True)
            with (config_dir / "mihomo.log").open("w+") as log:
                proc = subprocess.Popen([str(binary), "-d", str(config_dir), "-f", str(config_file)],
                                        stdout=log, stderr=subprocess.STDOUT)
                try:
                    deadline = time.monotonic() + 15
                    while True:
                        if proc.poll() is not None:
                            raise AssertionError("binary exited during startup")
                        try:
                            general = api_request(api_port)
                            # Upstream opens listeners before providers/profile
                            # initialization and OnRunning. A TCP accept or HTTP
                            # 200 can therefore still be followed by an intentional
                            # startup close. Prove ordinary forwarding is ready;
                            # this probe is not DNS and cannot exercise/mask the
                            # first pipelined DNS assertion below.
                            probe, reply, _ = socks_request(mixed_port, (resolver[0], 443))
                            with probe:
                                assert reply == 0, "readiness SOCKS handshake failed"
                                payload = b"mihomo-forwarding-ready"
                                probe.sendall(payload)
                                assert read_exact(probe, len(payload)) == payload
                            break
                        except (EOFError, OSError, urllib.error.URLError):
                            if time.monotonic() >= deadline:
                                raise AssertionError("listener failed to start")
                            time.sleep(0.05)
                    assert general["dns-rule-routing"] is True
                    assert "dns-proxy-port" not in general
                    assert general["mixed-port"] == mixed_port

                    q1, q2 = question("first.example", 101), question("second.example", 102)
                    blocked, dropped = question("blocked.example", 104), question("drop.example", 105)
                    chosen = question("selector.example", 103)
                    stream_queries = [q1, q2, blocked, dropped, chosen, chosen]
                    invalid_queries = malformed_queries(q1)
                    conn, status = http_request(mixed_port, resolver, frame(q1) + frame(q2))
                    with conn:
                        assert status == 200
                        for query in (q1, q2, blocked, dropped, chosen):
                            if query not in (q1, q2):
                                conn.sendall(frame(query))
                            assert read_frame(conn) == make_answer(query, upstream_a.answer)
                        api_request(api_port, "/proxies/Chosen", {"name": "B"}, "PUT")
                        conn.sendall(frame(chosen))
                        assert read_frame(conn) == make_answer(chosen, upstream_a.answer)
                        # Valid DNS followed by malformed/opaque frames remains
                        # one ordinary stream. No per-query parser may close it.
                        conn.sendall(b"".join(frame(wire) for wire in invalid_queries + [q2]))
                        for wire in invalid_queries:
                            assert read_frame(conn) == wire
                        assert read_frame(conn) == make_answer(q2, upstream_a.answer)
                    with record_lock:
                        assert (resolver, stream_queries + invalid_queries + [q2]) in upstream_a.tcp_streams
                    print("PASS HTTP CONNECT: pipelined first frames, exact bytes and one upstream stream; QNAME routes/REJECT/DROP/selector changes cannot intercept port 53")

                    conn, reply, _ = socks_request(mixed_port, resolver_v6)
                    with conn:
                        assert reply == 0
                        for query in (q1, q2, blocked, dropped):
                            conn.sendall(frame(query))
                            assert read_frame(conn) == make_answer(query, upstream_a.answer)
                    for use_4a, target in ((False, resolver), (True, resolver), (True, resolver_v6)):
                        conn, reply = socks4_request(mixed_port, target, use_4a)
                        with conn:
                            assert reply == 90
                            for query in (q1, q2, blocked):
                                conn.sendall(frame(query))
                                assert read_frame(conn) == make_answer(query, upstream_a.answer)
                    tcp_query(mixed_port, q2, upstream_a.answer, ("resolver.bootstrap.test", 53))
                    tcp_query(mixed_port, q1, upstream_b.answer, resolver_b)
                    print("PASS SOCKS4/4a/5 TCP: destination routing, persistent streams, IPv4/IPv6 and configured resolver hosts mapping")

                    for name in ("inbound.example", "source.example", "transport.example", "process.example"):
                        tcp_query(mixed_port, question(name), upstream_a.answer)
                    tcp_query(fixed_port, q1, upstream_b.answer)
                    tcp_query(scoped_port, q1, upstream_b.answer)
                    print("PASS normal metadata rules: DNS payload names cannot replace the destination; fixed outbound and inbound sub-rule remain effective")

                    conn, reply, relay = socks_request(mixed_port, ("0.0.0.0", 53), command=3)
                    with conn, socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                        assert reply == 0
                        udp.bind(("127.0.0.1", 0))
                        udp.settimeout(3)
                        for query, answer, target in ((q1, upstream_a.answer, resolver),
                                                       (q2, upstream_a.answer, resolver),
                                                       (blocked, upstream_a.answer, resolver),
                                                       (dropped, upstream_a.answer, resolver),
                                                       (q2, upstream_a.answer, resolver_v6),
                                                       (q1, upstream_a.answer, resolver_b)):
                            udp.sendto(b"\x00\x00\x00" + encode_address(*target) + query, relay)
                            target_back, response = decode_packet(udp.recvfrom(65535)[0])
                            assert target_back == target, (target_back, target)
                            assert response == make_answer(query, answer), (question_name(query), target, response.hex())
                        for wire in invalid_queries:
                            udp.sendto(b"\x00\x00\x00" + encode_address(*resolver) + wire, relay)
                            target_back, response = decode_packet(udp.recvfrom(65535)[0])
                            assert target_back == resolver and response == wire
                        # Ordinary SOCKS UDP NAT chooses an outbound on the
                        # first packet from a source socket and keeps it for
                        # that session, even as packet destinations change.
                        # A fresh source must independently select B by IP.
                        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as other:
                            other.bind(("127.0.0.1", 0))
                            other.settimeout(3)
                            other.sendto(b"\x00\x00\x00" + encode_address(*resolver_b) + q1, relay)
                            target_back, response = decode_packet(other.recvfrom(65535)[0])
                            assert target_back == resolver_b and response == make_answer(q1, upstream_b.answer)
                    with record_lock:
                        for wire in (q1, q2, blocked, dropped, *invalid_queries):
                            assert (resolver, "udp", wire) in upstream_a.wire_records
                        assert (resolver_b, "udp", q1) in upstream_a.wire_records
                        assert (resolver_b, "udp", q1) in upstream_b.wire_records
                    print("PASS SOCKS5 UDP: first destination selects the session outbound; changed destinations retain it, fresh sources reroute, QNAME REJECT/DROP ignored and bytes unchanged")

                    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
                        udp.bind(("127.0.0.1", 0))
                        udp.settimeout(3)
                        for query, answer, target in ((q2, upstream_a.answer, resolver),
                                                       (blocked, upstream_a.answer, resolver_v6),
                                                       (q1, upstream_a.answer, resolver_b)):
                            udp.sendto(b"\x00\x00\x00" + encode_address(*target) + query, ("127.0.0.1", mixed_port))
                            packet, source = udp.recvfrom(65535)
                            target_back, response = decode_packet(packet)
                            assert source[1] == mixed_port and target_back == target
                            assert response == make_answer(query, answer)
                        raw = b"ordinary-udp-payload"
                        target = (resolver[0], 443)
                        udp.sendto(b"\x00\x00\x00" + encode_address(*target) + raw, ("127.0.0.1", mixed_port))
                        target_back, response = decode_packet(udp.recvfrom(65535)[0])
                        assert target_back == target and response == raw
                        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as other:
                            other.bind(("127.0.0.1", 0))
                            other.settimeout(3)
                            other.sendto(b"\x00\x00\x00" + encode_address(*resolver_b) + q1, ("127.0.0.1", mixed_port))
                            target_back, response = decode_packet(other.recvfrom(65535)[0])
                            assert target_back == resolver_b and response == make_answer(q1, upstream_b.answer)
                    print("PASS fixed mixed UDP: ordinary NAT session routing, independent new sources and unchanged non-53 forwarding")

                    raw = b"ordinary-tcp-payload"
                    for protocol in ("http", "socks5", "socks4", "socks4a"):
                        for target in ((resolver[0], 443), RAW_TCP_TARGET):
                            if protocol == "http":
                                conn, reply = http_request(mixed_port, target)
                                assert reply == 200
                            elif protocol == "socks5":
                                conn, reply, _ = socks_request(mixed_port, target)
                                assert reply == 0
                            else:
                                conn, reply = socks4_request(mixed_port, target, protocol == "socks4a")
                                assert reply == 90
                            with conn:
                                if target == RAW_TCP_TARGET:
                                    # The server speaks before any client bytes.
                                    # A DNS classifier would wait for a first frame.
                                    assert read_exact(conn, len(SERVER_GREETING)) == SERVER_GREETING
                                conn.sendall(raw)
                                assert read_exact(conn, len(raw)) == raw
                    print("PASS all mixed TCP protocols: unchanged non-53 access and server-first arbitrary byte streams on port 53")

                    check_reply_compatibility(mixed_port, resolver, upstream_a.answer)
                    api_request(api_port, "/connections", method="DELETE")
                    check_dashboard_connections(mixed_port, api_port, resolver, upstream_a)

                    # An ordinary TCP tunnel retains its chosen outbound when
                    # the mode changes; only new connections use the new mode.
                    held, reply, _ = socks_request(mixed_port, resolver)
                    assert reply == 0
                    held.sendall(frame(q2))
                    check_answer(read_frame(held), q2, upstream_a.answer)
                    api_request(api_port, "/proxies/GLOBAL", {"name": "B"}, "PUT")
                    api_request(api_port, body={"mode": "global"}, method="PATCH")
                    with held:
                        held.sendall(frame(q2))
                        check_answer(read_frame(held), q2, upstream_a.answer)
                    tcp_query(mixed_port, q2, upstream_b.answer)
                    tcp_query(fixed_port, q1, upstream_b.answer)
                    api_request(api_port, body={"mode": "rule"}, method="PATCH")
                    tcp_query(mixed_port, q2, upstream_a.answer)
                    print("PASS ordinary mode transition: existing TCP stream retained; new Global/Rule connections and fixed outbounds follow normal routing")

                    # A scalar PATCH cannot leave resolver policy half changed.
                    try:
                        api_request(api_port, body={"dns-rule-routing": False, "mode": "global"}, method="PATCH")
                        raise AssertionError("partial DNS switch PATCH was accepted")
                    except urllib.error.HTTPError as error:
                        assert error.code == 400
                    assert api_request(api_port)["dns-rule-routing"] is True
                    assert api_request(api_port)["mode"] == "rule"

                    disabled_config = config.replace("dns-rule-routing: true\n", "")
                    api_request(api_port, "/configs", {"payload": disabled_config}, "PUT")
                    assert api_request(api_port)["dns-rule-routing"] is False
                    tcp_query(mixed_port, q2, upstream_a.answer)
                    api_request(api_port, "/configs", {"payload": config}, "PUT")
                    assert api_request(api_port)["dns-rule-routing"] is True
                    tcp_query(mixed_port, q2, upstream_a.answer)
                    print("PASS native DNS switch reloads leave ordinary forwarding unchanged; unsafe scalar PATCH rejected atomically")
                    check_builtin_dns(config, api_port, upstream_a, upstream_b)
                    native_direct_count = check_native_dns_pools(config, api_port, upstream_a, upstream_b)
                    mapping_exchange_count = check_redir_host_filter(api_port, mixed_port, upstream_a, upstream_b)
                    with record_lock:
                        assert all(target[1] == 53 and target[0] in (resolver[0], resolver_v6[0], resolver_b[0])
                                   for _, _, target, _ in records)
                        exchange_count = len(records) + native_direct_count + mapping_exchange_count
                    print(f"PASS actual binary end-to-end: {exchange_count} DNS exchanges plus ordinary TCP/UDP forwarding")
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
