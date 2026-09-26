# PRD #1719: Run scratch directory and an honest worktree path policy

**Issue**: [#1719](https://github.com/vtmocanu/uzi/issues/1719)
**Status**: Planned
**Priority**: Medium
**Owner**: uzi run (Codex harness), plan steered by the maintainer's session and a Codex peer

## Problem

The judge's most frequent open recommendation (34 open across 40 runs, both harnesses) is "guardrail read only access to agent produced artifacts outside the worktree". Validators (auditor, reviewer, tester) routinely create a snapshot of the commit under review, a gate log, or a screenshot somewhere outside the checkout, then find that direct file-tool operations on it are denied while the same operation succeeds through a shell `cd`. Two separate defects:

1. **The guarantee is unstated and the denial over-promises.** Direct file-tool paths are jailed to the worktree on both harnesses (Claude: the `PreToolUse` path hook in `agent/src/guardrails.ts`, `screenToolPath` at `:1424`, `PATH_TOOLS` excludes Bash; Codex: `agent/src/codex/broker.ts` also screens an explicit shell `cwd`). Claude's Bash screener is a deny-list, not path confinement, and a Codex worker without Landlock reports that filesystem confinement is unavailable. The denial text (`REASON_OUTSIDE_WORKTREE`, `guardrails.ts:210`: "file access outside the run worktree is not permitted") reads like a boundary the shell does not share.
2. **There is no standard place for run artifacts.** Guidance is inconsistent: `coder.md` says to keep gate logs "on a path the repo ignores (add the pattern if it is not)", which invites a tracked `.gitignore` edit into the agent's diff; `web-ux.md:71-73` and `ux-designer.md:67-68` still say "outside the tracked tree". Every validator rediscovers a convention per run.

## Decision (agreed 2026-09-26, maintainer session plus Codex peer review)

- **D1. Name the guarantee.** The direct-path jail is an enforced **tool policy** and defence in depth, not whole-process filesystem confinement. Keep it intact; do not weaken it. Shell access remains subject to OS permissions (the worker / runner / runner-cmd uid split), the `PreToolUse` deny-hooks, credential custody (the worker holds the PAT), and Codex Landlock where the kernel provides it. Do not attempt to make the path check a process boundary.
- **D2. One scratch directory per run, inside the checkout: `.uzi/scratch/`.** Inside the worktree, so direct tools, shell, Codex `cwd` screening and Landlock already allow it; no new allowed roots. Reserve only that subtree, not all of `.uzi/`.
- **D3. Worker-provisioned, never silently clobbering.** The worker creates it before the first agent turn with runner-compatible ownership (group `runner`, setgid, the same group-write posture `assertCommandWorktreePosture` checks for Codex at `agent/src/codex/codex-executor.ts:2979`). Use descriptor-relative, no-follow operations or the existing equivalent safe helper for creation and validation. Do not recursively chown/chmod `.uzi` or follow a pre-existing symlink. Preserve unrelated `.uzi` contents and permissions. Revalidate safely when adopting an existing scratch directory. A collision (the repo tracks anything at `.uzi/scratch`, or an ancestor is a symlink or non-directory) or unsafe filesystem posture produces a named provisioning failure before the first agent turn. Do not silently continue with an unavailable scratch directory, overwrite repository content, or widen allowed roots. Worker-created does not mean tamper-proof: the checkout is agent-writable.
- **D4. Ignore locally.** The worker adds `/.uzi/scratch/` to the clone's `.git/info/exclude` through its own trusted git operations, never by editing a tracked `.gitignore`. The exclude is a staging convenience, not protection: `git add -A` (including the WIP park auto-commit at `agent/src/git.ts:2915`) then skips it.
- **D5. Refuse publication, never silently strip.** If any path under `.uzi/scratch/` appears in content being published (the committed send range for a checkpoint or finalize push, and any WIP/index overlay capture), publication fails with a typed, named reason. Check the committed range, not only the index: an artifact committed and later deleted is still in history.
- **D6. Lifecycle.** Preserve scratch while the same runner clone is retained through parks and resumes. Recreate an empty scratch directory on fresh reseed or cross-worker recovery; scratch contents are not checkpointed or durably recoverable. Never delete scratch during active execution or routine checkpoint publication. Settled clone retirement performs cleanup.
- **D7. Review snapshots are plain exports** (`git archive <sha> | tar -x -C .uzi/scratch/snap-<shortsha>`) or direct `git show` / `git diff` reads, never a nested `git worktree add`. An export has no git metadata and no installed dependencies, so git-dependent gates still run in the real checkout; do not promise an export supports every validator.
- **D8. Reuse the gate-log convention inside scratch**: `mktemp .uzi/scratch/gate-log.XXXXXX` (the repo already ignores root-level `gate-log.*`, `.gitignore:41-42`; that stays for host use).
- **D9. Guidance.** Product builtins get the concrete path through worker-owned prompt guidance (the lead prompt, `agent/src/prompt.ts`, and the worker-owned subagent safety block, issue #1660, `agent/src/prompt.ts:229` / `agent/src/codex/render.ts:365`), so it is correct on both harnesses. The out-of-worktree denial points at the scratch dir without weakening the secret or `.git` restrictions. Builtin templates stop saying "outside the tracked tree" or "add the pattern".
- **D10. Record.** These are durable invariants (the guarantee, the reserved subtree, publication refusal, the lifecycle), so they get an ADR numbered 1719, written in the implementing run.

## Out of scope

- Any change to `.github/workflows/**` (the worker PAT cannot push workflow files).
- The upstream generic role library (`vtmocanu/skills` `agent-team/roles.yaml`) and this repo's `.claude/agents/*.md`: those are the maintainer's, decoupled from the product (see CLAUDE.md, "Builtin agent templates"). Follow-up for the maintainer: generic roles say "use the worker-provided scratch directory", with a host fallback, never uzi's literal path.
- Making Claude's Bash tool path-confined (a sandbox project of its own).

## Milestones

## Execution plan

| Phase | Milestones | Dependency / shared files |
|---|---|---|
| 1 | M1 | Records the implementation contract |
| 2, sequential | M2 to M5 | Shared Git, runner, prompt and lifecycle files |
| 3 | M6 | Integrates all preceding changes |

One gated run; these milestones must not become parallel writers.

Implementation and validation require no external documentation or upstream checkout. Inspect repository code and local fixtures. Upstream role propagation and live deployment verification remain maintainer follow-ups. Create ADR-1719, update applicable sandbox documentation, and preserve existing secret and `.git` restrictions.

- [ ] **M1. ADR-1719 and docs.** Write the ADR (file name adr/1719-run-scratch-dir.md, created in this milestone) stating D1 to D8 as invariants and the enforcement guarantee per harness and per Landlock availability. Update the relevant user/operator doc on the worker sandbox (find it with `git grep -n -i 'worktree' docs/`), run `task docs:sync`, and add a CHANGELOG `[Unreleased]` line. `task check-docs:web` green.
- [ ] **M2. Provisioning.** The worker creates `.uzi/scratch/` for every run kind that has a runner clone, on fresh clones and on resume/reseed (`agent/src/git.ts` `createOrAttachRunnerClone` `:829`, `runnerCloneForBranch` `:921`, and the reseed path), with the D3 posture, safe no-follow creation and named provisioning failure, and the D4 exclude entry. Works under uid split (Codex `runner-cmd` can write it) and single-uid.
- [ ] **M3. Publication refusal.** Checkpoint publication (`checkpointPack`, `agent/src/git.ts:1928`, called from `agent/src/runner.ts:7567`) and the finalize push path refuse, with a typed reason, when the send range or overlay contains any path under `.uzi/scratch/`. Reject `.uzi/scratch` itself and every descendant, including a committed file or symlink replacing the directory. Inspect every newly transmitted commit, not just the final tree or net diff, plus WIP/index captures. Apply the check to all checkpoint and finalize publication paths on every forge. A refused checkpoint must not advance its confirmed tip; preserve recovery custody and report a bounded, credential-free reason. Do not strip content or rewrite history automatically.
- [ ] **M4. Lifecycle.** Per D6: scratch contents survive an approval park, a limit park (WIP auto-commit skips them) and a resume on the retained clone; a fresh reseed or cross-worker recovery recreates it empty; cleanup happens only on settled retirement.
- [ ] **M5. Guidance.** Lead prompt and the worker-owned subagent safety block name the scratch path on both harnesses (Claude `agent/src/prompt.ts`, Codex `agent/src/codex/render.ts`); builtins updated (`api/internal/agenttmpl/builtins/coder.md`, `lead.md`, `web-ux.md:71-73`, `ux-designer.md:67-68`, and any validator template that describes snapshots or logs) to D7/D8, and correct the coder/tester claim that ignored artifacts "can never be staged" (ignore rules do not prevent `git add -f`; publication refusal, D5, is the guard); `REASON_OUTSIDE_WORKTREE` names the scratch dir. Run `cd api && go test ./internal/agenttmpl/... -count=1`. CHANGELOG line (builtin edits re-apply to pristine rows on boot).
- [ ] **M6. Tests.** Regression tests ship with M2 to M5 and are demonstrated red against the corresponding unfixed seam, then green with the fix. M6 integrates the complete suite and required component gates. Coverage:
  - provisioning: created with the right mode/group; named provisioning failure on a tracked `.uzi/scratch`, on a symlinked `.uzi`, and on a non-directory; unrelated `.uzi` contents untouched; preserved on a retained-clone resume and recreated empty on a fresh reseed (separate tests);
  - exclude: `git status --porcelain` stays empty with files in scratch; `git add -A` does not stage them;
  - publication: a commit adding a scratch path, an add-then-delete across two commits, and a file or symlink replacing the directory each make checkpoint and finalize publication fail with the typed reason without advancing the confirmed tip; a clean range publishes;
  - guardrail: a direct-tool read and write under `.uzi/scratch/` is allowed on both harnesses; a path outside the worktree is still denied, and the denial names the scratch dir; secret and `.git` denials unchanged;
  - guidance: the rendered lead and subagent prompts on both harnesses contain the scratch path.
  `task gate:agent` and `task gate:api` green.

## Success criteria

- A validator can write a gate log, an exported snapshot and a screenshot under `.uzi/scratch/` and read them back with direct tools and the shell, on both harnesses, with `git status` clean.
- Nothing under `.uzi/scratch/` can reach a pushed branch or checkpoint.
- The judge's "read-only access to agent-produced artifacts outside the worktree" recommendation stops recurring on runs after release (maintainer check, post-deploy).

## Risks

- **A repo that already uses `.uzi/`** for something else: only `.uzi/scratch` is reserved, and a collision is refused, not overwritten (D3).
- **Agents ignoring the guidance**: the path check stays in force outside the worktree, so the failure mode is unchanged, not worse.
- **Docker-lane `run-workdir` is an emptyDir**: scratch dies with the pod, like the rest of the working clone. Acceptable; scratch is transient by definition.
- **Codex runs on this repo currently hit #1716** (agent git fails "dubious ownership" as `runner-cmd`). Dispatch after #1716's checkout-trust fix is available in the worker image. Per-command Git flags do not repair Git subprocesses launched by gates, so they are not a reliable validation environment for this PRD.

## Decision Log

- 2026-09-26: Design agreed between the maintainer's session and a Codex peer (issue #1719 thread). Inside-checkout scratch chosen over a sibling directory added to the allowed roots, because it needs no policy widening and every enforcement layer already permits it. Refuse-not-strip on publication chosen so an artifact never silently changes what a human reviews.
