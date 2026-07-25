# PRD #128 — M7 gate: prove `web-ux` drives the uzi web UI from a hosted worker

**Issue**: [#128](https://gitlab.example.com/vtmocanu/uzi/-/issues/128) · **Label**: PRD · **Priority**: Medium
**Exists to close**: [PRD #87](87-prebake-browser-web-ux.md) M7, its headline DoD gate. This PRD is the gate, not a feature — it ships **no code** and closes the moment the run produces its evidence.

## Problem

PRD #87 prebaked chromium into the worker image and shipped the `web-ux` builtin. Its DoD requires "a real worker driving headless `--no-sandbox` Chromium against uzi's own web UI, a legibly-rendered screenshot". That line has never been satisfied by an **agent run**.

The browser *mechanics* were verified operator-side on 2026-07-25 (headless launch `exit=0`, shim `agent-browser 0.32.3`, legible DejaVu glyphs — `prds/mockups/87-m7-font-legibility-2026-07-25.png`). What is missing is the thing only a run can show: that the **agent** reaches the UI through `web-ux` without fighting the tooling — the friction PRD #120 recorded on run `2ebc093e`.

## Scope — deliberately the smallest run that closes the gate

**In scope.** One run, no code change, no MR:
1. Engage `web-ux`; navigate to `http://uzi-web.uzi.svc.cluster.local/`.
2. Capture an **accessibility snapshot** of the landing page.
3. Capture **one screenshot**, and state whether text renders as real glyphs or tofu.
4. Report the page title and the top-level nav items.
5. Report whether Chromium needed any flag rediscovery, or launched clean first try.

**Out of scope.** Any source edit, any MR, any fix to what it finds. Findings about the UI go to a new issue, not to this run.

## Why this is not a throwaway

Item 5 is the part that also feeds **PRD #120**. #120's open question is whether the `2ebc093e` friction was a real `web-ux` defect or **deploy lag** onto a mid-upgrade worker. A clean launch on a worker known to carry the fix is direct evidence for its Hypothesis A; a repeat of the friction falsifies it and makes #120 urgent. Either outcome is worth the run.

## Success criteria

- The run completes without a `tool provisioning failed` (it exercises tier-2 `ruby@3.3` on the way, per PRD #123 — a provisioning failure here is a #123 finding, not a #87 one, and must be recorded as such rather than read as an M7 failure).
- An a11y snapshot and a screenshot both exist in the run's output.
- Text in the screenshot is legible — real glyphs, not tofu.
- The tool-call count spent on browser setup is recorded, whatever it is.

## Milestones

- [ ] **M1 — Run it and record the evidence.** Create the run against `vtmocanu/uzi`, approve the plan gate, capture the a11y snapshot, screenshot, and browser-setup tool-call count into PRD #87's M7 section and (for item 5) PRD #120.
- [ ] **M2 — Close out.** Tick #87's M7 line with the evidence; close this PRD and its issue. If the run surfaced UI or `web-ux` defects, file them separately — they do not belong to this gate.
