# Agent team workflow for uzi

Generated 2026-07-03 by the `agent-team` skill (roster adapted from the example-app team).

## Team roster

| Role | Subagent type | Model | Tools |
|------|---------------|-------|-------|
| architect | architect | opus | Bash, Read, Grep, Glob, WebFetch, WebSearch, Edit, Write + team tools |
| coder | coder | opus | (inherit) |
| reviewer | reviewer | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| auditor | auditor | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| tester | tester | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| spec-keeper | spec-keeper | opus | Bash, Read, Grep, Glob, Edit, Write + team tools |
| fact-checker | fact-checker | opus | Bash, Read, Grep, Glob, WebFetch, WebSearch + team tools |
| documenter | documenter | sonnet | Bash, Read, Grep, Glob, Edit, Write, WebFetch + team tools |
| web-ux | web-ux | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| researcher | researcher | opus | Bash, Read, Grep, Glob, WebFetch, WebSearch + team tools |
| release | release | sonnet | Bash, Read, Grep, Glob + team tools |

## Orchestrator workflow

You (the team lead) NEVER do implementation, review, or audit work yourself.
You coordinate the team via Agent (name + subagent_type) + SendMessage +
the Task* tools.

Default flow for a typical task:
0. For a non-trivial task (new component, cross-cutting change, new or changed
   contract/interface), dispatch architect BEFORE the coder and fold its design
   summary into the coder's spawn prompt; skip it for small fixes. Also dispatch
   it whenever a PRD is being written or reviewed (including `/prd-create`
   flows): it contributes the architecture sections and the milestone
   dependency graph when writing, and judges feasibility, hidden milestone
   coupling, and independent shippability when reviewing. Open design questions
   it flags go to the user, not to the coder as guesses.
1. Spawn coder with the full task context. The coder runs the project's
   test/lint gates before reporting done: `cd api && go test ./...`;
   `cd web && npm test && npm run typecheck`;
   `cd agent && npm test && npm run typecheck` (plus `./e2e/run-e2e.sh` +
   `./scripts/smoke.sh` for stack-level changes).
2. After coder reports done, spawn reviewer + auditor IN PARALLEL with
   coder's diff + report (pin to commit SHAs). Dispatch fact-checker in the
   same wave when the change touches claim-bearing artifacts (README,
   plan.md, specs, "we beat inspiration X" claims); REFUTED claims are
   blocking. Add architect to this wave for an architectural-fit pass when the
   change moved boundaries.
3. Dispatch tester on the scenario surface when behavior changed.
4. Resolve any blocking findings (route them back to coder via SendMessage).
5. Once blocking findings are resolved, dispatch spec-keeper with the change
   summary plus a user-vs-AI provenance breakdown (lead work — only the lead
   has seen the conversation). specs/ai.md applies directly; specs/human.md
   edits go to the user for confirmation first.

## Context handoff (CRITICAL)

Every teammate cold-starts with no memory of prior conversation or other
teammates' outputs. Whatever you write in the spawn `prompt:` is the entire
context they have, plus the body of `.claude/agents/<role>.md`.

Therefore every spawn prompt MUST include:
- File paths the teammate should read (the spec, the files being modified)
- A summary of any prior teammate's findings when chaining workers
- The exact error message when retrying after a failure
- If context is long, write it to `.claude/agent-team-tasks/<slug>.md` and
  reference that path in the prompt instead of pasting inline

## Re-derive the claim at the moment you assert it (CRITICAL)

**Rule: re-derive a claim from the code at the moment you assert it, however
sure you are.** Having verified something once is not knowing it — you verified
a *past* state, and you assert in the present. This applies to every role.

**A comment is an assertion, so it deserves the same mutation as a test.**
Freeze the field, drop the line, move the path, and watch the assertion fail.
If nothing fails, the comment is describing a mechanism that is not there.

Earned the hard way on PRD #58 (2026-07-16), where **nine claims fell over**, all
believed by someone competent, each disproved in seconds once someone ran it:

- The PRD said quota enforcement was atomic. Measured: with the lock removed,
  **8 of 8 concurrent provisions passed a quota of 2**. Its stated mechanism was
  a guarded insert; the real one is the advisory lock.
- The design said one test caught a misplaced lock. Mutation: it stays **green**
  — a misplaced lock still blocks, so a blocking-assertion cannot see placement.
- The design said only a browser could prove a gate escapable. A page-level test
  does it; the blindness was the *component's*, not the boundary's.
- Three code comments named mechanisms the code did not have (a `seq` nothing
  read; a live region "covering" users it cannot; a `?raw` benefit whose
  corollary was omitted). **The logic was right every time — the story was wrong.**
- A test-count baseline was carried from memory (641 vs the real 612).
- A handoff note outlived the fix that killed it, and was reported as open twice.
- A browser pass "verified" a `title` attribute that reaches **no** screen-reader
  user — it checked *presence*, not *efficacy*.

The root, from the coder that made four of them: *"I trusted any claim I had
personally verified once, and stopped re-checking it, because having checked it
felt like knowing it."*

**Where it hides: the artifacts with no gate on them.** Comments get read in
review, tests get run, commit messages get diffed. A "still open" list, a
checkpoint, a handoff note — that is prose nobody executes, and it is what
decides where the next person spends their time. Re-derive those too.

**Corollaries worth knowing:**
- **Presence ≠ efficacy.** "The attribute is there" and "it reaches anyone" are
  different claims. Two validators can both be right and disagree, because they
  asked different questions. When two reports conflict, find the two questions
  before picking a winner.
- **The experiment that justifies a choice usually also bounds it.** Record both
  halves, not the flattering one.

### Traps in this repo that cost real time

- **`expect(document.activeElement?.textContent).toMatch(...)` is vacuous** — on
  `<body>`, `textContent` is the whole page, so it matches anything. Assert
  **identity** (`toBe(el)`), and cross-check text: identity alone gives false
  *negatives* when a selector drifts, text alone gives false *positives*. **The
  disagreement between them is the signal.**
- **`web/` has two `role="status"` regions** — `RateLimitAnnouncer` (app-wide,
  always-present, empty) comes first in the DOM, so any `querySelector("[role=status]")`
  silently grabs the wrong one. Selector-by-role here is ambiguous by construction.
- **A green Go suite can mean nothing ran.** Every `*LiveDB` test skips without
  `UZI_TEST_DATABASE_URL` and the package still prints `ok` — **51 of them were
  skipping in CI, silently, since they were written.** Check tests *ran*, not
  just passed. `test:api-store-it` now fails on zero-passed or any-skipped for
  exactly this reason.

## Inspiration-first rule

Before implementing something, check the submodules under `inspiration/`
(bottega, multica, dot-agent-deck) for prior art. Match the better
implementation, and beat it where we can. Reviewer and fact-checker
cross-check our work against these; verify "we do it better than X" claims
against the actual submodule code, not from memory.

## Quality gates

Paste this block into every tester, reviewer and auditor dispatch — teammates
cold-start and never read this file, so a slot you do not paste is a slot they
cannot run. A `none (gap)` slot that has been raised once gets a `noted` marker
appended here; roles report a gap only when its line carries no marker.

```
format         none (gap)          # gofmt -l ./api reports 26 drifted files
lint           none (gap)          # no golangci-lint, no eslint; go vet in CI only
typecheck      cd web && npm run typecheck
               cd agent && npm run typecheck
test           cd api && go test ./...
               cd controller && go test ./...
               cd web && npm test
               cd agent && npm test
               cd web && npm run check-docs
dead code      none (gap)
coverage       none (gap)
security scan  none (gap, noted 2026-07-21)
pre-commit     none (gap)
long-running   ./e2e/run-e2e.sh    # ~30 min; overrides the tester's 5-min bound
```

Every gap above is what PRD #103 exists to close; re-derive this block when its
milestones land rather than trusting these lines.

## Project signals

- Test commands (see CLAUDE.md for detail): `cd api && go test ./...`;
  `cd web && npm test && npm run typecheck`; `cd agent && npm test && npm run typecheck`;
  integration: `./e2e/run-e2e.sh` (isolated stack, dummy creds) and
  `./scripts/smoke.sh` (needs a fresh stack). Never bare `docker compose up`
  for testing — `--env-file` with dummy secrets + unique `-p` project.
- Release flow: tag-driven (PRD #52). `v*` tags publish the api/web images +
  the OCI Helm chart to Harbor (Model B: chart `version`/`appVersion` == the
  tag); k8s deploy is GitOps via ArgoCD to dev-cluster (see `deploy/` +
  `deploy/README.md`)
- Spec dir: `specs/` (`human.md` = user contract, edits need user approval;
  `ai.md` = AI design decisions)
- Authoring rules: `CLAUDE.md` at the repo root (commands, architecture map,
  conventions); plan.md is the working plan
- CI: real (`.gitlab-ci.yml`, PRD #52) — validate/test across api/web/agent +
  `helm lint`/`template` + kaniko image validation builds on every MR and
  `main`; `v*` tags additionally publish the images + OCI chart to Harbor. e2e
  is deliberately NOT in CI (it needs docker compose on the runner) — it stays
  the local pre-merge gate. Remote is GitLab (`gitlab.example.com:vtmocanu/uzi`,
  use `glab`, never `gh`/`tea`)
- MVP shape: local laptop demo via docker-compose, PostgreSQL DB, persistent
  storage (per plan.md)
- Inspiration submodules: `inspiration/{bottega,multica,dot-agent-deck}`
- Slash commands the orchestrator may invoke between delegations: none
  project-specific
