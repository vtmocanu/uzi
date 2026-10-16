# Production cancellation routes (#3030)

Audited against 4712fa5a, with the repair in this directory. Scope is run
cancellation, including automatic stops and supersession that converge on its
worker consumers. Inventory of tracked production routes, not a new source of
publication, discard or cleanup authority.

Reproduce the discovery from the repository root with
`git grep -n -i -E 'cancel|stop_kind|supersed'`. Independently inspect
`api/internal/store/queries/`, migration UPDATE writers, all SubmitInput and
CreateStopVerdictInput callers, worker status/ownership consumers and executors.
Generated store Go mirrors the named SQL below. Context cancellation, cancelled
forge CI jobs, UI dialog cancellation, pause_cancel, controller maintenance
reservation cancellation and historical migration rollback are not run-cancel
producers. Text enumeration cannot prove absence of dynamically constructed
routes; new routes require updating this inventory and behavioural coverage.

## Dispositions

- **N**: no source admitted, or independently proved empty. Exact verified-empty
  settlement still requires existing journal/pin/retained-source checks and ACK.
- **T**: authoritative terminal state plus exact attributed code inventory.
  Stop the model immediately, freeze all owed roots and dirty work, preserve via
  held publication or archive, reconcile immutable FINAL, then independently
  prove cleanup. Failed/cancelled V2 is held-first; V1 preserves its negotiated
  failed-current-generation policy and archive fallback.
- **U**: unknown/lost/released/stale ownership, retained predecessor, residue,
  unproven writer or incomplete preservation. Keep source, journal, pins and
  custody. Cancellation, an abort, 404 or a closed hold alone grants no deletion
  authority. Wall parks and clarification remain protected when nonterminal.
- **H**: holdless lane. Stop its session/output work; do not create code custody.

Every admitted code route below converges on T or U; pre-source cancellation is
N only with the existing proof. A queued status alone does not prove emptiness.
One monotonic absolute 600,000 ms budget begins at the first cancellation
observation, includes queues/locks/Git/HTTP/FINAL/cleanup, and is never renewed by
another observation. Expiry retains evidence for a later bounded attempt.

## Producers and arbitration

| Production entry | Convergence | Preservation coverage |
|---|---|---|
| `api/internal/handler/runs_lifecycle.go` CreateRunInput; `api/cmd/uzi/run_lifecycle.go` newRunCancelCmd; generic CLI/TUI input submission; `web/src/pages/RunView.tsx` submit cancel | workersvc/submit.go SubmitInputWithOptions: live CreateStopVerdictInput, non-live CancelRunServerSide; N/T/U | runner-plan-gate.test.ts live cancel; runner-owner-cancel-v2.test.ts identical/dirty and refusal matrix; runner-unadopted-guarded-settle.test.ts preclone |
| `agent/src/uzi-tools.ts` cancelRun proposal; `api/internal/handler/chat.go` CancelChatRun; `api/internal/slacksvc/chatactions.go` cancelRun; `api/cmd/server/main.go` gateSubmitter.CancelRunFromCard | Owner-confirmed SubmitInput; proposal alone has no cancellation authority; N/T/U of target | runner-plan-gate.test.ts; runner-owner-cancel-v2.test.ts; proposal is not preservation evidence |
| handler/chat.go EndChat; workersvc/chat.go EndChat | SubmitInput cancel; H | steering.test.ts ChatSteering End chat |
| handler/v1_jobs.go V1JobCancel; workersvc/jobs.go CancelJob; workersvc/job_sweeps.go CancelRevokedProductJobs | SubmitInputWithOptions cancel; H during download/session/result upload | job-runner.test.ts all three owner cancel phases |
| forgesvc/mr_watch.go cancel adapter; workersvc/mr_rework_cancel.go CancelReworkForMR | Live CreateStopVerdictInput or direct CancelRunServerSide; N/T/U | runner-owner-cancel-v2.test.ts; recovery-inventory.test.ts terminal run-kind matrix |
| workersvc/autostop.go auto-stop live branch | cancel wire with auto_stopped verdict; failed preservation, no new discard policy | steering.test.ts epoch-exempt cancel; recovery-inventory.test.ts failed matrix |
| workersvc/service.go SetState stopped failure or stamped cancelled verdict | CancelRunByWorker; N/T/U; worker cannot request cancelled directly | runner-owner-cancel-v2.test.ts terminal ACK; runner-cancellation-deadline.test.ts |
| workersvc/service.go SetState BranchMoved | SupersedeRunByWorker for mr_rework/ci_fix; N/T/U; no stale-claim grant | recovery-inventory.test.ts kind matrix; runner-stopped-source.test.ts fencing |
| workersvc/forgepark.go parkForgeUnreachable; providerpark.go parkProviderOutage; diskpark.go parkDataVolumeFull; codex_account_runpark.go parkRunningCodexAccountTx | Locked stop takes precedence over park through CancelRunByWorker; N/T/U | runner-unadopted-guarded-settle.test.ts terminal ACK/probe and stale controls; runner-codex-sinks-vault-recovery.test.ts |
| workersvc/submit.go cancelPendingOutcomeRun | Row-locked CancelRunServerSideWithPendingOutcome after explicit confirmation; N/T/U; discard bit selects pending outcome, not source | runner-terminal-journal.test.ts first winner; runner-owner-cancel-v2.test.ts source preservation |
| Plan/code cross-check expiration, supersession and lead-exit triggers | Checker cancellation; H, parent source follows its own route | cross-check-runner.test.ts; isolated-runner.test.ts cancel/lost claim; recovery-inventory.test.ts holdless kinds |

### Current named SQL cancellation writers

This table is checked by cancellation-routes.test.ts against every query file.
Installed triggers are enumerated separately below.

| SQL file | Query |
|---|---|
| runtime.sql | CancelRunServerSide |
| runtime.sql | CancelRunServerSideWithPendingOutcome |
| runtime.sql | CancelRunByWorker |
| runtime.sql | SupersedeRunByWorker |
| runtime.sql | ExpirePlanCrossCheck |
| runtime.sql | SupersedeExitedPlanCrossChecks |
| code_cross_check.sql | ExpireCodeCrossCheck |

Migration writers (all copies, including superseded definitions and Down):
00158_self_improve_dedup_per_repo.sql is upgrade-time dedup, not a live producer.
00302_plan_cross_check_settlement.sql and 00312_plan_cross_check_rounds.sql define
settle_exited_plan_cross_check; 00312 contains the current Up definition.
00324_code_cross_check.sql and 00326_code_cross_check_interactive.sql define
settle_exited_code_cross_check; 00326 contains the current Up definition.
Down definitions restore the prior trigger, not an additional runtime route.
Checker triggers are holdless and cannot settle the lead's inventory.

## Worker consumers and preservation seams

Paths in this table are under agent/src; test names are under agent/test.
Shared convergence is runner.ts reportGenericFailure / phasePublish /
settleRecoveryGeneration -> settleGuardedInventory -> recovery.ts
operateFrozenInventory -> exact held/archive FINAL. Cleanup has separate guards.

| Entry or race | Disposition and convergence | Tests mapping the route to preservation |
|---|---|---|
| steering.ts SteeringChannel.route, markCancelled, waitForVerdict, askUser, followup | Sticky epoch-exempt cancel invokes observer before model/lifecycle abort; T/U | steering.test.ts D2b, cancel verdict, epoch-exempt, pause rearm; runner-owner-cancel-v2.test.ts dirty |
| steering.ts SteeringChannel.poll terminal_cancelled, endFencedFlight | Exact run/generation control installs a fence, observes the shared deadline before abort, and retains U without a competing terminal report or publication authority | terminal-cancelled.test.ts first budget before abort and no renewal; runner-cancellation-deadline.test.ts active/expired fenced source; runner-terminal-cancelled-sdk.test.ts resumed SDK stop |
| sdk-executor.ts pending/resumed plan, plan gate, implement loop, askUser, clarification, followup, refused wall | Static cancel/typed gate outcome -> generic failure; T/U | sdk-executor.test.ts existing clarification cancel; runner-owner-cancel-v2.test.ts clarification verifies committed and dirty objects |
| codex/codex-executor.ts plan gate, loop, clarification, abort, redrive, wall outcome | Stop effects; credential-free registries must settle before capture; T/U | runner-owner-cancel-v2.test.ts registry/late-writer; runner-codex-sinks-vault-recovery.test.ts cancel captures dirty/unfetched work |
| runner.ts phaseClone first running ACK, executeClaim RunningAckTerminalError | N only before source admission; settleUnadoptedGuardedClaim proves absence; U for stale/pre-existing source | runner-unadopted-guarded-settle.test.ts first running cancelled refusal, stale, journal/pin/clone/unconfirmed ACK |
| runner.ts reportState ACK wrapper, retainStoppedSource, retainedLifecycleGuard, requireRetainedOwnership, recoverRetainedClaim | Stale/released/missing generation or 404 retains U; server wall park remains nonterminal | runner-stopped-source.test.ts; runner-owner-cancel-v2.test.ts retained/unknown/wrong bindings/live; runner-cancellation-deadline.test.ts unproven generation |
| runner.ts enterCompletionHold POST / ownership probe | Observe exact cancelled response before caller resumes; T/U | runner-completion-hold.test.ts first cancelled completion hold POST/probe, malformed/unknown ownership controls |
| runner.ts enterWallPark report / lost-answer ownership probe | Cancel observes before return; admitted source T; unknown answer U; ordinary park stays nonterminal | runner-owner-cancel-v2.test.ts wallpark verifies nonempty pack; runner-wall-park.test.ts UNDELIVERABLE retains clone/HOME/no failure |
| runner.ts phasePreflightHandoff pause callback / buffered followup probe | Cancel stops before continuation; released/mismatched/unknown ownership U | runner-pause-park.test.ts cancel, sticky cancel and unknown ownership; runner-stopped-source.test.ts |
| runner.ts deferTerminalExecutionForDisk / reconcileForgeParkByProbe | Observe exact terminal cancel; N before clone with positive proof, otherwise U/T according to admitted inventory | runner-unadopted-guarded-settle.test.ts same-generation terminal probe and absent/mismatched generation controls; runner-terminal-disk-deferral.test.ts |
| runner.ts handleRecoveryExhausted, confirmRunningForCredentialPark, terminal-disk recheck | Capture before honest terminal report; exact ownership/custody required; unknown/404 retains U | runner-codex-sinks-vault-recovery.test.ts terminal ACK/loop ownership, failed capture, 404, cancel during vault park |
| runner.ts generic failure, phasePublish terminal drive, settleRecoveryGeneration / settleHeldDeferred | Every later settle uses same frozen identity; N/T/U, no refreeze after deferral | runner-owner-cancel-v2.test.ts; recovery-inventory.test.ts V1 deferred failed inventory without held policy |
| recovery.ts resumePending / resumeLive / frozenOperationCycle; worker.ts boot/live dispatch | Model-free T on exact original/live custody, including older terminal generations; unknown is U | recovery-inventory.test.ts boot/live cancellation, worker drift, closed holds, lost ACK and immutable FINAL |
| runner.ts executeClaim finally / clone and pin retirement | Preserve on blocked/unknown/expiry; ACK does not replace independent deletion proof | runner-cancellation-deadline.test.ts lock admission/journal read/expiry; runner-owner-cancel-v2.test.ts late-quiescence/late-writer |
| job-runner.ts watchCancel; steering.ts ChatSteering.route; isolated-runner.ts pre-session/session; cross-check-runner.ts terminal reconciliation | H; no widening of code publishing kinds or recovery holds | job-runner.test.ts download/session/upload; steering.test.ts End chat; isolated-runner.test.ts cancel and stale claim; recovery-inventory.test.ts holdless kind matrix |

Coverage mapping identifies shared behavioural seams; it does not claim a distinct
end-to-end test of each UI/API producer. API/SQL/trigger LiveDB tests and E2E phases
12-cancel-queued, 22-steer-queue-delivery and 51-auto-stop-poison
remain integration evidence for main to select and run. No full-stack, k8s or
LiveDB execution is implied by the focused agent tests.
