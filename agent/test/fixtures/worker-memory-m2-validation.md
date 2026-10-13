# Worker memory guard — M2 core validation in progress

M1 is complete. This records the reviewed M2 adapter core and repairs through `791c7c490d8c48715a5bb61039224756cdd00a6c`. **M2 remains incomplete.** A bounded real registered-supervisor proof now passes for adopted detached descendants, larger-tree selection, sibling survival and matching tool feedback. Remaining rejection/race proofs and fix-absent integration evidence have not all been executed. Worker/runner begin/retire, preservation and production lifecycle wiring belong to M3 and remain unimplemented. Calibration remains unverified; default production enablement stays disabled. The M1 reconciliation record describes its completion-time state, not later M2 progress.

## Committed core and repairs

- `5241e7d20831fdf82cc1ecdfe2ea669095d191e8`: exact decimal uint64 process ticks; broker-created model-shell metadata after authorization/screening; private metadata binding to the captured registry/context/flight/revision; Codex root/child input contract; supervisor-attested observations retaining the original primary identity; matching structured tool feedback/projection and bounded cleanup/output/outcome waits. Internal probes and absent trusted metadata do not enroll. This is an adapter seam, not production runner enrollment.
- `928f44601a5e625e22ce786b17d5871e531ab9a8`: fixes pressure feedback accidentally granting ordinary disposal on denied/revoked/retired reservations, redacted callback ID mismatches, stale enrollment after late clean reap, absent-primary collisions with unobserved enrolled siblings, and fail-open captured cleanup timeout. It also reconciles the inherited launcher tokenizer lint without changing its grammar or canonical recipes.
- `50cf4a481616515fd189cd810e0af6b95df1ba24`: closes the same cleanup-timeout admission gap for ordinary commands without a captured memory context. Registry poison is sticky and epoch-local; uncertain roots remain owned and late clean completion does not reopen admission.
- `368d770f` and `791c7c49`: update the two unconfirmed startup fixture producers to `{ pid, startTime }` children matching the existing wire contract. Intentional malformed numeric-child rejection inputs remain unchanged. No production contract or classification assertion was weakened.

Three implementation delegations were cancelled before returning reports. Their retained edits were inspected by the lead; only observed completed logs and subsequent lead/validator checks are counted. The incomplete full executor log `gate-log.F5V6Pb` is not a passing result.

## Executed local evidence

Scratch logs are run-local artifacts, not published portable evidence. No real OS subprocess attribution or private-cgroup measurement is inferred from these injected-handle tests.

| Candidate | Check | Observed result | Scratch log |
|---|---|---|---|
| core worktree before `5241e7d2` | Canonical `task typecheck:agent` | PASS | `gate-log.HedeqX` |
| `5241e7d2` source | Adapter unit file | PASS: 12, zero skips | `gate-log.Rxos9A` |
| core source | Registry / guard unit files | PASS: 17 / 55, zero skips | `gate-log.TDbPI5`, `gate-log.hYIPeX` |
| core repair worktree before `928f4460` | Canonical `task typecheck:agent`, `task lint:agent`, `task deadcode:agent` | All PASS | `gate-log.V80DaV`, `gate-log.v7VKGw`, `gate-log.nsqpgP` |
| core repair source | Adapter / registry / guard unit files | PASS | `gate-log.TpllQH`, `gate-log.piVzOn`, `gate-log.IOzlOD` |
| `928f4460` export, plain-mode regression added | Missing ordinary timeout fence probe | FAIL at the named admission assertion, not import/timeout | `gate-log.wKANNO` |
| ordinary timeout repair source | Adapter unit file | PASS: 16, zero skips | `gate-log.2O0Cwa` |
| `928f4460` | Canonical `task gate:agent` | FAIL: unit stage 13,730 passed, 1 failed, 14 skipped; stage 2,678s, gate EXIT=201 | `gate-log.oKzg4N` |
| `928f4460` export | Narrow startup classification case | FAIL at unchanged classification assertion | `gate-log.HIdQfE` |
| M1 base `5d8d673d` export | Same narrow case, equivalent dependency environment | Same FAIL | `gate-log.DOroea` |
| rejected-startup fixture repair source | Same narrow startup case | PASS: 1, zero skips | `gate-log.3kQKM8` |
| `791c7c49` source | Full startup-cleanup unit file | PASS: 32, zero skips | `gate-log.YR6I91` |

Focused Node commands used the documented `node --import tsx --test --test-timeout=120000` form, with name selectors where noted. Canonical gate flags, settings and concurrency remained unchanged. The full gate's one failure was `unconfirmed: real launcher classification survives soft expiry in logs and failed state` in `runner-codex-sinks-startup-recovery.test.ts`. Both candidate and base reproduced it in fresh exports. Diagnosis identified stale numeric children in fake transports; malformed evidence changed classification, the inner assertion masked it, and the boundary then reported unknown/AbortError. Correct identity-shaped negative evidence now passes the unchanged assertion. **The full canonical gate has not been rerun after these reviewed fixture repairs. Its failed result remains recorded.**

The first gate attempt stopped at dependency drift (`gate-log.ka9CV1`): worker provisioning preceded the default-branch merge and installed older versions. A targeted `npm install --ignore-scripts --no-save` updated the changed SDK, browser, Knip and Node type packages without deleting the installed directory or changing tracked manifests/lockfiles (`gate-log.wpe0ea`). `task deps-check:agent` then passed (`gate-log.KwxoZX`). The next gate stopped at inherited tokenizer lint (`gate-log.PT3nDo`), repaired before the full run above. `npm ci` was not used, and browser install scripts were not run.

## Bounded real-supervisor proof

`7aac7169cd27e94a83b8b557491b03b62b9cb811` adds a focused fixture under `e2e/worker-uid/`, outside generic agent unit shards. The subsequent module-format repair uses `.test.mts`, so NodeNext recognizes ESM outside the agent package. This fixture is not yet wired into CI; M5 owns the required worker-UID composition.

Lead execution after relocation passed one test with zero skips/cancellations and EXIT=0 (`gate-log.7RE7hM`, 4.131s). Three read-only validators independently passed fresh exports at `7aac7169` with real supervisors as command UID 10003, and reported no runtime/security defect. Tester identified the out-of-package `.ts` module-format failure in an additional scoped typecheck; the `.mts` repair addresses it. Lead then ran a scratch-config typecheck extending the agent's compiler settings with explicit installed Node type roots, followed by the focused `.mts` runtime proof: both passed (`gate-log.pJ53eI`, runtime 3.295s, one test, zero skips, EXIT=0). These are subprocess-test durations, not cgroup calibration or CI timing. Focused lint passed at the original file (`gate-log.OtcaLD`); an earlier invocation rejected `..` in its path (`gate-log.D6iqdL`), corrected with an absolute path. The first scratch-config invocation could not locate Node type roots (`gate-log.oj4XwU`); correcting that invocation produced the passing scoped check.

The fixture builds HEAD supervisor/sandbox helpers under a unique scratch directory, observes actual primary exit and adoption of detached leaves, compares aggregate RSS between two enrolled trees, requires ECHILD+__WALL drain evidence, verifies the identical sibling PID/start-tick set survives, and joins supplied structured pressure feedback into its exact broker tool result. Helper-profile injection, an ordinary-cleanup barrier, synthetic pressure totals/outcome and sandbox mode `off` are explicit seams. It does not prove the worker-to-command UID transition, automatic cgroup intervention, real kernel PID reuse, production M3 lifecycle wiring, or general concurrent-cleanup/rejection cases. No load calibration was performed. All owned supervisors and callback joins are bounded and cleaned within the fixture; missing UID/profile prerequisites fail explicitly rather than pass by skip.

## Immutable review evidence and remaining work

Reviewer/tester/auditor reviewed the core at `5241e7d2`; all five demonstrated core defects were mandatory regardless of their initial labels. The same three roles reported clean scoped closure at `928f4460`, with focused tests and mutation controls. All three subsequently reported clean closure of the ordinary timeout fix at `50cf4a48`. All three found no mandatory issue in the fixture range ending at `791c7c49`. Their tests used fake transports/injected handles; none are counted as real-process proof.

Tester at `791c7c49` suggested optionally pinning the standalone cleanup case's classification; it currently proves forbidden recovery while the Runner regression detects the stale-wire error. That optional strengthening is deferred. Generic optional tooling suggestions from tester reports (format checking, coverage thresholds and check-only pre-commit hooks) remain deferred; no new tooling gates are part of this core repair. The M1 auditor's separately recorded transaction-clock risk remains unverified/deferred.

Next M2 work must complete real supervisor-backed concurrent-cleanup, unowned/protected rejection and identity revalidation proofs, and complete the named fix-absent behavioral evidence without counting compilation failures or skips. Full production admission and preservation proofs remain M3 work. Repeat the canonical agent gate after clean repair review and focused validation, retaining actual results. M4/M5 and worker-owned measured local calibration duration/bounds remain pending. Post-publication before/after CI step/dependency-path timing remains lander-owned mandatory acceptance before merge, outside worker completion.
