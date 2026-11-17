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
| Explicit owner Codex request, target ownership/kind and live-status admission | Unvalidated |
| Same successful flight/generation/worktree with no new attempt, custody or release/reclaim | Positive assertions present; independent preservation/control accounting unvalidated |
| Strict usable persistence, clear confirmation and continuation on A before teardown | R1–R4 below credit three failed-save/inspection variants and empty applied-ACK only; other clauses unvalidated |
| Proven A root/tool reap before B material, login and model work; fresh CODEX_HOME | R5 credits unproven registered root pre-apply only; tool-only, other modes/phases and HOME clauses unvalidated |
| Whole operation-scope/refresh/identity/capability/summary replacement and retired-A rejection | A2 credits one reader; A3 retained-A/Sub→Sub B-scope namespace isolation; R7 B dispatcher publication readiness only; remaining mixed clauses unvalidated |
| Exact saved-ID resume; confirmed-refusal-only fresh fallback and lineage; preservation re-hold | R6 credits uncertain-error classification only; confirmed-refusal/preserve companions positive, independent preservation and other clauses unvalidated |
| Policy episode rejection at request and under apply lock | Unvalidated |
| Refresh lease/intent exclusion, account lock ordering and boundary reconciliation before revoke | Unvalidated |
| Transactional frozen binding rewrite, capability revoke/remint and post-commit material response | Unvalidated |
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
