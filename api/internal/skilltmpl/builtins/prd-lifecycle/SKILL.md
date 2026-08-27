---
name: prd-lifecycle
description: How to update a PRD file at the end of a run — scan every unchecked item, tick only what direct evidence supports, and move the file to prds/done/ when (and only when) the whole PRD is complete. Use when the issue description links a prds/*.md file and you are about to signal the work done, or when reviewing another agent's PRD update.
---

# PRD lifecycle: update, and move when finished

The issue's linked `prds/*.md` file is this run's spec. Read it at the start; update
it before you finish. This is a normal file edit committed on the run's branch, like
any other change — you never push, and the worker delivers it in the merge request
that carries the code.

**If the issue links no PRD file, everything here is a no-op.** Do not go looking for
a PRD to update. A repo can have a `prds/` directory and an issue that deliberately
has nothing to do with it.

## 1. Scan every unchecked item, not just the ones you remember

Grep the file for unchecked boxes (`- [ ]`) and go through **all** of them. The item
you did not think about is exactly the one that gets wrongly ticked or wrongly left.
Categorise each: implementation, documentation, validation, or launch.

## 2. Tick only on direct evidence

Be conservative. Mark an item complete only when you can point at what makes it true:

- **Implementation** — the code exists, is wired to its callers, and has tests.
- **Documentation** — the file exists and its examples and links were checked.
- **Validation** — the check was actually run, and you saw it pass.

Do not tick because something looks good enough, because a test file exists without
having run it, or because an item is "basically done". An unticked item costs the
next run a few minutes; a wrongly ticked one is believed, and that is worse than no
update at all. If you did part of an item, leave it unchecked and add a sub-note
saying what landed.

Record what you actually built where it diverges from what was planned. A PRD that
silently describes different work than the branch contains is the failure this step
exists to prevent.

## 3. Move to `prds/done/` only when the WHOLE PRD is complete

Check first, then move. A PRD routinely outlives its first merge request: several
runs against one PRD is the expected shape, not the exception.

- **Every checkbox ticked** — update the status header, then move the file.
- **Any item still open** — update the checkboxes and **leave the file where it is**.
- **Already under `prds/done/`** — a no-op. Do not move it again, do not "tidy" the
  path. Update the checkboxes if there is anything to update, and stop.

The move itself:

```sh
mkdir -p prds/done
git mv prds/<file>.md prds/done/<file>.md
```

**The `mkdir -p` is required, not defensive.** Git does not track empty directories,
so in any repo that has never archived a PRD, `prds/done/` does not exist and a bare
`git mv` fails with `fatal: renaming ... failed: No such file or directory` (exit
128). That is the first-use case in every such repo, and it fails at the very end of
the run.

Then update the status header in the moved file to say it is complete, with the date.

**Repoint inbound links to the moved file — in the SAME commit as the move.** Moving
`prds/<file>.md` to `prds/done/` breaks every relative markdown link that pointed at
the old path. Those links live in files this run never touched (`docs/*.md`,
`adr/*.md`, `ARCHITECTURE.md`, other PRDs), so they are invisible unless you go
looking — and a docs link-checker fails on them at merge time, in a file you then
have to hunt for. Fix them now. Find the inbound links:

```sh
git grep -lF "prds/<file>.md"
```

Use `git grep` — it searches the repo's tracked files via the index, so it is
unaffected by your working directory and by ignore rules that would make some
recursive `grep` builds (ripgrep, ugrep) skip a tracked-but-git-ignored file. Use
`-F` so the `.` in the filename is matched literally instead of as a regex wildcard.
For every file it lists, repoint the link so it resolves to the new location,
`prds/done/<file>.md`:

- The common case — a link written `](../prds/<file>.md)` or `](prds/<file>.md)` — is
  a plain literal substitution of `prds/<file>.md` → `prds/done/<file>.md`. It is safe
  to apply blindly: an already-correct `prds/done/<file>.md` reference does not contain
  the substring `prds/<file>.md`, so it is never double-prefixed into
  `prds/done/done/...`.
- The correct `../` prefix depends on the *linking file's own directory*, so the rule
  is "make each inbound link resolve to `prds/done/<file>.md`", not one blind sed over
  the whole tree. A sibling link from another PRD written `](<file>.md)` becomes
  `](done/<file>.md)`, not `](prds/done/<file>.md)`.
- Repoint the link *text* too when it is itself a path (e.g.
  `[prds/<file>.md](../prds/<file>.md)`) — a link checker flags display text that names
  a file which no longer exists, even when the target already resolves.

Then confirm you caught them all: `git grep -F "prds/<file>.md"` should return nothing
except matches inside the moved file's own new path.

Commit the PRD change with the rest of your work, on the run's branch. Do not stage
the whole tree to do it, and do not push.

## 4. Declaring the move

If (and only if) you moved the file, pass the new repo-relative path as
`prd_done_path` when you call `signal_done`, e.g. `prds/done/72-thing.md`. That is
what lets the issue's own link be corrected after the merge request lands. Omit it
when you did not move anything.

## 5. If you are the reviewer

Check the PRD diff against what the branch actually changed, the same way you check
the code. Specifically: does every newly-ticked box have something in this diff that
supports it, and does the move (if there is one) match a PRD with no open items left?
An unsupported completion claim is a review finding; send it back.

Be aware of what this control is and is not. It is a prompt-level instruction, so it
raises the floor and does not make a false claim impossible. On a run whose subagents
came from the repository being worked on, the reviewer is repo-authored and its
sign-off is not uzi's own review at all. The human reading the merge request is the
real check; your job is to make their job possible, by leaving the PRD diff small,
honest, and easy to compare against the code.
