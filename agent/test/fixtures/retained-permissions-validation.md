# Retained object permissions: validation record (#3059)

## M1: observed baseline red (2026-10-18)

Production code was unchanged from `1edf88d3b6051cfbf3c0c47ac8bb004b68af67f6`.
The test and inventory revision is the M1 commit containing this record.
No permission-readiness implementation existed during this proof.

The base worker image was built from this checkout's checked-in Dockerfile:
`docker build --progress=plain -f agent/templates/base/Dockerfile -t recovery-3059-base:local .`.
Build exit: 0. Image ID:
`sha256:5e48b49cc6f85d2f2fc7d75c7a5868685b338b2d3614e7f5208e4e50032a3d5f`.
The optional source-version build argument was not set, so baked BUILD_INFO says
`unknown`; source provenance here is the checkout and read-only source mounts,
not a claim that the image advertises the commit. The supervisor and entrypoint
came from the same unmodified production checkout. Image construction and export
were cold costs and are excluded from the proof-step comparison below.

Versioned required-lane command:

```sh
WORKER_UID_IMAGE=recovery-3059-base:local WORKER_UID_SKIP_BUILD=1 bash e2e/worker-uid/run.sh
```

The runtime used the production root-start entrypoint and hardened capability
drop. Fixture assertions established worker UID 10001, runner UID 10002, command
UID 10003, runner group 10002, actual owner-only directories 2700 and files 0400,
worker/runner EACCES and successful command reads, and command denial of both
worker-private and provider-private dummy files. Fresh commits and their unique
blob were absent from worker storage before recovery. Sources were self-contained;
there was no alternate through which a captured tip could mask unreadable source
objects. Resumed capture instead began with an already-valid bare tip and pin.

The six fresh-source positive leaves (loose, packed, and all four mixed
worker/runner hardlink variants) failed at source-read recovery:
`recovery capture: worktree status unreadable (cannot assert clean)`, then bounded
exhaustion without successor adoption. The captured-tip ordering leaf reached
successor adoption using the valid pre-existing bare restore tip, then failed its
`source normalized BEFORE resumed-tip verification` assertion (`false !== true`):
the source was still unreadable at the resumed verification seam. The
command-owned hardlink mutation case also failed because the new
`hardlinked_mutation` refusal did not yet exist.
The corrupted-source preservation case passed. None of the nine leaves skipped,
errored during startup, or timed out. All 38 existing worker leaves and both
existing command leaves passed.

Local retained logs (scratch is not durable across a fresh runner):
- Image build: `.uzi/scratch/gate-log.nN3iM1`, exit 0.
- Existing-lane baseline: `.uzi/scratch/gate-log.dJ3r3f`, exit 0.
- Final M1 required-lane red: `.uzi/scratch/gate-log.4VBy5x`, exit 1.
- Source JUnit inventory/result report: `.uzi/scratch/worker-uid.AurKLU/`.
- Final cheap agent/scoped-project checks: `.uzi/scratch/gate-log.rCVCgc`, exit 0.
- Shell lint, Docker-free harness checks and existing inventory check:
  `.uzi/scratch/gate-log.JBxlgm`, exit 0.

Local step timing with the same image, serial execution and read-only source mounts:

| Step | Before added leaves | M1 expected-red revision |
|---|---:|---:|
| Worker proof | 39 s, exit 0 | 142 s, exit 1 |
| Command proof | 26 s, exit 0 | 31 s, exit 0 |
| Image layout/preflight | 4 s | 3 s |

The worker-proof difference is +103 s on this local expected-red run, not a
green implementation measurement. Command-proof variation is +5 s on unchanged
proofs. No CI queue, rerun or critical-path timing was measured. M3 must record
the final green timing before claiming runtime impact.

Earlier failed attempts were fixture setup failures (unsafe group-writable bare
config, inaccessible shared scratch, and a worker-private fake process table).
They are not counted as the baseline red. The scoped empty process table contains
no provider roots; all real fixture command roots use the production supervisor
and require child exit plus clean disposal. Test-only scratch permissions are
restored. This does not replace production quiescence checks.

Positive fixtures stop only after the real `prepareRecoverySuccessor` has
verified and durably handed off attribution to a fresh successor. They then
inject a test stop before provider/model admission. Assertions require the
ready-for-model journal, retained predecessor, exact tip/tree/blob, closure and
hardlink preservation. Those post-repair assertions remain unproven at M1.
Negative mutation refusal and final non-skipped green are also M2/M3 work.

## M1 review and full-gate triage

Review of `1edf88d3..8b199f4a` identified missing positive assertions for retained
predecessor object bytes/identities, source refs and recovery pins. The following
repair records the original command-owned file snapshots and checks them after
adoption, allowing only group-read mode additions; it also checks source refs,
worktree bytes, directory modes and the exact retained episode pin. The same
repair corrects the resumed-tip baseline evidence above. These additional
post-adoption assertions require the M3 green proof; they are not claimed green
on the baseline.

`task gate:agent` on `8b199f4a` failed: recorded task exit 201, log
`.uzi/scratch/gate-log.kkWiVY`. Static stages passed; unit results were 16,086
passed, one failed, 15 skipped, zero cancelled, in 3,744 seconds. The sole failure
was the existing `wall-during-dispose settles eligible Bash and retires roots
before park/report` case in `codex-executor.test.ts`, reporting unconfirmed
command cleanup. M4 did not run after this unit failure.

The smallest selected test passed on both candidate `8b199f4a` (log
`.uzi/scratch/gate-log.0IuWHw`) and an isolated base `1edf88d3` export using the
same provisioned dependencies and hermetic preload (log
`.uzi/scratch/gate-log.tqvBba`), exit 0 and one test each, no skips. This comparison
is inconclusive: it does not establish that the full-gate failure is unrelated.
Final delivery still requires a successful full agent gate.

## M2: run-local terminal cleanup regression

The human selected a run-only terminal failure for unconfirmed readiness-helper
cleanup, including startup cleanup, and explicitly kept the worker quarantine
policy unchanged. The accepted interruption/persistence limit is recorded in
ADR-1197; there is no new durable pre-launch marker or command-UID scan.

The final regression file was copied into an isolated export of production
`59936bbc1d8e1a36490ce9891e3d72c88a4ad88b`, with the provisioned dependencies
linked unchanged. The primary worktree was not reset or replaced. Command:

```sh
cd agent
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='runner first readiness result then clean helpers' \
  test/git-retained-readiness.test.ts
```

Baseline log `.uzi/scratch/gate-log.PGfr6B`, exit 1: seven tests,
four failed and three passed, no skips/cancellations. Unclean disposal, throwing
disposal and unconfirmed startup cleanup each reached the injected fault and
failed because two helpers launched instead of one. Cleanup at the deadline
failed because its persisted blocker was `budget_exhausted`. Clean disposal,
verified startup cleanup and ordinary helper-refusal controls all passed.

After the fix, `.uzi/scratch/gate-log.NO5zkw`, exit 0: the entire readiness
file plus `git-recovery-claim-window.test.ts` passed all 92 tests, no skips or
cancellations. Terminal cases assert one helper launch, the parsed persisted
`readiness_cleanup_unconfirmed` blocker, no successor preparation/adoption or
model start, and preserved dirty bytes, source refs, journal attribution, pin and
custody. Positive controls complete the real successor preparation and inspect
its durable ready-for-model handoff before a controlled test stop. Further
focused cases cover cleanup overriding an ordinary refusal and preserving its
classification when an operation aborts during disposal. A persisted cleanup
blocker remains blocked across a later claim and deadline expiry.

These are host runner/fake-supervisor fault tests, not replacements for the
separate real distinct-UID permission and credential proofs. No surviving
process was manufactured or claimed. Final M3 delivery still requires its
complete integration gates and real-UID proof.
