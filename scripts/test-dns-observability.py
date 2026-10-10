#!/usr/bin/env python3
"""Exercise the released core's memory-only DNS API with local fake services.

No production controller, private configuration, or public DNS is used. The
test verifies the observable contract, not the implementation's data layout.
"""
import argparse
import hashlib
import json
from pathlib import Path
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
import time
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, build_opener, ProxyHandler


SECRET = "local-observability-integration-test"
ADDRESS = "192.0.2.17"


def question(name, qtype=1):
    ident = time.monotonic_ns() & 65535
    labels = b"".join(bytes([len(label)]) + label.encode() for label in name.split(".")) + b"\0"
    return struct.pack("!HHHHHH", ident, 0x100, 1, 0, 0, 0) + labels + struct.pack("!HH", qtype, 1)


def decode_question(wire):
    offset, labels = 12, []
    while wire[offset]:
        length = wire[offset]
        offset += 1
        labels.append(wire[offset:offset + length].decode())
        offset += length
    return ".".join(labels), offset + 5


def response(wire):
    name, end = decode_question(wire)
    ident = struct.unpack("!H", wire[:2])[0]
    if name.startswith("missing."):
        return struct.pack("!HHHHHH", ident, 0x8183, 1, 0, 0, 0) + wire[12:end]
    qtype = struct.unpack("!H", wire[end - 4:end - 2])[0]
    if qtype == 16:
        # Long answer summaries must be truncated without changing the answer.
        value = bytes([240]) + b"x" * 240 + bytes([240]) + b"y" * 240
    else:
        value = socket.inet_aton(ADDRESS)
    rr = b"\xc0\x0c" + struct.pack("!HHIH", qtype, 1, 120, len(value)) + value
    return struct.pack("!HHHHHH", ident, 0x8180, 1, 1, 0, 0) + wire[12:end] + rr


def exact(sock, size):
    value = bytearray()
    while len(value) < size:
        block = sock.recv(size - len(value))
        if not block:
            raise EOFError("short DNS TCP response")
        value.extend(block)
    return bytes(value)


def unused_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Upstream(socketserver.ThreadingUDPServer):
    daemon_threads = True

    def __init__(self):
        self.lock = threading.Lock()
        self.queries = 0
        super().__init__(("127.0.0.1", 0), UpstreamHandler)

    def count(self):
        with self.lock:
            return self.queries


class UpstreamHandler(socketserver.BaseRequestHandler):
    def handle(self):
        wire, sock = self.request
        with self.server.lock:
            self.server.queries += 1
        sock.sendto(response(wire), self.client_address)


class Harness:
    def __init__(self, binary, directory, upstream):
        self.binary = str(Path(binary).resolve())
        self.directory = Path(directory)
        self.dns_port, self.api_port = unused_port(), unused_port()
        self.upstream_port = upstream.server_address[1]
        self.process = None
        self.log = None
        self.http = build_opener(ProxyHandler({}))

    def config(self, enabled=True):
        return f"""mode: rule
log-level: silent
find-process-mode: off
external-controller: 127.0.0.1:{self.api_port}
secret: {SECRET}
dns-rule-routing: true
profile:
  store-selected: false
  store-fake-ip: false
hosts:
  local.observation.test: 192.0.2.22
  alias.observation.test: local.observation.test
rules:
  - DOMAIN-SUFFIX,blocked.observation.test,REJECT
  - DOMAIN-SUFFIX,dropped.observation.test,REJECT-DROP
  - MATCH,DIRECT
dns:
  enable: true
  observability: {str(enabled).lower()}
  listen: 127.0.0.1:{self.dns_port}
  enhanced-mode: normal
  ipv6: false
  use-hosts: true
  use-system-hosts: false
  default-nameserver: [127.0.0.1:{self.upstream_port}]
  direct-nameserver: [127.0.0.1:{self.upstream_port}]
  nameserver: [127.0.0.1:{self.upstream_port}]
  prefetch-domain: false
  serve-expired: false
"""

    def api(self, path, method="GET", body=None, authenticated=True, status=200):
        headers = {"Content-Type": "application/json"}
        if authenticated:
            headers["Authorization"] = "Bearer " + SECRET
        data = None if body is None else json.dumps(body).encode()
        request = Request(f"http://127.0.0.1:{self.api_port}{path}", data, headers, method=method)
        try:
            result = self.http.open(request, timeout=10)
        except HTTPError as error:
            result = error
        with result:
            raw = result.read()
            assert result.status == status, (path, result.status, raw[:300])
            if path.startswith("/dns/observability") and status == 200:
                assert "no-store" in result.headers.get("Cache-Control", ""), result.headers
            return json.loads(raw) if raw else None

    def start(self):
        (self.directory / "config.yaml").write_text(self.config())
        self.log = (self.directory / "process.log").open("w+")
        self.process = subprocess.Popen(
            [self.binary, "-d", str(self.directory), "-f", str(self.directory / "config.yaml")],
            stdout=self.log, stderr=subprocess.STDOUT,
        )
        for _ in range(100):
            if self.process.poll() is not None:
                self.log.seek(0)
                raise RuntimeError(self.log.read())
            try:
                return self.api("/dns/observability")
            except (OSError, TimeoutError):
                time.sleep(0.05)
        raise RuntimeError("controller did not become ready")

    def stop(self):
        if self.process is not None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
            self.process = None
        if self.log is not None:
            self.log.close()
            self.log = None

    def query(self, name, address=ADDRESS, rcode=0, source="127.0.0.2", tcp=False, drop=False, qtype=1):
        wire = question(name, qtype)
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM if tcp else socket.SOCK_DGRAM) as sock:
            sock.bind((source, 0))
            sock.settimeout(0.2 if drop else 5)
            if tcp:
                sock.connect(("127.0.0.1", self.dns_port))
                sock.sendall(struct.pack("!H", len(wire)) + wire)
                result = exact(sock, struct.unpack("!H", exact(sock, 2))[0])
            else:
                sock.sendto(wire, ("127.0.0.1", self.dns_port))
                try:
                    result = sock.recvfrom(65535)[0]
                except TimeoutError:
                    if drop:
                        return
                    raise
        assert not drop, "REJECT-DROP unexpectedly sent an answer"
        assert result[:2] == wire[:2], "transaction ID changed"
        assert struct.unpack("!H", result[2:4])[0] & 15 == rcode, (name, result)
        if rcode == 0 and qtype == 1:
            assert result[-4:] == socket.inet_aton(address), (name, result)
        if qtype == 16:
            assert b"x" * 240 in result and b"y" * 240 in result, "observation changed the TXT answer"

    def page(self, **filters):
        return self.api("/dns/observability/queries?" + urlencode(filters))

    def last(self, name):
        result = self.page(qname=name, limit=50)
        assert result["items"], (name, result)
        return result["items"][0]

    def reload(self, enabled=True):
        self.api("/configs?force=true", method="PUT", body={"payload": self.config(enabled)}, status=204)

    def files(self):
        return {
            str(path.relative_to(self.directory)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in self.directory.rglob("*")
            if path.is_file() and path.name != "process.log"
        }


def run(binary, fill):
    upstream = Upstream()
    thread = threading.Thread(target=upstream.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="mihomo-dns-observability-") as directory:
            harness = Harness(binary, directory, upstream)
            try:
                status = harness.start()
                assert status["version"] == 1 and status["enabled"] and status["storage"] == "memory", status
                assert status["retained"] == 0
                initial_instance = status["instance_id"]
                baseline_files = harness.files()
                assert harness.api("/dns/observability/")["instance_id"] == initial_instance
                for endpoint in ("", "/queries", "/stats", "/upstreams"):
                    harness.api("/dns/observability" + endpoint, authenticated=False, status=401)

                harness.query("cold.observation.test")
                record = harness.last("cold.observation.test")
                assert record["outcome"] == "upstream" and record["rcode"] == "NOERROR", record
                assert record["client"] == "127.0.0.2" and record["protocol"].lower() == "udp", record
                before = upstream.count()
                upstream_before = harness.api("/dns/observability/upstreams")
                harness.query("cold.observation.test")
                cached = harness.last("cold.observation.test")
                assert cached["outcome"] == "cache_fresh" and cached["cache"] == "fresh", cached
                assert upstream.count() == before, "cache hit sent an upstream query"
                upstream_after = harness.api("/dns/observability/upstreams")
                assert upstream_before == upstream_after, "cache hit counted as an upstream attempt"

                harness.query("local.observation.test", address="192.0.2.22")
                assert harness.last("local.observation.test")["outcome"] == "hosts"
                harness.query("alias.observation.test", address="192.0.2.22")
                assert harness.last("alias.observation.test")["qname"] == "alias.observation.test"
                harness.query("missing.observation.test", rcode=3)
                missing = harness.last("missing.observation.test")
                assert missing["rcode"] == "NXDOMAIN" and missing["outcome"] != "error", missing
                harness.query("blocked.observation.test", rcode=5)
                assert harness.last("blocked.observation.test")["outcome"] == "reject"
                harness.query("dropped.observation.test", drop=True)
                assert harness.last("dropped.observation.test")["outcome"] == "drop"
                harness.query("tcp.observation.test", tcp=True, source="127.0.0.3")
                assert harness.last("tcp.observation.test")["protocol"].lower() == "tcp"
                harness.query("long.observation.test", qtype=16, tcp=True)
                long_answer = harness.last("long.observation.test")
                assert long_answer["answers_truncated"] and len(long_answer["answers"][0]["value"]) <= 256

                records = harness.page(limit=100)
                assert len(records["items"]) == 9, records
                assert harness.api("/dns/observability/stats")["totals"]["queries"] == 9
                filtered = harness.page(client="127.0.0.3", limit=50)["items"]
                assert len(filtered) == 1 and filtered[0]["qname"] == "tcp.observation.test", filtered
                assert len(harness.page(outcome="cache_fresh", qtype="A", limit=50)["items"]) == 1
                assert harness.page(qname="COLD.OBSERVATION.TEST.", limit=50)["items"] == [
                    record for record in records["items"] if record["qname"] == "cold.observation.test"
                ]

                # Exclusive IDs must not duplicate a page when new queries arrive.
                first = harness.page(limit=3)
                assert first["has_more"]
                harness.query("between-pages.observation.test")
                second = harness.page(limit=3, cursor=first["next_cursor"])
                assert not ({item["id"] for item in first["items"]} & {item["id"] for item in second["items"]})
                assert all(int(item["id"]) < int(first["next_cursor"]) for item in second["items"])

                before_reload = harness.api("/dns/observability/stats")
                harness.reload()
                assert harness.api("/dns/observability")["instance_id"] == initial_instance
                assert harness.api("/dns/observability/stats")["totals"] == before_reload["totals"]
                assert harness.last("between-pages.observation.test")["qname"] == "between-pages.observation.test"

                # The record budget is separate from totals and retained Top N.
                count = status["capacity"] + 32 if fill else 64
                start = time.perf_counter()
                for index in range(count):
                    harness.query(f"record-{index}.observation.test")
                elapsed = time.perf_counter() - start
                status = harness.api("/dns/observability")
                stats = harness.api("/dns/observability/stats")
                assert stats["totals"]["queries"] == count + 10, stats["totals"]
                assert status["retained"] <= status["capacity"]
                assert status["accounted_bytes"] <= status["memory_limit_bytes"]
                assert stats["top_scope"] == "retained" and len(stats["series"]) == 1440
                if fill:
                    assert status["evicted"] > 0
                    assert not harness.page(qname="cold.observation.test", limit=50)["items"]
                assert harness.files() == baseline_files, "DNS observations changed runtime files"

                # Disable must free state and late work must not retain it. The
                # Go race tests additionally cover disable during in-flight work.
                harness.reload(False)
                disabled = harness.api("/dns/observability")
                assert not disabled["enabled"] and disabled["retained"] == 0
                assert disabled["accounted_bytes"] == 0
                harness.query("while-disabled.observation.test")
                assert harness.api("/dns/observability/stats")["totals"]["queries"] == 0
                assert not harness.api("/dns/observability/upstreams")["items"]
                harness.reload(True)
                enabled = harness.api("/dns/observability")
                assert enabled["instance_id"] != initial_instance and enabled["retained"] == 0
                harness.query("after-enable.observation.test")
                assert harness.api("/dns/observability/stats")["totals"]["queries"] == 1
                previous_instance = enabled["instance_id"]
                harness.stop()
                restarted = harness.start()
                assert restarted["instance_id"] != previous_instance and restarted["retained"] == 0
                assert harness.api("/dns/observability/stats")["totals"]["queries"] == 0
                print("PASS memory-only DNS API: auth, cache/upstream identity, hosts alias, NXDOMAIN, REJECT/DROP, UDP/TCP, answer bounds, cursor/filter, reload, eviction, disable/re-enable, restart and unchanged runtime files")
                print(json.dumps({"controlled_queries": count, "seconds": round(elapsed, 3),
                                  "retained": status["retained"], "evicted": status["evicted"],
                                  "accounted_bytes": status["accounted_bytes"]}, sort_keys=True))
            finally:
                harness.stop()
    finally:
        upstream.shutdown()
        upstream.server_close()
        thread.join(timeout=5)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary")
    parser.add_argument("--quick", action="store_true", help="skip full record-capacity fill for local iteration")
    args = parser.parse_args()
    run(args.binary, not args.quick)
