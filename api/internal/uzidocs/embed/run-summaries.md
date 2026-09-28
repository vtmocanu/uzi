---
title: Run summaries
order: 39
audience: user
---

# Run summaries

Every run gets two short plain-English summaries, so you can tell what it's
doing without opening the issue or reading the raw plan:

- **Intent summary** — "what this run will implement", compiled from the
  issue title and body (plus a linked PRD, when there is one) shortly after
  the run starts. It shows as a one-line preview on the runs list and as a
  card on the run page.
- **Plan summary + deltas** — "what the proposed plan will do", compiled the
  moment the agent's plan reaches the approval gate. Alongside it, a short
  tagged list of how the plan diverged from the original ask: **added**,
  **changed**, or **dropped**. The card reads **Proposed plan** while the run
  sits at the gate and relabels to **Approved plan** once you approve — the
  same text, not a regenerated one. A revised plan (after **Request
  changes**) regenerates both.

Both summaries are generated on **your own Anthropic token**, on a cheap
model by default, so they cost very little next to the run itself.

## Read the deltas as a heads-up, not a substitute

The deltas are the model's own read of how the plan differs from what was
asked for — a quick way to spot something worth a closer look, not a
guarantee of completeness. The model is summarizing text that came from the
issue, the PRD, and the plan itself, all of which a hostile or careless
issue could shape to hide something (say, a dropped security step). Always
read the actual plan before approving; the deltas are there to draw your eye,
not to replace that read.

## It's advisory — it never blocks your run

Summary generation runs alongside the real work, never in place of it. If it
fails or times out, it's simply skipped: the card falls back to the issue
title and the run proceeds exactly as if summaries didn't exist. A seeded or
pre-approved run (one that skips planning) gets an intent summary only —
there's no plan gate for a plan summary to attach to.

## Collapsing a card

Each card can be collapsed; the choice is remembered **per run, in your
browser**, for up to 7 days. It's expanded by default and isn't synced
across devices.

## Which model it runs on

The instance default is **haiku** — summaries are lightweight and run once
or twice per run, so a fast, cheap model is the right default. From
**Settings → Run defaults → Run summaries**, the **Summary model** picker
lets you override that for your own runs; leave it on **Inherit** to use the
instance default. See [Admin settings](./admin-settings.md#run-summaries) for
the instance-wide setting.

## From the CLI

`uzi run get <id>` prints the same summaries as `INTENT`, `PLAN SUMMARY`, and
one `DELTA` row per change — each only when it exists. See
[the CLI docs](./cli.md#commands).

## Pull request description

Every pull/merge request uzi opens carries a short, plain-English description
of what changed, so a reviewer can tell what the PR does and roughly how big
it is without reading the diff first.

**What the body contains.** A 2-3 sentence summary of what the PR does and
why, a computed size line, a short "What changed" list, a "Verification"
section (only when there's something to report), and "Scope and review
notes" when the delivery differs from the ask, followed by a line saying
which commit it describes and against which branch — omitted when either the
head or the target branch isn't known yet. Below that sits a separate,
deterministic completion block: the issue reference — `Related to
#N.` on its own for a non-closing delivery, `Related to #N.` plus `Closes
#N` together for a closing delivery, or `Implements part of
#N` for a partial delivery. On an interlocked run `Closes #N` is added only
once the PR's head is independently verified, never at creation; a legacy
(non-interlocked) run renders both lines together at creation, with no head
verification — any partial-delivery or unmet-criteria
warnings, and a fixed footer: "Opened by uzi from `<branch>`. A human
reviews and merges; uzi never merges." The summary and the size line are
omitted, not left blank, when there's nothing to show.

**The size line.** A line like:

```
**Size:** code +896 −219 · tests +1,810 −84 · docs +101 −30 · generated +319 −44 · 52 files
```

is always computed from the actual diff (`git diff --numstat`), never
written by a model, and empty buckets are omitted; a bucket with a file in it
but no added and no deleted lines (a binary file, say) still shows as
`+0 −0`. Each changed file is
classified into one bucket, first by your repo's own `.gitattributes`
(`linguist-generated`, `linguist-documentation`, `linguist-vendored`), then by
generic path rules (test files, `docs/`, lockfiles and other generated
artifacts, config files) with anything left over counted as `code`. If your
repo classifies something uzi's generic rules get wrong (generated code that
doesn't match a common pattern, for example), add or adjust a
`.gitattributes` entry for it; the size line has no repo-specific rules
baked in. `uzi run get`'s own `SIZE` row and the run page's "Delivered"
section both print a shorter cut of the same numbers: each omits a bucket
that has a file but no added and no deleted lines, which the PR's own size
line still shows.

**Verification is only what the agent reported.** The Verification section
never claims the CI passed (that's on the PR itself, from the forge) and
never says "tests added" on its own — the size line's `tests` bucket is a
line count, not a claim that anything was actually tested. It lists only the
checks the agent ran and reported, labelled with the commit SHA they were
run at — the run's own worktree `HEAD` at the moment it signalled done, not
necessarily the PR's final head.

**The lead can supply its own claims.** Normally an editor pass writes the
plain-English text from the lead's structured claims and the final diff. If
that pass fails or times out, the description falls back to rendering the
lead's own claims directly (marked "Summary written by the agent, not
checked against the diff"); if neither is available, the region shows only
the size line. The completion block is always written, on every rung.

**Editing the description.** You can edit the text uzi writes, or add your
own notes outside it (a review bot's summary, a filled-in PR template
section), and uzi leaves that text alone on an ordinary refresh. If you edit
inside uzi's own description block, uzi notices on the next refresh (a
`mr_rework` run, for example) and leaves your edit alone instead of
overwriting it; the completion block below it is still kept current.

Text outside uzi's two blocks isn't untouchable, though: uzi rewrites the
**whole** body, region and all, in a few specific cases where preserving text
would let something it doesn't own slip through — a PR with none of uzi's
markers and no previously published description; markers that are malformed
(the publisher skips such a PR rather than write into damaged markers, and
the completion interlock's own reconcile is what then rewrites it whole); a
directive elsewhere in the body that would close the issue on a run whose
delivery must not close it; or, when the PR can't even be read back, a blind
non-closing rewrite as a last resort. A completion block that's gone
missing is different: an ordinary (non-refresh) publication repairs it in place, appending a fresh
completion block and keeping everything else, and the interlock's own
reconcile only rewrites the whole body if the block is *still* missing when
it reads the PR (the publisher's write failed or was stopped, or a human
deleted the block afterwards). A refresh run (`mr_rework`, or a `ci_fix`
picking up an existing branch) never does either kind of whole-body
rewrite, and leaves a missing completion block missing: it only ever touches its own two blocks, or leaves a PR with no
uzi markers untouched entirely.

**Where to see it.** The run page shows a "Delivered" section of the same
card as the intent and plan summaries. `uzi run get <id>` prints the same
summary as a `DELIVERED` row (followed by an `UNCHECKED` row when the summary is
the agent's own, not checked against the diff), the size line as `SIZE`, and, when the PR's
description couldn't be refreshed, a `PR_UPDATE` row explaining why.
