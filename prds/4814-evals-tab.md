# PRD #4814: Evals tab

**Status**: Draft  
**Priority**: Normal  
**Umbrella**: #4808  
**Blocked by**: #4809, #4810, #4811, #4812, #4813

## Problem

Owners can create an eval and inspect its report through the CLI. They need one web
surface that explains what will run, shows progress after reconnect, and lets them
compare the same issue across candidate configurations.

## Outcome

1. An owner selects open issue 42 and two candidates with two trials each. The
   server creates a revisioned draft. Attaching a holdout advances the revision.
   After verification and consent to the refreshed run and publication summary,
   confirm starts exactly four trials. A stale confirmation starts none.
2. The owner reopens a finished eval. The table shows all four trials, both
   reviewer scores, one disagreement, the draft PR and its exact-check status,
   and an ungradable holdout separately from the model review.
3. The owner compares one eval with unknown cost for one candidate. The comparison
   shows intervals and “Insufficient data”, identifies the candidate with unknown
   cost, and shows the composite weights and dropped cost term without a winner.

## Out of scope

New execution, publication, holdout, grading, or statistical rules. C renders the
owner API contracts shipped by its prerequisites. Cross-owner sharing stays out.

## Modules and seams

- Add protected `/evals` and `/evals/:id` routes through the same route table as Runs
  (the route table in `web/src/App.tsx`) and add Evals in Work (`web/src/components/navItems.tsx`).
  The list and details use the existing owner-scoped endpoints, never a second state machine. Map the CLI's draft and confirmation flow to a review
  screen that uses the server's canonical admission summary.
- Use the session and CSRF client (`web/src/lib/api.ts`). The API remains the
  authority for ownership, open issue eligibility, exact base, capability, candidate
  limits, and provider and credential ownership. Show owner-visible IDs and labels
  only. Keep raw credentials out of form payloads and browser persistence.
- Reuse the owner-scoped repo, issue, harness, and model/provider options used by the
  existing product where applicable. Require only candidate fields accepted by A.
  Use B's resolved reviewer options and consent language. Show frozen configuration
  on the detail page (in-run cross-check is always off for trials, per A).
- Snapshot-progress polling can use the existing API client with one in-flight
  request per visible view and a default five-second interval. Abort on route
  change, stop only when the eval is terminal and no publication, CI or holdout
  evidence is still pending, refetch on return to visibility, and back off
  on transient failures. Preserve the last successful snapshot while retrying.
- Use server pagination for the list, with a page size of at most 50. Cap the detail
  at the six trial rows specified by A. Fetch summary fields only. Render issue
  text, model labels and reviewer evidence as escaped text, with long fields behind
  details. Omit patch bytes and any large nested response from the default view.
- Use the run cost formatter (`web/src/lib/costStatus.ts`). Preserve recorded,
  API-equivalent, and unavailable values. Separate implementation from grading cost
  and active, elapsed, and queue durations. Show the limit-wait count and mark
  active time as including limit waits when the report says so. Label endpoint timing as
  dependent on the endpoint hardware where applicable. Link to the owned run when
  there is a valid run ID and the target route supports it.
- Use the server's pass counts, denominators and exclusions (B's mapping, E's
  joined rule, A's infrastructure failures outside the denominator), never a
  client recount. Per-arm counts show passed, failed, ungraded and excluded trials. Show
  whether all trials passed only once each has a grade. Display pairwise preference
  separately, including ties, order disagreement and unavailable evidence. Use E's
  final pass classification when a holdout was attached.
- Show D's per-repo opt-out, expected checks, and the per-eval publication
  override. Show the resolved mode, local fallback reason when applicable, target
  repo, maximum draft count, and CI and installed-app activity before confirmation. On a result, show draft or retained
  link, cleanup status, and, labelled "CI on live target", the check source, the published head and the target base observed at publication and at evidence collection as details. Render
  pending and ungraded check evidence without changing the pass classification.
- Provide E's bundle attach/verify controls on the unstarted draft. Clear upload
  inputs after the request and keep only returned metadata and reason codes. A bundle
  update replaces the revision and voids consent until the latest summary is shown.
  Never persist bundle bytes in browser storage, send them to analytics, or include
  them in a read request. Finished results show separate holdout outcome and reason.
- Add the admin holdout image allowlist page (admin-only) over E's admin API, next
  to the existing admin worker settings.
- Render F's owner-scoped compare query over up to 50 eligible evals. Use its returned
  intervals, sample sizes, exclusions, trade-off set, composite weights and dropped
  terms. Use F's response for insufficient data and unknown cost; avoid client-side
  score recomputation. Offer an accessible table alongside any plot.

### Layout reference

```text
Work / Evals                              Create eval
Repo · Issue                    Status             Updated

Evals / Issue title                       Cancel eval
Frozen issue · Base commit · Status
Candidate A [harness / model]             Candidate B [harness / model]
Pass count, unknown count                Pass count, unknown count

Trial  Status       Active  Elapsed  Cost      Lead   Cross
A1     Complete     04:32   05:01    Estimated Pass   Pass
B1     Ungraded     04:50   06:10    Unknown   N/A    N/A
Pairwise preference                       Reviewer disagreement
```

The form shows repo and issue first, candidate rows next, then reviewer settings,
limits and consent. On narrow screens, preserve the trial labels and header meanings
when the table scrolls. Use semantic labels for status and score, visible focus,
and text alongside color indicators. The prose governs behavior. The layout governs
grouping and reading order.

## Testing decisions

Use the existing route and app-shell fixture style
(`web/src/components/AppShell.test.tsx`) and mocked API responses. Cover deep-link
loading, signed-out access, generic unavailable handling for missing or foreign IDs,
duplicate submit, 409 eligibility changes, cancel conflict, polling through terminal
status, and estimated versus missing cost. Seed a lead pass with a cross-review
disagreement and an ungraded trial. Check keyboard navigation, a narrow viewport,
and the established light and dark themes using the mock build.

## Milestones

### M1. Find and watch evals

**Blocked by** #4809, #4810, #4811, #4812 and #4813.

Add the protected routes, paginated list, frozen detail and cancellation.
Acceptance: an owner opens an active eval from a direct link, reconnects after
a transient error, and cancels it. A foreign ID gets the same unavailable state
as a missing eval.

### M2. Create and confirm a draft

**Blocked by** M1.

Drive the server create, draft revision, and confirmation flow. Configure candidate
and reviewer options, repo publication opt-out and expected checks, publication mode,
and optional holdout attachment and verification. Render canonical limits,
endpoint consent, exposure and the final revision immediately before confirmation.
Acceptance: example 1. Retry an interrupted create with the same idempotency key.
Cover a repository, issue or revision becoming ineligible between create and confirm.
An admin adds and removes approved holdout image digests; a non-admin cannot reach
the page or its API; the owner's holdout form offers only approved digests.

### M3. Compare completed trial evidence

**Blocked by** M1.

Render model grades, per-arm counts, pairwise preferences, PR and CI evidence,
holdout results, and timings including the limit-wait disclosure. Keep pending,
ungraded, failed, and unavailable values visually distinct.
Acceptance: example 2. Check narrow and keyboard navigation, a skipped CI check,
an unavailable cost and an active-time value that includes wait time.

### M4. Render statistics and trade-offs

**Blocked by** M3.

Render F's comparison selector and output, including configurable weights, with an
accessible representation of intervals and trade-offs. Update `docs/evals.md`
for the complete web flow. Run
`task docs:sync`, and add the shipping changelog fragment.
Acceptance: example 3. Compare two compatible eval sets and show exclusions returned
by F. Switching weights displays the updated composite and the weight labels.
The existing in-app Evals guide describes the web flow after sync.

## Landing obligations

- Full `test:web`, `test:web-browser`, `check-docs:web` pending exact-head PR CI.

## Decision Log

- A single list and detail route supports navigation and recovery without inventing
  a second run stream. All controls use the same owner API as the CLI.
- Separate propagation states from grade outcomes. A finished implementation can
  still have no grade, and a missing value must never contribute a zero.
- The comparison shows raw evidence first. Additional outcomes from later PRDs can
  add detail without changing this view's core columns.
