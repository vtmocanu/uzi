# Issue 2009 M2 worker evidence

Worker slice on M1 HEAD `19c7e1ebe2b8c795a2f6840de55bb4204a7b1f20`, with the
uncommitted changes in this handoff. Main owns integration, review and commits.

`ActiveSnapshot.pending_all` is optional for wire compatibility. Both registry
builders emit the full once-captured terminal-pending identity list, including
separate generations and explicit `[]`. Only `run_id` and `claim_generation`
are projected; finalize-pending records are excluded. No second lister call,
capability gate, inferred ownership identity or lifecycle authority was added.

Selection, rotation, active caps, overflow, nonce stamping, epoch and the claim
gate are unchanged. Existing duplicate-run active construction is deliberately
preserved: the API must accept the enclosing snapshot before pending_all can
provide coverage; rejected snapshots provide none. The worker never truncates
pending_all, including above the API identity-count limit. The API must drop the
whole over-limit list; the wire-contract comment records this limit. API
implementation and validation belong to main's separate slice.

The M1 test assertions and characterization checks are retained. Only the stale
regression label and explanatory comments changed. Historical M1 red evidence
in `agent/issue-2009-m1-evidence.md` remains unchanged.

## Focused local checks

Commands run once, in the foreground, from the worktree's agent directory,
using provisioned dependencies. Logs are relative to the worktree root.
Each check used `mktemp .uzi/scratch/gate-log.XXXXXX`, redirected stdout/stderr,
recorded exit status and `date +%s%N` elapsed wall time, then read the log.

| Command | Exit | Wall seconds | Result | Log |
|---|---:|---:|---|---|
| `node --import tsx --test --test-timeout=120000 test/active-run-registry.test.ts` | 0 | 1.232004986 | 22 pass, 0 fail/cancelled/skipped/todo; includes all six issue 2009 regressions and 16 existing tests | `.uzi/scratch/gate-log.Z7n9Yt` |
| `npx --no-install oxlint --config .oxlintrc.json -D correctness --report-unused-disable-directives-severity=error src/protocol.ts src/active-run-registry.ts test/active-run-registry.test.ts` | 0 | 1.177832953 | 0 warnings/errors on 3 files | `.uzi/scratch/gate-log.ITtfpT` |

M1 measured separate candidate selections: new-only failed six assertions in
0.642658882 seconds; existing-only passed 16 tests in 0.608325511 seconds.
This post-fix measurement runs the entire file once. It is not a matched
new-only before/after comparison and supports no suite-growth, CI-job or
critical-path inference. No CI timing was measured.

Tracked consumer searches for pending_all and ActiveSnapshot identified the
API integration and client transport; a search for active_snapshot,
pending_overflow and pending_all across e2e/phases, e2e/lib.sh,
e2e/lib.test.sh and e2e/forge-fake found no matching expectations to update.
No full repo/module gates, full suites, builds, whole-repo typecheck, dead-code,
API checks, Kubernetes or end-to-end runs were performed in this worker slice.
No agent TypeScript formatter slot exists; file lint is recorded above.

Only the four authorized agent paths were edited. Concurrent API changes were
present at entry and left untouched. No commits, dependency installs, dependency
edits, workflow edits, migrations, M3 displays/DTOs or M4 ADR/changelog work.
