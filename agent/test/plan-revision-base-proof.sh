#!/usr/bin/env bash
# Run from the real checkout. Exports contain no .git; only test fixtures overlay base.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
cd "$root"
mkdir -p .uzi/scratch
sha=$(git rev-parse 'b0893dce^{commit}')
snap=$(mktemp -d .uzi/scratch/plan-revision-base.XXXXXX)
trap 'rm -rf "$snap"' EXIT
git archive "$sha" | tar -x -C "$snap"
cmp agent/package-lock.json "$snap/agent/package-lock.json"
ln -s "$root/agent/node_modules" "$snap/agent/node_modules"
# Only fake API support is newer than base: ownership evidence and atomic publication
# receipts model approved M1. Neither executor nor runner nor retry policy is overlaid.
cp agent/test/fake-api.ts agent/test/runner-plan-revision-provider.test.ts agent/test/plan-revision-provider-fixture.ts "$snap/agent/test/"
log=$(mktemp .uzi/scratch/gate-log.XXXXXX)
rc=0
(
  cd "$snap/agent"
  timeout 150s env -u TMPDIR node --import tsx --test --test-timeout=120000 \
    --test-name-pattern='real 429 inner exhaustion redrives revision without operator input' \
    test/runner-plan-revision-provider.test.ts
) > "$log" 2>&1 || rc=$?
echo "EXIT=$rc" >> "$log"
printf 'BASE=%s\nEXPORT=%s\nLOG=%s\n' "$sha" "$snap" "$log"
# An expected base red must be a behavioral assertion, never a loader/compiler failure.
test "$rc" -eq 1
rg -q 'real provider exhaustion must enter runner redrive backoff' "$log"
rg -q 'tests 2' "$log"
rg -q 'fail 2' "$log"
