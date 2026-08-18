---
title: FAQ
order: 108
audience: user
---

# FAQ

Quick, plain-language answers to the questions new readers ask most.

## What is uzi?

uzi ("Uzinele Întunecate") is an AI dark factory: agents pick up
`PRD`-labeled issues on your connected forge and work them end to end (plan →
approval gate → implement ⇄ review → branch + merge request), never touching
`main`.

## Which forges does uzi support?

GitLab, GitHub, and Forgejo/Gitea — one forge driver each. You connect one
forge per account.

## Does uzi ever push to `main`?

No. Four independent guardrail layers keep `main` untouched: a forge
Developer role on a protected branch, the worker (not the agent) holding the
PAT, an SDK deny-hook that blocks `git push` and history rewrites, and
`settingSources: []` so a cloned repo's own `.claude/` is never loaded. Agents
only open merge requests from `agent/*` branches.
