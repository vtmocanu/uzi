# Live Codex switching: per-seam proof ledger

Base: a233883de3c36bae2e940f999cd9d582889dad00.
Latest runtime fixture candidate: e7ee697e9b63abdfd754760926b4b7fca7990175.
Earlier R1–R6 fixture candidate: 47b0d8e374555c504be67ded2f4960f4a6c2e6d5.
Stopped-policy control candidate: 1cbcb917f156bfb8eb862f743acb1dde87a91a29.
Earlier stopped-authority control candidate: 2de82eafb2ace6b0a03c10de8f7a582b7b1d97bd.
Earlier stopped-guard controls: 76cc31aee1b0dafd644e9fa3fb7593442dc7ed6b.
Earlier Runner controls below used 6a7e7ae11b43c8fbf658d8b2fdcc557441c699c8
and the replacement-first adoption assertion as specified per control.
Results belong to their recorded revisions; this is not an exact-head full-suite claim.
Landing integration (2026-11-20) merges #4500's cross-worker Codex claim
restriction. Earlier cross-worker nonconformance traces below describe their
recorded revisions, not the integrated code. The remaining decision 11 gap is
same-worker new-incarnation recovery; see ADR-4201 and the validation record's
updated deferred checklist. Claude and forge-PAT gaps remain in ADR-4476.
This integration grants no additional proof credit. Current migration numbers
are 00496 (Codex epoch snapshot) and 00497 (pending switch); dated numeric
references below remain historical evidence.
The earlier [validation record](credential-switch-validation.md) retains
positive execution, review and migration evidence. Passing positive tests
alone does not satisfy the proof-method requirement below.

## Approved method

The restored owner decisions change proof methods only. No functional
criterion is accepted, waived or deferred.

- **(a)** Changed behavior: a narrow compiling mutation restores the exact
  a233 behavior; the focused candidate-export regression is red, and the
  unchanged candidate is green.
- **(b)** Identical historical behavior: quote the a233 guard and baseline
  role, candidate guard and reachable switch caller; remove that guard in
  a compiling candidate export and observe red through the actual switch
  path, with unchanged-candidate green. Label this a **preservation
  sensitivity control**, not pre-feature baseline evidence.
- **(c)** New ordering where a233 refused the whole operation: quote the
  historical refusal; relocate/reorder the particular call in a compiling
  candidate export, red through the real switch path and green unchanged.
  Label this an **ordering control**, not pre-feature baseline evidence.
- **(d)** New guard without a corresponding a233 seam: record exact baseline
  refusal/absence, then remove or neutralize exactly that guard in a compiling
  candidate export. The focused regression must be red through the real
  Runner/switch path, or the real API route and store transaction for server
  fences, with unchanged-candidate green. Label it a **guard control**, not
  pre-feature baseline evidence. Each negative case needs its own control;
  neutralizing more than one semantic fence together is invalid. The resolved
  redundant-fence method below permits enumerated copies of one fence.

Mixed clauses are split. An old helper does not establish historical
provenance for new callers, ordering or authority fences. Each required
clause needs qualifying evidence or remains unvalidated. Controls used one
candidate export at a time, removed immediately; no base was built for
these controls.

## Executed controls

The six earlier A/B controls below ran the real Runner case in
agent/test/codex-executor.test.ts:

```sh
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='M2 actualRunner api_key to subscription factory account resumes saved thread$' \
  test/codex-executor.test.ts
```

The test completes the actual A-to-B orchestration, rather than invoking
only a helper. The preservation controls and historical restorations each
passed `npm run typecheck` before execution. Each mutant executed one test,
failed it and exited 1; corresponding unchanged-candidate executions passed.
Exports contained the intended candidate test file. SHA256 fingerprints and
candidate-base revisions are recorded in the control logs. No mutant was
copied to the primary tree.

### A1 — live switching availability, class (a)

Historical `git show <base>:agent/src/runner.ts`, lines 25735–25738:

```ts
if (this.isCodexCapture(claim, flight)) {
  this.retainCaptureSource(flight);
  runLog.warn("Codex credential switch refused; source and session retained");
  return "retained_stop";
```

Candidate caller: CodexExecutor's `pauseNow.continueSwitch` invokes
`ctx.attemptCodexCredentialSwitch`; RunRunner supplies that callback at
agent/src/runner.ts:17289.

The export restores this conditional refusal before the callback's nonce
capture. It retains the same source and warning; `retain()` adapts the old
return to the callback's retained-stop exception contract. The real Runner
test fails waiting for switch cancellation/executor entry. This proves
sensitivity to enabling the operation only. It does **not** prove downstream
negative fences, custody invariants, or individual authority readers.

Evidence: .uzi/scratch/historical-live-refusal.log; unchanged green:
.uzi/scratch/historical-candidate.log.

### A2 — provider epoch reads the replacement binding, class (a)

Historical `git show <base>:agent/src/codex/codex-executor.ts`:

- Line 3023: `const binding = this.opts.binding;` captures the run binding.
- Lines 5226–5231: `buildProviderEpoch` destructures `binding` from the
  shared run context. Recreated provider epochs therefore retain that
  original binding.

Candidate `buildProviderEpoch`, lines 5676–5677, instead reads
`const { binding, committedGeneration } = authority;` and checks
`authority.assertCurrent()`.

The narrow export change restores `const binding = this.opts.binding;`
at that reader while keeping the candidate authority check and generation
cell. The selected old account is API-key mode, where the subscription
generation cell is not an old-account input. The real B credential-release
assertion fails: capability, auth mode, account and floor observations are
false, while A root/effect/fileop teardown observations remain true.

Evidence: .uzi/scratch/historical-original-provider-binding.log; unchanged
green: .uzi/scratch/historical-candidate.log. This credits the provider
construction binding reader, not independent scope, refresh, summary or
retired-authority fences.

### B1/B2/B3 — credential-free session filtering, class (b)

Historical `git show <base>:agent/src/codex/session-state.ts` and the
candidate file are byte-identical. At both revisions:

```ts
// isAllowedSessionArtifact, lines 257–262
const ext = extname(name).toLowerCase();
if (!SESSION_ALLOWED_EXTENSIONS.includes(ext)) return false;
if (SESSION_DENY_EXTENSIONS.includes(ext)) return false;
if (nameHasDenySubstring(name)) return false;
return true;
```

Subtree arguments in both revisions' `persist` (935–936) and `adopt`
(1028–1029) select `SESSION_ALLOWED_SUBDIR`, whose value is `"sessions"`.
The historical role was credential-free state preservation across provider
root recreation. The candidate switch reaches this same copying mechanism:
Runner suspends A and calls `a.persistRollout`; the executor's save path
calls the session store, and replacement provider preparation calls
`this.sessionStore.adopt` (candidate lines 5929/6017).

| Preservation sensitivity control | Narrow removal | Observed real Runner failure |
| --- | --- | --- |
| B1 filename | Remove only `if (nameHasDenySubstring(name)) return false;`. | Missing rejection: switch adoption excludes auth-named session artifacts. |
| B2 extension | Remove only the allowed-extension rejection; retain filename and denied-extension checks. | Missing rejection: switch adoption excludes non-session extensions. |
| B3 subtree | Replace the four sessions-subtree copy arguments with their held parent-fd roots. | Missing rejection: switch adoption excludes artifacts outside sessions subtree. |

B1/B2/B3 check the replacement destination before the initial destination,
so the failed assertion concerns B's adoption after actual switching.
B3 keeps fd-anchored traversal: parent-fd paths end in `/.`, and lexical
`resolve` normalization is bypassed for the copy arguments so it cannot
collapse that suffix back to the final magic symlink. Existing canonical
production arguments are unchanged by that mechanical accommodation.
No other file classification or traversal guard is removed.

Evidence: .uzi/scratch/preservation-session-name.log,
preservation-session-extension.log and preservation-session-subtree-valid.log.
Unchanged candidate: preservation-candidate-final.log, typecheck exit 0,
one test passed, exit 0. Earlier subtree prototypes failed with
`source-not-directory` before switching and are excluded.

These rows credit only the preserved copying guards. The new switch
caller's availability, strict persistence result and teardown ordering are
separate clauses.

### B4 — captured summary revision, class (b)

Historical `git show <base>:agent/src/codex/codex-executor.ts`, 2437–2442,
and candidate `assertCaptureAdmission`, 2520–2525, contain:

```ts
this.lifetime.assertOpen();
if (this.unstartedLegClosed || this.captureOwners || revision !== this.captureRevision) {
  throw new TrustedExecutionRefusal("codex epoch admission closed for capture");
}
```

Historical `ownSummaryFactory` called the guard at admission (2852) and
from its retained owner guard with the captured revision (2871). The
candidate's real B factory deliberately presents a stale capture revision,
calls its actual owner guard and restores the revision synchronously
(test lines 33435–33444). Its assertion requires `"capture-refused"`.

The export removes only the revision comparison, retaining lifetime,
unstarted-leg and capture-owner checks; `void revision` preserves compilation
without adding behavior. The test fails at **B summary rejects stale capture
revision**, observing `"admitted"`.

Evidence: .uzi/scratch/preservation-capture-revision.log; corresponding
unchanged green: preservation-initial-candidate.log. This control tested the
original 6a7e7ae11 test ordering; the later adoption-assertion ordering does
not change the earlier B capture probe. External Now ownership wiring and
new credential-identity checks are **not** credited by this preservation row.

### C1/C2 — already recorded ordering controls, class (c)

The [earlier record](credential-switch-validation.md#real-runner-ordering-controls)
contains exact a233 refusal lines and the two compiling call relocations:
A reconciliation after persistence, and executor suspension before Now drain.
C1 is executor suspension moved ahead of Now drain; C2 is A reconciliation
moved after persistence. Both failed the six real Runner cases; unchanged candidate passed.
These proofs remain credited for those exact orderings only.

### D1–D24 — stopped route guard controls, class (d)

The lead completed `.uzi/scratch/stopped-guard-controls.py`, exit 0, at
76cc31aee1b0dafd644e9fa3fb7593442dc7ed6b. The manifest
`.uzi/scratch/stopped-guard-results.json` records exact changes and logs.
These are **guard controls**, not pre-feature baseline evidence. All 24 use
the exact shared refusal/absence evidence in the baseline section below.

The fixture in api/internal/handler/codex_switch_stopped_guards_livedb_test.go
uses authenticated WorkerRoutes, negotiates a real registration nonce, seeds
a Codex binding and journal, and stamps the switch through the owner service.
Real HTTP preflight latches epoch 41; HTTP withdrawal clears the pending
switch; `/state` reaches the service with the pool as transaction beginner.
`epoch_applied_successor` instead applies through HTTP, commits epoch 42,
and reports the retained 41. Snapshot comparisons cover the full run,
base/switch journals, messages, inputs and custody rows. First settlement
permits its specified status/timestamps and health reset; replay compares
without those exclusions. The fixture seeds stalled health and checks its
reset, and checks the committed minimal acknowledgement. These test repairs
were reviewed clean at the recorded revision.

Each distinct negative used its own disposable candidate export and log;
exports ran serially and were removed immediately. Source query
api/internal/store/queries/codex_switch_stopped.sql and generated counterpart
api/internal/store/codex_switch_stopped.sql.go changed together, removing or
neutralizing one behavioral guard. No base builds or installations were used.
The wave recreated its owned PostgreSQL 17 fixture and removed it by exact
name with `docker rm -fv` after completion.

From each export's api directory:

```sh
GOFLAGS=-p=1 GOMAXPROCS=4 go test -buildvcs=false -race -count=1 -json -timeout=5m \
  ./internal/handler -run '^TestWorkerCodexStoppedGuardsRouteLiveDB$/<selected>'
```

For 23 mutants, `<selected>` includes both the negative case and
`fresh_and_replay`; the latter passed as an independent SQL-prepare control.
Only `replay_update` omits it because that mutation deliberately breaks the
same replay invariant. Each negative compiled and exited 1 at its intended
assertion, with no SQL-prepare/build failure credited. Unchanged before
`.uzi/scratch/gate-log.oduxq5` and after `.uzi/scratch/gate-log.COd4tJ`
each exited 0 with 25 subcases passed and no skips.

In the table, E neutralizes numeric epoch equality; P removes just the named
input kind from the pending-input anti-join; B replaces positive remaining
budget with a non-NULL check; C replaces the original paused clock with the
current clock; W removes `e.status <> 'paused'` from the update guard.
E/P/B produced `refusal mismatch: status=200 applied=true` (22 cases).
C produced `stop acceptance mismatch: status=409 applied=false`;
W produced `snapshot changed in run`.

| Case | Guard | Individual log |
| --- | --- | --- |
| `epoch_running_stale` | E: epoch equality | `.uzi/scratch/gate-log.ogXTFm` |
| `epoch_running_mismatched` | E: epoch equality | `.uzi/scratch/gate-log.c50m7z` |
| `pending_running_pause` | P: pause | `.uzi/scratch/gate-log.bWq6SF` |
| `pending_running_pause_cancel` | P: pause_cancel | `.uzi/scratch/gate-log.UWNi71` |
| `pending_running_cancel` | P: cancel | `.uzi/scratch/gate-log.KZmCnX` |
| `pending_running_stop` | P: stop | `.uzi/scratch/gate-log.nSMZWr` |
| `pending_running_scope` | P: scope | `.uzi/scratch/gate-log.hXkvEu` |
| `pending_running_revise_plan` | P: revise_plan | `.uzi/scratch/gate-log.CCYkJW` |
| `pending_running_reject_plan` | P: reject_plan | `.uzi/scratch/gate-log.R4pGvP` |
| `pending_running_resume` | P: resume | `.uzi/scratch/gate-log.zBOSyJ` |
| `epoch_paused_stale` | E: epoch equality | `.uzi/scratch/gate-log.mkAz6B` |
| `epoch_paused_mismatched` | E: epoch equality | `.uzi/scratch/gate-log.zKToni` |
| `pending_paused_pause` | P: pause | `.uzi/scratch/gate-log.EvnbDg` |
| `pending_paused_pause_cancel` | P: pause_cancel | `.uzi/scratch/gate-log.GlTnMJ` |
| `pending_paused_cancel` | P: cancel | `.uzi/scratch/gate-log.VQGklP` |
| `pending_paused_stop` | P: stop | `.uzi/scratch/gate-log.JCkZpL` |
| `pending_paused_scope` | P: scope | `.uzi/scratch/gate-log.AEcM5d` |
| `pending_paused_revise_plan` | P: revise_plan | `.uzi/scratch/gate-log.teLSlJ` |
| `pending_paused_reject_plan` | P: reject_plan | `.uzi/scratch/gate-log.w6NTYZ` |
| `pending_paused_resume` | P: resume | `.uzi/scratch/gate-log.cie8tb` |
| `epoch_applied_successor` | E: epoch equality | `.uzi/scratch/gate-log.kEvre2` |
| `paused_expired_budget` | B: remaining budget | `.uzi/scratch/gate-log.7ub1jo` |
| `paused_replay_original_clock` | C: original pause clock | `.uzi/scratch/gate-log.qzCiDa` |
| `paused_replay_no_write` | W: replay no-write | `.uzi/scratch/gate-log.wpJOcF` |

This credits five epoch cases, sixteen pending-kind/state cases and three
budget/clock/write cases. It does not credit the other stopped guards or
broader preservation and strict-wire clauses below.

### D25–D56 — stopped authority and wire guard controls, class (d)

The lead completed `.uzi/scratch/stopped-authority-controls.py`, exit 0, at
2de82eafb2ace6b0a03c10de8f7a582b7b1d97bd. The manifest
`.uzi/scratch/stopped-authority-results.json` records 32 individual
**guard controls**: 28 SQL running/paused cases and four wire cases.
The shared baseline refusal/absence quotes below apply; these are not
pre-feature baseline evidence. The matrix previously recorded as positive
at dda69dd2d now has qualifying evidence for these 32 seams only.

The authority fixture uses the existing authenticated WorkerRoutes and
pool-backed store transaction fixture. Generation/release cases require
no milestone reconciliation so the outer reconciliation check cannot mask
the SQL guard. Each mutant ran `fresh_and_replay`, which passed as an
independent SQL-prepare control. Each selected negative compiled and exited
1 at its intended assertion: SQL cases reported
`refusal mismatch: status=200 applied=true`; wire cases reported
`wire refusal status=200; want 400`. Unchanged before
`.uzi/scratch/gate-log.6TaSyI` and after `.uzi/scratch/gate-log.XaxRVc`
each exited 0 with 32 passes and no skips. The lead independently parsed
all logs and verified these counts; this documentation edit did not rerun
or independently parse those logs.

Each mutant used one serial candidate export, removed immediately. There
was no base build or installation. The owned database container was removed
by exact name with `docker rm -fv`. SQL controls changed
api/internal/store/queries/codex_switch_stopped.sql and
api/internal/store/codex_switch_stopped.sql.go together; wire controls
changed one condition in api/internal/handler/worker_state_decode.go.

From the export's api directory, each control used:

```sh
GOFLAGS=-p=1 GOMAXPROCS=4 go test -buildvcs=false -race -count=1 -json -timeout=5m \
  ./internal/handler -run '^(<target-top>|TestWorkerCodexStoppedGuardsRouteLiveDB)$/^(fresh_and_replay|<case>)$'
```

`<target-top>` is TestWorkerCodexStoppedAuthorityRouteLiveDB or
TestWorkerCodexStoppedWireAuthorityRouteLiveDB for the selected case.

Guard mapping (each change is limited to the named condition):

- `registration_nonce_mismatch`: `AND w.snapshot_register_nonce = sqlc.arg(register_nonce)::text` → `AND (sqlc.arg(register_nonce)::text IS NULL OR sqlc.arg(register_nonce)::text IS NOT NULL)`; generated: `AND w.snapshot_register_nonce = $5::text` → `AND ($5::text IS NULL OR $5::text IS NOT NULL)` (source/generated pair).

- `claim_generation_mismatch_no_reconciliation`: `AND r.claim_generation = sqlc.arg(claim_generation)::bigint` → `AND (sqlc.arg(claim_generation)::bigint IS NULL OR sqlc.arg(claim_generation)::bigint IS NOT NULL)`; generated: `AND r.claim_generation = $4::bigint` → `AND ($4::bigint IS NULL OR $4::bigint IS NOT NULL)` (source/generated pair).

- `claim_released_no_reconciliation`: `AND r.claim_released_at IS NULL` → removed (source/generated pair).

- `missing_frozen_secret`: `r.codex_secret_id IS NOT NULL AND` → removed (source/generated pair).

- `missing_capability_hash`: `AND r.codex_cap_hash IS NOT NULL` → removed (source/generated pair).

- `switch_requested_marker_only`: `r.credential_switch_requested_at IS NULL AND` → removed (source/generated pair).

- `switch_generation_marker_only`: `AND r.credential_switch_generation IS NULL` → removed (source/generated pair).

- `ordinary_hold_reason`: `r.hold_reason IS NULL AND` → removed (source/generated pair).

- `pause_requested_marker_only`: `r.pause_requested_at IS NULL AND` → removed (source/generated pair).

- `pause_mode_marker_only`: `AND r.pause_mode IS NULL` → removed (source/generated pair).

- `stop_kind_marker_only`: `r.stop_kind IS NULL AND` → removed (source/generated pair).

- `stop_reason_marker_only`: `AND r.stop_reason IS NULL` → removed (source/generated pair).

- `reserve_hold_marker_only`: `AND r.reserve_hold_reason IS NULL` → removed (source/generated pair).

- `status_claimed`: `AND r.status IN ('running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup', 'paused')` → removed (source/generated pair).

- `extra_known_typed_member`: `if len(fields) != 4 {` → `if len(fields) < 4 {` (Go decoder).

- `register_nonce_alias`: `if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {` → `if (!ok && key != "register_nonce") || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {` (Go decoder).

| Case | Guard mapping | Individual log |
| --- | --- | --- |
| `running_registration_nonce_mismatch` | `registration_nonce_mismatch` | `.uzi/scratch/gate-log.6boj2O` |
| `running_claim_generation_mismatch_no_reconciliation` | `claim_generation_mismatch_no_reconciliation` | `.uzi/scratch/gate-log.f1nAhn` |
| `running_claim_released_no_reconciliation` | `claim_released_no_reconciliation` | `.uzi/scratch/gate-log.gL1zDh` |
| `running_missing_frozen_secret` | `missing_frozen_secret` | `.uzi/scratch/gate-log.IeuXis` |
| `running_missing_capability_hash` | `missing_capability_hash` | `.uzi/scratch/gate-log.styewM` |
| `running_switch_requested_marker_only` | `switch_requested_marker_only` | `.uzi/scratch/gate-log.YkxFQN` |
| `running_switch_generation_marker_only` | `switch_generation_marker_only` | `.uzi/scratch/gate-log.JG7UIe` |
| `running_ordinary_hold_reason` | `ordinary_hold_reason` | `.uzi/scratch/gate-log.tzPIL4` |
| `running_pause_requested_marker_only` | `pause_requested_marker_only` | `.uzi/scratch/gate-log.xQtPMM` |
| `running_pause_mode_marker_only` | `pause_mode_marker_only` | `.uzi/scratch/gate-log.dnXwC9` |
| `running_stop_kind_marker_only` | `stop_kind_marker_only` | `.uzi/scratch/gate-log.YUGXPk` |
| `running_stop_reason_marker_only` | `stop_reason_marker_only` | `.uzi/scratch/gate-log.3pOAZu` |
| `running_reserve_hold_marker_only` | `reserve_hold_marker_only` | `.uzi/scratch/gate-log.jDkcyp` |
| `running_status_claimed` | `status_claimed` | `.uzi/scratch/gate-log.4mRH00` |
| `paused_registration_nonce_mismatch` | `registration_nonce_mismatch` | `.uzi/scratch/gate-log.x40v0L` |
| `paused_claim_generation_mismatch_no_reconciliation` | `claim_generation_mismatch_no_reconciliation` | `.uzi/scratch/gate-log.nMElyG` |
| `paused_claim_released_no_reconciliation` | `claim_released_no_reconciliation` | `.uzi/scratch/gate-log.pA0xe8` |
| `paused_missing_frozen_secret` | `missing_frozen_secret` | `.uzi/scratch/gate-log.DAChl9` |
| `paused_missing_capability_hash` | `missing_capability_hash` | `.uzi/scratch/gate-log.c4CEhL` |
| `paused_switch_requested_marker_only` | `switch_requested_marker_only` | `.uzi/scratch/gate-log.afHy5F` |
| `paused_switch_generation_marker_only` | `switch_generation_marker_only` | `.uzi/scratch/gate-log.Vsbxdw` |
| `paused_ordinary_hold_reason` | `ordinary_hold_reason` | `.uzi/scratch/gate-log.MutQ16` |
| `paused_pause_requested_marker_only` | `pause_requested_marker_only` | `.uzi/scratch/gate-log.JXtsMD` |
| `paused_pause_mode_marker_only` | `pause_mode_marker_only` | `.uzi/scratch/gate-log.bkdWPu` |
| `paused_stop_kind_marker_only` | `stop_kind_marker_only` | `.uzi/scratch/gate-log.TSyNjf` |
| `paused_stop_reason_marker_only` | `stop_reason_marker_only` | `.uzi/scratch/gate-log.krvMMU` |
| `paused_reserve_hold_marker_only` | `reserve_hold_marker_only` | `.uzi/scratch/gate-log.4e2tu9` |
| `paused_status_claimed` | `status_claimed` | `.uzi/scratch/gate-log.y97Jrq` |
| `running_extra_known_typed_member` | `extra_known_typed_member` | `.uzi/scratch/gate-log.0OUEmN` |
| `running_register_nonce_alias` | `register_nonce_alias` | `.uzi/scratch/gate-log.mYA3cG` |
| `paused_extra_known_typed_member` | `extra_known_typed_member` | `.uzi/scratch/gate-log.KM3efC` |
| `paused_register_nonce_alias` | `register_nonce_alias` | `.uzi/scratch/gate-log.J9FvFc` |

These controls do not establish the whole stopped predicate, ownership,
lane/policy exclusions, worker-latched bounds, other statuses, op_id,
minimal acknowledgement or broader preservation.

## Clause accounting

The rows below split mixed requirements. Positive coverage is recorded in
the linked validation record; **unvalidated** means the required qualifying
control is still absent, not that the functional criterion was deferred.

| Clause | Qualifying control status |
| --- | --- |
| Live switch can proceed instead of whole-operation refusal | A1, availability only |
| Recreated provider reads B instead of immutable original A | A2, selected API-key A to subscription B path |
| Session filename and extension filtering | B1, B2 |
| Sessions-only subtree confinement | B3 |
| Stale captured summary revision remains refused | B4 |
| Now raw drain before executor suspension | C1 in earlier record |
| A reconciliation before strict persistence | C2 in earlier record |
| Explicit owner Codex request, target ownership/kind and live-status admission | A4 credits request availability in four live statuses; U1 credits queued/paused/limit_wait/recovery_wait/claimed refusals, target usability and disabled target, the combined kind fence, a foreign API-key target, a foreign run and client-side CLI/web kind scoping. pool_wait, terminal statuses, pinned-without-secret, lane, no-live-worker, unsupported-worker and foreign-subscription refusals remain unvalidated; policy is the separate row below |
| Same successful flight/generation/worktree with no new attempt, custody or release/reclaim | U2 effect-injection sensitivity controls (owner-extended decision 10, U2 only): claimRun, attempt directory, custody release, queued transition, worktree reassignment, through the real Runner success test; other effects and the fresh_B return not separately controlled |
| Strict usable persistence, clear confirmation and continuation on A before teardown | R1–R4 credit three failed-save/inspection variants and empty applied-ACK; U3 credits op mismatch, target, claim_released ownership before/after, combined nonce fence and no-remaining-pending guards; pending-absent-before, other ownership legs, lost-reply clear and reopen's other guards not controlled |
| Proven A root/tool reap before B material, login and model work; fresh CODEX_HOME | R5 (runner hold) and U4: executor reap proof for root and command-root failures, combined no-B-before-reap fence, distinct per-epoch HOME (b); in-flight tool callbacks, launcher must-not-exist and other phases not controlled |
| Whole operation-scope/refresh/identity/capability/summary replacement and retired-A rejection | A2 one reader; A3 scope namespace; R7 B dispatcher readiness; U20 executor switch admission (B preparation child, (d)); B refresh suspension (fail-closed downstream, timeout red) and Now/summary gating during publication not credited (Now's switchGate masked by an undetermined layer, most likely the Runner lifecycle abort, except on the preserve-only resume-refusal path; plan and intent summary behaviour tested by the #4573 leaves with the fix at 5288bdce6, but their individual-fence proofs, post-body attribution and I4 B-ordinal attribution uncredited, see U20 plan and intent summaries); R8 retired API-key A reconcile fence; U5 retired A refresh bridge pre-call refusal (P3), combined executor summary-guard refusal, B expected identity carried (b); A-account rejection, bridge post-call copy, assertRefreshAdmission and Now stale-result drops not separately controlled |
| Exact saved-ID resume; confirmed-refusal-only fresh fallback and lineage; preservation re-hold | R6 uncertain-error classification; U6 resume carries the saved id, lineage notice, three-copy preserve re-hold, crash missing-id disclosure (d) and preserve retained refusal with a saved conversation (b); confirmed-refusal classification (timeout), :4148 check and session_missing on the switch path not controlled |
| Policy episode rejection at request and under apply lock | D74–D77 owner request and post-READY apply; U7 episode starting before preflight (both arms); hold arm unreachable in live statuses by `runs_policy_hold_shape`, paused hold refused by status (u1-status1); concurrent lock-order contention not controlled |
| Refresh lease/intent exclusion, account lock ordering and boundary reconciliation before revoke | D78/D79 credit isolated A coordination-lease and rotating-intent server checks; U8 credits alias and enablement (old/target) lock presence and NOWAIT keyword, the account lock and its NOWAIT against a reverse-order writer, the busy mapping, intent count/lock (A's account) and real admitted-refresh lease/intent redundant fences, and the executor A reconcile inside reconcileA before teardown (c); per-account lock isolation, the target account's intent check, the refresh admission's binding recheck in isolation and global lock sorting not controlled |
| Post-switch release epoch and secret-hash authorization | B5/B6 preservation sensitivity controls below, through real worker apply and release routes; U9 credits the final reauthorization's presence (b) and placement after the material read (c) against a switch apply committed between the two authorizations (subscription A release path, service level); the API-key A path and the vault-locked branch after a switch not controlled |
| Transactional frozen binding rewrite, capability revoke/remint and post-commit material response | U10 credits the zero-row fences (binding, mint redundant ×3, journal, stamp, message, last_seq), the commit-error check, material returned only after commit (c) and the persisted new hash, with the existing error-injection rollback test; A's refusal after remint by B5/B6; per-statement server-error returns (argued equivalent), the empty-AccessToken check and the caller's deferred rollback not separately controlled |
| Ordinal-zero baseline, successor journal ordinal/capability relation and usage attribution | U11 credits typed baseline-missing and baseline-match refusals (preflight), exact ordinal N+1 and its restart per generation, journal epoch independence from the ordinal, and B usage attribution via the preserved clamp (b); per-field baseline comparisons, the epoch<=0 refusal, the uncommitted-ordinal and generation-scope clamp branches on the Codex path, and cross-generation spend joins not controlled |
| Atomic rollback, lost-reply replay, stale/foreign apply and target fences | U10 rollback; U12 credits the Codex replay answer and its redundant no-material copies, the new-generation replay fence (b) and the non-owner ErrRunNotOwned ownership query (b; the refusal itself has at least four copies, not isolated), all through apply; and nonce/generation/released/status fence conjuncts, op mismatch, target mismatch and both target-moved checks through preflight; preflight's own non-owner refusal, the validator replay copy, Step 1 worker check, apply Step 0 copies, other fence conjuncts, the stamp checks, target sub-checks and old-alias refusals not controlled |
| Stopped-support negotiation, immutable expected-epoch latch and unsupported-response obligation | U13 credits owner-request negotiation, stopped-capability and stale-heartbeat refusals, the negotiation capability conjunct and a nonce-mismatched apply registration, preflight worker_upgrade_required, client stopped admission (capability and feature), untyped-refusal retained stop, settlement after withdrawal, the frozen tuple and the successor epoch latch; the runner's own admission check, fence/Step 0 negotiation copies, null-heartbeat and registration sub-conjuncts, the in-place conjunct, identical-tuple replay, first-READY latch, the unchanged-stamp assertions and the worker-side worker_upgrade_required reason not controlled |
| Stopped numeric epoch equality: stale/mismatched running and paused, applied successor | D controls: five individually executed cases above |
| Stopped pending-input exclusion for eight named kinds in running and paused states | D controls: sixteen individually executed cases above; U14 credits the applied and superseded carve-outs, the stop-cutoff exemption, its kind restriction and its id boundary (running run; stop only); paused-run carve-out variants, cancel/reject_plan under the cutoff and the cutoff's run correlation not controlled |
| Paused replay rejects expired wall budget | D: paused_expired_budget; U14 credits the running-clock arm, interactive and unstarted exemptions, extension, finalize and paused seconds, and the NULL-wall coalesce; chat/judge exemption unreachable; the RunTimeout value, the future-started_at clamp and the production default not controlled |
| Paused replay uses original pause clock and performs no run update | D: paused_replay_original_clock and paused_replay_no_write |
| Stopped registration nonce equality | D25–D56: running/paused registration_nonce_mismatch; U15 credits the worker-latched generation, epoch and nonce bounds (lower, upper, inclusive edges; nonce empty/length/NUL at the route; UTF-8 supporting only, uncredited); worker ownership is credited in the claim-generation row; a NULL snapshot nonce not controlled; resume/reclaim/re-registration are the withdrawn X/Y row |
| Stopped claim generation and unreleased claim | D25–D56: running/paused generation/release without reconciliation; U15 credits the reconciliation path's generation and release refusals (two copies each, Go copy (b)) and worker ownership; the reconciled `*g <= 0` copy not controlled |
| Stopped frozen secret and capability hash presence | D25–D56: running/paused missing_frozen_secret and missing_capability_hash; U15 credits binding ownership as the composite FK (b); binding identity beyond ownership has no predicate clause and stays unvalidated (owner batch) |
| Stopped switch-request/generation, ordinary hold, pause-request/mode, stop-kind/reason and reserve-hold markers | D25–D56: individual running/paused marker cases; U16 credits a pending second switch (both markers together, redundant-fence) and the running policy hold (shape, Go and SQL copies); late competing controls are the next row |
| Stopped refusal of claimed status | D25–D56: running/paused status_claimed; U15 credits refusal of queued, limit_wait, pool_wait, recovery_wait, completed, failed and cancelled (running origin) |
| Stopped ownership, lane and policy fences | D57–D60 credit pending-fresh/unresolved-outcome; D61 paused policy-hold; D62/D63 stable user-owner mismatch running/paused. D64–D73 credit the five enumerated STOP lane exclusions running/paused; U16 credits concurrent late controls (worker-row by the worker lock, run-row by the run locks alone, (b)) and the running policy hold; paused-origin replay under concurrency and op-less withdrawals not covered; owner writers that take no worker or run row lock not covered |
| Stopped replay after withdrawn X/Y; resume/reclaim and incarnation changes | U16 credits refusal while Y is pending, the withdraw op fence against a delayed op-carrying X withdrawal ((b); op-less v1 withdrawals are unfenced by op), and refusal after resume (three copies), reclaim and re-registration; settlement after withdrawn X/Y is positive coverage only (no guard to remove) |
| Stopped preservation of binding, journals, session, gates, questions and custody without input consumption/time banking | E1 base-journal deletion, E2 applied-input deletion, E3 custody timestamp corruption, E4 owner-discard/live-reference loss, E5 direct paused-time credit, E6 applied-switch journal deletion; U17 effect-injection for session id, gate presentation id and row, question id and message on the first stop, and a run-row write (`updated_at`) and presentation deletion on replay; other gate columns and other replay tables compared but not individually injected; Slack, `cross_checks` and other trigger-closure tables outside the snapshot |
| Stopped known extra typed member and register_nonce case alias | D25–D56: four running/paused wire cases only |
| Bounded strict stopped request, other forbidden combinations, op_id and minimal acknowledgement | U18 credits the 1 MiB inclusive bound and the single-value rule (one copy each), op_id and unknown-field refusal (two copies each), expected_epoch on a running report (two copies), and the acknowledgement's four-key run object and 16 KiB bound under a 64 KiB plan; other statuses and the envelope key count not controlled; null/missing fields and case aliases beyond D25–D56 not controlled |
| Decision 11: no worker-side use of delivered Codex material before the predecessor proof (crash points 1–4) | U21 credits, for API-key A → subscription B on same-worker retained recovery, no executor-client release or refresh before the proof ((c) insertion, (b)) and, on survivors/unverified, no executor run and the `quiescence_failed` blocker ((d), (b)); bodies, launches and requests before the proof asserted only; "discarded" not shown observably (non-use only); other auth directions, the `kind` half of the gate, the re-hold variant and cross-worker recovery not controlled |
| Crash point 5: predecessor host unreachable | Owner decision 2026-11-18: recorded scope decision, unvalidated and uncredited (environmental limitation). Not enforced by the code: on a successor claim on another worker there is no retained journal, so the decision 11 gate (U21, same-worker retained recovery only) does not run and the run proceeds with the binding the claim delivers; see the case 5 record below. Owner partial decision 2026-11-19: m3 deferred; the cross-worker path is a known nonconformance, not an accepted residual (validation record, deferred m3 checklist) |
| Tokens absent from all inputs/acknowledgements/messages/logs | U19 credits B material's registration with the Runner's log and run-message redactors ((d), each separately; the executor's second log registration not controlled), the Codex apply route's log redaction (`LogValue`, (d)) and the stopped acknowledgement and log (effect-injection); the remaining surfaces are guardless by construction and go to the owner batch; m4 security gates remain required |

## Baseline absence and the class (d) method boundary

At exact base a233883de3c36bae2e940f999cd9d582889dad00,
api/internal/workersvc/service.go:5901–5902 refuses unsupported states:

```go
default:
    return store.Run{}, false, ErrInvalidState
```

api/internal/handler/worker_protocol.go:1605–1606 maps ErrInvalidState to
HTTP 400 with the exact refusal:

```text
state must be one of running, awaiting_approval, awaiting_input, awaiting_followup, limit_wait, recovery_wait, paused, pause_failed, credential_switch, credential_switch_failed, credential_switch_held, credential_switch_hold_settled, credential_switch_uncertain, clarification_settled, completed, failed
```

This list excludes `codex_switch_stopped`. Read-only `git show` confirms
api/internal/store/queries/codex_switch_stopped.sql is absent at that exact
base (path-not-in-revision error), and there is no corresponding stopped
numeric `expected_epoch` request shape. No base was built.

At a233, ordinary `SetRunPaused` (queries/runtime.sql:4426–4446) required
`status = 'running'`, `pause_requested_at IS NOT NULL` and a pause mode
in `('milestone', 'now', 'reserve')`; it cleared the pending pause fields.
The candidate stopped operation instead requires NULL pause request/mode,
consumes no ordinary pause input, accepts other live states and supports
paused replay. Substituting old ordinary-pause SQL is not a restoration of
the same protocol or effects.

Historical capability authorization (codexauthz.go:674–687) parsed an
embedded-epoch capability and checked its hash. The new stopped report
supplies numeric `expected_epoch`, not that bearer capability. Substituting
the old prelude introduces a different credential requirement.

The stopped numeric epoch equality, pending-input anti-join and paused-replay
eligibility/accounting are new non-ordering predicates. Whole-operation
refusal cannot discriminate those negative fences; their guards are not
historically identical, and they are not call-order requirements. The subsequent owner class (d) extension resolves this method boundary.
The 24 earlier and 32 authority/wire real-route guard controls above discharge the precise
credited seams, not this entire predicate. Other clauses remain required and
unvalidated; no functional criterion is accepted, waived or deferred.

Other unvalidated rows also remain worker obligations. m3 and m4 are **INCOMPLETE**.
Exact-head CI obligations and two-real-account owner acceptance retain
their existing classifications.

## Duplicated enforcement method boundary

At source revision dda69dd2d82a11bf123066771decd3b6c5cec53f,
api/internal/workersvc/codex_switch_stopped.go:32–40 locks the claim and
then rejects in Go before issuing the stopped SQL:

```go
locked, run, tx, err := s.beginClaimMutation(ctx, w, id, req.ClaimGeneration)
// error handling and deferred rollback
if !codexSwitchLaneEligible(run) || UnresolvedPolicyEpisode(run) {
    return run, false, nil
}
```

The worker lock precedes the owned run lock in
api/internal/workersvc/milestone_reconciliation.go:295–305; the transaction
is retained through the stopped query. The owned read in
queries/runtime.sql:12843 ends in `FOR UPDATE OF runs`. A concurrent writer
of that run cannot commit a changed policy or lane between the Go read and
the SQL predicate. No race-based separation is established.

The smallest duplicated case is pending fresh policy.
api/internal/workersvc/policy_hold.go:189–192 rejects
`r.PolicyPendingFresh`; queries/codex_switch_stopped.sql:19 also requires
`NOT r.policy_pending_fresh`. Removing only the Go term leaves SQL refusal;
removing only the SQL term leaves Go refusal. Unresolved-outcome enforcement
is similarly duplicated: Go checks
`r.PolicyOutcomeID.Valid && !r.PolicyResolvedAt.Valid`, while SQL:20 requires
`r.policy_outcome_id IS NULL OR r.policy_resolved_at IS NOT NULL`.

Lane exclusions are also duplicated. The Go helper in
api/internal/workersvc/credential_switch_codex.go:333–342 rejects a review
target and chat/judge/job/cross_check; SQL:15–16 repeats those exclusions.
The SQL general-hold check is broader than the Go policy-hold check, so an
ordinary non-policy hold remains an independently discriminating candidate;
it must not be treated as proof of the duplicated policy fences.

An independent architect source investigation at 5212bffae found no
reachable single-copy policy/lane control under methods (a)–(d). The lead
spot-checked the predicates and lock ordering. This is static feasibility
evidence only; the subsequent four executed policy controls are recorded below.
Whole-operation historical refusal does not isolate these negative fences,
and the duplicated checks are not an ordering relocation.

The owner has resolved the method question: one redundant-fence guard
control per negative case may neutralize **all and only the enumerated
copies of one semantic fence**. Record a copy census with file/line anchors,
the search scope, and the lock trace connecting the real route/Runner to
each enforcing copy. Unresolved copy coverage remains unvalidated.
The exact mutant diff must leave every other semantic fence intact;
neutralizing more than one semantic fence in a mutant remains invalid.

Each required control must compile and make the real route plus store
transaction, or real Runner assertion, red, with the unchanged candidate
green. Per-copy sufficiency controls are optional unless a frozen criterion
requires independent enforcement. Do not infer independent sufficiency from
a redundant-fence control. A single-copy mutant staying green is expected
redundancy, not proof of a defect.

The four policy controls below now supply bounded redundant-fence evidence.
Lane and other redundant fences remain unvalidated. No functional criterion
is waived or deferred. m3 and m4 remain **INCOMPLETE**.


### D57–D60 — stopped policy redundant-fence guard controls, class (d)

Receipt: `.uzi/scratch/stopped-policy-results.json`, candidate
1cbcb917f156bfb8eb862f743acb1dde87a91a29. Each negative used its own
serial export, removed immediately. Each compiled, exited 1 at
`refusal mismatch: status=200 applied=true`, and passed its
`fresh_and_replay` companion. Unchanged before `.uzi/scratch/gate-log.6IMNKT`
and after `.uzi/scratch/gate-log.pEa5Kp` exited 0, four selected policy
leaves passed without skips. The receipt contains the exact three-file
diffs and line anchors per case. These are **redundant-fence guard controls**,
not baseline restorations or independent per-copy sufficiency proofs.

| Control/case | Semantic fence | Individual red log |
| --- | --- | --- |
| D57 `running/policy_pending_fresh` | Pending fresh | `.uzi/scratch/gate-log.idNWSq` |
| D58 `running/unresolved_policy_outcome` | Unresolved outcome | `.uzi/scratch/gate-log.8WNFnH` |
| D59 `paused/policy_pending_fresh` | Pending fresh | `.uzi/scratch/gate-log.LYdXmH` |
| D60 `paused/unresolved_policy_outcome` | Unresolved outcome | `.uzi/scratch/gate-log.yInYkj` |

Copy census: two runtime copies, Go helper and executing SQL.
api/internal/workersvc/policy_hold.go:190 removes ` || r.PolicyPendingFresh`
for pending fresh; :190–191 removes the unresolved outcome term (and joining
`||`) for unresolved outcome. Executing generated
api/internal/store/codex_switch_stopped.sql.go:29/30 removes respectively
`AND NOT r.policy_pending_fresh` or
`AND (r.policy_outcome_id IS NULL OR r.policy_resolved_at IS NOT NULL)`.
The declarative pair in queries/codex_switch_stopped.sql:19/20 changes
mechanically with generated SQL; these are not three independent fences.
The per-case exact diffs leave the other policy term, hold, lane, authority,
input, status and budget fences intact.

Search scope: WorkerRunState → SetStateReportWithReconciliation →
setStateWithTx (service.go:4675–4676 early STOP dispatch, before the generic
policy guard) → stopCodexSwitch → UnresolvedPolicyEpisode and
SetCodexSwitchStopped. beginClaimMutation takes worker then owned-run
`FOR UPDATE` locks in READ COMMITTED (milestone_reconciliation.go:295–305);
the same transaction holds the row through Go and SQL, owned reread and
commit (codex_switch_stopped.go:32–53). The reviewed shape constraints in
migrations 00397, 00419 and 00420 permit isolated tested policy tuples;
they supply no independent sufficiency credit. This direct route/service/query
census does not exhaustively trace triggers or broadcast consumers.
These controls prove STOP settlement refusal only. Explicit owner request
and apply-under-lock policy rejection remain **unvalidated**.

### R1–R6 — individual real Runner safety guard controls, class (d)

Canonical merged receipt: `.uzi/scratch/qualified-runtime-controls.json`.
The lead checked candidate source hashes against `git show`, exact old/new
mutation hashes, typecheck exit 0, intended ERR_ASSERTION frames/red and
before/after greens, and verified the primary unchanged. This documentation
edit reads those receipts; it does not rerun gates. Exact source/old/new and
log paths are in that receipt and the raw manifests below.

Baseline a233 runner.ts:25735–25738 (quoted in A1) refused the whole Codex
operation; there was no historical allowed-operation guard seam here.
These are **guard controls**, not baseline restorations. Each individual
control used one export at a time, removed immediately, with provisioned
agent dependencies linked, no installs or base builds. Each mutant passed
`npm run typecheck` (exit 0), then the Node CLI with
`--test-timeout=120000` and the TAP-selected names in the receipt. Each
selected negative exited 1 with the intended assertion; unchanged before
and after exited 0. Log names below are relative to `.uzi/scratch/`.

| Control / specific selected case | Witness in codex-executor.test.ts | Candidate | Typecheck / red / before / after logs |
| --- | --- | --- | --- |
| R1 `persist-clear-confirmed` | :33922 failedClearRequests empty vs one required clear | 47b0d8e37 | gate-log.5kbOjb / gate-log.PGp5F7 / gate-log.axLCDF / gate-log.Fm09HO |
| R2 `inspect-missing-clear-confirmed` | :33922 same clear assertion, separate control/export | 47b0d8e37 | gate-log.7wioNQ / gate-log.FybLZz / gate-log.axLCDF / gate-log.Fm09HO |
| R3 `inspect-unknown-clear-confirmed` | :33922 same clear assertion, separate control/export | 47b0d8e37 | gate-log.teHcg0 / gate-log.i5eqF7 / gate-log.axLCDF / gate-log.Fm09HO |
| R4 `persist-clear-empty-reply` | :33934 continued_A vs undefined | 1cbcb917f | gate-log.mjzHkm / gate-log.X6HcoB / gate-log.dRY1bQ / gate-log.8Z2C5h |
| R5 API-key A → subscription B, registered A root reap unproven | :34099 applies 1 vs 0 | 2a8fc55d2 | gate-log.6FfDlX / gate-log.s3yzFp / gate-log.7YV83H / gate-log.uPNNmH |
| R6 uncertain B resume failure without fresh fallback | :34724 forbidden thread/start count 1 vs 0 | 47b0d8e37 | gate-log.T54qde / gate-log.GSXHpM / gate-log.WiQMUx / gate-log.8RW7CW |

R1–R3 each remove only runner.ts's failed-save block
`if (saved.kind !== "usable" || !saved.sessionId) { ... }`, retaining the
save ternary. Raw receipt: runtime-safety-results-persist-per-case.json.
R4 removes only `if (ack.applied !== true) return false;`, adds `void ack`,
and keeps reconcile/owned/nonce/pending checks; raw attempt2 receipt.
R5 removes only the `if (!reaped)` held block and adds `void reaped`;
raw attempt3 receipt. It credits root reap pre-apply, not tool-only reaping.
R6 changes codex/codex-executor.ts's `error instanceof CodexResumeError`
to `Boolean(error) || !error`; raw attempt5 receipt. Confirmed-refusal and
wire preserve-only companions passed, supplying no independent preserve-guard
credit. Coverage is limited to the named modes/phases, not all consumers.

Reviewed committed source-fixture repairs: 2a8fc55d2 adds observation of
bad-apply race settlement; 85bc adds observation of bad fresh fallback;
47b0d8e37 skips the success-only prepared latch for resumeVariant failure
fixtures because a proper uncertain error throws. The old mutant was stuck
on that fixture latch. At the same content as 47, lint
`.uzi/scratch/gate-log.14E9jE`, typecheck `.uzi/scratch/gate-log.7H3tkW`,
and eight selected cases `.uzi/scratch/gate-log.X4rnyH` passed, as reported
by the lead. These are focused fixture gates, not full-suite evidence.

Excluded: stripped Git ENV harness ownership failure before tests; ACK
unused-variable mutant (typecheck gate-log.6qc_xh88); old root
gate-log.uIV7Ng timeout; resume attempt3/4 timeouts; and the first mixed
three-persist-case run, superseded by separate R1–R3 controls. The mixed run
was assertion-red but is not credited as the individual controls; compilation
failures and timeouts are not assertion-red proofs. Raw failed-attempt manifests are retained. No overall failed
attempt script is credited as PASS; credit is individual with before/after green.

## Preservation effects — source census and subsequent method amendment

Architect source analysis distinguishes a closed RUN six-column write
projection from preservation of other tables (no journal/input/custody writes).
Widening the RUN allowlist could plausibly be a class (d) control analogous
to B3, but has not executed. The direct stopped SQL writes only RUN status,
status_since, updated_at, health, health_reason and health_since; after the
query the service performs owned reread, commit and broadcast. There is no
existing journal/input/custody write guard to remove at that seam. Adding a
DELETE/UPDATE introduces a new forbidden effect rather than neutralizing an
existing guard. Baseline refusal is not preservation proof; ordinary pause
consumes controls at a different seam and does not establish class (b).

A subsequent read-only DBA census at 47b0d8e37 traced valid four-field
reports through the common wrapper, effective Goose Up triggers through
466, and configured PublishState consumers. It found no effective writer to
run_credential_epochs, run_credential_switch_epochs, run_user_inputs or
recovery_custody_holds on this accepted STOP path. The wrapper's registration,
checkpoint and source-publication payloads are absent; completion replay
requires the completed state (reconciliation.go:148 and the publication
helpers). Handler acknowledgement construction adds no state mutation.

The closest custody writer is the completion-publication stamp (319 Up),
but it requires completed status and a non-NULL final head; the 320 Up run
guard clears that head on paused. A separate 318 publication-identity guard
also applies. Removing only a pause/status exclusion therefore does not
make this preservation assertion red. These completion-evidence predicates
are not established copies of one pause-exclusion fence. The baseline already
contained the 319/320 guards; they do not supply a class (b) substitute for
the new operation's absence of writes.

This is not a claim that STOP has no indirect effects. The traced trigger
closure can write forge_origin_cooldowns, cross_checks and other runs fields
(432 and 392 Up). The configured MultiBroadcaster (server/main.go:707)
combines hub.PublishState with Slack notification; Slack may update
slack_run_messages, including presentation anchors. Those are distinct from
canonical gate state in runs.gate_* and run_gate_presentations. Question
identity is runs.open_question_id; question content is run_messages with
kind=question. No separate questions table is invented. Complete trigger,
accounting and presentation behavior remains a worker validation obligation;
this source census is not new red/green evidence or a database-effects waiver.

Proposal at 4db59ee48, **not approved at that revision**: one compiling mutant per named
preservation seam introduces exactly one narrow forbidden effect through the
real route/store or Runner, leaving other fences/effects intact; the relevant
preservation assertion must turn red and the candidate stay green. This is
not guard removal authorized by the current methods. Formal owner method
clarification was requested for that proposal; no functional waiver is sought.
The conclusion is bounded to the traced accepted route, trigger closure and
configured consumers, not every code path or deployed schema. Preservation
rows stay unvalidated and m3/m4 remain **INCOMPLETE**.


### E1 — base-journal deletion effect-injection sensitivity control

The subsequent owner amendment permits one narrow forbidden effect per named
preservation seam where the traced route has no existing write guard to remove.
This changes the proof method only: no functional criterion is waived or deferred.
Label this evidence **effect-injection sensitivity control**, not historical
restoration, guard removal or proof of absence of every other effect.

Candidate: 4db59ee485d42b251e438fa63c5315aa17991ec0. Named seam:
first accepted stopped settlement's base credential journal preservation.
The sole injected effect is deletion from run_credential_epochs for the run
returned by the existing changed CTE. Source query :40 and generated query :50
both add the same one data-modifying CTE; this is one executing effect, not two:

```sql
), forbidden_effect AS (
    DELETE FROM run_credential_epochs WHERE run_id IN (SELECT id FROM changed) RETURNING run_id
```

All eligibility guards, six RUN assignments and response expressions remain
unchanged. The existing fixture records a base epoch at
codex_switch_stopped_guards_livedb_test.go:83–88. The real authenticated state
route reaches stopCodexSwitch, then SetCodexSwitchStopped under worker/run locks;
its accepted first transition populates changed, so the deletion executes before
transaction commit. The whole-row base snapshot at :133 is compared after the
route response. The deletion control does not execute on an already-paused replay.

Receipt: .uzi/scratch/stopped-journal-effect-results.json, including exact diffs,
source/mutant hashes, command, leaf actions and PostgreSQL execution trace.
Unchanged before gate-log.5OuGIS exited 0; mutant gate-log.AJJe8D exited 1 at
:217, `snapshot changed in base`; unchanged after gate-log.QnDTeg exited 0.
All log names are under .uzi/scratch/. The selected leaf was
TestWorkerCodexStoppedGuardsRouteLiveDB/fresh_and_replay with no skips.
Command, from api, with GOFLAGS=-p=1 and GOMAXPROCS=4:

```sh
go test -buildvcs=false -race -count=1 -json -timeout=4m ./internal/handler -run '^TestWorkerCodexStoppedGuardsRouteLiveDB$/^fresh_and_replay$'
```

The compiling mutant failed the preservation assertion, not a SQLSTATE, timeout
or build failure. Database trace gate-log.51NJff records the base insertion,
mutant statement and subsequent unchanged settlement/replay statements. The
lead checked the receipt and corresponding log events. Two preparation errors
(query terminator comparison and catalog query before migration) preceded mutant
execution and are excluded from proof. One export was used at a time and removed;
the exact recreated PostgreSQL fixture was removed with exit 0. No primary source
mutation, installs or base builds occurred.

E1 proves this assertion detects this journal deletion at this first-settlement
seam only. It does not discharge switch-journal, input, custody, accounting,
canonical gate, question or replay-effect preservation. Those clauses and all
other unvalidated rows remain required; m3 and m4 remain **INCOMPLETE**.


### E2–E3 — populated input and custody timestamp effect controls

Candidate: 8ed61e8a2f088f16cb5f00509b8fd7a840c4a076. New fixture
codex_switch_stopped_preservation_routes_livedb_test.go:12–104 supplies separate
applied_input and open_custody cases. Each verifies owner/current claim fields
and the exact seeded row in a nonempty snapshot before first STOP and replay.
The applied pause has applied_at and receipt fields populated, so it is not an
unapplied competing input. The open inventory-guarded custody row keeps its
worker/run pointers and NULL release/publication evidence. The unchanged real
route passes both first settlement and replay without relaxing any guard.

These are **effect-injection sensitivity controls** under the preservation
method amendment. Each export adds one CTE after changed in the source SQL
and executing generated counterpart (same query, one effect). All predicates,
six RUN assignments, fixtures and response expressions remain unchanged:

| Control | Named seam / only injected effect | Intended assertion / red log |
| --- | --- | --- |
| E2 | Applied-input preservation at first accepted STOP; DELETE run_user_inputs for changed run IDs, kind=pause, applied_at non-NULL | :61 `snapshot changed in inputs`; gate-log.p1Rpfr |
| E3 | Custody released_at preservation at first accepted STOP; UPDATE open, unreleased holds for changed run IDs to released_at=2000-01-01 UTC | :101 `snapshot changed in custody`; gate-log.u8Buka |

E3 changes only the timestamp. It does not perform a custody lifecycle release:
state, live pointers and release evidence remain unchanged. It supplies no
claim that an actual custody-release mutant has been demonstrated.

Reachability: the authenticated state route dispatches to stopCodexSwitch;
worker then run locks cover the eligible first transition and changed CTE.
Each added data-modifying CTE selects that changed run ID within the same SQL
statement and transaction, before commit and the post-response snapshot.
The E3 timestamp-only update does not trigger the inventory guard's lifecycle
branch (effective 00409 Up checks state/live pointers/final disposition).
These controls inject no effect on paused replay; positive replay equality
is observed but has no separate replay-effect sensitivity credit.

Receipt .uzi/scratch/stopped-populated-effect-results.json contains exact
source/generated diffs and hashes, commands, durations, leaf actions, and cleanup.
Before gate-log.jSzDhs and after gate-log.m4RCxZ exit 0 with both leaves passing,
no skips. E2 and E3 each exit 1 at the named assertion, with no SQLSTATE, build
failure or timeout. Separate compilation checks exit 0 (gate-log.390s7u and
 gate-log.7vFT78); the actual negatives also compile with race detection.
All logs are under .uzi/scratch/. From api with GOFLAGS=-p=1, GOMAXPROCS=4:

```sh
go test -buildvcs=false -race -count=1 -json ./internal/handler -run '^TestWorkerCodexStoppedPreservationRouteLiveDB$'
```

Individual mutants use the same command with the corresponding exact subtest
suffix. The lead inspected the script, diffs, original source hashes, and recorded
leaf pass/fail events and assertion text. The tester's handoff had no returned
outer-command result; the subsequently inspected receipt and logs record completed
individual subprocess exits, final unchanged passes, and fixture cleanup exit 0.
No overall harness-exit claim is made. Primary status/diff remained clean, export
removed, and exact owned fixture removed. One export at a time, no installs or base
builds. The source fixture range was independently reviewed without findings.

Credit is limited to detecting these particular effects at these first-settlement
seams. It is not proof of every input mutation, actual custody release, switch
journal preservation, replay effects, accounting, gates or questions. Remaining
worker obligations stay unvalidated; m3 and m4 remain **INCOMPLETE**.


### D61 — paused policy hold redundant-fence guard control, class (d)

Candidate 096f614d7cd9cf6bd7adfbd8750eacb2888d0b4c. Receipt:
.uzi/scratch/stopped-hold-results.json. The new real-route fixture seeds an
already-paused row with a resolved policy outcome and pending_fresh=false,
then supplies either codex_policy_hold or ordinary completion_blocked hold.
It reads back the tuple and requires HTTP 409 with full unchanged snapshots.
The shared exact a233 STOP refusal/absence evidence above applies: the new
stopped-route fence has no historical request seam to restore.

This is a **redundant-fence guard control**, not per-copy sufficiency proof.
Two executing copies on this path are policy_hold.go:190's hold arm and
codex_switch_stopped.sql.go:29's hold predicate, mirrored by source SQL :19.
The mutant removes only the Go hold arm and changes the SQL accepted hold set
from NULL to NULL-or-codex_policy_hold. Ordinary holds remain excluded; pending
fresh, unresolved outcome, lane, authority, input and budget checks stay intact.
The receipt carries the exact three-file diff and before/after hashes.

Search scope and trace: WorkerRunState (worker_protocol.go:1541), reconciliation
wrapper, service.go:4675–4676 early STOP dispatch, stopCodexSwitch, policy helper,
beginClaimMutation, source and generated stopped query. Under READ COMMITTED,
worker then owned-run FOR UPDATE locks remain held through helper/query, reread
and commit. The generic policy check at service.go:4762 is bypassed by the early
return, not a third executing copy. The receipt's final source census exits 0.

With GOFLAGS=-p=1 and GOMAXPROCS=4, from api:

```sh
go test -buildvcs=false -race -count=1 -json -timeout=5m ./internal/handler -run '^TestWorkerCodexStoppedHoldRouteLiveDB$'
```

Unchanged before gate-log.KW7aQb exits 0 (455.917s). Mutant gate-log.oxnx8K
exits 1 (191.668s) at :47, `refusal mismatch: status=200 applied=true`, for
policy_hold; ordinary_hold still passes. Unchanged after gate-log.PJ29cW exits
0 (8.849s), both leaves passing. No skips, build failures or SQLSTATE supplied
the red. All logs are under .uzi/scratch/. These wall durations include compile
and cache costs, not measured CI before/after or critical-path impact.

The lead checked individual log events, source hashes and receipt diffs. Fixture
review found no findings. Export removed, primary clean and candidate hashes
unchanged; exact owned fixture cleanup exit 0. No install or base build.
Security-gate credit is not supplied by this control or by the tester's template
slot labels; m4 security remains required worker work. This proves the paused
policy-hold fence only, not running-policy-hold, owner-request/apply policy,
lane/ownership or remaining preservation clauses. m3/m4 remain **INCOMPLETE**.


### D62–D63 — stopped user-owner redundant-fence guard controls, class (d)

Candidate 05169b18baf7af89550ea226543a1177abbf1f30; receipt
.uzi/scratch/stopped-owner-results-05169.json. Shared exact a233 STOP
refusal/absence evidence above applies. These are **redundant-fence guard
controls**, not historical restoration or per-copy sufficiency evidence.

The source query :9–10 implements one semantic owner fence twice:
`w.user_id = r.user_id` in the join and `r.user_id = sqlc.arg(user_id)::uuid`
in eligibility. Generated codex_switch_stopped.sql.go:19–20 executes both;
the source is the declarative counterpart, not two additional executing copies.
Each mutant removes the join's owner comparison and neutralizes only the second
comparison as `$3::uuid IS NOT NULL` (matching sqlc.arg form in source). This
retains argument plumbing and all worker-ID, generation, nonce, epoch, binding,
status, lane, policy, input and budget checks. Exact two-file diffs and hashes
are in the receipt. Running and paused cases used separate serial exports.

Fixture codex_switch_stopped_owner_routes_livedb_test.go verifies the exact worker and run rows before
and after preparation. It keeps token, worker ID, nonce, claim and frozen run
binding unchanged, but assigns the worker to a different existing user. Matched
controls perform identical forge-origin/cooldown bookkeeping removal, retaining
the original owner. This avoids changing runs.user_id, which would break its
Codex-secret composite FK. Both cohorts assert NULL worker Anthropic binding,
then prove no worker field other than user_id changed and all run snapshots stayed unchanged.
This is disposable test preparation, not an application-supported ownership
transfer or a claim about concurrent authentication/ownership changes.

Path census: actual RequireWorker token lookup reads the current worker owner;
WorkerRunState → reconciliation wrapper (no optional payloads) → service early
STOP return → beginClaimMutation (READ COMMITTED, worker then owned-run FOR
UPDATE locks) → stopped lane/policy checks → SetCodexSwitchStopped → worker-ID
reread → commit. The lock query and post-read authorize worker identity, not
user identity. ownedRunOrRefusal service.go:6874–6896 adds cross-check-generation
and isolated-lane checks, neither a user-owner copy. No Go user-owner comparison
was found on this exercised four-field path. The query's two owner predicates
therefore execute under the same locks; the census is bounded to this route.

| Control | Negative case | Individual red log |
| --- | --- | --- |
| D62 | running/mismatched_owner | gate-log.0YkQk7 |
| D63 | paused/mismatched_owner | gate-log.WLnI46 |

Each negative exits 1 at owner fixture :100,
`refusal mismatch: status=200 applied=true`; its matched_positive companion
passes. No skips, SQLSTATE, timeout or build error supplies the red. Primary
before gate-log.sL2joH and after gate-log.zRS6LN exit 0, all ten lane and four
owner leaves passing. With GOFLAGS=-p=1 and GOMAXPROCS=4, from api:

```sh
go test -buildvcs=false -race -count=1 -json -timeout=5m ./internal/handler -run '^TestWorkerCodexStopped(Lane|Owner)RouteLiveDB$'
```

Each control selects the Owner test and one state's negative plus matched leaf.
All logs are under .uzi/scratch/. The lead checked original source hashes,
exact diffs, individual JSON leaf outcomes and assertion text. Exact owned
fixture cleanup exits 0, export removed, primary clean and hashes unchanged.
No installs, base builds or concurrent Go builds. Source unit and UUID repair
were independently reviewed without findings.

The initial candidate 7a1054dec failed six lane seed statements with SQLSTATE
42P08; no mutants were run on that failed baseline. Its log gate-log.CSHyW5,
receipt stopped-owner-results.json and trace remain preserved. Commit 05169b18b
consistently casts the shared target ID parameter to UUID, then text only for
branch construction. Focused judge/reviewtarget gate-log.674SKu exits 0 before
the complete fourteen-leaf run. This is fixture repair, not a product-guard
relaxation. Recorded wall times include compilation/cache; CI critical-path
impact remains unmeasured.

Only stable authenticated worker/run user-owner mismatch refusal is credited
by D62/D63. The lane fixtures now have positive refusal evidence, but their own
mutation controls remain **unvalidated**. No per-copy independent sufficiency,
concurrent owner-change freshness, all ownership surfaces, remaining preservation
or runtime/transaction clauses, or m4 security is claimed. m3/m4 remain incomplete.


### D64–D73 — five stopped lane exclusions, class (d)

Candidate 341c861df72618b359b81e1afc81a91300439cc2. Receipts:
.uzi/scratch/stopped-lane-results-a.json (chat/judge/job) and
.uzi/scratch/stopped-lane-results-b.json (cross_check/reviewtarget).
These are **redundant-fence guard controls** on the real authenticated stopped
route, not historical restorations or per-copy independent sufficiency proofs.
The shared exact a233 stopped-operation refusal/absence evidence above applies.

Each selected kind control removes only that kind from
credential_switch_codex.go:338's case list and the source stopped query :16 /
generated query :26 NOT IN list. Review-target controls remove only the helper's
ReviewTargetRunID.Valid block (:334–336) and source :15 / generated :25
review_target_run_id IS NULL conjunct. Source and generated SQL are counterparts
of one query, not two executing guards. Thus each semantic exclusion has two
executing copies: the Go helper and generated query. Each status/negative gets
its own serial candidate export, removed before the next export.

The bounded census follows RequireWorker → WorkerRunState → reconciliation
wrapper with four-field stopped payload → service.go:4675–4676 early STOP →
beginClaimMutation worker/run FOR UPDATE locks → codexSwitchLaneEligible and
policy helper → generated stopped query → worker-owned reread → commit.
The post-read cross_check generation-zero and isolated-lane backstops remain
intact; these fixtures use positive generations and compatible unbound workers.
Other kind/review exclusions, status, worker/user ownership, claim, nonce,
epoch, binding, policy, input and budget predicates are unchanged in each diff.
The controls do not certify trigger or concurrency behavior beyond this path.

| Control | Negative | Individual assertion-red log |
| --- | --- | --- |
| D64 | running/chat | gate-log.OgS8lD |
| D65 | paused/chat | gate-log.pWdgjR |
| D66 | running/judge | gate-log.gvaA6U |
| D67 | paused/judge | gate-log.I9odkU |
| D68 | running/job | gate-log.wXKqPC |
| D69 | paused/job | gate-log.3PZO67 |
| D70 | running/cross_check | gate-log.NAKUNI |
| D71 | paused/cross_check | gate-log.EF2wRb |
| D72 | running/reviewtarget | gate-log.u5TJNl |
| D73 | paused/reviewtarget | gate-log.sGK7x4 |

All logs are under .uzi/scratch/. Every selected leaf compiles and exits 1 at
lane fixture :79, `refusal mismatch: status=200 applied=true`; no skips,
SQLSTATE, build failure or timeout supplies the red. With GOFLAGS=-p=1 and
GOMAXPROCS=4, each command from the export's api directory is:

```sh
go test -buildvcs=false -race -count=1 -json -timeout=5m ./internal/handler -run '^TestWorkerCodexStoppedLaneRouteLiveDB$/^STATE$/^KIND$'
```

Unchanged green is reused from the previous actual before/after handler runs
at 05169b18b: gate-log.sL2joH and gate-log.zRS6LN, exit 0, all ten lane leaves
passing without skips. The only revision delta to 341c861df is the two ledger
docs. Both harnesses verify 5,946 API/fixture/Go/build inputs are byte-identical
at old candidate, current candidate and primary, and verify the raw green logs,
Go runtime, serial flags and PostgreSQL fixture recipe before reuse. They do
not rerun the unchanged gate just to report it again. The lead independently
checked the current input hashes, raw log hashes/leaf failures, and reconstructed
every exact three-file mutant to verify its recorded post-mutation hash.

Timing is recorded per case in the receipts (7.648–419.871s, including compile
and cache costs), not a CI before/after comparison or critical-path measurement.
Both exact owned database fixtures were removed with cleanup exit 0; exports
removed, primary unchanged, prior evidence preserved. No installs, base builds,
concurrent builds or broad security/full-suite result. Credit is only the five
enumerated stopped exclusions in running/paused states. Owner-request, preflight,
apply, other statuses and remaining runtime/transaction/security clauses are not
discharged by this shared helper's STOP controls. m3/m4 remain **INCOMPLETE**.


### R7 — B dispatcher publication-readiness guard, class (d)

Candidate 0028b62492b0bc29be64360ec865f314c6661f1e. Receipt:
.uzi/scratch/runtime-publication-results.json; independent lead source/log audit:
.uzi/scratch/runtime-publication-lead-audit.json. This is a **guard control**
through the real Runner switch, not helper-only or whole-handoff evidence.

The unique runner.ts:17677 callback normally rejects when
`!replacementAdmissionOpen || flight.codexAuthority !== bundle`.
The mutant neutralizes only the publication-readiness term, retains the
bundle-retirement comparison and callback, and adds `void replacementAdmissionOpen`
solely to preserve compilation after removing its effective read. Scope creation,
controller ownership, capability-derived identity and binding remain unchanged.
The receipt carries exact old/new snippets, diff and source/mutant hashes.

Selected actual Runner leaf:

```text
Codex api_key to subscription publication gates Now, retired A and B preparation children; finished B root dispatch admits work; finished B preparation broker dispatches an actual child
```

Its before-publish barrier obtains the newly installed B dispatcher, verifies it
is not retained A's dispatcher, and invokes ordinary root preparation with the
live lifecycle signal. The mutant fails at :34258 with ERR_ASSERTION,
`Missing expected rejection: new context dispatcher must refuse ordinary root admission before finishB`.
This witnesses early root preparation admission, not proof that model work ran.

From agent, the Node command uses `--import tsx --test --test-reporter=tap
--test-timeout=120000`, an escaped exact-name suffix pattern, and
`test/codex-executor.test.ts`. Before gate-log.nbWG0z exits 0; mutant
`npm run typecheck` gate-log.B5egXj exits 0; selected mutant gate-log.bPtPQR
exits 1 at the intended assertion; after gate-log.9F5E18 exits 0. Each selected
leaf actually executes, not SKIP; unselected Node test skips are normal. Logs
are under .uzi/scratch/. The lead parsed the exact TAP leaf/frame and verified
both source hash and reconstruction of the precise mutant hash.

Baseline evidence is `git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts`,
lines 25735–25738: Codex capture retains source and returns retained_stop (quoted
in A1). There is no allowed switch publication guard at that seam. The raw receipt
incorrectly calls 47b0d8e the refusing baseline in one prose annotation; it is not
used as baseline evidence. That candidate already contains live switching. The
lead audit preserves the raw receipt hash and records the exact a233 source
citation and correction without rewriting the run results.

One removed scratch export, primary fingerprint/status unchanged, provisioned
agent dependencies reused; no installs, base builds or concurrent Go build.
This proves only the new B dispatcher's pre-finish publication-readiness fence.
Capability-derived operation-scope identity, retained-A refresh, other authority
consumers and the remaining mixed clauses are **unvalidated**. No full-suite or
security result is supplied. m3/m4 remain incomplete.


### E4 — open custody owner-discard effect-injection sensitivity control

Candidate 7e82c7be1b8d6be08d2f41229ebbca7bcba8c631. Receipt:
.uzi/scratch/stopped-custody-disposition-results.json. This is one
**effect-injection sensitivity control**, distinct from E3's timestamp-only
corruption. The named seam is preservation of the open custody hold and its
live worker/run references during first accepted STOP.

The source query :40 and generated counterpart :50 add the same single CTE
after changed, with all existing eligibility predicates, six RUN assignments
and response expressions intact:

```sql
), discarded_custody AS (
    UPDATE recovery_custody_holds
    SET state='discarded', release_evidence='owner_discard', live_worker_id=NULL, live_run_id=NULL
    WHERE run_id IN (SELECT id FROM changed) AND state='open'
    RETURNING id
```

These four assignments form one coherent custody disposition effect, not four
independent seams. They match the disposition fields in DiscardCustodyHoldForOwner
(queries/recovery.sql:774–792), omitting its updated_at assignment. The mutant
is intentionally forbidden on STOP; it does not exercise the owner API's intent
or authorization checks and does not claim identical owner-query scoping.
Immutable provenance, release timestamps, FINAL/publication fields and all other
effects remain unchanged by the injected SET list. This is owner discard and
loss of live references, not completed-publication release.

Source-only DBA feasibility review traced the effective 00409 owner-discard
branch, 00318 completion guard, 00386 merged-predecessor guard, 00393 evidence
and closure checks, publication/ancestry ties and nullable live FKs. No guard is
removed or disabled. The subsequent compiling real-route run reaches the custody
comparison, rather than failing SQL preparation or silently retaining the row.

The unchanged open_custody fixture verifies its inventory-guarded open row,
current owner/generation/live pointers and NULL FINAL evidence before STOP. The
real authenticated state route locks worker/run, executes changed then the
custody effect within the same statement/transaction, commits, and reads the
unfiltered whole-row custody snapshot. At :101 the mutant fails with
`snapshot changed in custody`. Existing first-STOP RUN checks have already
passed; no unrelated RUN-field change supplies this failure.

With GOFLAGS=-p=1, GOMAXPROCS=4, from api:

```sh
go test -buildvcs=false -race -count=1 -json -timeout=4m ./internal/handler -run '^TestWorkerCodexStoppedPreservationRouteLiveDB$/^open_custody$'
```

Before gate-log.eOP2LA exits 0 (219.445s); mutant gate-log.KpbaC1 exits 1
(216.761s); after gate-log.bkwEJh exits 0 (9.997s). All logs are under
.uzi/scratch/. The selected leaf runs without skips; no SQLSTATE/build error or
timeout supplies the red. The lead reconstructed both precise file mutations
and verified their hashes, actual log leaf outcomes and assertion. The receipt's
old pending-stage marker is not a verdict; terminal result entries, raw exits
and cleanup prove completion. Its exact owned fixture cleanup exits 0, export
removed immediately, primary status/diff and hashes unchanged. No installation,
base build, concurrent build, full-suite or security-gate claim.

The CTE is gated by changed, so it exercises first settlement only. Positive
replay equality remains observed but has no separate replay-effect control.
Only detection of this open-hold owner-discard effect is credited; other custody
release/discard paths, replay, other preservation and remaining m3/m4 criteria
remain required and unvalidated.


### E5 — first-STOP paused-time credit effect-injection sensitivity control

Candidate d5834ca78dd84c3b06676ad5cf33d9120299dba9. Receipt:
.uzi/scratch/stopped-timebank-effect-results.json. Named seam: first accepted
stopped settlement preserves budget_paused_seconds instead of banking pause
time. This is an **effect-injection sensitivity control** on a preserved field
whose write is absent from the stopped SET projection, not guard removal.

The sole added effect is `budget_paused_seconds = r.budget_paused_seconds + 1`
in the existing changed UPDATE, source SQL :37 and generated counterpart :47.
Both represent the same executing query. There are no fixture, guard, CTE, other
assignment or response changes. The receipt's exact two-file diff and hashes
show the original six status/health assignments remain intact.

The existing real-route open_custody case calls the unchanged full RUN-row
preservation oracle, which permits only those six status/health fields to change.
The added budget write occurs in the first eligible transition under worker/run
locks, before commit and the post-response snapshot. After HTTP 200/applied
acknowledgement and health checks, it fails at preservation test :101 with
`first stop changed unrelated run fields`. The lead reconstructed both mutant
hashes and checked the raw selected leaf failure. Thus the accounting write,
not a fixture or SQL failure, causes the red on this actual route/transaction.

With GOFLAGS=-p=1 and GOMAXPROCS=4, from exported api:

```sh
go test -buildvcs=false -race -count=1 -json -timeout=4m ./internal/handler -run '^TestWorkerCodexStoppedPreservationRouteLiveDB$/^open_custody$'
```

Mutant gate-log.tRi9CC exits 1 (173.694s), selected leaf fails without skips,
SQLSTATE, build failure or timeout. Logs are under .uzi/scratch/. Unchanged
green is reused from the prior E4 validator's actual gate-log.eOP2LA and
gate-log.bkwEJh exit-0 runs of the exact same selected leaf and command. The
only revision delta from 7e82c7be1 is the two ledger docs; API trees match,
other build inputs are unchanged, and the receipt verifies raw log hashes,
commands, actions and runtime/serial flags. The lead independently checked
raw outcomes/hashes and matching API tree objects. These are reused earlier
E4 greens, not newly executed before/after E5 gates.

One export removed, exact recreated owned fixture cleanup exits 0, primary
HEAD/status/diff and hashes preserved, prior evidence retained. No installs,
base builds, concurrent builds, full-suite or security result. Credit is only
sensitivity to this direct first-STOP paused-time write. Replay, other accounting
triggers, concurrency and remaining preservation clauses are not discharged.
The broader m3/m4 completion contract remains required and incomplete.


### E6 — populated applied-switch journal deletion sensitivity control

Candidate 7ff6c1644f50dccc78825e0207f79d22e5808ca1. Receipt:
.uzi/scratch/stopped-switch-journal-results.json. This is one
**effect-injection sensitivity control** for preservation of applied switch
history on first accepted STOP, distinct from E1's ordinal-zero base journal.

The new TestWorkerCodexStoppedSwitchJournalRouteLiveDB applies X through the
real worker apply route at epoch 42, verifies its actual ordinal-1 row and frozen
B snapshot, and then prepares Y targeting the original credential. Y's actual
preflight response supplies epoch 42, claim generation and registration nonce
for the new stopped report. After Y withdrawal, the capability is unchanged,
there is no Y audit row, and the snapshot must contain exactly the unchanged X
row. This is fresh authority negotiated for a new operation, not relatching an
epoch after a rejected report. No fake journal INSERT supplies the fixture.
The pre-existing applied-successor case still rejects stale epoch 41.

One CTE is added after changed, identically to source SQL :40 and generated
constant :50, before the unchanged accepted-result SELECT:

```sql
), erased_switch_epochs AS (
 DELETE FROM run_credential_switch_epochs
 WHERE run_id IN (SELECT id FROM changed)
 RETURNING run_id
```

It is one journal DELETE effect; all eligibility fences, six RUN assignments,
responses and fixtures remain unchanged. It deletes the populated X row for
this fixture's changed run. Source-only DBA review traced 00446 table keys and
constraints, 00447 outgoing secret FK and 00465 Codex snapshot additions. The
bounded migration census found no switch-table DELETE guard, user trigger,
incoming FK, rule or RLS policy, and no RUN trigger referencing the switch table.
Deleting the child does not delete its run/secret parents or fire RUN triggers.
Actual execution then establishes the effect reaches the preservation assertion,
not merely that the SQL text appears valid.

The authenticated state route holds worker/run locks; changed identifies the
first transition, the DELETE executes in the same transaction before commit,
and the unfiltered whole-row switch snapshot is read after acknowledgement.
The mutant reaches new fixture :142, `snapshot changed in switch`, after its
HTTP 200/applied checks. Thus neither an empty set nor a preparation failure
supplies the red. The unchanged test also completes paused replay positively.

From api, with GOFLAGS=-p=1 and GOMAXPROCS=4:

```sh
go test -buildvcs=false -race -count=1 -json -timeout=4m ./internal/handler -run '^TestWorkerCodexStoppedSwitchJournalRouteLiveDB$'
```

Before gate-log.OkTFgQ exits 0 (130.205s), mutant gate-log.8M32R4 exits 1
(183.084s), after gate-log.JhaNNp exits 0 (9.382s). Logs are under
.uzi/scratch/. No skips, SQLSTATE, build failure or timeout supplies the red.
The lead verified raw test outcomes and reconstructed both exact mutant hashes;
its first reconstruction used different whitespace, was corrected to the saved
diff, and did not rerun any gate. Terminal entries and raw exits establish the
completed results despite the receipt's retained pending-stage marker.

Fixture source review found no findings. Export removed, exact owned fixture
cleanup exits 0, primary HEAD/status/diff and hashes unchanged. No installs,
base builds, concurrent builds or full-suite/security result. Credit is only
first-STOP applied-X journal deletion sensitivity. X was applied, not withdrawn:
this does not discharge withdrawn-X/withdrawn-Y delayed-report semantics.
Owner-request API behavior, Runtime teardown, B model work, journal-generation
relation controls and replay-effect sensitivity remain separate obligations.
All remaining m3/m4 criteria stay required and incomplete.


### A3 — capability-partitioned scope namespace, exact baseline restoration

Candidate e7ee697e9b63abdfd754760926b4b7fca7990175. Receipt:
.uzi/scratch/runtime-scope-key-results.json; lead reconstruction/log audit:
.uzi/scratch/runtime-scope-key-lead-audit.json. This is class **(a)** evidence
for the changed operation-scope namespace behavior through the actual Runner,
not a helper-only test or a publication-write/guard-removal control.

Exact baseline evidence:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/codex-operation-scope.ts
```

Baseline lines 39–41 accept client/run/generation and policy arguments without
an authority identity. Line 44 derives the namespace as:

```ts
const key = JSON.stringify([runId, generation]);
```

Candidate helper :51 instead uses `[runId, generation, authorityIdentity]`;
Runner's B construction passes codexOperationAuthorityIdentity of B's capability.
The unique mutant restores the exact old two-element key. `void authorityIdentity`
keeps the retained current signature compiling without changing key behavior.
All ownership guards, refcounts, retirement logic and callers stay intact.
The lead reconstructed both source and mutant hashes and checked baseline :44.

The strengthened existing leaf is:

```text
M2 actualRunner subscription to subscription factory account resumes saved thread
```

After real A enters, it acquires a nonowning reference with the actual client,
run ID, **claim generation** and A capability identity, asserting that it shares
actual A's controller. It never owns, binds or prepares that reference. Retaining
it keeps A's map entry present after Runner closes the owning A scope, whose
ownerClosed signal remains aborted. Candidate B then owns a different controller;
a lookup by B capability identity must find actual B's controller. Both added
references close unconditionally even if the existing execution join fails.

The old key aliases B with retained closed A. B's own() fails before the B flight
setter; this is not a demonstration of unsafe B authentication. The fixture's
existing uncertain-report diagnostic and switch-settled observation wake the
driver before B readiness waits. The mutant therefore fails a direct Runner
outcome assertion, not a missing-publication timeout or swallowed callback error:
:35156, `retaining A scope must not force an uncertain credential switch hold`.
The primary requires resumed_B, no switch error and the actual controller checks.
A failure-only fixture ACK lets this wrong held outcome join cleanup; it does
not change primary success behavior or any production handler.

Actual commands use Node `--import tsx --test --test-reporter=tap
--test-timeout=120000`, escaped exact-name suffix selectors and
`test/codex-executor.test.ts`. Before gate-log.qLXsz4 exits 0 with seven selected
leaves passing; exported `npm run typecheck` gate-log.6HeSzU exits 0; mutant
gate-log.HysbSl exits 1 at ERR_ASSERTION and the driver frame; after
gate-log.BrDwU1 exits 0 with the same seven leaves passing. The integration set
also covers R7 readiness, R5 unproven root, R6 uncertain resume, both factory
API-key/subscription directions and the already-running Sub→Sub baseline.
Selected leaves are not SKIP; unrelated Node skips carry no credit. No timeout,
cancellation, uncaught owner-close error or compiler failure supplies the red.
All logs are under .uzi/scratch/.

Lead-executed primary lint gate-log.C9cRyH and typecheck gate-log.55Xb9J each exit
0 before commit. Those logs lack embedded revision headers; the lead audit ties
them to the observed edit/check/commit sequence and independently reconstructs
the exact three boolean-assertion edits from 1f1a83bf4 to candidate e7ee697e9.
Controller identity comparisons are complete but report bounded boolean failures.
Source review found no findings. Measured command times are in the receipt, not
a CI critical-path comparison; no new sleep or broader timing claim is made.

One removed export, provisioned dependencies reused, primary fingerprint/status
unchanged; no installs, base or concurrent builds. Credit is namespace
availability/isolation for the retained-A, subscription→subscription factory
handoff. The real B lookup is a positive desired-input witness, while the exact
legacy restoration demonstrates the namespace-separation dependency. It does
not prove unsafe old-controller use, every auth-mode scope transition, retired-A
refresh, all capability consumers, summary authority, whole-handoff correctness
or m3/m4 completion. Remaining mixed clauses stay required and unvalidated.


### R8 — retired A credential-reconciliation request guard, class (d)

Candidate 79558399250c5c8b1000b5bde705381016c862ef. Receipt:
.uzi/scratch/runtime-retired-credential-results.json; source/counter audit:
.uzi/scratch/runtime-retired-credential-lead-audit.json. This is one **guard
control** through the actual Runner API-key A → subscription B factory path.
Shared exact a233 Codex-operation refusal at runner.ts:25735–25738 applies;
the new per-epoch retired authority guard has no historical allowed-switch seam.

Only createCodexEpochAuthority's assertCurrent guard is neutralized, replacing
`if (retired) throw new TrustedExecutionRefusal("codex credential authority retired")`
with `void retired` (codex-executor.ts:2090–2092). isCurrent, retire, all other
ownership/capture/phase/lifetime checks and callback creation remain unchanged.
The lead reconstructed the exact one-block mutant hash and verified the raw
assertion, source frame and request-counter values.

The existing actual Runner case retains production A's shared reconcile closure
and invokes it during B's account/login/start with a fresh, noncancelled signal.
Without the guard it reaches the fake credential-release client once using A's
capability and returns reconcile outcome ready. The driver, outside callbacks
that production may catch, fails at :35187 with ERR_ASSERTION:

```text
expected: { result: 'retired', releaseCalls: 0, releasedA: false }
actual:   { result: 'ready',   releaseCalls: 1, releasedA: true }
```

This is counter-sensitive, not merely a changed refusal label. Important fixture
semantics: retainedBoundaryReleasedA is set at test :33163 when an active retained
boundary sends req.capability equal to oldClaim.capability. It does not compare
the response token's identity. Credit is a prohibited A-capability release
request; no real-server token disclosure, returned-A-token identity, B use of A
material or unsafe B authentication is inferred from that flag. Server capability
revocation is a distinct obligation and this fake-backed guard test does not
establish or bypass the real API's checks.

Selected leaf: `M2 actualRunner api_key to subscription factory account resumes saved thread`.
The Node command uses --import tsx, --test, --test-reporter=tap,
--test-timeout=120000, the escaped exact-name suffix pattern and
`test/codex-executor.test.ts`. Mutant npm run typecheck gate-log.u1y7vJ exits 0;
mutant gate-log.wYdX9k exits 1 at the intended driver assertion. No timeout,
cancellation, uncaught owner-close or compilation error supplies the red.

Unchanged target green is reused from the prior scope validator's actual
seven-pass gate-log.qLXsz4 and gate-log.BrDwU1 runs. The revision delta is only
the two ledger docs, 1,824 agent inputs are verified identical, and raw target
passes/log hashes and runtime settings are checked. The lead independently
parsed the exact target green/red TAP lines and actual row diagnostics and
verified the docs-only delta. There is no claim of newly run primary positives
for R8. All logs are under .uzi/scratch/.

One removed export, unchanged primary fingerprint/status and reused provisioned
dependencies; no installs, base/concurrent builds or full-suite/security claim.
This credits the new retirement guard's prevention of that retained-A credential
release request during B login. API-key A has no subscription refresh bridge:
subscription-refresh ownership, other retired consumers, later phases, backend
revocation and the whole-authority/m3/m4 contract remain separate and incomplete.

### B5/B6 — post-switch release epoch and hash preservation

Candidate ce54d754aa0257fa85bc22c2cb639b0cc297457a. These are two separate
class (b) **preservation sensitivity controls**, not pre-feature baseline
evidence. Source fixture:
api/internal/handler/codex_switch_release_fences_routes_livedb_test.go.
Receipts under .uzi/scratch/: release-fence-results.json,
release-fence-finish-results.json and release-fence-lead-audit.json.

Exact baseline command:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/codexauthz.go
```

At baseline :680–682 the guard is:

```go
if presentedEpoch != row.CodexClaimEpoch {
    return store.GetRunCodexAuthContextRow{}, ErrCodexCapabilityEpoch
}
```

At :686–688:

```go
if subtle.ConstantTimeCompare(hashCodexCapability(secret), row.CodexCapHash) != 1 {
    return store.GetRunCodexAuthContextRow{}, ErrCodexCapabilityMismatch
}
```

Their historical role was authorization of an already-bound Codex credential,
not account switching. Both expressions and refusal bodies are preserved at
candidate codexauthz.go:693–695 and :699–701. The parser at :290 separates the
epoch from the bare secret; hashCodexCapability at :274 hashes only the secret,
so the negative cases isolate the two predicates.

The exercised candidate path is authenticated worker apply X, its real store
transaction, then POST /api/worker/runs/{id}/codex/release. WorkerCodexRelease
calls ReleaseCodexCredential, whose authorization passes at codexrefresh.go:1320
and :1335 both reach codexCredentialPreludeWithQueries and these same guards.
GetRunCodexAuthContext projects the stored epoch and hash (source query
codex_binding.sql:131–132; executing generated copy :589–590); it does not
compare the presented capability. The apply CAS and mint writers are left
intact. This is a trace of this release path, not a census of every credential
consumer.

Each subcase creates its own real fixture, applies pending X from A at epoch 41
to B at epoch 42, checks B's persisted hash and journal binding, and successfully
releases the exact synthetic B API key with Cache-Control: no-store.
old_epoch_new_secret sends epoch 41 with B's matching secret; only epoch is bad.
current_epoch_old_secret sends epoch 42 with A's obsolete secret; only hash is
bad. Each refusal must be HTTP 403 without an access_token member or A/B
plaintext, followed by successful B release. Both also reject the wholly old A
capability. That last request has two stale factors and is not used to isolate
either guard. The later direct store-backed service call distinguishes the
sentinels hidden by the HTTP mapping; the mutant fails at the preceding real
route assertion, before that call.

B5 neutralizes only epoch equality by adding false && to its if condition.
B6 independently adds false && only to the hash-equality condition. Parsing,
stored-hash presence, the other equality predicate, ownership, active-claim and
binding checks remain intact. Each export starts from the exact candidate and
is removed before another is created. In both compiling mutants, the selected
route returns 200 instead of 403; the assertion is attributed to test :150
because checkRefused uses t.Helper (assertion source :125). Neither a build
failure nor a helper-only failure supplies the red.

All four credited commands run from api with GOFLAGS=-p=1, GOMAXPROCS=4 and
go test -buildvcs=false -race -count=1 -json -timeout=4m
./internal/handler -run <selection>. Selections and logs:

| Stage | Selection after TestWorkerCodexSwitchReleaseFencesRouteLiveDB | Log under .uzi/scratch/ | Exit |
| --- | --- | --- | --- |
| Unchanged before, both cases | whole exact test | gate-log.x6KOS8 | 0 |
| B5 epoch neutralization | old_epoch_new_secret only | gate-log.l5v1Np | 1, intended assertion |
| B6 hash neutralization | current_epoch_old_secret only | gate-log.3vzPPG | 1, intended assertion |
| Unchanged after, both cases | whole exact test | gate-log.i4oWND | 0 |

Before and after each execute both named subcases without skips. Raw JSON
actions, terminal exit records, source/test hashes and exact one-condition
mutant hashes were independently checked. Cheap pre-commit checks were
file-scoped gofmt and git diff --check HEAD. Immutable source review of
a15d986e0..ce54d754a found no findings.

The tester corrected an assertion matcher from :125 to the observed t.Helper
callsite :150 and retained the existing red without rerunning it. Its
15-minute child timeout later interrupted the final green command before any
exit was recorded (gate-log.dkyM2l); that attempt receives no credit. The lead
removed the inherited fixture, created a fresh owned PostgreSQL fixture, and
completed only that missing final green. The recorded initial green and both
red controls were retained without repetition. All three owned fixture cleanup
commands returned 0; exports are absent and the complete tracked-file
fingerprint matches the initial candidate.

Credit is limited to preservation of epoch and secret-hash authorization on a
post-apply release. No pre-apply A-release positive was run. The proof does not
establish the second authorization pass's timing, concurrent revoke safety,
apply atomicity, random mint quality, other credential operations or real
provider behavior. The broader transaction/rebinding clauses and m3/m4 remain
incomplete; full suites and m4 security gates are not credited here.


### A4 — authenticated live owner-request availability

Candidate 21012454b1f03cf20782864392962d7f185316d3. Class (a): restore the
exact historical refusal path by removing only SetRunCredential's three-line
Codex dispatch at run_credential.go:186–188. The unchanged validator then
rejects Codex. Exact baseline commands:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/run_credential.go
git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_override.go
```

Baseline run_credential.go:185–190 calls validateCredentialOverride before
status dispatch; credential_override.go:202–205 contains the harnessCodex
branch returning ErrCredentialOverrideHarnessUnsupported. That validator
branch is still present; the control changes no validator or refusal mapping.

TestWorkerCodexOwnerLiveRequestRouteLiveDB exercises running,
awaiting_approval, awaiting_input and awaiting_followup independently through
real CLI Bearer authentication and the owner credential route. It first
withdraws seeded X through the actual worker route, then requests B and checks
a fresh operation, generation and target snapshot. Complete run comparisons
permit only the ten pending-switch fields enumerated in the test; both journals,
messages, inputs and custody snapshots stay identical. These are positive
preservation assertions, not separate sensitivity credit for every field.

The original d4b3bb2c0 control failed in setup: setupStoppedGuardsRoute called
SetRunCredential before the HTTP request. Its logs uDDCr1/7lWFqQ/aUWks2 and
owner-request-results.json remain recorded, but that setup-red receives no
class (a) credit. Review identified this mandatory defect. Fix 21012454b adds
an independent pending seed using the two real store queries in a transaction,
with actual B material revision and normal operation-ID trigger; existing
helper callers retain the original service path. Preflight, withdrawal and
the owner request remain actual routes. Scoped fix review found no new findings.

Corrected receipt: .uzi/scratch/owner-request-fixed-results.json; orchestration:
owner-request-fixed-control.py in the same scratch directory. Each command is
serial from api: GOFLAGS=-p=1 GOMAXPROCS=4 go test -buildvcs=false -race
-count=1 -json -timeout=4m ./internal/handler -run <pattern>. Before/after select
both exact TestWorkerCodexOwnerLiveRequestRouteLiveDB and
TestWorkerCodexStoppedSwitchJournalRouteLiveDB; mutant selects only the owner
test. Logs under .uzi/scratch/: gate-log.qKnKYY before exit 0;
gate-log.EsWdnN mutant exit 1; gate-log.29NkvH after exit 0. Both greens execute
all four owner leaves plus the original-helper journal companion without skips.
Every mutant leaf fails at owner test :77 with HTTP 422 rather than 200, not
setup, compilation or SQL failure. The lead parsed the raw actions and four
assertion frames, recomputed log hashes and reconstructed the exact one-block
mutant hash from candidate source. Cheap pre-commit checks: gofmt and git diff
--check HEAD. The single export was removed before final green; owned PostgreSQL
cleanup exited 0, primary fingerprints are unchanged and the invalid prior
receipt is retained unchanged. No installs or base builds occurred.

Credit is only authenticated request availability in these four live states.
Negative statuses, wrong owners/targets, policy fences, transaction safety and
other m3 requirements remain unvalidated unless separately credited above.
No full suite or m4 security gate is claimed; m3/m4 are incomplete.


### D74/D75 — isolated owner-request policy guards

Candidate e37fb6ca547800f99061b167b805fa2767106fb9. These are two separate
class (d) guard controls on the new owner-request seam, not baseline regression
evidence. The shared predicate body predates this request path; its existence
alone does not prove the new caller. Baseline-absence command:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_override.go
```

Lines 202–205 describe the harness refusal and execute:

```go
if harness == harnessCodex {
    return nil, ErrCredentialOverrideHarnessUnsupported
}
```

At that same baseline, run_credential.go:185–190 unconditionally calls the
validator before status dispatch (exact git-show command in A4). Thus neither
negative has a historical allowed Codex owner-request policy seam to restore.

Current owner POST reaches requestCodexCredentialSwitch's single policy call
at run_credential.go:632 under worker-then-run locks. codexSwitchPolicyBlocked
at credential_switch_codex.go:404 delegates to UnresolvedPolicyEpisode and
policy_hold.go:189–195. The source census covered that owner branch, transaction
helpers, both pending-write queries and effective migration triggers through
466. StampHeldCodexCredentialSwitch and SetPendingCodexSwitchTarget
(runtime.sql:13626, :13637) have no policy predicate; operation/hold identity
and pending-target triggers do not substitute a policy check. The separate
preflight/apply caller at credential_switch_codex.go:136 is not exercised by
this POST. DBA source review confirmed both fixture shapes satisfy effective
policy constraints; this does not claim concurrent lock-order or apply proof.

TestWorkerCodexOwnerPolicyRouteLiveDB independently seeds X through the store,
preflights and withdraws it through worker routes, then installs one complete
policy tuple on a running unheld claim. pending_fresh has fresh=true and a
nonnull resolution timestamp, disabling the outcome condition. unresolved_outcome
has fresh=false and a null resolution timestamp. Both assert these stored
facts and the remaining claim identity before making the real CLI Bearer
owner POST. Each requires HTTP 409 with the policy-hold diagnostic and exact
preservation of the six snapshot sections. Clearing fresh and resolving the
episode, while keeping its preserving tuple, is a positive companion: the
request must return 200 with a new pending B operation and the tested A
capability/claim and policy fields unchanged. Preservation here is positive
assertion evidence, not separate effect-injection sensitivity credit.

D74 removes only the PolicyPendingFresh disjunct at policy_hold.go:190.
D75 independently replaces only the unresolved-outcome disjunct at :191 with
false. Both keep the hold condition and the other policy arm intact. Only the
selected negative runs on each exact candidate export. Both compiling mutants
fail at the actual owner-route assertion, test :83: HTTP 200 instead of 409.
No setup, SQL or build failure supplies the red. No combined-arm mutation is
credited, and paused codex_policy_hold admission is not covered.

Receipt .uzi/scratch/owner-policy-results.json and script owner-policy-controls.py
record the single serial invocation; owner-policy-lead-audit.json records the
lead's independent raw-action/log-hash and exact mutant-hash reconstruction.
All commands use GOFLAGS=-p=1 GOMAXPROCS=4 and, from api:
go test -buildvcs=false -race -count=1 -json -timeout=4m ./internal/handler -run
<selection>. Before/after select the whole exact test; each mutant selects only
its named subcase. Logs under .uzi/scratch/: PMrcTX before exit 0, 308UIB D74
exit 1, Q6yexN D75 exit 1, 2Gi9w5 after exit 0 (all use gate-log. prefix).
Both greens pass both leaves without skips. Fixture cleanup 27S9NQ exits 0;
each export was removed immediately and primary fingerprints are unchanged.
Prior receipts remain unchanged. Cheap checks were gofmt and git diff --check
HEAD; immutable source review 949ec8ebc..e37fb6ca5 found no findings.

This credits only the two named owner policy cases. Apply-time policy races,
paused holds, other policy tuples, full suites, security gates and broader
m3/m4 requirements remain separate and incomplete.


### D76/D77 — policy introduced after READY, before worker apply

Candidate 183c2755d7d2ec79a0f91f0067a81f44ae90e2f3. Two independent class (d)
guard controls on the new Codex-specific apply seam. The predicate body existed
before; this is not class (a) restoration or wholesale class (b) credit for a
reused helper. Baseline evidence commands:

```sh
git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts
git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_switch_apply.go
```

At baseline runner.ts:25735–25738 the Codex capture branch retains source,
logs "Codex credential switch refused; source and session retained", and returns
"retained_stop". The owner validator also rejects Codex (exact quote/command
in D74/D75). Baseline apply's fresh step :327 calls chooseApplySecret and :337
calls LockAnthropicSecretForApply. This is absence evidence for the scoped
Codex resolver/target seam now dispatched at credential_switch_apply.go:342;
it is NOT evidence that an arbitrary baseline raw worker apply explicitly
rejected the harness. The pending Codex target tuple is new in migration 466.

The real worker route calls ApplyCredentialSwitch, enters a READ COMMITTED
transaction, locks worker then owned run, and dispatches applyCodexSwitchLocked.
That function calls validateCodexSwitchLocked at :31, whose policy call at
credential_switch_codex.go:136 precedes target/baseline resolution and all
binding/capability/journal writes. It delegates through :404 to policy_hold.go:
189–195. Source census covered this apply path, its transaction helpers and
writer SQL/triggers: ApplyRunCodexBinding (queries/codex_binding.sql:337,
generated constant :107) has no duplicate fresh/outcome policy predicate.
The owner caller at run_credential.go:632 and stopped-settlement SQL do not
execute here. This trace does not prove lock-order concurrency independently.

TestWorkerCodexApplyPolicyAfterPreflightRouteLiveDB gets actual READY at epoch
41, retains pending X targeting B, then installs an isolated complete policy
tuple. The fresh case has fresh=true/resolved timestamp; the outcome case has
fresh=false/unresolved outcome. Both are running/unheld and assert current
worker, generation, nonce, A/hash/41, baseline and pending target identity.
Setup seeds pending X through the store; it does not exercise a new owner POST.

Actual worker apply must return HTTP 200, outcome not_applied, reason
policy_refusal, and exactly those two wire keys. All six snapshot sections
must remain identical, including pending X, timestamps, health, capability,
journals, messages, inputs and custody. Resolution is direct fixture SQL: only
fresh=false/resolved timestamp change, retaining the preserving tuple. The
same X then applies B at epoch 42, ordinal 1, with a rotated hash and matching
journal/worker/generation identity. These preservation and positive companion
assertions are not separate sensitivity or transaction-atomicity proofs.

D76 removes only the pending-fresh disjunct at policy_hold.go:190; D77
independently folds only the unresolved-outcome disjunct at :191 to false.
Each keeps the hold condition, opposite arm, target and authority fences,
SQL and all other effects intact. Each compiling mutant fails at test :100:
policy-fenced apply outcome="applied", want not_applied. This decision assertion
precedes wire-size and snapshot checks, so neither extra auth fields nor a
helper-only failure supplies the red.

Receipt .uzi/scratch/apply-policy-results.json, script apply-policy-controls.py
and independent apply-policy-lead-audit.json record exact source/mutant/log
hashes, raw actions, timings, environment and cleanup. One serial invocation
uses GOFLAGS=-p=1 GOMAXPROCS=4 and, from api, go test -buildvcs=false -race
-count=1 -json -timeout=4m ./internal/handler -run <selection>. Primary checks
select the whole exact test; each mutant selects its named leaf. Logs under
.uzi/scratch/ (gate-log. prefix): HMqCw7 before exit 0; L840PQ D76 exit 1;
jaLtWN D77 exit 1; ePZY7K after exit 0. Both greens pass both leaves without
skips. No SQL, setup, timeout, build failure or race report receives red credit.
The lead reconstructed both mutant hashes and verified raw log hashes/actions,
source hashes and unchanged prior receipts. Cheap pre-commit checks: gofmt and
git diff --check HEAD. Source review 20870a4f7..183c2755d found no findings.
Owned fixture cleanup TrYIGl exits 0; each export is removed and primary
fingerprints match. No installs or base builds occurred.

Credit covers only these two sequential post-preflight conditions on the API-key
fixture. It does not establish an owner-to-worker end-to-end race, arbitrary
policy tuples, paused hold admission, atomic rollback, or other m3/m4 clauses.
Full suites and m4 security gates remain unperformed here.


### D78/D79 — subscription A coordination and unresolved-intent checks

Candidate 09342accbe0cecb5860812a76f1329ff2aeec7e0. These are separate class (d)
guard controls on the new Codex apply seam. Shared baseline absence evidence
in D76/D77 applies to each: exact git-show of a233 runner.ts:25735–25738
explicitly refuses live Codex switching, and credential_switch_apply.go:327/:337
uses the Anthropic chooser/lock. There was no corresponding allowed scoped
Codex apply to restore; no explicit raw-worker harness rejection is claimed.

TestWorkerCodexApplyRefreshFencesRouteLiveDB links subscription A before its
first binding freeze, with vault wiring, actual account identity/revisions,
ordinal-zero baseline and epoch 41. B remains a static API key. Existing
helper callers delegate with subscriptionA=false and retain their original
API-key setup. Real READY, registration nonce and pending X precede injection.

The coordination case commits A in_progress with an operation UUID and future
lease deadline, with zero rotating intents. The intent case leaves A idle,
operation/deadline NULL, and inserts one rotating intent for A's actual account
generation and owner. Both assert frozen A identity, current claim, matched X/B,
clear reauth/recovery, and the intended isolated state before HTTP apply.
These are valid synthetic states, not a claim that current worker admission
creates a lease without an intent. No provider I/O or real refresh admission
is exercised here.

The actual worker route runs apply validation under worker/run locks. It locks
aliases before accounts, takes account FOR UPDATE NOWAIT, reads A and locks
rotating intents with FOR SHARE NOWAIT (codex_binding.sql:1117, :1123). The intent
query uses LIMIT 1: its result length is an existence indicator, not a census
of all active intents. The decisive call at credential_switch_codex.go:276
uses codexSwitchAccountIdle at :349–353. Source census covered this validator,
account/intent reads, and apply writer; ApplyRunCodexBinding does not duplicate
these coordination/intent predicates. Lock-contention fences remain intact;
no conflicting transaction is held across apply.

D78 selectively relaxes only the three-field coordination-idle group:

```text
(idle && !operationValid && !deadlineValid)
  OR (in_progress && operationValid && deadlineValid)
```

The original idle branch remains; committed/quarantined states and partial
operation/deadline coordination tuples remain rejected. The unchanged unresolved==0, !reauthRequired and empty
RecoverySealed checks still follow the group. This neutralizes the coordination
fence for the valid coordinating tuple rather than replacing the whole helper
or globally discarding its three checks. D79 independently changes only
unresolved==0 to true in the original helper, retaining the original coordination,
reauth and recovery conditions. No combined coordination/intent mutant counts.

Unchanged apply must return HTTP 200, not_applied/credential_busy, exactly two
wire keys, and eight identical snapshot sections: the original six plus the
complete A account and all A intents ordered by operation ID. Clearing only
the injected blocker through fixture SQL then permits the same X to bind B at
42, ordinal 1, with current claim authority and a rotated hash. These are
positive preservation/availability assertions, not independent effect-sensitivity
or atomicity proofs. Each compiling mutant fails first at test :164 on actual
worker apply outcome="applied", want not_applied, before wire-size or snapshot
assertions. No setup, SQL, build, timeout or race error supplies red credit.

Receipts and lead audit: .uzi/scratch/apply-refresh-results.json and
apply-refresh-lead-audit.json; script apply-refresh-controls.py. One serial
invocation uses GOFLAGS=-p=1 GOMAXPROCS=4 and, from api, go test -buildvcs=false
-race -count=1 -json -timeout=4m ./internal/handler -run <selection>. Before/after
select both the new test and TestWorkerCodexStoppedSwitchJournalRouteLiveDB;
each mutant selects only its named new leaf. Logs under .uzi/scratch/ with
gate-log. prefix: jc9KJV before exit 0, MtvoZU D78 exit 1, S7TURV D79 exit 1,
z4Dm1C after exit 0. Both greens pass both new leaves and the existing helper
companion without skips. The lead verified raw log hashes/actions, reconstructed
both exact mutant hashes and checked current source/prior-receipt hashes.
Owned fixture cleanup bsCL69 exits 0; each export is removed; primary
fingerprints and prior receipts are unchanged. Cheap checks: gofmt and
git diff --check HEAD. Source review and concrete-SQL DBA review of
418094f63..09342accb found no findings.

API apply itself does not invoke refresh reconciliation. The actual worker has
a separate captured reconcileA boundary before teardown; neither it nor live
admitted-refresh timing is proved here. Multi-account lock ordering, recovery
states, other tuples, full suites, m4 security and the remaining m3/m4 clauses
remain separate and incomplete.

### U1 — owner request admission, target ownership and kind (2026-11-17)

Candidate: e2e84464c0230c06c97298b6c75837719225c1b3 plus the then-uncommitted
`api/internal/handler/codex_switch_owner_admission_routes_livedb_test.go`
(sha256 39b07d94…fcf2), committed with this entry. Checkpoint coverage of
e2e84464c was owner-confirmed (`refs/uzi-checkpoints/agent/issue-4202` =
e2e84464c0230c06c97298b6c75837719225c1b3) before this unit started.

Baseline (exact a233 commands):
`git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/run_credential.go`
reads the run owner-scoped at :166 and calls validateCredentialOverride at :188;
`git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_override.go`
:204 `if harness == harnessCodex {` refuses every Codex request before any status,
target-ownership or target-kind check (422; the lane switch at :198 returns 409
first for chat, judge, self_improve and job runs). The CLI at a233
(`api/cmd/uzi/run_limits.go`:405) resolved labels only as `kindAnthropicToken`;
the web `RunCredentialOverride.tsx`:157 returned null for `run.harness === "codex"`
and `CodexCredentialSwitch.tsx` did not exist. So every fence below except the
owner-scoped run read is new where a233 refused the whole operation.

Test: `TestWorkerCodexOwnerRequestAdmissionRefusalsLiveDB`, through real CLI
Bearer authentication and `POST /api/runs/{id}/credential`. Each of its 13
subtests asserts exact HTTP status and message, the status precondition, every
`stoppedRouteSnapshot` section unchanged, and NULL pending-switch fields.

Command per control (from `api` in a one-at-a-time export `.uzi/scratch/snap-ctl`,
removed after each; driver `.uzi/scratch/ctl.py`, specs `.uzi/scratch/u1-*.json`):
`GOFLAGS=-p=1 GOMAXPROCS=4 go test -race -count=1 ./internal/handler -run '^TestWorkerCodexOwnerRequestAdmissionRefusalsLiveDB$/<pattern>' -v`
against throwaway postgres `cdr-4202-pg`. Logs: `.uzi/scratch/ctl-logs/<name>.mutant.log`.

| Control | Class | Mutation (only change) | Red (assertion) |
|---|---|---|---|
| u1-claimed | (d) guard | run_credential.go `if observed.Status == "claimed"` → `if false && …` | claim_assembling_claimed: 422 Ineligible, want 409 claim-assembling (test :126) |
| u1-status1 | (d) guard | run_credential.go `if !applyLiveStatus(observed.Status)` → `if false && …` | ineligible_queued/paused/limit_wait/recovery_wait each: 409 raced, want 422 Ineligible (test :118) |
| u1-usable | (d) guard | `!codexSwitchTargetUsable(meta.Kind, …)` → `(false && …)` | five unusable_target_* each: 200 accepted, want 409 (test :165) |
| u1-anthropic | redundant-fence | the same condition's `err != nil` and `codexSwitchTargetUsable` terms both neutralized (the only two copies of "target is a usable Codex credential" on this path, run_credential.go:649) | anthropic_target_running: 200, want 409 (test :134) |
| u1-foreignsecret | redundant-fence | owner predicate `user_id = $2` → `$2::uuid IS NOT NULL` in the three target reads on this path: `LockSecretEnablementForShareNowait` (credential_disabled.sql.go), `GetUserSecretMetaByID` (user_secrets.sql.go), `LockCodexAliasForShareNowait` (codex_binding.sql.go) | other_users_secret_running: 200, want 404 (test :178) |
| u1-foreignrun | (b) preservation sensitivity | `s.q.GetRunByIDForUser(…{ID: runID, UserID: userID})` → `s.q.GetRunByID(ctx, runID)` in SetRunCredential | other_users_token_running: 200, want 404 (test :189) |
| u1-disabled | (d) guard | target condition `disabled` term → `(false && disabled)` | disabled_target_running: 200, want 409 (test :176) |
| u1-cli-kind | client-side (d) guard | run_limits.go Codex `eligible` also admits `kindAnthropicToken` | TestRunSetTokenHarnessKindsAndAmbiguity/codex-ignores-anthropic and codex-rejects-anthropic (test :51, :54) |
| u1-web-kind | client-side (d) guard | CodexCredentialSwitch.tsx `choices` filter also admits `anthropic_token` | "excludes Anthropic credentials…": option "Account B — unavailable" not found |

u1-cli-kind runs the real `uzi run set-token` command against the CLI's fake
API client, and u1-web-kind renders the real component in jsdom. Neither passes
through the API route or store; they are client-side controls for CLI and web
kind scoping only. The server-side kind fence is u1-anthropic. u1-disabled ran
on candidate 4bd0c1da9 with the test file that added `disabled_target_running`
(sha256 7bdf26f4…, committed with this entry).

Every mutant exited 1 at the named assertion, not in setup or compilation.
Each mutant stops at the status assertion, so no control proves the
sensitivity of the no-write snapshot comparison, which also excludes
user_secrets, codex_credential_state, provider accounts and workers.
An undisclosed earlier run, u1-status (`.uzi/scratch/ctl-logs/u1-status.*`),
neutralized both `applyLiveStatus` copies (:597 and :626) and was still
refused with 409, because `StampHeldCodexCredentialSwitch` also admits only the
four live statuses. "No switch is written for a non-live run" therefore has at
least three copies and no redundant-fence control; only the 422 guard is
controlled (u1-status1).
Redundant-fence notes: u1-anthropic's `err != nil` term also covers NOWAIT
lock contention, so that control proves sensitivity to the combined target
fence, not each copy independently; the subscription-only account lock is not
on these api-key/anthropic paths, so u1-foreignsecret covers a foreign API-key
target only; a foreign linked subscription target also reaches the owner-scoped
`LockCodexAccountForUpdateNowait` / `GetCodexProviderAccountByID`, which no
control neutralizes.

Unchanged-candidate greens (primary clone, same tree): handler test 13/13
subtests PASS, `ok` EXIT=0 (`.uzi/scratch/ctl-logs/u1-green-handler.log`);
`go test -race -count=1 ./cmd/uzi -run '^TestRunSetToken'` EXIT=0;
`npx vitest run src/components/CodexCredentialSwitch.test.tsx` 39 passed EXIT=0.

Credit: statuses queued, paused, limit_wait, recovery_wait and claimed (the 422
and 409 guards); target usability and disabled target; the combined Codex-kind
fence; foreign API-key target; foreign run; client-side CLI and web kind
scoping. Not credited: pool_wait (non-DB test only), terminal statuses,
pinned-without-secret, the lane check, no live worker, unsupported worker, a
foreign subscription target and the stamp SQL's own status filter.

U1 follow-up: `task lint:api` flagged gosec G101 on the test constant name
`msgSecretGone`; it was renamed `msgTargetGone` (no other change). The
unchanged-candidate green was re-run on the renamed file: 13/13 subtests PASS,
EXIT=0 (`.uzi/scratch/ctl-logs/u1-green-handler2.log`); `task lint:api` 0 issues.
The mutant runs used the pre-rename file; the rename touches no assertion.

U1 review fixes: test comments for the anthropic and foreign-secret subtests
now say "redundant fence (combined)"; `disabled_target_running` was added.
Green on the final file: 14/14 subtests PASS, EXIT=0
(`.uzi/scratch/ctl-logs/u1-green-handler3.log`).

U1 checkpoint record: the coverage question for this unit carried a malformed,
hand-typed SHA (`e8b4ab6fc1d4d8e0e8b4ab6fc0000000`). The owner confirmed the
correct full SHA instead: the published checkpoint ref equals
e8b4ab6fc54fb79d63eb4435966cdc27fb9507ae. Later coverage questions paste the
SHA from `git rev-parse HEAD`. The foreign-secret subtest comment now states
the 404/409 split (comment only; no assertion changed).

### U2 — in-place success keeps flight, generation, attempt and custody (2026-11-17)

Owner decision (2026-11-17, U2 only): the effect-injection sensitivity
control of owner Decision 10 (issue #4202 continuation packet; first applied
as E1 above) is extended to the in-place success path of
`attemptCodexCredentialSwitch`; it is not applied by analogy to any other unit.

Baseline: `git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts`
:25735–25738 refused every Codex switch with `retained_stop` (see A1), so there
was no historical in-place success path. On the candidate the success path
(agent/src/runner.ts `attemptCodexCredentialSwitch`, :17290 onward, returning
`return fresh ? "fresh_B" : "resumed_B";` at :17720) mutates no claim, custody,
attempt or clone state and makes no claim, custody, clone-seed or
claim-release call, so each property below holds by absence of a call. (It
does mutate local state: authority, secrets, operation scope, root dispatch and
emitter, the sequence hold, steering controllers and a new provider-epoch
CODEX_HOME.)

Injection point (traced): every control inserts exactly one statement between
`replacementAdmissionOpen = true;` and that return, i.e. after B `finishB`,
Now `finishB`, sequence-hold release and switch settlement, on the real success
path reached by both tests. Each mutant changes nothing else (diffs:
`.uzi/scratch/ctl-logs/u2-*.mutant.diff`) and passed `npm run typecheck` in the
export before the tests ran.

Command (from `agent` in the one-at-a-time export, removed after each run):
`npm run typecheck && node --import tsx --test --test-timeout=120000 --test-name-pattern='(M2 actualRunner api_key to subscription factory account resumes saved thread$)|(Codex real Runner switches A to B to C without a new claim or attempt)' test/codex-executor.test.ts`.
Candidate e8b4ab6fc54fb79d63eb4435966cdc27fb9507ae. An earlier attempt of all
five failed in setup (export lacked `.uzi/scratch` for the test's mkdtemp;
`u2-*.setupfail.log`) and receives no credit; the driver now creates it.

| Control (effect-injection sensitivity) | Seam | Injected effect (only change) | Baseline test red at | A→B→C test |
|---|---|---|---|---|
| u2-claim | no claimRun call during the switch | `await this.client.claimRun().catch(() => null);` | :35233 allocationCalls `claimRun: 1` vs 0 | red: "no extra claim, clone, custody or settlement in the window" |
| u2-attemptdir | no new attempt directory | `await fs.mkdir(join(pathDirname(flight.worktreePath!), "injected-attempt"));` | :35231 "no new attempt directory…" (`'injected-attempt'` added) | red, but not at :36375: the injected mkdir throws EEXIST on the second (B→C) switch, which holds `initialization_failed`, so the test times out at :36346; no A→B→C credit |
| u2-custody | no custody release | `await this.client.releaseRecoveryCustody(claim.run_id, generation).catch(() => undefined);` | :35233 `releaseRecoveryCustody: 1` vs 0 | red: same window assertion |
| u2-transition | no release/reclaim transition | `await this.client.reportState(claim.run_id, { status: "queued" } as never).catch(() => undefined);` | :35235 "no release/reclaim/hold transition…" | red: "no release/reclaim/hold state transition" |
| u2-worktree | same flight and worktree | ``flight.worktreePath = `${flight.worktreePath}-injected`;`` | :35224 worktreePath strict equality | green: this test checks clone path, not flight.worktreePath |

Every mutant exited 1. Unchanged candidate, primary clone: both tests pass,
2/2, EXIT=0 (`.uzi/scratch/ctl-logs/u2-green.log`, baseline test 3.3 s, A→B→C
1.5 s; re-run with recorded HEAD in `u2-green2.log`). In the A→B→C test only
u2-claim, u2-custody and u2-transition are red at its own assertions.

Credit: each row proves that the named positive assertion detects that one
injected effect at that seam on the real success path; it does not prove the
absence of other effects. Not separately controlled: a new flight object
(only a worktree reassignment was injected), `reserveRecoveryCapture`,
`settleRecoveryHold(Live)` and clone seeding (counted by the same
allocationCalls assertion, no injection run), and the `fresh_B` return (same
statement sequence; the controls ran through the resumed path). The claim
generation assertions (:35222–35223) were not shown sensitive. A plan-revision
`awaiting_approval` restore reachable through `restartAndResume` →
`epochSourceAdmission` → `PlanRevisionRecovery.beforeTurn` (plan-revision-retry.ts) (only with an active
silent revision) is not covered by u2-transition.

Evidence-log note (2026-11-17): a validator's cleanup deleted ten earlier
`.uzi/scratch/gate-log.*` files from this run, including the merge-unit
`check:sqlc-drift`, the seven-test LiveDB re-verification at 79f74862d and the
`task check:api` EXIT=0 at e2e84464c. Those results are reported from the
transcript only; the candidate-level checks are re-run before m3 completion.

U2 checkpoint record: the coverage question again carried a hand-typed SHA
that is not a commit (`e7263f3fc3b6…`). The owner confirmed the correct full
SHA from the `git rev-parse HEAD` output: the published checkpoint ref equals
e7263f3fc46ff8e1d9fc23248afb1ef05484214d. Coverage questions now contain only
the pasted `git rev-parse HEAD` output and the unit name.

Seam check after the default-branch merge: `git diff f1467ba56 HEAD` over the
production files the prior controls mutate or trace (derived by listing every
file path named in this ledger; full paths below) changes `policy_hold.go` only in
`ReportPolicyHold`'s ephemeral teardown (:311 onward; the credited D74/D76
anchors are at :189–195), `api/internal/workersvc/service.go` only in
ephemeral-lease reads (claim, terminal report, teardown),
`agent/src/codex/codex-executor.ts` only the progress-observer argument,
`agent/src/runner.ts` only a comment, `api/internal/handler/worker_protocol.go`
only WorkerHeartbeat's lease argument (:692, off the traced stopped path) and
`api/cmd/server/main.go` (cited only as a reference). No prior mutation anchor is inside
a changed hunk, so prior controls were not re-run; the affected packages'
positive tests are re-run in the m3 candidate-level checks.

### U3 — continuing on A after a failed save needs the exact operation and proven ownership (2026-11-17)

Baseline: `git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts`
:25735–25738 refused every Codex switch (A1), so a233 had no failed-clear or
continue-on-A seam. These are **guard controls** (class (d)), plus one
redundant-fence guard control.

Production guard (agent/src/runner.ts `confirmClear`, :17364–17375): before
the clear, `!before.owned`, `before.pending?.opId !== operation.opId`,
`before.pending.targetSecretId !== operation.targetSecretId`; after an applied
clear, `after.owned && after.registerNonce === nonce && !after.pending`. A false
result makes the persist-failure path (:17411) fall through to `retain()`
(`CredentialSwitchRetainedStop`) instead of `reopen` on A. R4 already credits
`ack.applied !== true`.

New test variants (same commit), in `describe("M2 actualRunner credential
replacement")`: `persist-clear-op-changed-before`,
`persist-clear-target-changed-before`, `persist-clear-owner-lost-before`. The
changed op, target or ownership is observed only after the real persist
injection (`persistFailures > 0`), i.e. on confirmClear's own reconcile; each
asserts `failedClearRequests` is `[]`, no `continued_A`, no reopen, one A turn,
no teardown, apply or B request, and the unconfirmed-clear report assertions.

Command (from `agent` in the one-at-a-time export; candidate
e7263f3fc46ff8e1d9fc23248afb1ef05484214d plus this commit's test file, sha256
1fde6293e298…): `npm run typecheck && node --import tsx --test
--test-timeout=120000 --test-name-pattern='Codex real rollout persist failure
before teardown \(<variant>\)$' test/codex-executor.test.ts`.

| Control | Kind | Mutation (only change in runner.ts) | Selected case | Red at |
|---|---|---|---|---|
| u3-owner-before | guard | `!before.owned` → `(false && !before.owned)` | persist-clear-owner-lost-before | :33975 failedClearRequests gains the clear request |
| u3-op-before | guard | `before.pending?.opId !== operation.opId` → `!before.pending` | persist-clear-op-changed-before | :33975 same |
| u3-target-before | guard | target term → `(false && before.pending?.targetSecretId !== …)` | persist-clear-target-changed-before | :33975 same |
| u3-owner-after | guard | `after.owned &&` → `(true \|\| after.owned) &&` | persist-clear-owner-lost | :33993 switchResult `continued_A` |
| u3-pending-after | guard | `&& !after.pending` → `&& (true \|\| !after.pending)` | persist-clear-pending | :33993 `continued_A` |
| u3-nonce-redundant | redundant-fence guard | nonce term in the confirmClear return and the nonce term in `guard()` (:17311), the two copies on this path | persist-clear-nonce-changed | :33993 `continued_A` |

u3-op-before keeps the pending-presence check so the target line still
type-narrows. A first u3-target-before attempt failed typecheck (TS18048;
`u3-target-before.compilefail.log`) and receives no credit. u3-nonce-after
(only the confirmClear nonce term) stayed green: `guard()`'s nonce check then
throws `StaleClaimError` on reopen, so that single removal is expected
redundancy; credit comes only from u3-nonce-redundant. Every credited mutant
passed typecheck and exited 1 at the named assertion.

Unchanged candidate (primary clone, same test file): the six selected cases
pass, 6/6, EXIT=0 (`.uzi/scratch/ctl-logs/u3-green.log`);
`npm run lint` in agent: 0 warnings, 0 errors. (The other four persist-clear
cases passed in the coder's unlogged run; no credit rests on that.)

Credit: continuing on A after a failed save requires a matching pending op id
and target, ownership (the claim_released leg) before and after the clear, the
same registration nonce (combined fence, including the shared `guard()` term
that reopen runs) and no remaining pending operation, plus R1–R4. Not
credited: a pending operation that disappears before the clear (u3-op-before
keeps the presence check), the hold/status/generation legs of `owned`, the
lost-reply clear and reopen's other guards. The new variants do not assert that
production read the changed value; the mutants show the gate.

U3 checkpoint record: the coverage question carried a non-commit SHA
(`5cd892025a6b…`, sent before the `git rev-parse HEAD` output returned). The
owner confirmed the correct SHA: the published checkpoint ref equals
5cd892025bdf867c7c43a4686381733eefcbdf55 and contains e7263f3fc46f…. The U3
review notes above were folded into this entry with U4.

### U4 — proven A root and tool reap before apply and B; fresh B home (2026-11-18)

Baseline: a233 `agent/src/runner.ts`:25735–25738 refused every Codex switch
(A1). The switch's reap gate is new; the provider-epoch HOME naming is not:
`git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/codex/codex-executor.ts`
:5247 already reads
`const ownedDataRoot = path.join(homeRoot, "codex-data", \`${epochNamespace}-epoch-${epochIndex}\`);`
(provider-epoch recreation). The candidate's identical line (:5693) is reached
by the switch through `restartAndResume` → `startProviderEpoch(…, ++epochIndex, …)` (:4157).

Mechanism: the switch's `teardown` closure (codex-executor.ts :4110–4128)
disposes A's epoch (`tearDownEpoch`: `registry.reapProcesses` over every
registered root, provider and command/tool alike, then terminal disposal) and
sets `reaped` only when the registry is not left in `unverifiedEpochRegistries`
(:4125–4126). The runner holds on `!reaped` (:17525, R5); `publishB` (:4131)
and `restartAndResume` (:4148) also refuse without `reaped`.

New test variant (this commit): `tool-reap-unproven` — "Codex real Runner holds
API-key A to subscription B when an A tool root reap is unproven". The rig's
injected effect root for A's file-operation command (a PassThrough-based fake
from `makeMultiEpochRig`; no fileop binary or sandbox runs) is launched and
registered as kind `command` through the real executor path
(`launchRegisteredEffectRoot`, `registeredRoot`). Its dispose reports unclean,
so the real registry reap of that root fails while A's provider root is reaped
through the same registry. The test does not assert the root's identity, the
registry error text, or the provider reap's own result (it checks that
`A-reaped` was logged). It shares root-reap-unproven's hold assertions (applies 0, teardown
results `[false]`, no B auth, hold reports, event order). Both variants now also
assert `providerAdmissions.length === 1` (no B provider launch).

Command (agent, one-at-a-time export; candidate
5cd892025bdf867c7c43a4686381733eefcbdf55 plus this commit's test file, sha256
abc0af4aba05…): `npm run typecheck && node --import tsx --test
--test-timeout=120000 --test-name-pattern='<pattern>' test/codex-executor.test.ts`.

| Control | Kind | Mutation | Case(s) | Red at |
|---|---|---|---|---|
| u4-exec-reap | (d) guard | `reaped = true \|\| (…unverifiedEpochRegistries…)` (:4125) | root- and tool-reap-unproven | :34170 "unproven A reap cannot apply a switch" (1 vs 0), both |
| u4-after-reap | redundant-fence guard | `!reaped` neutralized in the runner hold (:17525, fences apply and B), `publishB` (:4131) and replacement preparation (:4148) (these two fence B start only, after apply) | root-reap-unproven | :34170 applies 1 vs 0; only the runner copy's effect is observed, the same red as R5 |
| u4-epoch-index | (d) guard | the switch's new `++epochIndex` at :4157 → `epochIndex` | baseline API-key→subscription | :35271 "B provider uses a distinct owned HOME root" |
| u4-fresh-home | (b) preservation sensitivity | `-epoch-${epochIndex}` suffix removed from the owned data root (:5693) | baseline API-key→subscription | :35271 "B provider uses a distinct owned HOME root" |

Copy-census scope: every `reaped`/reap-proof read in agent/src between the
switch teardown and apply or B start (runner.ts attemptCodexCredentialSwitch,
codex-executor.ts suspendEpoch closures; now-summary.ts searched, none there).
The checks in codex-switch-coordinator.ts (:103, :174) are off the path: `coordinateCodexSwitch`
has no caller in agent/src.

Every mutant passed typecheck and exited 1 at the named assertion. Unchanged
candidate, primary clone, same test file: all three cases pass, 3/3, EXIT=0
(`.uzi/scratch/ctl-logs/u4-green.log`); agent `npm run lint` 0/0.
u4-epoch-index ran on candidate f9cc45f9d708 with the comment-corrected test
file (sha256 80be3475…), committed with this note; green re-run on that file:
3/3, EXIT=0 (`u4-green2.log`).

Credit: A's tool (command) root reap failure and root reap failure each block
apply and B through the executor's reap proof; the combined "no B before
proven reap" fence; a distinct per-epoch B HOME on the switch path (the old naming helper as (b),
the switch's new epoch increment as (d)). Not
credited: in-flight tool callbacks or pending launches at teardown
(registry quiesce checks, no switch-time test); the new `providerAdmissions`
assertion (every mutant fails earlier at applies); the production launcher's
`mkdir -m 700` must-not-exist check (the rig injects the provider launch);
"created after reap" beyond the combined fence. Sessions-only and `auth.json`
exclusion remain credited by B1–B3.

U4 checkpoint record: the owner confirmed the published checkpoint ref equals
85914608424d23f765700f50e7b0f591010b6f17 (pasted `git rev-parse HEAD`
output) and contains 5cd892025bdf….

### U5 — refresh bridge, identity, capability and summary authority follow B (2026-11-18)

Baseline: a233 `agent/src/runner.ts`:25735–25738 refused every Codex switch
(A1). The per-epoch authority retirement (`createCodexEpochAuthority`,
codex-executor.ts :2074–2095; `retire()` in teardown :4115; `publishB` builds
B's authority :4138) and the summary factory's authority check are new. The
bridge's expected identity is old: `git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/codex/codex-executor.ts`
:870 already reads `{ authMode: "subscription", chatgptAccountId },`.

New probes in agent/test/codex-executor.test.ts (this commit), all inside
`describe("M2 actualRunner credential replacement")`:
- P1 (retired A refresh bridge): in the gated subscription→API-key case, right
  after B finishes, the rig calls A's captured real `auth.config.bridge.refresh`
  (bypassing appserver-auth delivery, so `assertRefreshAdmission` at :5741 and
  appserver-auth.ts:369 are not exercised); the final block asserts a
  `TrustedExecutionRefusal` "codex credential authority retired" and unchanged
  A/B transport counts.
- P3 (A capability): `aRefreshesAfterFinish` counts `refreshCodex` calls with A's
  capability after B finishes; the final block asserts 0. It only bites in the
  gated subscription→API-key case, where P1's call is the one A refresh
  attempted after B finishes (API-key A has no refresh path).
- P2 (identity): B's ordinary refresh records `expected`; the subscription
  startup-refresh case (API-key A → subscription B) asserts in the final block
  that it equals B's account. A has no ChatGPT account there, so P2 shows B's
  identity is carried, not that A's is rejected.
- P4 (summary authority): the retired-A summary `guard()` probe records the
  error and asserts a `TrustedExecutionRefusal` with "summary credential
  authority changed" or "codex credential authority retired" (current code:
  the former).

Command (agent, one-at-a-time export; first round: commit
85914608424d23f765700f50e7b0f591010b6f17 plus test-file sha256 a9f25155fb8c…,
superseded by the review-fix rerun below): `npm run typecheck && node --import tsx --test
--test-timeout=120000 --test-name-pattern='<pattern>' test/codex-executor.test.ts`.

Review fix (commit a51553c28): P1's transport-count check and P2 first
asserted inside the production-called `finishB` hook, where a failure surfaces
as a failed switch and timeout. Both now record in the hook and assert in the
final block. The controls below were re-run on that test file (sha256
a11e001358ad…, candidate c586c8fcd); first-round logs are superseded.

| Control | Kind | Mutation | Case(s) | Red at |
|---|---|---|---|---|
| u5-identity | (b) preservation sensitivity | bridge `expected` → `chatgptAccountId: chatgptAccountId ? "" : chatgptAccountId` (:882; a233 :870 identical) | Codex subscription startup refresh stays deferred until B finish | :35318 "B refresh expectation names B's account" |
| u5-bridge-pre | (d) guard | the refresh wrapper's pre-call `authority.assertCurrent();` (:5750) → `void authority;` | Codex subscription to api_key publication gates Now… | :35321 "no refreshCodex call carries A's capability after finishB" (1 vs 0) |
| u5-summary-redundant | redundant-fence guard | in the summary factory's `assertAuthority` (:2945–2948) the `currentCodexAuthority !== authority` test and `authority.assertCurrent()`, the two copies of "retired A summary authority refuses" | baseline api_key→subscription and both "publication gates Now" cases | :35323 "retired A summary guard refuses with TrustedExecutionRefusal" (false), all three |

Superseded attempts: u5-identity first failed typecheck
(`u5-identity.compilefail.log`), then timed out (`u5-identity.timeout.log`)
because P2's assertion threw inside the production-called hook and failed the
switch; neither earns credit. Under u5-summary-redundant, the residual denial
of retired A (refusal=false) is most likely the Now call's aborted signal
(`params.signal?.throwIfAborted()` after now-summary.ts:346 aborts it), which is
why the older `retiredADenied` assertion was never authority-sensitive. The
now-summary.ts stale-result drops (:586, :597) are not refusals and are not
controlled. The bridge's
post-call `assertCurrent()` (:5754) and `assertRefreshAdmission` (:5741) have no
separate control (P1 stays green with only the pre-call check removed, because
the post-call copy still refuses; the A refresh then reaches the client, which
P3 catches). Capability use is credited only for the refresh path (P3) plus
earlier A3/R8/B5/B6.

Unchanged candidate, primary clone, final test file: four cases pass, 4/4,
EXIT=0 (`.uzi/scratch/ctl-logs/u5-green2.log`); agent typecheck 0 and
`npm run lint` 0/0.

Credit: retired A's refresh bridge refuses before any A-capability refresh
reaches the client; the executor summary factory's guard refuses retired A
(combined fence); B's refresh carries B's expected account through the switch
path (b). Not credited: rejection of A's account under a subscription→
subscription switch (that pair runs only in the baseline case).

U5 checkpoint record: the owner confirmed the published checkpoint ref equals
c5ad0d0cf59ac44ea54338bd74bf4a7eeb03f5c0 and contains 85914608424d….

### U6 — saved-ID resume, confirmed-refusal-only fresh lineage, preserve re-hold, crash-point IDs (2026-11-18)

Baseline: a233 `agent/src/runner.ts`:25735–25738 refused every Codex switch
(A1), so the switch's resume (`restartAndResume`), fresh-lineage
(`startFreshLineage`) and preserve re-hold paths are new. The retained-recovery
`attempt_path` refusal is old:
`git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts` :15149
reads `if (flight.effectiveAttemptPaths) throw new PolicyContinuationRejected("attempt_path", true);`
(candidate :15427, reached by the switch crash-recovery tests). The retained
`session_id_unavailable` lineage branch (candidate runner.ts :15698) has no a233
counterpart.

Production (codex-executor.ts unless noted): `restartAndResume` sends
`resumeSessionId: sessionId` (:4166) after `sessionId !== saved` refuses
(:4148); a `CodexResumeError` alone is a confirmed refusal (:4175);
`startFreshLineage` requires `resumeRefused`, refuses under
`preserveConversation` (:4186) and emits `resume_lineage_break` (:4196). The
runner (runner.ts :17688–17704) re-holds a preserve-only run with
`PolicyContinuationRejected("provider_resume_rejected")`.

No test change in this unit; existing real Runner cases. Command (agent,
one-at-a-time export; commit c5ad0d0cf59ac44ea54338bd74bf4a7eeb03f5c0, and for
u6-preserve-3copy / u6-crash-preserve-saved the ledger-only successor 22200286b):
`npm run typecheck && node --import tsx --test --test-timeout=120000
--test-name-pattern='<pattern>' test/codex-executor.test.ts`.

| Control | Kind | Mutation | Case(s) | Red at |
|---|---|---|---|---|
| u6-resume-id | (d) data-flow control | `resumeSessionId: sessionId` → `` sessionId && `${sessionId}-wrong` `` (:4166) | confirmed refusal fresh lineage; baseline api_key→subscription | :35357 resumed thread ids `[saved-wrong]` vs `[saved]`, both |
| u6-lineage-notice | (d) guard | fresh lineage's `ctx.emit({… resume_lineage_break …})` → `void (…)` (:4195) | Codex real Runner confirmed B resume refusal starts exactly one fresh lineage | :35376 lineage notices 0 vs 1 |
| u6-preserve-redundant | cause attribution only | `preserveConversation` neutralized in the runner refusal branch (runner.ts :17689) and in `startFreshLineage` (:4186) | Codex real Runner wire preserve-only claim re-holds confirmed B saved-thread resume refusal | :34706 cause `session_missing` vs `provider_resume_rejected`: the harness copy still refused, so this credits only the cause |
| u6-preserve-3copy | redundant-fence guard | the same two copies plus the harness fresh-thread branch (codex-harness.ts :1155, condition made unsatisfiable with `&& Number.NaN > 0`), the three copies on this path of "a preserve-only run never starts a fresh B thread" (codex-switch-coordinator.ts :208 is off the path: only its types are imported) | same case | :34705 `switchError instanceof PolicyContinuationRejected` false (no re-hold) |
| u6-crash-missing-id | (d) guard | `else if (!sessionId && claim.secrets.codex && flight.retainedEpisode)` → `false && …` (runner.ts :15698) | Codex switch crash during B login retains committed B; recovery ordinary missing persisted ID | :35216–35219 lineage-break `reason === "session_id_unavailable"` assertion |
| u6-crash-preserve-attempt | cause attribution only | `attempt_path` refusal → `false && …` (runner.ts :15427; a233 :15149) | …; recovery preserve missing persisted ID | :35180 cause `session_missing` vs `attempt_path` (runner.ts :15444 still refused) |
| u6-crash-preserve-saved | (b) preservation sensitivity | same mutation | Codex switch crash during B login retains committed B; recovery preserve saved conversation | :35174 "preservation refusal launches no provider root" (1 vs 0) |

Not credited (timeouts, `.uzi/scratch/ctl-logs/*.timeout.log`): u6-resume-id's
first form (`resumeSessionId` → undefined) and u6-classify-never (confirmed
refusal never classified) both stalled the switch until the test's own
barrier timed out; a timeout is not an assertion red. The opposite direction
(uncertain error never starts a fresh thread) remains credited by R6. The
`sessionId !== saved` check (:4148) has no control: the runner always passes
the saved id. The `session_missing` guard (runner.ts :15444) is not reached by
the switch crash cases (the retained `attempt_path` refusal fires first); it is
covered only by non-switch runner-policy-preserve tests, although
u6-crash-preserve-attempt shows it is the next fence on the missing-id path.
Crash controls ran through the crash-during-login publication only; the other
three crash publications were not mutated (crash after A teardown recovers A,
not B). u6-resume-id's first (undefined) form survives only as its timeout
log; its spec and diff were overwritten by the second form.

Every credited mutant passed typecheck and exited 1 at the named assertion
(u6-preserve-3copy's first form failed typecheck, TS18048,
`u6-preserve-3copy.compilefail.log`, no credit). Unchanged candidate, primary
clone: the five cases pass, 5/5, EXIT=0 (`.uzi/scratch/ctl-logs/u6-green.log`),
plus the preserve-saved-conversation case, 1/1, EXIT=0 (`u6-green2.log`);
agent `npm run lint` 0/0.

Credit: B's `thread/resume` carries the exact saved thread id (later turns on
that thread are not pinned in the baseline case, and the returned thread id is
not checked against the requested one); a fresh lineage emits its notice; a
preserve-only run's re-hold depends on the three-copy fence (the red shows the
re-hold missing; a fresh B thread start was not observed); after worker loss a missing persisted id is disclosed as a lineage
break; a preserve-only retained recovery with a saved conversation refuses
without launching a provider (preserved behaviour).

U6 checkpoint record: the owner confirmed the published checkpoint ref equals
bb4559fa8a5e8fa71915f69095b882f105ece139 and contains c5ad0d0cf59a….

### U7 — policy episode refusal at owner request, preflight and apply (2026-11-18)

Baseline: a233 refused every Codex owner request at
`git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_override.go`
:204 (see D74), so the switch's policy fences are new.

Predicate (api/internal/workersvc/policy_hold.go :189–192): the hold arm
(`hold_reason = 'codex_policy_hold'`), the pending-fresh arm and the
unresolved-outcome arm. Call sites: owner request (run_credential.go :632) and
`validateCodexSwitchLocked` (credential_switch_codex.go :136), shared by worker
preflight and apply.

Hold arm: unreachable on these live-status paths. The database rejects
`hold_reason = 'codex_policy_hold'` for running and every awaiting status:
`runs_policy_hold_shape` (api/internal/store/migrations/00397_compact_policy_hold.sql)
requires `status IN ('paused','failed','cancelled','completed')` for that hold
(probe: `new row … violates check constraint "runs_policy_hold_shape"`). A paused
policy hold is refused by the owner request's status guard, credited by
u1-status1 (paused); preflight/apply require a live claimed run.

New test (this commit): `TestWorkerCodexPolicyEpisodeBeforePreflightRouteLiveDB`
(api/internal/handler/codex_switch_apply_policy_routes_livedb_test.go), leaves
`pending_fresh` and `unresolved_outcome`. After the shared fixture's first
(rolled-back) READY, it installs the policy tuple, then calls the real worker
preflight route again and the apply route. Each must return `not_applied` /
`policy_refusal` (preflight with exactly five fields and no `expected_epoch`),
with all six snapshot sections unchanged. This is the "episode starts between
the owner request and preflight" interleaving; D76/D77 cover "after READY,
before apply"; D74/D75 the owner request.

Command (api, one-at-a-time export; commit
bb4559fa8a5e8fa71915f69095b882f105ece139 plus this commit's test file, sha256
2e7cf79f830b…): `GOFLAGS=-p=1 GOMAXPROCS=4 go test -race -count=1
./internal/handler -run '^TestWorkerCodexPolicyEpisodeBeforePreflightRouteLiveDB$/<leaf>' -v`
on throwaway postgres `cdr-4202-pg`.

| Control | Kind | Mutation | Leaf | Red at |
|---|---|---|---|---|
| u7-preflight-fresh | (d) guard | `r.PolicyPendingFresh` → `(false && r.PolicyPendingFresh)` (policy_hold.go :190) | pending_fresh | test :59 preflight outcome "ready", want not_applied |
| u7-preflight-outcome | (d) guard | unresolved-outcome arm → `(false && …)` (policy_hold.go :191) | unresolved_outcome | test :59 preflight outcome "ready", want not_applied |

Unchanged candidate: the three policy route tests, six leaves, pass, EXIT=0
(`.uzi/scratch/ctl-logs/u7-green.log`); `task lint:api` 0 issues.

Credit: an unresolved policy episode (pending fresh or unresolved outcome)
refuses the switch at the owner request (D74/D75), at preflight when it
starts after the request (U7) and at apply when it starts after READY
(D76/D77); the hold arm is unreachable in live statuses by schema, and a paused
hold is refused by status. Not credited: true concurrent interleaving under
contention (lock-order) beyond these sequential orderings; that is U8.

### U8 — refresh lease/intent exclusion, NOWAIT lock refusals, reverse-order writer and reconcile before teardown (2026-11-18)

Baseline: a233 refused every live Codex switch
(`git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts`
:25735–25738, `if (this.isCodexCapture(claim, flight)) { … "Codex credential
switch refused; source and session retained" … return "retained_stop"; }`) and
every Codex owner request (credential_override.go :204, see D74), so the
validator's alias/account/intent locks and the executor's `reconcileA` call on
the switch path are new.

Mechanism (api/internal/workersvc/credential_switch_codex.go, candidate):
per alias, old A first, `LockSecretEnablementForShareNowait` (:173) then
`LockCodexAliasForShareNowait` (:181); then `LockCodexAccountForUpdateNowait`
per account (:229–233); then `LockUnresolvedCodexSwitchIntents` (:246,
`FOR SHARE NOWAIT`, LIMIT 1) and `codexSwitchAccountIdle` (:349–353). Any NOWAIT
failure maps to not_applied/credential_busy in `codexSwitchReadFailure`
(:312, the `isLockNotAvailable` test at :313). Worker: the runner calls
`a.reconcileA` (runner.ts :17390) before `persistRollout` (:17408) and
`teardown` (:17516); the executor's `reconcileA` calls `capturedReconcile` =
`shared.reconcile` (agent/src/codex/codex-executor.ts :4088), built by `buildRunLaneReconcile`
(:1055): subscription A one `refreshCodex`, API-key A one fresh `releaseCodex`.

Real writers that take an account before its alias (the order the
reverse-order test emulates): the reconciler's quarantine/reauth restore,
`RefreshCodexAccountLogin` then `LinkCodexCredentialState` in one
`installAndLink` transaction (workersvc/codexcred.go), and account deletion
cascading ON DELETE SET NULL to the alias. `BumpCodexMaterialRevision`
(handler/secrets.go, after locking the user_secrets row) and the healthy link
path take the user_secrets or alias row first, the same order as apply; the
enablement leaves model the former. (The comment at
store/queries/codex_binding.sql :161–166 attributes the reverse order to
BumpCodexMaterialRevision / LinkCodexCredentialState; that SQL comment is
outside this unit and was not changed.)

New tests:
- api/internal/workersvc/credential_switch_codex_apply_lockorder_livedb_test.go
  (1b0d1a69c; 08d5c0e4b constant rename; 6155e078b and 79b4dc9b3 review rework; line numbers below are at 79b4dc9b3).
  `TestCodexApplyAliasRowContentionLiveDB` (:121; leaves old_alias,
  target_alias, old_enablement, target_enablement): a blocker tx holds FOR
  UPDATE on the alias state row or the user_secrets row; apply must be an empty
  busy decision with no writes and an unchanged account/intent/alias snapshot,
  and the same request then applies. Apply's transaction runs through
  `codexStatementTimeoutBeginner` (:77–112), a test-only `SET LOCAL
  statement_timeout = '1s'` issued right after the service's isolation
  statement, and each leaf asserts it was armed (:156). A NOWAIT refusal
  (55P03) returns at once and maps to busy; any wait on the held row (a
  blocking lock in place of NOWAIT, or a later statement such as the
  foreign-key check) is cancelled with 57014, which `codexSwitchReadFailure`
  maps to an error, so it fails the busy assertion. (6155e078b used
  `lock_timeout`; review showed its expiry is also 55P03 and would let a
  NOWAIT-keyword-only mutant read as busy, so 79b4dc9b3 replaced it.)
  `TestCodexApplyReverseOrderReloginWriterLiveDB` (:211): just before apply's
  account NOWAIT for A, a writer tx takes A's account FOR UPDATE and starts the
  real `LinkCodexCredentialState` alias write, observed blocked through
  `pg_blocking_pids` (event-driven, bounded by a 20 s context); apply must refuse
  busy without writes, the writer must finish without 40P01 (:306), and after
  its rollback the same request applies. The account step is an emulated row
  lock (the real reconciler cannot be paused between its steps). On a failure
  path the deferred cleanup cancels and drains the writer goroutine before
  rolling back its connection and logs the writer's result.
  `TestCodexApplyRotatingIntentRowLockLiveDB` (:329): A idle with one committed
  rotating intent on A's account whose row a blocker holds FOR UPDATE: busy
  (leg intent_row_lock, :350); after release still busy (leg intent_count,
  :357).
- agent/test/codex-executor.test.ts (235e433fe): the six `M2 actualRunner …
  account resumes saved thread` baseline cases count A-capability
  `releaseCodex`/`refreshCodex` calls and assert (:33343–33353) the exact
  delta inside `reconcileA` (subscription A {releases 0, refreshes 1}, API-key A
  {1, 0}), no A call between reconcile and teardown entry (:33352), and none
  between teardown entry and apply (:33194, called at :35288). Before this, the
  baseline observed only the runner-level lease call: the (c) mutant below
  survived the api_key→subscription factory case (one case run,
  `.uzi/scratch/ctl-logs/u8-reconcile-after-teardown.survived-pre-assert.log`,
  EXIT=0), which is why the assertion was added.

Commands. Go (one-at-a-time export of the named commit, throwaway postgres
`cdr-4202-u8pg`, `UZI_TEST_DATABASE_URL`): from api,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m
./internal/workersvc -run '<selection>' -v`. Agent (export of 235e433fe with the
primary agent/node_modules symlinked): `npm run typecheck && node --import tsx
--test --test-timeout=120000 --test-name-pattern='M2 actualRunner .* account
resumes saved thread$' test/codex-executor.test.ts`. Driver scripts
`.uzi/scratch/u8tools/ctl.sh` / `actl.sh`; each arm's diff and log is
`.uzi/scratch/ctl-logs/<log name>.{diff,log}` as named in the table. Red
line numbers are at the arm's named commit (79b4dc9b3 added 15 lines above
the reverse-order and intent tests, so their 6155e078b lines :265/:277 and
the intent legs are :280/:292 and :350/:357 at 79b4dc9b3).

| Control | Kind | Mutation | Selection | Log name(s), commit | Red at |
|---|---|---|---|---|---|
| u8-alias-share | (d) guard | `FOR SHARE NOWAIT` clause removed from `lockCodexAliasForShareNowait` (store/codex_binding.sql.go :1167) | old_alias, target_alias | u8s-alias-share, 79b4dc9b3 | lockorder test :69 "apply was APPLIED; expected credential_busy", both |
| u8-alias-nowait | (d) guard | `FOR SHARE NOWAIT` → `FOR SHARE` (same query) | old_alias, target_alias | u8s-alias-wait, 79b4dc9b3 | :72 busy decision missing, `err=codex credential switch preflight unavailable` (the wait was cancelled by statement_timeout, 57014, mapped to the read error), both |
| u8-enable-share | (d) guard | `FOR SHARE NOWAIT` clause removed from `lockSecretEnablementForShareNowait` (store/credential_disabled.sql.go :295) | old_enablement, target_enablement | u8s-enable-share, 79b4dc9b3 | old: :69 applied; target: :72 busy decision missing, `err=… canceling statement due to statement timeout (SQLSTATE 57014)` (inferred from the code, not named in the log: apply waited at `ApplyRunCodexBinding`'s composite foreign-key check (user_id, codex_secret_id) → user_secrets(user_id, id), FOR KEY SHARE on B's row; the error is unmapped, so it came after preflight) |
| u8-enable-nowait | (d) guard | `FOR SHARE NOWAIT` → `FOR SHARE` (same query) | old_enablement, target_enablement | u8s-enable-wait, 79b4dc9b3 | :72 busy decision missing, `err=codex credential switch preflight unavailable`, both |
| u8-account-call | (d) guard | the account-lock loop iterates `accounts[:0]` (credential_switch_codex.go :229) | AccountRowContention old, target; ReverseOrder | u8r-account-call, 6155e078b | races test :82 busy decision missing (both leaves); lockorder :277 "apply was APPLIED while a writer held A's account lock" |
| u8-account-nowait | (d) guard | `FOR UPDATE NOWAIT` → `FOR UPDATE` in `lockCodexAccountForUpdateNowait` (codex_binding.sql.go :1147) | ReverseOrder | u8r-account-nowait, 6155e078b | lockorder :277 applied; the cleanup logs the writer's result `ERROR: deadlock detected (SQLSTATE 40P01)` (:265): Postgres aborted the writer and apply then won A's account. No data race in that log |
| u8-busy-map | (d) guard | `isLockNotAvailable(err)` → `false && …` (:313) | old_alias; AccountRowContention old | u8s-busy-map (old_alias), 79b4dc9b3; u8r-busy-map (both), 6155e078b | lockorder :72 `err=codex credential switch preflight unavailable`; races :82 |
| u8-intent-redundant | redundant-fence guard (decision 9) | (i) `unresolved == 0` → `true` (:351) alone; (ii) intent `FOR SHARE NOWAIT` removed (codex_binding.sql.go :1273) alone; (iii) both | RotatingIntentRowLock | u8r-intent-count-only, u8r-intent-lock-only, u8r-intent-both; 6155e078b | (i) the intent_row_lock leg stays busy (the lock copy refuses); red only at the uncontended intent_count leg, :69; (ii) EXIT=0, the count copy refuses; (iii) red at the intent_row_lock leg, :69 |
| u8-admitted-refresh-redundant | redundant-fence guard (decision 9) | (i) D78 coordination relaxation alone (:350–351); (ii) `unresolved == 0` → `true` alone; (iii) both | TestCodexRefreshAdmittedBeforeApplyLiveDB, TestCodexInitialSubscriptionRefreshAdmissionLiveDB (real `CoordinatedCodexRefresh` admission, apply during the provider call) | u8-adm-coord-only, u8-adm-count-only, u8-adm-both; 1b0d1a69c (files unchanged since) | (i), (ii) EXIT=0; (iii) refresh test :41 "admitted refresh did not block switch [outcome=applied …]", both |
| u8-reconcile-after-teardown | (c) ordering | executor `reconcileA` returns `{ kind: "ready" }` without calling `capturedReconcile` (codex-executor.ts :4088); `teardown` calls `capturedReconcile` right after `await captured.dispose()` (:4119) | six M2 baseline cases | u8-reconcile-after-teardown, 235e433fe | codex-executor.test.ts :33350 "real A credential reconcile (subscription refresh / api_key release) runs inside reconcileA before teardown", 6/6 |

The first-round logs at 1b0d1a69c (u8-alias-share, u8-enable-share,
u8-account-call, u8-account-blocking, u8-busy-map, u8-intent-*) are superseded
by the u8r-*/u8s-* arms above; in that round the target_enablement leaf
reddened only by its 10 s context deadline (no credit) and the
u8-account-blocking failure path had a data race in cleanup, both fixed in
6155e078b. The u8r-alias-share / u8r-enable-share / u8r-busy-map arms at
6155e078b ran under the superseded lock_timeout wrapper and are replaced by the
u8s-* arms at 79b4dc9b3 (the account, intent and reverse-order arms do not use
that wrapper and stand at 6155e078b).

Not credited: u8-late-binding / u8-late-binding-all (the refresh admission's
locked binding recheck, codexrefresh.go :660–664, partly and wholly
neutralized) both survive TestCodexApplyWinsLateRefreshAdmissionLiveDB: the
stale capability is refused first by `authorizeCodexCredentialOpWithQueries`;
that recheck has no isolating control. The u8-account-call fold removes the
whole account loop; the old and target account locks are not isolated
individually. The intent controls use A's (old) account only; the target
account's intent check is not controlled. The (c) mutant fails first at
:33350, so the two "no A credential call" assertions (:33352 and :33194) are
positive assertions without their own control.

Unchanged candidate 79b4dc9b3, primary clone: the three new tests plus
AccountRowContention, AccountLockOrder, BusyOldAccount, WinsLateRefreshAdmission,
RefreshAdmittedBeforeApply, InitialSubscriptionRefreshAdmission and
AcceptsIdleSameAuthorityNewGeneration pass 10/10, EXIT=0
(`.uzi/scratch/ctl-logs/u8s-green-server.log`); `task lint:api` 0 issues at
79b4dc9b3 (`u8s-lint-api.log`). The six M2 baseline cases pass 6/6 in each of
three repeats at 6155e078b, EXIT=0 (`.uzi/scratch/ctl-logs/u8r-green-agent-x3.log`;
agent/ is unchanged since 235e433fe); agent `npm run lint` 0/0.

Credit: a held alias (old or target) or enablement (old or target) row makes
apply refuse busy with no writes, and the NOWAIT keyword on those locks is what
keeps apply from waiting (test-bounded by statement_timeout); a held account
row makes apply refuse busy with no writes (the races test is not
statement-bounded; the no-wait property for the account lock is credited by
the reverse-order 40P01 control); the NOWAIT account lock is what prevents a wait cycle with an
account-then-alias writer (a blocking lock produced a real 40P01); the busy
mapping is what makes contention a retryable public refusal; a rotating intent
on A's account is refused by either the count or the intent row lock; a real
admitted subscription refresh blocks apply by the coordination lease or the
intent; the executor performs the real A credential reconcile (API-key release
or subscription refresh) inside `reconcileA`, before teardown. Not credited:
global lock sorting (two opposite-direction switches of one user's accounts
can both refuse busy; no deadlock), per-account lock isolation, the target
account's intent check, the refresh recheck isolated from the capability fence,
the "no A call after reconcile" assertions, other writers.

### U9 — final release reauthorization against a switch apply committed mid-release (2026-11-18)

Baseline: the recheck is old and unchanged.
`git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/codexrefresh.go`
:1287–1289 reads `if _, aerr := s.AuthorizeCodexCredentialOp(ctx, wkr, runID,
capability, ScopeReleaseAccessToken); aerr != nil { return CodexReleaseResult{}, aerr }`
(comment :1282–1286, issue #1766); candidate codexrefresh.go :1335–1337 is
identical. What is new is the interleaving that makes it decisive for this
feature: a live Codex switch apply (a233 refused every live switch, runner.ts
:25735–25738, see U8) that re-mints the capability and advances the claim
epoch while a release with A's capability is between its first authorization
(:1320) and this final reauthorization. Existing tests pin the recheck only for
non-switch staleness (codexrefresh_livedb_test.go `releaseHookStore`, :141–156),
all staling authority by direct SQL updates.

New test (628535ef9):
`TestCodexReleaseRecheckRefusesAfterSwitchApplyLiveDB`
(api/internal/workersvc/credential_switch_codex_release_recheck_livedb_test.go).
A run bound to linked subscription A with a pending switch to a static API key
(`codexApplyFromSubscription`). A release with A's capability first succeeds
(:20). Then `releaseHookStore.onAccountRead`, which fires right after the
subscription account read returns inside `releaseCodexCredential` (before the
login is opened), runs the real
`f.apply` once and requires `applied` (:32). The release must return no token
(:41), an error (:44) that is `ErrCodexCapabilityEpoch` (:48–49); afterwards A's
capability is refused at the first authorization with the same sentinel, and
the run's binding has changed from A with the request cleared. The test
calls the service (`ReleaseCodexCredential`, `ApplyCredentialSwitch`) with the
hook store, not the HTTP release route: the mid-call injection needs the store
seam. (B5/B6 cover the route's epoch and hash guards.)

Command (export of 628535ef9, throwaway postgres `cdr-4202-u9pg`): from api,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m
./internal/workersvc -run '^TestCodexReleaseRecheckRefusesAfterSwitchApplyLiveDB$' -v`.

| Control | Kind | Mutation | Log, commit | Red at |
|---|---|---|---|---|
| u9-recheck | (b) **preservation sensitivity control** (the guard is the unchanged a233 recheck; the switch interleaving that triggers it is new) | final reauthorization `aerr != nil` → `false && aerr != nil` (codexrefresh.go :1335) | `.uzi/scratch/ctl-logs/u9-recheck.{diff,log}`, 628535ef9 | test :41 "release returned A's token after a switch apply committed before the final reauthorization" |
| u9-recheck-relocate | (c) ordering | the same reauthorization moved from after the material read (:1335) to just before `releaseCodexCredential` (:1325) | `.uzi/scratch/ctl-logs/u9-recheck-relocate.{diff,log}`, fc465e5aa (code identical to 628535ef9; fc465e5aa changed only the ledger). The `.diff` holds the applied final-state relocation; the driver's intermediate removal-only diff is kept separately as `u9-recheck-relocate.diff.with-intermediate` | test :41, as above |

Unchanged candidate 628535ef9: the test passes, EXIT=0
(`.uzi/scratch/ctl-logs/u9-green.log`); `task lint:api` 0 issues
(`u9-lint-api.log`).

Credit: when a switch apply commits between a release's first authorization
(:1320) and its final reauthorization (:1335), that reauthorization — present
and placed after the material read — is what keeps A's token from being
returned; the release fails with the stale-epoch authorization error. An apply
that commits after the final reauthorization passed, before the response is
written, still returns A's token: nothing is checked between the passing
reauthorization and `return result, nil` (:1341), and the token is one A's
epoch was still authorized to receive when the reauthorization passed. The
test exercises one point in the window (after the account read, before the
login is opened); other points are refused the same way by reading the code,
not by test. Not credited: the API-key A
release path (its material read is not hooked here), the vault-locked branch of
the same recheck after a switch (pinned without a switch by
`TestReleaseCodexCredentialVaultLockedAuthorityLostLiveDB`), and releases that complete before the apply
commits (they legitimately return A's token, under A's still-current epoch).

### U10 — transactional binding rewrite, capability remint and material only after commit (2026-11-18)

Baseline: `git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_switch_codex_apply.go`
fails: the file does not exist at a233, and a233 refused every live Codex switch
(runner.ts :25735–25738; credential_override.go :204, see D74). Every fence in
`applyCodexSwitchLocked` (api/internal/workersvc/credential_switch_codex_apply.go
:29–125) is new, so these are class (c)/(d) controls.

Mechanism (candidate): after `validateCodexSwitchLocked` returns ready, one
transaction under the caller's worker/run locks writes `ApplyRunCodexBinding`
(:40, zero-row fence :53), `SetRunCodexClaimCapability` with a freshly minted
hash (:57–58), `InsertCodexRunCredentialSwitchEpoch` (:76),
`ClearCredentialSwitchStampByOp` (:83, fence :87), `InsertRunMessage` (:97,
fence `!ins.Inserted` :104) and `UpdateRunLastSeq` (:107, fence :113), then
`tx.Commit` (:116, error check). B's material is returned only after the commit
(:123). Any error before the commit leaves the caller's deferred rollback to
undo every write.

Tests:
- Existing: `TestCodexTransactionalApplyBundleLiveDB`
  (credential_switch_codex_apply_livedb_test.go :66; old capability refused
  :79, replacement capability authorizes the target :83, coordinates, bundle,
  no plaintext in logs/journal/run, replay `refused_committed`, second switch at
  ordinal 2 / epoch 43) and `TestCodexApplyWriteRollbackLiveDB`
  (credential_switch_codex_apply_rollback_livedb_test.go :12; RAISE triggers at
  binding, mint, journal, stamp, message, last_seq and a deferred commit-time
  constraint; :46 no auth on failure, :48–49 snapshot unchanged).
- New (d43fd2ca6; mint and journal leaves added in ca154ebae review rework;
  line numbers at ca154ebae): `TestCodexApplyZeroRowWriteRollbackLiveDB`
  (credential_switch_codex_apply_zero_row_livedb_test.go), leaves binding,
  mint, journal, stamp, message, last_seq: a per-run BEFORE trigger
  `RETURN NULL`s the one intended statement so it affects zero rows without a
  server error. Apply must return an error and no material (:47),
  "codex switch message sequence occupied" for message (:51–52),
  `pgx.ErrNoRows` for mint and journal (:55–56; the two RETURNING writes
  surface the skip client-side, which does not abort the Postgres transaction),
  `ErrStaleClaim` otherwise (:59–60), and leave the snapshot unchanged (:64).
  The d43fd2ca6 control rows below cite that commit's lines (:39 = today's
  :47). The three `ErrStaleClaim` leaves rely on each trigger
  condition matching only its statement (checked against the SQL: the binding
  writer leaves the stamp, last_seq and epoch columns alone).

Command (export of d43fd2ca6, or of ca154ebae for the rows marked so; throwaway postgres `cdr-4202-u10pg`): from api,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m
./internal/workersvc -run '<selection>' -v`. Diffs and logs:
`.uzi/scratch/ctl-logs/<log name>.{diff,log}`.

| Control | Kind | Mutation | Selection | Log name | Red at |
|---|---|---|---|---|---|
| u10-binding-n | (d) guard | binding `n != 1` → `false && n != 1` (:53) | zero-row binding | u10-binding-n | zero-row test :39 "binding: zero-row write returned auth … outcome=applied" |
| u10-stamp-n | (d) guard | stamp `n != 1` → `false && …` (:87) | zero-row stamp | u10-stamp-n | :39 "stamp: …" |
| u10-message-ins | (d) guard | `!ins.Inserted` → `false && …` (:104) | zero-row message | u10-message-ins | :39 "message: …" |
| u10-lastseq-n | (d) guard | last_seq `n != 1` → `false && …` (:113) | zero-row last_seq | u10-lastseq-n | :39 "last_seq: …" |
| u10-commit-check | (d) guard | `tx.Commit` error check → `false && err != nil` (:116) | rollback commit | u10-commit-check | rollback test :46 "write failure returned auth … outcome=applied" |
| u10-return-before-commit | (c) ordering | the applied result with B's material is returned before `tx.Commit` (:116); the commit becomes unreachable | bundle (both auth modes); rollback commit | u10-return-before-commit (bundle), u10-return-before-commit-rollback (commit leaf) | bundle :79 "old capability still authorized" (both leaves: the returned material belongs to a switch the caller's rollback undid); rollback :46 "write failure returned auth" |
| u10-mint-hash | (d) behavioural write mutation (not a guard removal) | the mint writes the run's old hash instead of the freshly minted one (`Hash: func() []byte { _ = hash; return run.CodexCapHash }()`, :58) | bundle | u10-mint-hash | bundle :83 "replacement capability cannot authorize credential release codex capability does not match", both leaves |
| u10-journal-err | (d) guard, ca154ebae | journal insert `err != nil` → `false && err != nil` (:80) | zero-row journal | u10-journal-err | zero-row test :47 "journal: zero-row write returned auth … outcome=applied ordinal=0" |
| u10-mint-redundant | redundant-fence guard (decision 9); arms (i)–(iii) at ca154ebae, (p) and (v) at 56ea71667 (api tree identical: ca154ebae..56ea71667 changed only the ledger) | mint zero-row has three copies: the mint `err != nil` (:60), the `capabilityEpoch <= 0` check (:65) and the journal insert's `r.codex_claim_epoch = @capability_epoch` predicate (api/internal/store/queries/runtime.sql :2466; generated api/internal/store/runtime.sql.go :11130). Arms: (i) :60 alone; (ii) :65 alone; (p) the predicate alone, rewritten to `$8::bigint = $8::bigint` so the bind count is unchanged; (iii) :60+:65; (v) :60+:65+predicate, with the journal error check :80 left intact | zero-row mint | u10-mint-err-only, u10-mint-epochcheck-only, u10-mint-pred-only, u10-mint-both, u10-mint-iso3 | (i) the zero-row refusal held; red only at the error-identity assertion :56 ("incomplete Codex switch material, want pgx.ErrNoRows"), which shows by mutation that :60 refuses first in the baseline and that :65 refused next; (ii) and (p) EXIT=0; (iii) EXIT=0, refused at :80 with ErrNoRows (by code, the only remaining zero-row source is the journal predicate; arm (v) confirms it); (v) :47 "mint: zero-row write returned auth … outcome=applied ordinal=1". Census scope: `applyCodexSwitchLocked` (:29–125) and the SQL of the statements it runs. Lock trace: `ApplyCredentialSwitch` takes worker then run `FOR UPDATE` in `beginClaimMutation` (milestone_reconciliation.go :295, :298; called at credential_switch_apply.go :288) and holds them for the whole transaction, so within it the run's epoch changes only through the mint; the journal predicate is therefore a copy of the mint fence, not a separate fence. Superseded, no credit: u10-mint-triple (it also disabled :80, a second fence) and u10-mint-iso3.argmismatch (removing `$8` broke the bind count) |

Not credited: the first u10-mint-hash form (`Hash: run.CodexCapHash`) failed
to compile (`declared and not used: hash`,
`u10-mint-hash.compilefail.log`); the first u10-return-before-commit selection
named the rollback commit leaf in a top-level alternation that `-run` does not
match, so only the bundle half ran there (the commit leaf was re-run as
u10-return-before-commit-rollback). The empty-AccessToken half of the :65
check is not controlled (validate refuses empty material first). The
per-statement `err != nil` returns for server-raised errors (:50, :60, :72, :80,
:84, :101, :110) and the JSON errors (:68, :94) have no individual control:
`TestCodexApplyWriteRollbackLiveDB` exercises the RAISE paths, and a
server-raised error aborts the transaction so the next statement or the commit
also fails (argued from Postgres semantics, not tested per guard); :60 and :80
are controlled for their zero-row ErrNoRows case above. No control removes the
caller's deferred rollback (credential_switch_apply.go :295); the snapshot
assertions (zero-row :64, rollback :48–49) are positive assertions on it. Revocation of A's capability is the epoch bump plus the new
hash; the epoch and hash guards that refuse A are credited by B5/B6, and
u10-mint-hash credits that the new hash is what is persisted.

Unchanged candidate ca154ebae: the bundle, target-moved-after-preflight,
write-rollback and zero-row (six leaves) tests pass, EXIT=0
(`.uzi/scratch/ctl-logs/u10r-green.log`); `task lint:api` 0 issues
(`u10r-lint-api.log`). (d43fd2ca6's u10-green.log / u10-lint-api.log are the
first-round records.)

Credit: the binding rewrite, capability remint, journal, stamp clear and
message commit together or not at all, including when any of the six writes
affects zero rows without a server error (for the mint, the zero-row refusal
held in arms (i), (ii), (p) and (iii) and failed in arm (v), with :60, :65 and
the journal epoch predicate all disabled; :60 refusing first is shown by arm
(i); the pairs :60+predicate and :65+predicate were not run); B's material is returned only after a successful commit (a
commit failure or a return placed before the commit is detected); the persisted
capability hash is the newly minted one. Not credited: storage or power-loss
durability (process-loss model only), concurrent readers' isolation beyond
Postgres READ COMMITTED atomic commit, and the uncontrolled checks listed above.

### U11 — ordinal-zero baseline, ordinal N+1 per generation, epoch independence, B usage attribution (2026-11-18)

Baselines: `git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_switch_codex.go`
fails (file absent; a233 refused every live Codex switch, runner.ts :25735–25738),
and a233 `api/internal/store/queries/runtime.sql` has no
`InsertCodexRunCredentialSwitchEpoch` (only the Anthropic
`InsertRunCredentialSwitchEpoch` at :2400). So the Codex baseline check and the
Codex journal's ordinal/epoch columns are new: class (d). The attribution clamp
is old: a233 `api/internal/workersvc/usage_fold.go` :1004–1037
(`clampCredentialEpochOrdinals`, `if sent <= *ceiling {` at :1027) is identical
at the candidate (:1004–1037, :1027), so its control is a (b) preservation
sensitivity control on the new Codex spend path.

Mechanism (candidate):
- Ordinal-zero baseline: preflight and apply read the generation's
  `run_credential_epochs` row (`GetCodexClaimBaseline`), refuse
  `baseline_missing` when absent (credential_switch_codex.go :144–147) and
  `baseline_mismatch` unless `codexSwitchBaselineMatches` (:158; :357–390)
  finds the ordinal-zero baseline, overlaid with the latest child of the same
  generation, equal to the run's binding and epoch.
- Ordinal N+1: the journal insert computes
  `COALESCE(max(e.ordinal), 0) + 1` over the run's rows of the same claim
  generation (runtime.sql :2456–2457, generated runtime.sql.go :11120–11121); the
  table's primary key (run_id, claim_generation, ordinal) rejects a duplicate
  ordinal. The caller's worker/run `FOR UPDATE` (beginClaimMutation)
  serializes the computation. All SQL mutants below edit the generated
  `runtime.sql.go` constant, which is what executes.
- Epoch independence: the journal records `r.codex_claim_epoch` (the post-mint
  capability epoch), not the ordinal (runtime.sql :2462).
- B usage attribution: frames carry the worker's `credential_epoch_ordinal`;
  the append path keeps it only if that ordinal is already committed for the
  current generation (usage_fold.go :1027), otherwise stores -1.

Tests: existing `TestCodexSwitchPreflightLiveDB`
(credential_switch_codex_livedb_test.go :15; leaves missing_baseline,
binding_mismatch, epoch_mismatch; assertion :135),
`TestCodexTransactionalApplyBundleLiveDB` (credential_switch_codex_apply_livedb_test.go
:66; ordinal 1 / epoch 42, journal epoch :117–120, second switch ordinal 2 /
epoch 43 at :140–143) and `TestCodexSwitchSpendJoinsAuthorityEpochLiveDB`
(credential_switch_codex_spend_livedb_test.go :8; A spend at ordinal 0 / epoch
41, B spend at ordinal 1 / epoch 42, `assertJoins` called at :63 and after a
refold). New (a2bf58dae): `TestCodexSwitchOrdinalRestartsPerGenerationLiveDB`
(credential_switch_codex_generation_ordinal_livedb_test.go): a first switch
in generation G at ordinal 1, recovery by a fresh claim into G+1 (the fixture
sets status running before the second owner request, as the first generation
does), a second switch in G+1 must be ordinal 1 (:69), its journal row ordinal 1
with the run's post-mint epoch (:79), and the G row still ordinal 1 (:83).

Command (export of a2bf58dae, or of 36e5f488e for u11-ordinal-skip (api tree identical), throwaway postgres `cdr-4202-u11pg`): from api,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m
./internal/workersvc -run '<selection>' -v`; diffs and logs
`.uzi/scratch/ctl-logs/<log name>.{diff,log}`.

| Control | Kind | Mutation | Selection | Red at |
|---|---|---|---|---|
| u11-ordinal-genscope | (d) guard | drop `AND e.claim_generation = r.claim_generation` from the ordinal subquery (generated runtime.sql.go) | generation-ordinal test | :69 "second-generation switch ordinal = 2, want 1 (ordinals restart per claim generation)" |
| u11-ordinal-n1 | (d) behavioural mutation | `COALESCE(max(e.ordinal), 0) + 1` → `COALESCE(min(e.ordinal) * 0, 0) + 1` (always 1) | bundle | :140, the second switch's apply fails with `duplicate key … run_credential_switch_epochs_pkey` (the primary key is the backstop that caught the repeated ordinal), both leaves |
| u11-ordinal-skip | (d) behavioural mutation (non-colliding), 36e5f488e | `COALESCE(max(e.ordinal), 0) + 1` → `COALESCE(max(e.ordinal), 0) * 2 + 1` (first 1, second 3) | bundle | :142 "second switch: [outcome=applied … ordinal=3 …]", both leaves |
| u11-epoch-indep | (d) behavioural mutation | the journal's `codex_claim_epoch` column takes the ordinal expression instead of `r.codex_claim_epoch` | bundle; spend | bundle :120 "journal used ordinal as capability epoch" (both leaves); spend :63 "B spend did not join the exact minted B epoch" |
| u11-baseline-missing | (d) guard | `errors.Is(err, pgx.ErrNoRows)` → `false && …` (:145) | preflight missing_baseline | :135 "… codex credential switch preflight unavailable; want not_applied/baseline_missing" |
| u11-baseline-match | (d) guard | `!codexSwitchBaselineMatches(…)` → `false && …` (:158) | preflight binding_mismatch, epoch_mismatch | :135 "preflight = {… Outcome:ready …}; want not_applied/baseline_mismatch", both |
| u11-attrib-clamp | (b) **preservation sensitivity control** | `if sent <= *ceiling {` → `if false && …` (usage_fold.go :1027) | spend | :63 "spend rows did not join both authority snapshots" (B's frame stored unattributed) |

Unchanged candidate a2bf58dae: the four tests pass, EXIT=0
(`.uzi/scratch/ctl-logs/u11-green.log`); `task lint:api` 0 issues
(`u11-lint-api.log`).

Credit: a missing ordinal-zero baseline is refused with the typed reason
`baseline_missing` (with that guard removed it still fails closed, as a read
error), and a baseline (with the latest same-generation child) that does not
match the run is refused `baseline_mismatch`, both observed through preflight
(apply shares `validateCodexSwitchLocked`, credential_switch_codex_apply.go
:31, but no apply-route red was recorded); ordinals are exactly N+1 within a
claim generation (u11-ordinal-skip, at the second-switch assertion; a constant
ordinal is caught by the primary key) and restart in a new one; the journal
records the minted capability epoch, distinct from the ordinal; B's usage joins
the B journal row only once that ordinal is committed (preserved clamp). Not
credited: the clamp's upper bound for an uncommitted ordinal on the Codex path
and the generation scope of `MaxCommittedCredentialEpochOrdinal` (only the
committed case is exercised here; the general clamp is #4201's),
`codexSwitchBaselineMatches`'s individual field comparisons and latest-child
overlay, the separate `CodexClaimEpoch <= 0` → `baseline_mismatch` refusal
(credential_switch_codex.go :299–300), spend rows at (G,1) and (G+1,1)
joining distinct epochs, and the u11-ordinal-n1 red as an ordinal assertion (it
surfaced through the primary key). The generation test's first-generation-row
and post-mint-epoch assertions are positive only (no control changes them; the
epoch is read from the same column the insert reads). The (b) clamp control
rests on a233 code identity; a233 has no Codex path to execute.

### U12 — rollback, lost-reply replay, stale or foreign apply and target fences (2026-11-18)

Baselines. The shared apply path's Step 1 replay check is old:
`git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/credential_switch_apply.go`
:302 reads `if prior.ClaimGeneration != gen || !prior.WorkerID.Valid ||
uuid.UUID(prior.WorkerID.Bytes) != wkr.ID { return fenced, nil }`, identical at
the candidate (credential_switch_apply.go :322), so its control is a (b)
preservation sensitivity control on the new Codex replay. The Codex replay
branch (`if run.Harness == harnessCodex { return … RefusedCommitted, Reason:
applyReasonCredentialChanged }`, :325–327) and the Codex validator
(credential_switch_codex.go, absent at a233; a233 refused every live Codex
switch, runner.ts :25735–25738) are new: class (d).

Mechanism (candidate). `ApplyCredentialSwitch` takes worker then run locks in
`beginClaimMutation` (credential_switch_apply.go :288; a run the worker does
not own fails there with `ErrRunNotOwned`), applies the Step 0 fence (:298–316),
then Step 1 looks up a journal row for the op (:318–331): a row from another
generation or worker is fenced; a Codex row answers `refused_committed` /
`credential_changed` without material (:325–327). Otherwise the Codex
validator `validateCodexSwitchLocked` repeats the fence (`codexSwitchFence`,
credential_switch_codex.go :322–329), refuses a different pending op
(:124–129, `superseded` / `no_pending_switch`), a target mismatch (:139–143)
and a moved target (material revision :200–203; account revision :261–263).
Rollback of partial writes is U10's (`TestCodexApplyWriteRollbackLiveDB`,
`TestCodexApplyZeroRowWriteRollbackLiveDB`, both green here).

New tests (2c5e02296; 02f2933a0 review rework):
api/internal/workersvc/credential_switch_codex_apply_foreign_livedb_test.go.
`TestCodexApplyForeignWorkerFencedLiveDB`: a second worker of the same user
applies the pending op: `ErrRunNotOwned`, no material (:27), its preflight is
fenced (:32), no writes; the owner then applies (:36).
`TestCodexReplayCommittedOpUnderNewGenerationFencedLiveDB`: after a committed
switch and a fresh recovery claim (the fixture moves the run to running, as the
first generation does, so the live-status fence passes), the original op
replayed with the new generation must be fenced (:93) with no writes (:96).
The first form left the run `claimed`, so the status fence decided and the
u12-replay-gen mutant survived (`u12-replay-gen.status-fenced.log`, EXIT=0);
02f2933a0 fixed the fixture. 02f2933a0 also pins the Codex replay reason in
`TestCodexApplyCommittedLostReplyLiveDB`
(credential_switch_codex_apply_lostreply_livedb_test.go :55–56); before that,
u12-replay-codex survived because the shared Anthropic replay also refused
(`u12-replay-codex.pre-reason.log`, EXIT=0).

Command (export of 02f2933a0, throwaway postgres `cdr-4202-u12pg`): from api,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m
./internal/workersvc -run '<selection>' -v`; diffs and logs
`.uzi/scratch/ctl-logs/<log name>.{diff,log}`. Preflight leaves are
`TestCodexSwitchPreflightLiveDB` (credential_switch_codex_livedb_test.go,
assertion :135).

| Control | Kind | Mutation | Selection | Red at |
|---|---|---|---|---|
| u12-replay-codex | (d) guard | Codex replay branch `if run.Harness == harnessCodex {` → `if false && …` (credential_switch_apply.go :325) | lost-reply | lostreply :56 "committed Codex replay reason = \"credential_gone\", want \"credential_changed\"" |
| u12-replay-nomaterial | redundant-fence guard (decision 9) | (i) kind filter `AND kind = 'anthropic_token'` dropped from `LockAnthropicSecretForApply` (generated runtime.sql.go :16533) alone; (ii) with the Codex branch also removed | lost-reply | (i) EXIT=0 (`u12-replay-kind-only`); (ii) lostreply :51 "committed retry returned or reminted authority" (`u12-replay-nomaterial-both`): with neither copy, the shared Anthropic replay hands back the Codex secret |
| u12-foreign-owner | (b) **preservation sensitivity control** (ownership query identical at a233; a233 apply reached it through `beginClaimMutation` before any harness branch), 7d0205caa (code identical to the 02f2933a0 green) | worker filter dropped from both clauses of `GetRunOwnedByWorkerForUpdate` (generated runtime.sql.go, `$2` kept referenced); arms with further copies: + validator worker conjunct (credential_switch_codex.go :326) with Step 0 kept; + Step 0 worker conjunct (credential_switch_apply.go :313); + Step 0 and the validator conjunct; validator conjunct alone | foreign worker | :27, on the error identity: owner-query alone, owner+validator (`u12-foreign-both`, the arm that shows Step 0 refusing on its own) and owner+Step 0 return `fenced` (no material) instead of `ErrRunNotOwned`; owner+Step 0+validator returns `run claim is stale` from the binding writer (`ApplyRunCodexBinding` matches no row: its `worker_id` predicate, codex_binding.sql :345, and its owner-worker nonce `EXISTS`, :369–370, both refuse; which one decided is not isolated); validator conjunct alone EXIT=0. No arm removed every copy, so the non-owner refusal itself is not isolated |
| u12-replay-gen | (b) **preservation sensitivity control** (a233 code identity only; not executed at a233) | `prior.ClaimGeneration != gen ||` → `false && …` (:322) | new-generation replay | foreign test :93 "committed op replayed under a new generation was not fenced: [outcome=refused_committed …]" |
| u12-fence-nonce | (d) guard | nonce conjunct of `codexSwitchFence` → `true` (:325) | preflight stale_nonce | :135 "Outcome:ready … want fenced" |
| u12-fence-gen | (d) guard | generation conjunct → `true` (:327) | preflight stale_generation | :135 "Outcome:not_applied Reason:generation_mismatch … want fenced" (the later stamp-generation check still refused, with the wrong outcome) |
| u12-fence-released | (d) guard | released conjunct → `true` (:327) | preflight released | :135 "Outcome:ready … want fenced" |
| u12-fence-status | (d) guard | live-status conjunct → `true` (:328) | preflight claimed | :135 "Outcome:ready … want fenced" |
| u12-op-mismatch | (d) guard | op inequality → `false && …` (:124) | preflight wrong_operation | :135 "Outcome:ready … want superseded" |
| u12-target-mismatch | (d) guard | target secret id and validity checks → `false && …` (:139) | preflight foreign_target, wrong_target | :135 "want not_applied/target_mismatch" (foreign: "credential_gone"; wrong: "Outcome:ready") |
| u12-target-moved-rev | (d) guard | target material-revision check → `false && (…)` (:201) | target-moved API key leaf | apply test :168 "preflight accepted moved target {… ready …}" |
| u12-target-moved-acct | (d) guard | target account-revision check → `false && (…)` (:262) | target-moved subscription leaf | apply test :168, as above |

Unchanged candidate 02f2933a0: preflight, target-moved, lost-reply, the two
new tests, write-rollback and zero-row pass, EXIT=0
(`.uzi/scratch/ctl-logs/u12-green.log`); `task lint:api` 0 issues
(`u12-lint-api.log`).

Credit: a non-owner worker's apply fails with `ErrRunNotOwned` because of the
ownership query (preserved), and the refusal is backed by at least three more
copies (Step 0, the validator fence, and the binding writer's predicates); a committed Codex op
replayed in its own generation answers
`refused_committed`/`credential_changed` and never returns material (the Codex
branch and the Anthropic replay's kind filter are redundant copies of the
no-material property); replayed under a new generation it is fenced (preserved
check); a stale nonce, stale generation, released claim or non-live status
fences the validator; a different pending op is superseded; a foreign or wrong
target is refused `target_mismatch`; a target whose material or account
revision moved after preflight is refused `target_moved`; partial writes roll
back (U10). The fence-conjunct, op and target-mismatch controls ran through
preflight; the two target-moved controls failed at the preflight assertion
(:168), so the apply assertion (:172) was not exercised by them. Not
credited: the preflight path's own non-owner refusal (credential_switch_codex.go
:69–74, new code) and the validator's copy of the prior-journal check (:114–120)
— both positive only; the Step 1 worker check (argued redundant: the
generation check short-circuits first, and each claim bumps the generation); the apply path's
Step 0 copies for Codex, including the Codex-only negotiation conjunct
(credential_switch_apply.go :309); `codexSwitchFence`'s worker (:326),
negotiation (:324), harness and user conjuncts (lane: D64–D73 for the stopped
route); the no_pending_switch arms (:128, :130–131) and the stamp-generation
refusal (:133–134, seen still refusing in u12-fence-gen); the target generation and op (:140–141),
target auth mode (:200) and target account key (:261) checks; old-alias
`credential_changed`/`credential_gone` refusals (:208–217, :239–244); and
apply-route reds for the validator conjuncts.

### U13 — stopped-support negotiation, expected-epoch latch and unsupported-response obligation (2026-11-18)

Baselines: at a233883de3c36bae2e940f999cd9d582889dad00 none of these seams
exist — `api/internal/workersvc/credential_switch_codex.go`,
`api/internal/workersvc/codex_switch_stopped.go` and
`agent/src/codex-switch-preflight.ts` are absent, `agent/src/client.ts` has no
`codex_switch_stopped_v1` or `settleCodexSwitchStopped`, the a233
`run_credential.go` has no `CodexSwitchStoppedV1`, and a233 refused every
Codex owner request (credential_override.go :204, see D74) and live switch
(runner.ts :25735–25738). All controls are class (d).

Mechanism (candidate):
- Server negotiation: `codexSwitchNegotiated` (credential_switch_codex.go
  :393–395) requires the `codex_credential_switch_v1` capability and
  `currentSwitchApplyRegistration` (:398–402: the snapshot nonce equals the
  apply nonce). The owner request refuses with
  `CredentialSwitchWorkerUnsupportedError` when the holding worker is not
  negotiated, lacks `codex_switch_stopped_v1`, or has a stale heartbeat
  (run_credential.go :635); worker preflight answers
  `not_applied/worker_upgrade_required` without stopped support
  (credential_switch_codex.go :78–80).
- Worker admission: `supportsCodexSwitchStopped` (client.ts :2209–2212)
  requires this registration's stopped capability and the server's stopped
  feature as well as Codex apply.
- Unsupported-response obligation: a preflight refusal whose reason is not in
  the typed set (today that includes `worker_upgrade_required`, absent from the
  list at codex-switch-preflight.ts :21–27) is a retained stop, never a clear
  (:125). After admission, a withdrawn feature
  does not cancel the one bounded stopped settlement on the admitted tuple
  (`settleCodexSwitchStopped`, client.ts :2215–2240, deliberately not gated on
  the feature; a nonce change fences it).
- Epoch latch: the runner freezes `{claimGeneration, registerNonce,
  expectedEpoch}` from the first READY (runner.ts :17430, :17494–17496) and
  reuses that object for each of up to three serial settlement attempts
  (loop at :17499); a successor READY with a different epoch pauses instead of
  applying (:17595).

New test (69c028a39): `TestOwnerCodexSwitchUnsupportedWorkerLiveDB`
(api/internal/workersvc/credential_switch_codex_owner_unsupported_livedb_test.go),
leaves supported (:49, :52), missing_switch_capability,
stale_apply_registration, missing_stopped_capability, stale_heartbeat: each
refusal must be `CredentialSwitchWorkerUnsupportedError` (:58) with the switch
stamp unchanged (:62) and no target written (:65). Existing: the handler route
test `TestWorkerCodexSwitchPreflightRouteLiveDB`
(api/internal/handler/codex_switch_preflight_routes_livedb_test.go, leaf
missing_stopped_support, :38, assertion :55), `codex-switch-preflight.test.ts`
("committed or unknown refusal retains A gate", :179–183),
`client-credential-switch-apply.test.ts` (stopped admission matrix :573–582;
settlement after withdrawal :584–600) and the executor matrix
(`Codex real Runner pending op disappears after clean A teardown (…)`, frozen
identical tuple asserted at :32770–32780; `Codex real Runner rejects successor
READY epoch 12 after latching epoch 11`, :34277).

Commands. Go (export of 69c028a39, throwaway postgres `cdr-4202-u13pg`): from
api, `GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1
-timeout=6m <package> -run '<selection>' -v`. Agent (export of 69c028a39 with
the primary agent/node_modules symlinked): `npm run typecheck && node --import
tsx --test --test-timeout=120000 --test-name-pattern='<pattern>' <file>`.
Logs `.uzi/scratch/ctl-logs/<log name>.{diff,log}`.

| Control | Kind | Mutation | Selection | Red at |
|---|---|---|---|---|
| u13-own-negotiated | (d) guard | owner request `!codexSwitchNegotiated(w)` → `false` (run_credential.go :635) | unsupported leaves missing_switch_capability, stale_apply_registration | :58 "… not refused as unsupported: <nil>", both |
| u13-own-stopped | (d) guard | owner request stopped-capability check → `false` (:635) | missing_stopped_capability | :58 |
| u13-own-heartbeat | (d) guard | owner request stale-heartbeat check → `false` (:635) | stale_heartbeat | :58 |
| u13-neg-cap | (d) guard | `codexSwitchNegotiated` capability conjunct → `true` (credential_switch_codex.go :394) | missing_switch_capability | :58 |
| u13-neg-reg | (d) guard | `codexSwitchNegotiated` registration conjunct → `true` (:394) | stale_apply_registration | :58 |
| u13-preflight-stopped | (d) guard | preflight stopped-support check → `false && …` (:78) | handler preflight route, missing_stopped_support | handler test :55 "unnegotiated preflight status=200 body={\"outcome\":\"fenced\" …}" (want not_applied/worker_upgrade_required) |
| u13-w-unknown-reason | (d) guard | untyped-reason retained stop made unsatisfiable (`… && Number.NaN > 0`, codex-switch-preflight.ts :125) | unknown refusal retains A gate | codex-switch-preflight.test.ts :181 (retained_stop expected) |
| u13-w-admit-cap | (d) guard | `supportsCodexSwitchStopped` capability conjunct → `(… \|\| true)` (client.ts :2211) | stopped admission matrix | client test :579 |
| u13-w-admit-feature | (d) guard | its feature conjunct → `true` (:2211) | stopped admission matrix | client test :579 |
| u13-w-withdrawal | (d) behavioural mutation | settlement additionally gated on `supportsCodexSwitchStopped()` (client.ts :2220) | settlement after withdrawal | client test :593 (the admitted settlement after withdrawal returned unconfirmed, not confirmed) |
| u13-w-latch-frozen | (d) guard | `Object.freeze({…})` → `({…})` for the stopped tuple (runner.ts :17494) | clean-stopped-lost-reply | executor :34285 "fake ownership is independent of local park evidence": the frozen-tuple assertion (:32776) runs inside the production-called settlement hook, so its failure surfaced as a missing park, not at :32776 |
| u13-w-latch-successor | (d) guard | successor epoch inequality made unsatisfiable (`Number.NaN > 0 && …`, runner.ts :17595) | successor READY epoch 12 after latching 11 | executor :34277 "advanced successor epoch reached a forbidden second apply" |

Not credited: the first u13-w-unknown-reason (`false && …`) and u13-w-admit-cap
(`true && …`) forms failed typecheck (TS2339, TS6133;
`*.compilefail.log`). The runner's own admission check
`!this.client.supportsCodexSwitchStopped()` (runner.ts :17380) has no control
(the executor matrix stubs `supportsCodexSwitchStopped`); the feature-withdrawal
matrix leaf `clean-stopped-withdrawn` and `clean-stopped-unsupported` are
positive only; `codexSwitchFence`'s negotiation conjunct (:324) and apply Step 0's
(credential_switch_apply.go :309) are redundant copies not isolated here (see
U12). Also not controlled: the owner request's null-heartbeat conjunct
(`!w.LastHeartbeatAt.Valid`, :635), `currentSwitchApplyRegistration`'s
validity/non-empty sub-conjuncts (u13-neg-reg replaced the whole call and only
a nonce mismatch was exercised), `supportsCodexSwitchStopped`'s
`supportsInPlaceCodexCredentialSwitch()` conjunct (client.ts :2210), the
refused leaves' stamp-unchanged assertions (:62, :65; no guard-removal mutation
reaches them, since :58 fails first for every guard mutation run), the
identical-tuple replay assertion (executor :32778) and the
first-READY latch (runner.ts :17430) separately from the freeze, and the
specific `worker_upgrade_required` reason on the worker (the preflight test uses
an arbitrary untyped reason; adding that reason to the typed list would not
redden any U13 test).

Unchanged candidate 69c028a39: the unsupported-worker and preflight tests
(workersvc), the handler preflight route test, the two agent unit files (140/140)
and the seven executor cases pass, EXIT=0
(`.uzi/scratch/ctl-logs/u13-green-{workersvc,handler,agent}.log`); `task lint:api`
0 issues (`u13-lint-api.log`).

Credit: the owner request refuses an un-negotiated, stopped-unsupported or
stale worker as unsupported (the unchanged stamp is a positive assertion); negotiation needs both
the Codex capability and a current apply registration; preflight without
stopped support answers `worker_upgrade_required`; the worker admits a Codex
switch only with both the stopped capability and feature (in the client; the
runner's own check is uncontrolled), treats an untyped refusal as a retained
stop, keeps the admitted settlement after feature withdrawal, freezes the expected-epoch tuple, and refuses a successor whose
epoch moved.

### U14 — stopped pending-input carve-outs and wall-budget branches (2026-11-18)

Baseline: the stopped settlement is new — a233 has no
`api/internal/store/queries/codex_switch_stopped.sql` or
`api/internal/workersvc/codex_switch_stopped.go` (see D1–D24 and U13) — so all
controls are class (d).

Mechanism: `SetCodexSwitchStopped` (api/internal/store/queries/codex_switch_stopped.sql;
generated api/internal/store/codex_switch_stopped.sql.go, which the mutants
edit) refuses while an unapplied, non-superseded input of the eight
competing kinds exists, except a `stop`/`cancel`/`reject_plan` input at or
below a milestone reconciliation's `stop_cutoff`; and refuses when the wall
budget is exhausted unless the run is interactive or not yet started.
Remaining budget = `COALESCE(budget_wall_seconds, timeout) +
budget_extension_seconds + budget_finalize_seconds - (GREATEST(0, elapsed) -
budget_paused_seconds)`, with elapsed measured to `status_since` when paused
and to the statement clock otherwise (running and awaiting_*). The service passes
`TimeoutSeconds: int32(s.p.RunTimeout.Seconds())`
(api/internal/workersvc/codex_switch_stopped.go :42); the route fixture builds
the service without `RunTimeout`, so the fixture default is 0.

New leaves (f7a795fb1) in `TestWorkerCodexStoppedGuardsRouteLiveDB`
(api/internal/handler/codex_switch_stopped_guards_livedb_test.go, `inputCases`
from :332, cutoff helper `insertStoppedCutoff` :159), all on a running run:
input_applied_pause, input_superseded_pause, input_stop_under_cutoff (accept);
input_pause_under_cutoff (refuse); budget_running_expired (refuse);
budget_interactive_exempt, budget_unstarted_exempt,
budget_extension_keeps_positive, budget_finalize_keeps_positive,
budget_paused_seconds_keeps_positive (accept); budget_default_timeout (refuse:
a NULL wall falls back to the 0 fixture timeout);
budget_default_timeout_with_extension (accept). Each uses `reportStoppedRoute`,
whose assertions are "stop acceptance mismatch" (:263) and "refusal mismatch"
(:275); as a `t.Helper` they report at the table call site (:381 at f7a795fb1,
:391 at 1b3afe49e). Review rework 1b3afe49e adds input_stop_above_cutoff
(a stop filed after the cutoff, refuse) and budget_unstarted_zero_base
(`started_at` NULL, NULL wall, zero fixture timeout: accept only through the
unstarted arm).

Command (export of f7a795fb1 for u14-* rows, 1b3afe49e for u14r-* rows except
u14r-cutoff-equal at 039947e1d, a ledger-only child, which ran the whole
test under `-run '^TestWorkerCodexStoppedGuardsRouteLiveDB$'` (one leaf red, 38 pass); throwaway postgres `cdr-4202-u14pg`): from api,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m
./internal/handler -run '^TestWorkerCodexStoppedGuardsRouteLiveDB$/^<leaf>$' -v`;
logs `.uzi/scratch/ctl-logs/<log name>.{diff,log}`.

| Control | Kind | Mutation (generated SQL) | Leaf | Red at |
|---|---|---|---|---|
| u14-applied | (d) guard | drop `i.applied_at IS NULL AND` | input_applied_pause | :381 "stop acceptance mismatch: status=409 applied=false" |
| u14-superseded | (d) guard | drop `AND i.disposition IS DISTINCT FROM 'superseded'` | input_superseded_pause | :381, as above |
| u14-cutoff | (d) guard | cutoff exemption made unsatisfiable (`i.id <= a.stop_cutoff AND false`) | input_stop_under_cutoff | :381, as above |
| u14-cutoff-kind | (d) guard | drop the cutoff's `AND i.kind IN ('stop','cancel','reject_plan')` | input_pause_under_cutoff | :381 "refusal mismatch: status=200 applied=true" |
| u14-running-clock | (d) behavioural mutation | running elapsed measured to `r.started_at` instead of the clock (`ELSE r.started_at END`) | budget_running_expired | :381 "refusal mismatch" |
| u14-interactive | (d) guard | `OR r.interactive` → `OR false` | budget_interactive_exempt | :381 "stop acceptance mismatch" |
| u14r-unstarted-isolated | (d) guard, 1b3afe49e | `OR r.started_at IS NULL` → `OR false` | budget_unstarted_zero_base | :391 "stop acceptance mismatch: status=409 applied=false" |
| (superseded) u14-unstarted | observation only, f7a795fb1 | the same arm alone (`u14-unstarted-arm-only`) and `GREATEST(0, elapsed)` → `(0 + elapsed)` alone (`u14-unstarted-greatest-only`) both EXIT=0 on budget_unstarted_exempt; both together red at :381 (`u14-unstarted-both`) | budget_unstarted_exempt (positive 60 s wall) | not credited: the combined arm also removes the elapsed clamp, a second fence; the apparent redundancy holds only while the base is positive (`GREATEST` ignores the NULL elapsed) |
| u14r-cutoff-boundary | (d) guard, 1b3afe49e | `AND i.id <= a.stop_cutoff` → `AND true` | input_stop_above_cutoff | :391 "refusal mismatch: status=200 applied=true" |
| u14r-cutoff-equal | (d) guard, 039947e1d | `AND i.id <= a.stop_cutoff` → `AND i.id < a.stop_cutoff` | input_stop_under_cutoff (pending id equals the cutoff) | :391 "stop acceptance mismatch: status=409 applied=false" |
| u14-extension | (d) behavioural mutation | `+ r.budget_extension_seconds` → `+ 0` | budget_extension_keeps_positive | :381 "stop acceptance mismatch" |
| u14-finalize | (d) behavioural mutation | `+ r.budget_finalize_seconds` → `+ 0` | budget_finalize_keeps_positive | :381, as above |
| u14-paused-secs | (d) behavioural mutation | `- r.budget_paused_seconds` → `- 0` | budget_paused_seconds_keeps_positive | :381, as above |
| u14-default-timeout | (d) behavioural mutation | `COALESCE(budget_wall_seconds, $7)` → `COALESCE(budget_wall_seconds, $7 + 1000000)` | budget_default_timeout | :381 "refusal mismatch" |
| u14r-coalesce | (d) guard, 1b3afe49e | `COALESCE(budget_wall_seconds, $7)` → `(budget_wall_seconds + 0 * $7)` (no fallback; a NULL wall makes the remainder NULL) | budget_default_timeout_with_extension | :391 "stop acceptance mismatch" |

Unchanged candidate 1b3afe49e: the whole test passes (40 PASS lines, parent
and all leaves, no skips), EXIT=0 (`.uzi/scratch/ctl-logs/u14r-green.log`);
`task lint:api` 0 issues (`u14r-lint-api.log`). (f7a795fb1's u14-green.log, 38
PASS, is the first-round record; the u14-* rows ran there, the u14r-* rows at
1b3afe49e, whose only change adds the two leaves.)

Credit: an applied or superseded competing input does not block the stopped
settlement; a stop/cancel/reject_plan at or below a reconciliation cutoff does
not, but a pause under the same cutoff, or a stop filed after it, still does;
a running run is measured to the clock and refused when its budget is spent;
interactive and unstarted runs are exempt (the unstarted arm is what accepts
when the base is zero); extension, finalize and paused seconds each count; a
NULL wall coalesces to a non-NULL base (the fixture's zero timeout; the value
passed from `RunTimeout` is not discriminated). Not
credited: the chat/judge budget exemption (unreachable: the kind filter
excludes them earlier), the production 6-hour default timeout (the fixture
passes 0), awaiting_* statuses, the paused-run variants of the input carve-outs,
the cancel/reject_plan kinds under the cutoff (only stop was exercised), the
cutoff's same-run correlation (`a.run_id = i.run_id`; no foreign-run
reconciliation leaf), the elapsed clamp at 0 for a future `started_at`, and
the specific `RunTimeout` value (dropping `TimeoutSeconds` at
codex_switch_stopped.go :42 would pass every leaf while the fixture passes 0).
Fixture-shape notes: a NULL-disposition stop below a cutoff is a state the
reconciliation writer normally marks superseded (milestone_reconciliation.go
:584), a running run with NULL `started_at` has no known production writer, and
`interactive` is set on an issue run; the predicate keys on none of these, so
the controls stand, but the states are synthetic.

### U15 — stopped bounds, ownership, reconciliation path, statuses and binding ownership (class (d), redundant-fence, (b); one service-entry unit control, supporting only)

Candidate b52b38e6d9e573dda9663b12b32b9d06e709c948 adds
api/internal/handler/codex_switch_stopped_authority_bounds_livedb_test.go
(`TestWorkerCodexStoppedAuthorityBoundsRouteLiveDB`, 12 leaves;
`TestWorkerCodexStoppedOwnershipRouteLiveDB`, 11 leaves) and extends
api/internal/workersvc/codex_switch_stopped_test.go
(`TestCodexSwitchStoppedAuthorityBounds`: empty, invalid UTF-8, NUL and
129-byte nonces at the service entry). Every route leaf except
foreign_binding goes through the authenticated WorkerRoutes `/state` route and
the pool transaction of `setupStoppedGuardsRoute`, after withdrawal, and
asserts no write (`assertStoppedSnapshot`). foreign_binding, on the candidate,
asserts only that `runs_codex_secret_fk` refuses the foreign binding (23503,
constraint name pinned at f355da022) and reaches the route only when that FK
is relaxed.

a233 reference: the stopped settlement is new.
`git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/workersvc/codex_switch_stopped.go`
and `…:api/internal/store/queries/codex_switch_stopped.sql` do not exist, and
worker_state_decode.go does not exist at a233, so the
bounds, reconciliation-path, status and worker-join controls are class (d).
Three seams are identical at a233 and are recorded as (b) preservation
controls: `validateReconciledGeneration` (milestone_reconciliation.go:343 at
both), the worker filters of `GetRunOwnedByWorkerForUpdate` /
`GetRunOwnedByWorker` (runtime.sql.go :8290 / :8027 at a233), and the
composite binding foreign key (00202_run_codex_binding.sql:73–74 at both).

Copy census (search: `grep -rn 9007199254740991 api/internal --include=*.go`
excluding tests, plus the stopped path read from the decoder through
`stopCodexSwitch`; exhaustive for that path):
- claim_generation / expected_epoch bounds: decoder
  worker_state_decode.go:52–53 and service codex_switch_stopped.go:22–23 (two
  copies; `validateReconciledGeneration`'s `*g <= 0` is a third lower-bound
  copy only when a reconciliation exists, which these leaves do not have);
- empty nonce: decoder :53 `req.RegisterNonce == ""` and service :24
  `validHoldRegisterNonce` (`nonce != ""`, credential_switch.go:576);
- nonce length, NUL and UTF-8: service :24 only;
- reconciled generation / release: `validateReconciledGeneration`
  (milestone_reconciliation.go:343) and the stopped query
  (codex_switch_stopped.sql.go:21);
- worker identity: `GetRunOwnedByWorkerForUpdate` (runtime.sql.go:8535
  candidates and :8558 final select, called from `beginClaimMutation`,
  milestone_reconciliation.go:298), the stopped join (codex_switch_stopped.sql.go:19,
  with `w.id = $2`), and `runOwnedByWorker` → `GetRunOwnedByWorker`
  (service.go:6913, runtime.sql.go:8254; called at codex_switch_stopped.go:47
  before commit);
- status: the stopped query's IN list (codex_switch_stopped.sql.go:27) only;
  `beginClaimMutation` reads no status.

Command (export of b52b38e6d, throwaway postgres `cdr-4202-u15pg`; the FK
control used a fresh `cdr-4202-u15fk`, since a migration edit applies only to
an empty database), from the export's api directory:

```sh
GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m \
  ./internal/handler -run '^<Test>$/^<leaf regex>$' -v
```

(`./internal/workersvc -run '^TestCodexSwitchStoppedAuthorityBounds$'` for
u15-utf8). Mutants and logs: `.uzi/scratch/ctl-logs/u15-*.{diff,log}`; each mutant
log ends `SHA=b52b38e6d… EXIT=1` (u15r-binding-fk at f355da022). Bounds leaves report at
codex_switch_stopped_authority_bounds_livedb_test.go:67 ("bounds mismatch");
the ownership leaves at :82, :89 and :110 are `reportStoppedRoute` call sites
("refusal mismatch"), :101 is "foreign worker refusal mismatch", :131 is the
foreign_binding report.

| Control | Class | Mutation | Leaf | Red |
| --- | --- | --- | --- | --- |
| u15-gen-lower | redundant-fence (2 copies) | `*req.ClaimGeneration <= 0 \|\|` removed in decoder and service | generation_zero, generation_negative | :67 "status=409 … want 400" (both) |
| u15-gen-upper | redundant-fence (2 copies) | `\|\| *req.ClaimGeneration > 9007199254740991` removed in both | generation_unsafe | :67 "status=409 … want 400" |
| u15-gen-edge | (d), both copies | `>` → `>=` in both | generation_max_safe | :67 "status=400 … want 409" |
| u15-epoch-lower | redundant-fence (2 copies) | `*req.ExpectedEpoch <= 0 \|\|` removed in both | epoch_zero, epoch_negative | :67 "status=409 … want 400" (both) |
| u15-epoch-upper | redundant-fence (2 copies) | `\|\| *req.ExpectedEpoch > 9007199254740991` removed in both | epoch_unsafe | :67 "status=409 … want 400" |
| u15-epoch-edge | (d), both copies | `>` → `>=` in both | epoch_max_safe | :67 "status=400 … want 409" |
| u15-nonce-empty | redundant-fence (2 copies) | decoder `\|\| req.RegisterNonce == ""` removed; service `!validHoldRegisterNonce(…)` → `len(req.RegisterNonce) > 128` | nonce_empty | :67 "status=409 … want 400" |
| u15-nonce-length | (d) | service `!validHoldRegisterNonce(…)` → `req.RegisterNonce == ""` | nonce_over_128 | :67 "status=409 … want 400" |
| u15-nonce-edge | (d) | `len(nonce) <= 128` → `< 128` (shared helper) | nonce_128 | :67 "status=400 … want 409" |
| u15-nonce-nul | (d) | `strings.ContainsRune(…, 0)` → `(…, -1)` (never true) | nonce_nul | :67 "status=500 … want 400" |
| u15-utf8 | service-entry unit control (not (d); not route evidence) | `!utf8.ValidString(req.RegisterNonce)` → `!utf8.ValidString("")` | TestCodexSwitchStoppedAuthorityBounds `"\xff"` | codex_switch_stopped_test.go:36 "semantic completion authority unavailable" |
| u15-recon-generation | redundant-fence (2 copies; Go copy (b)) | `*g != r.ClaimGeneration \|\|` removed; SQL `r.claim_generation = $4::bigint` → `$4::bigint = $4::bigint` | reconciled_generation_mismatch | :82 "refusal mismatch: status=200 applied=true" |
| u15-recon-released | redundant-fence (2 copies; Go copy (b)) | `\|\| r.ClaimReleasedAt.Valid` removed; SQL `AND r.claim_released_at IS NULL` removed | reconciled_claim_released | :89 "refusal mismatch: status=200 applied=true" |
| u15-foreign-worker | redundant-fence (4 copies; query copies (b)) | both `runs.worker_id = $2` filters of GetRunOwnedByWorkerForUpdate and GetRunOwnedByWorker's `worker_id = $2` → `$2::uuid = $2::uuid`; stopped join `w.id = r.worker_id` → `true` | foreign_worker (same user, own nonce) | :101 "status=200 applied=true" |
| u15-status | (d) | the stopped IN list removed | status_queued, _limit_wait, _pool_wait, _recovery_wait, _completed, _failed, _cancelled | :110 "refusal mismatch: status=200 applied=true" (all seven) |
| u15-binding-fk | (b) preservation (schema) | 00202 FK `(user_id, codex_secret_id) → user_secrets(user_id, id)` → `(codex_secret_id) → user_secrets(id)` | foreign_binding | :131 "refusal mismatch: status=200 applied=true"; rerun as u15r-binding-fk at f355da022 (constraint name pinned), same red |

Unchanged candidate b52b38e6d: the two new tests plus
`TestWorkerCodexStoppedGuardsRouteLiveDB` and
`TestWorkerCodexStoppedAuthorityRouteLiveDB` pass (94 PASS lines, no FAIL or
SKIP), `SHA=b52b38e6d… EXIT=0` (`.uzi/scratch/ctl-logs/u15-green.log`);
`TestCodexSwitchStoppedAuthorityBounds` passes (`u15-unit-green.log`);
`task lint:api` 0 issues (`u15-lint-api.log`). After the constraint-name pin,
f355da022: the two new tests pass (25 PASS lines), `SHA=f355da022… EXIT=0`
(`u15r-green.log`), lint 0 issues (`u15r-lint-api.log`). Runtime: about 9.6 s
marginal in-package (5.16 s + 4.41 s in u15-green.log, same worker, warm
cache, migrated schema, one serial run); the standalone 18.7 s in
u15-first.log (no SHA line) includes one-time schema migration. Hosted effect
on test-livedb-workersvc and test-api-store-it is unmeasured.

Credit: the worker-latched bounds (generation and epoch lower, upper and the
inclusive 2^53−1 edge; nonce non-empty, ≤128 bytes inclusive, no NUL); generation and release refusal on the
reconciliation path; worker identity (a same-user foreign worker with its own
current nonce); refusal of the seven statuses outside the live set and
`claimed` (D); binding ownership as the composite foreign key, identical at
a233 (the predicate checks presence only, so a foreign binding is refused by
being unrepresentable).

Supporting only, outside the approved classes: UTF-8 validity (u15-utf8,
red at the service entry; the route cannot deliver invalid UTF-8, so the guard
has no route-reachable negative case and it is not credited).

Not credited: the redundant copies individually (no single-copy mutant was
run; each copy's removal alone is expected to be masked); the NUL guard's
contribution is the typed 400 only, since without it Postgres rejects the NUL
byte (500) and nothing is accepted; UTF-8 is controlled at the service entry,
not the route (encoding/json substitutes U+FFFD); a NULL
`snapshot_register_nonce`; a cross-user worker (D62/D63 cover the user-owner
comparisons); `validateReconciledGeneration`'s `*g <= 0` copy; binding identity
beyond ownership (the alias relinked to another provider account) — the
stopped predicate has no such clause, settlement reads and releases no
material, and this stays unvalidated for the owner batch; the egress-lane
`laneMismatch` check in `runOwnedByWorker` (fail-closed via rollback, not an
identity copy); the two GetRunOwnedByWorkerForUpdate copies differ under a
lock wait (only the final FOR UPDATE select re-checks the newest row), which
no single-copy control separates. The stopped query's identity copy is the
join together with `w.id = $2`; the mutant removed the join term.

Fixture-shape notes: the status leaves set only `status` on a running run that
keeps its worker, `started_at` and an unreleased claim, a state production
does not reach for queued or terminal runs (DB CHECKs permit it), so the IN
list is shown to be the sole fence only in this synthetic state; the foreign
worker is inserted directly with a hand-set `snapshot_register_nonce` rather
than registered, which the predicate (nonce equality) does not distinguish.

### U16 — stopped withdrawn X/Y, resume/reclaim/re-registration, late-control lock order and running policy hold (2026-11-18)

Candidate 8cca0f215438496b514dd8dd0cbf75294d70ec97 adds
api/internal/handler/codex_switch_stopped_xy_livedb_test.go
(`TestWorkerCodexStoppedWithdrawnXYRouteLiveDB`: xy_settles, y_pending,
stale_x_withdraw, through the authenticated `/state` route; Y is filed with
the service `SetRunCredential` and withdrawn through the route with Y's op id)
and api/internal/workersvc/codex_switch_stopped_lockorder_livedb_test.go
(`TestCodexSwitchStoppedLateControlLockOrderLiveDB`: policy, registration,
pause; `TestCodexSwitchStoppedRunningPolicyHoldLiveDB`). The existing
`TestCodexSwitchStoppedResumeReclaimReregistrationLiveDB` (service SetState on
the real store transaction) supplies the resume, reclaim and registration
leaves.

The lock-order test differs from the existing
`TestCodexSwitchStoppedConcurrentLateControlLiveDB`, whose late control is
written after the report starts and could commit before the report reads,
so a lock mutant there need not go red. Here (at 8cca0f215) the control is
written, uncommitted, under the held worker lock before the report starts;
the test commits only once `pg_blocking_pids` shows a backend blocked behind
the control's transaction, or the report has already returned. From b853f9487
the pause_run_only control takes no worker lock, and a report that returns
without having been seen blocked fails.

a233 reference: the stopped settlement is new (no
`api/internal/workersvc/codex_switch_stopped.go` or
`queries/codex_switch_stopped.sql` at a233), so the predicate controls are
class (d). Identical at a233, and therefore (b) preservation copies:
`beginClaimMutation`'s `GetWorkerForUpdate` / `GetRunOwnedByWorkerForUpdate`
calls (milestone_reconciliation.go:295/:298 at both); the two `FOR UPDATE`
clauses of GetRunOwnedByWorkerForUpdate (runtime.sql.go :8307/:8318 at a233,
:8549/:8560 now); the withdraw op fence of ClearCredentialSwitchByWorker
(runtime.sql.go:2402 at a233, :2418 now); `unresolvedPolicyEpisode`'s hold arm
(policy_hold.go:190 at both); and the `runs_policy_hold_shape` hold arm
(00397_compact_policy_hold.sql:42 at both).

Copy census (the stopped path as traced in U15, plus the withdraw route
`failCredentialSwitch`, credential_switch.go:205–261):
- pending Y: the two marker conjuncts (codex_switch_stopped.sql.go:28) only;
- stale X withdrawal: ClearCredentialSwitchByWorker's op conjunct only
  (`optionalSwitchOpID` parses, it does not compare);
- resume (`ResumePausedRun`: status → queued, `codex_cap_hash` NULL,
  `codex_claim_epoch` + 1, worker kept): status IN list (:27), capability
  presence (:24) and epoch equality (:23), three copies;
- reclaim (generation + 1, no reconciliation): generation conjunct (:21);
- re-registration: nonce equality (:22);
- late control vs the report: worker row lock (`GetWorkerForUpdate`) for a
  worker-row control; for a run-row control also the run row locks
  (`FOR UPDATE OF lead`, which for a non-cross-check run is the run itself,
  and `FOR UPDATE OF runs`);
- running `codex_policy_hold`: `runs_policy_hold_shape` (unrepresentable on a
  running row), the Go hold arm (policy_hold.go:190, via
  codex_switch_stopped.go:37) and the SQL `r.hold_reason IS NULL` (:29), three
  copies.

Command: from the export's api directory,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m <pkg> -run '<regex>' -v`
(`.uzi/scratch/u8tools/ctl.sh`; export of 8cca0f215; throwaway postgres
`cdr-4202-u16pg`, and a fresh `cdr-4202-u16ph` for the migration control).
Mutant diffs and logs: `.uzi/scratch/ctl-logs/u16-*.{diff,log}`; each mutant log
ends `SHA=8cca0f215… EXIT=1`.

| Control | Class | Mutation | Leaf | Red |
| --- | --- | --- | --- | --- |
| u16-y-markers | redundant-fence (the two pending-switch markers, both set by Y; each is separately credited by D25–D56) | both switch-marker conjuncts → `true` | xy route y_pending | codex_switch_stopped_xy_livedb_test.go:67 "refusal mismatch: status=200 applied=true" |
| u16-stale-withdraw | (b) preservation | withdraw op conjunct `runs.credential_switch_op_id = $4::uuid` → `$4::uuid = $4::uuid` | xy route stale_x_withdraw | :75 "delayed X withdraw status=200; want 409" |
| u16-resume | redundant-fence (3 copies) | status IN list removed; epoch → `$6::bigint = $6::bigint`; `AND r.codex_cap_hash IS NOT NULL` removed | ResumeReclaimReregistration resume | codex_switch_stopped_livedb_test.go:177 "delayed report/replay disposition mismatch" |
| u16-reclaim | (d) | generation → `$4::bigint = $4::bigint` | reclaim | :177 (same message) |
| u16-registration | (d) | nonce → `$5::text = $5::text` | registration | :177 (same message) |
| u16-worker-lock | (b) preservation | `q.GetWorkerForUpdate` → `q.GetWorkerByID` in beginClaimMutation | lock-order registration | codex_switch_stopped_lockorder_livedb_test.go:86 "accepted=true err=<nil>"; policy and pause PASS (masked by the run lock) |
| u16-locks | redundant-fence (worker lock + two run-lock clauses; (b)) | the u16-worker-lock change plus both `FOR UPDATE` clauses of GetRunOwnedByWorkerForUpdate removed | lock-order policy, registration, pause | :86 (all three) |
| u16r-worker-lock | (b), b853f9487 | as u16-worker-lock | lock-order registration | :89 "accepted=true err=<nil>"; policy, pause and pause_run_only PASS |
| u16r-run-locks | redundant-fence (the two run-lock clauses; (b)), b853f9487 | both `FOR UPDATE` clauses removed, worker lock kept | lock-order pause_run_only (control writes the run row without the worker lock) | :89 "accepted=true err=<nil>"; policy, registration and pause PASS (their controls hold the worker lock) |
| u16r-locks | redundant-fence, b853f9487 | both changes | all four lock-order leaves | :89 (all four) |
| u16-policy-hold | redundant-fence (3 copies incl. schema; (b) for the Go and schema copies, (d) for the new SQL copy), at 8cca0f215 (function unchanged at b853f9487, where its Fatal is at :113) | Go hold arm removed; SQL `r.hold_reason IS NULL` → `(… OR r.hold_reason = 'codex_policy_hold')`; 00397 hold arm → `OR (policy_outcome_id IS NOT NULL)` (fresh database) | RunningPolicyHold | :107 "running policy hold accepted <nil>" |

Review rework b853f9487 adds the lock-order leaf pause_run_only and requires
the report to have been observed blocked behind the control
(`report was refused without waiting behind the control`), which moves the
red line from :86 to :89; the u16r rows ran there and their logs end
`SHA=b853f9487… EXIT=1` (same `ctl.sh` command and `cdr-4202-u16pg`). At b853f9487 every `^TestCodexSwitchStopped`
workersvc test passes (75 PASS lines, `u16r-green-w.log`), the route test
passes (4 PASS lines, `u16r-green-h.log`), both EXIT=0; `task lint:api` 0
issues (`u16r-lint-api.log`).

Unchanged candidate 8cca0f215: the route test passes (4 PASS lines,
`u16-green-h.log`) and every `^TestCodexSwitchStopped` workersvc test passes
(74 PASS lines, no FAIL or SKIP, `ok … 33.097s`, `u16-green-w.log`), both
`SHA=8cca0f215… EXIT=0`; `task lint:api` 0 issues (`u16-lint-api.log`).
Runtime: the new route test 1.78 s in-package, the two new workersvc tests
1.33 s and 0.37 s (same worker, warm cache, migrated schema, one serial run;
first-run logs `u16-first-{h,w}.log` include schema migration and carry no SHA
line). Hosted effect unmeasured.

Credit: a pending second switch Y refuses a stop latched from X; a delayed
withdrawal naming X cannot discharge Y (op fence, (b)); after resume, reclaim
or re-registration the latched stop is refused (resume through three copies);
a late control on the worker row is fenced by the worker lock, and a late
control on the run row by the run row locks alone (pause_run_only, whose
writer takes no worker lock, as single-statement owner writers do), all (b);
where the writer also takes the worker lock, either lock suffices (masking
observed in u16r-worker-lock and u16r-run-locks); a running run cannot carry
`codex_policy_hold` and, were it representable, the stop would still refuse
it (three copies).

Not credited: xy_settles (and the service `TestCodexSwitchStoppedWithdrawnXYLiveDB`)
is a positive acceptance with no removal control; nothing in the predicate
keys on op history, so no guard exists to remove. The individual copies of
each redundant fence are not separated (u16-worker-lock shows the run lock
masks run-row controls). The lock-order leaves cover one policy, two pause
and one registration write, not every owner writer; a late write that takes no
row lock on the worker or run is not covered (the auditor found none on this
path: inserts into run_user_inputs and run_milestone_reconciliations take a
key-share lock on the run through their foreign keys, and the unlocked
`disposition='superseded'` updates only remove fences). A paused-origin stop
replays with no write, its statement taking no lock of its own, so the
caller's locks are its only fence there, and no lock-order leaf covers that branch. The barrier matches
any backend blocked behind the control's transaction, not specifically the
report's (the test runs alone, `-p=1`, no `t.Parallel`). Op-less (v1)
`credential_switch_failed` withdrawals carry no op id and are unfenced by op
(`$4::uuid IS NULL`, runtime.sql.go:2418; the documented liveness tradeoff at
credential_switch.go:198–204), so they can discharge Y; only the op-carrying
withdrawal is credited. The advertisement leaf
(acceptance) is U13's credit. Fixture-shape notes: Y is filed with the service
owner call rather than the owner HTTP route (U1 covers that route); the policy
episode is written directly; the lock-order fixtures write the late control
by SQL; reclaim (`claim_generation+1`) and re-registration
(`snapshot_register_nonce` only) are one-column writes, whereas a real reclaim
or registration changes more (a fresh claim, retired active claims), so the
single-copy census holds for these synthetic states only; the running policy
hold early return accepts any violation of `runs_policy_hold_shape`, not only
its hold arm (the u16-policy-hold red shows the other arms hold for this
fixture).

### U17 — stopped preservation of session, gate, question and replay effects (effect-injection, 2026-11-18)

Candidate 0434cd750e6d167630b5f819c9504986511931e5 adds
api/internal/handler/codex_switch_stopped_state_preservation_livedb_test.go
(`TestWorkerCodexStoppedStatePreservationRouteLiveDB`) and extends the shared
`stoppedRouteSnapshot` (codex_switch_stopped_guards_livedb_test.go) with
`run_gate_presentations`. After withdrawal the fixture populates the session
(`runs.session_id`, set by the route fixture), the canonical gate
(`runs.gate_revision`, `gate_presentation_id`, `gate_presented_payload`,
`gate_payload_digest` and the matching `run_gate_presentations` row) and the
open question (`runs.open_question_id` and a `kind='question'` run message,
per the source census above: no separate questions table), reads it back,
then sends a first stop and a replay through the authenticated `/state`
route. `assertStoppedSnapshot` compares the whole run row (except status,
status_since, updated_at and health on the first stop; everything on the
replay) and every snapshot table.

Class: effect-injection sensitivity control (decision 10), one narrow
forbidden effect per named stopped-route seam. The stopped settlement does not
exist at a233 (`git show a233883de3c36bae2e940f999cd9d582889dad00:api/internal/store/queries/codex_switch_stopped.sql`
fails), and the traced route has no write guard to remove (source census
above). Each mutant edits only the executing generated query
api/internal/store/codex_switch_stopped.sql.go: three add one column to the
`changed` CTE's SET list (:45–49), four add one sibling data-modifying CTE
after it;
the declarative queries/codex_switch_stopped.sql is not executed by the test.
Every eligibility conjunct and the six run assignments stay as they are.

Command: `.uzi/scratch/u8tools/ctl.sh` (export of 0434cd750, throwaway postgres
`cdr-4202-u17pg`), from the export's api directory,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m ./internal/handler -run '^TestWorkerCodexStoppedStatePreservationRouteLiveDB$' -v`.
Diffs and logs: `.uzi/scratch/ctl-logs/u17-*.{diff,log}`; each mutant log ends
`SHA=0434cd750… EXIT=1`. :33 is the first-stop `reportStoppedRoute`, :34 the
replay.

| Control | Seam | Injected effect | Red |
| --- | --- | --- | --- |
| u17-session | first stop, session | `changed` also sets `session_id = NULL` | :33 "first stop changed unrelated run fields" |
| u17-gate-run | first stop, canonical gate on the run | `changed` also sets `gate_presentation_id = NULL` | :33 (same message) |
| u17-question-id | first stop, question identity | `changed` also sets `open_question_id = NULL` | :33 (same message) |
| u17-gate-presentation | first stop, gate presentation row | added CTE deletes `run_gate_presentations` for the changed run | :33 "snapshot changed in presentations" |
| u17-question-message | first stop, question content | added CTE deletes the run's `kind='question'` messages for the changed run | :33 "snapshot changed in messages" |
| u17-replay-run | replay, run row | added CTE updates `updated_at` on the eligible already-paused row | :34 "snapshot changed in run" |
| u17-replay-presentation | replay, gate presentation row | added CTE deletes `run_gate_presentations` for the eligible already-paused row | :34 "snapshot changed in presentations" |

Unchanged candidate 0434cd750: every `^TestWorkerCodexStopped` handler test
passes with the extended snapshot (132 PASS lines, no FAIL or SKIP,
`ok … 54.384s`), `SHA=0434cd750… EXIT=0` (`u17-green.log`); `task lint:api`
0 issues (`u17-lint-api.log`). Runtime: the new test 0.39 s in-package
(same worker, warm cache, migrated schema, one serial run); the extra
snapshot subquery's cost across the other stopped tests was not separately
measured (no before run of the same selection at the parent); hosted effect
unmeasured.

Credit: the first stop is shown sensitive to loss (NULLing or deletion) of the
session id, the run's gate presentation id, the gate presentation row, the
open question id and the question message; the replay to a run-row write
(shown on `updated_at`) and to gate presentation deletion. The session id was
already populated by the route fixture (credential_switch_apply_routes_livedb_test.go:90–91)
and compared by the existing fresh_and_replay leaf; U17 adds the injection
control, not first coverage.

Review rework 372aa398a: the `presentations` key broke three consumers of the
shared snapshot that count its sections (codex_switch_apply_policy_routes_livedb_test.go
:91 and :202, codex_switch_owner_policy_routes_livedb_test.go:92) and
`codex_switch_apply_refresh_routes_livedb_test.go`:174, none of which the
`^TestWorkerCodexStopped` green selection ran. Their section lists now include
it. Every test that calls `stoppedRouteSnapshot` (all `^TestWorkerCodexStopped`
tests plus the policy-episode, apply-policy, apply-refresh, owner-policy,
owner-admission and owner-request route tests) passes at 372aa398a: 19 tests,
164 PASS lines, no FAIL or SKIP, `SHA=372aa398a… EXIT=0`
(`u17r-green.log`); `task lint:api` 0 issues (`u17r-lint-api.log`).

Not credited: rewriting a value (every mutant NULLs or deletes); a replay
write that stores an unchanged value; the first stop's own `status_since` and
`updated_at` values, which the first-stop comparison drops and does not pin;
replay loss of the session id, question id or question message (no replay
injection on them); the other gate columns (`gate_revision`, `gate_presented_payload`,
`gate_payload_digest`) and other session/question carriers are compared by
the same whole-row snapshot but were not individually injected; replay
injections cover the run row and presentations only (inputs, journals, custody
and messages on replay were not injected, though the snapshot compares them);
Slack presentation anchors (`slack_run_messages`) and other trigger-closure
tables named in the census are outside the snapshot; this is sensitivity of
the assertion, not proof that no other effect exists. Fixture-shape note: the
gate and question state is written directly on a running run, which
production reaches only through the gate and question writers; it is not
internally coherent (revision 2 with no revision-1 row, a one-byte digest,
an open question outside awaiting_input), which the stop predicate does not
read; awaiting_approval, awaiting_input and awaiting_followup origins, whose
trigger closure differs, are not exercised. The most security-relevant state outside the
snapshot is `cross_checks`: on running → paused the
`settle_exited_plan_cross_check` / `settle_exited_code_cross_check`
triggers (00432) stamp interruption, fail a pending verdict, cancel the
checker child and bank wait time into the lead's `budget_paused_seconds`;
this fixture seeds no cross check, so that path is not exercised (the run-row
banking would be caught by the run compare, the `cross_checks` rewrite and the checker
child's cancellation, a different run row, would not). `u17-first.log` (no SHA line) is the uncommitted first run, not
evidence.

### U18 — bounded strict stopped request, forbidden combinations, op_id and minimal acknowledgement (2026-11-18)

Candidate 3b78045f6dc458906a63fd48e9b5c5e8ca8a901f adds
api/internal/handler/codex_switch_stopped_request_livedb_test.go
(`TestWorkerCodexStoppedRequestShapeRouteLiveDB`). Every leaf posts a raw body
to the authenticated `/state` route of `setupStoppedGuardsRoute` after
withdrawal. Refused leaves (body_over_bound, trailing_value, op_id_present,
unknown_field, expected_epoch_on_running) require 400 and an unchanged
snapshot (:52). body_at_bound (a body of exactly 1 MiB) requires acceptance
(:67). minimal_ack_large_plan sets a 64 KiB `plan_md` and requires the
`reportStoppedRoute` acceptance: a three-key envelope of at most 16 KiB whose
run object has exactly id, status, claim_generation and hold_reason (:74).

a233 reference: `worker_state_decode.go` and the stopped settlement do not
exist at a233883de3c36bae2e940f999cd9d582889dad00, and a233's service.go has
no `expected_epoch requires codex_switch_stopped` refusal, so the decoder,
service and acknowledgement controls are class (d). The shared strict decoder
`httpx.DecodeJSONStrictBounded` is byte-identical at a233 (respond.go :133,
:137, :142), but a233's `WorkerRunState` decoded with `httpx.DecodeJSON`
(a233 worker_protocol.go:1482), which truncates at 1 MiB and has no
trailing-value check; the route adopted the strict decoder only with the
stopped settlement (abb3c7835). The bound and single-value refusals are
therefore new on this route (class (d), not (b)). Unknown-field refusal did
exist at a233 (`DecodeJSON` runs `DisallowUnknownFields`, a233 respond.go:112);
its u18-unknown control is class (d) because the stopped status itself is
absent at a233 (see "Baseline absence and the class (d) method boundary").
The httpx mutants edit the shared helper, which `DecodeJSONStrict` and 37
other non-test call sites in 24 handler files also use; on this route their effect is
confined to the decodes in `decodeWorkerState`.

Copy census (the route: `WorkerRunState` worker_protocol.go:1541 →
`decodeWorkerState` → `SetStateReportWithReconciliation` → `setStateWithTx`
→ `stopCodexSwitch`; the route is mounted through `mountWorkerRoutes` on both
the main router (handler.go:1181) and `WorkerRoutes` (:1210–1222; Recoverer
and RequestID), and `mountWorkerRoutes` itself applies RequireWorker and the
lane guard (:1238 onwards); none adds a body bound or strictness layer):
- 1 MiB body bound: the first `DecodeJSONStrictBounded(r, &raw, 1<<20)`
  (worker_state_decode.go:19). The second decode (:49) re-reads the captured
  value; it is a backup bound only for oversize bytes inside the value
  (`json.RawMessage` keeps inner whitespace but drops trailing whitespace),
  and the body_over_bound leaf pads with trailing spaces, so it exercises the
  first bound alone;
- inclusive edge: respond.go:133 (`len(body) > limit`);
- one JSON value: respond.go:142 in the first decode;
- op_id: decoder `len(fields) != 4` (:31) and service `req.OpID != nil`
  (codex_switch_stopped.go:24), two copies;
- unknown field: decoder `len(fields) != 4` and `DisallowUnknownFields`
  (respond.go:137) in the second decode, two copies;
- expected_epoch on another status: decoder `EqualFold` loop (:41–44) and
  service.go:4700–4701, two copies;
- minimal acknowledgement: `stoppedSettlementAck` (worker_protocol.go:1786,
  :2828) only.

Command: `.uzi/scratch/u8tools/ctl.sh` (export of 3b78045f6, throwaway postgres
`cdr-4202-u18pg`), from the export's api directory,
`GOFLAGS='-p=1 -buildvcs=false' GOMAXPROCS=4 go test -race -count=1 -timeout=6m ./internal/handler -run '^TestWorkerCodexStoppedRequestShapeRouteLiveDB$/^<leaf>$' -v`.
Diffs and logs: `.uzi/scratch/ctl-logs/u18-*.{diff,log}`; each mutant log ends
`SHA=3b78045f6… EXIT=1`, except u18-ack-extra-key, run at daeb23b8b (a
ledger-only child of 3b78045f6).

| Control | Class | Mutation | Leaf | Red |
| --- | --- | --- | --- | --- |
| u18-body-bound | (d) | first decode limit `1<<20` → `1<<21` | body_over_bound | :52 "malformed stopped request status=200; want 400" |
| u18-body-edge | (d), shared helper | respond.go `len(body) > limit` → `>= limit` | body_at_bound | :67 "status=400; want accepted" |
| u18-trailing | (d), shared helper | respond.go trailing check → `false && …` | trailing_value | :52 "status=200; want 400" |
| u18-op-id | redundant-fence (2 copies) | decoder `len(fields) != 4` → `< 4`; service `\|\| req.OpID != nil` removed | op_id_present | :52 "status=200; want 400" |
| u18-unknown | redundant-fence (2 copies; the httpx copy in the shared helper) | decoder `len(fields) != 4` → `< 4`; `dec.DisallowUnknownFields()` removed from the strict-bounded decoder | unknown_field | :52 "status=200; want 400" |
| u18-epoch-other | redundant-fence (2 copies) | decoder EqualFold check → `false && …`; service `req.ExpectedEpoch != nil` → `false && …` | expected_epoch_on_running | :52 "status=200; want 400" |
| u18-minimal-ack | (d) | `ack = stoppedSettlementAck(run)` → the default ack with `disposition` set | minimal_ack_large_plan | :74 "stop acceptance mismatch: status=200 applied=false" |
| u18-ack-full-run | (d) | the minimal ack kept but its `run` replaced by the full run DTO | minimal_ack_large_plan | :74 "stopped acknowledgement is not a bounded minimal envelope" (the envelope keeps three keys, so the 16 KiB size check fired) |
| u18-ack-extra-key | (d) | the ack's run object gains a fifth key (`kind`) | minimal_ack_large_plan | :74 "stopped acknowledgement does not identify the committed paused run" (run-object key count) |

Unchanged candidate 3b78045f6: the new route test and the existing
`TestWorkerStateStoppedStrictBody`, `TestStoppedSettlementAck` and
`TestWorkerRunStateCodexSwitchStoppedRoute` pass (17 PASS lines, no FAIL or
SKIP), `SHA=3b78045f6… EXIT=0` (`u18-green.log`); `task lint:api` 0 issues
(`u18-lint-api.log`). Runtime: 4.31 s for the new test in that run (a LiveDB test,
skipped when `UZI_TEST_DATABASE_URL` is unset; same
worker, warm cache, migrated schema, one serial run; `u18-first.log` has no
SHA line and includes migration). Hosted effect unmeasured.

Credit: the stopped request is bounded at 1 MiB inclusive (one copy), must be
one JSON value (one copy), refuses `op_id` and unknown fields (two copies
each), and `expected_epoch` is refused on a `running` report (two copies);
the acceptance answers with the stopped acknowledgement (u18-minimal-ack shows
only that the stopped builder supplies `applied`; the mutant sets
`disposition` itself, so no U18 route mutant covers it; the builder unit
test `TestStoppedSettlementAck` asserts it and the three-key envelope), whose run
object has exactly four keys (u18-ack-extra-key) and which stays under 16 KiB
with a 64 KiB plan where the full DTO would not (u18-ack-full-run). The
three-key envelope count itself is not reached by any mutant.

Not credited: the redundant copies individually; `expected_epoch` on
statuses other than `running` (the decoder loop is generic, but statuses that
leave `SetStateReportWithReconciliation` early through the replay paths,
reconciliation.go:171/:177, rely on the decoder copy alone); no-write for a
`running` report that carries both `registration_evidence` and
`expected_epoch` (`validateRegistrationEvidence` admits evidence only on
running reports, and `recordRegistrationEvidence`, reconciliation.go:184,
writes before the service copy at service.go:4700, so only the decoder copy
keeps that shape write-free; the leaf sends no evidence);
the exact limit within `[1<<20, 1<<20 + len(valid))` (a limit raised by fewer
bytes than the JSON length stays green); the overlap with the decoder unit
test's own at-bound case; `op_id: null`, missing or
null authority fields and case aliases beyond U15/D25–D56 (the existing
decoder unit test covers them without a control); the second strict decode's
own bound and trailing checks (not independent fences); `expected_epoch`
case variants (EqualFold) on other statuses; the 16 KiB acknowledgement
bound under other large fields.

### U19 — token absence: B material redaction and stopped-route surfaces (2026-11-18)

Candidate 8a3df9d63612fcec13207d254e3f352429b9b8b3:
- agent/test/codex-executor.test.ts, the six "M2 actualRunner … account
  resumes saved thread" baseline leaves (subscription → API key, API key →
  subscription, subscription → subscription on another account; factory and
  already-running-body), now assert after the real Runner switch that B's
  access token and capability are registered with the log redactor (a
  recording `addSecret` on the Runner's logger) and with the flight's
  run-message redactor (`flight.secrets` holds them and `flight.redactText`
  scrubs them);
- api/internal/handler/codex_switch_stopped_token_livedb_test.go
  (`TestWorkerCodexStoppedTokenAbsenceRouteLiveDB`) opens the run's bound
  fixture secret with the handler's box, captures slog, and requires that the
  first stop and its replay through the authenticated `/state` route leave the
  secret out of both acknowledgements, every run message, every user input
  body and the server log.

a233 reference: a233883de3c36bae2e940f999cd9d582889dad00 has no Codex switch
(`git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts`
has no `bundle.binding.accessToken, bundle.binding.capability` loop) and no
stopped settlement, so the redaction-registration controls are class (d)
and the stopped-route controls are effect-injection (decision 10): the
stopped route reads no material, so there is no redaction guard to remove.

Seams. The registration guard is runner.ts:17657–17659
(`for (const token of [bundle.binding.accessToken, bundle.binding.capability]) {
flight.secrets.add(token); this.log.addSecret(token); flight.runScopedSecrets.push(token); }`),
run after the committed apply and before B is published. The stopped route's
token-bearing sinks are its acknowledgement (worker_protocol.go:1786,
`stoppedSettlementAck`) and the server log.

Commands: agent controls by `.uzi/scratch/u8tools/actl.sh` (export,
`npm run typecheck`, then
`node --import tsx --test --test-timeout=120000 --test-name-pattern='M2 actualRunner .* account resumes saved thread' test/codex-executor.test.ts`);
handler controls by `.uzi/scratch/u8tools/ctl.sh` (throwaway postgres
`cdr-4202-u19pg`, `-run '^TestWorkerCodexStoppedTokenAbsenceRouteLiveDB$'`).
Diffs and logs `.uzi/scratch/ctl-logs/u19-*.{diff,log}`; each mutant log ends
`SHA=8a3df9d63… EXIT=1`.

| Control | Class | Mutation | Red |
| --- | --- | --- | --- |
| u19-agent-log-redactor | (d) | drop `this.log.addSecret(token);` from the loop | all six leaves, codex-executor.test.ts:35293 "Codex switch registers B material with the log redactor" |
| u19-agent-message-redactor | (d) | drop `flight.secrets.add(token);` from the loop | all six leaves, :35294 "Codex switch redacts B material from run messages" |
| u19-ack-leak | effect-injection | the stopped branch opens the bound secret (`GetUserSecretCiphertextByID`, `h.box.Open`) and adds it to the acknowledgement | codex_switch_stopped_token_livedb_test.go:38 "first stop acknowledgement carries the bound secret" |
| u19-log-leak | effect-injection | the stopped branch opens the bound secret and logs it with `slog.Info` | :51 "stopped settlement logged the bound secret" |

Unchanged candidate 8a3df9d63: the six agent leaves pass (`ℹ pass 6`,
`u19-green-a.log`), the stopped token test passes (`u19-green-h.log`), both
`SHA=8a3df9d63… EXIT=0`; `task lint:api` 0 issues (`u19-lint-api.log`);
`npm run lint` in agent 0 warnings and 0 errors (`u19-lint-agent.log`).
Runtime: the agent assertions add no wait to existing leaves (the six take
about 12 s together, as before, unmeasured as a delta); the new handler test
is about 1 s in-package after migration (LiveDB, skipped without
`UZI_TEST_DATABASE_URL`). Hosted effect unmeasured.

Review rework c24bfa7fa71fccc439735e1cc62218a8a6af6ee8:
- api/internal/handler/codex_switch_apply_log_livedb_test.go
  (`TestWorkerCodexApplyLogRedactionRouteLiveDB`) performs a real Codex apply
  through the authenticated apply route with slog set to the production JSON
  handler (cmd/server/main.go:170), requires the applied response to carry an
  access token and capability (length only; the test does not compare them
  with the target's plaintext), and requires the whole captured log, including
  the "worker credential switch apply" line (worker_protocol.go:1864 logs
  `"result", res`), to carry neither. The guard is `CredentialSwitchApplyResult.LogValue`
  (workersvc/credential_switch_apply.go:136); without it the JSON handler
  marshals the wire shape (:157–170), codex material included. `LogValue`
  exists at a233 (credential_switch_apply.go:134) but the result had no Codex
  field there, so the Codex exclusion is new: class (d).
- The stopped token test now checks both fixture secrets (the bound A and the
  withdrawn target) and matches with `strpos` rather than `LIKE`.

| Control | Class | Mutation | Red |
| --- | --- | --- | --- |
| u19r-log-value | (d) | `LogValue` renamed to `logValue` (and `String` follows), so the result is no longer a `slog.LogValuer` | codex_switch_apply_log_livedb_test.go:40 "Codex apply logged B material" |
| u19r-ack-leak | effect-injection | as u19-ack-leak, rerun on the extended test | codex_switch_stopped_token_livedb_test.go:50 |
| u19r-log-leak | effect-injection | as u19-log-leak, rerun | :65 |

Each u19r mutant log ends `SHA=c24bfa7fa… EXIT=1`. Unchanged c24bfa7fa: both
handler tests pass (`u19r-green-h.log`), the six agent leaves pass
(`u19r-green-a.log`), both EXIT=0; `task lint:api` 0 issues and agent
`npm run lint` 0 warnings and 0 errors (`u19r-lint-*.log`).

Credit: after a real Runner Codex switch, B's access token and capability are
registered with the Runner's log redactor and its run-message redactor (each
registration controlled separately in the test wiring). In production the
Runner and the Codex executor share one logger (agent/src/main.ts:763–764,
:841), and the executor's `publishB` registers B with it again
(codex/codex-executor.ts:3210–3214, :4139); the test gives the executor a
no-op logger, so u19-agent-log-redactor isolates the Runner's copy, and the
executor's copy is not controlled. `flight.runScopedSecrets.push` (runner.ts:17658;
the terminal eviction it feeds is at runner.ts:7615) is retention hygiene and
not controlled. The Codex
apply route's server log stays free of B material through `LogValue` ((d)).
The stopped settlement keeps the bound secret out of its acknowledgement and
the server log (effect-injection; both mutants inject only the bound secret);
the withdrawn target's absence there, and both secrets' absence from run
messages and inputs, are asserted only. The test's failure messages say
"bound secret" for either secret.

Not credited (no qualifying class; for the owner batch): every other surface
keeps the token out by construction, with no redaction or omission guard to
remove — the server apply/preflight responses' non-token fields, the Codex
switch feed message and journals (`TestCodexTransactionalApplyBundleLiveDB`
asserts them), the apply route's error log (its errors wrap no material;
the comment at worker_protocol.go:1857–1858 is inaccurate for Codex, where
B is decrypted before later apply errors, but its conclusion holds), the
stopped route's run row and journals (compared by the U14–U17 snapshot,
not checked for material), the worker client's fixed
detail strings (`client-credential-switch-apply.test.ts` asserts them), the
owner set-token input (it carries a secret id, never material), and message
and input injection on the stopped route (asserted, not injected). The
redactors' own matching (`SecretSet`, the log scrubber) is outside this
seam. m4 `scan:secrets` and `sast:semgrep` remain required.

### U20 — publication ordering and admission gates (2026-11-18)

Real Runner leaves in agent/test/codex-executor.test.ts ("M2 actualRunner
credential replacement"):
- the two "… publication gates Now, retired A and B preparation children;
  finished B root dispatch admits work" leaves;
- the eleven "… partial B publication never reopens A (<stage>)" leaves, one
  injected failure at each publication stage;
- "Codex subscription startup refresh stays deferred until B finish";
- the six baseline leaves.

At every publication barrier (scope clear/install, executor publication, Now
publication, B preparation, login, prepared) they already try Now, the
retired A refresh bridge, the retained A root dispatch, a retired-broker
child, the newly installed B context dispatcher before finishB, and a
B-preparation child.

Final candidate cd09701961ae0d38d9ef12c047ce5ceeacd3b737. Its test changes
over 8a0d54655 (21327f701, b0ddd1c14) move two assertions out of the
production-called `finishB` hook into the final block, where a failure
reaches the test instead of only retaining the switch: "no ordinary refresh
before finishB" and "startup refresh remains deferred through full B
preparation" (now recorded in the hook and asserted at the end). An interval
probe added at c85f434e7 was dropped at cd0970196. It could not observe the
gate (see below), and it advanced the whole Runner clock (checkpoint time
gate, step durations).

a233 reference: a233883de3c36bae2e940f999cd9d582889dad00's
agent/src/codex/codex-executor.ts has neither `switchAdmissionClosed` nor
`suspendRefreshAdmission` (`grep -c` is 0), so a credited control is class (d).

Command: `.uzi/scratch/u8tools/actl.sh` (export, `npm run typecheck`, then
`node --import tsx --test --test-timeout=120000 --test-name-pattern='publication gates Now|startup refresh stays deferred until B finish|partial B publication never reopens A' test/codex-executor.test.ts`,
18 leaves). Logs `.uzi/scratch/ctl-logs/u20*-*.{diff,log}`. `actl.sh`
overwrites a log of the same name, so only the latest run of each name is on
disk.

| Control | Class | Mutation | Result |
| --- | --- | --- | --- |
| u20f-exec-admission | (d), at cd0970196 (also u20-exec-admission at 8a0d54655 and u20r-exec-admission at 21327f701, same result) | executor admission check codex-executor.ts:2509 `if (this.switchAdmissionClosed && (!construction \|\| construction !== this.switchBuildAuthority)) throw` → `if (false && …)` | EXIT=1, 6 of 18 red, all at "B preparation child sends no ordinary request at login" (test :34424 at cd0970196, the log's stack frame for the `assert.equal`, whose message is on :34425; the same assertion is :34427 at HEAD d8717dd28 numbering and :34421 in the u20-exec-admission log at 8a0d54655): both gated leaves, three partial-publication leaves and the deferred-refresh leaf |
| u20r-b-refresh | not credited (timeout red), at b0ddd1c14 | B's refresh suspension (:4164) `replacementRefresh = fresh.harness.suspendRefreshAdmission();` → `replacementRefresh = undefined;` | EXIT=1, 1 of 18 red, but by "timed out waiting for M2 B turn"; the diagnostics show "credential switch unconfirmed; retaining work", so the switch fails closed downstream before either moved assertion runs |
| u20-exec-summary-switch (at 8a0d54655), u20-exec-summary-both (log at c85f434e7), u20-summary-all (at 8a0d54655) | diagnostic only (multi-fence where more than one line is removed) | `summaryAllowed` terms (:3302–3303) and the summary guard (:2979), with Now's `switchGate` for the last | EXIT=0, 18 pass: no plan or intent summary is started during the window (producers are aborted and joined at :4066–4074), so `summaryAllowed` is untested (nothing starts a plan or intent summary at the barriers), not masked; the shared factory guard (:2975–2981) is reached only by a Now build, which never begins, and the removed `switchGate` line is evaluated but masked by an undetermined layer, most likely the Runner lifecycle abort. Each combined mutant removes more than one fence, so these rows are diagnostic only |
| u20-now-gate (only surviving log is at c85f434e7; no log at 8a0d54655 survives, since `actl.sh` overwrites a log of the same name) | diagnostic only | Now's `!this.switchGate &&` (now-summary.ts:499) removed | EXIT=0, 18 pass |
| u20-now-gate-lifecycle, u20-now-switch-gates (at c85f434e7) | diagnostic only (multi-fence; timeout reds) | `switchGate` removed with Now's lifecycle-abort condition, or with that condition relaxed for a `CredentialSwitchSignal` abort | EXIT=1 by "timed out waiting for M2 B usage and ordinary cancellation"; no assertion fires |

R7 (above) is the Runner's B dispatcher publication-readiness guard for the
scope-replacement stage; it is unchanged here.

Unchanged candidate cd0970196: the 18 selected leaves plus the six baseline
leaves pass (`ℹ pass 24`, 30.3 s, `u20f-green.log`, `SHA=cd0970196… EXIT=0`);
agent `npm run lint` 0 warnings and 0 errors (`u20f-lint-agent.log`). The whole
"M2 actualRunner credential replacement" describe passes at 3fdfe9127 (a
ledger-only child of cd0970196): 88 tests, `ℹ pass 88`, 155.7 s
(`u20f-describe.log`, `SHA=3fdfe9127… EXIT=0`). Timing
is not a finding: the 24-leaf runs on disk took 30.3 s (`u20f-green`), 33.3 s
(`u20r-pre-commit`), 36.8 s (`u20-green`, c85f434e7), 39.1 s (`u20-first`) and
43.7 s (`u20r-first`), all on the same worker and node. The five 24-leaf runs
span 30.3–43.7 s, about ±6.7 s around their midpoint. The 18-leaf runs on disk
are `u20-green-pre` at 20.9 s and `u20-now-gate` at 29.3 s (different revisions
and one with a mutant, so not a noise estimate). The 24-leaf spread is
comparable to the reported delta, so no wait change is established or ruled
out. Hosted effect unmeasured.

Credit: during B preparation the executor's switch admission check refuses
child work except the replacement build itself ((d)). R7 covers the B
dispatcher's root readiness. The retired A root dispatch and retired-broker
child refusals rest on the retired-authority guards (U5).

Not credited:
- **Now advice.** Frames do reach Now (runner.ts:17015–17021 sends each frame
  to the batcher and to `nowSummary.observeFrame` directly). During the whole
  switch window, though, Now's gate also fails on
  `!d.steering.lifecycleSignal().aborted` (now-summary.ts:501): the switch
  aborts the lifecycle with `CredentialSwitchSignal` (steering.ts:2846), and
  the success path re-arms it only after Now's finishB (runner.ts:17712, then
  :17718). Behind that, `begin()` aborts a call when the lifecycle is aborted,
  and the executor's summary factory guard (`assertAuthority` /
  `assertSwitchAdmission`, codex-executor.ts:2943–2957, checked before the
  build at :3043) refuses before a build. On the success path and on
  failure before Now's finishB, `switchGate` is therefore masked by an
  undetermined layer, most likely the Runner lifecycle abort (the batcher is
  not the cause: frames reach Now directly, runner.ts:17015–17021), and the
  mutant did not redden alone. On the preserve-only confirmed
  resume-refusal path the Runner re-arms the lifecycle (runner.ts:17699–17700)
  after Now's publishB (:17684) without Now's finishB, so from there
  `switchGate` is the only Now-level gate until `nowSummary?.stop()` in
  `settleExecution` (runner.ts:18979), because nothing on that path clears it
  (Now's `finishB` lease opens it, now-summary.ts:354, and `reopenA` also calls
  `open()` at :361 but refuses once B is published); the executor's
  switch admission likely still refuses behind it. No selected leaf covers that path.
  Separating the copies needs a controller-level test
  (`NowSummaryController.suspendCodexSwitch` with an un-aborted lifecycle);
  none was added.
- **Plan and intent summaries** are untested in the window (nothing starts one at the barriers); the `summaryAllowed` mutants are therefore not masked but unexercised. Superseded for behaviour by "U20 plan and intent summaries (2026-11-21, #4573)" below.
- **B's refresh suspension until finishB.** Its removal is caught by a
  downstream fail-closed retain, not by the deferred-refresh assertion.

### U20 plan and intent summaries (2026-11-21, #4573)

Real Runner leaves in agent/test/codex-executor.test.ts, in the "M2 actualRunner
credential replacement" describe, one set per publication name. The gated
variants run in both U20 gated directions; the preserve-only variant runs once
(api_key to subscription). Test names contain "plan summary" or "intent summary":
- plan summary: gated-plan-summary, gated-plan-summary-late,
  gated-plan-summary-unacked;
- intent summary: gated-intent-summary, gated-intent-summary-retained,
  gated-intent-summary-late, and the preserve-only variant
  resume-refuse-preserve-intent-summary.

Fixture mechanisms:
- The executor's own summary factory is used (`makeExecutor` home option), not a
  stub.
- An event-driven abort-then-release barrier runs after the sibling drain
  operands plus one `setImmediate`.
- Late result: a wrapper at the `summaryRunner` seam returns a synthesized
  real-shaped result, because the model pass turns a post-abort completion into
  null.
- Retained uninvoked A plan callback: kept across the switch by an unapplied ACK.
- Retained unlaunched A intent ownership: the A intent build keeps its ownership
  and settles at the abort.
- Follow-on (I4): a claim reaches a follow-on execute of the same run through the
  same executor.

Tested revisions: product fix 5288bdce6, final tests c1b399b07, base 8be78a548
(intermediate test commits a43bfe06b and 28222928c). Mutant revisions: plan
mutants (M1-M8 and the P2 D rows) ran at a43bfe06b; intent J, F and G rows at
28222928c; the I4 K1, K2 and K3' rows at c1b399b07. The 'plan summary' and
'intent summary' 11/11 baselines include older matching leaves (plan: 6 new + 5
older; intent: 7 new + 4 older).

Commands. Focused: from `agent/`, `node --import tsx --test --test-timeout=120000
--test-name-pattern='publication gates Now|plan summary|intent summary' test/codex-executor.test.ts`
(24 leaves: 13 #4573 leaves, the 2 'publication gates Now' leaves and 9 older
inline-summary leaves that the pattern also matches); the 'plan summary' and 'intent summary' sets also run alone with
their pattern. Mutation: apply one edit in a scratch export of the exact
candidate and run the single-file form above; nothing is mutated in the checkout.
Gates: `npm run check:agent`, the full test/codex-executor.test.ts file,
`test:codex-m4` (at 5288bdce6), `git diff --check`.

Base-red record (P3). Production at 8be78a548 with the new tests:
`.uzi/scratch/gate-log.W3wMAl`, 9 pass and 2 fail, both uninvoked-A-plan-callback
leaves, at "retired A plan callback never builds or posts with B": actual
`{inputs:1,builds:2,posts:1}`, expected `{inputs:0,builds:1,posts:0}`. Taken
before the product edit: the retained, uninvoked A plan callback built and posted
a summary with B after finishB.

Fix (5288bdce6): `beginPlanSummary` captures the admitting authority
(`admittedAuthority`) and `authorityCurrent()` refuses when it is no longer
current, in the callback entry check and in the in-flight `allowed()`. Identity
only: `summaryInputs` is unchanged. The changelog fragment is in this
PR (fragment id 29b991fd).

Per-leaf results. Label: R = regression (red on base), C = characterization
(passes on base behaviour or has no base-red record). "Individual fence" means
the one removed line alone reddens a named assertion.

| Leaf | Label | Behaviour tested/pass | Individual fence mutation-proven | Mutant (edit) | Named red | Log |
| --- | --- | --- | --- | --- | --- | --- |
| P1a plan join | C | pass | yes | M1: drop flights from the `suspendEpoch` allSettled | "A plan summary joins before suspension completes" | `.uzi/scratch/gate-log.hpxqYc` |
| P1b plan post check | C | pass | yes | M2: delete `allowed()` before `postPlanSummary` | "late A plan result never posts as B" (1 vs 0) | `.uzi/scratch/gate-log.VhJTUx` |
| P1b plan abort observation | C | pass | yes (observed by the reviewer; no retained log) | reviewer mutant B: `signal: undefined` to `generatePlanSummary` | "late A plan result resolves only after the switch aborted it" (only failure in the late leaves) | reviewer-1 export run at a43bfe06b; log not kept |
| P1b plan late leaves, join | C | pass | yes | M1 | "the suspension is still pending when A plan work is released" | `.uzi/scratch/gate-log.hpxqYc` |
| P2 plan pending-switch refusal | C | pass | NO (uncredited) | see matrix | see matrix | see matrix |
| P3 retired A callback | R | pass | yes | M3: `authorityCurrent` replaced by `=> true` | "retired A plan callback never builds or posts with B" | `.uzi/scratch/gate-log.zZxuib` (base red W3wMAl) |
| P4 binding | C | pass | yes | M4: `summaryInputs` `admitted.claim` replaced by `authority.claim` | "B plan summary build is admitted with B's credential" (0 vs 1) | `.uzi/scratch/gate-log.xL2MsF` |
| P4 attribution | C | pass | yes | M5: bypass of the `summary_usage` ordinal stamp (runner.ts) | "B plan summary usage is delivered with B's credential_epoch_ordinal" (undefined vs 1) | `.uzi/scratch/gate-log.5ZcVEd` |
| I1a intent join | C | pass | yes | J1: drop the join | "A intent summary joins before suspension completes" | `.uzi/scratch/gate-log.7gJ7V6` |
| I1b intent join | C | pass | yes | J1 | "late A intent result settles before B publication" | `.uzi/scratch/gate-log.7gJ7V6` |
| I1b intent abort | C | pass | yes | J2: `signal: undefined` | "late A intent result resolves only after the switch aborted it" (other leaves derail on teardown timeouts) | `.uzi/scratch/gate-log.10x27l` |
| I1b intent post isolation | C | pass | NO (uncredited) | F1-F4, see matrix | green alone | see matrix |
| I2 intent retained/launch guard, incl. preserve-only | C | pass | NO (uncredited) | G1-G5, see matrix | green alone | see matrix |
| I3 retained ownership | C | pass | NO (uncredited) | see guard matrix | see guard matrix | see matrix |
| I4 post | C | pass | yes | K1: skip the post | "exactly one B intent summary post" | `.uzi/scratch/gate-log.ds6IsD` |
| I4 credential | C | pass | yes | K2: `run()` falls back to `opts.binding` | "B intent summary uses B's credential" | `.uzi/scratch/gate-log.9gnbr7` |
| I4 follow-on baseline ordinal | C | pass | yes | K3': stamp ordinal 1 when 0 (runner.ts epochEmitter) | "follow-on intent summary usage carries the follow-on flight's baseline (no ordinal)", also the late leaves' "A intent usage carries no epoch ordinal" | `.uzi/scratch/gate-log.kGKbz9` |

Baselines: plan 11/11 (`.uzi/scratch/gate-log.SbWMZY`), intent 11/11 (`.uzi/scratch/gate-log.2VPhcS`),
I4 at c1b399b07 (`.uzi/scratch/gate-log.24Um5g`). Under M5 the existing
'publication gates Now' leaves stay green at 5288bdce6 (`.uzi/scratch/gate-log.pnKNgh`).
M1 at 5288bdce6 timed out the P1a join leaf (`.uzi/scratch/gate-log.zOBwCY`),
which led to the rework at a43bfe06b.

The individual-fence proofs below are UNCREDITED. Combined removals (D-rows) are
diagnostic only because they remove more than one fence. I1b post isolation is
outside the owner decision of 2026-11-21 and is reported as an unmet clause.

P2 fence matrix (plan summary refused while a switch is pending; production
fences `authorityCurrent`, `switchAdmissionClosed`, `summaryLifecycleClosed`, the
first `assertCurrent`):

| Row | Edit | Result | Log |
| --- | --- | --- | --- |
| M6 | remove `switchAdmissionClosed` | green (masked) | `.uzi/scratch/gate-log.5WdlmY` |
| M7 | remove `summaryLifecycleClosed` | green (masked) | `.uzi/scratch/gate-log.g1Bvoj` |
| M8 | remove the first `assertCurrent` | green (masked) | `.uzi/scratch/gate-log.WCKGVN` |
| D1 | M6 + M3 | red only at callback-after-B | `.uzi/scratch/gate-log.eWjrh5` |
| D2 | M7 + M3 | same as D1 | `.uzi/scratch/gate-log.QE9OHl` |
| D3 | M6 + M7 | green | `.uzi/scratch/gate-log.D0Iky3` |
| D4 | M6 + M7 + M3 + M8 | red P2 at after-publish (inputs 1) | `.uzi/scratch/gate-log.YKDX73` |
| D5 | M8 + M3 | same as M3 | `.uzi/scratch/gate-log.rPj8mU` |
| D6 | M6 + M7 + M3 | red at after-publish | `.uzi/scratch/gate-log.YJLX70` |

After-publish, three fences guard the stage (`authorityCurrent`,
`switchAdmissionClosed`, `summaryLifecycleClosed`); P2 reddens only when all three
are removed, any two suffice. The before-publish masker is unidentified (not M3,
M6, M7 or M8). Later stages are unobserved under D4 and D6 because the loop stops
at the first failure.

I1b post-isolation matrix (late A intent result must not post as B; fences F1
`aborted`, F2 `summaryAllowed`, F3 `abandoned`, F4 `intentFlight === flight`):

| Row | Edit | Result | Log |
| --- | --- | --- | --- |
| F1 | remove `aborted` | green | `.uzi/scratch/gate-log.u7BHqv` |
| F2 | remove `summaryAllowed` | green | `.uzi/scratch/gate-log.cH1WVf` |
| F3 | remove `abandoned` | green | `.uzi/scratch/gate-log.zk15bz` |
| F4 | remove `intentFlight === flight` | green | `.uzi/scratch/gate-log.WJP1Hb` |
| DIAG-F | F1 + F2 | red "late A intent result never posts as B" | `.uzi/scratch/gate-log.rACzqu` |
| F34 | F3 + F4 | green | `.uzi/scratch/gate-log.638IAy` |
| F123, F124, F1234 | three or four removed | red | `.uzi/scratch/gate-log.iNohG4`, `.uzi/scratch/gate-log.YkMi1r`, `.uzi/scratch/gate-log.zFYOjt` |

F1 and F2 mask each other; F3 and F4 are masked by F1/F2.

Guard matrix (retained unlaunched A intent ownership; launch/summary guard
pieces G1 `assertSwitchAdmission`, G2 `assertCurrent`, G3 identity check, G4
`summaryLifecycleClosed`, G5 signal `throwIfAborted`):

| Row | Edit | Result | Log |
| --- | --- | --- | --- |
| G1 | remove | green | `.uzi/scratch/gate-log.85sDsx` |
| G2 | remove | green | `.uzi/scratch/gate-log.R1vsFM` |
| G3 | remove | green | `.uzi/scratch/gate-log.V4Zaq3` |
| G4 | remove | green | `.uzi/scratch/gate-log.CHyf1A` |
| G5 | remove | derails the Now drain with timeouts (the factory is shared with Now), no credit | `.uzi/scratch/gate-log.dDQ7Mh` |
| D1 | substitute keeping G5 | red "retired A intent launch never starts at before-publish" and "retired A summary guard refuses with TrustedExecutionRefusal" | `.uzi/scratch/gate-log.6NyRWy` |
| D2 | remove the `assertAuthority` call | red at the guard refusal | `.uzi/scratch/gate-log.Sj9L6Z` |
| D4 | G1 + G2 + G3 | red | `.uzi/scratch/gate-log.Fjqb43` |
| D5 | G1 + G2 + G3 + G4 | red, at the same assertions as D1 | `.uzi/scratch/gate-log.9SdHTs` |
| D6 | G1 + G2 + G4 | red at "never starts at before-publish" | `.uzi/scratch/gate-log.EgoI6z` |
| D7 | G1 + G4 | green | `.uzi/scratch/gate-log.8cWB2y` |
| D8 | G2 + G4 | green | `.uzi/scratch/gate-log.JlQ4J3` |
| D9 | G3 + G4 | green | `.uzi/scratch/gate-log.oK9V3O` |
| D10 | G2 + G3 + G4 | red | `.uzi/scratch/gate-log.y91wgW` |
| D11 | G1 + G3 + G4 (G2 only left) | green | `.uzi/scratch/gate-log.xEInIr` |

G2 alone suffices; G1 and G3 are masked by G2. G5 derails the Now path, so it
earns no credit. Recorded refusal messages: before-publish "codex credential
authority retired"; later stages "summary credential authority changed". The
preserve-only refusal is "codex epoch admission closed for capture"
(`assertCaptureAdmission`, reviewer instrumentation). The "preserve-only
re-armed lifecycle..." and "never acquires B after finishB" test assertions
were not reached by any diagnostic (uncredited).

Rejected or derailing mutants:
- M5 original (dropping the emitter swap) throws; replaced by the
  `summary_usage` bypass above.
- K3 original (`?? 1` at runner.ts:17266) derails the A turn
  (`.uzi/scratch/gate-log.5c4qvi`); replaced by K3'.
- First-round P1a timeout (`.uzi/scratch/gate-log.zOBwCY`, at 5288bdce6) and
  first-round K1/K2 timeouts (`.uzi/scratch/gate-log.u88o4c`,
  `.uzi/scratch/gate-log.3dlSTY`, at 28222928c): fixtures then reworked
  (a43bfe06b, c1b399b07).

Attribution:
- Delivered `credential_epoch_ordinal` 1 on B's plan `summary_usage` (P4); absent
  on A usage (P1b, I1b).
- Follow-on intent usage is unstamped. The follow-on flight's emitter ordinal is 0
  (runner.ts `epochEmitter`; `flight.codexAuthority` is written only on a switch)
  while the reused executor still holds ordinal 1. Recorded as an open
  observation: I4 B-ordinal attribution is UNCREDITED.
- REST post bodies carry no epoch field (client.ts `postPlanSummary`,
  `postIntentSummary`): post-body attribution is UNCREDITED.

Repair pass (459a299f3, 187208c46, after the cross-check):
- Stage coverage. The P2 and I2 retained-consumer probes now also run at the
  scope-assignment stages (before/after scope clear, and before/after scope
  install when B is subscription) and at the Now publication stages
  (before-Now-publish, after-Now-publish), besides executor publication
  (before-publish, after-publish) and B preparation (before-prepare, login,
  prepared). Observed stage lists: plan-unacked: scope stages, before-publish,
  after-publish, before-Now-publish, after-Now-publish, before-prepare, login,
  prepared; gated and retained intent: the same plus after-publishB and
  after-finishB; preserve-only intent: scope stages, before-publish,
  after-publish, before-Now-publish, after-Now-publish, before-prepare, login,
  resume, policy-hold. Results are asserted at top level with the P2/I2
  messages. The D6 diagnostic (both `summaryAllowed` flags removed and
  `authorityCurrent` forced true) was run by the implementer in a scratch export
  of the 459a299f3 candidate (`.uzi/scratch/gate-log.oBo3Vp`; the log does not
  record the applied edit). It reddens P2 first at after-publish (inputs 1); the
  scope stages and before-publish record zero-work attempts under it (masked), and the later stages, including the Now stages, are unobserved
  because the assertion stops at the first failing stage.
- At 187208c46 the I4 follow-on execute had a timeout-bounded cleanup join;
  a timeout could still leave it running. The maintainer correction below
  supplies an independent stop and requires actual settlement before restoration.
- Gates at 459a299f3: `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` EXIT 0
  (`.uzi/scratch/gate-log.gNJ2af`); full test/codex-executor.test.ts 1979/1979,
  671.1 s (`.uzi/scratch/gate-log.0pXZ4s`); `git diff --check` clean
  (`.uzi/scratch/gate-log.zyBkM1`). At 187208c46 (cleanup-only test change): the
  focused 'publication gates Now|plan summary|intent summary' selection 24/24
  (`.uzi/scratch/gate-log.Di171j`), typecheck and lint clean
  (`.uzi/scratch/gate-log.5bBUjb`).

Not applicable (production-path reasons):
- Plan production trigger at the barriers: the Runner is inside the switch, not
  inside gatePlan, during publication; plan attempts use the retained callback.
- Plan on the preserve-only path: that claim is pre-approved, so no plan gate or
  plan summary runs (codex-executor.ts preApproved check).
- Intent production trigger at the barriers: the intent launch runs once per
  `runLeg`, before any in-place switch; intent attempts use the retained
  unlaunched ownership.
- Scope-install stages when B is api_key: production installs no operation
  scope for an API-key B, so only the clear stages exist.

Exact selections per row. Plan rows (P1a, P1b, P2, P3, P4) ran with
`--test-name-pattern='plan summary'`; intent rows (I1a, I1b, I2, I3, I4) with
`--test-name-pattern='intent summary'`, each in a scratch export of the named
revision (`git archive <sha>`, `.uzi/scratch` created inside the export,
agent/node_modules symlinked), from the export's `agent/`:
`env GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null node --import tsx --test --test-timeout=120000 --test-name-pattern=<pattern> test/codex-executor.test.ts`.
The restored-green baselines for the same selections are
`.uzi/scratch/gate-log.SbWMZY` (plan, a43bfe06b), `.uzi/scratch/gate-log.2VPhcS`
(intent, 28222928c) and `.uzi/scratch/gate-log.24Um5g` (intent, c1b399b07).

Timing (not a finding; no wait change is established):
- Matched selections, base 8be78a548 against 459a299f3, one sample each, node
  v26.12.1, same worker:
  - 'publication gates Now|plan summary|intent summary': base 11 tests, 6.65 s
    (M2 suite 2.90 s, `.uzi/scratch/gate-log.OfniEz`); candidate 24 tests,
    16.99 s (M2 suite 15.39 s, `.uzi/scratch/gate-log.eoDLII`).
  - 'M2 actualRunner': base 96 tests, 124.6 s (suite 123.10 s,
    `.uzi/scratch/gate-log.bmB3rE`); candidate 109 tests, 141.3 s (suite
    139.74 s, `.uzi/scratch/gate-log.XFgV3M`), so about +16.7 s for the run
    (suite +16.6 s) for 13 leaves.
  - Single samples: noise is unmeasured. The command, environment, export setup
    and node version are as reported by the gate owner; the logs record the
    results, not those run conditions.
- Earlier, unmatched figures kept for the record: base 'publication gates Now'
  alone 2 tests, 8.1 s (describe 3.34 s, `.uzi/scratch/gate-log.QEGFlu`); the
  focused selection 19.8 s at c1b399b07 (`.uzi/scratch/gate-log.FGj8Dj`) and
  21.1 s at 28222928c (`.uzi/scratch/gate-log.af2F7W`); M2 describe 151.0 s at
  28222928c (`.uzi/scratch/gate-log.LGBnDk`).
- Full file 1979 tests, 655 s at c1b399b07 (`.uzi/scratch/gate-log.W4dRoA`) and
  671.1 s at 459a299f3; no pre-change full-file baseline was measured.
- CI step/job time and critical path: unmeasured, landing evidence; queue, cache
  and rerun are not separated.

Not credited or open:
- Unmet acceptance clauses (individual targeted mutation proof): P2, I2 and I3
  (fence matrix under the owner decision of 2026-11-21) and I1b post isolation
  (outside that decision). The preserve-only and after-finishB intent
  assertions were also not reached by any diagnostic. Combined removals are
  diagnostic only and do not substitute for individual proof, so delivery is
  partial and non-closing.
- Post-body attribution (no epoch field on the REST summary posts) and I4
  B-ordinal attribution (the follow-on frame characterizes a new Runner
  flight's baseline; it does not prove delivered B attribution on the
  transitioned claim).
- Exact-head PR CI for the full agent suite is a landing obligation, not run here.
- Reviewer N1: either half of the fix alone (entry check or in-flight
  `allowed()`) is unpinned; the post-await `allowed()` term is defence in depth.
- Reviewer N4: a plan-gate ACK spanning a completed switch now drops that
  presentation's summary silently; low reachability.
- Deferred non-blocking notes: late result synthesized at the `summaryRunner`
  seam; decorations; dead entry-time `lateInput.aborted`; redundant final order
  check; message wording; `summaryInputs` reads the live `currentAuthority`;
  launch probe does not pin the refusal reason; duplicated release block;
  CI time unmeasured.

Maintainer fixture corrections (2026-11-21, review r6005818453):

These tighten observations and fixture ownership. They do not upgrade P2, I2,
I3, I1b post isolation, REST post-body attribution or I4 switched-claim ordinal
attribution. The product's `admittedAuthority` fix is unchanged.

| Control | Change and observed result | Scope |
| --- | --- | --- |
| S1 plan factory abort observation | At A's raw fixture factory, `Reflect.set(params, "signal", undefined)`: the old test passes; the corrected test fails at "A plan work observed its signal aborted by the switch" in both directions | Observation sensitivity, not an independent production-fence proof |
| S2 intent factory abort observation | The same missing-signal fault at the intent factory: the old retained/preserve-only leaves pass; corrected retained leaves fail at "A intent work observed its signal aborted by the switch", and the preserve-only leaf at "preserve-only A intent work observed its supplied signal aborted" | Three named assertion reds; no guard-credit upgrade |
| S3 fresh B post after an unacknowledged A gate | Omit only `postPlanSummary`: the old unacknowledged-gate leaves pass; corrected leaves fail at "exactly one B plan summary post for this run before cancellation" in both directions | Pins the positive post's run id and B-generated summary, after the existing retired-A assertions |
| S4 B completion rather than timer cancellation | A diagnostic 1700 ms delay in the B fixture factory makes the old 1500 ms cancellation race fail the valid plan summary; the corrected leaves pass in both directions | The delay is diagnostic-only. Committed tests wait for the post or explicit callback completion; timeout reports failure instead of successful readiness |
| S5 independent follow-on stop | The generation-8 inputs fixture deliberately never reports cancellation. Omit the body's independent stop while retaining outer cleanup: both leaves fail at "follow-on flight stops independently of its next inputs poll" | Runner shutdown stops the owned flight; cleanup awaits actual settlement before restoring fixtures |
| S6 failure-path probe join | The new cleanup-only variant deliberately throws at the first publication barrier with a detached probe outstanding. Removing only the cleanup summary-probe join fails at "failure-path summary probes settle before fixture restoration" | Test-fixture regression only; it does not execute or credit the U20 publication assertions |

Restoring the same fixture passes the focused selection (25 cases, including
one cleanup-only case; zero skips). The missing-signal and missing-post controls
show the former observation gaps without disabling other production guards.
Probe scheduling is sealed before teardown releases barriers, and all started
summary probes are joined before mock restoration. The follow-on readiness race
records rejection before its named assertions, and its owned flight is stopped
independently of polling; a timeout is never treated as proof of settlement.

Control driver: `cdr-4583-fixture-mutants.py`; individual `cdr-4583-*-old.log`
and `cdr-4583-*-fixed.log` records. The diagnostic factory delay is not a CI wait.
On one local Linux sample the old 24-case selection took 6.724 s and the
25-case selection 6.517 s; noise and CI critical-path impact are unmeasured.
The new cleanup-only leaf took about 0.15 s. The full-file and check results
were independently executed: all 1980 cases in the touched file passed on
Linux with zero skips or cancellations, 219.150 s, concurrency 2 and the
unchanged 120000 ms timeout. `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`
passed as one job. The full agent suite remains an exact-head PR CI obligation.

### U21 — decision 11: no worker-side use of delivered Codex material before the predecessor proof (2026-11-18)

Final candidate feb16cdf2547797b706b5305131fc3ed8114150b (tests from
e3bc3e722, extended at feb16cdf2) extends the four crash-point recovery leaves
of "M2 actualRunner credential replacement" in agent/test/codex-executor.test.ts:
- "Codex switch crash after A teardown before apply recovers committed A
  without switching" (crash point 1);
- "… after apply commit loses auth response and recovers committed B" (2);
- "… after apply acknowledgement before B login recovers committed B" (3);
- "… during B login retains committed B" (4).

The crash leaves exist only for API-key A switching to subscription B
(factory phase), so the credit below is for that direction: delivered
API-key A material at crash point 1, delivered subscription B material at
2–4. Each leaf recovers through a distinct real Runner
(`recoverRetainedClaim` → `recoverRetainedClaimBody`) that consumes the
recovered claim, whose payload delivers the committed material with a
reminted capability.

The recovery Runner gets a `quiesceRun` option. At the `predecessor_capture`
site (the `quiesceRun` call at runner.ts:31043–31045, inside the `runFreeCapture` that starts at :31040) it records the state of every credential sink
the test can observe, then delegates to the real `quiesceRunAttempt`. The
recorded state is the executor provider client's `releaseCodex` (`releases`)
and `refreshCodex` (`refreshCalls`), the recovery Runner's own client, executor
bodies, provider launches and provider transport requests. Two new recovery
modes, "quiescence survivors" and "quiescence unverified", return that verdict
for that site only. Assertions:
- every one of the 32 leaves (four crash points × eight recovery modes)
  requires the first predecessor proof to run with
  `releases=0 refreshes=0 runner=0 bodies=0 launches=0 requests=0`
  (codex-executor.test.ts:35217–35218 at feb16cdf2 and HEAD: the assertion starts
  at :35217 and its message is on :35218; at e3bc3e722 it starts at :35216);
- the forced leaves (8) require:
  - zero executor bodies;
  - zero releases and refreshes on the executor client and zero credential
    operations on the Runner client;
  - at least one recovery rig, with no provider launch and no provider request
    on any rig (so no `account/login/start`);
  - the retained journal's `blocker: "quiescence_failed"`;
  - a `failed` report whose reason starts `worker_residue_blocked`.

a233 reference: the gate is identical at a233883de3c36bae2e940f999cd9d582889dad00
(`git show a233883de3c36bae2e940f999cd9d582889dad00:agent/src/runner.ts`
:29462–29469: the `predecessor_capture` proof and `blocker = "quiescence_failed"; throw finalize;`),
so the gate controls are class (b) preservation, exercised here through the
switch crash points with switch-delivered material.

Trace (code reading). Before the proof the recovery path reads the delivered
material only to:
- register it with the redactors (runner.ts:6223–6226);
- construct, but not run, the executor (runner.ts:6230; the `CodexExecutor`
  constructor makes no client call);
- create a lazy operation scope (runner.ts:6284–6296;
  `acquireCodexOperationScope` makes no client call until an operation).

The other pre-proof steps are credential-free: policy admission
reconciliation, the memory receipt handoff and repark check, the predecessor
memory-settlement log, held-source disposal and predecessor source admission.
The first credential operation is the executor's eager release inside
`executor.run` (codex/codex-executor.ts:3903), reached at runner.ts:19017
after recovery returns. No use before the proof was found, so no fix was
needed.

Command: `.uzi/scratch/u8tools/actl.sh` (export, `npm run typecheck`, then
`node --import tsx --test --test-timeout=120000 --test-name-pattern='Codex switch crash' test/codex-executor.test.ts`,
32 leaves). Logs `.uzi/scratch/ctl-logs/u21*-*.{diff,log}`. Three earlier
attempts did not compile (`u21-*.compilefail*.log`) and earn nothing.

| Control | Class | Mutation (runner.ts) | Red |
| --- | --- | --- | --- |
| u21r-exec-release-before-proof | (c) insertion of the real first use ahead of the proof, (b) | before the proof (:31038, the `await check()` line itself, ahead of the proof call that starts at :31040) insert a `releaseCodex` through the executor's own provider client (`flight.executor.opts.client`), the client its eager release uses | at feb16cdf2: 32 of 32 at :35217 "no delivered Codex material is used before the first predecessor proof", actual `releases=1 …` |
| u21r-exec-refresh-before-proof | (c) insertion, (b) | the same position, a `refreshCodex` through the executor's provider client | at feb16cdf2: 32 of 32 at :35217, actual `refreshes=1 …` |
| u21-release-before-proof | (c) insertion, sensitivity of the Runner-client counter only | the same position, a `releaseCodex` on the Runner's own client (production never releases through it) | at e3bc3e722: 32 of 32 at :35216 (the assertion's start line at that revision), `runner=1` |
| u21-gate | (d) removal, (b) | the `quiescence_failed` gate (:31050) → `if ((…) && Number.NaN > 0)` | at e3bc3e722: 8 of 32 (every forced leaf) at :35219 "unproven predecessor never reaches the executor body"; the 24 quiescent leaves pass. Rerun as u21f-gate at e0f730e4f (a ledger-only child of feb16cdf2; log `.uzi/scratch/ctl-logs/u21f-gate.log`): the same 8 of 32 at :35220 |
| u21r-blocker | (d), (b) | the gate's `blocker = "quiescence_failed"` → `"preservation_failed"` (:31051) | at feb16cdf2: 8 of 32 (every forced leaf) at :35231 (the assertion's start line; its message is on :35232) "an unproven predecessor takes the quiescence_failed blocker", the executor still unrun |

Unchanged candidate feb16cdf2: 32 of 32 pass (`u21r-green.log`, 92.2 s,
`SHA=feb16cdf2… EXIT=0`); agent `npm run lint` 0 warnings and 0 errors
(`u21r-lint-agent.log`). Runtime: the eight forced leaves take about 1.1–1.6 s
each (observed 1.08–1.58 s, about 10.5 s in total, from per-leaf durations in
`u21r-green.log`). Shard time before and after this change is unmeasured: no
pre-change baseline of the 24 existing leaves was recorded. Hosted effect
unmeasured.

Credit (API-key A → subscription B crash points 1–4, same-worker retained
recovery):
- delivered material is not released or refreshed through the executor's
  provider client before the predecessor proof (two (c) insertion controls,
  (b));
- when the proof reports survivors or unverified, the executor never runs
  (u21-gate), and the run takes the `quiescence_failed` blocker (u21r-blocker),
  all (b).

Asserted but not controlled: zero executor bodies, provider launches and
provider requests before the proof; on the forced leaves, zero releases,
refreshes, launches and requests and the `worker_residue_blocked` report
(the gate mutant fails earlier, at the body assertion); in the normal modes,
the existing assertions that the release and the B login happen at all (they
are not ordered against the proof except by the first-proof snapshot).

Not credited:
- "Discarded" is not shown observably; the tests observe non-use only: no executor body and no provider root.
  Nothing checks the flight's memory, redactor eviction, scope closure or
  executor disposal. The redactor registration of the delivered material is
  the permitted exception.
- Subscription A and API-key B delivered material.
- The gate's `kind !== "observed_empty"` half: the forced verdicts arrive as
  `observed_empty` with `blocked`.
- The re-hold variant at runner.ts:30857–30866.
- Recovery on a different worker, which has no local retained journal, so
  this gate does not run.
- The outcome on `quiescence_failed`: it is a terminal `failed` report with the
  clone and custody kept (existing behaviour), not an owner pause.

Crash point 5 is the owner's recorded limitation below.

Crash point 5 (predecessor host unreachable): owner decision (2026-11-18),
recorded scope decision, **unvalidated and uncredited**, an environmental
limitation. Fencing refuses stale uzi operations (generation, capability,
registration), but neither a different B credential nor revoking A's uzi
capability stops a provider credential that an unreachable predecessor
already holds. No option-B protocol work on this run.

The owner's stated requirement is that B is never used while predecessor
quiescence is unproven, with the run held for the owner instead. **The code
does not enforce that requirement on this path.** Traced at HEAD:
1. `RequeueRunsOfStaleWorkers` (runtime.sql) requeues a stale worker's run.
2. `ClaimRun`'s affinity pin (runtime.sql) lets another of the user's workers
   claim it once the owner is heartbeat-stale, not draining or in maintenance, and past the stale-requeue grace, after the
   affinity ceiling (default 2h), or when the worker row is deleted.
3. `codexClaimSecrets` (workersvc/codexauthz.go) mints a fresh capability and
   delivers the run's current binding, which after an applied switch is B.
4. The new worker has no retained journal, so `if (retainedJournal)` in
   `agent/src/runner.ts` is false, `recoverRetainedClaim` is skipped, the
   `predecessor_capture` / `quiescence_failed` gate never runs, and the run
   proceeds with B.

So B can be used while an unreachable predecessor may still hold provider credential A, and B too if the switch applied on that host (the apply response delivers B's material: a static API key, or a subscription access token until it expires). Without a switch the same path delivers A to the new worker. The Codex claim path (`codexClaimSecrets`) and `ClaimRun`'s affinity pin contain no switch-specific logic, so this is not introduced by the switch (read from the code, not executed). U21 (above) evidences the gate only
for same-worker retained recovery; the cross-worker path is unvalidated and
uncredited. This gap goes to the owner as a formal decision; fail-closing it (for example refusing or holding a cross-worker Codex claim while the predecessor is unproven) is an option for the owner, its size not assessed, and outside the approved plan.
On the same-worker path the held outcome is a terminal `failed` report with
clone and custody kept, not an owner pause. Continuation is never credited
because the host is gone.
