---
paths:
  - "prds/**"
---

# PRDs

Loaded when you touch a PRD file. The repo-wide map is the root `CLAUDE.md`; PRD
lifecycle (active in `prds/`, completed in `prds/done/`, Decision Logs, ADR
promotion) lives there under *Conventions*.

## 🔴 A PRD sent to uzi MUST NOT change `.github/workflows/**`

uzi's worker pushes with a `repo`-scoped bot PAT that **deliberately lacks the
`workflow` scope** — a worker that could rewrite CI is a supply-chain risk, so this
is by design, not a bug to "fix" by widening the token. GitHub enforces the scope
at the **push**, and the push is **atomic**: if the branch diff touches even one
file under `.github/workflows/`, the *entire* push is rejected (`refusing to allow
a Personal Access Token to create or update workflow … without workflow scope`) and
**every commit on the branch is lost and unrecoverable** — the worker container is
gone and nothing lands on the remote. Issue #377 makes that failure *graceful* (it
detects the case pre-push, fails early with a typed reason, and preserves the diff
for a human to land) but does **not** remove it.

So when you author a PRD that will go to uzi, scope it so that **neither its
implementation nor its validation** ever creates, modifies, or commits a real file
under `.github/workflows/`. If the change genuinely needs a workflow-file edit,
handle it one of two ways and keep the uzi-bound PRD self-contained without it:

- **Split it out.** File a **separate issue** for just the workflow piece,
  cross-link it with the PRD (each references the other), and tag it with a label
  that marks it **maintainer / local-only** — it must be done in a local session
  with a `workflow`-scoped token (your own `gh`), never sent to uzi.
- **Or do it yourself, same session.** If the workflow edit is small, make it
  locally on a branch → PR → merge with your own scoped token, and leave it out of
  the PRD entirely.

**The trap that has already cost us (a #377 run lost 9 clean commits to it):** a PRD
whose *implementation* is workflow-free but whose *validation* writes a **real**
workflow file to exercise CI-related behavior. Validate such behavior with
synthetic / in-memory fixtures (stub the change-detection; return workflow paths as
string data), never a real file on disk under `.github/workflows/`, not even one you
intend to revert (an imperfect revert still leaves the path in the finalize diff).
The safe invariant: `.github/workflows/**` never appears in a uzi run's branch diff
— before finalize, `git diff --name-only <base>..HEAD` must show **zero** entries
there.

See the `uzi-watcher` skill's *The workflow-scope guardrail* section and its
plan-trap check for the sending and merging side of this: the maintainer applies any
deferred workflow edits locally, with a workflow-scoped token, after the PRD's MR
merges.
