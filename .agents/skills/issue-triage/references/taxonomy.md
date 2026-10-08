# Issue taxonomy

Every open issue carries one `area::*` and one `priority::*` label when the evidence supports them.
Leave either off when unsure: a missing area or priority means categorization is incomplete, never "low".
Assigning a new area or priority removes the superseded one; other labels stay.
GitHub label descriptions mirror the one-liners below; keep both in sync.

## Area: the maintainer's decision domain (one primary)

| Label | Scope |
|---|---|
| `area::runtime` | run lane: worker, runner, Claude and Codex harnesses, SDK, prompts, plan/implement/review loop |
| `area::recovery` | work preservation: custody holds, captures, parks, checkpoints, worker_lost, honest terminal states |
| `area::security` | trust boundaries, credentials, sandboxing, egress, untrusted input |
| `area::forge` | forge drivers, sync, labels, MR/PR behaviour, mr_rework |
| `area::interfaces` | web, TUI, CLI, Slack, in-app docs, external products API |
| `area::platform` | k8s, chart, controller, dind, capacity, disk, cost |
| `area::quality` | tests, flakes, CI, e2e, gates |
| `area::release` | release train, changelog, publish, worker-image pins |
| `area::tooling` | repo skills, Taskfile, scripts, dev environment |

- Pick the domain the decision belongs to, not the directory touched: a Codex park bug is `recovery`, a Codex sandbox escape is `security`.
- `security` stays as a cross-cutting risk flag beside any area.
- Themes (Codex readiness, completion interlock, cross-check) are parent issues with sub-issues, not labels.

## Priority

| Label | When |
|---|---|
| `priority::urgent` | credible ongoing credential exposure, irreversible work or data loss, or a core outage with no workaround |
| `priority::high` | recurrent core-run failure, a broken security or completion guarantee, or a prerequisite blocking the current maintainer goal |
| `priority::normal` | concrete useful improvement, or a contained bug with a workable workaround |
| `priority::low` | speculative capability, cosmetic, or optimization without demonstrated impact |

- Record the reason as one sentence in the triage comment: "High because X happens under Y; workaround Z."
- Within a bucket: prerequisites first, then recurring impact and workaround burden; smaller scope breaks ties.
- Recurrence evidence is a "hit again: run X / PR #N" comment; it warrants reconsidering priority, not raising it automatically.
- Priority never authorizes a run, and readiness is separate: an urgent issue with an open design question needs investigation first.

## Recurring

`recurring`: the same verified root cause observed in at least two distinct incidents, evidence from before filing included.

- A run and its PR are one incident; repeated reports or judge passes over that incident do not count again.
- Whoever records the second incident's "hit again" comment adds the label (uzi-lander, judge-triage, issue-triage).
- Closing ends its backlog presence; reopening keeps the history. A hit after a verified fix is a regression: say so in the comment, then reopen or file fresh.

## Labels with consumers: never rename or repurpose

- `uzi`: run eligibility. `bug`, `Planned` and `on-deck` (ondeck-sweep, [on-deck.md](on-deck.md)): sweep selectors. `refactor`: refactor-sweep selector.
- `In Progress`, `Human Review`: uzi board columns (`api/internal/board/board.go`).
- `Later`, `brainstorm`: this skill's park tiers. `local`, `scheduled`, `acceptance`, `agent-found`, `nightly-e2e*`: workflow markers.
- `reviewed`: root `AGENTS.md` rule. `recurring`: above. `unreviewed`: intake, awaiting first triage.
- `effort::*` counts milestones, not elapsed time.
