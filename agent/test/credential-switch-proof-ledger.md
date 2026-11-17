# Live Codex switching: per-seam proof ledger

Base: a233883de3c36bae2e940f999cd9d582889dad00.
Candidate code: 6a7e7ae11b43c8fbf658d8b2fdcc557441c699c8, with the
test-only replacement-first adoption assertion in this commit.
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
  removing several guards together does not discharge them individually.

Mixed clauses are split. An old helper does not establish historical
provenance for new callers, ordering or authority fences. Each required
clause needs qualifying evidence or remains unvalidated. Controls used one
candidate export at a time, removed immediately; no base was built for
these controls.

## Executed controls

All six new controls below ran the real Runner case in
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
| Strict usable persistence, clear confirmation and continuation on A before teardown | Unvalidated |
| Proven A root/tool reap before B material, login and model work; fresh CODEX_HOME | Unvalidated |
| Whole operation-scope/refresh/identity/capability/summary replacement and retired-A rejection | A2 credits one reader only; remaining mixed clauses unvalidated |
| Exact saved-ID resume; confirmed-refusal-only fresh fallback and lineage; preservation re-hold | Unvalidated |
| Policy episode rejection at request and under apply lock | Unvalidated |
| Refresh lease/intent exclusion, account lock ordering and boundary reconciliation before revoke | Unvalidated |
| Transactional frozen binding rewrite, capability revoke/remint and post-commit material response | Unvalidated |
| Ordinal-zero baseline, successor journal ordinal/capability relation and usage attribution | Positive assertions present; qualifying controls unvalidated |
| Atomic rollback, lost-reply replay, stale/foreign apply and target fences | Unvalidated |
| Stopped-support negotiation, immutable expected-epoch latch and unsupported-response obligation | Unvalidated |
| Stopped numeric epoch, registration/claim ownership, pending-control exclusion, wall/policy/reserve fences | Unvalidated |
| Stopped replay after withdrawn X/Y; refusal after epoch/claim/incarnation changes; original pause clock | Unvalidated |
| Stopped preservation of binding, journals, session, gates, questions and custody without input consumption/time banking | Unvalidated |
| Bounded strict stopped request, forbidden op_id/combinations and minimal acknowledgement | Unvalidated |
| Tokens absent from all inputs/acknowledgements/messages/logs | Targeted positive assertions present; qualifying controls and m4 security gates remain required |

## Baseline absence and the class (d) method boundary

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
These clauses remain required and unvalidated until their individual real-path
guard controls have executed; no functional criterion is accepted, waived
or deferred.

Other unvalidated rows also remain worker obligations. m3 is incomplete.
m4, exact-head CI obligations and two-real-account owner acceptance retain
their existing classifications.
