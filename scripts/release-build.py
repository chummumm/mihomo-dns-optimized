#!/usr/bin/env python3
"""Build the checked release matrix and inspect installable packages, never install them."""

import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parent.parent
PACKAGE = "mihomo-optimized"
LEGACY_PACKAGE = "mihomo-dns-optimized"
NDK_VERSION = "29.0.14206865"
GO_ENV = ("GOOS", "GOARCH", "GOAMD64", "GO386", "GOARM", "GOMIPS", "GOMIPS64", "CGO_ENABLED", "CC", "CXX")
PR_TARGETS = ("linux-amd64", "linux-arm64", "linux-386", "windows-amd64",
              "darwin-arm64", "freebsd-amd64", "android-arm64-v8")
PLATFORM_NAMES = frozenset("""
    aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd
    openbsd plan9 solaris wasip1 windows zos unix bsd posix win32 win64
    386 amd64 amd64p32 arm arm64 arm64be armbe loong64 loongarch64 mips mipsle
    mips64 mips64le mips64p32 mips64p32le ppc ppc64 ppc64le riscv riscv64 s390
    s390x sparc sparc64 wasm x86 aarch64 armv5 armv6 armv7
    arch platform platforms cgo netlink syscall syscalls
""".split())
NATIVE_SOURCE_SUFFIXES = {".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx",
                          ".m", ".mm", ".s", ".asm", ".inc", ".syso"}
PAYLOAD = {
    "usr/bin/mihomo": 0o755,
    "etc/mihomo/config.yaml": 0o644,
    "usr/lib/systemd/system/mihomo.service": 0o644,
    f"usr/share/licenses/{PACKAGE}/LICENSE": 0o644,
    f"usr/share/doc/{PACKAGE}/README": 0o644,
}


def run(*args, env=None, cwd=None, binary=False, timeout=None):
    result = subprocess.run(args, cwd=cwd or ROOT, env=env, check=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=not binary, timeout=timeout)
    return result.stdout


def targets():
    rows = json.loads((ROOT / "packaging/targets.json").read_text())
    ids = [row["id"] for row in rows]
    if len(ids) != len(set(ids)):
        raise ValueError("duplicate release target")
    for row in rows:
        if not re.fullmatch(r"[a-z0-9-]+", row["id"]):
            raise ValueError("unsafe target name")
        if row["packages"] and row["goos"] != "linux":
            raise ValueError("native Linux packages assigned to another OS")
    return rows


def target(name):
    name = {"amd64": "linux-amd64", "arm64": "linux-arm64"}.get(name, name)
    return next(row for row in targets() if row["id"] == name)


def matrix_path_risk(path):
    name = path.rsplit("/", 1)[-1]
    lower = name.lower()
    if path.startswith(("packaging/", "scripts/", ".github/workflows/", "vendor/")):
        return "build, package or dependency path"
    if name in {"go.mod", "go.sum"} or name.startswith(("go.work", "UPSTREAM")):
        return "dependency or upstream version"
    if lower in {"makefile", "gnumakefile"} or lower.startswith(("dockerfile", ".goreleaser")):
        return "build definition"
    version_parts = set(re.split(r"[._-]+", lower))
    if (lower in {".go-version", ".tool-versions", ".mise.toml", "mise.toml"} or
            "toolchain" in lower or
            version_parts.intersection({"go", "golang", "ndk"}) and
            version_parts.intersection({"version", "versions"})):
        return "toolchain version"
    if Path(path).suffix.lower() in NATIVE_SOURCE_SUFFIXES:
        return "native source or assembly"
    if path.endswith(".go") and PLATFORM_NAMES.intersection(re.split(r"[/_.-]+", path.lower())):
        return "platform or architecture Go source"
    return None


def pr_matrix_risk(base, head):
    """Return why a PR needs every target; unknown observations also require all.

    Inspect committed objects at the unique merge base and PR head, never the
    checked-out merge or working tree. Raw NUL-delimited records preserve all
    path bytes, file types and both blobs; disabling renames retains deletions.
    """
    try:
        if not all(isinstance(ref, str) and re.fullmatch(r"[0-9a-f]{40}", ref)
                   for ref in (base, head)):
            raise ValueError("base and head must be full commit SHAs")
        ancestors = run("git", "merge-base", "--all", base, head, timeout=15).splitlines()
        if len(ancestors) != 1 or not re.fullmatch(r"[0-9a-f]{40}", ancestors[0]):
            raise ValueError("PR does not have one known merge base")
        # This is base...head, with the merge base checked for ambiguity first.
        changes = run("git", "diff", "--raw", "-z", "--no-renames", "--no-abbrev",
                      "--no-ext-diff", "--no-textconv", "--no-color",
                      ancestors[0], head, "--", binary=True, timeout=15)
        if not changes:
            return None
        records = changes.split(b"\0")
        if records.pop() != b"" or len(records) % 2:
            raise ValueError("incomplete NUL-delimited git diff")
        header_pattern = rb":([0-7]{6}) ([0-7]{6}) ([0-9a-f]{40}) ([0-9a-f]{40}) ([ADMT])"
        for header, raw_path in zip(records[::2], records[1::2]):
            entry = re.fullmatch(header_pattern, header)
            if entry is None or not raw_path:
                raise ValueError("unrecognized raw git diff entry")
            path = raw_path.decode("utf-8")
            old_mode, new_mode, old_blob, new_blob, status = entry.groups()
            if status == b"T" or any(mode not in {b"000000", b"100644", b"100755"}
                                     for mode in (old_mode, new_mode)):
                return f"non-regular file or type change: {path!r}"
            risk = matrix_path_risk(path)
            if risk:
                return f"{risk}: {path!r}"
            if not path.endswith(".go"):
                continue
            for blob in dict.fromkeys((old_blob, new_blob)):
                if blob == b"0" * 40:
                    continue
                content = run("git", "cat-file", "blob", blob.decode("ascii"),
                              binary=True, timeout=15).decode("utf-8-sig")
                if re.search(r"(?m)^[ \t]*//[ \t]*(?:go:build|\+build)(?=\s|$)", content):
                    return f"Go build constraint in old or new source: {path!r}"
                if re.search(r"\b(?:GOOS|GOARCH)\b", content):
                    return f"runtime platform selection in old or new source: {path!r}"
                if '"C"' in content or '`C`' in content:
                    # Comments may separate import tokens or contain a closing
                    # parenthesis. Strip comments without altering quoted literals.
                    code = re.sub(r'''("(?:\\.|[^"\\])*"|`[^`]*`|'(?:\\.|[^'\\])*')|//[^\n]*|/\*.*?\*/''',
                                  lambda match: match[1] or ' ', content, flags=re.DOTALL)
                    if re.search(r'\bimport\s*(?:["`]C["`]|\([^)]*["`]C["`])', code):
                        return f"cgo import in old or new source: {path!r}"
        return None
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        # A shallow checkout, missing fork object or unreadable blob must never
        # silently turn an uncertain change into a reduced release build.
        return f"cannot inspect PR diff: {error}"


def release_matrix(profile="full", base=None, head=None):
    if profile not in {"full", "pr"}:
        raise ValueError("unknown matrix profile")
    rows = targets()
    reason = "full profile requested"
    selected = "full"
    if profile == "pr":
        reason = pr_matrix_risk(base, head)
        by_name = {row["id"]: row for row in rows}
        if reason is None and not set(PR_TARGETS).issubset(by_name):
            reason = "representative targets are missing from the release matrix"
        if reason is None:
            rows = [by_name[name] for name in PR_TARGETS]
            selected, reason = "pr", "ordinary base...head changes"
    print(f"release matrix: profile={selected}; targets={len(rows)}; {reason}", file=sys.stderr)
    return {"include": [{"target": row["id"], "android": row["goos"] == "android",
                         "packages": bool(row["packages"])} for row in rows]}


def epoch(build_time):
    return int(datetime.fromisoformat(build_time.replace("Z", "+00:00")).timestamp())


def version_numbers(value, prefix):
    match = re.fullmatch(re.escape(prefix) + r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", value)
    if not match or any(int(part) >= 1 << 64 for part in match.groups()):
        raise ValueError("invalid canonical version")
    return tuple(int(part) for part in match.groups())


def project_numbers(version):
    return version_numbers(version, "mihomo-optimized-v")


def legacy_version(version):
    match = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-(?:dns-)?optimized-([1-9][0-9]*)", version)
    if not match or any(int(part) >= 1 << 64 for part in match.groups()):
        raise ValueError("invalid historical release version")
    return match.groups()


def package_versions(build_time, commit, upstream, version=None):
    version_numbers(upstream, "v")
    if version is not None and version.startswith("mihomo-optimized-"):
        base = ".".join(map(str, project_numbers(version)))
        # A permanent package epoch makes the independent project version newer
        # than the historical upstream-derived package numbering.
        return {"deb": f"1:{base}", "rpm_epoch": "1", "rpm_version": base,
                "rpm_release": "1", "archlinux": f"1:{base}-1"}
    stamp = datetime.fromtimestamp(epoch(build_time), timezone.utc).strftime("%Y%m%d%H%M%S")
    # Retain historical calculations for migration checks, not new releases.
    revision = "0"
    if version is not None:
        # Historical tags remain readable; the native package version below
        # keeps its existing prefix so renaming a release never resets upgrades.
        major, minor, patch, revision = legacy_version(version)
        if upstream != f"v{major}.{minor}.{patch}":
            raise ValueError("release version must contain a numeric optimized revision")
    suffix = f"dns.{stamp}.{revision}"
    base = upstream[1:]
    return {"deb": f"{base}+{suffix}", "rpm_epoch": "0", "rpm_version": base,
            "rpm_release": f"1.{suffix}", "archlinux": f"{base}.{suffix}-1"}


def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as source:
        for data in iter(lambda: source.read(1024 * 1024), b""):
            h.update(data)
    return h.hexdigest()


def basename(row, version):
    if version.startswith("mihomo-optimized-"):
        project_numbers(version)
        return f"{version}-{row['id']}"
    legacy_version(version)
    return f"mihomo-{row['id']}-{version}"


def package_basename(row, version):
    # Native installers have stable download URLs. Their control metadata still
    # records the version that was installed; core self-updates do not edit the
    # system package manager's database.
    if version.startswith("mihomo-optimized-v"):
        project_numbers(version)
        return f"mihomo-optimized-{row['id']}"
    return basename(row, version)


def asset_names(row, version):
    base = basename(row, version)
    names = [base + (".zip" if row["goos"] == "windows" else ".gz")]
    suffix = {"deb": ".deb", "rpm": ".rpm", "archlinux": ".pkg.tar.zst"}
    return names + [package_basename(row, version) + suffix[kind] for kind in row["packages"]]


def write_archive(binary, row, version, output, timestamp):
    name = asset_names(row, version)[0]
    path = output / name
    if row["goos"] == "windows":
        prefix = "mihomo-optimized-" if version.startswith("mihomo-optimized-") else "mihomo-"
        info = zipfile.ZipInfo(f"{prefix}{row['id']}.exe", datetime.fromtimestamp(timestamp, timezone.utc).timetuple()[:6])
        info.create_system = 3
        info.external_attr = (0o100755 << 16)
        info.compress_type = zipfile.ZIP_DEFLATED
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6) as archive:
            archive.writestr(info, binary.read_bytes())
        with zipfile.ZipFile(path) as archive:
            if archive.namelist() != [info.filename] or hashlib.sha256(archive.read(info.filename)).hexdigest() != digest(binary):
                raise ValueError("Windows archive changed the binary")
    else:
        with path.open("wb") as destination, gzip.GzipFile(fileobj=destination, mode="wb", filename="", mtime=0, compresslevel=6) as archive:
            with binary.open("rb") as source:
                shutil.copyfileobj(source, archive)
        with gzip.open(path, "rb") as archive:
            if hashlib.sha256(archive.read()).hexdigest() != digest(binary):
                raise ValueError("gzip archive changed the binary")
    return path


def stage_payload(binary, directory, timestamp):
    sources = {
        "usr/bin/mihomo": binary,
        "etc/mihomo/config.yaml": ROOT / "packaging/config.yaml",
        "usr/lib/systemd/system/mihomo.service": ROOT / "packaging/mihomo.service",
        f"usr/share/licenses/{PACKAGE}/LICENSE": ROOT / "LICENSE",
    }
    for path, source in sources.items():
        destination = directory / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination)
    readme = directory / f"usr/share/doc/{PACKAGE}/README"
    readme.parent.mkdir(parents=True, exist_ok=True)
    readme.write_text(
        "mihomo-optimized\n\n"
        "Binary: /usr/bin/mihomo\nConfiguration: /etc/mihomo/config.yaml\n"
        "Service: mihomo.service\n\n"
        "The service runs as mihomo. Installation creates a non-login system\n"
        "account only if absent, and assigns /etc/mihomo (recursively) and\n"
        "/usr/bin/mihomo to that account and its primary group, retaining modes.\n"
        "This package does not start, enable or restart any service.\n"
        "Review your configuration, then run:\n"
        "  mihomo -t -d /etc/mihomo\n"
        "  systemctl daemon-reload\n"
        "  systemctl enable --now mihomo\n\n"
        "Configuration is preserved on upgrades. This package replaces the\n"
        "official mihomo package and owns the same binary/config/service paths.\n"
        "Back up an existing installation before deliberately switching packages.\n"
        "https://github.com/chummumm/mihomo-optimized/blob/main/docs/releases.md\n"
    )
    for name, mode in PAYLOAD.items():
        os.chmod(directory / name, mode)
    for path in directory.rglob("*"):
        os.utime(path, (timestamp, timestamp))


def pack_tar(root, timestamp):
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.PAX_FORMAT) as archive:
        for path in sorted(root.rglob("*")):
            info = archive.gettarinfo(str(path), arcname=path.relative_to(root).as_posix())
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            info.mtime = timestamp
            if path.is_file():
                with path.open("rb") as source:
                    archive.addfile(info, source)
            else:
                archive.addfile(info)
    return buffer.getvalue()


def package_postinstall(kind):
    script = (ROOT / "packaging/postinstall.sh").read_text()
    if kind == "deb":
        # Only configure installs/upgrades own resources. Abort hooks do not.
        return script.replace("set -eu\n", 'set -eu\n[ "$1" = configure ] || exit 0\n', 1)
    if kind == "rpm":
        return script
    if kind == "archlinux":
        # A subshell keeps set -e scoped to our hook in pacman's shell.
        return "post_install() (\n" + script + ")\n\npost_upgrade() {\n    post_install\n}\n"
    raise ValueError("unknown package format")


def inspect_package(path, kind, architecture, binary, versions):
    with tempfile.TemporaryDirectory(prefix="mihomo-package-inspect-") as temp:
        extracted = Path(temp)
        if kind == "deb":
            fields = run("dpkg-deb", "-f", str(path), "Package", "Architecture", "Version", "Provides", "Conflicts", "Replaces", "Depends")
            required = [f"Package: {PACKAGE}", f"Architecture: {architecture}", f"Version: {versions['deb']}",
                        f"Provides: mihomo, {LEGACY_PACKAGE}", f"Conflicts: mihomo, {LEGACY_PACKAGE}", f"Replaces: mihomo, {LEGACY_PACKAGE}", "Depends: passwd"]
            if any(value not in fields for value in required):
                raise ValueError(f"unexpected Debian metadata: {fields}")
            control = extracted / "control"
            run("dpkg-deb", "-e", str(path), str(control))
            if (control / "conffiles").read_text().splitlines() != ["/etc/mihomo/config.yaml"]:
                raise ValueError("Debian package lost conffile preservation")
            if set(p.name for p in control.iterdir()) != {"control", "conffiles", "postinst"}:
                raise ValueError("unexpected package lifecycle scripts")
            if (control / "postinst").read_text() != package_postinstall(kind) or (control / "postinst").stat().st_mode & 0o777 != 0o755:
                raise ValueError("incorrect Debian account/resource installation hook")
            shutil.rmtree(control)
            payload = run("dpkg-deb", "--fsys-tarfile", str(path), binary=True)
            with tarfile.open(fileobj=io.BytesIO(payload)) as archive:
                if any(member.uid != 0 or member.gid != 0 for member in archive.getmembers()):
                    raise ValueError("Debian package payload must be owned by root")
            run("dpkg-deb", "-x", str(path), str(extracted))
        elif kind == "rpm":
            fields = run("rpm", "-qp", "--qf", "%{NAME}\n%{ARCH}\n%{EPOCHNUM}\n%{VERSION}\n%{RELEASE}\n", str(path)).splitlines()
            if fields != [PACKAGE, architecture, versions["rpm_epoch"], versions["rpm_version"], versions["rpm_release"]]:
                raise ValueError(f"unexpected RPM metadata: {fields}")
            ownership = run("rpm", "-qp", "--qf", "[%{FILEUSERNAME}:%{FILEGROUPNAME}\n]", str(path)).splitlines()
            if any(owner != "root:root" for owner in ownership):
                raise ValueError("RPM payload must be owned by root")
            verify = run("rpm", "-qp", "--qf", "[%{FILENAMES} %{FILEVERIFYFLAGS}\\n]", str(path)).splitlines()
            for entry in verify:
                name, mask = entry.rsplit(" ", 1)
                dynamic_owner = name in {"/usr/bin/mihomo", "/etc/mihomo/config.yaml"}
                # RPMVERIFY_USER | RPMVERIFY_GROUP; every other check remains.
                if int(mask) & 0x18 != (0 if dynamic_owner else 0x18):
                    raise ValueError("incorrect RPM ownership verification policy: " + name)
            flags = run("rpm", "-qp", "--qf", "[%{FILENAMES} %{FILEFLAGS}\n]", str(path))
            if "/etc/mihomo/config.yaml 17\n" not in flags:
                raise ValueError("RPM package lost config(noreplace)")
            if run("rpm", "-qp", "--qf", "%{POSTIN}", str(path)).strip() != package_postinstall(kind).strip():
                raise ValueError("incorrect RPM account/resource installation hook")
            for hook in ["PREIN", "PREUN", "POSTUN", "PRETRANS", "POSTTRANS", "VERIFYSCRIPT"]:
                if run("rpm", "-qp", "--qf", f"%{{{hook}}}", str(path)).strip() != "(none)":
                    raise ValueError("unexpected RPM lifecycle script: " + hook)
            if "shadow-utils" not in run("rpm", "-qp", "--requires", str(path)).splitlines():
                raise ValueError("RPM package lost account creation dependency")
            for relation in ["--conflicts", "--obsoletes"]:
                if not {"mihomo", LEGACY_PACKAGE}.issubset(set(run("rpm", "-qp", relation, str(path)).splitlines())):
                    raise ValueError("RPM package lost official/legacy package replacement")
            payload = run("rpm2cpio", str(path), binary=True)
            subprocess.run(["cpio", "-id", "--quiet", "--no-absolute-filenames"], input=payload, cwd=extracted, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        elif kind == "archlinux":
            payload = run("zstd", "-q", "-d", "-c", str(path), binary=True)
            with tarfile.open(fileobj=io.BytesIO(payload)) as archive:
                for member in archive.getmembers():
                    if member.uid != 0 or member.gid != 0:
                        raise ValueError("Arch package payload must be owned by root")
                    if member.name.startswith("/") or ".." in Path(member.name).parts or member.issym() or member.islnk():
                        raise ValueError("unsafe package member")
                archive.extractall(extracted, filter="data")
            info = (extracted / ".PKGINFO").read_text()
            for field in [f"pkgname = {PACKAGE}", f"arch = {architecture}", f"pkgver = {versions['archlinux']}",
                          "backup = etc/mihomo/config.yaml", "conflict = mihomo", "replaces = mihomo",
                          f"conflict = {LEGACY_PACKAGE}", f"replaces = {LEGACY_PACKAGE}", "depend = shadow"]:
                if field not in info.splitlines():
                    raise ValueError(f"Arch package missing metadata: {field}")
            (extracted / ".PKGINFO").unlink()
            if (extracted / ".INSTALL").read_text() != package_postinstall(kind):
                raise ValueError("incorrect Arch account/resource installation hook")
            (extracted / ".INSTALL").unlink()
        else:
            raise ValueError("unknown package format")
        actual = {p.relative_to(extracted).as_posix() for p in extracted.rglob("*") if p.is_file()}
        if actual != set(PAYLOAD):
            raise ValueError(f"unexpected package payload: {actual ^ set(PAYLOAD)}")
        for name, mode in PAYLOAD.items():
            if (extracted / name).stat().st_mode & 0o777 != mode:
                raise ValueError(f"incorrect mode for {name}")
        if digest(extracted / "usr/bin/mihomo") != digest(binary):
            raise ValueError("package binary differs from the tested build")
        if (extracted / "etc/mihomo/config.yaml").read_bytes() != (ROOT / "packaging/config.yaml").read_bytes():
            raise ValueError("package contains an unexpected configuration")
        if (extracted / "usr/lib/systemd/system/mihomo.service").read_bytes() != (ROOT / "packaging/mihomo.service").read_bytes():
            raise ValueError("package contains an unexpected service unit")


def package_binary(binary, row, version, output, build_time, commit, upstream):
    timestamp = epoch(build_time)
    versions = package_versions(build_time, commit, upstream, version)
    built = []
    with tempfile.TemporaryDirectory(prefix="mihomo-package-") as temp:
        work = Path(temp)
        stage = work / "root"
        stage_payload(binary, stage, timestamp)
        for kind, architecture in row["packages"].items():
            path = output / (package_basename(row, version) + {"deb": ".deb", "rpm": ".rpm", "archlinux": ".pkg.tar.zst"}[kind])
            if kind == "deb":
                control = stage / "DEBIAN"
                control.mkdir()
                (control / "control").write_text(
                    f"Package: {PACKAGE}\nVersion: {versions['deb']}\nArchitecture: {architecture}\n"
                    "Maintainer: chummumm <chummumm@users.noreply.github.com>\n"
                    "Depends: passwd\n"
                    f"Section: net\nPriority: optional\nProvides: mihomo, {LEGACY_PACKAGE}\nConflicts: mihomo, {LEGACY_PACKAGE}\nReplaces: mihomo, {LEGACY_PACKAGE}\n"
                    f"Installed-Size: {sum((stage / p).stat().st_size for p in PAYLOAD) // 1024 + 1}\n"
                    "Homepage: https://github.com/chummumm/mihomo-optimized\n"
                    "Description: mihomo-optimized core with shared DNS transports and managed updates\n"
                )
                (control / "conffiles").write_text("/etc/mihomo/config.yaml\n")
                (control / "postinst").write_text(package_postinstall(kind))
                (control / "postinst").chmod(0o755)
                env = dict(os.environ, SOURCE_DATE_EPOCH=str(timestamp))
                run("dpkg-deb", "--root-owner-group", "-Zgzip", "-z6", "--build", str(stage), str(path), env=env)
                shutil.rmtree(control)
            elif kind == "rpm":
                top = work / "rpm"
                for sub in ["BUILD", "RPMS", "SOURCES", "SPECS", "SRPMS"]:
                    (top / sub).mkdir(parents=True, exist_ok=True)
                spec = top / "SPECS/mihomo.spec"
                files = "\n".join(
                    ("%verify(not user group) " if name in {"usr/bin/mihomo", "etc/mihomo/config.yaml"} else "") +
                    ("%config(noreplace) " if name.startswith("etc/") else "%license " if "/licenses/" in name else "") +
                    "/" + name for name in PAYLOAD)
                spec.write_text(f"""Name: {PACKAGE}
Epoch: {versions['rpm_epoch']}
Version: {versions['rpm_version']}
Release: {versions['rpm_release']}
Summary: mihomo-optimized proxy core
License: GPL-3.0-or-later
URL: https://github.com/chummumm/mihomo-optimized
AutoReqProv: no
Provides: mihomo = %{{epoch}}:%{{version}}-%{{release}}
Provides: {LEGACY_PACKAGE}
Conflicts: mihomo, {LEGACY_PACKAGE}
Obsoletes: mihomo, {LEGACY_PACKAGE}
Requires(post): shadow-utils, /usr/bin/getent, coreutils
%global debug_package %{{nil}}
%global __os_install_post %{{nil}}
%description
mihomo-optimized with shared DNS transports and managed updates.
Configuration is preserved. Services are never started by the package.
%install
mkdir -p %{{buildroot}}
cp -a {shlex.quote(str(stage))}/. %{{buildroot}}/
%post
{package_postinstall(kind)}
%files
%defattr(-,root,root)
{files}
""")
                env = dict(os.environ, SOURCE_DATE_EPOCH=str(timestamp))
                run("rpmbuild", "-bb", "--target", architecture, "--define", f"_topdir {top}", "--define", "_buildhost reproducible", "--define", "_build_id_links none", "--define", "_binary_payload w6.gzdio", "--define", "use_source_date_epoch_as_buildtime 1", "--define", "clamp_mtime_to_source_date_epoch 1", str(spec), env=env)
                packages = list((top / "RPMS").rglob("*.rpm"))
                if len(packages) != 1:
                    raise ValueError("RPM build did not produce exactly one package")
                shutil.copyfile(packages[0], path)
                shutil.rmtree(top)
            else:
                info = stage / ".PKGINFO"
                info.write_text(
                    f"pkgname = {PACKAGE}\npkgbase = {PACKAGE}\npkgver = {versions['archlinux']}\n"
                    "pkgdesc = mihomo-optimized proxy core\n"
                    "url = https://github.com/chummumm/mihomo-optimized\n"
                    f"builddate = {timestamp}\npackager = chummumm\nsize = {sum((stage / p).stat().st_size for p in PAYLOAD)}\n"
                    f"arch = {architecture}\nlicense = GPL-3.0-or-later\nprovides = mihomo\n"
                    "depend = shadow\n"
                    f"provides = {LEGACY_PACKAGE}\nconflict = mihomo\nreplaces = mihomo\nconflict = {LEGACY_PACKAGE}\nreplaces = {LEGACY_PACKAGE}\nbackup = etc/mihomo/config.yaml\n"
                )
                install = stage / ".INSTALL"
                install.write_text(package_postinstall(kind))
                install.chmod(0o644)
                with path.open("wb") as destination:
                    subprocess.run(["zstd", "-q", "-T0", "-10", "-c"], input=pack_tar(stage, timestamp), stdout=destination, check=True)
                info.unlink()
                install.unlink()
            inspect_package(path, kind, architecture, binary, versions)
            built.append(path)
    return built


def build(row, output, version, build_time):
    project_numbers(version)
    output = Path(output)
    if not output.is_absolute():
        raise ValueError("output directory must be absolute")
    output.mkdir(parents=True, exist_ok=True)
    basename(row, version)
    timestamp = epoch(build_time)
    commit = run("git", "rev-parse", "HEAD").strip()
    upstream = (ROOT / "UPSTREAM_VERSION").read_text().strip()
    env = dict(os.environ)
    for name in GO_ENV:
        env.pop(name, None)
    env.update(GOOS=row["goos"], GOARCH=row["goarch"], CGO_ENABLED="0", GOTOOLCHAIN="local", GOFLAGS="-mod=readonly")
    env.update(row["env"])
    if row["goos"] == "android":
        ndk = Path(os.environ["ANDROID_NDK_HOME"])
        properties = (ndk / "source.properties").read_text()
        if f"Pkg.Revision = {NDK_VERSION}" not in properties:
            raise ValueError(f"Android requires the pinned NDK {NDK_VERSION}")
        compiler = ndk / "toolchains/llvm/prebuilt/linux-x86_64/bin" / row["android_cc"]
        if not compiler.is_file():
            raise ValueError("Android NDK compiler is missing")
        env.update(CGO_ENABLED="1", CC=str(compiler))
    with tempfile.TemporaryDirectory(prefix=f"mihomo-{row['id']}-") as temp:
        binary = Path(temp) / ("mihomo.exe" if row["goos"] == "windows" else "mihomo")
        flags = f"-s -w -buildid= -X github.com/metacubex/mihomo/constant.Version={version} -X github.com/metacubex/mihomo/constant.BuildTime={build_time}"
        subprocess.run(["go", "build", "-tags", "with_gvisor", "-trimpath", "-ldflags", flags, "-o", str(binary), "."], env=env, cwd=ROOT, check=True)
        info = run("go", "version", "-m", str(binary))
        for key in ("GOOS", "GOARCH", "CGO_ENABLED"):
            if f"\t{key}={env[key]}" not in info:
                raise ValueError(f"binary build information does not confirm {key}")
        if "\t-tags=with_gvisor" not in info:
            raise ValueError("binary is missing the intended gVisor build tag")
        if row["id"] == "linux-amd64" and run("go", "env", "GOHOSTOS").strip() == "linux" and run("go", "env", "GOHOSTARCH").strip() == "amd64":
            subprocess.run([str(binary), "-v"], check=True)
            subprocess.run([str(binary), "-t", "-d", temp, "-f", str(ROOT / "packaging/config.yaml")], check=True)
            subprocess.run(["python3", "scripts/test-dns-proxy.py", str(binary)], cwd=ROOT, check=True)
            subprocess.run(["python3", "scripts/test-dns-perf.py", str(binary), "--expect-shared"], cwd=ROOT, check=True)
            subprocess.run(["python3", "scripts/test-dns-observability.py", str(binary)], cwd=ROOT, check=True)
        built = [write_archive(binary, row, version, output, timestamp)]
        built.extend(package_binary(binary, row, version, output, build_time, commit, upstream))
        assets = []
        for path in built:
            sha = digest(path)
            assets.append({"name": path.name, "size": path.stat().st_size, "sha256": sha})
            path.with_name(path.name + ".sha256").write_text(f"{sha}  {path.name}\n")
        manifest = {"target": row, "version": version, "commit": commit, "upstream": upstream,
                    "build_time": build_time, "toolchain": run("go", "version").strip(),
                    "cgo": env["CGO_ENABLED"], "tags": ["with_gvisor"], "assets": assets}
        (output / f"manifest-{row['id']}.json").write_text(json.dumps(manifest, indent=2) + "\n")
        print(f"PASS release target {row['id']}: {len(assets)} verified assets", flush=True)


def collect(directory, version, commit):
    directory = Path(directory)
    records = []
    assets = {}
    for row in targets():
        record = json.loads((directory / f"manifest-{row['id']}.json").read_text())
        if record["commit"] != commit or record["version"] != version or record["target"] != row:
            raise ValueError(f"manifest identity mismatch for {row['id']}")
        if {a["name"] for a in record["assets"]} != set(asset_names(row, version)):
            raise ValueError(f"missing or extra assets for {row['id']}")
        for asset in record["assets"]:
            name = asset["name"]
            if name in assets:
                raise ValueError("duplicate asset name")
            path = directory / name
            if path.stat().st_size != asset["size"] or digest(path) != asset["sha256"]:
                raise ValueError(f"asset checksum mismatch: {name}")
            if path.with_name(name + ".sha256").read_text() != f"{asset['sha256']}  {name}\n":
                raise ValueError(f"asset checksum file mismatch: {name}")
            assets[name] = asset["sha256"]
        records.append(record)
    allowed = set(assets) | {name + ".sha256" for name in assets} | {f"manifest-{row['id']}.json" for row in targets()} | {"BUILDINFO.json", "SHA256SUMS", "version.txt"}
    unexpected = {path.name for path in directory.iterdir()} - allowed
    if unexpected:
        raise ValueError(f"unexpected release files: {sorted(unexpected)}")
    buildinfo = directory / "BUILDINFO.json"
    buildinfo.write_text(json.dumps({"commit": commit, "version": version, "targets": records}, indent=2) + "\n")
    assets[buildinfo.name] = digest(buildinfo)
    # The in-core updater discovers Latest once, then pins this exact version
    # tag for both SHA256SUMS and the architecture-specific archive.
    version_file = directory / "version.txt"
    version_file.write_text(version + "\n")
    assets[version_file.name] = digest(version_file)
    (directory / "SHA256SUMS").write_text("".join(f"{sha}  {name}\n" for name, sha in sorted(assets.items())))
    print(f"PASS release collection: {len(records)} targets, {len(assets)} checksummed assets", flush=True)


def verify_published(directory, metadata):
    directory = Path(directory)
    release = json.loads(Path(metadata).read_text())
    expected = {}
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        sha, name = line.split("  ", 1)
        expected[name] = {"size": (directory / name).stat().st_size, "digest": "sha256:" + sha}
    expected["SHA256SUMS"] = {"size": (directory / "SHA256SUMS").stat().st_size,
                              "digest": "sha256:" + digest(directory / "SHA256SUMS")}
    actual = {asset["name"]: {"size": asset["size"], "digest": asset.get("digest")} for asset in release["assets"]}
    if (release.get("draft") or release.get("prerelease") or
            release.get("tag_name") != (directory / "version.txt").read_text().strip() or
            actual != expected or len(actual) != len(release["assets"])):
        raise ValueError("existing release is incomplete or differs from this build; refusing to overwrite or promote it")
    print("PASS existing immutable release contains every expected asset", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    matrix = sub.add_parser("matrix")
    matrix.add_argument("--profile", choices=("full", "pr"), default="full")
    matrix.add_argument("--base", help="pull request event base commit SHA")
    matrix.add_argument("--head", help="pull request event head commit SHA")
    builder = sub.add_parser("build")
    builder.add_argument("target")
    builder.add_argument("output")
    builder.add_argument("version")
    builder.add_argument("build_time")
    collector = sub.add_parser("collect")
    collector.add_argument("directory")
    collector.add_argument("version")
    collector.add_argument("commit")
    published = sub.add_parser("verify-published")
    published.add_argument("directory")
    published.add_argument("metadata")
    args = parser.parse_args()
    if args.command == "matrix":
        print(json.dumps(release_matrix(args.profile, args.base, args.head), separators=(",", ":")))
    elif args.command == "build":
        build(target(args.target), args.output, args.version, args.build_time)
    elif args.command == "collect":
        collect(args.directory, args.version, args.commit)
    else:
        verify_published(args.directory, args.metadata)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        if error.stdout:
            print(error.stdout.decode() if isinstance(error.stdout, bytes) else error.stdout, flush=True)
        if error.stderr:
            print(error.stderr.decode() if isinstance(error.stderr, bytes) else error.stderr, flush=True)
        raise
