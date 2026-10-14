# Issue 2009 M4 worker evidence

M4 adds two real-Outbox integration tests in `test/pending-all-capture.test.ts`
and documents the shared fixture. No shipping source changes or commits.
The lead-owned duplicate-active JSON was read and left unchanged.
M1–M3 evidence files remain historical records and were not edited.

## Real journals and wire assertions

Each test creates its own mkdtemp directory under the command's absolute
scratch TMPDIR, initializes Outbox, journals A/generation 1 and 2 with
phase running, messages-through sequence 0, and canonicalized completed body
with report_md fixture. Limits: runMaxBytes 67108864, maxBytes 536870912,
retentionMs 604800000. A second Outbox initializes from those same authenticated
filesystem journals; no pending-lister fixture or journal injection is used.
Test cleanup removes only its exact mkdtemp path.

The cap-2 epoch-1 build's JSON serialization equals the shared fixture JSON
(structural equality after JSON parsing). Duplicate active IDs remain present,
both distinct generation identities appear in pending_all, only run_id and
claim_generation are projected, and the lister is captured once. No register
nonce is added. Duplicate active IDs still imply API whole-snapshot rejection:
the standalone complete pending list gives no coverage when enclosing rejection
occurs. That API assertion belongs to the separately owned API tests.

The cap-0 test checks exact register and heartbeat snapshots before retirement,
after generation 1 retires, and after generation 2 retires. Both builders keep
empty active arrays and full pending identities, then remove the retired identity,
then emit explicit [] with overflow false. Each builder captures the lister once.
Both retirement calls returned true.

## Matched original M1 benchmark

Runtime Node v24.20.0. Current production archive:
`ef47589b0a679ac4a2ce7dd2f4e64293048d2a0f`.
Pinned production archive: `25d59524d33d2b74156cad143956d04d9ca509e8`.
Both archives received the identical ORIGINAL M1 registry test file from
`19c7e1ebe2b8c795a2f6840de55bb4204a7b1f20`; the worktree registry test was
not overwritten. Helpers came unchanged from each archive; no compatibility
adaptation was needed. New M4 tests were not part of the benchmark.

Setup ran from the real worktree root, with set -o pipefail. Each SHA was
resolved using git rev-parse before git archive extraction into fresh owned
scratch slots `snap-issue2009-m4-worker-current` and
`snap-issue2009-m4-worker-pinned`. git show of the original commit's
agent/test/active-run-registry.test.ts supplied each overlay. cmp verified both
archive agent lockfiles equal the worktree lockfile; SHA256 for all three:
`59bfe0c845a84f8a0a3bd630fc40c792d383fdd12cb40d2029fc79f8f554032b`.
Each archive agent/node_modules symlinked the provisioned worktree dependencies.
Setup exit 0; no dependency installs and no Git commands inside exports.

From the real root, set root to the absolute worktree path. Exact benchmark
commands (revision = current or pinned):

```sh
(cd "$root/.uzi/scratch/snap-issue2009-m4-worker-$revision/agent" && node --import tsx --test --test-timeout=120000 --test-name-pattern='issue 2009' test/active-run-registry.test.ts)
(cd "$root/.uzi/scratch/snap-issue2009-m4-worker-$revision/agent" && node --import tsx --test --test-timeout=120000 --test-skip-pattern='issue 2009' test/active-run-registry.test.ts)
```

The actual bounded loop ran current then pinned, name then skip, using the
equivalent single argument "--test-$selection-pattern=issue 2009". Independent
selections continued after the expected pinned regression failure; each ran once
in the foreground and recorded its own exit status.

| Selection | Exit | Pass/fail | Wall seconds | Node duration ms | Scratch log |
|---|---:|---|---:|---:|---|
| Current original M1 only | 0 | 6/0 | 0.530543764 | 451.422541 | gate-log.HtTzrw |
| Pinned original M1 only | 1 | 0/6 | 0.592661347 | 503.576988 | gate-log.A1FYij |
| Current original existing | 0 | 16/0 | 0.398291115 | 311.465263 | gate-log.VOmT76 |
| Pinned original existing | 0 | 16/0 | 0.496045383 | 406.602060 | gate-log.46PNjy |

All cancelled/skipped/todo summaries are zero. Pattern-excluded tests are not
reported as skipped by this Node runtime; they did not execute. The M1 selections
execute six tests and existing selections execute sixteen. All six baseline
failures are ERR_ASSERTION for undefined pending_all: both builders' complete
generation projection, both explicit-empty outputs, cap-zero completeness across
six builds, and over-cap completeness across seven rotating builds. No import,
setup, missing-module or timeout failure. Current passes those same assertions.

Current-minus-pinned wall deltas: original new-only -0.062117583 seconds;
original existing -0.097754268 seconds. One sample per selection, sequential
local runs including launcher overhead and baseline assertion-reporting cost.
Logs retain individual test body timings. These measurements do not establish
CI runtime, queue/cache effects, full-suite growth or critical-path impact.
The new M4 journal tests are measured separately below.

## Focused validation

Exact commands, run from the real root:

```sh
(cd "$root/agent" && TMPDIR="$root/.uzi/scratch" node --import tsx --test --test-timeout=120000 test/pending-all-capture.test.ts)
(cd "$root/agent" && npx oxlint --config .oxlintrc.json -D correctness --report-unused-disable-directives-severity=error test/pending-all-capture.test.ts)
```

| Check | Exit | Result | Wall seconds | Scratch log |
|---|---:|---|---:|---|
| Real-journal file | 0 | 2 pass, 0 fail/cancelled/skipped/todo | 1.103053439 | gate-log.5xW7h1 |
| File lint | 0 | 0 warnings/errors | 1.265410437 | gate-log.Idmx5j |

Node file duration 1014.292536 ms; journal test bodies 139.647122 ms and
61.517205 ms. Oxlint reports 22 ms. Logs live under .uzi/scratch and remain
untracked. Every focused invocation used this foreground wrapper from the root:

```sh
scratch=.uzi/scratch
log=$(mktemp "${scratch:?}/gate-log.XXXXXX")
start=$(date +%s%N)
rc=0
<command> > "$log" 2>&1 || rc=$?
end=$(date +%s%N)
echo "EXIT=$rc" >> "$log"
echo "ELAPSED_NS=$((end-start))" >> "$log"
printf 'LOG=%s\n' "$log"
test "$rc" -eq 0
```

Whitespace verification: the first no-index diff-check against /dev/null returned
1 for the added test file with no diagnostic output (gate-log.0MKnqS,
0.003992169 seconds). This is Git's added-file difference status, not a whitespace
finding; that chained command stopped before checking the documents. The remaining
documents were checked separately with the same no-index check, treating 0/1 plus
empty diagnostics as clean; wrapper exit 0 (gate-log.hRtuRS). No check was rerun
just to read its output. This evidence file was subsequently updated to record
that distinction and its final version checked separately.

All result logs were read after execution. Owned archive snapshots were removed
after measurement; logs retained for main. Full suites, builds, repo/component
gates, typecheck, dead-code, CI, k8s and e2e were not run in this parallel
test-only scope; main owns canonical integration and validation.
Foreign API test changes were observed and left untouched. No staging/commits.
