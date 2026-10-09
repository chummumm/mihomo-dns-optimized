#!/usr/bin/env bash
# Prepare a stable upstream merge. This script NEVER commits or pushes.
# The workflow tests the staged candidate, checks for concurrent main updates,
# and only then commits and pushes without --force.
set -euo pipefail

dry_run=false
if [[ "${1:-}" == --dry-run ]]; then
  dry_run=true
  shift
fi
tag=${1:?Usage: upstream-sync.sh [--dry-run] vX.Y.Z [upstream-git-url]}
upstream_url=${2:-https://github.com/MetaCubeX/mihomo.git}
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  printf 'Only stable vX.Y.Z release tags are accepted.\n' >&2
  exit 2
}

repo=$(git rev-parse --show-toplevel)
cd "$repo"
[[ -z "$(git status --porcelain)" ]] || {
  printf 'Start with a clean working tree and index.\n' >&2
  exit 1
}

if "$dry_run"; then
  scratch=$(mktemp -d)
  cp "${BASH_SOURCE[0]}" "$scratch/prepare.sh"
  cleanup_dry_run() {
    git -C "$repo" worktree remove --force "$scratch/tree" >/dev/null 2>&1 || true
    rm -rf "$scratch"
  }
  trap cleanup_dry_run EXIT
  git worktree add --detach "$scratch/tree" HEAD
  (
    cd "$scratch/tree"
    GITHUB_OUTPUT= bash "$scratch/prepare.sh" "$tag" "$upstream_url"
  )
  printf 'Dry run finished. Original working tree, HEAD, and remote branches were not changed.\n'
  exit 0
fi

emit() {
  printf '%s=%s\n' "$1" "$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf '%s=%s\n' "$1" "$2" >> "$GITHUB_OUTPUT"
  fi
}
base_commit=$(git rev-parse HEAD)
current_tag=$(cat UPSTREAM_VERSION)
current_commit=$(cat UPSTREAM_COMMIT)
[[ "$current_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$current_commit" =~ ^[0-9a-f]{40}$ ]]
git merge-base --is-ancestor "$current_commit" "$base_commit" || {
  printf 'Recorded upstream commit is not an ancestor of this fork.\n' >&2
  exit 1
}
emit base_commit "$base_commit"
if [[ "$tag" == "$current_tag" ]]; then
  emit changed false
  printf 'Already tracking upstream %s.\n' "$tag"
  exit 0
fi
python3 - "$current_tag" "$tag" <<'PY'
import sys
old, new = (tuple(map(int, tag[1:].split('.'))) for tag in sys.argv[1:])
if new <= old:
    sys.exit('Refusing to replace the tracked stable version with an older release.')
PY

# Record upstream commits in a dedicated namespace, not as derivative release
# tags. Reject a previously observed tag that is now pointing at another commit.
upstream_ref="refs/remotes/upstream-stable/$tag"
known_ref=$(git rev-parse -q --verify "$upstream_ref" || true)
git fetch --no-tags "$upstream_url" "refs/tags/$tag"
upstream_commit=$(git rev-parse "FETCH_HEAD^{commit}")
if [[ -n "$known_ref" && "$(git rev-parse "$known_ref^{commit}")" != "$upstream_commit" ]]; then
  printf 'Previously observed upstream tag now points to another commit.\n' >&2
  exit 1
fi
git merge-base --is-ancestor "$current_commit" "$upstream_commit" || {
  printf 'New upstream release does not descend from the recorded upstream commit.\n' >&2
  exit 1
}
git update-ref "$upstream_ref" "$upstream_commit" "${known_ref:-0000000000000000000000000000000000000000}"

merge_started=false
abort_on_failure() {
  status=$?
  if (( status != 0 )) && "$merge_started"; then
    git merge --abort || true
  fi
  exit "$status"
}
trap abort_on_failure EXIT
merge_status=0
git merge --no-commit --no-ff "$upstream_ref" || merge_status=$?
if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
  merge_started=true
elif (( merge_status != 0 )); then
  exit "$merge_status"
fi

# Restore the complete directory, including removal of newly imported workflows.
# Upstream build/release/dispatch jobs and changes to this guard never take over
# the derivative's automation, even when Git reports workflow merge conflicts.
preserved=(
  .github/workflows
  scripts/upstream-sync.sh
  scripts/ci-check.sh
  scripts/test-upstream-sync.py
  scripts/release-build.py
  scripts/test-release-build.py
  packaging
  UPSTREAM_VERSION
  UPSTREAM_COMMIT
)
git restore --source="$base_commit" --staged --worktree -- "${preserved[@]}"
conflicts=$(git diff --name-only --diff-filter=U)
if [[ -n "$conflicts" ]]; then
  printf 'Upstream merge requires manual conflict resolution:\n%s\n' "$conflicts" >&2
  exit 1
fi
git diff --exit-code "$base_commit" -- .github/workflows \
  scripts/upstream-sync.sh scripts/ci-check.sh scripts/test-upstream-sync.py \
  scripts/release-build.py scripts/test-release-build.py packaging

printf '%s\n' "$tag" > UPSTREAM_VERSION
printf '%s\n' "$upstream_commit" > UPSTREAM_COMMIT
git add UPSTREAM_VERSION UPSTREAM_COMMIT
emit changed true
emit upstream_commit "$upstream_commit"
printf 'Prepared %s. Test this candidate before committing or pushing.\n' "$tag"
