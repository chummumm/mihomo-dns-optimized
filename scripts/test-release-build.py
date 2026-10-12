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
    """Select CI coverage from real committed PR changes, without package tools."""

    representatives = ['linux-amd64', 'linux-arm64', 'linux-386', 'windows-amd64',
                       'darwin-arm64', 'freebsd-amd64', 'android-arm64-v8']

    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix='mihomo-matrix-test-')
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.write('packaging/targets.json', (release.ROOT / 'packaging/targets.json').read_bytes())
        self.write('scripts/release-build.py', Path(release.__file__).read_bytes())
        self.write('component/service.go', 'package component\n')
        self.write('PROJECT_VERSION', '1.0.7\n')
        self.git('init', '-b', 'main')
        self.base = self.commit()

    def git(self, *args):
        result = subprocess.run(['git', '-c', 'user.name=Matrix Fixture',
                                 '-c', 'user.email=matrix@example.invalid',
                                 '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=/dev/null', *args],
                                cwd=self.root, check=True, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        return result.stdout.strip()

    def write(self, name, content):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content.encode() if isinstance(content, str) else content)

    def commit(self):
        self.git('add', '--all')
        self.git('commit', '--allow-empty', '-m', 'matrix fixture')
        return self.git('rev-parse', 'HEAD')

    def matrix(self, *args):
        diagnostics = io.StringIO()
        with mock.patch.object(release, 'ROOT', self.root), contextlib.redirect_stderr(diagnostics):
            result = release.release_matrix(*args)
        return result, diagnostics.getvalue()

    def assert_profile(self, profile, base, head):
        result, diagnostics = self.matrix('pr', base, head)
        expected = self.representatives if profile == 'pr' else [row['id'] for row in release.targets()]
        self.assertEqual([row['target'] for row in result['include']], expected, diagnostics)
        self.assertIn(f'profile={profile}; targets={len(expected)};', diagnostics)
        return diagnostics

    def cli(self, *args):
        result = subprocess.run([sys.executable, '-B', str(self.root / 'scripts/release-build.py'),
                                 'matrix', *args], cwd=self.root, check=True, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(len(result.stdout.splitlines()), 1)
        return json.loads(result.stdout), result.stderr

    def test_default_and_explicit_full_keep_every_target_without_git(self):
        with mock.patch.object(release, 'run', side_effect=AssertionError('full must not inspect Git')):
            default, diagnostics = self.matrix()
            explicit, _ = self.matrix('full', 'invalid', 'invalid')
        cli, cli_diagnostics = self.cli()
        self.assertEqual(default, explicit)
        self.assertEqual(default, cli)
        self.assertEqual(len(default['include']), 37)
        self.assertIn('profile=full; targets=37;', diagnostics)
        self.assertIn('profile=full; targets=37;', cli_diagnostics)
        self.assertTrue(all(set(row) == {'target', 'android', 'packages'} for row in cli['include']))

    def test_ordinary_go_and_project_version_use_seven_representatives(self):
        self.write('component/service.go', 'package component\nconst Value = 2\n')
        self.write('PROJECT_VERSION', '1.0.8\n')
        head = self.commit()
        self.assert_profile('pr', self.base, head)
        cli, diagnostics = self.cli('--profile', 'pr', '--base', self.base, '--head', head)
        self.assertEqual([row['target'] for row in cli['include']], self.representatives)
        self.assertIn('profile=pr; targets=7;', diagnostics)
        self.assertEqual([row['target'] for row in cli['include'] if row['android']], ['android-arm64-v8'])
        self.assertEqual([row['target'] for row in cli['include'] if row['packages']], self.representatives[:3])

    def test_each_build_dependency_and_platform_path_requires_full(self):
        paths = [
            'go.mod', 'go.sum', 'go.work', 'go.work.sum', 'UPSTREAM_VERSION', 'UPSTREAM_COMMIT',
            '.go-version', '.tool-versions', '.mise.toml', 'GO_VERSION', 'NDK_VERSION', 'toolchain.txt',
            'packaging/config.yaml', 'scripts/helper.py', '.github/workflows/ci.yml', 'vendor/pkg/source.go',
            'Makefile', 'nested/Makefile', '.goreleaser.yml', 'Dockerfile',
            'component/source_linux.go', 'component/source_windows_amd64_test.go',
            'component/source_arm64_test.go', 'component/linux/source.go', 'component/unix/source.go',
            'component/arch/source.go', 'component/netlink/source.go',
            'component/helper.c', 'component/helper.C', 'component/helper.cpp', 'component/helper.h',
            'component/helper.s', 'component/helper.S', 'component/helper.asm', 'component/helper.syso',
        ]
        base = self.base
        for path in paths:
            with self.subTest(path=path):
                self.write(path, 'fixture\n')
                head = self.commit()
                self.assert_profile('full', base, head)
                base = head

    def test_new_and_removed_go_constraints_and_platform_code_require_full(self):
        sources = [
            '//go:build with_gvisor\n\npackage component\n',
            '// +build linux\n\npackage component\n',
            '\ufeff//go:build linux\n\npackage component\n',
            'package component\nimport "runtime"\nvar platform = runtime.GOOS\n',
            'package component\nimport machine "runtime"\nvar arch = machine.GOARCH\n',
            'package component\nimport "C"\n',
            'package component\nimport (\n"C"\n)\n',
            'package component\nimport /* comment */ "C"\n',
            'package component\nimport /* comment */ (\n/* ) */ "C"\n)\n',
            'package component\nimport `C`\n',
        ]
        base = self.base
        for source in sources:
            with self.subTest(source=source):
                self.write('component/service.go', source)
                added = self.commit()
                self.assert_profile('full', base, added)
                self.write('component/service.go', 'package component\n')
                removed = self.commit()
                self.assert_profile('full', added, removed)
                base = removed

    def test_deleted_and_renamed_platform_files_retain_old_path_and_blob(self):
        self.write('component/source_linux.go', 'package component\n')
        base = self.commit()
        self.git('mv', 'component/source_linux.go', 'component/portable.go')
        self.assert_profile('full', base, self.commit())
        self.write('component/tagged.go', '//go:build linux\n\npackage component\n')
        base = self.commit()
        self.git('rm', 'component/tagged.go')
        self.assert_profile('full', base, self.commit())
        self.write('UPSTREAM_COMMIT', 'upstream\n')
        base = self.commit()
        self.git('rm', 'UPSTREAM_COMMIT')
        self.assert_profile('full', base, self.commit())

    def test_unusual_paths_are_read_without_splitting_or_revision_interpretation(self):
        path = 'component/space : with\nnewline\tname.go'
        self.write(path, 'package component\n')
        base = self.commit()
        self.assert_profile('pr', self.base, base)
        self.write(path, '//go:build linux\n\npackage component\n')
        self.assert_profile('full', base, self.commit())

    def test_true_pr_range_excludes_later_base_branch_and_worktree_changes(self):
        self.git('checkout', '-b', 'pr')
        self.write('component/service.go', 'package component\nconst Value = 2\n')
        head = self.commit()
        self.git('checkout', 'main')
        self.write('packaging/base-only.txt', 'base branch change\n')
        base = self.commit()
        self.write('component/service.go', '//go:build linux\n\npackage component\n')
        self.assert_profile('pr', base, head)

    def test_missing_or_invalid_diff_context_falls_back_with_diagnostic(self):
        for base, head in [(None, None), ('main', self.base), (self.base, '--help'),
                           ('0' * 40, self.base), (self.base, 'f' * 40)]:
            with self.subTest(base=base, head=head):
                self.assertIn('cannot inspect PR diff:', self.assert_profile('full', base, head))
        result, diagnostics = self.cli('--profile', 'pr')
        self.assertEqual(len(result['include']), 37)
        self.assertIn('cannot inspect PR diff:', diagnostics)

    def test_unavailable_git_and_failed_diff_do_not_reduce_coverage(self):
        errors = [FileNotFoundError('git unavailable'), subprocess.TimeoutExpired('git', 15)]
        for error in errors:
            with self.subTest(error=type(error).__name__), mock.patch.object(release, 'run', side_effect=error):
                self.assertIn('cannot inspect PR diff:', self.assert_profile('full', self.base, self.base))
        # The merge base can succeed while the subsequent diff is unavailable.
        failure = subprocess.CalledProcessError(128, ['git', 'diff'])
        with mock.patch.object(release, 'run', side_effect=[self.base + '\n', failure]):
            self.assertIn('cannot inspect PR diff:', self.assert_profile('full', self.base, self.base))
        with mock.patch.object(release, 'run', return_value=self.base + '\n' + 'f' * 40 + '\n'):
            self.assertIn('one known merge base', self.assert_profile('full', self.base, self.base))

    def test_missing_committed_blob_requires_full(self):
        self.write('component/service.go', 'package component\nconst Value = 123\n')
        head = self.commit()
        blob = self.git('rev-parse', head + ':component/service.go')
        (self.root / '.git/objects' / blob[:2] / blob[2:]).unlink()
        self.assertIn('cannot inspect PR diff:', self.assert_profile('full', self.base, head))

    def test_unreadable_go_content_and_type_changes_require_full(self):
        self.write('component/service.go', b'package component\n//\xff\n')
        invalid = self.commit()
        self.assertIn('cannot inspect PR diff:', self.assert_profile('full', self.base, invalid))
        (self.root / 'component/service.go').unlink()
        (self.root / 'component/service.go').symlink_to('elsewhere')
        self.assertIn('type change', self.assert_profile('full', invalid, self.commit()))

    def test_unrelated_history_requires_full(self):
        self.git('checkout', '--orphan', 'unrelated')
        self.write('unrelated.txt', 'independent root commit\n')
        head = self.commit()
        self.assertIn('cannot inspect PR diff:', self.assert_profile('full', self.base, head))


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
