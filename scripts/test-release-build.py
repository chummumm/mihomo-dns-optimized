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
VERSION = 'v1.19.32-optimized-9'
LEGACY_VERSION = 'v1.19.32-dns-optimized-8'
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
        for row in rows:
            for name in release.asset_names(row, VERSION):
                self.assertTrue(name.startswith(f"mihomo-{row['id']}-{VERSION}."), name)
                self.assertFalse(name.startswith('mihomo-dns-'), name)
        self.assertIn('mihomo-linux-arm64-v1.19.32-optimized-9.deb',
                      release.asset_names(release.target('linux-arm64'), VERSION))

    def test_version_upgrade_order_and_timestamp_timezone(self):
        versions = release.package_versions(TIME, COMMIT, 'v1.19.32', VERSION)
        self.assertEqual(versions['deb'], '1.19.32+dns.20261009020000.9')
        later = release.package_versions('2026-10-09T02:01:00Z', '0' * 40, 'v1.19.32', 'v1.19.32-optimized-10')
        release.run('dpkg', '--compare-versions', later['deb'], 'gt', versions['deb'])
        same_time = release.package_versions('2026-10-09T10:00:00+08:00', COMMIT, 'v1.19.32', VERSION)
        self.assertEqual(versions, same_time)
        release.run('dpkg', '--compare-versions', later['deb'], 'gt', '1.19.32+dns.20261009020000.123456789abc')
        first_new = release.package_versions('2026-10-09T02:01:00Z', COMMIT, 'v1.19.32', 'v1.19.32-optimized-1')
        release.run('dpkg', '--compare-versions', first_new['deb'], 'gt', '1.19.32+dns.20261009020000.2')

    def test_renamed_tags_preserve_package_identity_and_upgrade_order(self):
        self.assertEqual(release.PACKAGE, 'mihomo-dns-optimized')
        for revision in [1, 7, 8, 9, 10, 1234]:
            with self.subTest(revision=revision):
                canonical = release.package_versions(TIME, COMMIT, 'v1.19.32', f'v1.19.32-optimized-{revision}')
                legacy = release.package_versions(TIME, COMMIT, 'v1.19.32', f'v1.19.32-dns-optimized-{revision}')
                # Exact equality covers Debian, RPM and Arch package versions:
                # their comparators never see the changed release-tag prefix.
                self.assertEqual(canonical, legacy)
        for previous_tag, next_tag in [
            ('v1.19.32-dns-optimized-7', VERSION),
            ('v1.19.32-optimized-8', VERSION),
            ('v1.19.32-dns-optimized-9', 'v1.19.32-optimized-10'),
            ('v1.19.32-optimized-9', 'v1.19.32-dns-optimized-10'),
        ]:
            with self.subTest(previous=previous_tag, next=next_tag):
                previous = release.package_versions(TIME, COMMIT, 'v1.19.32', previous_tag)
                following = release.package_versions(TIME, COMMIT, 'v1.19.32', next_tag)
                release.run('dpkg', '--compare-versions', following['deb'], 'gt', previous['deb'])
        previous = release.package_versions(TIME, COMMIT, 'v1.19.32', LEGACY_VERSION)
        following = release.package_versions('2026-10-09T02:01:00Z', COMMIT, 'v1.19.32', VERSION)
        release.run('dpkg', '--compare-versions', following['deb'], 'gt', previous['deb'])

    def test_release_versions_reject_malformed_or_mismatched_values(self):
        invalid_versions = ['v1.19.32-dns.8', 'v1.19.32-dns-deadbeef1234',
                            'v1.19.33-optimized-8', '1.19.32-optimized-8',
                            'v1.19.32-dns-dns-optimized-8', 'v1.19.32-optimized']
        for prefix in ['v1.19.32-optimized-', 'v1.19.32-dns-optimized-']:
            invalid_versions.extend(prefix + revision for revision in
                                    ['', '0', '00', '01', '-1', '+1', '8.1', '8-extra', '8/../', '8\n', '8 ', '８'])
        for invalid in invalid_versions:
            with self.subTest(version=invalid), self.assertRaises(ValueError):
                release.package_versions(TIME, COMMIT, 'v1.19.32', invalid)
        for upstream in ['1.19.32', 'v1.19', 'v1.19.32\n', VERSION, '../v1.19.32']:
            with self.subTest(upstream=upstream), self.assertRaises(ValueError):
                release.package_versions(TIME, COMMIT, upstream, VERSION)
        with self.assertRaises(ValueError):
            release.basename(release.target('amd64'), '../unsafe')

    def test_windows_and_unix_archives_preserve_binary(self):
        for row in release.targets():
            name = row['id']
            path = release.write_archive(self.binary, row, VERSION, self.out, release.epoch(TIME))
            self.assertEqual(path.name, f'mihomo-{name}-{VERSION}' + ('.zip' if row['goos'] == 'windows' else '.gz'))
            if row['goos'] == 'windows':
                with zipfile.ZipFile(path) as archive:
                    self.assertEqual(archive.namelist(), [f'mihomo-{name}.exe'])
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
                    self.assertEqual({path.name for path in built}, set(release.asset_names(row, VERSION)[1:]))
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
        self.assertEqual(len((self.out / 'SHA256SUMS').read_text().splitlines()), 68)
        self.assertEqual((self.out / 'version.txt').read_text(), VERSION + '\n')
        recorded = json.loads((self.out / 'BUILDINFO.json').read_text())
        for target in recorded['targets']:
            for asset in target['assets']:
                self.assertTrue(asset['name'].startswith('mihomo-'))
                self.assertFalse(asset['name'].startswith('mihomo-dns-'))
        with self.assertRaisesRegex(ValueError, 'identity mismatch'):
            release.collect(self.out, VERSION, '0' * 40)
        with self.assertRaisesRegex(ValueError, 'identity mismatch'):
            release.collect(self.out, LEGACY_VERSION, COMMIT)
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
        metadata = {'draft': False, 'prerelease': False, 'tag_name': VERSION, 'assets': [{'name': name, 'size': (self.out / name).stat().st_size, 'digest': 'sha256:' + release.digest(self.out / name)} for name in names]}
        saved = self.root / 'published.json'
        saved.write_text(json.dumps(metadata))
        release.verify_published(self.out, saved)
        metadata['tag_name'] = LEGACY_VERSION
        saved.write_text(json.dumps(metadata))
        with self.assertRaisesRegex(ValueError, 'incomplete or differs'):
            release.verify_published(self.out, saved)
        metadata['tag_name'] = VERSION
        metadata['assets'].pop()
        saved.write_text(json.dumps(metadata))
        with self.assertRaisesRegex(ValueError, 'incomplete or differs'):
            release.verify_published(self.out, saved)

    def test_collection_rejects_unmanifested_assets(self):
        self.fixture_collection()
        # New releases contain only canonical assets, never old-name copies.
        for name in ['unexpected.gz', f'mihomo-dns-linux-arm64-{VERSION}.gz']:
            with self.subTest(name=name):
                path = self.out / name
                path.write_bytes(b'not verified')
                with self.assertRaisesRegex(ValueError, 'unexpected release files'):
                    release.collect(self.out, VERSION, COMMIT)
                path.unlink()


if __name__ == '__main__':
    unittest.main(verbosity=2)
