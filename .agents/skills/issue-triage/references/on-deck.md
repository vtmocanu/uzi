# On-deck

The `ondeck-sweep` runs with plan auto-approval by default: nobody reviews the plan before implementation. Catalog defaults: one start per fire, oldest first, only while the owner's unfinished runs are at most 2 (capacity limit 4, room needed 2). After a run starts it removes `on-deck` best-effort; a failed removal keeps the run and may keep the label. Verify `auto_approve`, capacity and removal live with `uzi schedule list --json`; owner edits change them.

An issue fires only with `on-deck` plus eligibility (`uzi` or bot assignment).

## Criteria (all must hold)

1. **Priority**: `priority::low` or `priority::normal`.
2. **Bounded**: one change with one testable outcome. It may span components when the body names every affected site, pins behaviour and compatibility, and names the regression checks. No migration, no new subsystem.
3. **No risk-class exclusion**:
   - auth, secrets, vault, guardrails, worker claim, custody, recovery, pause/park/requeue paths;
   - tests that enforce guardrails or credential isolation, even test-only changes;
   - release guards that could fail open;
   - `.github/workflows/**` (the worker PAT cannot push them);
   - `security` label, "investigate" or diagnosis-first issues, and anything verified only by a live or cluster check.
4. **Fresh**: issue-triage Step 4 ran on current `origin/main`: premise verified, anchors refreshed, conditionals resolved.
5. **Pinned**: every design fork is decided in the body (option and reason); no implementation decision is left for the run or the user.
6. **Testable**: an acceptance section. A bug fix requires a regression test watched failing on current main and passing with the fix. LiveDB fixes name `./e2e/run-store-it.sh`.
7. **Compatible**: a DTO change only as an optional additive field, safe when absent, verified compatible with older producers and consumers, needing no coordinated rollout, and with no change to authorization, validation, persistence or lifecycle semantics.
8. **Dependencies**: a new one only when the body explains why existing tools or a small in-house implementation are insufficient, pins package, version and lockfile change, and names verification. Promoting an existing indirect dependency at the same version is fine.
9. **One lane**: no `bug`/`Planned` or other enabled label-sweep selector (custom and refactor sweeps included; read `uzi schedule list --json`), no bot assignment, no enabled one-shot schedule, no active run, no open PR for the issue.

## Body

Rewrite the body before queuing, in the maintainer's words:

```
## Problem      1-3 sentences; anchors as path:symbol, never line numbers
## Fix          pinned direction per site
## Out of scope what the run must not touch
## Acceptance   testable checks; regression test watched both ways; gate to run
```

- Rewrite a non-maintainer body (bot `agent-found` included) this way; the run plans from the body alone.
- **Split**: when only part of an issue qualifies, file that part as a new issue with this body and "Split from #N", trim the parent body to the remainder, and comment on the parent with the link.

## Review

The buddy reviews the exact final body (pin its hash) and the label set; `reviewed` follows that approval only. A criteria review approves no issue. Queuing also needs the user's authorization: their OK for this batch, or an existing scoped grant. Buddy approval alone does not supply it.

## Drop

Close only for a verified obsolete premise, already-implemented behaviour, a demonstrated duplicate (the survivor covers the rest), an invalid claim, or the user's explicit value decision. "Low value" or "speculative" alone goes to the user.
