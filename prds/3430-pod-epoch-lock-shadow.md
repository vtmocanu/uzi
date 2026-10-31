# PRD #3430: Pod-epoch lock and shadow reclaim verdicts on Docker-wired workers

**Status**: Draft (architecture and lead/buddy reviewed; contract corrections approved 2026-10-25; M1 source implemented and reviewed, full agent gate red and final-head image proofs pending; M2a controller handoff implemented and verified; M2b worker shadow evaluation implemented and reviewed; M2c local docs/checks recorded below, frozen final verification pending; M3 pending). PRD A of two for #3329's disposal with the writer-exclusion proof PRD #1860 M0 recommended (`prds/1860-pod-epoch-writer-exclusion.md`). This PRD deletes **nothing**: it ships the epoch lock, the controller handoff and a shadow verdict. PRD B (#3431) adds deletion once this PRD's shadow acceptance passes. **Dependency** #3421 (`dind-meter` SIGTERM): its source fix is present; current validation blockers are recorded below. The CRI-O source audit is committed beside it in `prds/3430-pod-epoch-crio-audit.md`. PRD #3431 M1a extends the shadow (v2 evidence).

## Implementation evidence (2026-10-25–26)

M0 is complete; its approved contract amendment is committed. M1 source is implemented through `fa911d436f1711c1f23c8b2592b9aa94b3473fb3`: lifetime epoch ownership, protected initialization, pod-entry records, bounded durable consumption, bootstrap scratch isolation and data-root posture preservation. Real-image investigation found and repaired ownership-before-chmod failures without FOWNER (`60069720`) and a non-traversable baked helper parent directory (`41886061`). Both image templates now restore that directory to root:root 0755; the image preload checks baked metadata and imports the actual baked wrapper. At the M1 source boundary, the worker's tentative shadow EX path was disabled; [M2b](#m2b-worker-shadow-evidence-2026-10-26) implements that path. This branch adds no attempt-tree deletion.

The earlier M1 full controller gate passed at `388f6d77`; the M2a results below supersede that result for the changed controller source. At `cd0218a3`, agent typecheck, lint, knip, worker-UID typecheck and 65 focused tests passed with zero skips. At `60069720`, lint, both typechecks and 21 native epoch tests passed; independently restoring either old ownership/mode order made the new regression fail. At `41886061`, preload syntax, worker-UID typecheck and all 24 calibration-producer tests passed with zero skips. The maintenance-bootstrap E5 guard remains unchanged; static imports and an exact token-posture mutation target repair the two M1 regressions found by broader testing.

`fa911d43` repairs only the old fsGroup test assertion to check the participating-Docker `/data` exception. The old assertion passed on M0 but failed on candidate `4567544f` (135/136 focused tests passed); the corrected named test and typecheck passed (`.uzi/scratch/gate-log.AMKD9H`). Reviewer, auditor and tester scoped reviews of `4567544f..fa911d43` found no mandatory defects. The reviewer also verified the `d3a00429..4567544f` proof-script repair; the corresponding auditor and tester reviews were clean. Prior scoped reviews at `60069720` and `41886061` were clean; the cumulative audit through `60069720` covered the previously cancelled `388f6d77..cd0218a3` range.

The earlier full agent gate at `388f6d77` reached its 120-minute command deadline; inherited `UZI_SQLC_DIR` and command UID/GID 10003 versus checkout/scratch UID/GID 10002 exposed baseline failures. Matching-owner runs at `fa911d43` now provide the current red result. The first UID/GID 10002 attempt (`.uzi/scratch/gate-log.bWw8BJ`) also reached the 120-minute command deadline, with no exit status and no M4. Its remaining owned container was stopped and removed. That initial test runtime lacked the provided Go module cache, init and local hostname mapping; controls demonstrated baseline and candidate environment failures.

Corrected runs used the existing pinned image `sha256:ebadd032a7e6a1812bc3ebfd2fc10eb58150d31b3f9c413768bab1c776484fa8` only as an ordinary test environment, mounting the real checkout at UID/GID 10002 with a copied installed toolchain and read-only existing Go module cache (no installs). Docker used `--init --hostname uzi-agent-gate --add-host uzi-agent-gate:127.0.0.1`. Go sanitizer controls failed on both M0 and candidate with an empty cache (log suffix `.8eNq3w`), then passed with the cache (log suffix `.tqDe0m`, 59.637/53.874 s wall, respectively). Seed-residue controls passed on both with `--init`; hostname identity Git lookup fell from 5,021 to 14 ms, and both pressure-case controls passed after the mapping. This runtime is not final-image acceptance; no build retry occurred.

| Corrected full agent gate | Result | Unit result | Remaining failures |
|---|---|---|---|
| Four workers, `.uzi/scratch/gate-log.jINYeM` | EXIT 201; 5,320 s; M4 not reached | 691 files, 21,522 tests: 21,493 pass, 2 fail, 27 skip; zero real-process skips | Two approval-replay tests hit the fixture's 8 s finish/cancel boundary. Targeted base/candidate controls passed 2/2 and the four-file cohort passed 8/8; comparison is inconclusive, proving neither an unrelated failure nor a source bug. |
| Three workers, `.uzi/scratch/gate-log.sqT6QW` | EXIT 201; 6,505 s; M4 not reached | Same unit counts and zero real-process skips | The two approval tests passed in the actual full run (7,876.923/8,205.249 ms); two memory/deadline tests failed, detailed below. |

No assertions, budgets, filters or production code changed between these full runs. The lead's `git diff` confirmed that the actual source core and both failing test files are unchanged from M0 through `fa911d43`. The checkout remained clean at `fa911d43` after the runs, and owned containers were removed.

Current failures, with attribution still inconclusive:

- `agent/test/codex-executor.test.ts:821`, late-cleanup mode: assertion 992, “cover both pending capture and capture already settled beneath pending executor cleanup,” expected capture completion (`memory.settled` true) at public return but got false. Full-run duration: 2,704 ms. The unchanged 1,500 ms fixture-supplied budget is an actual production boundary. Unmodified targeted base/candidate passed in 3,882/3,831 ms; diagnostics measured capture-to-cleanup at about 1,173 ms on both, with cleanup 217–317 ms before public return. No baseline bug is confirmed; do not classify this as unrelated. Waiting for capture after public return would weaken the assertion and is rejected.
- `agent/test/runner-worker-memory.test.ts:779`, native park ACK with `credential=true`: assertion 920 timed out observing successor admission. Its 5,000 ms observer includes retained recovery/setup; full-run duration was 7,492 ms. Targeted base/candidate passed in 4,176/4,658 ms, with admission diagnostics at 2,790/2,870 ms. This observer timeout is distinct from the actual production capture boundary of 5,000 ms.

After owner-reported storage recovery, pinned-runtime release builds passed at `1fc61c8d` (359 s) and `60069720` (92 s), including util-linux 2.42.3 packaging. The first full worker-UID lane attempt passed 71 worker leaves, two command leaves and three single-UID preflights, then failed all six new epoch leaves on the ownership/mode defect or an unavailable OpenSSL fixture dependency. A focused six-case run with repaired mounted sources passed, but it did not establish final baked-image acceptance: subsequent baked-path checks found the parent-directory defect. Both defects are repaired; the final rebuilt-image lane, actual baked imports under the UID profiles, lead-owned final descriptor proof and private cgroup calibration remain pending.

The full local base build at `41886061` passed every build instruction, including Nix permissions, toolchain, util-linux packaging and the final helper-directory repair, but failed during final image unpack with `ENOSPC` after 1,443 s. Its exported image is `sha256:80210abb84547898940a2e8d256709f97275d304edc938dbb8b6d3e22d338479`; the failed layer is `sha256:3a340eb1494da69b1c111e0ebac0cbd3ac6a45e2c4f714c2eac445ba1f6040c7`. No build retry followed, as the owner required. Streaming archive inspection measured the remaining 48 layers, including the failed layer: exactly 6,313,833,864 regular-file payload bytes, 148,091 regular files and 56,595 directories. Rounding each regular file to 4 KiB gives 6,699,544,576 bytes before directory/filesystem metadata. These are measured archive requirements, not an exact additional-volume allocation: partial extraction, overlay/copy-up behavior, metadata and concurrent users affect peak space. The pre-build available space was 12,725,112 KiB; after failure cleanup it was 11,418,292 KiB, with 2,484,181 inodes available. No further image build or extraction retry will run on this worker. The owner subsequently relocated final-head image acceptance to the lander's storage host, as recorded below; host BusyBox tests and future CI do not replace it.

M1 source is reviewed, but the local full agent gate remains red and M1 cannot be claimed fully verified. The owner directed continuation of the non-image work and M2a/M2b/M2c, retaining in-run static checks, touched and directly affected tests, red/green proofs and audits. The full agent suite is to be checked by exact-head PR CI. This execution direction does not waive the frozen full-gate criteria: a formal partial decision is still required before delivery. M2a controller handoff is implemented and verified as recorded below; [M2b worker evaluation](#m2b-worker-shadow-evidence-2026-10-26) is implemented and reviewed with scoped evidence. M2c closeout and M3 runtime attestation/live acceptance remain pending. Keep this PRD Draft and in place.

The owner relocated M1's real-image proof to the lander's storage host. The lander must build and verify **both base and JVM** images at the **final exact reviewed PR head**, including subsequent M2 work, on a host with sufficient storage. The exact commands and required image-identity relationships are in [the image-proof handoff](3430-pod-epoch-image-proof.md). Exact-head CI running `task test:agent:worker-uid` supplements the lander evidence. Both final-head image proofs remain separately pending; host BusyBox tests, the ordinary gate runtime and future CI do not replace them.

### M2a controller handoff evidence (2026-10-26)

Controller source is implemented through `8dda8084cd03d31d14cd91a4419882713cb065b3`. It provides the strict shared 8 KiB evidence contract and fixtures; bounded unfiltered Docker-namespace LIST/WATCH sessions; predecessor terminality and historical foreign-claim-user checks; live Deployment/ReplicaSet and claim identity binding; session-owned maintenance remint receipts; one UID/resourceVersion-fenced successor annotation request; direct downward-API projection; and the unset/two-value runtime configuration. Only the Docker Role gains pod watch/patch. At the M2a source boundary, the worker consumer and shadow verdict were unimplemented and tentative worker EX was disabled; [M2b](#m2b-worker-shadow-evidence-2026-10-26) implements them. No deletion is added.

Review and gate findings were repaired before closeout: startup timeout now discards a late stream and requires a fresh baseline; declaration rolls accept an otherwise valid predecessor without requiring its old runtime env to match the successor; the initial projection was registered in the secret-mount overlap guard; the advisory repair below moves it outside `/run` to preserve valid Secret overrides. A real HTTP probe demonstrated an oversized PATCH-response gap, fixed by applying the existing 8 MiB response cap. Another real HTTP probe demonstrated client-go automatic Retry-After retries: the unfixed call sent two requests and returned success; `.MaxRetries(0)` now limits the production evidence PATCH to one actual request, retaining the marked response cap and 10 s deadline. Explicit redelivery remains refused, including ambiguous results.

`task gate:controller` passed at `8dda8084` (`.uzi/scratch/gate-log.A8wffU`, EXIT 0): format, vet, build, lint, dead code and the full race/count-one test suite. Kube package time was 52.634 s; total statement coverage was 91.0% (measurement, not a threshold). Earlier failure at `6a851b04` was the missing mount-path registration; the same named test passed on base `07fac261` and failed on that candidate before repair. Focused actual-HTTP RED/GREEN logs cover late startup synchronization (`.ibYF7Y`/`.wz6nUU`), oversized PATCH responses (`.J8lGHp`/`.CNux8r`) and automatic retries (`.tdvGXC`/`.REDdlJ`). The two memory-test files remain unchanged.

Reviewer and tester coverage spans `dfaf60cf..6a851b04` and the committed repair ranges through `8dda8084`. The first auditor attempts were incomplete and are not counted as approval; a subsequent full source audit covered `dfaf60cf..b14bf0db`, followed by clean scoped audits of both later repairs. All mandatory findings are closed. The executed secret scan at `8dda8084` reported EXIT 0, zero tracked-file findings and canaries DETECTED (`.uzi/scratch/gate-log.XU6qVW`); final repo scans remain part of M2c. CI critical-path timing is unmeasured and remains an explicit M2c reporting item.

The lead's isolated terminality mutation at `b14bf0db` passed its baseline and mutant typecheck, then failed the named `TestPodEpochRefusals/137` assertion when exit 137 was allowed in the shared validator (`.uzi/scratch/m2a-terminal-logs-439lb2_k`). Its export was deleted and the primary tree was unchanged; later delivery-only repairs do not change that validator, producer or assertion. Complete offline chart assertions passed on base and candidate with `deploy/values/ci-render.yaml`, stripping only the unused CNPG dependency as the existing helper does. A default-input attempt failed on both for missing Antrea policy; an incomplete-export attempt remained inconclusive until the referenced API/Compose files were included. Full final chart/repo/docs closeout remains in M2c. M1's final-head image proof and frozen full-agent criteria, and M3 live acceptance, are still pending and unwaived.

### M2b worker shadow evidence (2026-10-26)

Worker shadow evaluation is implemented through `f5b95db5495aa44ae33033051fe07fea1963c945`. The strict shared-fixture parser enforces the 8 KiB contract. The environment/consumption peek chooses the first EX lock before full configuration loading; durable consumption and runtime TMPDIR restoration precede writes. Existing startup reapers run before shadow evaluation and admission. GET-only daemon predicates and a healthy `since` replay stream provide a veto/abort guard, never an exclusion acknowledgement; the stream cap is cumulative 64 KiB of actual event bytes. Bounded discovery covers at most 200 physical paths, preserving the operator inspector's independent 64-descriptor default. Evaluation reads fresh API holds and terminal ownership and reuses the full retained-source inspector's authority. It deletes no trees and releases no holds.

Evidence waiting uses a 115 s soft / 120 s hard cap. The whole pass uses a 595 s soft / 600 s hard cap, including evidence waiting and a 5 s settlement reserve. Guard, API and filesystem cleanup are joined under EX; uncertain settlement is fatal and forces `process.exit(1)` while retaining EX until process exit. After cleanup, SH reacquisition has a separate native 120 s wait before admission, and SH is retained for the process lifetime. OS filesystem latency has not been made cancellable.

Reviewer, auditor and tester covered the initial `67016688..7f17e43d` implementation and returned clean scoped reviews for the repair range `7f17e43d..f5b95db5`. Mandatory review repairs join realpath siblings with `allSettled`, enforce the cumulative event-byte cap, and use the existing `MaintenanceBudget.runOwned` to join real `WorkerClient` response cancellation inside the owned fatal deadline.

Root evidence at `f5b95db5`, `.uzi/scratch/gate-log.AJ5iQB`, recorded EXIT 0: 10 executed files, 482 tests, zero failures/skips/cancellations, 182.948 s Node duration. The command ran from `agent/` with hermetic global/system Git configuration and `UZI_AGENT_TEST_CONCURRENCY=2`:

```sh
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null UZI_AGENT_TEST_CONCURRENCY=2 node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 test/pod-epoch-bootstrap.test.ts test/pod-epoch-evidence.test.ts test/pod-epoch-daemon.test.ts test/pod-epoch-reclaim.test.ts test/pod-epoch-retained.test.ts test/pod-epoch-startup.test.ts test/pod-epoch-native.test.ts test/clone-attempt-paths.test.ts test/retained-source-ops.test.ts test/runner-stopped-source.test.ts test/runner-recovery-exit-retention.test.ts
```

The combined command mistakenly also named the absent `pod-epoch-native.test.ts` selector; Node ignored that unmatched selector. The real native suite was then run separately with `test/pod-epoch-lock.test.ts test/maintenance-bootstrap.test.ts`, using the same command prefix and environment: 26 tests passed with zero failures/skips/cancellations in 3.102 s (`.uzi/scratch/gate-log.jaTpE7`, EXIT 0). The native suite's BusyBox-adapter kernel test is not the required util-linux final-image proof. The worker-UID project typecheck also passed: `npx --no-install tsc --noEmit --project ../e2e/worker-uid/tsconfig.json` from `agent/` (`.uzi/scratch/gate-log.jbmWzU`, EXIT 0).

These tests used the ordinary pinned runtime at UID 10002, not a final-image proof. Root `npm run typecheck`, `npm run lint` and `npm run knip` passed (`.uzi/scratch/gate-log.4p1t6i`); 13 repair tests passed (`.uzi/scratch/gate-log.fghGPn`). The tester independently reported 13 passing repair tests (`.uzi/scratch/gate-log.CL56Sq`) and passing static checks (`.uzi/scratch/gate-log.DMFAiM`); root inspected both independent logs and confirmed their recorded EXIT 0 results.

The lead's four isolated mutations at `7f17e43d` (`.uzi/scratch/m2b-mutation-logs-4ix0q8bn`) each passed the baseline and mutant typecheck, then failed the named test for record binding, open holds, restart policy or startup ordering. Exports were removed and the primary tree was unchanged. The later repair changes none of those predicate/test lines; the existing M2a terminality proof is retained.

This evidence does not establish complete worker boot, final-head CI, final base/JVM image acceptance or live acceptance. M1's final-image proofs and frozen full-agent criteria remain pending and unwaived. `task check-docs:web` passed (`.uzi/scratch/gate-log.aiqfvw`, EXIT 0; 79 docs validated with 48 house-style warnings). M2c retains chart/repo/docs closeout and unmeasured CI critical-path timing. User-doc synchronization waits for M2c; no workflow edits are included. M3 runtime attestation and live shadow acceptance remain pending.

### M2c local documentation and verification evidence (2026-10-26)

Operator documentation, the byte-identical embedded worker-Docker mirror, chart-value guidance and a UUID changelog fragment landed at `3644f939`. `task docs:sync` changed only the worker-Docker mirror; that task generates no manifest. `task check-docs:web` passed (79 documents, 48 house-style warnings), changelog fragment validation passed (`.uzi/scratch/gate-log.OztmRY`), and YAML lint passed (`.gr85Wc`). The focused `go test -race -count=1 ./internal/uzidocs -run '^TestEmbeddedDocsMatchSource$'` passed (`.T7Pde3`). The documentation acceptance paragraph also records the unresolved full-agent gate and formal partial-decision requirement.

`task gate:repo` ran against `3644f939`; its retained log `.uzi/scratch/gate-log.vdHCbh` records `EXIT=0`. The tool response was cancelled before delivery of its result, so the result was verified from that completed log. All repository tasks reached completion, including worker-UID harness checks. The secret scan reported zero findings in 7,738 tracked files and detected both canaries; 729 ignored/untracked scratch findings were explicitly non-gating, with permission warnings for scratch directories. Semgrep reported a detected canary and zero tree findings/errors. No unavailable or skipped executable proof is counted as passed.

The initial M2c closeout reused the verified `8dda8084` controller gate. The subsequent projection compatibility repair is covered by the new controller gate recorded below. The complete offline chart assertions passed on base and candidate as recorded above; the only later chart change is a values comment. These unchanged checks were not rerun merely to report completion. The full local agent gate remains the recorded historical failure, with attribution inconclusive after the bounded comparisons; the current affected-test and static results do not replace exact-head PR CI or waive the frozen full-gate criteria.

| Local selected comparison | Base `07fac261310c879248ddcbc1af92d7bdf4ffed43` wall seconds | Candidate `dfd60c766022d239c6c77348035e812fe39f2b8e` wall seconds |
|---|---|---|
| Three existing controller render test families | 13.633 | 14.972 |
| Four existing agent retained-source cases | 15.134 | 12.014 |

Each side ran once, serially, from a fresh scratch export with provisioned dependencies. All four commands exited 0; the agent selections each ran four tests with zero skips/cancellations. Controller command: `go test -race -count=1 ./internal/kube -run '^(TestRenderedPodPosture|TestDockerWorkerRendersNativeSidecarsInOrder|TestRunnerInitContainerShape)$'`. Agent command used the same pinned ordinary runtime, UID/GID 10002 and hermetic Git environment as the other local runs: `UZI_AGENT_TEST_CONCURRENCY=2 node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 --test-name-pattern='a cap on entries|a cap on pending directories|a cap on depth|operator inspect pins what a park keeps' test/retained-source-ops.test.ts test/runner-recovery-exit-retention.test.ts`. Exact wrapper arguments, export paths, revisions, statuses and timings are retained in `.uzi/scratch/m2c-timing-rfkreo3m/results.jsonl`.

These are single local wall measurements, including wrapper/compile/cache effects, not a demonstrated speed change or CI critical-path impact. Shared cache warmth was not isolated; CI queueing, rerun cost, cache contribution and critical-path impact are **unmeasured**. Final-head base/JVM image-lane candidate/base timing is also unmeasured because those proofs remain lander-owned and pending. No value is inferred from the local comparisons.

M0, M2a and M2b are complete. M1 and M2c are not declared fully complete while their final-head image/full-gate criteria remain outstanding. M3 runtime attestation and repeated live shadow acceptance remain pending. Before any non-closing delivery, a formal owner partial decision must select the actual retained/deferred milestone IDs and explicitly cover these outstanding criteria. Keep this PRD Draft and in place.

### Single advisory repair (2026-10-26)

`SECRET_MOUNT_RUN_UZI_REGRESSION` was verified and repaired at `5d3baede538f51d3370974e0b08c758eb4d62239`. The new projection is now mounted directly and read-only at `/etc/uzi/pod-epoch`, outside the entire configurable `/run/<single-component>` Secret namespace. The renderer and known-mount guard share one constant; the worker accepts only `/etc/uzi/pod-epoch/evidence`. The formerly valid `SecretMountPath=/run/uzi` is accepted again, without a new chart refusal or a breaking configuration change. The old projection path is retained only in a negative worker-bootstrap test.

The new Go compatibility test compiled but failed on the old source in all six posture/isolation cases (`.uzi/scratch/gate-log.oqUooN`, EXIT 1). After repair, validation/render/token/CA/non-overlap assertions and the projection/known-mount tests passed with race/count-one flags (`.CTxnMZ`). The 64 worker bootstrap/startup/shadow tests passed without skips or cancellations (`.QgHDeD`); agent typecheck, lint and knip passed (`.dZw9BA`, `.02thag`, `.gzkPY5`). The full standard `task gate:controller` passed at `5d3baede` (`.kGH3bC`, EXIT 0), including format, vet, build, lint, dead code and all race/count-one tests. Reviewer, auditor and tester completed clean scoped reviews of the repair; cancelled/incomplete dispatches are not counted.

`INLINE_ENV_GATE_COMMANDS` was declined: `AGENTS.md` explicitly documents hermetic inline-environment single-test forms, and the approved plan/owner steering require those focused comparisons. They are not a replacement for the still-red full-agent result. The image proof invokes the existing worker-UID task using its supported image/build/calibration inputs; skipping its parent-image build follows the orchestrator's own build and identity checks and leaves tool preparation, all UID profiles and calibration mandatory.

`PROOF_TOOLING_SCOPE` was declined: approved M1 includes the existing worker-UID execution/inventory wiring, and subsequent owner steering requires committed image-lane changes and an exact two-image handoff. The Docker-free proof regression test validates that tooling in the existing harness. No workflow or Taskfile change was made during this repair; unavailable CI critical-path timing remains explicitly unmeasured.

The source repair changes the final head required for lander image proofs and exact-head PR CI. Both remain pending before merge, as does M3 live acceptance; no new execution or waiver is claimed. The prior repository gate covers unchanged repository infrastructure; final tracked-secret and Semgrep checks on the repair are recorded in the delivery summary. Keep #3430 open.

### Owner-directed validation follow-up (2026-10-26)

The earlier two-file fixture-redesign proposal is superseded. Do not modify `agent/test/codex-executor.test.ts` or `agent/test/runner-worker-memory.test.ts` merely to stabilize the gate. A demonstrated candidate regression must be repaired at its cause; any test change needs a scoped justification.

For each red test, compare the actual merge-base of `origin/main` (`07fac261310c879248ddcbc1af92d7bdf4ffed43`) with candidate `f37f2e316790e1302fde57f7a60a4bad0109d299`, serially with `UZI_AGENT_TEST_CONCURRENCY=2`. Run at most three base/candidate pairs, stopping early only when the cause is established. Record exact commands, revisions, failure signatures and observed frequencies. Matching failures establish baseline presence alone; passing targeted controls do not explain the full-suite failures. Unresolved attribution stays inconclusive.

Bounded comparisons completed: three serial pairs per red test, all 24 invocations exited 0. Every selected invocation ran one test with zero skips. Failure frequency was **0/3 on base and 0/3 on candidate for each test**. Attribution remains **inconclusive**; these controls neither reproduce nor explain the full-suite failures. No source or test file changed.

| Selected red test | Base invocation wall seconds (pairs 1/2/3) | Candidate invocation wall seconds (pairs 1/2/3) |
|---|---|---|
| `releaseAtGate waits for the shown plan when its POST follows the gate state` | 19.381 / 13.549 / 11.316 | 16.906 / 16.084 / 12.434 |
| `D5: a approve_plan applied at a resumed gate gets one status line naming the verdict` | 14.551 / 13.545 / 15.363 | 13.554 / 16.054 / 13.545 |
| `memory deadline returns publicly while real Codex capture and finally stay owned: late cleanup` | 10.006 / 10.043 / 11.252 | 10.773 / 9.792 / 9.224 |
| `M3 late native park ACK stays owned after deadline and cannot mutate a reclaimed successor credential=true` | 8.452 / 9.010 / 8.612 | 6.590 / 8.030 / 8.601 |

For each row, the exact package command was the following with `PATTERN` set to that row's complete test name and `FILE` set respectively to `runner-gate-verdict-interruption.test.ts` (first two rows), `codex-executor.test.ts`, or `runner-worker-memory.test.ts`:

```sh
UZI_AGENT_TEST_CONCURRENCY=2 node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-concurrency=2 --test-timeout=120000 --test-name-pattern="^${PATTERN}$" "test/${FILE}"
```

Each invocation used a fresh exact-revision source export, provisioned dependencies, UID/GID 10002, the same existing ordinary test image/toolchain/cache and corrected init/hostname environment described above. Global and system Git configuration were `/dev/null`; HOME and TMPDIR were private. Exact wrapper commands, revisions, exit statuses and durations are recorded in `.uzi/scratch/tester-1-pairs-3430.jsonl`; per-run logs are `.uzi/scratch/tester-1-3430-{plan-post,approve-status,late-cleanup,late-ack}-p{1,2,3}-{base,candidate}.log`. These local artifacts are not final-head CI or image evidence.

The final PR description must retain the red full-gate evidence, these bounded comparison results and the pending exact-head CI check. Both final-head image proofs remain lander-owned and pending, and M3 remains operator-owned and pending. Before delivery, request a formal partial decision naming the actual milestone IDs and the outstanding frozen full-gate and image-proof criteria; ordinary continuation guidance supplies no waiver.

## Problem

On a Docker-wired hosted worker, attempt trees under `/data/runner` stay on the data volume after their runs end and their custody holds are discarded or released. Nothing deletes them automatically, because nothing proves that no Docker-side writer can still reach a tree (#1858, #1860). On busy workers they fill `/data` until runs park or fail on disk. #3329 recorded 10 settled trees, about 12.5 GiB, on one 25 GiB volume, plus recurrences on three more workers. Today an operator can discard eligible sources through the operator channel (#1786). Anything else must be preserved and the volume grown.

## Outcome

When a Docker-wired worker pod is replaced (a worker-pin roll, a template-drift rollout or a DinD maintenance recycle), the new pod logs one `pod_epoch_reclaim` verdict before it takes any run. The verdict says whether PRD B would be allowed to delete settled attempt trees at that epoch, and lists exactly which trees it would delete. Nothing is deleted.

The verdict is `eligible` only when all three of these hold:
1. The controller positively observed every container of the immediately preceding pod terminate, on a runtime stack the operator declared and this PRD audited.
2. No other pod used the volumes in between.
3. No other worker process holds the data volume's epoch lock.

Every Docker-wired worker takes part in the lock from this PRD on, so PRD B's deletion is excluded from the moment it ships.

**What the proof covers.** The immediate predecessor, any pod in between, and any concurrent worker process. It does **not** exclude every historical writer. The owner accepted one residual on 2026-10-24: a process of an older pod that survived its own termination (for example through a runtime dead-shim cleanup failure) and still writes into a settled tree after at least one later evidenced epoch. Races in the reclaim interval are **not** part of that residual; the epoch lock closes them.

Acceptance examples:
1. A worker holds three settled trees (run terminal, every hold discarded or released) and one tree with an open hold. A worker-pin roll replaces its pod gracefully on a declared runtime. The new pod logs `pod_epoch_reclaim` `eligible`, with a would-delete list naming exactly the three settled trees and their sizes. It deletes nothing, then claims.
2. The same worker's pod is force-deleted (grace 0). The controller patches a negative document. The new pod logs `deferred` (`no_predecessor_evidence`) without waiting, and claims as usual.
3. `workers.docker.podEpochReclaim.runtime` is unset. Every Docker pod logs `deferred` (`runtime_not_declared`) without waiting and claims as today. It still holds the shared epoch lock.

## Out of scope

- **Any deletion.** That is PRD B (#3431), together with the ADR amendments it requires.
- An on-demand "recycle pod, keep volumes" operation (M0 Q7). Parked for a later PRD.
- Reclaim triggered by idle time or disk pressure (#3329's original trigger). M0 Q7 replaces it, and those paths never delete attempt trees.
- Cross-worker reclaim of moved runs (#2823).
- Removing stopped Docker containers. The verdict pass sends the daemon only GET requests; container ownership is #3387's.
- Compose and non-Docker lanes. Runtime stacks other than the audited ones. Any `runtimeClassName`. Ephemeral containers.
- Storage without attach enforcement (CephFS, NFS and other shared filesystems).
- A node agent, cluster-scoped RBAC, or any threat-model relaxation beyond the owner-accepted residual.

## Modules and seams

### 1. Epoch lock: lifetime shared ownership, exclusive for reclaim

This module closes the reclaim interval. Both architecture reviews found the earlier startup-only lock unsound: T could take and release it, admit work, and then let S reclaim.

- **Lock file.** `/data/.uzi-epoch.lock` lives on the data-volume root, outside the `runner-shared` subPath, so DinD (which mounts only that subPath) cannot reach it.
  - `seed-nix` (root under uid-split) creates it if missing, `chown -h`s it to the worker uid and gid (10001:10001) and sets mode 0600, before it locks it. It is never unlinked or replaced. Existing safe lock metadata is validated rather than rewritten during an overlapping epoch; unsafe metadata refuses startup. Like `runnerInitScript` (`render.go`), it refuses a symlink (`[ -L ]`) or non-regular file at the lock or record path; on refusal it skips the record, and the worker fails closed (operator repair, as in ADR-3153).
  - The worker never creates it. It opens it with `O_NOFOLLOW` and checks that it is a regular file the worker owns. A missing file, or any other state, makes the worker fail closed (exit non-zero, no admission), because an attacker-swapped or broken lock file on a supported volume cannot be told apart from a reclaimer's.
- **Participation and fail-open.** Locking is keyed on a render-time env var, `UZI_EPOCH_LOCK=1`, set only for `withDinD` pods. It is not keyed on runtime Docker wiring, which degrades to unwired when the daemon is slow (`main.ts:374-385`). Compose and other lanes never lock. Participation is skipped (fail-open) **only** for conditions identical for every pod that can overlap on the volume, which make it impossible for any pod to have taken EX:
  - the filesystem type fails the allowlist, checked by `statfs` before any epoch-lock call (the pods share the volume).

  Then no pod can reclaim, so skipping is safe. The worker image **ships** the required `flock` (util-linux), and the worker invokes it by a trusted absolute path. Every other failure fails **closed**: a missing or failing `flock`, a non-`EWOULDBLOCK` error on an allowlisted filesystem, or an unverified ownership or type check on the lock file. One process's capability result never implies volume-wide exclusion. The worker does not touch protected state and does not open admission; it exits non-zero and retries on restart. `seed-nix` skips its record update. Default-off workers may skip deletion, but never participation.
- **Every participating worker holds `LOCK_SH` for its whole process lifetime.** That includes a pod with the runtime value unset, a pod without evidence and a pod whose one-shot is already consumed. The worker chooses and takes its epoch lock near the top of `main()`, after the explicitly permitted read-only maintenance bootstrap `/proc` snapshot and before the first worker data-volume writers (`ensureRunnerTmpdir`, `main.ts:364`, under `/data/runner`; `outbox.init()`, `main.ts:508`) and before startup reaping (`inspectRetireScratchAtStartup`, `reclaimStrandedRunHomes`, `reapStaleWorkspaces`, `reapCodexOrphansAtStartup` and `startupReapVerdict`), before any other worker access in `main()` to `/data/runner` or the bare repos, and before any Docker request or `worker.run()`. Pre-Node initialization writes are separately inventoried and protected below; they are not claimed to be read-only. Startup reaping and reclaim both run under that ownership. The wait is bounded and the bound exceeds the pod grace period (proposed 120 s), so an ordinary overlap costs no restart. On timeout the worker exits non-zero without opening admission.
- **Candidate S takes a tentative `LOCK_EX|LOCK_NB` at the top of `main()` instead of SH.** S is a candidate when the runtime env var is set, the projection is mounted, the storage gate passes and the one-shot is unconsumed. `flock(2)` conversion is not atomic, and a failed non-blocking conversion drops the shared lock, so S chooses its first lock mode up front.
  - S holds EX through startup reaping, evidence validation and the bounded read-only verdict pass.
  - If EX is busy, S defers (`lock_busy`) and takes SH (bounded) with admission still closed.
  - If the evidence is absent, negative or invalid, S closes the descriptor and takes SH (bounded) with admission still closed.
  - Every other worker takes SH.
- **Under EX**, S re-reads the pod-entry record (module 2) and requires `current == S` and `previous == X`. It checks this before the predicate and again before each tree. Then it runs the predicate and the read-only verdict pass. Once that pass has settled, S closes EX and takes SH (bounded) with admission still closed, and only then opens admission. Settlement of destructive children under EX is PRD B's requirement.
- **Mechanism from Node.** Node has no `flock` API, and libuv opens descriptors close-on-exec. Node opens the file, then runs the image's `flock` utility on that descriptor passed in an extra stdio slot (`flock -s|-x [-n] 3`). The lock lands on Node's open file description and outlives the helper. A-M1 ships util-linux `flock` in the worker image (util-linux is already the source of `setpriv`) and calls it by absolute path. Use `-n` polling within the bound and a distinct conflict exit code (200), so contention is distinguishable from missing utility or other failures; unexpected nonzero exits fail closed.
- **Release semantics.** The lock is released only when the last descriptor on that open file description closes. This PRD runs no destructive work under EX, so the only holders are the worker process itself (and, under EX, its read-only verdict pass). Passing the descriptor to destructive child processes, and the definition of unsettled cleanup, belong to PRD B.
- **Storage gate.** The worker checks `statfs('/data').type` against an allowlist (ext4, xfs) and defers reclaim otherwise. Declaring the runtime value also attests that the data storage enforces attach exclusivity (the RWO Multi-Attach refusal) or is node-local. Only then do overlapping pods share one kernel's view of the filesystem.
- **How the cases come out.**
  - **X→S→T, S acquires first.** T's `seed-nix` finds the lock busy and skips the record update, leaving `current = S`. T's DinD can only restore containers, which the barrier covers. T's worker SH blocks while S holds EX, so T never admits work during S's pass (in PRD B, during deletion). Once S is SIGTERMed, its read-only pass stops, so T waits at most about the grace period.
  - **T acquires first.** T never releases SH, so S's EX attempt fails and S defers.
  - **T's DinD before T's lock.** T's DinD sees only `runner-shared` and the DinD volume. Without a client it can only restore containers, and restore starts only containers whose restart policy is not `no`. S's predicate already proved there are none, against a daemon restored from the same persisted root with no client since. Its only clients before the lock are the kubelet's `docker info` probe and the connect-only wiring probe, both read-only. Execution/render tests pin that every application-owned initializer which can touch protected state holds temporary SH for those operations. Init record EX remains non-blocking; protected initialization SH may wait for an existing EX holder, without serializing ordinary SH overlap.
- **Mixed versions.** Pods that overlap on one volume come from the same ReplicaSet, so they run the same image. Graceful deletes are replaced from the same ReplicaSet, and Recreate's `oldPodsRunning` stops overlap across templates. Shipping SH in the same image as deletion, in this PRD before PRD B enables deletion, is therefore enough. An old controller renders no projection, so the worker defers immediately (module 4).

### 1a. Exact initialization inventory and startup protection

This is exhaustive for write-command families in the tracked entrypoint at 07fac261, not a claim about arbitrary replacement CMDs, environment preloads, external kubelet/CSI actions or untracked dependency code. All root-window families below will run inside temporary epoch SH, before protected operations; this avoids treating nominally disjoint paths as safe when a token, hardlink, symlink prefix or mounted alias can reach protected state. Only an unsupported filesystem may bypass epoch participation. A 120 s SH timeout fails startup before the operation. Close reserved shell FD 9 after synchronous initializer children finish and before setpriv/tini/npm/tsx; Node opens a separate private descriptor and chooses EX or SH. No lock descriptor crosses that launch chain or reaches run children.

| Baseline site | Existing writes and target | Handling / reason |
|---|---|---|
| entrypoint.sh:198–203, `migrate_tree` | `"$CHOWN" -R "$owner" "$path"`; sentinel redirection/chown for `/data` and `/nix` | SH: directly reaches attempts, bares, retention and ledger inodes; never claim no protected writes. |
| :219–221 | reclaim/chmod1755/chown `/nix/store` | Same SH; fixed directory only normally, alias/hardlink interactions are not exemptions. |
| :259,308–309 | mkdir/chmod3775/chown `/data/{runner,agent-home,provision}` | Same SH; root-only normally, but these are protected ancestors. |
| :324–328 | mkdir/reclaim/chmod3710/chown `/data/jobs` worker:codex-session | Same SH. Every root boot writes the root inode; no descendant recursion in this block. |
| :442–444 | legacy reclaim/chmod3775/chown carve-out parents | Same SH, guarded by legacy sentinel. |
| :487 | `"$CHOWN" -R runner:runner "$c"` under runner/provision | Same SH: directly touches attempts; `.tmp` is skipped but hardlinked protected inodes may be reached. |
| :499,501,505,514,537 | HOME roots/codex-data roots and recursive epoch/other/shared-dot chowns | Same SH; preserve symlink, single-link top-level file and private session-store exclusions. |
| :540 | write/chown `.uzi-legacy-split-migrated` | Same SH; do not call shell redirection no-follow-safe. |
| :573,576 | nonrecursive shared HOME dot chown; repair sentinel write/chown | Same SH; nominal metadata and alias-sensitive sentinel access. |
| :635,644–646 | mkdir/reclaim/chmod0755/chown `/data/runner/.tmp` | Same SH, preserving real-directory checks. |
| :777 and :731,733,736–737 | create per-UID scratch leaf; inode-pinned `.` reclaim/chmod0700/chown and rollback | Same SH; no recursive Docker scratch mutation. |
| :815,817,819,823–824,830,833 | non-Docker TMPDIR branch reclaim/walk/chmod/rm/mkdir/normalize | Unchanged for nonparticipating lanes. Participating Docker workers must use validated current `/data/runner/.tmp`, never the runner-root legacy or arbitrary alias lane. Guard any applicable path with SH; do not infer `rm` has find's `-xdev` bound. |
| :857,859,862–863 | mkdir/reclaim/chmod0700/chown command-cache root | Same SH root window; normally `/var/cache`, not `/data`, but no unsafe alias exemption. |
| :910–912 | token chown/chmod0400/chown | Same SH; configured token can alias protected state, so no unqualified disjointness claim. |
| :948–955 | `align_pvc_root` reclaim/chmod2775/chown `/data` and `/nix` roots | On participating Docker, preserve data-root group/mode without reclaim/alignment, as specified below; other root alignment remains inside SH. |
| non-root branch :65–108 | environment changes then `exec "$TINI" -- "$@"` | No filesystem writes by this tracked branch; no permanent shell FD handed to Node. |
| render.go:855–856, `dataClear` | `rm -rf` four shared provisioning paths under `/data-seed` | Seed record EX remains nonblocking; after it settles/skips, take temporary SH before reseed body. Alias-sensitive deletes are not exempt. This SH can wait for EX; ordinary SH overlap is not serialized. |
| render.go:903,927–941, `runnerInitScript` | create/chown/chmod runner root and `.tmp` through data mount | Temporary SH after existing image gate and before protected volume access; physically `/data-seed/runner-shared` and `.tmp`, mounted as worker `/data/runner`; preserve golden and both UID modes. |

New epoch lock creation never unlinks/replaces its inode. Seed's record and initial consumption-state setup use the specified safe initialization ownership. Record EX attempt still never waits; a protected initialization SH may wait for an existing EX holder. These bounds replace the earlier blanket no-init-write/no-wait claim; initialization is not read-only.

Read-only category, separate from the table: `main.ts:332–334` calls `recordMaintenanceBootstrap(snapshotMaintenanceBootstrap())`; `run-quiescence.ts:763–788` bounds `/proc` process identity reads, and :795 onward records frozen memory only. Preserve this before Node lock acquisition. `main.ts:357–359` / `run-quiescence.ts:824–848` maintenance posture diagnostics are bounded reads and can move after acquisition. Neither is a `/data` initialization write.

The controller-rendered `NODE_EXTRA_CA_CERTS` remains supported: native validation permits only its regular, root-owned group-10001 mode-0440 `ca.crt` inside the same single-directory `/run` Secret mount as the token (`ValidateSecretMountPath`, including the #1761 relocated mount). Projected-file symlinks must resolve inside that mount. Node/OpenSSL reads this certificate trust data during bootstrap; it grants no epoch authority and writes no reclaim-protected state. Executable preloads and npm overrides remain refused.

Gap after temporary SH closes: normal tracked launch is image-baked `npm run start` → `tsx src/main.ts` (`agent/package.json:9`), HOME `/home/worker` (base Dockerfile:67; JVM:58). These dependency-owned programs can write before `main()`. Do not exempt arbitrary persistent scratch aliases: for participating Docker startup, preserve the validated persistent runtime TMPDIR in entrypoint-set `UZI_EPOCH_RUNTIME_TMPDIR`, then set bootstrap TMPDIR to an image-created worker-owned mode-0700 container-local directory `/tmp/uzi-epoch-bootstrap` before invoking npm/tsx in BOTH UID branches. Disable tsx's disk cache during this bootstrap. Image/default HOME/npm cache stay container-local; pin rendered CMD/environment and reject unsupported preloads/configurations. After Node acquires epoch ownership, validate the saved runtime TMPDIR against the fixed lane/UID layout, restore it and unset `UZI_EPOCH_RUNTIME_TMPDIR` before `ensureRunnerTmpdir` or volume writers. Keep runner TMPDIR semantics after locking. The helper used for shell validation must be plain image-baked JS/native utilities, never tsx, and may only read/validate before its SH. Nothing passes FD9 across setpriv/tini/npm. Tests instrument the actual image startup and assert pre-main writes remain container-local, including planted persistent scratch aliases. Compose/nonparticipating lanes retain their existing behavior. This narrow bootstrap-temp change avoids a new persistent bootstrap process or lock handoff protocol.

External boundary: kubelet/CSI ownership processing precedes all scripts and cannot take an application lock. Existing `render.go:1007` selects `FSGroupChangeOnRootMismatch`; `entrypoint.sh:934–950` explains its root predicate and currently changes root gid to 0. For participating Docker startup, verify the data mount root already has gid10001, owner and group permissions covering 0770, and setgid from mount preparation; fail startup on a mismatch. Preserve those fields throughout initialization: exclude the mount-root inode itself from the one-time `/data` ownership traversal and skip `/data` root reclaim/chmod alignment on this lane (retain all descendant migration and the non-Docker path). The root may remain trusted root-owned with exclusive worker-group write; do not require worker ownership of the mount root, only of epoch/ledger files and worker-private children. Also require the participating token target to be a regular file before its metadata operations, so configured directory aliases cannot mutate the data mount root. Normal rendered Secret/projected-file inputs remain supported. This removes application-created root-mismatch windows. The [pinned storage source audit](3430-pod-epoch-storage-audit.md) records the exact kubelet owner/group permission predicate and CSI delegation boundary. Verify the pinned kubelet/CSI root-skip semantics and actual driver honoring the policy: source/design evidence and synthetic root-posture invariance tests are worker obligations; the operator must attest actual driver/node behavior before declaring runtime. A driver that performs overlapping ownership walks despite matching root posture is unsupported and must remain default-off, not an accepted residual. No claim that SH fences kubelet; no runtimeClassName or new privilege. Test group/mode preservation after every data-root-affecting initializer operation and preserve existing nonparticipating fsGroup/migration tests.

Additional verification: actual initializer execution under a held EX must log no protected mutations until release, including absent migration sentinels, populated runner/repos/ledger, jobs root and configured token aliases. Test FD closure before both exec paths and the reacquisition gap with no protected writes. In both UID modes, assert observable post-lock `process.env.TMPDIR`, `os.tmpdir()`, actual temp creation and runner-child scratch use the expected persistent runtime paths, not the private bootstrap directory; missing/invalid saved paths refuse startup. A mutation omitting restoration must fail a named assertion; `ensureRunnerTmpdir` returning `checked:false` outside runner is not a passing restoration proof. Expected `.tmp` dependency writes and any external storage acceptance remain explicit, never reported as absent.



### 2. Pod-entry record (volume binding; corroboration, never authority)

- The first init container (`seed-nix`, `controller/internal/kube/render.go:1195`, before `dind`) takes `flock -x -n` on the same lock file and updates `/data/.uzi-pod-entry` under it, so the read-modify-write is serialized.
- If record EX is busy, because an overlapping predecessor holds SH or EX, `seed-nix` skips the record update without waiting. It then takes bounded temporary SH before protected seeding/reseeding writes. Ordinary SH overlap adds no serialization; an EX holder may delay protected initialization. A skipped record can cost the next epoch: `previous` differs from that pod's predecessor and reclaim defers. DinD restore remains covered by the restart-policy predicate, and worker admission by lifetime SH.
- The record holds two slots, `current` and `previous` pod UID. It is replaced atomically (write to a temp file, fsync it, rename, fsync the directory).
- Re-entry with the same UID (an init-container restart) leaves the record unchanged.
- An unreadable, torn or schema-invalid record makes reclaim defer, and the record never supplies authority. The record file is mode 0644.
- The update runs only for Docker workers (`withDinD`). The non-Docker render stays unchanged, as `render.go:1191` requires. It uses a new downward-API env var `UZI_POD_UID` in that container.

### 3. Controller: a non-transferable predecessor-termination handoff

- **Observe.** The controller watches pods in the **Docker** worker namespace only. This adds the `watch` and `patch` verbs on pods to the docker Role in `deploy/chart/templates/worker-rbac.yaml`. The restricted and isolated Roles are unchanged, and none of these becomes a ClusterRole. The comment there ("`list` ONLY") and `TestObserveIssuesListOnlyOnPodsAndActuallyListsThem` are updated together. Today's 10 s `list` (`materializer.go`) is unchanged.
- **Session.** A session starts from a full list and then a watch from the list's resourceVersion. It must be synced (list plus watch established) before the materializer's first mutating tick after a controller start, so a roll the controller applies is observed. That wait is bounded (60 s); after the bound the controller proceeds. Epochs in the unsynced window get no handoff, so they defer: verdicts fail closed and rolls fail open. Events are handled serially in stream order; resourceVersions are opaque and never compared. A reconnect that resumes from the last delivered resourceVersion, and that the server accepts, continues the session. A `410 Gone`, a relist, a decode error or a controller restart ends it. Pods present in the initial list are never treated as newly added successors.
- **Signal for pod X.**
  - One pod UID with `status.phase ∈ {Failed, Succeeded}`.
  - Container and init-container status **names** equal the spec's name sets exactly, with nothing missing and nothing extra.
  - No ephemeral container is declared or reported.
  - Every entry is `state.terminated` with reason `Completed` or `Error`, a non-empty `containerID`, and an exit code that is ≥ 0 and not 137 or 255.

  M0's refusals are kept. None of these is evidence: a tombstone, a pod that disappeared without an observed terminal status, or podgc's `Failed` patch.
- **Pods that count.** These count as pods of that worker:
  - every pod with the worker's labels;
  - **any pod whose `spec.volumes` names the worker's data or DinD claim** (M0, Binding).
- **Handoff rules.** Within one session, all of these must hold:
  1. X's terminal status was observed satisfying the signal.
  2. S was then observed added: a new UID, owned by the worker Deployment's current ReplicaSet (Deployment → ReplicaSet → Pod ancestry verified), and mounting the same data claim name.
  3. Over X's lifetime (from X's addition, or from session start if X was in the initial list) up to S's addition, every other counted pod seen non-terminal showed the signal before X's terminal status. No counted pod was deleted without the signal in that window.
  4. X's deletion after its observed terminal status is fine.
  5. Identical duplicate events are idempotent.
  6. **DinD claim identity.** It is unchanged, except where the session recorded an ADR-1759 maintenance recycle for this worker (`maintenance.go`) that reminted the DinD claim. Then the old and new claim UIDs are bound to that operation. An unexplained DinD claim change voids the handoff.

  Any failed rule means no handoff, and S defers.
- **Negative document.** When S's addition fails the handoff, the controller patches S with a negative document (`{"handoff":"none","reason":…}`) through the same patch path, so S defers at once instead of waiting out the cap.
- **Deliver.** The handoff is one bounded JSON document. The controller writes it only onto S's pod as the annotation `uzi.dev/pod-epoch-evidence`, through a UID-preconditioned patch whose body carries only `metadata.uid`, `metadata.resourceVersion` and that annotation. A test asserts the body. There is no PVC persist step: the annotation on S is durable. Restart discards decisions not yet submitted. A patch already submitted may commit after restart, remains bound to its exact successor UID and resourceVersion, and still requires all worker-side lock, runtime, volume-binding and durable-consumption checks. Restart cannot retract an in-flight API-server write or authorize replay onto a different successor. The PVC carries `annotationRoll`, which `mirrorRoll` patches under resourceVersion guard, so adding a second writer there would block rolls.

  The document holds:
  - schema version and session id;
  - X's UID, with per-container `containerID`, exit code and `restartCount`;
  - S's UID;
  - data and DinD claim UIDs and `spec.volumeName`, and any recycle binding;
  - the declared runtime value;
  - times.

  The controller verifies S's mounted claim UIDs itself, from S's `spec.volumes` and the PVC list.
- **Projection.** S's render gains a `downwardAPI` volume, mounted directly (no `subPath`), with one item: `fieldRef: metadata.annotations['uzi.dev/pod-epoch-evidence']`. The worker re-opens the path on each read. An empty or missing file means "pending".
- **Declaration.** The runtime value is rendered as an env var into the Docker pod template, so changing it rolls the workers through Recreate. That roll is itself an evidenced epoch. The controller adds nothing to the document unless the value is set.
- **Bounds.** The document is controller-built and never taken from worker or user input. It is at most 8 KiB, and at most one patch request is attempted per epoch, counting conflicts and ambiguous timeouts; it is never retargeted. LIST uses at most 100 objects/page and an 8 MiB response limit before decoding; watch frames are limited to 2 MiB before decoding. Session proof state is capped at 4,096 distinct pod UIDs/claim identities and 16 MiB of retained metadata. Overflow invalidates positive evidence without filtering out foreign claim users or blocking ordinary reconciliation.
- **Tests.** Controller unit tests drive event sequences through the session handler:
  - graceful Recreate; X deleted after its terminal status;
  - eviction; force delete; podgc;
  - a sidecar in `waiting`; a status-name mismatch; an ephemeral container;
  - exit codes 137, 255 and -1; `ContainerStatusUnknown`;
  - reconnect with resume; `410` and relist;
  - restart discards an unsubmitted decision; an already-submitted patch may commit only to its exact successor and remains subject to worker validation; duplicate events;
  - A→B→C with B unobserved; A→B→C with B unevidenced;
  - a non-terminal W next to X; a foreign pod naming the claim;
  - a stale replay to a later pod;
  - a DinD recycle with and without a recorded operation;
  - the patch body containing only the three fields.

### 4. Worker: once-per-pod shadow verdict before claim admission opens

- **Where.** In `agent/src/main.ts`, preserve `recordMaintenanceBootstrap(snapshotMaintenanceBootstrap())` first. It performs bounded read-only `/proc` identity reads and freezes an in-memory record, with no `/data` write or epoch authority. Parse only the required lock/candidate environment fields before acquisition; move full `loadConfig()` (including token-file reads) under acquired epoch ownership. Acquire before worker data-volume writes, Docker requests and startup reaping; then durably consume, run the existing reapers and `startupReapVerdict`, the shadow pass, and finally `worker.run()`. Before acquisition, worker data-volume reads are limited to `statfs`, bounded consumption-state reads and lock validation. Move optional maintenance-posture `/proc` diagnostics after acquisition. The ordering test asserts this `main()` boundary; the separate pre-Node write inventory below remains part of the contract.
- **One-shot, durable per pod on every path.** `/data/.uzi-epoch-consumed` is a worker-owned mode-0600, immutable-inode append-only ledger, not a replaceable last-UID marker. It has the eight-byte header `UZIEC01\n` (LF byte), followed by at most 4,096 distinct canonical Kubernetes UUID records, each 36 ASCII bytes plus LF. The logical storage bound is exactly 151,560 bytes and one inode, plus filesystem allocation overhead; no accumulating temporary files. It is never unlinked, replaced, truncated, evicted, compacted or automatically reset.
  - Seed safely initializes the header and fsyncs file and directory. On supported storage this is under epoch EX; an overlapping seed validates existing state rather than rewriting it. The worker never creates missing state.
  - After obtaining epoch ownership on supported storage, briefly lock the ledger inode EX with a 5 s bound, validate its bounded complete contents, append a fresh UID and fsync before evaluation or admission. Lock order is always epoch then ledger. Fsync a valid existing ledger before admission as well, settling a complete append whose producer died before fsync. Release the ledger descriptor before changing epoch ownership.
  - Only a `fresh` consumer holding epoch EX may evaluate. An existing UID refuses reevaluation forever. Another pod's append cannot erase it. Test S consumes and admits, T consumes and exits, then S restarts with late evidence: no second pass may begin.
  - At 4,096 records, overflow **refuses evaluation** (and PRD B reclaim), appends no bytes and never bypasses mandatory epoch locking. A full ledger is stable because records are not evicted; new pods still hold lifetime SH before admission. Runtime unset, no projection, busy EX and other deferrals still consume before admission. A torn/malformed ledger, short append, uncertain fsync or metadata-lock failure stops startup without admission; do not repair history automatically.
  - An unsupported filesystem still skips epoch-file flock and refuses evaluation uniformly. It initializes/consumes the same bounded ledger using only the ledger's metadata mutex; this mutex grants no epoch-exclusion authority and cannot re-enable evaluation. Uncertain consumption is fatal as above. Storage type remains the only participation skip; capacity, runtime and local capability results never are.
- **Policy update (2026-10-31).** PRD #3431's owner option A supersedes the declaration policy below; see the 2026-10-31 Decision Log row. Evaluation stays on without a declaration, and evidence is versioned.
- **Immediate deferral.** The worker defers without waiting when the runtime env var or the projection mount is absent (`runtime_not_declared`, `no_projection`), when the storage gate fails, or when the document is negative.
- **Evidence read.** Otherwise the worker re-reads the projected path until a cap (proposed 120 s; the shadow acceptance measures it). Empty means pending. Late arrival after a deferral is ignored. The worker accepts a document only when all of these hold:
  - its schema is valid;
  - S equals `UZI_POD_UID`;
  - the runtime value equals the rendered env;
  - module 2's record shows `current == S` and `previous == X`. This is the worker-side volume binding, independent of the controller.
- **Daemon predicate.** Only GET requests are sent: `/_ping`, `/info`, `/containers/json?all=1`, `/containers/{id}/json`, `/volumes`, `/plugins` and `/events`.
  - Conditions:
    - the daemon served its first response;
    - no container is running, restarting or paused;
    - every container's restart policy is `no`;
    - swarm is inactive, no plugin is enabled, and live-restore is off;
    - every volume is `local` with empty `Options`;
    - an unfiltered `/events` GET stream with `since` replay covering the interval before inventory is held open and monitored for transport health throughout the pass. It is only a veto/abort guard: any event, malformed input, EOF or transport error aborts. Docker flushes HTTP 200 before registering the subscription, so headers or silence are never positive subscription-liveness or exclusion proof. Exclusion rests on the no-client interval, restore/restart-policy predicate and stopped-state proof in PRD #1860.
  - Bounds:
    - at most 500 containers, 500 volumes and 50 plugins (defer above);
    - inspect concurrency is 4;
    - a truncated or unparseable inventory defers;
    - events are buffered only up to the first event, which aborts the pass.
  - A test-only fake daemon records the methods it receives; the test asserts the pass sends only GET requests.
- **Read-only authority evaluation** (the would-delete list). For each attempt tree, the worker evaluates #3329's authority **without deleting**:
  - holds through `listRecoveryHolds` (as `ops/retained-source-server.ts` `readHolds` does);
  - lifecycle from the ownership probe (`GET /worker/runs/{id}/ownership`); a tree is listed only if the run is terminal;
  - the retained-source disposition through the existing inspection path (`inspectRetainedSources`, which uses `evaluateRetained`, `agent/src/git.ts:8309`), which must be `local_discard` or `local_protected_discard`;
  - the tree's size.

  Hold currency rests on "read before admission ever opened in this pod, under EX". T shares the worker id, so T's claims could add holds, and EX excludes T.
- **Aggregate bounds** (proposed; the shadow acceptance measures them):
  - at most 200 physical tree paths per epoch, deduplicating multipart descriptors; overflow is exact only when bounded discovery completed, otherwise a lower bound with `inventory_complete=false`;
  - discovery stops at 4,096 directory entries or 1,024 bares, metadata at 8 MiB or 4,096 records per bare, and finite subprocess output at 8 MiB; overflow is partial/unknown, never silent authority truncation. Retain the existing 256-hold cap and measurement caps (100,000 entries, 10,000 pending directories, depth 256 per tree); preserve the operator inspector's independent 64-descriptor default;
  - total wall time at most 10 min, covering discovery, hold and ownership reads, inspection and size measurement;
  - the remaining deadline is passed into each operation, not checked only between trees.

  On SIGTERM or deadline, the pass stops and the verdict is `partial`.
- **Verdict.** One `pod_epoch_reclaim` log line carrying:
  - `eligible`, `deferred` or `partial`, with a reason code;
  - counts of trees examined, would-delete and kept;
  - the would-delete list (bounded: ids plus sizes; an overflow count past 200);
  - bytes, marked `measured`, `partial` or `unknown`;
  - the evidence-wait and pass durations.
- **Untrusted inputs.** Daemon responses have bounded bodies (as `dockerRequest`, `run-quiescence.ts:2038`) and are bounded by the counts above. The projected file is at most 8 KiB under a strict schema.

### 5. Runtime gate (chart value)

`workers.docker.podEpochReclaim.runtime`. Unset (the default) disables reclaim. Accepted values are exactly the audited stacks:

- `containerd-runc` (M0): containerd 2.3.4, runc 1.5.1, kubelet 1.37.1 read.
- `crio-crun` (CRI-O audit):
  - CRI-O **1.36.2** with `runtime_type` `oci` (classic conmon) and crun as the default handler;
  - **crun 1.28 or 1.30.1** (namespace creation and state output were read at both tags; intervening versions are not included);
  - conmon v2.1.10 to v2.2.1;
  - kubelet 1.35.3 read;
  - no namespace-adjusting NRI plugin (an unsupported configuration).

Declaring a value attests that every node running worker pods uses that stack within those bounds, with attach-enforced or node-local data storage. A version outside the bounds needs a new audit, not a declaration. A pod with any `runtimeClassName` defers.

### CRI-O + crun audit: summary

The full audit, with READ / INFERRED / NOT READ markings and source references, is `prds/3430-pod-epoch-crio-audit.md`.

- **Private pid namespace (READ).** Kubelet `PidNamespaceForPod` returns `CONTAINER`. CRI-O uses the runtime-tools `generate.New` defaults, which include a pathless `pid` namespace; `SpecAddNamespaces` maps only NODE, POD and TARGET. crun unshares a pathless namespace.
- **A non-negative exit code comes only from conmon's exit file (READ).** conmon writes ≥ 0 only after `waitpid` returns init's pid, or (v2.2.x) after a kill probe gets `ESRCH`. The kernel's `exit_notify` runs `zap_pid_ns_processes` before init becomes a zombie.
- **Synthetic codes.**
  - A missing exit file gives -1.
  - A `crun state` error with no file gives 255.
  - A dead conmon later surfaces as -1 / `Error` with a `containerID`. Hence exit codes `< 0` are refused.
- **Fail-closed cases.** `crun state` can report a live init as stopped on a procfs `ENOMEM`, under `hidepid`, or when the freeze file is missing. conmon is still waiting in all three, so the exit code is -1 and refused.
- **Verified live on one maintainer cluster (2026-10-24):** nodes report `cri-o://1.36.2`, `default_runtime = "crun"`, `runtime_type` unset, kubelet `v1.35.3`.
- **Not verified:**
  - installed conmon and crun packages, `/proc` options, NRI plugins, the storage class's attach enforcement;
  - the audit's inference that conmon's `container_pid` probe runs in the host pid namespace;
  - the running kernel build's identity against the audited source. The exact `kernel-6.12.0-243.el10` source tag now has the same audited exit/PID/procfs guarantees (2026-10-29 source extension); other kernel builds remain unaudited.

  These are acceptance prerequisites, not residuals.

## Risks

- **Startup latency.** The verdict pass runs before registration, so a roll's canary waits up to 120 s plus the pass longer to count as usable (`roll_pacing.go:593/771`, which needs Registered, PodUID and a heartbeat). The shadow acceptance measures it.
- **Restart-loop flag.** Fail-closed exits count toward `stuckRestartThreshold = 3` (`rollhealth.go:106`). Repeated lock timeouts would mark the worker stuck, which is visible and fails closed.
- **Wedged holder.** A wedged holder of the lock delays a successor indefinitely through bounded retries.
- **File overlap.** Overlaps with #3103 (`roll_pacing.go`, `materializer.go`) and #3153 (`render.go`). Sequence against them.
- **Controller release roll.** This PRD's controller release rolls every Docker worker once (capacity at zero for a few minutes, #1470).
- **DinD sharing.** Two dockerd processes sharing one DinD root during an overlap is a hazard that exists today, and this PRD does not change it.

## Testing decisions

- **Unit tests** cover the seams above, including Linux two-process flock tests in CI. These must fail, not skip, on Linux:
  - S first (T's worker SH waits until S's process is gone);
  - T first;
  - a swapped lock file caught after a successful successor init record update, plus a descriptor/path device-inode mismatch caught after acquisition; these do not claim detection of every out-of-band replacement throughout EX;
  - the default-off path still holds SH;
  - a storage type outside the allowlist.
- **Shared fixture.** A contract fixture under `fixtures/` is read by the Go controller test and the agent test (the cross-module `fixtures/api-contract/` pattern).
- **No KinD lane.** The KinD smoke runs no controller and no workers, its local-path volumes would fail the storage gate, and extending it means a workflow edit a uzi run cannot push. In-run validation is the controller event-fixture tests and the worker two-process tests. Watch capture, projection latency and real `eligible` verdicts move to a live shadow acceptance, which is safe because this PRD deletes nothing. `e2e/pod-epoch/` keeps covering daemon semantics only. A KinD hosted-worker smoke, if wanted, is a separate maintainer issue.
- **Pending-create regression.** The #1858 pending-create control (`agent/test/clone-attempt-paths.test.ts`, which has an open flake, #2578) keeps its sentinel-preservation assertion.

## Milestones (one run, one PR)

- **M1: Lifetime epoch lock, pod-entry record and one-shot.** Modules 1 and 2 and the one-shot. Contents:
  - `UZI_EPOCH_LOCK`, the tentative EX and SH;
  - util-linux `flock` shipped in the worker image and invoked by absolute path;
  - lock-file ownership and symlink safety;
  - the non-blocking init record;
  - bounded, immutable-inode per-pod consumption before evaluation/admission on every path, including saturation without skipping mandatory epoch participation;
  - temporary SH for protected pre-Node writes, private bootstrap temp storage, and preservation of the mounted data root's fsGroup posture;
  - the storage gate and the fail-closed boundary;
  - lock, render and ordering tests.

  The worker's tentative reclaim EX path is not enabled until M2. Seed still briefly takes EX for record/state initialization. Other protected initializers take SH, compatible with ordinary workers' lifetime SH. Record EX never waits; protected initialization SH may wait for an EX holder up to 120 s, then refuses startup. A persistent lock error on supported storage stops that worker's starts (owner decision 2026-10-24). Blocked by: none.
- **M2: Controller handoff and shadow verdict.** Module 3 (the negative document and the bounded startup barrier), module 4's evidence read, daemon predicate and read-only authority evaluation under tentative EX, and module 5. Also:
  - the `pod_epoch_reclaim` verdict;
  - controller event-fixture tests;
  - docs on the runtime value and what declaring it attests.

  Blocked by: M1.

### Delivery, acceptance and closure

The PR references #3329 and #1860 and closes neither.

A linked `acceptance` issue, owned by the user, covers:
- **Before declaring:** node packages, `/proc` options, NRI plugins, storage attach enforcement, the conmon `container_pid` namespace check, the image's util-linux `flock`, and the actual kubelet/CSI ownership policy honoring matching-root skip semantics. Drivers with an overlapping recursive ownership walk despite matching data-root posture are unsupported, not a new accepted residual.
- **In shadow, over several graceful rolls:**
  - handoff capture rate;
  - projection latency;
  - pass time;
  - verdicts and would-delete lists, checked by hand against the trees and holds.

  This acceptance unblocks PRD B (#3431).

## Decision Log

| Date | Decision | Why | Rejected |
|---|---|---|---|
| 2026-10-24 | Implement #3329's disposal at pod epochs only, in two PRDs: this one (lock plus shadow verdicts) and PRD B (deletion, blocked by this PRD's shadow acceptance and by #2852 C, which #3329's dispatch gate names). | Owner-approved split. The caps can only be measured live; #2852 C gates only deletion; shadow evidence is independently valuable. | One PRD and one PR for all of it. |
| 2026-10-24 | Immediate predecessor + intervening pods + concurrent workers proved; older-pod survivor accepted as a residual. | Owner decision. It relieves today's inventory at the next evidenced epoch. The ADR states the limit. | Strict proven history from a trusted genesis (buddy's preference): it reclaims nothing on existing workers. |
| 2026-10-24 | A readers-writer `flock` on the data volume: SH for every worker's lifetime, EX non-blocking for reclaim, record re-check under EX. | Both architecture reviews: a startup-only lock fails when T acquires first. This is the smallest mechanism that also covers S's helpers and default-off workers. | A startup-only lock; ReadWriteOncePod (needs new claims and a data migration for existing workers); EX for every worker's lifetime, or a blocking init EX (both serialize every overlap). |
| 2026-10-24 | Consume the one-shot before admission on every path. | Otherwise defer, then run, then restart, then late evidence would reclaim after Docker use. | Consuming it only before the first reclaim action. |
| 2026-10-24 | Handoff on S's pod annotation only; no PVC persist. | A second PVC writer conflicts with `mirrorRoll`'s resourceVersion-guarded `annotationRoll`; the pod annotation is durable enough. | Persisting on the PVC. |
| 2026-10-24 | A single-session watch, stream-ordered, counting claim-mounting pods, with ReplicaSet ancestry and a DinD-recycle binding. | Closes A→B→C, stale replay and foreign pods; M0's Binding rules. | Counting by labels only; comparing resourceVersions. |
| 2026-10-24 | The runtime declaration is a pod-template env var; defer immediately when it is absent. | Gives the worker an input the evidence document does not supply, and means default-off costs no wait. | Comparing the document only against itself. |
| 2026-10-24 | Initially crun 1.30.1 only (extended by the 2026-10-29 decision below); storage restricted to ext4/xfs with attach enforcement. | Namespace creation was then read only at 1.30.1. A lock fences nothing on shared filesystems. | Accepting the range 1.26-1.30.1; accepting any RWO claim. |
| 2026-10-29 | Extend the crio-crun declaration to crun 1.28 or 1.30.1, with all other prerequisites unchanged. | The namespace-creation and state-output paths were re-read at upstream 1.28; the matching el10 SRPM has no patches and its relevant sources are byte-identical to that tag. | An unexamined continuous version range; changing nodes to satisfy the original bound. |
| 2026-10-31 | Per PRD #3431's owner option A, the runtime value is the attestation of the full audited runtime/storage contract (needed for future hosted-Docker deletion); evaluation stays on without it. Evidence is versioned: v2 adds the successor's container IDs and a controller/worker runtime-prefix cross-check, with an exact kernel allowlist for crio-crun; v1 is legacy evidence with no authority. | Refines the 2026-10-24 declaration row: the namespaced API cannot establish the full audited tuple, so the declaration stays the operator's attestation while observations can only contradict it. | Autodetected proof from incomplete API facts; silently upgrading v1 evidence. |
| 2026-10-24 | Refuse exit codes below 0, 137 and 255; exact status-name sets; defer on ephemeral containers; fix `dind-meter` first (#3421). | M0's dead-shim follow-up; the CRI-O dead-conmon -1 case; a count alone can hide a missing entry. | A node agent; a threat-model amendment. |
| 2026-10-24 | Support `crio-crun` beside `containerd-runc`. | Source audit, and the maintainers' cluster runs CRI-O 1.36.2 with crun. | containerd only. |
| 2026-10-24 | Stopped containers are not removed. | M0 Q3; #3387. | Removing bound containers. |
| 2026-10-24 | Skip participation only for a filesystem type outside the allowlist, which every pod sharing the volume sees the same way. A missing or failing `flock` and any other lock failure fail closed; the image ships `flock`. `seed-nix` locks non-blocking and skips the record when busy. | A local "reclaim disabled" cannot stop a reclaimer that is already active, so failing open is safe only when no pod could have taken EX. A blocking init lock would serialize every overlap. | Failing open on any lock error (buddy BLOCK: T admits while S deletes); failing closed on unsupported storage (breaks today's starts). |
| 2026-10-24 | Non-closing PR; #3329 and #1860 close after PRD B's acceptance. | Real runtimes and attested versions can only be verified live. | Auto-closing. |
| 2026-10-25 | Replace the last-UID marker with immutable-inode append-only per-pod consumption: 4,096 UUID records, 151,560 logical bytes, 5 s ledger mutex, no eviction/reset. Overflow refuses evaluation/reclaim but never mandatory epoch participation. | Lead/buddy-approved correction: T must not erase S's admitted history; bounded storage must fail closed for evaluation. | Replaceable last UID or rolling history that can rearm S. |
| 2026-10-25 | `/events` is only a healthy-open-stream plus `since` replay veto/abort guard, never positive exclusion proof. | Docker flushes 200 before subscription; writer exclusion rests on no-client interval, restore/restart-policy predicate and stopped state. | Treating headers or silence as a live-subscription/exclusion acknowledgement. |
| 2026-10-25 | Scope early ordering to worker access in `main()`, preserving the read-only maintenance bootstrap `/proc` snapshot first. Inventory and SH-protect application-owned pre-Node writes; record EX stays nonblocking, protected init SH may wait. Use container-private bootstrap temp storage and preserve data-root fsGroup posture. | Existing migrations, jobs repair, sentinels, scratch and token operations write before Node; main-only ordering cannot fence them. Kubelet/CSI ownership work is a separate supported-storage prerequisite, not covered by SH. | Claiming pre-Node initialization is read-only or exempting alias-sensitive writes without exclusion. |
| 2026-10-25 | Restart discards unsubmitted handoff decisions; an already-submitted patch may commit only to its exact successor UID/resourceVersion, subject to worker validation. One request attempt per epoch. | API-server writes already submitted cannot be retracted by controller cancellation/restart. | Guaranteed revocation after submission, replay or retargeting. |
| 2026-10-25 | Apply this approved PRD/Decision Log amendment as the first implementation commit, before code. | Planning worker prohibits file writes; owner explicitly moved the amendment after plan approval and required its exact text in the plan gate. | Claiming the branch PRD was amended during read-only planning. |
| 2026-10-25 | Owner confirmed the Docker-lane startup contract: preserve data-root gid 10001 and setgid instead of resetting them, refuse startup on mismatch, and protect pre-Node writes with SH. | Reaffirms the approved section 1a posture and exclusion design; implementation and live verification remain pending. | Resetting the data-root gid or allowing protected pre-Node writes outside epoch ownership. |
| 2026-10-25 | Relocate M1 real-image proof to the lander on a storage-capable host, for both base and JVM images at the final exact reviewed PR head; exact-head CI supplements it. Continue all non-image work and gates serially. | Repeated image-unpack ENOSPC on this worker; no image build or extraction retry here. Proof remains required, M1 stays partly unverified, and a formal partial decision naming actual milestone IDs and the outstanding image-proof portion plus M3 is still required before delivery. | Waiving image proof, substituting CI alone, or inferring a partial-completion decision from operational guidance. |
