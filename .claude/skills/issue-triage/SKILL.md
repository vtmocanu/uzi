---
name: issue-triage
description: "Triages one GitHub issue on this repo end to end, prioritizing issues that are silently un-sweepable by uzi — they look queued but never fire because they are missing a sweep selector (bug/Planned) or the fireable half (a valid PRD link or PRDLESS), including a PRD label with no prds/*.md link. Hunts those gaps first, then the raw un-triaged backlog, then proposes revisiting parked (brainstorm/Later) issues. Explains in plain English what the issue proposes, recommends whether implementing makes sense, verifies the issue is not stale (premise still holds, file anchors current, any referenced PR or PRD actually merged), then on your confirmation applies the missing sweep labels and posts a freshness comment. Use when triaging the issue backlog, finding issues the nightly sweep will never touch, deciding what to send to uzi, or asked to triage the next issue, go through the open issues, or judge whether an issue is worth doing. Triggers include triage issue, triage the backlog, un-sweepable issues, issues not swept by uzi, next issue to implement, should we do this issue, queue an issue for uzi."
---

# Issue triage

Take ONE un-triaged GitHub issue from backlog to a queued (or parked) decision. One
issue per run. This repo is **GitHub** (`github.com/vtmocanu/uzi`): use `gh` only, never
`glab`/`tea`.

This skill owns the **triage workflow**. It does NOT restate mechanics documented
elsewhere:

- **Sweep gating + this instance's sweep labels/schedules** live in `CLAUDE.local.md`
  → "uzi scheduled sweeps on this instance", plus `docs/scheduling.md` and
  `docs/prdless.md`. **Read those for how a label makes an issue fire**; do not hardcode
  the mechanics here (they are machine/instance-specific and drift). Live source of
  truth for the schedules is `uzi schedule list`.
- **Sending an issue to uzi and driving it to a merged PR** is the **uzi-watcher**
  skill. This skill stops at "queued for the sweep" or "hand to uzi-watcher".

## Step 1 — Pick the issue

If the user named an issue (number/URL), use it. Otherwise hunt in **priority order**,
picking the lowest-numbered issue in the highest non-empty tier.

**Why the tiers.** A sweep fires an issue only when it has BOTH halves: (a) a
**selector** label (`Planned`, or `bug` for a bug) AND (b) it is **fireable** — either
a valid PRD (`PRD` label *and* the body links a `prds/*.md`) or the `PRDLESS` label.
(Authoritative mechanics, which drift: `CLAUDE.local.md` → "uzi scheduled sweeps",
plus `docs/scheduling.md`, `docs/prdless.md`.) An issue missing **either** half looks
queued but silently never runs, and nobody notices — so those are the highest-priority
triage targets, ahead of the raw backlog.

- **Tier 1 — silently un-sweepable gaps** (not parked). Three shapes:
  - **1A selector, no fire** — has `bug`/`Planned` but is not fireable (no `PRDLESS`,
    no valid PRD). The `bug` sweep picks it as a candidate, then the gate drops it.
    (This is #190's shape.)
  - **1B fireable, no selector** — has `PRDLESS` or a valid PRD but no `bug`/`Planned`,
    so it never even becomes a candidate. Common on a fully-PRD'd issue nobody labelled
    `Planned`.
  - **1C `PRD` label, no link** — carries `PRD` but the body links no `prds/*.md`, and
    it has no `PRDLESS`. Inconsistent labelling: won't fire directly and isn't PRDLESS.
- **Tier 2 — un-triaged backlog**: no sweep and no park label at all.
- **Tier 3 — parked** (`brainstorm`/`Later`): propose *revisiting* only when tiers 1
  and 2 are empty — these need a human decision the sweep will never make.

```sh
gh issue list --repo vtmocanu/uzi --state open --json number,title,labels,body --limit 400 \
  | jq -r '
    def park: ["brainstorm","Later","In Progress","Human Review","wontfix","duplicate","invalid"];
    def names: [.labels[].name];
    def has($l): (names | index($l)) != null;
    def selector: (has("bug") or has("Planned"));
    def fireable: (has("PRDLESS") or (has("PRD") and (.body // "" | test("prds/[^\\s)]+\\.md"))));
    def parked: ((names) - park) != (names);
    [ .[]
      | (if parked and (has("brainstorm") or has("Later")) then "3:parked"
         elif parked then empty
         elif (selector and (fireable|not)) then "1A:selector-no-fire"
         elif (fireable and (selector|not)) then "1B:fireable-no-selector"
         elif (has("PRD") and (fireable|not)) then "1C:PRDlabel-no-link"
         elif (selector and fireable) then empty
         else "2:untriaged" end) as $tier
      | select($tier != null)
      | "\($tier)\t#\(.number)\t[\(names|join(","))]\t\(.title[0:64])" ]
    | sort | .[]'
```

The `prds/[^\s)]+\.md` test is the PRD-link detector — do not trust the `PRD` label
alone (that is exactly gap 1C). Confirm the picked number with the user before spending
effort on it. A picked gap issue still runs the full Step 2–4 flow: the gap tells you
which label is *missing*, not that adding it is correct — the issue may be Already done,
Not worth it, or genuinely `Later` (in which case the fix is to make that intent
explicit, not to complete the sweep config).

## Step 2 — Understand and explain

Read the full issue (`gh issue view NNN --repo vtmocanu/uzi --json title,body,labels,comments`).
**Read the existing comments** — a decision or verdict may already be recorded.

Give the user a **short plain-English** summary: what the issue proposes and why, in
one small paragraph, no jargon. Then a one-line "what changes for a user" if it is
user-visible.

## Step 3 — Recommend

State a verdict with a one-line reason. Each verdict has a label action, applied only
after the user confirms (Step 5):

| Verdict | When | Action on confirm |
|---|---|---|
| **Send to sweep** | clear value, self-contained, premise holds, does NOT touch `.github/workflows` | sweep selector (`Planned`, or `bug` for a bug) + `PRDLESS` unless already PRD'd; post freshness comment |
| **Do locally** | tiny/mechanical, or it MUST touch `.github/workflows` (a sweep cannot), or the user wants it now | do it in-session or hand to **uzi-watcher**; no sweep labels |
| **Needs design** | open question or competing approaches | add `brainstorm`; summarize the fork |
| **Defer** | valid but not now | add `Later` |
| **Already done** | premise no longer holds (verify in code) | recommend close, cite the code that already implements it |
| **Not worth it** | duplicate / invalid / out of scope | comment the rationale + `wontfix`/`duplicate`/`invalid` |

Prefer the best-practice choice and say why. Do not author a `prds/*.md` when the issue
body is already a complete spec — prefer `PRDLESS`.

**For a Tier-1 gap issue, the "Send to sweep" action is just completing the missing
half** — add only what the gap lacks, don't blindly re-add both labels:

- **1A** (selector, no fire): add `PRDLESS` (spec in body) or a valid PRD; the `bug`/`Planned` is already there.
- **1B** (fireable, no selector): add the selector only (`Planned`, or `bug` for a bug) — `PRDLESS`/PRD already present.
- **1C** (`PRD` label, no link): the label is wrong. Either the spec is in-body → drop to `PRDLESS`; or a `prds/*.md` exists but is unlinked → link it; or the PRD was never written → decide PRD-vs-PRDLESS per the body. Then add a selector if missing.

If a Tier-1 gap issue is actually deferred on purpose (that is *why* it lacks a
selector), the fix is **Defer** — add `Later` to make the intent explicit so it stops
surfacing as a gap — not to complete the sweep config.

## Step 4 — Freshness check (only if worth implementing)

Before queuing, confirm the issue is not stale. Do NOT trust its line numbers.

1. **Premise still true.** Grep the code the issue targets. If the change is already
   implemented, switch the verdict to **Already done** and cite the code.
2. **Referenced PR/PRD merged.** For a follow-up issue (e.g. "PR #508 review
   follow-up"), confirm the referenced PR/PRD actually merged (`gh pr view NNN --json
   state,mergedAt`) so the findings are live, not superseded.
3. **Refresh anchors.** The issue's `file:line` references drift. Re-grep the named
   symbols, note current locations, and flag any **omitted or extra sites** the issue
   missed (e.g. a fourth scan site it listed three of).
4. **Pin design forks.** If a step embeds a decision — especially one that appears to
   conflict with an existing ADR / PRD Decision — state the direction and why, so the
   worker does not guess. Verify the apparent conflict against the actual code/comment
   rather than the issue's framing.
5. **Workflow-scope guardrail.** If the fix MUST touch `.github/workflows/**`, it
   **cannot** go to a uzi sweep: the worker PAT lacks `workflow` scope, so the whole
   branch push is rejected and the work is lost. Recommend **Do locally**, or split the
   workflow edit into a separate local-only issue. (Cross-ref `.claude/rules/prds.md`
   and the uzi-watcher skill's guardrail.)

## Step 5 — Propose, confirm, apply

Labels and comments are public writes on the repo. **Always propose first and apply
only after the user's OK.** Then:

```sh
gh issue edit NNN --repo vtmocanu/uzi --add-label "SELECTOR" --add-label "PRDLESS"
gh issue comment NNN --repo vtmocanu/uzi --body "$(cat <<'EOF'
Queued for the nightly SELECTOR sweep (SELECTOR + PRDLESS added; spec-in-body).
[anchor refresh + any pinned design direction from Step 4]
EOF
)"
```

The comment captures the Step-4 findings: refreshed anchors, omitted/extra sites, and
any pinned design direction. Mirror the style of the freshness comments on #525/#509.

After queuing, remind the user: the sweep runs unattended past the plan gate
(auto-approve is on for these schedules), but a human still merges the MR and `main` is
never touched. To review the plan before code lands, drive it via **uzi-watcher** (Auto
mode) instead of waiting for the sweep.
