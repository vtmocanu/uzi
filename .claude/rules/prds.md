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
**nothing lands on the remote**. On a hosted (k8s) worker the commits are NOT lost:
they survive in the worker PVC at `refs/uzi-runner/agent/issue-N` and are recoverable
per the `uzi-watcher` skill's *Recovering a failed run's work from the worker PVC*
(measured twice: #422 on 2026-08-20, #954 on 2026-09-01; this sentence said "lost
and unrecoverable" until the second one). Issue #377 makes that failure *graceful* (it
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

**This guardrail is about a branch that MODIFIES `.github/workflows/**` — a distinct
case from a branch that is merely BEHIND `main` on those files.** A branch can lose
its whole push even without touching a workflow file itself, if `main`'s workflow
files changed after the run's clone base (GitHub gates the push on the pushed tip's
tree, not on what the branch's own commits changed). That case is now handled
automatically at finalize, GitHub-only: uzi fetches `main`'s current tip and, only
when the workflow trees actually differ, realigns the branch to it (merge first,
rebase fallback) before pushing — no PRD authoring change needed for it. See
[ADR-456](../../adr/0456-rebase-before-finalize-push.md). It still fails cleanly
with the diff preserved if the realignment itself conflicts, so a PRD is never
silently corrupted by it either way.

## 🔴 A PRD whose tests need secret-SHAPED strings must assemble them at runtime, and its gate must include `task scan:secrets`

The **second** push-rejection class, same shape as the workflow-scope one above and
found the same way (a finished run that could not push). **GitHub Push Protection**
scans **every commit in a push** against provider token patterns (GitLab `glpat-` +
20 chars, GitHub `ghp_`/`github_pat_`, Slack `xox…`, and so on) and refuses the whole
push with `GH013 … Push cannot contain secrets`. Two properties make it a PRD
authoring concern rather than a worker bug:

- **The worker's gate cannot see it.** A PRD names its component gate (`task
  gate:api`); the secret scan (`task scan:secrets`, gitleaks over every tracked file
  with a canary) lives in **`gate:repo`**, so a run can be gate-green through every
  milestone and still be unpushable at finalize.
- **A fix on top does not help.** Because every commit in the range is scanned, a
  later commit that removes the literal still ships the commit that introduced it.
  The literal must never be committed: fold the fix into the introducing commit
  before the first push (a `--soft` reset and re-commit; nothing was on the remote).

Measured 2026-09-01, PRD #954: M1 widened the `secretscrub` GitLab pattern to a
`{16,}` body, which forced the slacksvc fixtures from `glpat-x` to a 20-character
token — exactly the shape gitleaks' `gitlab-pat` rule and GitHub's scanner match.
`task scan:secrets` reported 4 findings (`redact_test.go:43,:45`,
`replier_test.go:587,:592`), the push was refused after all three milestones were
done and reviewed, and the work was recovered from the PVC and landed as PR #968.

So, when a PRD touches `secretscrub`, `snapshotSecretPatterns`, a redaction test, or
any test that feeds a credential-shaped value through a scrubber:

1. **Build the fixture from parts**, never as one literal: `const fake = "glpat-" +
   "notAReal" + "0123456789"` (or `strings.Repeat`). The scrub sees the joined value
   at runtime; the source never carries a token shape. Say why in a comment beside
   it.
2. **Put `task scan:secrets` in the PRD's per-milestone gate line**, next to the
   component gate, and require the canaries-detected line in the run's evidence.
3. **`//gitleaks:allow` with a written justification** is the in-repo precedent for a
   literal that must stay literal (`slacksvc/chatactions_test.go`), and it silences
   only gitleaks. GitHub Push Protection has no in-file allow directive (only an
   out-of-band bypass URL), so for any shape GitHub recognises, runtime assembly is
   the only form that satisfies both scanners.
