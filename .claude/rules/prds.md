---
paths:
  - "prds/**"
---

# PRDs

Loaded when you touch a PRD file. The repo-wide map, and PRD lifecycle (active in
`prds/`, completed in `prds/done/`, Decision Logs, ADR promotion), are in the root
`CLAUDE.md` under *Conventions*.

## A PRD sent to uzi must not change `.github/workflows/**`

- The worker pushes with a `repo`-scoped bot PAT that deliberately lacks the `workflow` scope; a worker that could rewrite CI is a supply-chain risk. Do not widen the token.
- The push is atomic: one file under `.github/workflows/` in the branch diff rejects the *entire* push (`refusing to allow a Personal Access Token to create or update workflow …`) and nothing lands on the remote.
- Commits are not lost on a hosted (k8s) worker: they survive in the worker PVC at `refs/uzi-runner/agent/issue-N`, recoverable per the `uzi-watcher` skill's *Recovering a failed run's work from the worker PVC*. Issue #377 makes that failure graceful (pre-push detection, typed reason, diff preserved), not absent.
- Scope a uzi-bound PRD so **neither its implementation nor its validation** creates, modifies, or commits a real file under `.github/workflows/`.
- A change that genuinely needs a workflow edit stays out of the PRD. File a separate maintainer / local-only issue for it, cross-linked with the PRD and done in a local session with your own `workflow`-scoped `gh` token, never sent to uzi; or make the edit yourself on a local branch, PR and merge.
- Validate CI-related behavior with synthetic or in-memory fixtures: stub the change detection, return workflow paths as string data. Never a real file on disk, not even one you intend to revert, since an imperfect revert still leaves the path in the finalize diff. The trap is a workflow-free implementation whose *validation* writes one; a #377 run lost 9 clean commits to exactly that.
- Invariant: before finalize, `git diff --name-only <base>..HEAD` shows zero entries under `.github/workflows/`.
- The sending and merging side is the `uzi-watcher` skill's *The workflow-scope guardrail* section and its plan-trap check; the maintainer applies deferred workflow edits locally, with a workflow-scoped token, after the PRD's MR merges.
- A branch merely *behind* `main` on those files is a distinct case needing no authoring change: GitHub gates the push on the pushed tip's tree, not on what the branch's own commits changed. uzi realigns at finalize (GitHub-only, merge first with rebase fallback, only when the workflow trees differ) per [ADR-456](../../adr/0456-rebase-before-finalize-push.md), failing cleanly with the diff preserved if that conflicts.

## A PRD whose tests need secret-shaped strings must assemble them at runtime

- GitHub Push Protection scans **every commit in a push** against provider token patterns (GitLab `glpat-` + 20 chars, GitHub `ghp_` / `github_pat_`, Slack `xox…`) and refuses the whole push: `GH013 … Push cannot contain secrets`.
- The worker's gate cannot see it: a PRD names its component gate (`task gate:api`), but `task scan:secrets` (gitleaks over every tracked file, with a canary) lives in `gate:repo`, so a run can be gate-green through every milestone and still be unpushable at finalize.
- A fix on top does not help, because every commit in the range is scanned and a later commit that removes the literal still ships the one that introduced it. Fold the fix into the introducing commit before the first push (a `--soft` reset and re-commit; nothing was on the remote).

When a PRD touches `secretscrub`, `snapshotSecretPatterns`, a redaction test, or any
test feeding a credential-shaped value through a scrubber:

1. **Build the fixture from parts**, never as one literal: `const fake = "glpat-" + "notAReal" + "0123456789"` (or `strings.Repeat`), with a comment saying why. The scrub sees the joined value at runtime; the source never carries a token shape.
2. **Put `task scan:secrets` in the PRD's per-milestone gate line**, beside the component gate, and require the canaries-detected line in the run's evidence.
3. **`//gitleaks:allow` with a written justification** is the in-repo precedent for a literal that must stay literal (`slacksvc/chatactions_test.go`), and it silences only gitleaks. Push Protection has no in-file allow directive, only an out-of-band bypass URL, so runtime assembly is the only form that satisfies both scanners.
