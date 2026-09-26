# ADR-1719: Run scratch lives inside the runner checkout

**Status**: Accepted and implemented
**Date**: 2026-09-26
**Related**: [PRD #1719](../prds/done/1719-run-scratch-dir-path-policy.md)

## Context

Validators need a place for gate logs, exported review snapshots and screenshots
that direct file tools can read. Direct file-tool paths outside the run worktree
are denied, but that tool policy is not whole-process filesystem confinement.
A shell can reach paths its OS identity permits. The previous denial wording
overstated the boundary and ad hoc artifact locations caused repeated failures.

## Path-policy guarantee

| Harness / command mode | Direct file tools | Shell commands |
|---|---|---|
| Claude | Worktree path policy is enforced by the `PreToolUse` hook. | Bash command screening is a deny-list; filesystem access is governed by OS permissions and other guards, not a worktree-wide path jail. |
| Codex with Landlock | Worktree path policy is enforced by the broker. | The broker screens explicit `cwd`; Landlock confines the command to its configured roots, including the checkout and private command tmp. |
| Codex without Landlock in `best-effort` mode | The same broker path policy remains enforced. | The broker still screens explicit `cwd`, but command execution has no filesystem confinement; the uid split and OS permissions remain. |
| Codex without Landlock in `required` mode | No run starts. | The worker does not advertise the Codex capability. |

The direct-path policy is defence in depth, not a whole-process boundary. The
scratch directory is an artifact convention, not a new access-control boundary.

## Decision

1. **Keep the direct-path jail.** Claude's `PreToolUse` path hook screens
   direct file tools; its Bash screener is a deny-list, not path confinement.
   Codex screens direct tools and explicit shell working directories through
   its broker. Neither check makes the whole process path-confined. Shell
   access also depends on OS permissions, the worker/runner/runner-cmd uid
   split where deployed, deny-hooks and credential custody. Codex Landlock
   confines filesystem access only when the kernel supports and enables it;
   a worker without Landlock must report confinement unavailable. The
   existing secret and `.git` restrictions remain in force.
2. **Reserve `.uzi/scratch/` inside each runner checkout.** Only that
   subtree is reserved; other `.uzi/` contents remain the repository's.
   The path is already inside the direct-tool, shell working-directory and
   available Landlock roots, so no allowed root is added. An inside-checkout
   path gives Claude and Codex the same artifact location; on Claude, the
   Bash limitation in decision 1 still applies.
3. **Provision safely before the first agent turn.** The worker creates or
   validates the directory with runner-compatible group-write and setgid
   posture, using descriptor-relative no-follow operations or an equivalent
   safe helper. It must not follow a pre-existing symlink, recursively change
   `.uzi/` ownership or permissions, or overwrite unrelated contents.
   A tracked `.uzi/scratch` path, a symlink or non-directory ancestor, or
   unsafe filesystem posture causes a named provisioning failure. Revalidate
   safely on adoption. The checkout is agent-writable: worker provisioning
   does not make scratch tamper-proof.
4. **Exclude scratch locally.** The worker adds `/.uzi/scratch/` to that
   clone's `.git/info/exclude` through trusted git operations, without
   changing tracked `.gitignore`. This keeps ordinary `git add -A` and
   WIP park auto-commits from staging scratch. Ignore rules are staging
   convenience, not a security boundary; forced staging remains possible.
5. **Refuse publication.** Checkpoint and finalize publication fail with a
   typed, named reason if any path at or beneath `.uzi/scratch` is present
   in any commit of the send range or in a WIP/index overlay capture.
   Inspect every newly transmitted commit, including an add followed by a
   delete, rather than only the final tree or net diff. Refusal must not
   advance a confirmed checkpoint tip; preserve recovery custody and report
   a bounded, credential-free reason. Do not silently strip artifacts or
   rewrite history. The scan covers the candidate's full history, so a
   branch whose history ever contained a scratch path stays refused even
   after a later commit deletes it.
6. **Keep scratch ephemeral.** Preserve it on the same retained runner clone
   across parks and resumes; do not delete it during execution or routine
   checkpoint publication. Fresh reseed and cross-worker recovery create an
   empty directory. Scratch is not checkpointed or durably recoverable.
   Settled clone retirement cleans it up.
7. **Use plain exports for review snapshots.** Export a commit into a
   fresh scratch subdirectory (for example, from the checkout root,
   `sha=$(git rev-parse --verify HEAD) || exit; snap=$(mktemp -d .uzi/scratch/snap.XXXXXX) || exit; set -o pipefail; git archive "$sha" | tar -x -C "$snap" || exit`) or use
   `git show` and `git diff`. Remove the export after review. Do not create a nested git worktree.
   Exports have neither git metadata nor installed dependencies, so run
   git-dependent gates in the real checkout.
8. **Put gate logs in scratch.** Use
   `mktemp .uzi/scratch/gate-log.XXXXXX` for a unique log per invocation.
   The root-level `gate-log.*` ignore rule remains available for host use.

## Consequences

Worker prompt guidance and out-of-worktree denials point agents at the
scratch directory on both harnesses. The path solves direct-tool access to
run artifacts; it does not broaden shell confinement. Publication refusal is
the guard even if an agent force-stages an ignored scratch file. Scratch
persists only while the identical runner clone remains retained.
