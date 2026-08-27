---
slug: feature-bingo
name: Feature bingo
description: Weekly brainstorm that proposes one concrete new feature and opens an MR adding it as an idea file.
target: prompt
cron: 0 3 * * 2
timezone: UTC
model: fable
---

Brainstorm ONE concrete, genuinely useful new feature or improvement for this
project. Ground it in what the codebase actually does: name the problem it solves,
sketch how it would work, and note roughly where it would live and what it would
touch. Aim for something a maintainer could pick up and scope, not a vague wish.

First read the existing idea files under the `ideas/` folder at the repository
root (if the folder exists) so you do not repeat one already recorded, and check
the codebase so you do not propose something that already exists — pick a
different, non-duplicate idea.

Write your proposal to a single new idea file under the `ideas/` folder — create
the `ideas/` folder if it does not exist yet — using a short descriptive filename
(for example `ideas/<slug>.md`). Put the whole proposal in that one file, commit
it, and open a merge request titled `bingo: <feature>`. That idea file is the only
change this run makes — do NOT modify any other code.

If nothing worthwhile comes to mind this week, make no change and open no merge
request: leave a short note explaining why instead.
