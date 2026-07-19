# PRD #86: keep the uzi-cli Claude Code skill fresh via an opt-in SessionStart hook

**GitLab Issue**: [#86](https://gitlab.example.com/vtmocanu/uzi/-/issues/86)
**Status**: Draft (created 2026-07-19; scope pivoted 2026-07-19 — see Change History)
**Priority**: Medium
**Created**: 2026-07-19
**Depends on**: PRD #64 (the `uzi` CLI, the bundled self-upgrading skill `uzi skill install/status`, and `Formula/uzi-cli.rb`).
**Related**: PRD #16 (the *server-side* agent-skills system) — shares no code with the CLI's bundled skill; unaffected.

## Change History

- **v1 (abandoned):** refresh the skill from the Homebrew formula's `post_install`, gated on
  `~/.claude` via a `uzi skill install --skip-without-claude` flag. **Disproven in review** and
  verified against the brew installed on this machine: `post_install` runs with `$HOME`
  overridden to an ephemeral tmpdir (`Library/Homebrew/formula.rb` `run_post_install`:
  `Dir.mktmpdir(...) { new_env[:HOME] = postinstall_home; with_env(new_env){ post_install } }`)
  **and** inside a sandbox that `deny_read_home` with no `allow_write_path(Dir.home)`
  (`formula_installer.rb` post_install path). A formula therefore **cannot** read or write the
  user's real `~/.claude` from `post_install` — the mechanism delivered nothing. This PRD is the
  rewrite.

## Problem

The uzi CLI keeps its Claude Code skill fresh **as a side effect of running the binary**:
`root.go`'s `PersistentPreRun` calls `maybeAutoUpgradeSkill` before every `uzi` command (except
`uzi skill …`), and `Install(false)` rewrites `~/.claude/skills/uzi-cli/SKILL.md` only when the
on-disk version ≠ the binary's stamped version. Reading `SKILL.md` runs no code.

That leaves a one-step-behind window. After `brew upgrade uzi-cli`, the on-disk skill stays stale
until the *next* `uzi` invocation. A fresh Claude Code session that reads `SKILL.md` before any
`uzi` command has run acts on the **old** skill once, then self-heals on its first command. The
window is small and self-correcting, but it is open exactly when the content just changed (a
version bump).

The only place that can close it is something that runs **in the user's real `$HOME`, at session
start, before the model reads `SKILL.md`.** The Homebrew formula is not that place (v1, above).
A **Claude Code `SessionStart` hook** is — it runs in the real session environment before the
turn begins. (Precedent: this environment already runs a `dot-ai skills generate` SessionStart
hook whose freshly-generated skills are usable in the same session.)

## Solution Overview

Ship an **opt-in** CLI command that wires a `SessionStart` hook into the user's
`~/.claude/settings.json`; the hook runs `uzi skill install` at session start.

1. **`uzi skill install-hook` / `uzi skill uninstall-hook`.** Idempotently add (or remove) a single
   `SessionStart` hook entry whose command refreshes the skill. The write is **surgical**: parse
   the existing `settings.json`, merge our one entry alongside whatever hooks are already there,
   back the file up first, and **abort rather than clobber** if the file is present but not valid
   JSON. Re-running is a no-op.
2. **Hook command = `uzi skill install`.** Fast and version-gated (a no-op write when already
   current), so it adds negligible session-start latency; best-effort.
3. **`uzi skill status` reports hook state** (installed? current?) alongside the existing SKILL.md
   status, so an agent or human can see whether auto-refresh is wired.
4. **Formula `caveats` nudge.** `Formula/uzi-cli.rb` gains a `caveats` block (print-only — the one
   formula affordance that is safe and *does* run for the real user) suggesting `uzi skill
   install-hook` to enable automatic refresh. Opt-in stays opt-in; brew never edits settings.json.
5. **Docs cover both surfaces.** `README.md` points users at `uzi skill install-hook`; `docs/cli.md`
   documents the **skill** and the **hook** side by side (see M5). Discoverability is the whole point
   of an opt-in feature — an unadvertised `install-hook` closes nothing.

Opt-in (not auto) because editing a user's `settings.json` without consent is invasive, and — per
v1 — nothing in the brew lifecycle can reach the real `~/.claude` to do it anyway.

## Design Decisions (Decision Log)

1. **SessionStart hook, not `post_install`.** v1's mechanism is disproven (Change History). A
   SessionStart hook is the only mechanism that runs in the real `$HOME` before `SKILL.md` is read.
2. **Opt-in via a CLI subcommand.** The user runs `uzi skill install-hook` once; brew only *nudges*
   via `caveats`. We never mutate `settings.json` without an explicit user action.
3. **Non-clobbering, idempotent `settings.json` merge, with backup.** The file is shared with many
   unrelated hooks (dot-ai, others). The command parses → merges our single entry → writes, backs
   up to `settings.json.bak` first, identifies our entry by a **stable command string** (settings.json
   is strict JSON, so no marker comment is possible), and **aborts on malformed JSON** instead of
   overwriting. Re-run = no-op.
4. **Hook command = `uzi skill install`.** Reuses the existing, tested writer; version-gated no-op
   keeps session-start cost near zero.
5. **Matcher = `startup`** (a new interactive session), likely **+ `resume`** to cover reopened
   sessions — `startup`/`resume`/`clear`/`compact` are the four `SessionStart` matchers, but the docs
   do **not** state which fires on a fresh session, so M1 confirms empirically.
6. **`timeout` set low + rely on best-effort exit.** Command hooks default to a **10-minute** timeout;
   we pin a short one (~15s) so a hung `uzi` cannot stall session start, and lean on the documented
   "non-zero exit ≠ blocking" behaviour so a failed refresh never breaks the session. Hook command is
   `uzi skill install`.
7. **Identity = full command string (settings.json has no hook `id`).** There is no upstream
   uniqueness/collision mechanism for the `SessionStart` array; the merge dedupes by exact command
   string and must not disturb sibling hooks. `~/.claude/settings.json` is user-scope; a project
   `.claude/settings.json` can override it, which is fine for a global default.
8. **`caveats` is print-only and safe.** Unlike `post_install`, `caveats` merely prints text for the
   real user; it runs for from-source formulae and cannot fail the install.
9. **Reuse the SKILL.md backup/USER_EDITED posture.** The settings.json edit mirrors the existing
   `.bak` safety valve so the two skill-management surfaces behave consistently.
10. **Docs advertise both surfaces (skill + hook).** `README.md` points users at
   `uzi skill install-hook`; `docs/cli.md` documents skill and hook side by side (see M5). An
   unadvertised opt-in closes nothing.

## Milestones

- [ ] **M1 — Empirical go/no-go on same-session hot-load (hard gate).** Confirmed from docs (guide
  review, Claude Code hooks-guide + settings docs): the `settings.json` `SessionStart` schema
  (`matcher` + `hooks[].{type,command,timeout}`), strict-JSON-only, user-scope `~/.claude/settings.json`,
  4 matchers (`startup`/`resume`/`clear`/`compact`), 10-min default timeout, and non-zero-exit is
  non-blocking. **Undocumented and load-bearing:** whether a `SessionStart` hook that rewrites
  `~/.claude/skills/uzi-cli/SKILL.md` is picked up **in the same session** before the model reads it.
  The `dot-ai` precedent generates `~/.claude/commands/*` (slash commands), a *different* surface from
  a Skill-tool `SKILL.md`, so it is suggestive, not proof. **Spike:** wire the hook, bump the skill
  version, open a fresh session, observe whether the model sees the new skill. **Go** → build M2–M6 as
  the same-session window-closer. **No-go** (hot-load is next-session-only) → the hook still guarantees
  freshness for the *next* session without requiring a `uzi` run (weaker than advertised, marginally
  better than today's self-heal); **stop and re-decide with the user** whether that residual value is
  worth the settings.json-editing surface, rather than shipping on a false premise like v1.
- [ ] **M2 — `uzi skill install-hook` / `uninstall-hook` (CLI, Go).** Idempotent, non-clobbering
  `settings.json` merge with backup and malformed-JSON abort. Unit tests over fixtures: missing
  file, empty file, file with unrelated hooks incl. an existing `SessionStart` array, re-run
  idempotency, our-entry removal leaves siblings intact, malformed input aborts without writing.
- [ ] **M3 — `uzi skill status` reports hook state.** Extend status + its `--json` output with
  hook-installed / hook-current fields; tests.
- [ ] **M4 — Formula `caveats` nudge.** Add a `caveats` block to `Formula/uzi-cli.rb` (source of
  truth) pointing at `uzi skill install-hook`.
- [ ] **M5 — Docs (README + cli.md + tap README).** `README.md`: extend the existing skill mention
  (line ~61) to tell users to run `uzi skill install-hook` so the skill auto-refreshes. `docs/cli.md`:
  document **both** surfaces together — the bundled **skill** (what it is, `uzi skill install/status`)
  **and** the **hook** (`uzi skill install-hook`/`uninstall-hook`: what it wires into
  `~/.claude/settings.json`, why, how to check with `uzi skill status`, how to remove). Tap README:
  the same enable-auto-refresh pointer.
- [ ] **M6 — Live validation.** Install the hook, `brew upgrade` (or bump the local binary), open a
  fresh session, confirm `SKILL.md` was refreshed before first use AND that the user's other
  SessionStart hooks still fire. Flip Status to Implemented with the date.

### Parallelization

| Phase | Milestones | Depends on | Files |
|-------|-----------|-----------|-------|
| 0 | M1 | — | (research; docs) |
| 1 | M2 | M1 | `api/cmd/uzi/skill.go`, `api/internal/uzicli/` (+ tests) |
| 2 | M3, M4, M5 | M2 (schema settled) | `skill.go`; `Formula/uzi-cli.rb`; `docs/cli.md`, tap README |
| 3 | M6 | M2–M5 | — |

## Risks

- **R1 — Corrupting a user's `settings.json`.** The file is shared with many hooks; a bad merge
  breaks Claude Code globally. Mitigation: parse-validate → back up → abort-on-malformed → write;
  idempotent stable-string identity; tests over realistic multi-hook fixtures (M2).
- **R2 — Same-session hot-load is undocumented (the v1-shaped risk).** If a hook-refreshed
  `SKILL.md` is only visible next session, the feature does not close the same-session window it
  exists for — it degrades to "guarantees next-session freshness without a `uzi` run," barely above
  today's self-heal. The docs do not state the ordering, and the `dot-ai` precedent is a *different*
  surface (`commands/` vs `skills/`). Mitigation: **M1 is an empirical hard gate with an explicit
  no-go path back to the user** — we do not build on this assumption, we test it first.
- **R3 — Session-start latency.** Every session pays the hook. Mitigation: `uzi skill install` is a
  version-gated local no-op when current; measure in M6.
- **R4 — Stale hook after uninstall.** If the user removes the `uzi` binary but leaves the hook, it
  errors each session. Mitigation: the hook command is best-effort; `uzi skill uninstall-hook`
  removes it cleanly; documented in M5.
- **R5 — settings.json is strict JSON.** No marker comment is possible, so our entry is identified
  by its command string; a user who hand-edits that string orphans our entry (a duplicate on next
  install-hook). Mitigation: match tolerantly on the command prefix; `status` surfaces duplicates.

## Validation Strategy

Go unit tests drive M2/M3 over `settings.json` fixtures (the risky surface). M1 settles the
Claude Code semantics from docs before any code. M6 is the end-to-end proof: a real fresh session
after an upgrade shows the skill refreshed ahead of first use with all sibling hooks intact.
