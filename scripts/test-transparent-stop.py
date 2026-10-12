#!/usr/bin/env python3
"""Verify real mihomo stop/reexec cleanup in disposable, retained netns fixtures.

Run as root on a Linux CI runner with iproute2, nftables and util-linux:
  python3 scripts/test-transparent-stop.py --core /absolute/path/to/mihomo --require-systemd

The keeper stays outside the service cgroup. The namespace therefore survives
SIGTERM and systemctl stop, so deleting the namespace cannot hide leaked rules.
No network mutation is performed in the runner's original namespace. Required
privileges and, with --require-systemd, a working systemd manager are hard gates.
"""

from __future__ import annotations

import argparse
import configparser
import json
import os
from pathlib import Path
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = Path(__file__).resolve()
OWN_TABLE = "mihomo_optimized_tp"
CONTROLLER = "http://127.0.0.1:19091"
SECRET = "isolated-stop-test"
INTERFACES = ("lo", "wan0", "wan-peer", "lan0", "lan-peer")
STOP_TIMEOUT = 35


class Failure(RuntimeError):
    pass


def run(argv: list[str], *, timeout: float = 10, input: str | None = None) -> str:
    result = subprocess.run(
        argv, input=input, text=True, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, timeout=timeout, check=False,
    )
    if result.returncode:
        raise Failure(
            f"command failed ({result.returncode}): {argv!r}\n"
            f"{result.stdout}{result.stderr}"
        )
    return result.stdout


def keeper() -> None:
    print(os.readlink("/proc/self/ns/net"), flush=True)
    sys.stdin.buffer.read()


class Namespace:
    def __init__(self) -> None:
        self.process = subprocess.Popen(
            ["unshare", "--net", sys.executable, str(SCRIPT), "--keeper"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True,
        )
        self.path = f"/proc/{self.process.pid}/ns/net"
        try:
            readable, _, _ = select.select([self.process.stdout], [], [], 10)
            if not readable:
                raise Failure("network namespace keeper did not become ready")
            self.identity = self.process.stdout.readline().strip()
            if not self.identity:
                raise Failure(f"cannot create isolated netns: {self.process.stderr.read()}")
            if self.identity == os.readlink("/proc/self/ns/net"):
                raise Failure("refusing to use the runner's original network namespace")
            self.verify()
        except BaseException:
            self.close()
            raise

    def verify(self) -> None:
        if self.process.poll() is not None or os.readlink(self.path) != self.identity:
            raise Failure("network namespace keeper exited or changed identity")

    def command(self, argv: list[str]) -> list[str]:
        self.verify()
        return ["nsenter", f"--net={self.path}", "--", *argv]

    def run(self, argv: list[str], **kwargs) -> str:
        return run(self.command(argv), **kwargs)

    def json(self, argv: list[str]):
        return json.loads(self.run(argv))

    def close(self) -> None:
        if self.process.stdin:
            self.process.stdin.close()
        try:
            self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=3)
        for stream in (self.process.stdout, self.process.stderr):
            if stream:
                stream.close()


SYSCTL_HELPER = """
import json, pathlib, sys
values = json.load(sys.stdin)
result = {}
for name, value in values.items():
    path = pathlib.Path('/proc/sys') / name
    if value is not None:
        path.write_text(value + '\\n')
    try:
        result[name] = path.read_text().strip()
    except FileNotFoundError:
        if name.endswith('/force_forwarding'):
            continue
        raise
print(json.dumps(result, sort_keys=True))
"""


def sysctls(ns: Namespace, values: dict[str, str | None]) -> dict[str, str]:
    return json.loads(ns.run(
        [sys.executable, "-c", SYSCTL_HELPER], input=json.dumps(values),
    ))


def monitored_sysctls() -> dict[str, None]:
    names = {"net/ipv4/ip_forward": None}
    for interface in ("all", "default", *INTERFACES):
        for setting in ("forwarding", "rp_filter", "accept_redirects"):
            names[f"net/ipv4/conf/{interface}/{setting}"] = None
        for setting in ("forwarding", "accept_ra", "force_forwarding"):
            names[f"net/ipv6/conf/{interface}/{setting}"] = None
    return names


def configure_namespace(ns: Namespace) -> None:
    sysctls(ns, {
        "net/ipv6/conf/all/disable_ipv6": "0",
        "net/ipv6/conf/default/disable_ipv6": "0",
        "net/ipv6/conf/default/accept_dad": "0",
        "net/ipv6/conf/lo/disable_ipv6": "0",
    })
    ns.run(["ip", "link", "set", "lo", "up"])
    for first, peer in (("wan0", "wan-peer"), ("lan0", "lan-peer")):
        ns.run(["ip", "link", "add", first, "type", "veth", "peer", "name", peer])
        ns.run(["ip", "link", "set", first, "up"])
        ns.run(["ip", "link", "set", peer, "up"])
    for interface, v4, v6 in (
        ("wan0", "198.19.0.1/24", "2001:db8:1::1/64"),
        ("lan0", "192.0.2.1/24", "2001:db8:2::1/64"),
    ):
        ns.run(["ip", "-4", "address", "add", v4, "dev", interface])
        ns.run(["ip", "-6", "address", "add", v6, "dev", interface, "nodad"])
    ns.run(["ip", "-4", "route", "add", "default", "via", "198.19.0.2", "dev", "wan0"])
    ns.run(["ip", "-6", "route", "add", "default", "via", "2001:db8:1::2", "dev", "wan0"])

    # Choose host-mode originals so a passing restoration check cannot merely
    # mean that all requested settings already had the managed values.
    sysctls(ns, {
        "net/ipv4/ip_forward": "0",
        "net/ipv6/conf/all/forwarding": "0",
        "net/ipv6/conf/default/forwarding": "0",
    })
    originals = {}
    for interface in ("all", "default", *INTERFACES):
        originals[f"net/ipv4/conf/{interface}/rp_filter"] = "1"
        originals[f"net/ipv6/conf/{interface}/accept_ra"] = "1"
    for interface in INTERFACES:
        originals[f"net/ipv4/conf/{interface}/forwarding"] = "0"
        originals[f"net/ipv6/conf/{interface}/forwarding"] = "0"
    sysctls(ns, originals)

    # Unrelated resources must survive normal stop and controlled reexec.
    for family in ("-4", "-6"):
        ns.run(["ip", family, "rule", "add", "pref", "12000", "fwmark", "0xf00f", "lookup", "120", "protocol", "static"])
    ns.run(["ip", "-4", "route", "add", "blackhole", "203.0.113.0/24", "table", "120", "proto", "static"])
    ns.run(["ip", "-6", "route", "add", "unreachable", "2001:db8:feed::/64", "table", "120", "proto", "static"])
    ns.run(["nft", "-f", "-"], input="""
table inet mihomo_stop_sentinel {
  chain unrelated {
    meta mark 0xf00f counter return
  }
}
""")


def sorted_objects(objects: list) -> list:
    return sorted(objects, key=lambda item: json.dumps(item, sort_keys=True))


def network_snapshot(ns: Namespace) -> dict:
    snapshot = {}
    for family in ("4", "6"):
        snapshot[f"rules{family}"] = sorted_objects(ns.json(["ip", "-j", f"-{family}", "rule", "show"]))
        snapshot[f"routes{family}"] = sorted_objects(ns.json(["ip", "-j", f"-{family}", "route", "show", "table", "all"]))
    ruleset = ns.json(["nft", "-j", "list", "ruleset"])["nftables"]
    snapshot["nftables"] = sorted_objects([obj for obj in ruleset if "metainfo" not in obj])
    snapshot["sysctls"] = sysctls(ns, monitored_sysctls())
    snapshot["interfaces"] = sorted(link["ifname"] for link in ns.json(["ip", "-j", "link", "show"]))
    return snapshot


HTTP_HELPER = """
import json, sys, urllib.request
method, url, secret = sys.argv[1:]
request = urllib.request.Request(url, method=method, headers={'Authorization': 'Bearer ' + secret})
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
with opener.open(request, timeout=3) as response:
    data = response.read()
    print(data.decode() if data else '{}')
"""


def request(ns: Namespace, path: str, method: str = "GET") -> dict:
    return json.loads(ns.run(
        [sys.executable, "-c", HTTP_HELPER, method, CONTROLLER + path, SECRET], timeout=6,
    ))


def write_config(directory: Path) -> None:
    (directory / "ui").mkdir()
    (directory / "ui" / "index.html").write_text("<!doctype html><title>isolated stop test</title>\n")
    (directory / "config.yaml").write_text(f"""
mode: rule
log-level: debug
find-process-mode: off
ipv6: true
external-controller: 127.0.0.1:19091
secret: {SECRET}
core-auto-update: false
external-ui-auto-update: false
geo-auto-update: false
profile:
  store-selected: false
  store-fake-ip: false
ntp:
  enable: false
tproxy-port: 7895
tun:
  enable: true
  device: utun
  stack: system
  auto-route: false
  auto-redirect: false
  dns-hijack: []
  inet6-address: [fdfe:dcba:9876::1/126]
transparent-proxy:
  enable: true
  tcp: tproxy
  udp: tun
  local:
    enable: true
    interfaces: [wan0]
  gateway:
    enable: true
    interfaces: [lan0]
  bypass:
    interface-prefixes: [wan0]
dns:
  enable: false
rules:
  - MATCH,DIRECT
""")


def service_properties() -> list[str]:
    unit = configparser.ConfigParser(interpolation=None)
    unit.optionxform = str
    unit.read(ROOT / "packaging" / "mihomo.service")
    section = unit["Service"]
    if section.get("Type") != "simple" or "ExecStop" in section:
        raise Failure("packaged service lifecycle changed; adapt the stop fixture to its actual unit")
    # Use the shipped capabilities, restart policy and kill behavior. Only its
    # command, log destination and isolated namespace are fixture-specific.
    return [f"--property={name}={value}" for name, value in section.items()
            if name not in {"ExecStart", "ExecReload"}]


class Core:
    def __init__(self, ns: Namespace, binary: Path, directory: Path, systemd: bool) -> None:
        self.ns, self.binary, self.directory = ns, binary, directory
        self.log = directory / "core.log"
        self.process = None
        self.output = None
        self.waiter = None
        self.wait_output = None
        self.wait_log = directory / "systemd-run.log"
        self.unit = None
        argv = [str(binary), "-d", str(directory), "-f", str(directory / "config.yaml")]
        if systemd:
            self.unit = f"mihomo-stop-test-{os.getpid()}-{uuid.uuid4().hex[:10]}.service"
            try:
                self.wait_output = self.wait_log.open("wb")
                environment = dict(os.environ, LC_ALL="C", SYSTEMD_COLORS="0", SYSTEMD_LOG_TARGET="console", SYSTEMD_LOG_LEVEL="info")
                self.waiter = subprocess.Popen([
                    "systemd-run", "--wait", f"--unit={self.unit}",
                    *service_properties(), f"--property=NetworkNamespacePath={ns.path}",
                    "--property=TimeoutStopSec=30s", f"--property=StandardOutput=append:{self.log}",
                    "--property=StandardError=inherit", "--", *argv,
                ], stdout=self.wait_output, stderr=subprocess.STDOUT, env=environment)
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    if self.waiter.poll() is not None:
                        raise Failure(f"systemd-run exited before core startup:\n{self.diagnostics()}")
                    try:
                        if int(self.properties().get("MainPID", "0")) > 0:
                            break
                    except Failure:
                        pass  # StartTransientUnit has not published the unit yet.
                    time.sleep(0.1)
                else:
                    raise Failure("systemd did not start the transient core service")
            except BaseException:
                self.close()
                raise
        else:
            environment = {key: value for key, value in os.environ.items()
                           if not key.startswith("CLASH_")}
            self.output = self.log.open("wb")
            self.process = subprocess.Popen(
                ns.command(argv), stdout=self.output, stderr=subprocess.STDOUT, env=environment,
            )

    def properties(self) -> dict[str, str]:
        text = run(["systemctl", "show", self.unit, "--property=MainPID,ActiveState,SubState,Result,ExecMainStatus"])
        return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)

    def pid(self) -> int:
        if self.process is not None:
            if self.process.poll() is not None:
                raise Failure(f"core exited before stop (status {self.process.returncode})")
            return self.process.pid
        properties = self.properties()
        pid = int(properties["MainPID"])
        if pid == 0 or properties["ActiveState"] not in {"active", "activating"}:
            raise Failure(f"systemd core is not running: {properties}")
        return pid

    def verify_identity(self) -> None:
        pid = self.pid()
        if os.readlink(f"/proc/{pid}/ns/net") != self.ns.identity:
            raise Failure("core is outside the retained test network namespace")
        if Path(f"/proc/{pid}/exe").resolve() != self.binary:
            raise Failure("signal target is not the compiled core executable")

    def stop(self) -> None:
        self.verify_identity()
        if self.process is not None:
            self.process.send_signal(signal.SIGTERM)
            status = self.process.wait(timeout=STOP_TIMEOUT)
            if status != 0:
                raise Failure(f"SIGTERM did not complete graceful cleanup (exit {status})")
        else:
            run(["systemctl", "stop", self.unit], timeout=STOP_TIMEOUT)
            status = self.waiter.wait(timeout=10)
            receipt = self.wait_log.read_text(errors="replace")
            # A stopped transient unit may already be garbage-collected when
            # systemctl show runs. --wait retains its own completion result.
            # Its exit 0 alone also accepts SIGTERM as a "clean" signal, so
            # require the reported main process to have actually exited 0.
            expected = "Main processes terminated with: code=exited/status=0"
            if status != 0 or expected not in receipt.splitlines():
                raise Failure(f"systemctl stop did not complete a normal main-process exit:\n{receipt}")

    def diagnostics(self) -> str:
        texts = [path.read_text(errors="replace") for path in (self.log, self.wait_log) if path.exists()]
        return "\n".join(texts)[-16000:] or "no core log was created"

    def close(self) -> None:
        # Forced termination is failure teardown only, never a passing stop.
        if self.process is not None and self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=5)
        if self.output:
            self.output.close()
        if self.unit:
            try:
                run(["systemctl", "stop", self.unit], timeout=STOP_TIMEOUT)
            except (Failure, subprocess.TimeoutExpired):
                subprocess.run(["systemctl", "kill", "--signal=SIGKILL", self.unit],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
            subprocess.run(["systemctl", "reset-failed", self.unit],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
        if self.waiter is not None and self.waiter.poll() is None:
            try:
                self.waiter.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.waiter.kill()
                self.waiter.wait(timeout=5)
        if self.wait_output:
            self.wait_output.close()


def assert_no_record(directory: Path) -> None:
    if (directory / "transparent-proxy").exists() or list(directory.rglob("state.json")):
        raise Failure("transparent proxy created a persistent ownership record")


def readiness(ns: Namespace, baseline: dict) -> dict:
    config = request(ns, "/configs")
    if not config.get("transparent-proxy", {}).get("enable") or not config.get("tun", {}).get("enable"):
        raise Failure("controller has not published the managed TUN configuration")
    links = ns.json(["ip", "-j", "address", "show", "dev", "utun"])
    if not links or "UP" not in links[0]["flags"]:
        raise Failure("TUN is not up")
    families = {address["family"] for address in links[0]["addr_info"]
                if not {"tentative", "dadfailed"}.intersection(address.get("flags", []))}
    if not {"inet", "inet6"}.issubset(families):
        raise Failure("TUN has not acquired both address families")
    for family in ("4", "6"):
        rules = ns.json(["ip", "-j", f"-{family}", "rule", "show"])
        if not {1000, 1001, 1010, 1011}.issubset({rule.get("priority") for rule in rules}):
            raise Failure(f"IPv{family} managed lookup/blackhole rules are missing")
        for table, device, route_type in (("100", "lo", "local"), ("101", "utun", "unicast")):
            routes = ns.json(["ip", "-j", f"-{family}", "route", "show", "table", table])
            active = any(route.get("dev") == device and route.get("type", "unicast") == route_type
                         and route.get("dst") == "default" for route in routes)
            if not active or not any(route.get("type") == "blackhole" for route in routes):
                raise Failure(f"IPv{family} table {table} is not ready with its backup blackhole")
    ns.run(["nft", "list", "table", "inet", OWN_TABLE])
    current = sysctls(ns, monitored_sysctls())
    if current == baseline["sysctls"] or current["net/ipv6/conf/all/forwarding"] != "1":
        raise Failure("fixture did not exercise managed sysctl changes")
    return request(ns, "/upgrade/check")


def await_ready(core: Core, baseline: dict, previous_instance: str | None = None) -> str:
    deadline, last = time.monotonic() + 40, "no response"
    while time.monotonic() < deadline:
        core.pid()
        try:
            status = readiness(core.ns, baseline)
            instance = status.get("instance_id")
            if instance and instance != previous_instance:
                core.verify_identity()
                assert_no_record(core.directory)
                return instance
            last = "waiting for a new core instance after reexec"
        except (Failure, subprocess.TimeoutExpired, json.JSONDecodeError, FileNotFoundError) as error:
            last = str(error)
        time.sleep(0.2)
    raise Failure(f"core did not become ready: {last}")


def assert_restored(ns: Namespace, baseline: dict, directory: Path) -> None:
    ns.verify()  # Cleanup is observed before the namespace is destroyed.
    current = network_snapshot(ns)
    differences = {key: {"before": baseline[key], "after": current[key]}
                   for key in baseline if current[key] != baseline[key]}
    if differences:
        raise Failure("stop left network changes or removed unrelated resources:\n" + json.dumps(differences, indent=2, sort_keys=True))
    assert_no_record(directory)


def test_case(binary: Path, label: str, *, reexec: bool = False, systemd: bool = False) -> None:
    print(f"RUN {label}", flush=True)
    ns, core = None, None
    with tempfile.TemporaryDirectory(prefix="mihomo-stop-test-") as temp:
        directory = Path(temp)
        try:
            ns = Namespace()
            configure_namespace(ns)
            baseline = network_snapshot(ns)
            write_config(directory)
            core = Core(ns, binary, directory, systemd)
            instance = await_ready(core, baseline)
            if reexec:
                if request(ns, "/restart", "POST").get("status") != "ok":
                    raise Failure("controller did not acknowledge a prepared restart")
                await_ready(core, baseline, instance)
            core.stop()
            assert_restored(ns, baseline, directory)
            print(f"PASS {label}: exit 0; TUN, own nft/routes/rules removed; originals and foreign resources preserved", flush=True)
        except BaseException:
            if core:
                print(f"{label} core log:\n{core.diagnostics()}", file=sys.stderr, flush=True)
            if ns:
                try:
                    print(f"{label} final namespace snapshot:\n{json.dumps(network_snapshot(ns), indent=2, sort_keys=True)}", file=sys.stderr, flush=True)
                except Exception as error:
                    print(f"cannot inspect failed fixture: {error}", file=sys.stderr)
            raise
        finally:
            try:
                if core:
                    core.close()
            finally:
                if ns:
                    ns.close()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--core", required=True, type=Path, help="compiled mihomo executable (absolute path recommended)")
    parser.add_argument("--require-systemd", action="store_true", help="also require the actual systemctl stop test; unavailable systemd is a failure")
    arguments = parser.parse_args()
    binary = arguments.core.resolve(strict=True)
    if sys.platform != "linux" or os.geteuid() != 0:
        raise Failure("this required integration test needs root on Linux with network namespace capabilities")
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise Failure(f"core is not an executable file: {binary}")
    commands = ["unshare", "nsenter", "ip", "nft"]
    if arguments.require_systemd:
        commands += ["systemd-run", "systemctl"]
    for command in commands:
        if not shutil.which(command):
            raise Failure(f"required command is missing: {command}")
    if arguments.require_systemd:
        run(["systemctl", "show", "--property=Version"])
        service_properties()
    test_case(binary, "SIGTERM")
    test_case(binary, "controlled reexec then SIGTERM", reexec=True)
    if arguments.require_systemd:
        test_case(binary, "packaged service policy and systemctl stop", systemd=True)


if __name__ == "__main__":
    if sys.argv[1:] == ["--keeper"]:
        keeper()
    else:
        try:
            main()
        except (Failure, OSError, subprocess.TimeoutExpired) as error:
            print(f"FAIL: {error}", file=sys.stderr, flush=True)
            sys.exit(1)
