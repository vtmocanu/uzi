# PRD #1143: Ship the `uzi-cli` skill to Codex CLI — dual-target install and SessionStart auto-refresh

> Reworked after architect, reviewer, and Codex fact-check passes on 2026-09-05. Repository anchors are current at 4871f920. Codex behavior is pinned to codex-cli 0.153.4 and the official OpenAI skills/hooks documentation read the same day. The review replaced the deprecated Codex skill root, separated explicit install from auto-detection, made hooks target-specific, defined the JSON compatibility envelope, and removed two blind M0 probes.

**GitHub Issue**: [#1143](https://github.com/vtmocanu/uzi/issues/1143)
**Status**: Ready for implementation
**Priority**: Medium
**Related**: PRD #1106 (Codex as a *worker* harness; a different tree entirely, see *Non-goals*), the `uzi-cli` skill source `api/internal/uzicli/skill/SKILL.md`, `docs/cli.md` §"Bundled skill and session-start hook".

## Problem

`uzi skill install` writes the agent skill to exactly one place, and `uzi skill install-hook` wires its auto-refresh into exactly one harness:

- `api/internal/uzicli/skill.go:30` fixes the install dir as `skillDirParts = {".claude", "skills", "uzi-cli"}` under `$HOME`; `Status()` and `Install()` report one path, with `.uzi-cli-state.json` beside the skill.
- `api/internal/uzicli/skillhook.go:16-44` manages a Claude Code `SessionStart` hook in `home/.claude/settings.json` only. The canonical command is the unscoped `uzi skill install`.
- `api/cmd/uzi/skill.go:22-167` exposes one target in text and JSON. `status --json` is `{skill, hook}`; each mutating verb emits one result object.
- The embedded body, CLI docs, README, Homebrew caveat, and `specs/ai.md` describe Claude as the only consumer. Several fixed-path comments also say no environment-derived component can reach the installer.

An operator who drives uzi from OpenAI Codex CLI therefore gets no skill and no session-start refresh. Codex supports the same `SKILL.md` body and lifecycle hook shape, but its documented user skill root, hook configuration, trust flow, and matcher semantics differ enough that they must be modeled explicitly.

## Verified facts about Codex CLI (0.153.4, 2026-09-05)

1. **Skill roots.** The documented user root is `$HOME/.agents/skills`; repository skills live in `.agents/skills` from cwd through the repo root. `codex-rs/ext/skills/src/host_roots.rs` still reads `$CODEX_HOME/skills` as a deprecated compatibility location. This PRD writes the documented root and does not create a new dependency on the legacy one. `$CODEX_HOME` defaults to `~/.codex` and remains the user configuration/hook home.
2. **SKILL.md frontmatter.** Codex consumes `name`, `description`, and optional `metadata.short-description`. Unknown keys are ignored. Our `name` and `description` work unchanged; Claude-specific `allowed-tools: Bash(uzi *)` and `user-invocable: false` remain in the one shared body but are inert in Codex.
3. **Trigger.** Codex can load a skill explicitly through `$uzi-cli` and can select it when the request matches its description. The existing "send to uzi" / "ship to uzi" cue is a candidate for implicit selection; the live verification measures that inference rather than declaring it deterministic.
4. **Hook events and defaults.** Codex supports `SessionStart` and the other documented events, including `Interrupt`. Hooks are enabled by default; `[features] hooks = false` disables them. `SessionStart.matcher` filters the source values `startup`, `resume`, `clear`, and `compact`.
5. **Hook sources and shape.** The user layer reads `$CODEX_HOME/hooks.json` and/or inline `[hooks]` tables in `$CODEX_HOME/config.toml`. Loading both forms in one layer is valid but emits a startup warning. The JSON shape is `{"description"?: string, "hooks": {"SessionStart": [{"matcher"?: string, "hooks": [{"type": "command", "command": string, "timeout"?: int, "statusMessage"?: string, "async"?: bool}]}]}}`.
6. **Trust.** A non-managed hook is skipped until the user reviews and trusts its current hash through `/hooks`; startup prints a warning directing the user there. Trust keys include the source, event, group index, and handler index, so inserting before a trusted entry or removing a non-terminal entry can make a successor require review again.
7. **Claude Code, for contrast.** Personal skills load from `~/.claude/skills/` and hooks from `~/.claude/settings.json`. Claude does not natively read `$HOME/.agents/skills`, so one file cannot serve both harnesses without two install copies.

## Solution

Teach the installer two explicit targets backed by one embedded body:

| | Claude Code (existing) | Codex CLI (new) |
|---|---|---|
| skill path | `~/.claude/skills/uzi-cli/SKILL.md` | `$HOME/.agents/skills/uzi-cli/SKILL.md` |
| state / backup | `.uzi-cli-state.json`, `SKILL.md.bak` beside the copy | same, beside the Codex copy |
| auto-detected when | always | `$CODEX_HOME` (or default `~/.codex`) already exists as a directory |
| explicit `--target` | selects Claude even if auto-detection changes later | selects Codex even when its config home is absent; only install verbs create directories |
| hook file | `~/.claude/settings.json` | `$CODEX_HOME/hooks.json` |
| hook entry | `matcher:"startup"`, command `uzi skill install --target claude` | `matcher:"startup|resume"`, command `uzi skill install --target codex` |
| trust | none needed | user reviews once with `/hooks`; uzi never writes trust state |

- `uzi skill status|install|install-hook|uninstall-hook [--target claude|codex|all]`; `install` retains `--force`. Omitting `--target` means all detected targets. An explicit target is selected even when it was not auto-detected. `install --target codex` creates the validated Codex config home plus the documented user skill copy; `install-hook --target codex` creates the hook parent as needed. `status` and `uninstall-hook` remain read-only/no-op over missing directories.
- Automatic installation remains presence-based and best-effort. It never creates a Codex home merely because `uzi` ran. Each target is attempted independently.
- Each installed hook refreshes only its own copy. Existing Claude hooks containing the legacy bare command are migrated in place to `--target claude`; uninstall recognizes both the legacy and target-specific forms.
- Codex hook installation reads `config.toml` only to detect an existing inline `[hooks]` representation. If inline hooks exist and `hooks.json` does not already contain uzi's hook, it refuses with `uzicli.ExitUsage` and explains that the user must consolidate on `hooks.json`; it never creates a permanent mixed-representation warning silently.
- Text status prints one group per selected target: `TARGET`, `PATH`, `INSTALLED`, `UP_TO_DATE`, `USER_EDITED`, `HOOK_INSTALLED`, `HOOK_CURRENT`, and `HOOK_CONFIG_CONFLICT` where applicable.

Target selection is identical across the four verbs; filesystem effects remain verb-specific:

| selector | selected targets | `status` | `install` | `install-hook` | `uninstall-hook` |
|---|---|---|---|---|---|
| omitted | Claude plus Codex only when auto-detected | read only | create selected skill tails; never create an undetected Codex home | create selected hook parents | remove if present; create nothing |
| `claude` | Claude | read only | create Claude skill tail | create Claude settings parent | remove if present; create nothing |
| `codex` | Codex even when absent | inspect canonical paths; create nothing | create Codex config home and canonical skill tail | create Codex config home/hook parent | remove if present; create nothing |
| `all` | Claude and Codex even when Codex is absent | inspect both canonical paths; create nothing | create both skill tails and the Codex config home | create both hook parents | remove from both if present; create nothing |

## Milestones

- [x] **M1 — Target resolution, flags, and the Codex skill copy.** Replace the single `skillDirParts` assumption with a `skillTarget{name, skillDir, hookPath}` resolved by `resolveSkillTargets(home string, getenv func(string) string)`, and add `--target claude|codex|all` parsing/selection to all four verbs before later milestones use it. Claude is always detected. Codex auto-detection checks the existing absolute `$CODEX_HOME`, or `home/.codex` when unset; explicit `codex` and `all` select it even when absent, with filesystem effects exactly as in the table above. The Codex skill path is always `home/.agents/skills/uzi-cli`, independent of `$CODEX_HOME`; only its hook/config path follows `$CODEX_HOME`. Add `Env.Getenv` (`os.Getenv` in `DefaultEnv`, deterministic empty lookup in `fakeEnv`) and use that seam instead of package-global environment reads for target resolution. A relative `CODEX_HOME` is skipped on the auto path and is `uzicli.ExitUsage` when Codex is explicitly selected. Keep `target` in CLI envelopes, not `SkillStatusResult` or `SkillInstallResult`. Update the fixed-path comments in `api/internal/uzicli/skill.go`, `skillhook.go`, and `api/cmd/uzi/root.go`. Tests cover every row/verb of the selection table with no Codex home, including that read-only/no-op verbs create nothing; an existing absolute `CODEX_HOME` supplies only the hook/config path; relative values are rejected explicitly; user edits are backed up independently; Claude and Codex writes cannot cross into each other's directories; a fake test environment never consults the developer's exported `CODEX_HOME`. Gate: `task gate:api`.
- [x] **M2 — Target-specific hook managers and final CLI/JSON contract.** Generalize `HookManager` over `{target, path, canonicalMatcher, canonicalCommand}`. Claude keeps `settings.json`; Codex uses `$CODEX_HOME/hooks.json`, appends its entry, uses `matcher:"startup|resume"`, and tells the user to run `/hooks` to review it. Migrate the legacy Claude bare command to `uzi skill install --target claude`; idempotence and uninstall recognize the legacy form without treating `uzi skill install-hook` as ours. Parse Codex `config.toml` read-only with the existing TOML dependency and refuse a new mixed hook representation as described above. Never write `config.toml`, `[hooks.state]`, or `trusted_hash`. Back up the original hook file byte-for-byte before every mutation of an existing file; after re-encoding, require semantic preservation of every foreign value and exact preservation of hook array order. Reformatting the live JSON is allowed. Append keeps existing trust indices stable; uninstall warns before removing our non-terminal entry. Test each target with its sibling path unwritable; plant two foreign entries around ours and assert semantic equality/order after install and uninstall; assert a byte-identical `.bak`; exercise malformed JSON, inline-hook conflict, legacy migration, idempotence, target-specific matching, and non-terminal removal warning. Gate: `task gate:api`.
- [x] **M3 — Backward-compatible output, documentation, specs, and changelog.** Attempt selected targets in deterministic `claude`, then `codex` order without stopping after one failure. A `targets[]` item is `{target, result, error?, exit_code}`; `status.result` contains `{skill, hook}` and mutating results keep their existing DTOs unchanged. When the caller omits `--target`, retain the current Claude top-level fields and values exactly and add `targets`; when the caller uses the new `--target` syntax, emit `{targets}` only. Emit the envelope before returning the first non-zero target code so partial work and every failure remain visible. Add exact JSON assertions for successful legacy invocations of all four verbs, Codex-only invocations, and partial failures; the existing substring test is insufficient. Update every current command/path/harness claim: `api/internal/uzicli/skill/SKILL.md` (including the synopsis, the false "silently lost" wording, and "every command" → "every executing non-skill command"), `docs/cli.md`, `README.md`, `Formula/uzi-cli.rb`, `api/cmd/uzi/root.go`, `api/cmd/uzi/versioncheck.go`, the evidence comments in `api/cmd/uzi/instructions_test.go`, `specs/ai.md` sections around lines 7654, 7960-7977, and 18019, plus `CHANGELOG.md`. Run `task docs:sync` and commit the mirror. Gates: `task gate:api`, `task gate:web`, and `task gate:repo`.
- [ ] **M4 — Maintainer live verification on Codex 0.153.4 or newer.** Back up any existing target files before the probe. Verify the canonical `$HOME/.agents/skills/uzi-cli/SKILL.md` appears in `/skills`; `$uzi-cli` reads it and a non-mutating request such as `uzi run list --json` works; the description cue is either observed or recorded as non-deterministic without weakening explicit invocation. Install the Codex hook, confirm startup directs the user to `/hooks`, trust it there, make the Codex copy stale, and verify a new `startup` or `resume` refreshes it in the same session. Confirm the Claude hook refresh succeeds while the Codex target is unwritable and the inverse. Append a trusted foreign successor after uzi, uninstall uzi, and confirm the warning plus the successor's review state; reinstall uzi last, uninstall it, and confirm foreign entries remain semantically identical, in order, and trusted. Restore the backed-up live files and record results in *Progress*.

## Success criteria

1. Automatic install writes the Codex copy only when a Codex config home already exists. Explicit Codex install verbs create their required fixed directories; explicit status and uninstall remain non-mutating when those directories are absent.
2. Claude and Codex hooks run target-specific install commands and still refresh their own copy when the sibling target is unwritable.
3. `uzi skill status` reports every selected target; legacy successful JSON keeps today's Claude top-level fields and values, while `targets[]` truthfully reports every attempt and partial failure.
4. Existing hook files receive a byte-identical backup. Foreign JSON values and hook order survive semantically; existing trust remains valid whenever uzi's append or terminal removal leaves indices unchanged.
5. Codex hook installation never writes trust state or `config.toml`, and it refuses to create a second hook representation silently when inline hooks already exist.
6. The shared embedded body works in both harnesses; every harness-specific statement names its harness, and explicit `$uzi-cli` invocation is live-verified.
7. Docs, specs, README, Homebrew caveats, fixed-path comments, and the embedded command synopsis describe both targets and the `/hooks` trust flow; `task docs:sync` is committed.
8. Tests with `CODEX_HOME` exported outside their temp home write nothing outside controlled paths. `task gate:api`, `task gate:web`, and `task gate:repo` pass.

## Decision Log

- **D1 — Use `$HOME/.agents/skills`, the documented Codex user root.** `$CODEX_HOME/skills` is retained upstream only for backward compatibility. Sharing the `.agents/skills` inventory is preferable to creating a new dependency on a deprecated root; uzi owns only its `uzi-cli/` child and preserves an edited copy before replacement.
- **D2 — Presence controls automation; explicit selection bypasses detection.** The automatic path must not litter a machine without Codex. Explicit `codex` or `all` selects Codex regardless of presence; only `install` and `install-hook` create their required directories, while `status` and `uninstall-hook` remain non-mutating, matching issue #1143.
- **D3 — One embedded body, with documented inert metadata.** `name` and `description` serve both harnesses. Claude-specific permission/invocation keys remain for Claude and are ignored by Codex; harness-specific operational prose names its harness.
- **D4 — uzi never writes Codex trust or TOML.** The user reviews the hook through `/hooks`. `config.toml` is parsed read-only to detect mixed hook representations; uzi refuses the conflict instead of rewriting TOML or causing a permanent warning without consent.
- **D5 — Hook commands are target-specific.** A lifecycle hook must not fail or skip its own refresh because an unrelated harness is unwritable. The existing bare Claude command is migrated and remains uninstallable for backward compatibility.
- **D6 — JSON compatibility applies to legacy syntax, not fictional Claude actions.** Calls without `--target` retain today's Claude top level and add `targets`. New targeted syntax uses the per-target envelope. All selected targets run; the process exits with the first failure code after printing the complete result.
- **D7 — Automatic installation stays per-target best-effort.** Claude failures retain today's warning. Codex failures are silent on the automatic path so a read-only Codex tree does not add noise to every uzi command. Explicit verbs report all failures and return non-zero.
- **D8 — User-influenced paths are bounded.** The user home supplies fixed Claude and `.agents/skills/uzi-cli` tails. `CODEX_HOME` affects only the Codex config/hook home, must be absolute, is cleaned, and is injected in tests. No remote input reaches any path component.
- **D9 — Preserve meaning and trust, with raw recovery.** The manager may normalize JSON formatting. The pre-write backup preserves exact original bytes; the live file preserves foreign values and array order. Trust retention is asserted from normalized hook identity and live behavior, not inferred from file-byte equality.

## Dependencies & conflicts

- **None on PRD #1106.** That PRD provisions a worker's private per-run `CODEX_HOME`; this PRD manages an operator's user-level skill and hook through the `uzi` binary.
- Files touched: `api/internal/uzicli/skill.go`, `skillhook.go`, their tests, `api/cmd/uzi/root.go`, `skill.go`, `skill_cmd_test.go`, `skill_drift_test.go` if stronger flag coverage belongs there, `versioncheck.go`, `instructions_test.go`, `api/internal/uzicli/skill/SKILL.md`, `docs/cli.md`, `api/internal/uzidocs/embed/cli.md` via `docs:sync`, `README.md`, `Formula/uzi-cli.rb`, `specs/ai.md`, and `CHANGELOG.md`. No `.github/workflows/**`, migration, web runtime, or API route change.
- **Ordering:** M1 → M2 → M3 → M4. M1-M3 share CLI files and are sequential. M4 is a maintainer verification after the implementation branch exists; it does not block uzi from implementing M1-M3 and opening its MR.

## Risks & mitigations

- **R1 — Codex changes its documented skill root or hook schema.** The design now follows the documented user root and pins the verified version. Target descriptors isolate path and wrapper changes; M4 records the version actually exercised.
- **R2 — Hooks are disabled by user or administrator policy.** Installation remains valid but cannot override that policy. The trust notice says hooks must be enabled and directs the user to `/hooks`; uzi does not edit feature or managed-policy settings.
- **R3 — Another installer owns `$HOME/.agents/skills/uzi-cli`.** Content-hash state distinguishes uzi's last write; a differing user/tool copy is backed up before replacement, independently from the Claude copy.
- **R4 — Inline and JSON hook representations coexist.** Read-only TOML detection refuses a new mixed representation and provides a concrete consolidation instruction.
- **R5 — Removing a non-terminal hook shifts successor trust indices.** Append makes uzi terminal at install time; uninstall detects and warns when later entries exist. Live verification measures the successor's actual review state.
- **R6 — Multi-target work partially succeeds.** Every target is attempted and represented in output; overall exit is non-zero when any selected target fails. Target-specific hooks avoid the problem on session refresh.

## Non-goals

- The worker-side Codex harness (PRD #1106).
- A Codex exec-policy entry allowing `uzi *` without approval.
- Porting this repo's `.claude/skills/uzi-watcher` to `.agents/skills`.
- An `AGENTS.md` pointer in the Codex home.
- Other harnesses. Each needs its own verified target and hook contract.
- Editing inline Codex hooks or any Codex trust/feature/managed-policy state.

## Progress

- 2026-09-05: issue filed and initial PRD drafted against codex-cli 0.153.4.
- 2026-09-05: architect, reviewer, and fact-check passes folded in. The PRD now uses the documented Codex skill root, target-specific hooks, truthful JSON envelopes, semantic hook preservation, current `/hooks` trust UX, complete documentation surfaces/gates, and worker-safe milestone ordering. Ready to send to uzi for M1-M3 implementation.
- 2026-09-05: M1-M3 implemented on branch `agent/issue-1143` and reviewed clean (reviewer + tester + auditor across the hook-mutation milestone; fact-checker over docs/specs). `task gate:api`, `task gate:web`, and `task gate:repo` all pass. M4 (maintainer live verification on a real Codex 0.153.4 install — `/skills` visibility, `$uzi-cli` load, `startup`/`resume` hook refresh, `/hooks` trust, foreign-successor preservation) is deferred to a maintainer run; it cannot run in the implementation worktree, which has no Codex install.
