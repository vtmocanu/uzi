---
slug: refactor-scout
name: Refactor scout
description: Biweekly propose-only scout that surveys the repo for one high-value structural refactor and opens an MR adding it as a proposal file.
target: prompt
cron: 0 5 1,15 * *
timezone: UTC
model: fable
---

Survey this repository for ONE high-value structural refactor worth proposing — and PROPOSE it, never implement it. This job never changes the code under refactor; its only output is a single proposal file. Propose-only is the point: structural refactors are exactly the category where unattended implementation is riskiest, and the human gate ("real refactor or nitpick?") is the value.

Pick a single candidate from these shapes: duplication with 3+ occurrences of the same responsibility; an oversized file with a natural cohesion seam (a seam, not merely a high line count); dead branches or config the automated gates cannot see; a consistency defect that actively costs. Choose the one with the best impact-to-effort ratio — one proposal per cycle, not the first thing you find.

Dedup before you propose. Read the existing `ideas/refactors/` folder from your clone of the default branch, INCLUDING already-declined proposals together with their decline reasons (a `## Disposition: declined — <reason>` section). Do not re-propose an idea already recorded. Re-proposing a declined idea is allowed ONLY when the evidence has materially changed — the file grew again, the duplication count rose, the constraint behind the decline is gone — and your new proposal must state exactly what changed. There is no time-based expiry: a declined-for-a-reason refactor becomes valid again when the reason no longer holds, not after any number of months.

Evidence discipline — a single scheduled run has no multi-agent verification wave behind it, so the proposal must carry its own rigour: derive every count from a named command you actually ran (for example `git grep -F '<literal>'`) and quote that command; cite every claim as `file:line @ <sha>`; mark each claim verified or plausible. Never assert a count you did not measure.

Apply this triage rubric to your own proposal and file it ONLY if it passes: impact must be >= effort; the change must be behavior-preserving, or any behavior change must be flagged explicitly; a deduplication needs 3+ occurrences of the SAME responsibility (not merely similar-looking code); a file split needs a natural cohesion seam, not a line count. A proposal that fails its own rubric is not filed this cycle — an honest "nothing above the bar this cycle" with no merge request is a valid, expected outcome.

Stay on the propose-only, structural side of the boundary with the self-improvement job: that job reviews the codebase and IMPLEMENTS one small improvement per cycle; this job PROPOSES structural refactors deliberately too big or too risky for a single unattended MR — multi-file dedups, god-file splits, cross-package moves. Do not propose the small self-contained fixes self-improvement already handles.

When you have a proposal that passes the rubric, write it to a single new file `ideas/refactors/YYYY-MM-DD-<slug>.md` (today's date, a short descriptive slug) — create the `ideas/refactors/` folder if it does not exist. Use this skeleton: problem plus evidence; the proposed refactor; an effort/risk estimate; and what the child PRD's acceptance criteria would be. That proposal file is the ONLY change this run makes — do NOT modify any other file and do NOT perform the refactor. Commit it and open a merge request titled `refactor-scout: <slug>`.

If nothing clears the bar this cycle, make no change and open no merge request: leave a short note explaining what you surveyed and why nothing qualified.
