# PRD #603 — Close the driver-hazard gaps in uzi-cli's Send-to-uzi playbook

**Issue**: [#603](https://github.com/vtmocanu/uzi/issues/603)
**Status**: Done (2026-08-22) — all three milestones landed on `agent/issue-603`.
Note: M2 also updated `docs/cli.md` (the parallel `run wait` synopsis + a flag
bullet), which the Technical-scope list did not name — a small consistency addition so
the shipped CLI docs and the skill stay in step.
**Priority**: Medium

## Background: what already ships (and why this PRD shrank)

This PRD was originally scoped to "ship a forge-neutral `uzi-drive` skill so users
get the driver experience our maintainer-local `uzi-watcher` skill gives us." Two
independent PRD reviews, verified against the code, found that premise is wrong: a
forge-neutral, **all-three-forge** driver **already ships** in the `uzi-cli` skill
(`api/internal/uzicli/skill/SKILL.md`, `go:embed`-ed and installed to every user's
`~/.claude/skills/uzi-cli/`). Specifically, its `### Send to uzi (orchestration)`
section already provides:

- Auto / Supervised / Seed & ship / Custom modes, asked once up front.
- The full 9-step Auto loop: gated `run create` → `uzi run wait` to the gate →
  review/approve/revise/reject → `run wait` to MR → get MR URL → review + merge →
  watch CI + fix.
- **All three forges** for the merge (`glab mr merge` / `gh pr merge` /
  `tea pr merge`) and CI (`glab ci status` / `gh run watch` / tea), with a "detect
  the remote host" rule.
- plan-as-untrusted-data, and the poll-`run get`-don't-hold-a-long-wait hazard.

So building a new skill would ship *less* coverage (a GitHub-only tail) than users
already have. Dropped.

**The genuine gap** is three driver hazards that live only in the maintainer-local
`uzi-watcher` skill and are absent from the shipped `uzi-cli` playbook (confirmed by
grep against `SKILL.md`), plus one supporting CLI capability the shipped
orchestration cannot express without a bundled bash script:

1. **revise-by-seq.** The shipped step 5 says "revise, then `uzi run wait` for the
   gate again." But after `uzi run revise`, `run wait --until awaiting_approval` can
   return on the **stale** gate — the run may still read `awaiting_approval` (or
   briefly return there) before the revised plan exists. `watch-run.sh` handles this
   with a `min-plan-seq` argument; `uzi run wait` has no equivalent, so the shipped
   playbook cannot tell a user how to wait for the *revised* plan without the script.
2. **`-m` message shell-injection.** A backtick / `$` / unescaped `!` inside a
   **double-quoted** `-m` message is evaluated by the shell (command substitution),
   silently corrupting what uzi receives; the revise still succeeds, so it is
   invisible until you re-read the plan. Not in `uzi-cli`.
3. **cookie-only route plan-trap.** A plan that adds a new `uzi` CLI command **and**
   a new API route will 401 the CLI at runtime if the route is mounted in the
   cookie-only `RequireAuth` group instead of `RequireUser` (cookie **or** `uzc_`
   Bearer). A pre-approve check catches it. Not in `uzi-cli`.

## Solution

1. **Add `--min-plan-seq N` to `uzi run wait`** so revise-by-seq is enforceable by
   the shipped CLI (no bundled script). Semantics mirror `watch-run.sh`: when
   `awaiting_approval` is in the `--until` set and `--min-plan-seq` is set, the wait
   only stops at `awaiting_approval` once the run has a plan message with `seq > N`;
   terminal states and `awaiting_input` still stop unconditionally.
2. **Fold the three hazards into `uzi-cli`'s `### Send to uzi (orchestration)`
   section**, in forge-neutral wording (all three are forge-agnostic — (1) and (2)
   are `uzi`/shell behavior, (3) is uzi's own API auth groups). Wire (1) to the new
   flag: step 5 says capture the latest plan seq before revising, then
   `uzi run wait <id> --min-plan-seq <seq>`.
3. **Document the new flag** in `uzi-cli`'s `uzi run wait` command-reference entry
   (the skill↔cobra drift test guards command/flag coverage, so an undocumented flag
   or an undefined documented flag reddens `gate:api`).
4. **Trim the now-duplicated hazards in `uzi-watcher` to pointers** at the `uzi-cli`
   section, keeping only its GitHub/admin-merge/workflow-scope/k8s-PVC specifics — so
   the shared hazards have one source of truth and cannot drift.

No new skill, no installer generalization, no new verb.

## Scope decisions (settled at PRD creation, then corrected by review)

- **Not a new `uzi-drive` skill.** The forge-neutral driver already ships in
  `uzi-cli`; a new skill would duplicate it and (as drafted) regress forge coverage.
  A weaker discoverability argument remains (`uzi-cli` is `user-invocable: false`),
  but it does not justify the build and is explicitly out of scope here.
- **Enhance `uzi run wait`, not a new `uzi run watch`.** `run wait` already polls to
  a stop-state and exits (`--until`, `--timeout`); the only missing capability is
  seq-aware stopping, which is a flag, not a new verb.
- **Forge-neutral hazards go in the shipped skill; forge/role-specific procedures
  stay in `uzi-watcher`** (workflow-scope PAT guardrail, admin-merge past branch
  protection, hosted-k8s PVC recovery, red-main diagnosis).

## Technical scope

- **`api/cmd/uzi/run.go`** (`runWait`, inside the `api` Go module → `gate:api`
  affected): add the `--min-plan-seq` int flag and the plan-seq gate on the
  `awaiting_approval` stop. Reuse the existing run-logs read to compute the current
  max plan seq (`kind == "plan"`), matching `watch-run.sh`'s
  `[.[]|select(.kind=="plan")|.seq]|max // 0`.
- **`api/internal/uzicli/skill/SKILL.md`**: the three hazard paragraphs into the
  orchestration section + the flag into the `run wait` reference entry. This is
  `go:embed`-ed, so the edit ships on the next release with no install step.
- **`.claude/skills/uzi-watcher/SKILL.md`**: trim the three now-shared hazards to
  pointers.
- **No changes** to the forge drivers, api routes/DTOs, the controller, the web app,
  or the skill installer.

## Milestones

- [x] **M1 — `uzi run wait --min-plan-seq N`.** Flag + gated `awaiting_approval`
  stop in `runWait`; terminal/`awaiting_input` still stop unconditionally; help text.
  Unit test with a fake client returning a **stale** plan seq first, then a fresh one
  — assert the wait does *not* stop on the stale gate and does stop on the fresh one
  (a positive control that fails if the gate is inverted).
- [x] **M2 — Fold the three hazards into `uzi-cli`'s Send-to-uzi section** (forge-
  neutral), wire revise-by-seq to `--min-plan-seq`, and document the flag in the
  `run wait` reference entry. `gate:api` green (drift test sees the documented flag).
- [x] **M3 — Re-point `uzi-watcher`** at the shared hazards (trim to pointers; keep
  only GitHub/admin/workflow-scope/k8s specifics). (`agnix` is not installed on the
  worker, so it was not re-run; the edit leaves the frontmatter untouched and keeps
  valid Markdown, and was verified by review instead.)

## Success criteria

1. `uzi run wait <id> --until awaiting_approval --min-plan-seq <seq>` returns only
   once a plan message with `seq > <seq>` exists; it does **not** return on a stale
   `awaiting_approval` present at call time. Proven by the M1 unit test's stale-then-
   fresh control.
2. A **static** check of `uzi-cli`'s shipped `SKILL.md` shows all three hazards
   present in the Send-to-uzi section: the revise-by-seq wait using `--min-plan-seq`,
   the single-quote-the-`-m`-message warning, and the cookie-only-route pre-approve
   check.
3. `uzi run wait --help` lists `--min-plan-seq`, and the `run wait` entry in
   `SKILL.md` documents it (drift test green).
4. `uzi-watcher` no longer carries a second full copy of the three shared hazards —
   it points at the `uzi-cli` section — while retaining its GitHub/admin/k8s content.
5. `task gate:api` (and `task gate:repo`) green; no change to install behavior, the
   forge drivers, or any route.

## Risks & mitigations

- **`--min-plan-seq` semantics get the boundary wrong** (`>=` vs `>`, or gating a
  state it shouldn't). Mitigate: mirror `watch-run.sh` exactly (`cur > MIN`, only for
  `awaiting_approval`, terminal/`awaiting_input` unconditional), and the M1
  stale-then-fresh control catches an inverted or off-by-one gate.
- **Hazard wording leaks a forge or an agent-harness assumption** into a shipped,
  product-facing skill. Mitigate: write them in general terms — "your shell may
  evaluate a double-quoted `-m`", "uzi's `RequireUser` vs `RequireAuth` groups" — not
  "the Bash tool runs zsh" or GitHub specifics.
- **The two copies (uzi-cli ↔ uzi-watcher) drift again.** Mitigate with M3 (one
  source of truth + pointer), the repo's standing anti-duplication discipline.
- **Drift test reddens on the new flag if undocumented.** Mitigate: M2 documents it;
  this is a caught-by-gate failure, not a shipped one.

## Dependencies

- `api/cmd/uzi/run.go` `runWait` and the run-logs read it already performs.
- The shipped orchestration section (`api/internal/uzicli/skill/SKILL.md`
  `### Send to uzi (orchestration)`) and the skill↔cobra drift test
  (`api/cmd/uzi/skill_drift_test.go`).
- Source wording: `.claude/skills/uzi-watcher/SKILL.md` and its
  `scripts/watch-run.sh` (`min-plan-seq` logic).

## Out of scope

- A new `uzi-drive` skill, the installer generalization to N skills, and a new
  `uzi run watch` verb — all dropped as unjustified once the shipped driver was
  confirmed.
- The maintainer-only guardrail / admin-merge / PVC-recovery procedures (stay in
  `uzi-watcher`).
- Any change to the forge drivers, api routes, DTOs, controller, or web app.

## Decision log

- **Rescoped from "new skill" to "close three hazard gaps in the shipped skill."**
  Review verified a forge-neutral, all-three-forge driver already ships in `uzi-cli`;
  the real delta is three hazard paragraphs + one flag. Building a new skill would
  have shipped *less* forge coverage than exists today.
- **`uzi run wait --min-plan-seq` over a bundled poller script or a new verb.** The
  shipped skill can then express revise-by-seq with a CLI flag every user has, rather
  than depending on `watch-run.sh` (which does not ship).
- **Hazards land in `uzi-cli`, not a maintainer skill.** They are forge-neutral and
  every driving user hits them; `uzi-watcher` keeps only what is GitHub/admin/k8s.

## Phase / dependency map

| Phase | Milestone | Depends on | Files |
|---|---|---|---|
| 1 | M1 (`--min-plan-seq`) | — | `api/cmd/uzi/run.go` (+ test) |
| 2 | M2 (skill hazards + flag doc) | M1 (flag must exist for the drift test + wiring) | `api/internal/uzicli/skill/SKILL.md` |
| 3 | M3 (uzi-watcher re-point) | M2 (shared text must exist to point at) | `.claude/skills/uzi-watcher/SKILL.md` |

M1 is the only code; M2/M3 are skill content. M2 must follow M1 (the drift test fails
on a documented-but-undefined flag), and M3 must follow M2 (it points at M2's text).
