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
        super().__init__(("127.0.0.1", 0), MockHandler)

    def respond(self, wire, target, network):
        assert target[1] == 53, "non-DNS destination reached outbound"
        with self.record_lock:
            self.records.append((self.answer, question_name(wire), target, network))
        return make_answer(wire, self.answer)


class MockHandler(socketserver.BaseRequestHandler):
    def handle(self):
        conn = self.request
        conn.settimeout(8)
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


def run(binary):
    records, record_lock = [], threading.Lock()
    upstream_a = MockSOCKS("198.51.100.11", records, record_lock)
    upstream_b = MockSOCKS("198.51.100.22", records, record_lock)
    for upstream in (upstream_a, upstream_b):
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
    dns_port, api_port, disabled_port = unused_port(), unused_port(), unused_port()
    resolver = ("203.0.113.53", 53)
    resolver_v6 = ("2001:db8::53", 53)
    config = f"""mode: rule
log-level: debug
external-controller: 127.0.0.1:{api_port}
dns:
  enable: false
listeners:
  - name: dns-e2e
    type: dns-proxy
    port: {dns_port}
  - name: disabled-e2e
    type: dns-proxy
    listen: 127.0.0.1
    port: {disabled_port}
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
rules:
  - DOMAIN,first.example,A
  - DOMAIN,second.example,B
  - DOMAIN,selector.example,Chosen
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

                    try:
                        with connect(disabled_port):
                            raise AssertionError("disabled listener opened a port")
                    except ConnectionRefusedError:
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
                        req = urllib.request.Request(f"http://127.0.0.1:{api_port}/proxies/Chosen", data=json.dumps({"name": "B"}).encode(), headers={"Content-Type": "application/json"}, method="PUT")
                        with urllib.request.urlopen(req, timeout=3) as response:
                            assert response.status == 204
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
                        blocked = question("blocked.example")
                        udp.settimeout(3)
                        udp.sendto(b"\x00\x00\x00" + encode_address(*resolver) + blocked, relay)
                        _, response = decode_packet(udp.recvfrom(65535)[0])
                        assert response[3] & 15 == 5, "REJECT did not return DNS REFUSED"
                    print("PASS SOCKS5 UDP: SmartDNS ASSOCIATE, per-datagram routes, IPv4/IPv6 targets, drop/REFUSED, malformed and fragmented packets")

                    with record_lock:
                        before = len(records)
                    conn, status = http_request(dns_port, (resolver[0], 443))
                    with conn:
                        assert status == 403
                    conn, reply, _ = socks_request(dns_port, (resolver[0], 443))
                    with conn:
                        assert reply == 2, "SOCKS5 non-53 CONNECT accepted"
                    with connect(dns_port) as conn:
                        conn.sendall(b"GET http://example.invalid/ HTTP/1.1\r\nHost: example.invalid\r\n\r\n")
                        assert b" 405 " in conn.recv(1024)
                    with record_lock:
                        assert len(records) == before, "forbidden request reached an outbound"
                        assert all(target in (resolver, resolver_v6) for _, _, target, _ in records)
                    print("PASS non-53 SOCKS5/HTTP and ordinary HTTP rejected before any outbound dial")
                    print(f"PASS actual binary end-to-end: {len(records)} independently routed DNS exchanges; disabled listener stayed closed")
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
