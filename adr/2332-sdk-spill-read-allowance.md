# ADR-2332: Allow a Claude run to Read its own SDK tool-result spill

**Status**: Accepted
**Date**: 2026-10-06
**Issue**: [#2332](https://github.com/vtmocanu/uzi/issues/2332)

## Context

When a tool returns more output than the Claude SDK will inline, the CLI writes
the full output to `<HOME>/.claude/projects/<P>/<session_id>/tool-results/<file>`
and tells the model "Output too large ... Full output saved to: ...". That
directory is outside the run worktree, so the file-tool path guard in
[agent/src/guardrails.ts](../agent/src/guardrails.ts) denied the agent's follow-up
`Read` as outside the worktree, and the run could not see the output it was told to read.

## Decision

On the Claude run lane only, the path guard lets `Read` open a direct-child
regular file of the run's own spill directory. The directory is derived from
trusted inputs, never from tool output or agent text: the worker-supplied run
HOME and the SDK-supplied hook-input `session_id` and `transcript_path`, which
must be `<HOME>/.claude/projects/<P>/<session_id>.jsonl` (`sdkSpillRoots`).
Anything else yields no allowance.

- Only `Read`. `Write`, `Edit`, `MultiEdit`, `NotebookEdit`, `Glob`, `Grep` and
  Bash screening are unchanged.
- The chat, isolated and job-runner path guards pass no HOME and are unchanged.
  The Codex harness does not spill this way and is out of scope.
- Fresh, resumed (same session id) and subagent sessions are covered; subagent
  hooks carry the root session id, and spills land in the root session's directory.
- Denied: other sessions' spill directories, other runs' HOMEs, other files in
  HOME, the directory itself, nested paths, `..` escapes, and symlinks. The check
  uses `lstat` for the file and `realpath` for the HOME, with the tail kept
  lexical so an agent-made symlink at `tool-results` cannot move the root.

### Check order

In `classifyResolvedPath` the `/proc` deny and the secret-file deny run first,
then the worktree containment check. The spill allowance lives only inside the
outside-worktree branch. The `.git` deny runs after the containment check and
applies to in-worktree paths, so it still applies on every in-worktree path. A
spill path is outside the worktree; a path whose realpath lands inside it (for
example `tool-results` symlinked into `.git`) is classified as in-worktree and
hits the `.git` deny.

## Alternatives rejected

- **Relocate spills into `.uzi/scratch/`.** The CLI builds the spill directory
  only from `CLAUDE_CONFIG_DIR` or `$HOME/.claude` (plus `CLAUDE_CODE_PROJECT_DIR_NAME`
  only when `CLAUDE_CONFIG_DIR` is set). Setting `CLAUDE_CONFIG_DIR` would move
  credentials and config into the worktree.
- **A PostToolUse hook recording `persistedOutputPath`.** It is Bash-only; the
  generic spill path (other tools, MCP) is written after the PostToolUse loop.

## Consequences

- Checks are check-time only: the CLI opens the file later, so a parallel Bash
  swap could race, as it can for the existing worktree jail.
- The agent can copy any file it can already read into the spill directory and
  `Read` it. Neither residual reaches anything Bash (same runner uid) cannot
  already read; under the worker/runner uid split OS permissions remain the boundary.
- Nested spill subdirectories (for example PDF page extraction `pdf-<id>/`) are
  not readable. This is a limit, not a hole.
- Transcript paths outside the expected `<HOME>/.claude/projects/<P>/<session_id>.jsonl`
  layout fail closed. A `CLAUDE_CONFIG_DIR` override that changes that layout
  receives no allowance. No Unicode normalization policy is enforced.
