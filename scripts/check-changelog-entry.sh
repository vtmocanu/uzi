#!/usr/bin/env bash
# PR-time half of the CHANGELOG coverage rule: a branch that changes shipping code must
# add a CHANGELOG.md line or say why it has nothing to announce.
#
# usage: scripts/check-changelog-entry.sh
#
# WHY. scripts/assert-changelog-covers-release.sh (the release oracle) refuses a cut when a
# shipping merge since the last stable is not cited in the release section. It runs at cut
# time, so a merge that forgot its entry is found by whoever cuts the release, who then
# reconstructs the entry from the PR (0.85.0-rc.5 and rc.6 each needed such a commit). This
# check moves the same question to the PR, where the author still knows the answer. The
# release oracle stays the final check: it also covers citations written at cut time and
# merges that reached main without a PR.
#
# WHAT IT CHECKS. The diff from merge-base(origin/main, HEAD) to the working tree (committed,
# uncommitted and untracked, so a pre-commit `task gate` agrees with CI). When any path in it
# is shipping (scripts/lib/shipping-paths.sh, the oracle's own definition), one of these must
# hold:
#   - CHANGELOG.md gains at least one line (removals alone do not count; the landing
#     tooling's changelog guard owns deletions).
#   - A non-merge commit on the branch carries `Changelog: none` (the oracle's regex). The
#     repo squashes with COMMIT_MESSAGES, so the line reaches the squash body and exempts
#     the merge at release time too; it is therefore PR-wide by design. Add a reason.
#   - Every non-merge commit touching a shipping path has a `docs(...)`/`docs:` subject and
#     no shipping change is uncommitted. The oracle reads the SQUASH subject, so a docs-only
#     branch squashed under a non-docs PR title still fails there: a stricter later check,
#     never a bypass.
#   - Every shipping path is a dependency manifest (basename go.mod, go.sum, Dockerfile or
#     devbox.lock). Dependency bumps are cited in bulk at cut time, as before; this only
#     spares the bot branch a per-PR commit. deploy/chart/** is never exempt.
# Merge commits (a pull_request checkout's synthetic merge, a merged-in main) are skipped
# for messages, so their "Merge ..." subject neither breaks nor satisfies a rule.
#
# BASE AND FAILING CLOSED. When refs/remotes/origin/main does not resolve it is fetched
# (CI's lint-repo job checks out with fetch-depth 0, so this is the offline-laptop path). No
# base, or no merge-base (a shallow clone), exits 2: a check that cannot see its baseline
# must not read as "nothing to announce". On main itself HEAD is the merge-base and the
# diff is empty.
#
# EXIT CODES (check-migration-numbering.sh convention; `task` itself reports 201):
#     2 = instrument broken (not a git repo, base unresolvable, no merge-base)
#     1 = shipping change with no CHANGELOG line and no exemption
#     0 = nothing shipping, or an entry or exemption covers it
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "check-changelog-entry.sh: not inside a git repository" >&2
  exit 2
}
cd "$ROOT"
# shellcheck source=scripts/lib/shipping-paths.sh
. "$ROOT/scripts/lib/shipping-paths.sh"

BASE_REF="refs/remotes/origin/main"
if ! git rev-parse --verify --quiet "$BASE_REF^{commit}" > /dev/null; then
  git fetch --quiet --no-tags origin "+refs/heads/main:$BASE_REF" 2>/dev/null || true
fi
git rev-parse --verify --quiet "$BASE_REF^{commit}" > /dev/null || {
  echo "check-changelog-entry.sh: $BASE_REF does not resolve and could not be fetched; run: git fetch origin main" >&2
  exit 2
}
BASE="$(git merge-base "$BASE_REF" HEAD 2>/dev/null)" || {
  echo "check-changelog-entry.sh: no merge-base between HEAD and $BASE_REF (shallow clone?); fetch full history" >&2
  exit 2
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Every path changed since the base: tracked (committed or not) plus untracked.
{ git diff --name-only "$BASE" --; git ls-files --others --exclude-standard; } | sort -u > "$TMP/files"

shipping=0
manifest_only=1
: > "$TMP/shipping"
while read -r f; do
  [ -n "$f" ] || continue
  is_shipping "$f" || continue
  shipping=1
  printf '%s\n' "$f" >> "$TMP/shipping"
  case "${f##*/}" in
    go.mod|go.sum|Dockerfile|devbox.lock) ;;
    *) manifest_only=0 ;;
  esac
  case "$f" in deploy/chart/*) manifest_only=0 ;; esac
done < "$TMP/files"

pass() { echo "check-changelog-entry.sh: OK: $1"; exit 0; }

[ "$shipping" = 1 ] || pass "no shipping path changed since $(git rev-parse --short "$BASE")"

git diff "$BASE" -- CHANGELOG.md > "$TMP/changelog.diff"
grep -qE '^\+[^+]|^\+$' "$TMP/changelog.diff" && pass "CHANGELOG.md gains an entry"

git log --no-merges --format=%B "$BASE..HEAD" > "$TMP/bodies"
grep -qiE '^Changelog:[[:space:]]*none' "$TMP/bodies" && pass "a branch commit carries 'Changelog: none'"

# docs exemption: at least one shipping commit, all of them docs-typed, nothing shipping
# left uncommitted.
docs_ok=1
shipping_commits=0
while read -r sha; do
  [ -n "$sha" ] || continue
  touches=0
  while read -r f; do
    [ -n "$f" ] || continue
    if is_shipping "$f"; then touches=1; break; fi
  done < <(git diff-tree --no-commit-id --name-only -r "$sha")
  [ "$touches" = 1 ] || continue
  shipping_commits=$((shipping_commits + 1))
  git log -1 --format=%s "$sha" > "$TMP/subject"
  grep -qE '^docs(\([^)]*\))?:' "$TMP/subject" || docs_ok=0
done < <(git rev-list --no-merges "$BASE..HEAD")
{ git diff --name-only HEAD --; git ls-files --others --exclude-standard; } > "$TMP/uncommitted"
while read -r f; do
  [ -n "$f" ] || continue
  if is_shipping "$f"; then docs_ok=0; break; fi
done < "$TMP/uncommitted"
[ "$docs_ok" = 1 ] && [ "$shipping_commits" -gt 0 ] && pass "every shipping commit is docs-typed"

[ "$manifest_only" = 1 ] && pass "only dependency manifests changed (cited in bulk at cut time)"

echo "check-changelog-entry.sh: FAIL: shipping paths changed with no CHANGELOG.md entry:" >&2
sed 's/^/  /' "$TMP/shipping" | head -20 >&2
cat >&2 <<'MSG'

Add a line under `## [Unreleased]` in CHANGELOG.md (Keep-a-Changelog subsection, cite the
issue or PR number), or, when there is genuinely nothing to announce, put

    Changelog: none    # and why

in a commit message on this branch.
MSG
exit 1
