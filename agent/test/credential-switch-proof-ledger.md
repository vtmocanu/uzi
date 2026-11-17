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
| Strict usable persistence, clear confirmation and continuation on A before teardown | R1–R4 below credit three failed-save/inspection variants and empty applied-ACK only; other clauses unvalidated |
| Proven A root/tool reap before B material, login and model work; fresh CODEX_HOME | R5 credits unproven registered root pre-apply only; tool-only, other modes/phases and HOME clauses unvalidated |
| Whole operation-scope/refresh/identity/capability/summary replacement and retired-A rejection | A2 credits one reader; A3 retained-A/Sub→Sub B-scope namespace isolation; R7 B dispatcher publication readiness; R8 retired API-key A reconcile release-request fence during B login; remaining mixed clauses unvalidated |
| Exact saved-ID resume; confirmed-refusal-only fresh fallback and lineage; preservation re-hold | R6 credits uncertain-error classification only; confirmed-refusal/preserve companions positive, independent preservation and other clauses unvalidated |
| Policy episode rejection at request and under apply lock | D74–D77 credit the two isolated owner cases and the same two conditions introduced after READY before apply; paused holds, concurrent interleavings and other policy clauses unvalidated |
| Refresh lease/intent exclusion, account lock ordering and boundary reconciliation before revoke | D78/D79 credit isolated A coordination-lease and rotating-intent server checks; actual refresh admission, multi-account lock ordering and worker reconciliation ordering remain unvalidated |
| Post-switch release epoch and secret-hash authorization | B5/B6 preservation sensitivity controls below, through real worker apply and release routes; final reauthorization timing unvalidated |
| Transactional frozen binding rewrite, capability revoke/remint and post-commit material response | Unvalidated; B5/B6 do not establish transaction atomicity or mint/revoke implementation |
| Ordinal-zero baseline, successor journal ordinal/capability relation and usage attribution | Positive assertions present; qualifying controls unvalidated |
| Atomic rollback, lost-reply replay, stale/foreign apply and target fences | Unvalidated |
| Stopped-support negotiation, immutable expected-epoch latch and unsupported-response obligation | Unvalidated |
| Stopped numeric epoch equality: stale/mismatched running and paused, applied successor | D controls: five individually executed cases above |
| Stopped pending-input exclusion for eight named kinds in running and paused states | D controls: sixteen individually executed cases above; other control flags/cutoffs unvalidated |
| Paused replay rejects expired wall budget | D: paused_expired_budget; other wall-budget branches unvalidated |
| Paused replay uses original pause clock and performs no run update | D: paused_replay_original_clock and paused_replay_no_write |
| Stopped registration nonce equality | D25–D56: running/paused registration_nonce_mismatch only; worker-latched bounds and other incarnation clauses unvalidated |
| Stopped claim generation and unreleased claim | D25–D56: running/paused generation/release without reconciliation; ownership and other reconciliation paths unvalidated |
| Stopped frozen secret and capability hash presence | D25–D56: running/paused missing_frozen_secret and missing_capability_hash; binding identity/ownership unvalidated |
| Stopped switch-request/generation, ordinary hold, pause-request/mode, stop-kind/reason and reserve-hold markers | D25–D56: individual running/paused marker cases; policy and other competing controls unvalidated |
| Stopped refusal of claimed status | D25–D56: running/paused status_claimed; other statuses unvalidated |
| Stopped ownership, lane and policy fences | D57–D60 credit pending-fresh/unresolved-outcome; D61 paused policy-hold; D62/D63 stable user-owner mismatch running/paused. D64–D73 credit the five enumerated STOP lane exclusions running/paused; concurrent owner changes and other policy clauses unvalidated |
| Stopped replay after withdrawn X/Y; resume/reclaim and incarnation changes | Unvalidated; the fixture's single HTTP withdrawal does not discharge X/Y |
| Stopped preservation of binding, journals, session, gates, questions and custody without input consumption/time banking | E1 base-journal deletion, E2 applied-input deletion, E3 custody timestamp corruption, E4 owner-discard/live-reference loss, E5 direct paused-time credit, E6 applied-switch journal deletion only; other dispositions/effects, replay sensitivity and broader clauses unvalidated |
| Stopped known extra typed member and register_nonce case alias | D25–D56: four running/paused wire cases only |
| Bounded strict stopped request, other forbidden combinations, op_id and minimal acknowledgement | Unvalidated |
| Tokens absent from all inputs/acknowledgements/messages/logs | Targeted positive assertions present; qualifying controls and m4 security gates remain required |

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
`epochSourceAdmission` → `PlanRevisionRetry.beforeTurn` (only with an active
silent revision) is not covered by u2-transition.

Evidence-log note (2026-11-17): a validator's cleanup deleted ten earlier
`.uzi/scratch/gate-log.*` files from this run, including the merge-unit
`check:sqlc-drift`, the seven-test LiveDB re-verification at 79f74862d and the
`task check:api` EXIT=0 at e2e84464c. Those results are reported from the
transcript only; the candidate-level checks are re-run before m3 completion.
