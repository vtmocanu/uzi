# PRD #4811: Eval draft PRs and CI evidence

**Status**: Draft  
**Priority**: Normal  
**Umbrella**: #4808  
**Blocked by**: #4809

## Problem

An owner reviewing a trial may prefer the forge's diff UI and existing checks.
Publishing drafts creates durable artifacts but also triggers repository automation
and exposes candidate changes to the repository audience.

## Outcome

1. A GitHub repo owner creates an eval on a verified draft-capable connection.
   The summary shows Draft PRs, the target repo, maximum PR count, and that
   publication triggers CI and installed apps and bots. The owner confirms that
   revision. After every trial is terminal, the report shows draft links tied
   to the verified trial trees. Selecting `--publish local` skips publication.
   On an unsupported forge even `--publish draft-pr` resolves to local, with
   the reason shown in the summary before confirmation.
2. GitHub returns a green check with the expected app ID and name, on the published
   head, associated with this PR. The published and observed target base match.
   The report shows “CI on live target” as passed. A check associated only with
   another trial's PR, or evidence collected after the base moves, is ungraded.
3. At the end of the review window, uzi closes its unchanged draft and deletes its
   exact ref. An owner who moved another eval branch before cleanup sees a manual
   cleanup record, and the changed branch survives.

## Out of scope

Web UI (#4814), draft publication on unverified forges, workflow and third-party
bot configuration, merge or ready-for-review automation, checkpoint publication,
and hidden tests.

## Modules and seams

- Resolve publication mode on create. On a verified draft-capable connection,
  default to `draft_pr`, or `local` if the owner opted the repo out. Accept
  `--publish local|draft-pr` as an explicit per-eval override of that default.
  With unsupported draft capability, resolve to `local` and show the reason in
  the admission summary. Treat this as an ordinary local eval. Expose repo opt-out
  through `uzi eval publication get|configure --repo <id>` and owner-scoped API
  settings. Match policies by immutable repo ID and use the existing forge scope.
- Add `eval_publications` and `eval_trial_checks`. Freeze the effective mode,
  publication policy revision, target repo and ref/base, expected checks, and
  review lifetime. The admission summary states the target repo, maximum draft
  count, repository audience, and that publication triggers CI and installed apps
  and bots. Confirmation consents to those effects for that revision. Policy
  changes before confirmation require a refreshed summary. Recheck owner,
  connection, and publication authority before each write. If the confirmed
  authority has been withdrawn, stop publication and retain only custody-checked
  cleanup authority. Keep the confirmed local mode fixed.
- Only the API publishes. A's frozen artifact carries a stored thin pack and
  neutral commit whose parent is `base_sha` and tree is `tree_sha`. Bind each
  publication intent to repository, trial, artifact digest, neutral commit and a
  reserved opaque branch under `refs/heads/uzi/eval/<eval-id>/<opaque>`. Before
  any write, verify pack bounds and integrity, unique neutral commit and tree
  identity, and reject unrelated packed objects or unresolved prerequisites.
  Extend `pushbroker.validManagedRef` narrowly for the eval namespace and expose
  the zero-old-tip packed create through `createRefWithPack`. The API lends its
  existing repo credential only to this broker operation and the draft driver.
  Workers keep an empty `forge_pat` and never write to the forge. Keep both the
  original and published SHA. Remote checkpoints stay disabled in every mode.
- Before creating the ref, run the existing publication and secret safeguards
  and require the broker's `ciConfigGuard` in fail-closed mode. Compare both the
  candidate's pinned-base delta and its difference from the live destination at
  protected CI configuration paths, reusing the guard's path matching and tree
  comparison. A changed path, missing comparison evidence or guard refusal
  keeps the trial local with `ci_config_changed`. Do not rebase the neutral commit
  to make it publishable.
- Add `CreateDraftMergeRequest`, `CloseMergeRequest`, and a bounded check listing
  with app identity and PR associations to `forge.CodeHost`. Update GitHub,
  GitLab, Forgejo and `forgetest.BaseFake`. GitLab and Forgejo return
  `ErrUnsupported` and resolve to local before any ref is written. GitHub's
  create API accepts `draft: true`; use a fixed title and body with “Evaluation
  draft. Do not merge.”, an opaque publication marker, and no close directives,
  model identity or issue links. A definitive draft refusal settles local with
  `draft_unavailable`. Never retry as a ready PR. Reconcile an ambiguous create
  response before retrying or cleaning up its branch. Where a ref was already
  written for a refused draft, prove no PR exists (the refusal was definitive
  and a list of PRs by that head ref returns none), then delete the ref with
  `pushbroker.Delete` and `ExpectedOldTip` equal to the neutral SHA.
  [GitHub pull request endpoints](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request)
- Use `ListLabels` before applying the existing `uzi-eval` label. If the label
  is absent, skip it and preserve the draft marker. The eval label path must
  never create a repository label, including on a retry if the label disappeared.
  External automation that acts on creation can identify the reserved branch.
- Publish the whole eligible eval together once every trial is terminal. Reserve
  all its required repo capacity and write its per-trial intents in one
  transaction before the first forge call. The batch does not wait for one
  draft's checks to finish before publishing its other drafts. Forge calls still
  settle per intent. Reconcile lost acknowledgements by exact ref, head, fork
  origin, draft marker and PR identity. A conflict cannot adopt unrelated work
  or allocate a replacement PR. If the whole batch cannot fit, defer it and
  report the capacity reason. Count pending reservations and live drafts under
  the same repo cap. Release an occupied open-draft place only after verified
  closure, or when the intent is proved to have no draft.
- Keep PR ownership in `eval_publications`. Trial rows have no `runs.mr_iid` or
  `runs.issue_iid`. GitHub's `ListIssues` excludes responses with
  `PullRequestLinks`, and board and generic MR watches use issue/MR linkage
  absent here. Preserve the automations' eligibility allowlists
  (`ListCIAutofixCandidateRefs`, `ListMRReworkCandidates` and `JudgeEligible`)
  and reject reserved eval refs at the shared CI-fix and MR-rework service gates.
- Label check evidence “CI on live target”. Match expected app ID, exact check
  name, and `head_sha` equal to the published commit. Independently verify the
  record's association with this specific PR using forge metadata. Distinct
  trial refs can share a neutral SHA. Capture the PR's `base.sha` at publish and
  evidence collection, and require the same base ref, base SHA, head and PR on
  readback. Changed head/base, missing or ambiguous association, unexpected app,
  skipped and absent checks are ungraded. Reuse the GitHub check-run listing
  pattern in `listCheckRunsForRef`, keeping per-check identity. This records
  evidence observed on the live target, with no claim about the tested merge
  SHA. The base SHA is recorded at publication and at every poll; any change
  makes the evidence `no_evidence`. A move that returns to the same SHA between
  two polls cannot be excluded, a residual the report states. The integration does not create a pinned target branch.
  [GitHub check-run fields](https://docs.github.com/en/rest/checks/runs)
- Resource bounds: at most six retained open drafts per repo, including pending
  reservations, and at most six drafts awaiting CI per repo. No separate
  instance-wide slot. Cap configured checks at 16 per repo and each poll at
  three pages of 100 results. Back off from 15 seconds to two minutes with
  jitter, ending after 60 minutes per draft. Poll timeout ends evidence
  collection and does not release retained draft capacity. Review window is
  seven days.
- At expiry or authorized cancellation, re-read the recorded PR, source and
  target coordinates, draft state, and ref before uzi closes it. A moved ref,
  changed PR, undrafted or merged PR, or ambiguous readback records manual
  cleanup without further mutation. A matching draft is closed by the API;
  reconcile ambiguous close responses and verify closure before calling
  `pushbroker.Delete` with `ExpectedOldTip` equal to the neutral SHA. A branch
  change after re-read is protected at deletion. GitHub close follows the
  ownership re-read, so a concurrent PR state change between read and close
  remains a residual race.

## Testing decisions

Use fake forge responses and a local bare repository. Test packed creation,
refusal before send for a changed CI path or invalid pack, idempotency across lost
responses, cross-owner and early-publication refusal, and no auto-reactor or board
admission. Test both mode overrides, repo opt-out, unsupported and refused drafts,
and revision-bound publication disclosure. Exercise exact check attribution,
including equal heads across two distinct PRs and changes to the live base.

## Milestones

### M1. Review frozen trials in draft PRs

**Blocked by** #4809.

Ship repo opt-out, the per-eval override, revision-bound consent, API publication
and CLI report links, the retained cap, and custody-checked cleanup.
Acceptance: examples 1 and 3. A batch starts only after all trials settle and only
if the whole reservation fits. The API publishes A's neutral tree, and neither
mode publishes remote checkpoints. Simulate a draft refusal, lost create or close
response, and a human branch update during cleanup. Document use in `docs/evals.md`,
run `task docs:sync`, and add a shipping changelog fragment.

### M2. Attach CI evidence on the live target

**Blocked by** M1.

Configure expected checks through the owner CLI/API and report per-draft evidence
coordinates and status. Evidence remains independent of any model grade or
implementation verdict.
Acceptance: example 2. Run two repos through the fake check seam. Verify whole-eval
admission, the six-draft repo bound, the 60-minute per-draft deadline, and restart
reconciliation. Extend `docs/evals.md` and run `task docs:sync`.

## Landing obligations

- API integration and repository checks on exact-head PR CI.

## Decision Log

- Default to drafts once the forge capability is verified, starting with GitHub.
  Normal uzi work already opens PRs in the same repo. Drafts give owners the same
  review surface, while the admission summary, caps and opt-out address the
  additional CI and bot load. Unsupported capability resolves to local.
- Publication uses one neutral commit so reviewer-facing history cannot echo SDK
  trailers (the SDK attribution settings in `agent/src/protocol.ts`). Graders consume the original anonymous
  patch and never need PR access.
- Store publication and CI evidence independently of the trial's settlement.
  Neither CI nor cleanup gets authority to change a frozen artifact.
- CI runs on the live target. A private target branch would add a forge write and
  change which existing workflows can run. One batch lets owners review the
  whole comparison while the repo cap bounds retained drafts.
