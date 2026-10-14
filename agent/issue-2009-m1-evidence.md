# Issue 2009 M1 regression evidence

Worker and web M1 regressions are enabled and intentionally red until M2 and M3 respectively.
The approved additive web fields are optional `pending_outcome_count` and
`pending_overflow`. No aggregate age or membership field is required.
The component is `web/src/components/WorkerPendingOutcomeBadge.tsx`.
The worker evidence below is retained; the fresh web archive results follow it.

Candidate production revision: `baa879cddab4ad52be6b50d4a7a70ce43601120b`.
Pinned baseline: `25d59524d33d2b74156cad143956d04d9ca509e8`.
For the original worker evidence, candidate means current branch plus only the
new worker regression block. Baseline means the pinned archive plus the identical
candidate worker test file. Fresh web measurements below use archives of both SHAs.
Runtime: Node `v24.20.0`, Vitest `4.1.11`.

## Assertions and actual results

Six new worker tests fail with `AssertionError [ERR_ASSERTION]`, not compilation,
import, setup or timeout failures, on both revisions:

- `build` and `buildRegisterSnapshot`: one pending-lister capture; preserve
  identities A/generation 2, A/generation 3, B/generation 7 in `pending_all`,
  projecting only run_id/claim_generation. Actual `pending_all`: undefined.
- Both builders after retirement: explicit `pending_all: []`. Actual: undefined.
- Cap 0: six complete builds, active stays empty and overflow/claim pause stay true.
  Actual coverage: six undefined values instead of six full identity lists.
- Cap 2: seven builds retain fixed blocked A plus rotation B,C,D,B,C,D,B, with
  exactly two terminal-pending active entries and overflow/claim pause true.
  Actual coverage: seven undefined values instead of seven full A,B,C,D lists.

Every preceding assertion on capture count, duplicate-run active behavior,
register emptiness, rotation, caps and claim closure passed before the coverage
assertion failed. Existing tests remain unchanged: 16 worker tests and 6 badge
tests pass in each focused existing selection. No API lease logic was changed or
validated here; API duplicate rejection remains for M2.

## Exact selections, status, logs and elapsed wall time

All paths below are relative to the current worktree root. Set `root=$(pwd)`
there. Each row used the logging wrapper below, with the row's command in place
of `<selection>`. Log files are scratch-only.

| Selection | Exact command | Exit | Wall seconds | Totals | Scratch log |
|---|---|---:|---:|---|---|
| Candidate worker new-only | `(cd "$root/agent" && node --import tsx --test --test-timeout=120000 --test-name-pattern='issue 2009' test/active-run-registry.test.ts)` | 1 | 0.642658882 | 0 pass, 6 fail | `.uzi/scratch/gate-log.7rbc7P` |
| Baseline worker new-only | `(cd "$root/.uzi/scratch/snap-issue-2009-m1/agent" && node --import tsx --test --test-timeout=120000 --test-name-pattern='issue 2009' test/active-run-registry.test.ts)` | 1 | 1.020569271 | 0 pass, 6 fail | `.uzi/scratch/gate-log.OULfNX` |
| Candidate worker existing | `(cd "$root/agent" && node --import tsx --test --test-timeout=120000 --test-skip-pattern='issue 2009' test/active-run-registry.test.ts)` | 0 | 0.608325511 | 16 pass, 0 fail | `.uzi/scratch/gate-log.gNtmXD` |
| Baseline worker existing | `(cd "$root/.uzi/scratch/snap-issue-2009-m1/agent" && node --import tsx --test --test-timeout=120000 --test-skip-pattern='issue 2009' test/active-run-registry.test.ts)` | 0 | 0.594302251 | 16 pass, 0 fail | `.uzi/scratch/gate-log.a2t6kB` |
| Candidate badge existing | `(cd "$root/web" && npx vitest run src/components/WorkerPendingOutcomeBadge.test.tsx -t '^(?!.*issue 2009)')` | 0 | 5.364514482 | 6 pass, 0 fail | `.uzi/scratch/gate-log.OkWMoP` |
| Baseline badge existing | `(cd "$root/.uzi/scratch/snap-issue-2009-m1/web" && npx vitest run src/components/WorkerPendingOutcomeBadge.test.tsx -t '^(?!.*issue 2009)')` | 0 | 5.018721253 | 6 pass, 0 fail | `.uzi/scratch/gate-log.11uyAN` |
| Candidate worker file lint | `(cd "$root/agent" && npx oxlint --config .oxlintrc.json -D correctness --report-unused-disable-directives-severity=error test/active-run-registry.test.ts)` | 0 | 1.193706277 | 0 warnings/errors | `.uzi/scratch/gate-log.Psh2Eg` |

New-only worker selections both report cancelled/skipped/todo = 0.
These are local sequential process wall times, including launcher overhead,
measured once per selection; they are not CI job/critical-path measurements.
Existing selections do not measure the cost of the new regressions.
Badge timings include npx/Vitest startup; test bodies were 102ms and 121ms.

```sh
root=$(pwd)
scratch=.uzi/scratch
log=$(mktemp "${scratch:?}/gate-log.XXXXXX")
start=$(date +%s%N)
rc=0
<selection> > "$log" 2>&1 || rc=$?
end=$(date +%s%N)
echo "EXIT=$rc" >> "$log"
echo "ELAPSED_NS=$((end-start))" >> "$log"
printf 'LOG=%s\n' "$log"
test "$rc" -eq 0
```

## Baseline setup and limits

Executed from the worktree root, no nested worktree, install or checkout:

```sh
set -o pipefail
sha=$(git rev-parse 25d59524d33d2b74156cad143956d04d9ca509e8)
snap=.uzi/scratch/snap-issue-2009-m1
rm -rf "$snap" && mkdir -p "$snap" && git archive "$sha" | tar -x -C "$snap"
root=$(pwd)
cp agent/test/active-run-registry.test.ts .uzi/scratch/snap-issue-2009-m1/agent/test/active-run-registry.test.ts
ln -s "$root/agent/node_modules" .uzi/scratch/snap-issue-2009-m1/agent/node_modules
ln -s "$root/web/node_modules" .uzi/scratch/snap-issue-2009-m1/web/node_modules
sha256sum agent/package-lock.json .uzi/scratch/snap-issue-2009-m1/agent/package-lock.json web/package-lock.json .uzi/scratch/snap-issue-2009-m1/web/package-lock.json
```

Setup commands exit 0. The pinned and current agent lockfiles match SHA256
`59bfe0c845a84f8a0a3bd630fc40c792d383fdd12cb40d2029fc79f8f554032b`;
web matches `218a9ab3ddbec1986c8449f79a65dfe197bbc3af75b677c5cb3640b0c991a18d`.
Dependencies came from the provisioned runtime; no installation was run.
Archive production files and web harness needed no compatibility adaptation.
In that original setup, only the worker test file was overlaid; no baseline-only source adaptation.
Git commands were run in the real checkout, never inside the archive.

No full suites, repo/component gates, build, typecheck, dead-code checks,
CI, Kubernetes or end-to-end scenarios were run (parallel test-only scope).
Focused file lint passed. `git diff --check -- agent/test/active-run-registry.test.ts agent/issue-2009-m1-evidence.md` exited 0 (scratch log `.uzi/scratch/gate-log.57aFX8`; Git does not include the untracked evidence file in that diff). No formatter recipe applies to these TS test edits.
No shipping source, dependencies, workflows or shared fixtures were edited.
No commits or staging: main owns integration/commit. A foreign untracked file,
`api/internal/workersvc/issue_2009_pending_health_livedb_test.go`, was observed
and left untouched. Scratch logs are not intended for tracking.

## Web M1 follow-up: approved aggregate contract

Only `web/src/components/WorkerPendingOutcomeBadge.test.tsx` and this evidence
document changed in this follow-up. Worker tests and API files were untouched.
The fixtures use a structural cast to the component's existing worker prop;
there are no future DTO type imports or production edits.

Seven new cases exercise the rendered badge:

- Overflow with authoritative zero and empty reported rows: exact
  `0 confirmed pending; more may be pending`.
- Overflow with authoritative three versus one listed pending row: exact
  `3 confirmed pending; more may be pending`.
- Overflow false: authoritative one versus two listed, and three versus one
  listed, use ordinary singular/plural labels.
- Overflow false and authoritative zero suppress a listed pending row's badge.
- Count absent and overflow true fall back to listed pending rows, excluding
  live rows: exact `1 confirmed pending; more may be pending`.
- Known per-row generation and pending age remain in the title with an
  aggregate count (including equal aggregate/list counts). The badge has no
  aggregate oldest annotation; the test does not remove per-row ages.

On each archive, the first six cases fail at rendered-output assertions:
five missing-label TestingLibraryElementErrors and one AssertionError for the
unexpected badge at authoritative zero. The per-row title case passes.
No compilation, import, setup, timeout or missing-module failures occurred.
The unchanged six existing cases pass on both archives.

### Fresh archive setup

Setup log `.uzi/scratch/gate-log.m23JHr`: exit 0, 4.785124478s.
From the real worktree root, with `set -o pipefail`, resolve each SHA with
`git rev-parse`, then execute these foreground commands:

```sh
root=$(pwd)
candidate=$(git rev-parse baa879cddab4ad52be6b50d4a7a70ce43601120b)
baseline=$(git rev-parse 25d59524d33d2b74156cad143956d04d9ca509e8)
mkdir .uzi/scratch/snap-issue2009-web-m1-codex-candidate
git archive "$candidate" | tar -x -C .uzi/scratch/snap-issue2009-web-m1-codex-candidate
mkdir .uzi/scratch/snap-issue2009-web-m1-codex-pinned
git archive "$baseline" | tar -x -C .uzi/scratch/snap-issue2009-web-m1-codex-pinned
cp web/src/components/WorkerPendingOutcomeBadge.test.tsx .uzi/scratch/snap-issue2009-web-m1-codex-candidate/web/src/components/WorkerPendingOutcomeBadge.test.tsx
cp web/src/components/WorkerPendingOutcomeBadge.test.tsx .uzi/scratch/snap-issue2009-web-m1-codex-pinned/web/src/components/WorkerPendingOutcomeBadge.test.tsx
cmp web/package-lock.json .uzi/scratch/snap-issue2009-web-m1-codex-candidate/web/package-lock.json
cmp web/package-lock.json .uzi/scratch/snap-issue2009-web-m1-codex-pinned/web/package-lock.json
sha256sum web/package-lock.json .uzi/scratch/snap-issue2009-web-m1-codex-candidate/web/package-lock.json .uzi/scratch/snap-issue2009-web-m1-codex-pinned/web/package-lock.json
ln -s "$root/web/node_modules" .uzi/scratch/snap-issue2009-web-m1-codex-candidate/web/node_modules
ln -s "$root/web/node_modules" .uzi/scratch/snap-issue2009-web-m1-codex-pinned/web/node_modules
node --version
(cd web && npx --no-install vitest --version)
```

All three lockfiles match the web SHA256 recorded above. Runtime remains
Node v24.20.0 / Vitest 4.1.11 on linux-x64. Both archives keep production
files unchanged; only the identical test file is overlaid. Neither archive
reuses another agent's snapshot. Git commands run only in the real worktree.
Provisioned node_modules are symlinked; nothing was installed. Lock equality
and successful focused runs do not constitute a full dependency audit.

### Focused web commands and results

Each row ran once, sequentially in the foreground, using the earlier wrapper
that records EXIT and ELAPSED_NS. Paths are relative to the real worktree root;
`root` is its absolute path. These times include npx/Vitest launch overhead,
not CI timing. Filtered-out tests are intentional selections.

| Selection | Exact command | Exit | Wall seconds | Totals | Scratch log |
|---|---|---:|---:|---|---|
| Candidate web new | `(cd "$root/.uzi/scratch/snap-issue2009-web-m1-codex-candidate/web" && npx --no-install vitest run src/components/WorkerPendingOutcomeBadge.test.tsx -t 'issue 2009')` | 1 | 5.212021382 | 6 fail, 1 pass, 6 skipped | `.uzi/scratch/gate-log.q4hc8q` |
| Pinned web new | `(cd "$root/.uzi/scratch/snap-issue2009-web-m1-codex-pinned/web" && npx --no-install vitest run src/components/WorkerPendingOutcomeBadge.test.tsx -t 'issue 2009')` | 1 | 4.916258150 | 6 fail, 1 pass, 6 skipped | `.uzi/scratch/gate-log.hInH7T` |
| Candidate web existing | `(cd "$root/.uzi/scratch/snap-issue2009-web-m1-codex-candidate/web" && npx --no-install vitest run src/components/WorkerPendingOutcomeBadge.test.tsx -t '^(?!.*issue 2009)')` | 0 | 5.721013678 | 6 pass, 7 skipped | `.uzi/scratch/gate-log.EkM5jM` |
| Pinned web existing | `(cd "$root/.uzi/scratch/snap-issue2009-web-m1-codex-pinned/web" && npx --no-install vitest run src/components/WorkerPendingOutcomeBadge.test.tsx -t '^(?!.*issue 2009)')` | 0 | 4.814663683 | 6 pass, 7 skipped | `.uzi/scratch/gate-log.pDcEFj` |
| Web file lint | `(cd "$root/web" && npx --no-install oxlint --config .oxlintrc.json --report-unused-disable-directives-severity=error src/components/WorkerPendingOutcomeBadge.test.tsx)` | 0 | 1.035014871 | 0 warnings/errors | `.uzi/scratch/gate-log.zkL57i` |

New selection test bodies total 150ms candidate / 163ms pinned; existing
selection bodies total 97ms / 84ms. Full-suite runtime impact is unmeasured.

Read-back verification confirms the original evidence filenames are correct:
`gate-log.11uyAN` is baseline badge existing (6 pass, exit 0,
ELAPSED_NS=5018721253); `gate-log.gNtmXD` is candidate worker existing
(16 pass, exit 0, ELAPSED_NS=608325511). No filename correction was warranted.

No full gates, suites, builds, installs, typechecks, CI, browser, Kubernetes,
API, CLI or end-to-end commands were run in this follow-up. These are M1 red
tests against unchanged production, not evidence of an M2 implementation.
No staging or commits. Other agents' uncommitted worker/API changes remain
untouched. Only this follow-up's two archive snapshots are removed after review;
logs remain scratch-only.

Final scoped diff/overlay/status check and removal of owned archives:
`.uzi/scratch/gate-log.VUqS4p`, exit 0, 0.794340065s. The test copies
matched byte-for-byte before removal. `git diff --check -- web/src/components/WorkerPendingOutcomeBadge.test.tsx agent/issue-2009-m1-evidence.md`
passed; Git excludes the untracked evidence document from that diff.
The later evidence-only additions record this result. Final owned status:
badge test modified and evidence document untracked, intentionally uncommitted
for main. Foreign worker/API changes were present before and after this work.
