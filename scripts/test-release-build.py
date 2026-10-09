#!/usr/bin/env python3
"""Verify release identities, package metadata and payloads without installation."""
import gzip
import importlib.util
import json
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
import zipfile

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location('release_build', Path(__file__).with_name('release-build.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
VERSION = 'v1.19.32-dns-123456789abc'
COMMIT = '123456789abcdef0123456789abcdef0123456789'
TIME = '2026-10-09T02:00:00+00:00'


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='mihomo-release-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binary = self.root / 'mihomo'
        self.binary.write_bytes(b'\x7fELFpackage fixture\x00\xff\n' * 40)
        self.binary.chmod(0o755)
        self.out = self.root / 'dist'
        self.out.mkdir()

    def test_matrix_matches_promised_formats_and_cpu_baselines(self):
        rows = release.targets()
        self.assertEqual(len(rows), 37)
        self.assertEqual({os: sum(row['goos'] == os for row in rows) for os in ['linux', 'windows', 'darwin', 'freebsd', 'android']},
                         {'linux': 19, 'windows': 5, 'darwin': 4, 'freebsd': 5, 'android': 4})
        self.assertEqual(sum(len(release.asset_names(row, VERSION)) for row in rows), 66)
        self.assertEqual(sum('deb' in row['packages'] for row in rows), 12)
        self.assertEqual(sum('archlinux' in row['packages'] for row in rows), 5)
        self.assertEqual(release.target('amd64')['env'], {'GOAMD64': 'v1'})
        self.assertEqual(release.target('linux-amd64-v3')['env'], {'GOAMD64': 'v3'})
        self.assertEqual(release.target('linux-mips-softfloat')['env'], {'GOMIPS': 'softfloat'})
        self.assertFalse(any('abi1' in row['id'] or 'go120' in row['id'] for row in rows))

    def test_version_upgrade_order_and_timestamp_timezone(self):
        versions = release.package_versions(TIME, COMMIT, 'v1.19.32')
        self.assertEqual(versions['deb'], '1.19.32+dns.20261009020000.123456789abc')
        later = release.package_versions('2026-10-09T02:01:00Z', '0' * 40, 'v1.19.32')
        release.run('dpkg', '--compare-versions', later['deb'], 'gt', versions['deb'])
        same_time = release.package_versions('2026-10-09T10:00:00+08:00', COMMIT, 'v1.19.32')
        self.assertEqual(versions, same_time)
        with self.assertRaises(ValueError):
            release.basename(release.target('amd64'), '../unsafe')

    def test_windows_and_unix_archives_preserve_binary(self):
        for name in ['windows-amd64', 'linux-armv7']:
            row = release.target(name)
            path = release.write_archive(self.binary, row, VERSION, self.out, release.epoch(TIME))
            if row['goos'] == 'windows':
                with zipfile.ZipFile(path) as archive:
                    self.assertEqual(archive.namelist(), ['mihomo-dns-windows-amd64.exe'])
                    self.assertEqual(archive.read(archive.namelist()[0]), self.binary.read_bytes())
            else:
                self.assertEqual(gzip.decompress(path.read_bytes()), self.binary.read_bytes())

    def test_all_package_architectures_preserve_configuration_and_binary(self):
        for tool in ['dpkg-deb', 'rpm', 'rpmbuild', 'rpm2cpio', 'cpio', 'zstd']:
            self.assertIsNotNone(shutil.which(tool), f'Install package inspection dependency: {tool}')
        count = 0
        for row in release.targets():
            if row['packages']:
                with self.subTest(target=row['id']):
                    built = release.package_binary(self.binary, row, VERSION, self.out, TIME, COMMIT, 'v1.19.32')
                    self.assertEqual(len(built), len(row['packages']))
                    count += len(built)
        self.assertEqual(count, 29)

    def fixture_collection(self):
        for row in release.targets():
            assets = []
            for name in release.asset_names(row, VERSION):
                path = self.out / name
                path.write_bytes(name.encode())
                sha = release.digest(path)
                path.with_name(name + '.sha256').write_text(f'{sha}  {name}\n')
                assets.append({'name': name, 'size': path.stat().st_size, 'sha256': sha})
            record = {'target': row, 'version': VERSION, 'commit': COMMIT, 'assets': assets}
            (self.out / f"manifest-{row['id']}.json").write_text(json.dumps(record))

    def test_collection_requires_every_target_and_exact_checksums(self):
        self.fixture_collection()
        release.collect(self.out, VERSION, COMMIT)
        self.assertEqual(len((self.out / 'SHA256SUMS').read_text().splitlines()), 67)
        with self.assertRaisesRegex(ValueError, 'identity mismatch'):
            release.collect(self.out, VERSION, '0' * 40)
        asset = self.out / release.asset_names(release.target('amd64'), VERSION)[0]
        original = asset.read_bytes()
        asset.write_bytes(b'corrupt')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            release.collect(self.out, VERSION, COMMIT)
        asset.write_bytes(original)
        (self.out / 'manifest-android-arm64-v8.json').unlink()
        with self.assertRaises(FileNotFoundError):
            release.collect(self.out, VERSION, COMMIT)

    def test_existing_release_cannot_promote_incomplete_or_changed_assets(self):
        self.fixture_collection()
        release.collect(self.out, VERSION, COMMIT)
        names = [line.split('  ', 1)[1] for line in (self.out / 'SHA256SUMS').read_text().splitlines()] + ['SHA256SUMS']
        metadata = {'draft': False, 'assets': [{'name': name, 'size': (self.out / name).stat().st_size, 'digest': 'sha256:' + release.digest(self.out / name)} for name in names]}
        saved = self.root / 'published.json'
        saved.write_text(json.dumps(metadata))
        release.verify_published(self.out, saved)
        metadata['assets'].pop()
        saved.write_text(json.dumps(metadata))
        with self.assertRaisesRegex(ValueError, 'incomplete or differs'):
            release.verify_published(self.out, saved)

    def test_collection_rejects_unmanifested_assets(self):
        self.fixture_collection()
        (self.out / 'unexpected.gz').write_bytes(b'not verified')
        with self.assertRaisesRegex(ValueError, 'unexpected release files'):
            release.collect(self.out, VERSION, COMMIT)


if __name__ == '__main__':
    unittest.main(verbosity=2)
