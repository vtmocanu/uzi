# PRD #2971: re-verifying a recovery episode blocked at an old decoded-history limit (investigation checkpoint)

**Issue**: [#2971](https://github.com/vtmocanu/uzi/issues/2971) (Refs only; the issue stays open)
**Status**: Superseded (archived 2026-10-23). An investigation checkpoint only, nothing here was implemented. Consolidated into #3031, which carries its acceptance as R2971-01 to R2971-07.
**Earlier status**: Investigation checkpoint, 2026-10-14. No re-verification, consent endpoint, requeue, claim predicate, CLI authorization or migration is built. Ends at the M3 decision checkpoint for the human.
**Related**: [ADR-1197](../../adr/1197-transient-recovery-park.md) (#2512 and #2743 amendments), #2743 (limit raised), #2958 / PR #2976, #2979 / PR #2981.

This record holds observed evidence, not authority to retry, release custody or delete a source. It moved here from the `docs/` tree on 2026-10-14 because it is a working record, not a shipped doc page.

## M0: base integration and newer-generation restart (2026-10-14)

The branch merged base `84d365ed5aafa3642872e92e3efd0ee556bcd604` (merge commit
`3a60aa3b`), which carries #2958 (PR #2976, `2b84c993`, one deadline window per
fenced claim) and #2979 (PR #2981, `0be10c57`, verified-object reuse within an
operation). The only conflict was adjacency in the characterization test file; the
resolution adds exactly the #2971 test relative to base.

The characterization now also restarts a third time with a newer claim generation
(`claim_generation: 3`, ownership fenced at 3), the input on which #2976 opens a fresh
deadline window for an unblocked episode. The blocked short-circuit in
`recoverRetainedClaim` runs before that window computation, so the generation-3 claim
never reaches `reserveRecoveryIteration`; the test pins that precedence. The blocked
journal (which records `claimGeneration: 2`, asserted) is still not revived:
zero closure proofs, successor preparations, model calls and custody releases; the
journal is byte-for-byte unchanged; pin and dirty literal retained; the third terminal
report repeats “previous decoded verification exhaustion; current cap 1048576 bytes;
prior cap not recorded”. Focused run after the merge, from `agent/`:

```sh
umask 022; TMPDIR=<worktree>/.uzi/scratch node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 --test-name-pattern='issue 2971 unique-content' test/runner-retained-episode.test.ts
```

Result: exit 0, 1 passed, 0 failed/cancelled/skipped. The test's fake API does not enable the
`claim_generation_fence` feature, so the terminal report body carries no
generation field; the reason text, not a body field, shows the blocked branch ran.

## M1: permanent-block reproduction

Observed 2026-10-14 UTC against tree baseline
`150e442c43cf1f64bdcd3e24231ee2295a8a1f38` (starting HEAD
`a86fb127` has an identical tree). Added characterization:
`agent/test/runner-retained-episode.test.ts`,
“issue 2971 unique-content exhaustion stays blocked after a larger-cap restart”.

Command, from repository root:

```sh
(cd agent && node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 --test-name-pattern='issue 2971 unique-content' test/runner-retained-episode.test.ts)
```

First focused run exited 0, one test passed, no failures/cancellations/skips.
Scratch log: `.uzi/scratch/gate-log.RHk6w5` (session-local, not published).
The fixture uses real Git, a real retained clone and unique 64 KiB public
content, plus the untracked literal `only local dirty work\n`. The initial
runner exhausted on its first closure check; repeated-read accounting is not
needed. The unique captured closure measured 66,492 decoded bytes.

| Observation | Initial execution | Recreated cache and runner |
| --- | --- | --- |
| Configured cap | 4,096 bytes | 1,048,576 bytes |
| Runner closure calls | 1 | 0 |
| Real model calls | 0 | 0 |
| Successor preparations | 0 | 0 |
| Custody release calls | 0 | 0 |
| Journal | attempts 1, backoff 1 ms, blocked / decoded_history_limit | byte-for-byte unchanged |
| Source and pre-existing episode pin | retained | retained |
| Dirty-work literal | preserved | preserved |

The initial exception was an actual `RecoveryClosureLimitError` with
`limit === 4096`, message:

> reachable recovery history exceeds the 4096 bytes decoded verification limit

The exact initial terminal report was:

> Retained recovery blocked: reachable recovery history exceeds the 4096 bytes decoded verification limit; local work and custody retained

The original journal timestamps were `startedAt=1791980473960`
(2026-10-14T12:21:13.960Z) and `deadline=1791980773960`
(2026-10-14T12:26:13.960Z). The test asserts the deadline remains in the
future at the initial block, equals startedAt + 300000, and that attempts,
backoff, source/attempt attribution and the entire journal survive restart
unchanged. It pre-establishes a real source-tip episode ref to check pin
preservation; this is not a claim that the failed capture created a restore
pin. The blocked episode has no restoreTip.

An independent fresh local closure verification under the larger cap succeeded
before the restarted runner was invoked. That proof does not authorize
execution. The restarted runner still reported exactly:

> Retained recovery blocked: previous decoded verification exhaustion; current cap 1048576 bytes; prior cap not recorded; local work and custody retained

The test is passing characterization of the permanent block. It is not an
observed recovery-authorization fix or the eventual recovery-positive red/green.
The fake API supplies positively fenced generation 2 ownership for this local
fixture. It does not exercise the server lifecycle of a terminal incident run,
a live worker, forge upload, owner consent or production custody.

### Code evidence and related work

Starting points re-verified after the M0 merge (tree of `820d53a2`), not an
exhaustive reader inventory (the M2 audit below is the inventory):

- `agent/src/runner.ts:17270`:
  `if (source.recovery?.stage === "blocked") await fail();` — runs before the
  #2976 window computation at `runner.ts:17276`
  (`const newWindow = episode?.claimGeneration !== flight.claimGeneration;`).
- `agent/src/git.ts:5907`:
  `if (old?.stage === "blocked") throw new Error("recovery episode is blocked");` —
  runs before `git.ts:5911`
  (`const newWindow = claimGeneration !== undefined && old?.claimGeneration !== claimGeneration;`).
- `agent/src/git.ts:6095`:
  `if (journal.recovery?.stage === "blocked") return;` (a blocked record is never
  rewritten by a later block).
- `agent/src/runner.ts:17417`:
  `if (error instanceof RecoveryClosureLimitError) { blocker = "decoded_history_limit"; await fail(); }`
- `agent/src/runner.ts:17258-17260`: the blocked-journal report detail
  (“previous decoded verification exhaustion; current cap …; prior cap not recorded”).
- `adr/1197-transient-recovery-park.md` (#2743 amendment, “Configuration and failure
  boundary”) says an already blocked decoded-history journal stays blocked across
  restart or a cap increase and reports “prior cap not recorded”.

## M1: relationship to #2958 and #2979

Forge reads on 2026-10-14 (after 14:18 UTC): PR #2976 `state: merged`, PR #2981
`state: merged`; issue #2958 `closed`; issue #2979 `closed`; issue #2971 `opened`
with no comments. This corrects the earlier observation at about 12:18 UTC that
both PRs were `opened`. Both merges are in base `84d365ed` as `2b84c993` (#2976) and
`0be10c57` (#2981), which this branch merged in M0.

| Issue | What it fixes | Does it change the #2971 outcome? | Evidence |
| --- | --- | --- | --- |
| #2971 | Nothing yet. An episode blocked at `decoded_history_limit` under an older cap stays blocked after the cap is raised; restart, reclaim and newer claim generations all fail without re-verifying. | — | Characterization test, three executions (cap 4096; cap 1048576 same generation; cap 1048576 generation 3). |
| #2958 / PR #2976 | An unblocked episode whose five-minute deadline expired during a park or long turn gets one fresh deadline window per positively fenced claim. | **No.** Both blocked short-circuits (`runner.ts:17270`, `git.ts:5907`) precede the window computations (`runner.ts:17276`, `git.ts:5911`). #2958 itself scoped this: “Episodes already blocked stay blocked with their original reasons, including `decoded_history_limit`.” | Generation-3 restart in the characterization test: zero proofs, zero successors, journal byte-identical. The test pins only the runner guard: making it generation-scoped turns the generation-3 assertions red while the same-generation restart stays green. Making only the `git.ts:5907` guard generation-scoped leaves the test green, because the runner guard stops the claim first; that precedence is read from code, not pinned by this test. |
| #2979 / PR #2981 | Repeated closure checks within one recovery operation no longer re-hash and re-charge the same objects (an operation-scoped verified-object cache, `git.ts` `closureCache`), so a fresh episode with a large shared history stops exhausting 8 GiB by repetition. | **No.** It lowers the chance that a *new* episode blocks; it never revisits a journal already at `stage: "blocked"`, which never reaches a closure check. Genuinely unique content over the cap still blocks (the #2971 fixture uses unique 64 KiB content, and its single first closure check exhausts the 4096-byte cap). | #2979's description: “It is distinct from #2971, which covers an episode already blocked by an older limit.” Characterization test unchanged in outcome after the merge. |

Practical consequence: after #2976 and #2981 fewer *new* episodes should block, but
every journal already at `blocked / decoded_history_limit` — whatever cap blocked it
(1 GiB before #2743, 8 GiB for run `1ab9349c`) — stays permanently blocked. The only
ways forward remain the existing salvage paths or a new authorized re-verification
(M3).

## M2: cap-recording audit — NOT CLEAN, proposed work only

Proposal audited: persist the actual `RecoveryClosureLimitError.limit` when a
*fresh* episode blocks on `decoded_history_limit`, keep legacy blocked journals as
explicitly “prior cap not recorded”, change no blocked behaviour and grant no retry
authority. The approved rule allows the change only after a complete, clean
reader/writer and downgrade audit. Audit at `94594b12` (researcher, 2026-10-14):
**not clean**, so no code changed. The audit is recorded here as proposed work.

Post-#2995 reassessment on 2026-10-14, at merged tree `5d42cfe9`: M2 remains
**NOT CLEAN**. B1's worker key allowlist and downgrade exposure remain, and B2's
protected backup validator still rejects an additional cap key. The separate
`claimGeneration` backup defect was fixed by PR #2995 (`7a0481d7`); it is no
longer a prerequisite left to implement. No M2 cap-recording code is built here.

### Blocking sites

- **B1 — closed progress key set, and downgrade.** `agent/src/recovery-progress.ts`
  `recoveryProgress` rejects any key not in
  `keys(o, ["version", "source", "attempts", "startedAt", "deadline", "claimGeneration", "backoffMs", "stage", "restoreTip", "successor", "blocker"])`.
  A current worker would need an allowlist entry. Every worker from
  `v0.86.0-rc.8` on (when `recovery-progress.ts` first shipped), including the
  chart's pinned `0.86.0-rc.15` image, rejects the new key and fails closed. Older
  workers (for example `v0.85.2`, `v0.86.0-rc.7`) ignore `recovery` and may rewrite
  or blank the journal, dropping it. Fail-closed parse sites on an rc.8+ worker
  include the following; this is a starting point, not exhaustive:
  - `readRecoveryCapture` (seed paths and `terminalRetainedSnapshot`)
  - `readRecoveryCaptureWithRaw`
  - `journaledClonePaths` (canonical reuse and sweeps for the whole bare)
  - `protectedRecoveries` and `detachTerminalRetained`
  - `discardRetainedSource`, so a protected discard through the ops channel would
    fail
  - ACK retirement in `readAcknowledgedReportSourcesUnderLock`

  Operator inspect in `gatherRetainedUnderLock` degrades to `unreadable_journal`.
  The same rc.8–rc.14 fail-closed exposure already applies to base's
  `claimGeneration` when downgrading from rc.15; this is pre-existing, from #2976. ADR-1197 already calls
  downgrade during pending recovery unsupported, but this adds a new failure on a
  terminal blocked journal.
- **B2: backup validator, historical observation and post-#2995 measurement.**
  Original observation dated 2026-10-14, audit at `94594b12` against base
  `84d365ed`: the protected-descriptor validator in
  `.agents/skills/uzi-watcher/scripts/backup-runs.sh` had a closed progress key
  set without `claimGeneration`. The script's own jq program rejected that
  extra key, which #2976 writes on positively fenced reservations. The run
  recorded these constructed blocked `decoded_history_limit` descriptors:

  | Original descriptor class (base `84d365ed`, 2026-10-14) | Output | Exit code |
  | --- | --- | --- |
  | base shape without `claimGeneration` | accepted | 0 |
  | with `claimGeneration` | `invalid protected descriptor` | 5 |
  | with `claimGeneration` plus a cap key | `invalid protected descriptor` | 5 |

  At that base, the active-journal validator accepted extra keys and
  `backup-runs.test.sh` had no `claimGeneration` fixture. This pre-existing
  #2976 defect was reported as an incidental issue and **was fixed separately
  by PR #2995**, merged as `7a0481d7`.

  Reconstructed descriptors, same shape classes, measured at `5d42cfe9`
  (post-#2995) on 2026-10-14: the original fixture files were not retained, so
  these are reconstructed inputs, not byte-identical originals. The full jq
  program was extracted from the merged script and run with its `-ers` flags,
  matching run/branch/key/root arguments, and empty ledger/active-journal
  arguments. Each descriptor has a canonical source, attempts 1, startedAt
  1000, deadline 301000, backoffMs 1, and stage `blocked` with blocker
  `decoded_history_limit`. The second adds `claimGeneration: 2`; the third also
  adds `closureLimit: 4096`. **`closureLimit` is an illustrative cap-key name,
  not a decided field name.**

  | Reconstructed descriptor class (merged `5d42cfe9`, 2026-10-14) | Output | Exit code |
  | --- | --- | --- |
  | base shape without `claimGeneration` | accepted | 0 |
  | with `claimGeneration` | accepted | 0 |
  | with `claimGeneration` plus illustrative `closureLimit` | `invalid protected descriptor` | 5 |

  Both merged validators now accept an optional positive safe-integer
  `claimGeneration` (1 through 9007199254740991), and the backup tests cover
  legacy absence, valid boundaries and malformed values in both formats.
  The ordinary active-journal jq also accepted all three reconstructed journal
  shapes (exit 0 with source output); its progress key set remains open beyond
  the validated claim fence. The protected validator still maps an unknown cap
  key to `probe_inconclusive` and FAIL. Adding a nested cap field would therefore
  still require a protected-progress allowlist and type/range validation change,
  independently of B1's worker parser and downgrade requirements.

### Neutral sites (no change needed beyond B1)

- `parseRecoveryJournal`: top-level allowlist; a key nested under `recovery` is fine.
- `writeRecovery`: re-parses before writing.
- The spread writers in `reserveRecoveryIteration`, `recoveryClockCurrent`,
  `recordRecoveryCapture` and `prepareRecoverySuccessor` carry the key forward.
- `blockRecoveryEpisode` would set the key in its single blocking write. An
  already-blocked journal returns early, so a first recorded value is never renewed.
- `completeRecoveryEpisode` strips `recovery` and never applies to blocked journals.
- The protected-descriptor `JSON.stringify` equality checks in
  `detachTerminalRetained` and `protectedRecoveries` compare parsed values with a
  stable key order.
- The ops channel files (`agent/src/ops/retained-source.mjs`,
  `retained-source-server.ts`) never read journal fields themselves. Their inspect
  and protected-discard operations do reach journal parsing in `git.ts` (B1).
- No reader was found in `e2e/`, `scripts/`, `deploy/`, `.claude/` or `Taskfile.yml`.

### Server and web sweep (tracked, verified rather than assumed)

Each command was `git grep -n -F -- "<needle>" -- <dir>`:

| Needle | `api/` | `web/src` | `controller/` | Classification |
| --- | --- | --- | --- | --- |
| `decoded_history_limit` | 0 | 0 | 0 | — |
| `Retained recovery blocked` | 1 | 0 | 0 | embedded doc |
| `prior cap not recorded` | 1 | 0 | 0 | embedded doc (`configuration.md` mirror) |
| `failure_reason` | 386 | 112 | 0 | store/sqlc, DTOs, display and tests; no text parser found |
| `blocker` | 141 | 23 | 19 | tests, embedded docs, unrelated identifiers |

A further grep for the other `RecoveryBlocker` names over `api/`, `web/src` and
`controller/` (non-test, excluding embedded docs) found only unrelated identifiers with the same names: the run hold reason
`budget_exhausted` (270 lines) and `oversize`/“oversized” (91 lines). The other
seven names returned nothing. The parser search was line-oriented. Conclusion: the server treats `failure_reason` as opaque text. The
`blocker` hits were spot-checked, not read exhaustively.

### Proposed work, if the human chooses it later

1. The independent `claimGeneration` backup fix is complete in PR #2995
   (`7a0481d7`). B2 still requires validator support for any proposed nested cap
   field; the existing claim-fence support does not admit other progress keys.
2. Cap recording itself, choosing between:
   - (a) a nested optional `recovery` key: allowlist and type check in
     `recoveryProgress`, set only in `blockRecoveryEpisode` for a fresh
     `RecoveryClosureLimitError`, plus B2 validator support for that cap field, with downgrade documented as
     failing closed;
   - (b) a separate `uzi-recovery.<branch>.<name>` config key: older workers ignore
     it, but it needs its own clear/retire lifecycle at every journal-blanking site
     and is not carried into the protected descriptor.
3. Reword the prose that would become false for newly blocked journals: ADR-1197
   (#2743 amendment), `docs/configuration.md`, `docs/run-recovery-wait.md` and the
   embedded mirrors via `task docs:sync`. Update the two tests that pin the
   “prior cap not recorded” text. Add a CHANGELOG entry.
4. Required test: red/green on persistence, with these controls:
   - other blockers write no cap;
   - legacy journals stay unknown;
   - an already-blocked journal never renews its recorded cap;
   - the older-format parser's rejection is pinned.

Separate documentation gap found by the audit:

- ADR-1197 (“may drop the durable recovery fields”), ADR-1783, `docs/run-recovery.md`
  and `docs/run-recovery-wait.md` describe the downgrade risk as dropping recovery
  fields. That is accurate only for workers before `v0.86.0-rc.8`.
- They omit that rc.8+ workers reject unknown keys and fail closed.
- Reported as an incidental issue; the first report overstated it as “wrong” and was
  corrected.

Recorded decisions read at `specs/human.md:877,886,1004,1303` retain custody,
require exact disposal authority and qualify checkpoint coverage. No conflict
was found with this characterization; this is not an exhaustive compatibility
claim. In particular, line 1004 states:

> The server recorded checkpoint <tip> before this hold. Current availability and the latest local edits are not verified.

## M3: decision checkpoint — salvage versus authorized re-verification

This is where the investigation stops. Nothing below is built. The human chooses.

### Facts the decision rests on

- A `decoded_history_limit` block ends the run terminal `failed` with
  `keepCustody`. ADR-1197 (#2512 amendment): “it neither auto-reclaims nor exposes
  failed-run Resume”. `docs/run-recovery.md` (“Retained-source failure and local
  proof”) says the same. The server never re-claims a terminal failed run.
  Generic resume does not accept a terminal failed run. Thus in production
  nothing reaches the blocked short-circuit; M1 drives it directly in a fixture,
  where every reclaim still fails without re-verifying.
- The published checkpoint ref `refs/uzi-checkpoints/<branch>` (or
  `refs/uzi-recovery/<run-id>` after supersession) is kept only while some
  custody hold on the run is open. `docs/run-recovery.md` (“Published checkpoint
  refs”): “Once every hold on the run is released or discarded, uzi removes the
  ref.” **Discarding the last open hold deletes the retained checkpoint ref.** Any
  salvage must first make an independent copy, for example a fetched local branch
  or an exported archive. Destructive salvage is never tested on incident holds.
- The checkpoint ref holds only what was last published. It may lag dirty or
  untracked bytes in the retained source.

### Option (a): the smallest existing salvage path (no code change)

Steps, in this order:

1. `uzi run recovery <run-id>` lists the holds, checkpoint ref and capture state.
2. Copy the checkpoint or recovery ref into a named local branch
   (`git fetch origin refs/uzi-checkpoints/<branch>:refs/heads/recovered/<run-id>`).
   A salvage copy `refs/uzi-salvage/<run-id>` has its own bounded expiry,
   independent of hold discard, so where present it survives the checkpoint-ref
   deletion. An archive is available via `uzi run export` only if one was
   captured. Code reading (not exercised) for a `decoded_history_limit` failure:
   - The report maps to fail origin `agent_failure`.
   - **Held publication does not fire.** The retained-recovery flight sets
     `retainedLocalCustody`, and `prepareTerminalInventory`, the only starter of
     the held step, returns early on it. With `keepCustody` the flight is not
     reaped, so no settlement or archive capture runs either. So no
     `refs/uzi-held/...` ref is created.
   - **Salvage can fire, conditionally.** The salvage candidate query has no
     blocker or custody filter. It needs all of:
     - a failed issue or self-improvement run with a published checkpoint tip;
     - a forge listed in `UZI_SALVAGE_FORGES` (empty by default);
     - a finish within the retention window.

     It copies only the last checkpoint tip.
3. Locate the retained clone and its hold disposition through the worker-local
   `retained-source` inspect channel (`agent/src/ops/retained-source.mjs`). That
   channel is content-free: path, presence, size, digest, holds; no HEAD or diff.
   Then compare the clone's latest bytes against the copied ref with read-only
   `git` run on the worker via exec.
4. Land the work through a new run or by hand. Only then discard the holds.

| | Option (a) |
| --- | --- |
| Pros | No new authority, endpoint, migration or ADR change. Does not depend on blocker type, but requires recoverable source/checkpoint/archive evidence and appropriate operator access. Every custody invariant is untouched. |
| Cons | Manual and operator-heavy. Latest dirty or untracked bytes on the worker need worker access (hosted k8s: `kubectl exec`, with the uid-drop recipe only on split-UID workers) and can be lost if the holds are discarded first. The run cannot simply continue; the work is re-landed by a new run. For 21a4d826 and 38b52617 the issue reports only that holds were discarded by hand; whether a copy was made first is not recorded. |

### Option (b): owner-authorized one-shot re-verification

The one-pass-per-grant bound is a proposed requirement, not an established
property of this outline. It requires a separately reviewed server-consumption
design with exact source, worker/incarnation, generation and cap bindings, plus
replay controls. Local journal consumption alone does not establish that bound:
a restored journal or another worker could present the grant again. The server
consumption and replay design must be reviewed before this option can authorize
any re-verification.

What it would add:

- **Owner action:** an owner-only API action, plus a `uzi run` CLI verb, for a
  `failed` run with an open guarded hold and a recorded `decoded_history_limit`
  blocker.
- **Grant:** the action creates a one-shot grant (a DB migration) bound to:
  - the run and the hold's claim generation;
  - the blocked episode identity (`startedAt`, `attempts`, `source.clonePath` and
    `attemptId`);
  - the requesting owner.
- **Requeue:** the action requeues the run.
- **Claim:** the claim carries the grant.
- **Worker:**
  1. verifies the grant matches the journal exactly;
  2. requires server-consumption authority under the separately reviewed design
     and durably records local consumption *before* any closure check; the local
     journal is a crash marker, not sufficient replay prevention;
  3. re-verifies under the current cap with every existing hold, pin, generation
     fence and integrity check;
  4. on success, proceeds through the normal successor handoff; on failure,
     re-blocks with the grant spent.
- **Not covered:** other blockers, and episodes blocked at the current cap.
- **Legacy journals:** they have no recorded cap, so they need explicit
  authorization; no increase is inferred.

| | Option (b) |
| --- | --- |
| Pros | Recovers the exact retained source in place, with fresh verification and no hand-built copy. The intended at-most-one-pass bound requires the separately reviewed server-consumption design and replay controls above. Also covers new 8 GiB blocks such as 1ab9349c, if the cap is raised further. |
| Cons | It **is a failed-run Resume for this blocker** and amends ADR-1197 (#2512 amendment: “neither auto-reclaims nor exposes failed-run Resume”) and `docs/run-recovery.md`. It touches four components (API with a migration, worker, CLI, docs) and a trust boundary (a new owner-authorized state transition). Cap recording (M2) is a prerequisite to telling a block at the current cap from an old-cap block, and M2 remains not clean after #2995: B1's worker parser/downgrade requirements and B2's additional cap-key validation remain. Frequency is UNVERIFIED: two reported old-limit cases, both already discarded by hand. |

### Lead's reading (advisory, not a decision)

Given unverified frequency, no known surviving old-limit source, and the remaining
B1 worker/downgrade and B2 cap-key prerequisites, the evidence favours:

1. documenting option (a) as the supported path for blocked decoded-limit runs;
2. retaining the independent `claimGeneration` backup fix completed by #2995;
   any later nested cap field still needs B2 validator support;
3. deferring option (b) until a surviving source makes it worth amending ADR-1197.

The human decides. #2971 stays open.

## Bounded incident and surviving-source ledger

Observation date: 2026-10-14 UTC. Read issue #2971's captured description and
fresh comments, issue #2958 (including its correction comment), and issue
#2979. These returned no truncation/withheld/unknown-comment flags. All incident
descriptions remain reports; this run independently inspected **no production
journal**.

A bounded forge search requested opened issues with label `area::recovery`
updated after `2026-10-11T00:00:00Z` through the observation at approximately
12:22 UTC on October 14. It returned 31 issue summaries with `truncated:
false`; this is coverage of that query, not all runs/incidents. Full incident
content inspected is limited to the three named issues above. Closed,
unlabelled and older reports, unavailable operator evidence and worker-local
journals are outside that coverage. A tracked token search for the supplied run
prefixes found no matching incident journals (the unrelated numeric matches
are not evidence). A filename search for incident/operator/journal artifacts
likewise supplied no incident journal.

| Reported run / issue | Classification and blocking cap evidence | Safe worker / generation / attempt evidence | Source, pin, hold and checkpoint evidence |
| --- | --- | --- | --- |
| 21a4d826 / #2971 | Reported old 1 GiB exhaustion; failed Oct 11 08:58Z. No durable cap field inspected. | Worker/incarnation, generation, attempt unknown. | Holds reportedly discarded 09:13Z. Not counted as surviving. Source/pin existence, checkpoint availability and latest-edit coverage unverified. |
| 38b52617 / #2971 | Reported old-limit failure Oct 12 07:29Z; no journal or recorded cap inspected. | Worker/incarnation, generation, attempt unknown. | Issue groups it with manually discarded holds; exact discard time unavailable. Not counted as surviving. Source/pin/checkpoint availability and coverage unverified. |
| 1ab9349c / #2979 | Reported **fresh** 8 GiB exhaustion Oct 14 10:19:43Z, attempts 1, start 10:18:21Z, decoded_history_limit. Not evidence of an old-limit block. | Codex worker rc.14 reported; exact worker/incarnation, source/execution generations and attempt ID unknown. | Checkpoint prefix 3fcd6a21 reported on origin; no live availability/coverage verified. Current source/pin/hold status unknown. |
| 0d5a0f7c / #2958 | Reported expired episode clock, budget_exhausted, attempts 1; start 07:31:17.572Z, deadline 07:36:17.572Z Oct 14. Decoded cap not applicable to reported cause. | Codex ephemeral rc.14 reported; exact identities unknown. | Three holds reportedly left; current status/source/pins unknown. Later capture is reported but preservation not independently established. |
| 84ed3050 / #2958 | Reported expired clock, budget_exhausted, attempts 1; start 01:57:47.572Z, deadline 02:02:47.572Z Oct 14. | Claude persistent rc.11 reported; exact identities unknown. | Current hold/source/pin/checkpoint availability and latest-edit coverage unknown. |
| 63ea43d9 / #2958 comment | Reported stale deadline on new claim after OOM, failed Oct 14 10:56:06Z; start 10:46:41Z, deadline 10:51:41Z. Not old-cap exhaustion. | Codex ephemeral rc.14 reported; new claim asserted, exact fence/attempt unknown. | Comment asserts retained checkpoint/custody and “No work was lost”; this run has not verified complete latest-work preservation. |
| Local issue-2971 fixture | Independently reproduced 4096-byte exhaustion; recreated runner at 1048576 bytes remains blocked, at the same claim generation and at generation 3. | Synthetic claim generation 2, same retained run/source attribution asserted; no live worker. | Source, literal, existing pin and journal retained within isolated fixture; no production hold or checkpoint inference. |

Evidence classes, kept separate (observation date 2026-10-14; forge reads
refreshed after 14:18 UTC with no new #2971 comments):

| Class | What it contains | Coverage | Strength |
| --- | --- | --- | --- |
| Reported incidents | 21a4d826 (failed 2026-10-11 08:58Z, holds discarded 09:13Z) and 38b52617 (failed 2026-10-12 07:29Z), both from the #2971 description; 1ab9349c (fresh 8 GiB block, #2979); 0d5a0f7c, 84ed3050, 63ea43d9 (deadline, #2958) | Issue texts #2971, #2958, #2979 only; the earlier bounded `area::recovery` query above | Reports only, not re-verified |
| Independently inspected journals | **None** | No worker, PVC, journal, hold listing or checkpoint was read by this run | — |
| Fixture results | The characterization test: 4096-byte block, larger-cap restart at the same generation, restart at generation 3 | One isolated local fixture, fake API, real Git | Observed, local only |

Of the reported incidents, only 21a4d826 and 38b52617 are old-limit (#2971) cases,
and both holds are reported discarded, so neither is a known surviving source.
1ab9349c is a fresh 8 GiB block; by the same code path it is now also a permanently
blocked journal, which #2981 does not revisit (see the M1 relationship table).
Current frequency and the set of surviving blocked sources are **UNVERIFIED**.

The **current live configured cap is unknown for every incident**. The code
default is 8 GiB; it cannot establish an individual worker's configuration.
Reported historical limits are not durable recorded-cap evidence.

Current frequency and surviving-source status are **unverified**. There is no
eligible-run denominator, fleet inventory, complete incident interval, live
worker read-only grant, authenticated exact-source descriptor/pin inventory,
current owner hold listing or independently verified checkpoint byte coverage.
Two historical reports do not establish a rate, and discarded holds do not
establish surviving sources. Required next evidence is a dated read-only run
population for the same interval, exact worker/incarnation and source/execution
generation/attempt bindings, original journals and current cap configuration,
current source/pin/hold availability, and independent checkpoint availability
with a comparison against dirty/untracked latest bytes. No credential request,
live mutation, hold discard, limit increase or redispatch was performed.

## Validation status (earlier run, historical)

This section records the earlier run b8016a14 at `0b3afc33`. Its “M2/M3 not yet
performed”, “M1 remains in progress” and “broad validation still blocks M1”
statements are superseded. M1–M3 are recorded above. Completion of this
checkpoint rests on this run's required gates (see “Validation status (this
run)”). Scratch log names in that section are from the earlier run and are not
published. This run's
validation is recorded in “Validation status (this run)” at the end.

Focused characterization passed as recorded above. Coder and researcher
delegations returned transport failures, not review results. Reviewer, tester
and auditor independently ran the focused characterization successfully on
`70bb9a4af0c26438bb819bb67c3849942c14241a`. Each reported the same
restart-successor-counter coverage gap; the repair instruments the recreated
cache separately. Reviewer and auditor then reported clean scoped static
repair review at `0b3afc33253b73fd77390cf7cdc19ba4eada9936`; tester's first
repair dispatch was cancelled with no result, then its retry independently
passed the named test on that SHA (1 pass, no failures/cancellations/skips).
The lead's repaired focused run also passed, scratch log
`.uzi/scratch/gate-log.jmw6oZ`; the tester retained
`.uzi/scratch/gate-log.jDRWUA`. No deferred review defect remains.

### Blocking broad-validation result

After every validator returned, the candidate was frozen at
`0b3afc33253b73fd77390cf7cdc19ba4eada9936` and the lead ran
`task gate:agent` alone in the foreground with a unique scratch log.
The runtime stopped it at its **60-minute deadline** and reaped its process
tree on 2026-10-14, approximately 13:49 UTC. It is **not a pass**.
The shell wrapper was interrupted before writing its numeric exit status;
status is runtime deadline termination, numeric exit unavailable. Log:
`.uzi/scratch/gate-log.2bvT9T` (session-local, not published).

The log reached deps-check, lint, dead code, typecheck and the unit suite.
Lint reported zero errors/warnings; no preceding task failure was reported.
The unit suite had not finished; no final unit summary or Codex M4 completion
was reached, and the new characterization was not shown in this partial log.
Its independent focused passes remain the evidence for that test.
Two named failures appeared:

- `runner-acknowledged-final-restart.test.ts`: “new report proof transport
  failure preserves existing physical archive authority within the same budget”.
- `runner-plan-cross-check.test.ts`: “forces the supported human disposition
  on submit checker_unavailable”.

There was no broad-suite final diagnostic block before termination. To avoid
attributing either failure to baseline without evidence, the lead selected the
two named tests on candidate and base, then the two whole files on candidate
and base. Commands used the same wrappers, Node imports, timeout and
concurrency, with matching-lockfile installed dependencies in the immutable
base scratch export. No Git-dependent gate ran inside an export.

From the respective `agent` directory:

```sh
../scripts/real-procfs-skip-check.sh ../scripts/tmpdir-leak-guard.sh node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-concurrency=3 --test-timeout=120000 --test-name-pattern='new report proof transport failure|forces the supported human disposition on submit checker_unavailable' test/runner-acknowledged-final-restart.test.ts test/runner-plan-cross-check.test.ts
```

The whole-file command was identical with `--test-name-pattern` omitted.

| Selection / revision | Recorded outcome | Session-local scratch log |
| --- | --- | --- |
| Named two / candidate 0b3afc33 | exit 0, 2 passed, zero failed/cancelled/skipped | gate-log.zOgDZR |
| Named two / base a86fb127 | exit 0, 2 passed, zero failed/cancelled/skipped | gate-log.5cAmOH |
| Whole two files / candidate 0b3afc33 | exit 0, 105 passed, zero failed/cancelled/skipped | gate-log.i5i7GD |
| Whole two files / base a86fb127 | exit 1, 104 passed, 1 failed, zero cancelled/skipped | gate-log.gIO2Cu |

The base export was resolved to
`a86fb127d4cea4dfa8ea42b8f7f4331b3b1f9436` and has the original baseline
tree. Base failure reproduces the report-retirement assertion at line 866:
actual `{ retired: false }`, expected
`{ retired: true, authority: "existing_archive" }`. This is an observed
baseline test instability; its production root cause is not established.
It was reported as an off-task finding without a code fix.
The checker-unavailable failure passed all narrowed comparisons and remains
**inconclusive**, not attributed to baseline. Neither result converts the
stopped broad gate to green.

`task gate:repo` remains **unrun** because the required first gate did not
pass. No Go changes, gate:api, live E2E, LiveDB or production inspection occurred.
No full gate was repeated on an unchanged tree to retrieve output. Broad
validation still blocks M1 completion and any shipping claim. A successful
required gate execution is needed before M1 can be declared complete; no
baseline waiver or alternate gate equivalence is claimed.

M1 remains in progress with committed passing characterization and evidence.
M2 cap audit and M3 salvage/design checkpoint are not yet performed.

The eventual acceptance remains outstanding: owner-authorized recovery-positive
red/green, blocker/current-cap controls, one pass per consent, every custody and
integrity check, actual blocking-cap storage with explicit legacy unknown,
episode-policy ADR amendment if semantics change, and required serial gates.

## Validation status (this run)

- **Focused characterization, one red run.** One lead run between the M0 merge
  and `820d53a2` failed (exit 1). The cause was an assertion the lead wrote and
  then dropped, which expected a generation field in the failure-report body. The
  fake API does not send that field without `claim_generation_fence` (see M0). It
  was not a product defect.
- **Focused characterization run.** Run on this run's merged tree, from `agent/`,
  with the command in the M0 section: exit 0, 1 passed, 0 failed/cancelled/skipped.
  Runs:
  - lead after the M0 merge;
  - lead after the M1 assertion;
  - the M0 reviewer, independently, at `820d53a2`;
  - the M1 fact-checker, independently, at `94594b12`.

  The M2 and M3 fact-checks ran no tests; those ranges change only this record.
- **`task check-docs:web`.** The lead ran it on the final record candidate: exit 0.
  The log is run-local and unpublished. Some earlier interim runs were not logged;
  every run exited 0.
- **Read-only validation per commit.** M0 by a reviewer; M1, M2 and M3 by a
  fact-checker. Each blocking finding was fixed in a follow-up commit, and the M2 fix
  range was re-checked. The M3 fix range was re-checked at `1c7a6ac2`, its follow-up
  `763a264a` was re-checked, and the final wording commits were reviewed by a
  reviewer.
- **Required gates.** `task gate:agent`, then `task gate:repo`, are run in the
  foreground on the frozen candidate that contains this record, after validation.
  Their recorded results are reported in the run's final validation report (the
  verification list of the pull request summary), not here, so the record does not
  change after the gates have run.
