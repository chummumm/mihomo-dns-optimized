#!/usr/bin/env python3
"""Verify release identities, package metadata and payloads without installation."""
import contextlib
import gzip
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import zipfile

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location('release_build', Path(__file__).with_name('release-build.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
VERSION = 'mihomo-optimized-v1.0.0'
LEGACY_VERSION = 'v1.19.32-dns-optimized-8'
COMMIT = '123456789abcdef0123456789abcdef0123456789'
TIME = '2026-10-09T02:00:00+00:00'


class AccountFixture:
    """Run the real hooks against account/filesystem commands confined to fixtures."""

    def __init__(self, root):
        root.mkdir()
        self.root = root
        self.state = root / 'accounts.json'
        self.state.write_text(json.dumps({'passwd': None, 'group': None, 'calls': []}))
        commands = root / 'commands'
        commands.mkdir()
        # No privileged command is ever run against the host. In particular,
        # the chrootless dpkg migration below must redirect absolute chown paths.
        mock = '''#!/usr/bin/python3
import json, os, pathlib, sys
path = pathlib.Path(os.environ['MIHOMO_ACCOUNT_FIXTURE'])
state = json.loads(path.read_text())
command = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
state['calls'].append([command, *args])
path.write_text(json.dumps(state))
if os.environ.get('MIHOMO_ACCOUNT_FAIL') == command:
    sys.exit(9)
if command == 'getent':
    assert args in [['passwd', 'mihomo'], ['group', 'mihomo']], args
    entry = state[args[0]]
    if entry is None:
        sys.exit(2)
    print(entry)
elif command == 'groupadd':
    assert state['group'] is None
    state['group'] = 'mihomo:x:991:'
elif command == 'useradd':
    assert state['passwd'] is None
    gid = state['group'].split(':')[2]
    state['passwd'] = 'mihomo:x:990:' + gid + '::/nonexistent:/usr/sbin/nologin'
elif command == 'id':
    assert args == ['-g', 'mihomo'], args
    print(state['passwd'].split(':')[3])
elif command == 'chown':
    assert args[0] in ['-hR', '-h'] and args[1] == '--', args
    assert args[3] in ['/etc/mihomo', '/usr/bin/mihomo'], args
else:
    raise AssertionError(command)
path.write_text(json.dumps(state))
'''
        for name in ['getent', 'groupadd', 'useradd', 'id', 'chown']:
            command = commands / name
            command.write_text(mock)
            command.chmod(0o755)
        self.env = dict(os.environ, MIHOMO_ACCOUNT_FIXTURE=str(self.state),
                        PATH=str(commands) + os.pathsep + os.environ['PATH'])

    def read(self):
        return json.loads(self.state.read_text())

    def replace(self, **fields):
        state = self.read()
        state.update(fields)
        self.state.write_text(json.dumps(state))

    def run(self, kind, argument=None):
        script = self.root / ('postinstall-' + kind)
        script.write_text(release.package_postinstall(kind))
        if kind == 'archlinux':
            return release.run('sh', '-c', '. "$1"; "$2"', '--', str(script),
                               argument or 'post_install', env=self.env)
        return release.run('sh', str(script), argument or ('configure' if kind == 'deb' else '1'), env=self.env)


class MatrixTests(unittest.TestCase):
    """PRs only compile representatives; release collection still requires all."""

    representatives = ['linux-amd64', 'linux-arm64', 'linux-386', 'windows-amd64',
                       'darwin-arm64', 'freebsd-amd64', 'android-arm64-v8']

    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix='mihomo-matrix-test-')
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        (self.root / 'packaging').mkdir()
        (self.root / 'scripts').mkdir()
        shutil.copyfile(release.ROOT / 'packaging/targets.json', self.root / 'packaging/targets.json')
        shutil.copyfile(release.__file__, self.root / 'scripts/release-build.py')

    def matrix(self, *args):
        diagnostics = io.StringIO()
        with mock.patch.object(release, 'ROOT', self.root), contextlib.redirect_stderr(diagnostics):
            result = release.release_matrix(*args)
        return result, diagnostics.getvalue()

    def cli(self, *args):
        result = subprocess.run([sys.executable, '-B', str(self.root / 'scripts/release-build.py'),
                                 'matrix', *args], cwd=self.root, check=True, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(len(result.stdout.splitlines()), 1)
        return json.loads(result.stdout), result.stderr

    def test_default_and_explicit_full_keep_every_target_without_git(self):
        with mock.patch.object(release, 'run', side_effect=AssertionError('matrix must not inspect Git')):
            default, diagnostics = self.matrix()
            explicit, _ = self.matrix('full')
        cli, cli_diagnostics = self.cli()
        self.assertEqual(default, explicit)
        self.assertEqual(default, cli)
        self.assertEqual(len(default['include']), 37)
        self.assertIn('profile=full; targets=37;', diagnostics)
        self.assertIn('profile=full; targets=37;', cli_diagnostics)
        self.assertTrue(all(set(row) == {'target', 'android', 'packages'} for row in cli['include']))
        expected_packages = [row['id'] for row in release.targets() if row['packages']]
        self.assertEqual([row['target'] for row in cli['include'] if row['packages']], expected_packages)

    def test_pr_profile_is_bounded_even_without_git_or_diff_context(self):
        with mock.patch.object(release, 'run', side_effect=AssertionError('PR must not inspect Git')):
            matrix, diagnostics = self.matrix('pr')
        cli, cli_diagnostics = self.cli('--profile', 'pr')
        self.assertEqual(matrix, cli)
        self.assertEqual([row['target'] for row in matrix['include']], self.representatives)
        self.assertEqual([row['target'] for row in matrix['include'] if row['android']], ['android-arm64-v8'])
        self.assertFalse(any(row['packages'] for row in matrix['include']))
        self.assertIn('profile=pr; targets=7; compile checks only; no release artifacts', diagnostics)
        self.assertEqual(diagnostics, cli_diagnostics)

    def test_missing_required_pr_target_fails_instead_of_packaging_the_full_matrix(self):
        path = self.root / 'packaging/targets.json'
        rows = [row for row in json.loads(path.read_text()) if row['id'] != 'android-arm64-v8']
        path.write_text(json.dumps(rows))
        with self.assertRaisesRegex(ValueError, 'required PR compile targets are missing'):
            self.matrix('pr')

    def test_unknown_profile_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'unknown matrix profile'):
            self.matrix('partial-release')


class CompileCheckTests(unittest.TestCase):
    """Exercise the public PR command with a recorded external Go tool boundary."""

    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix='mihomo-compile-command-test-')
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        (self.root / 'packaging').mkdir()
        (self.root / 'scripts').mkdir()
        shutil.copyfile(release.ROOT / 'packaging/targets.json', self.root / 'packaging/targets.json')
        shutil.copyfile(release.__file__, self.root / 'scripts/release-build.py')
        commands = self.root / 'commands'
        commands.mkdir()
        go = commands / 'go'
        go.write_text(r'''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
record = {'args': args, 'env': {key: os.environ[key] for key in
          ['GOOS', 'GOARCH', 'CGO_ENABLED', 'GOARM', 'GOAMD64', 'CC'] if key in os.environ}}
with pathlib.Path(os.environ['MIHOMO_COMPILE_LOG']).open('a') as output:
    output.write(json.dumps(record) + '\n')
if args[0] == 'build':
    output = pathlib.Path(args[args.index('-o') + 1])
    output.write_text(json.dumps(record['env']))
    sys.exit(int(os.environ.get('MIHOMO_COMPILE_FAIL', '0')))
if args[:2] == ['version', '-m']:
    metadata = json.loads(pathlib.Path(args[2]).read_text())
    for key in ['GOOS', 'GOARCH', 'CGO_ENABLED']:
        value = os.environ.get('MIHOMO_COMPILE_BAD_OS') if key == 'GOOS' else None
        print('\t' + key + '=' + (value or metadata[key]))
    print('\t-tags=with_gvisor')
    sys.exit(0)
raise AssertionError(args)
''')
        go.chmod(0o755)
        self.log = self.root / 'go-calls.jsonl'
        self.env = dict(os.environ, PATH=str(commands) + os.pathsep + os.environ['PATH'],
                        MIHOMO_COMPILE_LOG=str(self.log), GOOS='windows', GOARCH='amd64',
                        GOAMD64='v3', CGO_ENABLED='1', CC='unrelated-compiler')

    def check(self, **env):
        result = subprocess.run([sys.executable, '-B', 'scripts/release-build.py',
                                 'compile-check', 'linux-armv7'], cwd=self.root,
                                env=dict(self.env, **env), text=True, capture_output=True)
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        binary = Path(calls[0]['args'][calls[0]['args'].index('-o') + 1])
        self.assertFalse(binary.parent.exists(), 'temporary core must be removed even on failure')
        self.assertEqual(calls[0]['env'], {'GOOS': 'linux', 'GOARCH': 'arm',
                                          'CGO_ENABLED': '0', 'GOARM': '7'})
        self.assertEqual(sorted(path.name for path in self.root.iterdir()),
                         ['commands', 'go-calls.jsonl', 'packaging', 'scripts'])
        return result, calls

    def test_public_compile_command_never_creates_release_assets(self):
        result, calls = self.check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call['args'][0] for call in calls], ['build', 'version'])
        self.assertNotIn('-ldflags', calls[0]['args'])
        self.assertIn('PASS compile check linux-armv7: no release artifacts', result.stdout)

    def test_compile_failure_is_not_reported_as_success(self):
        result, calls = self.check(MIHOMO_COMPILE_FAIL='23')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 1)
        self.assertNotIn('PASS', result.stdout)

    def test_wrong_binary_platform_is_rejected(self):
        result, _ = self.check(MIHOMO_COMPILE_BAD_OS='windows')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('binary build information does not confirm GOOS', result.stderr)
        self.assertNotIn('PASS', result.stdout)


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

    def test_package_hooks_create_nonlogin_account_and_migrate_resources(self):
        for kind in ['deb', 'rpm', 'archlinux']:
            with self.subTest(kind=kind):
                fixture = AccountFixture(self.root / kind)
                fixture.run(kind)
                state = fixture.read()
                self.assertEqual(state['passwd'], 'mihomo:x:990:991::/nonexistent:/usr/sbin/nologin')
                self.assertIn(['groupadd', '--system', 'mihomo'], state['calls'])
                self.assertIn(['useradd', '--system', '--gid', 'mihomo', '--home-dir', '/nonexistent',
                               '--no-create-home', '--shell', '/usr/sbin/nologin', 'mihomo'], state['calls'])
                self.assertEqual(state['calls'][-2:], [
                    ['chown', '-hR', '--', 'mihomo:991', '/etc/mihomo'],
                    ['chown', '-h', '--', 'mihomo:991', '/usr/bin/mihomo']])
                fixture.replace(calls=[])
                fixture.run(kind, 'post_upgrade' if kind == 'archlinux' else None)
                again = fixture.read()
                self.assertEqual(again['passwd'], state['passwd'])
                self.assertFalse(any(call[0] in ['groupadd', 'useradd'] for call in again['calls']))
                self.assertEqual(again['calls'][-2:], state['calls'][-2:])

    def test_package_hooks_reuse_existing_user_and_primary_group(self):
        original = 'mihomo:x:4321:5678:Existing account:/custom/home:/bin/bash'
        for kind in ['deb', 'rpm', 'archlinux']:
            with self.subTest(kind=kind):
                fixture = AccountFixture(self.root / kind)
                fixture.replace(passwd=original)
                fixture.run(kind)
                state = fixture.read()
                self.assertEqual(state['passwd'], original)
                self.assertIsNone(state['group'])
                self.assertFalse(any(call[0] in ['groupadd', 'useradd'] for call in state['calls']))
                self.assertEqual([call[3] for call in state['calls'] if call[0] == 'chown'],
                                 ['mihomo:5678', 'mihomo:5678'])

    def test_package_hooks_reuse_existing_group_and_surface_failure(self):
        fixture = AccountFixture(self.root / 'account')
        fixture.replace(group='mihomo:x:7654:')
        fixture.run('deb')
        self.assertEqual(fixture.read()['passwd'].split(':')[3], '7654')
        self.assertFalse(any(call[0] == 'groupadd' for call in fixture.read()['calls']))
        for kind in ['deb', 'rpm', 'archlinux']:
            for command in ['groupadd', 'useradd', 'chown']:
                with self.subTest(kind=kind, command=command):
                    broken = AccountFixture(self.root / (kind + '-' + command))
                    broken.env['MIHOMO_ACCOUNT_FAIL'] = command
                    with self.assertRaises(subprocess.CalledProcessError):
                        broken.run(kind)
                    self.assertEqual(broken.read()['calls'][-1][0], command)

    def test_debian_abort_hook_does_not_change_accounts_or_resources(self):
        fixture = AccountFixture(self.root / 'account')
        for argument in ['abort-upgrade', 'abort-remove', 'abort-deconfigure']:
            fixture.run('deb', argument)
        self.assertEqual(fixture.read()['calls'], [])

    def test_packaged_service_uses_mihomo_and_its_existing_primary_group(self):
        import configparser
        unit = configparser.ConfigParser(interpolation=None)
        unit.read(release.ROOT / 'packaging/mihomo.service')
        self.assertEqual(unit['Service']['User'], 'mihomo')
        self.assertNotIn('Group', unit['Service'])

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
                expected = f"{VERSION}-{row['id']}" if name.endswith(('.gz', '.zip')) else f"mihomo-optimized-{row['id']}"
                self.assertTrue(name.startswith(expected + '.'), name)
                self.assertFalse(name.startswith('mihomo-dns-'), name)
        self.assertIn('mihomo-optimized-linux-arm64.deb',
                      release.asset_names(release.target('linux-arm64'), VERSION))

    def test_version_upgrade_order_and_timestamp_timezone(self):
        versions = release.package_versions(TIME, COMMIT, 'v1.19.32', 'v1.19.32-optimized-9')
        self.assertEqual(versions['deb'], '1.19.32+dns.20261009020000.9')
        later = release.package_versions('2026-10-09T02:01:00Z', '0' * 40, 'v1.19.32', 'v1.19.32-optimized-10')
        release.run('dpkg', '--compare-versions', later['deb'], 'gt', versions['deb'])
        same_time = release.package_versions('2026-10-09T10:00:00+08:00', COMMIT, 'v1.19.32', 'v1.19.32-optimized-9')
        self.assertEqual(versions, same_time)
        release.run('dpkg', '--compare-versions', later['deb'], 'gt', '1.19.32+dns.20261009020000.123456789abc')
        first_new = release.package_versions('2026-10-09T02:01:00Z', COMMIT, 'v1.19.32', 'v1.19.32-optimized-1')
        release.run('dpkg', '--compare-versions', first_new['deb'], 'gt', '1.19.32+dns.20261009020000.2')

    def test_renamed_tags_preserve_package_identity_and_upgrade_order(self):
        self.assertEqual(release.PACKAGE, 'mihomo-optimized')
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

    def test_project_versions_have_permanent_migration_epoch_and_stable_installers(self):
        first = release.package_versions(TIME, COMMIT, 'v1.19.32', VERSION)
        self.assertEqual(first, {'deb': '1:1.0.0', 'rpm_epoch': '1', 'rpm_version': '1.0.0',
                                 'rpm_release': '1', 'archlinux': '1:1.0.0-1'})
        # Package version comes from the release, independent of wall time,
        # source hash and the separately recorded upstream base.
        self.assertEqual(first, release.package_versions('2030-01-01T00:00:00Z', 'f' * 40, 'v2.0.0', VERSION))
        for older, newer in [('1.0.0', '1.0.1'), ('1.0.2', '1.0.12'), ('1.9.99', '2.0.0')]:
            old = release.package_versions(TIME, COMMIT, 'v1.19.32', 'mihomo-optimized-v' + older)
            new = release.package_versions(TIME, COMMIT, 'v1.19.32', 'mihomo-optimized-v' + newer)
            release.run('dpkg', '--compare-versions', new['deb'], 'gt', old['deb'])
        for historical in ['1.19.32+dns.20261011000000.13', '9.99.99+dns.20300101000000.999']:
            release.run('dpkg', '--compare-versions', first['deb'], 'gt', historical)
        for row in release.targets():
            old = release.asset_names(row, VERSION)
            new = release.asset_names(row, 'mihomo-optimized-v1.0.1')
            self.assertNotEqual(old[0], new[0])
            self.assertEqual(old[1:], new[1:])

    def test_debian_rename_preserves_locally_modified_configuration(self):
        # Exercise real dpkg replacement/conffile behavior in a disposable root.
        # Account and chown commands are fixtures, including for chrootless hooks.
        stage = self.root / 'old-package'
        (stage / 'DEBIAN').mkdir(parents=True)
        (stage / 'usr/bin').mkdir(parents=True)
        (stage / 'etc/mihomo').mkdir(parents=True)
        (stage / 'usr/bin/mihomo').write_bytes(b'old installed binary')
        (stage / 'etc/mihomo/config.yaml').write_text('mixed-port: 7890\n')
        (stage / 'DEBIAN/control').write_text(
            'Package: mihomo-dns-optimized\nVersion: 1.19.32+dns.20261011000000.13\n'
            'Architecture: amd64\nMaintainer: Test <test@example.invalid>\nDescription: migration fixture\n')
        (stage / 'DEBIAN/conffiles').write_text('/etc/mihomo/config.yaml\n')
        old_package = self.root / 'previous.deb'
        release.run('dpkg-deb', '--root-owner-group', '--build', str(stage), str(old_package))
        root = self.root / 'installed-root'
        (root / 'var/lib/dpkg').mkdir(parents=True)
        (root / 'var/lib/dpkg/status').touch()
        fixture = AccountFixture(self.root / 'account')
        command = ['dpkg', '--root=' + str(root), '--force-not-root', '--force-bad-path',
                   '--force-architecture', '--force-confold', '--force-script-chrootless', '--force-depends']
        if os.geteuid() != 0:
            self.assertIsNotNone(shutil.which('fakeroot'), 'Install fakeroot for the isolated dpkg migration test')
            command.insert(0, 'fakeroot')
        release.run(*command, '--install', str(old_package), env=fixture.env)
        config = root / 'etc/mihomo/config.yaml'
        local_config = 'mixed-port: 8990\ncore-auto-update: true\n'
        config.write_text(local_config)
        row = dict(release.target('linux-amd64'), packages={'deb': 'amd64'})
        package, = release.package_binary(self.binary, row, VERSION, self.out, TIME, COMMIT, 'v1.19.32')
        release.run(*command, '--install', str(package), env=fixture.env)
        self.assertIsNotNone(fixture.read()['passwd'])
        self.assertEqual(config.read_text(), local_config)
        self.assertEqual((root / 'usr/bin/mihomo').read_bytes(), self.binary.read_bytes())
        status = release.run('dpkg-query', '--admindir=' + str(root / 'var/lib/dpkg'), '-W',
                             '-f=${Status} ${Version}', 'mihomo-optimized')
        self.assertEqual(status.strip(), 'install ok installed 1:1.0.0')
        owner = release.run('dpkg-query', '--admindir=' + str(root / 'var/lib/dpkg'), '-S', '/usr/bin/mihomo')
        self.assertEqual(owner.strip(), 'mihomo-optimized: /usr/bin/mihomo')

    def test_release_versions_reject_malformed_or_mismatched_values(self):
        invalid_versions = ['v1.19.32-dns.8', 'v1.19.32-dns-deadbeef1234',
                            'v1.19.33-optimized-8', '1.19.32-optimized-8',
                            'v1.19.32-dns-dns-optimized-8', 'v1.19.32-optimized']
        invalid_versions.extend('mihomo-optimized-v' + suffix for suffix in
                                ['1.0', '01.0.0', '1.01.0', '1.0.01', '1.0.0-rc1', '1.0.0+build',
                                 '1.0.0/../', '1.0.0\n', '1.0.0 ', '１.0.0',
                                 '18446744073709551616.0.0', '1.18446744073709551616.0', '1.0.18446744073709551616'])
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
            self.assertEqual(path.name, f'{VERSION}-{name}' + ('.zip' if row['goos'] == 'windows' else '.gz'))
            if row['goos'] == 'windows':
                with zipfile.ZipFile(path) as archive:
                    self.assertEqual(archive.namelist(), [f'mihomo-optimized-{name}.exe'])
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
