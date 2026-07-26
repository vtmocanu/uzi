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
