#!/usr/bin/env bash
# Hermetic #1389 guard regressions; no remote, network, or real tags.
set -euo pipefail
section="${1:-$(cd "$(dirname "$0")" && pwd)/changelog-section.sh}"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/repo/deploy/chart"
cd "$root/repo"
git init -q
git config user.name fixture
git config user.email fixture@example.com
git config core.fsmonitor false
cat > CHANGELOG.md <<'CL'
# Changelog
## [Unreleased]
## [1.2.0] - 2026-10-01
### Changed
- **Published feature.**
  Its continuation is frozen too.
## [1.1.0] - 2026-09-01
- Initial.
CL
printf 'version: 1.2.0-rc.1\n' > deploy/chart/Chart.yaml
git add .
git commit -qm baseline
git tag v1.2.0-rc.1
cp CHANGELOG.md "$root/original.md"
pass=0
check() {
  local expected="$1" version="$2" label="$3" rc=0
  bash "$section" check-rc-bullets "$version" > "$root/output" 2>&1 || rc=$?
  if { [ "$expected" = pass ] && [ "$rc" -ne 0 ]; } ||
     { [ "$expected" = fail ] && [ "$rc" -eq 0 ]; }; then
    cat "$root/output" >&2
    echo "FAIL: $label (rc=$rc)" >&2
    exit 1
  fi
  pass=$((pass + 1))
}
check pass 1.2.0-rc.2 'unchanged shipped block passes'
awk '/^## \[1.1.0\]/{print "- **New feature.**"; print "  New work."} {print}' "$root/original.md" > CHANGELOG.md
check pass 1.2.0-rc.2 'unchanged plus new block passes'
check pass HEAD 'branch compares open section against reachable candidate'
awk '{sub(/continuation is frozen too/, "continuation was edited"); print}' "$root/original.md" > CHANGELOG.md
check fail 1.2.0-rc.2 'editing a shipped continuation fails'
if ! grep -F 'Published feature' "$root/output" >/dev/null; then
  echo 'FAIL: guard names the offending block' >&2; exit 1
fi
check fail HEAD 'branch gate rejects an edited shipped block'
awk '/Its continuation/{print "- **Inserted split.**"} {print}' "$root/original.md" > CHANGELOG.md
check fail 1.2.0-rc.2 'splitting a shipped block fails'
cp "$root/original.md" CHANGELOG.md
check fail 1.2.0-rc.3 'missing immediate predecessor fails closed'
check fail 1.2.0-rc.18446744073709551618 'oversized RC index cannot skip predecessor evidence'
check pass 1.2.0 'stable promotion preserves the newest RC blocks'
check pass 2.0.0-rc.1 'first candidate has no predecessor'
git tag v1.2.0-rc.2
printf '## [Unreleased]\n## [1.2.0]\n- altered\n' > CHANGELOG.md
check fail 1.2.0 'stable promotion rejects edited newest candidate'
printf '## [Unreleased]\n## [1.3.0]\n- Different section.\n' > CHANGELOG.md
git add CHANGELOG.md
git commit -qm missing-section
git tag v1.2.0-rc.3
cp "$root/original.md" CHANGELOG.md
check fail 1.2.0-rc.4 'predecessor tag without its section fails closed'
git checkout -q --orphan unrelated
git rm -q -rf .
printf '# Changelog\n## [Unreleased]\n## [2.0.0]\n- New train.\n' > CHANGELOG.md
mkdir -p deploy/chart
printf 'version: 2.0.0-rc.1\n' > deploy/chart/Chart.yaml
git add .
git commit -qm unrelated
check pass HEAD 'unreachable RC tags do not freeze an unrelated train'
echo "changelog-rc-bullets: $pass checks passed"
