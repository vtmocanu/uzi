# Agent team workflow for uzi

Generated 2026-07-03 by the `agent-team` skill (roster adapted from the example-app team).

## Team roster

| Role | Subagent type | Model | Tools |
|------|---------------|-------|-------|
| coder | coder | opus | (inherit) |
| reviewer | reviewer | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| auditor | auditor | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| tester | tester | opus | Bash, Read, Grep, Glob, WebFetch + team tools |
| spec-keeper | spec-keeper | opus | Bash, Read, Grep, Glob, Edit, Write + team tools |
| fact-checker | fact-checker | opus | Bash, Read, Grep, Glob, WebFetch, WebSearch + team tools |
| documenter | documenter | sonnet | Bash, Read, Grep, Glob, Edit, Write, WebFetch + team tools |
| web-ux | web-ux | opus | Bash, Read, Grep, Glob, WebFetch + team tools |

## Orchestrator workflow

You (the team lead) NEVER do implementation, review, or audit work yourself.
You coordinate the team via Agent (name + subagent_type) + SendMessage +
the Task* tools.

Default flow for a typical task:
1. Spawn coder with the full task context. The coder runs the project's
   test/lint gate before reporting done (TBD — no gate exists yet; for now
   that means a `docker compose up` smoke once the stack lands).
2. After coder reports done, spawn reviewer + auditor IN PARALLEL with
   coder's diff + report (pin to commit SHAs). Dispatch fact-checker in the
   same wave when the change touches claim-bearing artifacts (README,
   plan.md, specs, "we beat inspiration X" claims); REFUTED claims are
   blocking.
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

## Inspiration-first rule

Before implementing something, check the submodules under `inspiration/`
(bottega, multica, dot-agent-deck) for prior art. Match the better
implementation, and beat it where we can. Reviewer and fact-checker
cross-check our work against these; verify "we do it better than X" claims
against the actual submodule code, not from memory.

## Project signals

- Test commands (see CLAUDE.md for detail): `cd api && go test ./...`;
  `cd web && npm test && npm run typecheck`; `cd agent && npm test && npm run typecheck`;
  integration: `./e2e/run-e2e.sh` (isolated stack, dummy creds) and
  `./scripts/smoke.sh` (needs a fresh stack). Never bare `docker compose up`
  for testing — `--env-file` with dummy secrets + unique `-p` project.
- Lint command: none dedicated; `npm run build` in web/ runs the
  check-docs + tsc gate
- Release flow: none
- Spec dir: `specs/` (`human.md` = user contract, edits need user approval;
  `ai.md` = AI design decisions)
- Authoring rules: `CLAUDE.md` at the repo root (commands, architecture map,
  conventions); plan.md is the working plan
- CI: none; remote is GitLab (`gitlab.example.com:vtmocanu/uzi`, use
  `glab`, never `gh`/`tea`)
- MVP shape: local laptop demo via docker-compose, PostgreSQL DB, persistent
  storage (per plan.md)
- Inspiration submodules: `inspiration/{bottega,multica,dot-agent-deck}`
- Slash commands the orchestrator may invoke between delegations: none
  project-specific
