#!/usr/bin/env python3
"""Exercise stable sync in local repositories; never access GitHub or push."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SOURCE_ROOT = Path(__file__).resolve().parent.parent


def run(repo, *args, check=True, env=None):
    result = subprocess.run(
        args, cwd=repo, text=True, stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT, env=env,
    )
    if check and result.returncode:
        raise AssertionError(f"{args!r} failed ({result.returncode}):\n{result.stdout}")
    return result


def git(repo, *args):
    return run(repo, "git", *args).stdout.strip()


def write(repo, name, text):
    target = repo / name
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(text)


def commit(repo, message):
    git(repo, "add", ".")
    git(repo, "commit", "-m", message)
    return git(repo, "rev-parse", "HEAD")


class StableSyncTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="mihomo-sync-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.upstream = self.root / "upstream"
        self.upstream.mkdir()
        git(self.upstream, "init", "-b", "main")
        for repo in [self.upstream]:
            git(repo, "config", "user.name", "Sync test")
            git(repo, "config", "user.email", "test@example.invalid")
            git(repo, "config", "commit.gpgsign", "false")
        write(self.upstream, "code.txt", "upstream base\n")
        write(self.upstream, ".github/workflows/build.yml", "upstream automation\n")
        self.initial = commit(self.upstream, "initial upstream")
        git(self.upstream, "tag", "v1.0.0")
        self.fork = self.root / "fork"
        run(self.root, "git", "clone", str(self.upstream), str(self.fork))
        git(self.fork, "config", "user.name", "Sync test")
        git(self.fork, "config", "user.email", "test@example.invalid")
        git(self.fork, "config", "commit.gpgsign", "false")
        write(self.fork, ".github/workflows/build.yml", "fork automation\n")
        write(self.fork, "UPSTREAM_VERSION", "v1.0.0\n")
        write(self.fork, "UPSTREAM_COMMIT", self.initial + "\n")
        for name in ["upstream-sync.sh", "ci-check.sh", "test-upstream-sync.py", "release-build.py", "test-release-build.py"]:
            target = self.fork / "scripts" / name
            target.parent.mkdir(exist_ok=True)
            shutil.copyfile(SOURCE_ROOT / "scripts" / name, target)
        shutil.copytree(SOURCE_ROOT / "packaging", self.fork / "packaging")
        self.base = commit(self.fork, "fork DNS feature and automation")

    def sync(self, tag, *, dry_run=False, check=True):
        # Execute outside the repository, as the real workflow does.
        script = self.root / "prepare.sh"
        shutil.copyfile(self.fork / "scripts/upstream-sync.sh", script)
        args = ["bash", str(script)]
        if dry_run:
            args.append("--dry-run")
        args.extend([tag, str(self.upstream)])
        env = dict(os.environ, GITHUB_OUTPUT=str(self.root / "github-output"))
        return run(self.fork, *args, check=check, env=env)

    def new_upstream(self, *, code="upstream improved\n"):
        write(self.upstream, "code.txt", code)
        # Force a workflow conflict AND an added workflow, both to be discarded.
        write(self.upstream, ".github/workflows/build.yml", "changed upstream automation\n")
        write(self.upstream, ".github/workflows/unexpected.yml", "new upstream dispatch\n")
        write(self.upstream, "scripts/release-build.py", "unexpected build replacement\n")
        write(self.upstream, "packaging/unexpected", "unexpected package hook\n")
        self.latest = commit(self.upstream, "new upstream release")
        git(self.upstream, "tag", "v1.0.1")
        return self.latest

    def assert_clean_base(self, base=None):
        self.assertEqual(git(self.fork, "rev-parse", "HEAD"), base or self.base)
        self.assertEqual(git(self.fork, "status", "--porcelain"), "")
        self.assertFalse((self.fork / ".git/MERGE_HEAD").exists())

    def test_first_update_preserves_fork_workflows_and_repeat_is_noop(self):
        latest = self.new_upstream()
        result = self.sync("v1.0.1")
        self.assertIn("changed=true", result.stdout)
        self.assertEqual(git(self.fork, "rev-parse", "HEAD"), self.base)
        self.assertEqual((self.fork / "code.txt").read_text(), "upstream improved\n")
        self.assertEqual((self.fork / ".github/workflows/build.yml").read_text(), "fork automation\n")
        self.assertFalse((self.fork / ".github/workflows/unexpected.yml").exists())
        self.assertFalse((self.fork / "packaging/unexpected").exists())
        self.assertEqual((self.fork / "scripts/release-build.py").read_bytes(), (SOURCE_ROOT / "scripts/release-build.py").read_bytes())
        self.assertEqual((self.fork / "UPSTREAM_COMMIT").read_text().strip(), latest)
        merged = commit(self.fork, "tested sync")
        parents = git(self.fork, "show", "-s", "--format=%P", "HEAD").split()
        self.assertEqual(parents, [self.base, latest])
        result = self.sync("v1.0.1")
        self.assertIn("changed=false", result.stdout)
        self.assert_clean_base(merged)

    def test_existing_upstream_merge_only_updates_metadata(self):
        self.new_upstream()
        # Model a manually completed merge whose metadata was not updated.
        git(self.fork, "fetch", "origin", "main")
        git(self.fork, "merge", "--strategy=ours", "--no-ff", "FETCH_HEAD", "-m", "manual merge")
        base = git(self.fork, "rev-parse", "HEAD")
        self.sync("v1.0.1")
        self.assertEqual(git(self.fork, "rev-parse", "HEAD"), base)
        self.assertEqual((self.fork / "UPSTREAM_VERSION").read_text(), "v1.0.1\n")
        self.assertFalse((self.fork / ".git/MERGE_HEAD").exists())

    def test_code_conflict_aborts_without_overwriting_fork(self):
        write(self.fork, "code.txt", "fork DNS routing change\n")
        base = commit(self.fork, "fork source change")
        self.new_upstream(code="upstream conflicting source change\n")
        result = self.sync("v1.0.1", check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("manual conflict resolution", result.stdout)
        self.assert_clean_base(base)
        self.assertEqual((self.fork / "code.txt").read_text(), "fork DNS routing change\n")

    def test_dry_run_keeps_original_branch_and_working_tree(self):
        self.new_upstream()
        result = self.sync("v1.0.1", dry_run=True)
        self.assertIn("Dry run finished", result.stdout)
        self.assert_clean_base()
        self.assertEqual(len(git(self.fork, "worktree", "list", "--porcelain").split("worktree ")) - 1, 1)

    def test_rejects_prereleases_old_versions_and_unrelated_history(self):
        self.assertNotEqual(self.sync("v1.0.1-rc1", check=False).returncode, 0)
        self.assertNotEqual(self.sync("v0.9.9", check=False).returncode, 0)
        git(self.upstream, "checkout", "--orphan", "unrelated")
        write(self.upstream, "code.txt", "unrelated history\n")
        commit(self.upstream, "unrelated stable release")
        git(self.upstream, "tag", "v2.0.0")
        result = self.sync("v2.0.0", check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not descend", result.stdout)
        self.assert_clean_base()

    def test_dirty_working_tree_is_never_merged(self):
        self.new_upstream()
        write(self.fork, "code.txt", "uncommitted user work\n")
        result = self.sync("v1.0.1", check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("clean working tree", result.stdout)
        self.assertEqual((self.fork / "code.txt").read_text(), "uncommitted user work\n")
        self.assertEqual(git(self.fork, "rev-parse", "HEAD"), self.base)

    def test_retargeted_observed_tag_is_rejected(self):
        self.new_upstream()
        self.sync("v1.0.1")
        git(self.fork, "merge", "--abort")
        write(self.upstream, "code.txt", "retargeted release content\n")
        commit(self.upstream, "retarget a release")
        git(self.upstream, "tag", "--force", "v1.0.1")
        result = self.sync("v1.0.1", check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("now points to another commit", result.stdout)
        self.assert_clean_base()


if __name__ == "__main__":
    unittest.main(verbosity=2)
