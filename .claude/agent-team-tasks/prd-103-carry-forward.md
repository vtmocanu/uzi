# PRD #103 — carry-forward for M4, M5, M6

Written 2026-08-02 by the lead, at the end of M3's design + implementation, **before M3's validators reported.** Everything here was measured during M3 and is milestone-independent. It exists so M4/M5/M6 do not re-derive it, and because three separate agents walked into the first item below during one design wave.

**This is not M4's design.** Each remaining milestone still gets its own brief and its own design-critique wave.

---

## 1. EVERY golangci-lint FIGURE IS A CAP READING UNLESS IT CARRIES BOTH FLAGS

`--max-issues-per-linter` defaults to **50**, `--max-same-issues` to **3**. Any number quoted without `--max-issues-per-linter=0 --max-same-issues=0` is a cap reading, not a count.

```
api goconst    50 capped    1178 uncapped     <- 50 is EXACTLY the default. that is the tell.
api errcheck   36 capped      79 uncapped
controller     41 capped      41 uncapped     <- identical. THIS IS THE CAMOUFLAGE.
```

**The two flags are not redundant, and they COMPOSE IN ORDER rather than each owning a linter.** Measured by the lead 2026-08-02 on the shipped `goconst`-off config, `cache clean` first, each flag isolated:

```
A  --new-from-merge-base=                                 56   errcheck 36  staticcheck 13
B  --new-from-merge-base= --max-issues-per-linter=0       56   errcheck 36  staticcheck 13   <- NO CHANGE
C  --new-from-merge-base= --max-same-issues=0             78   errcheck 50  staticcheck 21
D  --new-from-merge-base= both                           107   errcheck 79  staticcheck 21
```

`--max-same-issues` folds duplicate messages **first**; `--max-issues-per-linter` then truncates the survivors at 50. So on this config `--max-issues-per-linter=0` **alone is a complete no-op** (nothing exceeds 50 until the fold is lifted), and errcheck only clears 50 once the other flag has fired.

**The trap this creates is worse than the original.** Anyone checking "is the second flag load-bearing?" the obvious way runs row B, sees 56→56, and deletes it — after which errcheck reads **50 forever**, which is the tell this whole section is about.

*(Corrected 2026-08-02, hours after this file was written. The first version said "one takes goconst 50→1178, the other takes errcheck 36→79", attributing each delta to a single flag. **That is the same carried-through-attribution error this section exists to warn about, written by the lead into the section warning about it** — and it was inherited from a Taskfile comment rather than derived. A tester independently endorsed the same wrong attribution from a 2-cell measurement that could not distinguish the hypotheses, while a reviewer's 2×2 refuted it; the matrix above is the lead's own re-derivation, run because the two disagreed. **Do not settle a disagreement by trusting the cheaper instrument.**)*

**The camouflage is what makes this survive review.** A careful person sanity-checks "do the caps matter?" on the cheap module, gets the same number both ways, and concludes no. That happened to the auditor, to the lead's ruling, and to the architect's own ruling — the architect caught it in its own prior work.

**The generalisation, which is the part that outlives golangci-lint:** a warning stated in section N does not protect section N+2 when both rest on the same underlying run. M3's Amendment 1 wrote the caps warning; Amendment 1's *own* next-but-one section then quoted a capped figure, because the two were derived by different people from one shared measurement.

## 2. `.gate_needs` AND `.publish_needs` ENUMERATE JOBS BY NAME

Any new CI job that is not in both is invisible to them. On a `v*` tag that means **every image and the OCI chart publish to Harbor while the new job is failing** — the pipeline reddens afterwards, the artifacts are already out. `validate:api`'s own comment calls this "this repo's signature failure shape".

- **M5 inherits this in full.** Its checks (`shellcheck`, `yamllint`, `gitleaks`, `govulncheck`, `ruby -c`) are repo-wide and cannot fold into a per-toolchain `validate:*` job, so it *will* open lint-stage jobs and every one of them needs adding to both lists.
- **The two lists are a second single-hot-line contention** that the PRD's Parallelization section does not name — it lists only `stages:` and the two `package.json`s. M3 moved `.gate_needs` 9→11.
- **Verify by parsing, never by grepping.** This repo lost a ServiceAccount from the chart for days because a grep found `kind:` at column 0 in a document where two objects had merged.

## 3. THE FAIL-OPEN / FAIL-LOUD-BUT-MISLEADING DISTINCTION

Three shapes seen in M3, and they need different fixes:

- **Fail-open (silent green).** `test -z "$(gofmt -l .)"` on an unparseable file; `oxlint` without severity pinned; a `sources:`-carrying Task target. Fix: make the recipe fail closed intrinsically (`|| exit 2` on the assignment), or pin severity in config.
- **Fail-loud-but-misleading.** An unresolvable `new-from-merge-base` ref does **not** skip the ratchet — it emits one `level=warning` line and reports the *entire* unfiltered backlog at exit 1. The reader concludes the linter found 165 problems and starts a burn-down; the fix is one `git fetch`. Fix: a **pre-flight assertion** that turns it into a one-line error. `exit 2` for "instrument broken", `exit 1` for "there are findings" — `task`'s own rc is 201 for both, so the distinction must live in the recipe.
- **Silent no-op.** A suppression comment naming a rule that does not match; a config naming a rule whose plugin is not declared (a *typo* is loud, an undeclared plugin is silent); `plugins:` replacing rather than extending the default list. Fix: a strip-and-restore control proving each suppression is load-bearing.

## 4. HOST-GLOBAL TOOL CACHES LIE ACROSS SIBLING WORKTREES

golangci-lint's result cache lives at `~/Library/Caches/golangci-lint` and is shared by every worktree, exactly like the Go build cache. A warm cache from worktree A replays A's absolute paths into worktree B; the diff processor cannot match a path outside the repo against `git diff` and drops everything, so **the ratcheted run reports `0 issues` while the unfiltered run stays red. The ratchet is the half that lies.**

Two worktrees at the same SHA is the normal state of a fresh `git new-wt`, and it is exactly the configuration of a tester reproducing a calibration from its own tree.

- **`golangci-lint cache clean` before each calibration arm**, and confirm the clean executed rather than assuming (zsh will not exec a command held in a string variable).
- **Assert the finding path is repo-root-relative. A `../` in the path is an invalid run, not a finding.**
- Expect the same class from any tool M4/M5/M6 adds that caches by content — check before trusting a green.

## 5. CALIBRATION BAR — FOUR PROPERTIES, NOT ONE

A red is not a control. Each arm needs all four:

1. **Non-zero exit.** `task` returns 201 on any failure, never the underlying code. Test for non-zero, never a number.
2. **The rule NAME in the output.** rc≠0 alone is satisfiable by an unrelated finding — measured: `oxlint --react-plugin -D correctness` exits 1 on a *different* rule while the one under test never fires.
3. **A sane path** (see item 4).
4. **Green on restore, verified with `git status`** — not a grep count. A count only means something if you already know how many occurrences should exist; the VCS does not require you to know that.

**Restore with a `cp`-based backup, never `git checkout --`** — it reverts to HEAD and silently wipes uncommitted work. A broken restore is worse than a broken mutation: it reddens loudly and reads as proof.

**And a companion that reports a single number needs a positive PAIR** — the unfiltered run must be nonzero where the ratcheted one is zero.

## 6. TASKFILE AND CI RULES THAT BOUND EVERY REMAINING MILESTONE

- **npm targets DELEGATE to a `package.json` script.** `Taskfile.yml`'s header: *"a target that reimplements the command drops them SILENTLY"*. M4's `knip` and M6's `@vitest/coverage-v8` both land here.
- **Never a bare `npx <tool>`** — it fetches from the network when the dep is missing, which a gate may not do.
- **No `sources:`/`generates:`/`status:`, no `dotenv:`, no `includes:`, no dynamic root `vars:`, no unquoted variable spliced into a `cmds:` line.** Branch names legally contain `;`, `$` and backticks, and the MR author picks the target branch.
- **The CI job provides the tool, never devbox.** Root `devbox.json` is tier-2 *worker* config read into opted-in runs by `agent/src/repo-tools.ts` and bounded by shape only. Do not put dev tooling there.
- **Prefer a pinned `go run pkg@version` inside the already-pinned `golang:1.26`** over a new image family. M3 measured golangci-lint this way at 51.6s cold / 0.89s warm; it pins the version identically local and CI, and leaves `go.mod` untouched. The official `golangci/golangci-lint` image was rejected because it ships Go 1.26.2 with `GOTOOLCHAIN=auto` against both modules' `go 1.26.4`, so it downloads a toolchain at job time — the pipeline's first unpinned effective toolchain.
- **Give a new job its own `cache:` prefix** if it builds something the jobs sharing that key do not. `validate:api` and `test:api` share `.go_job`'s key without building golangci-lint, so whichever finished last would have decided the next pipeline's lint warmth.

## 7. `npm ci` AND `npm install` IN `agent/` BREAK `agent-browser` HOST-WIDE

`agent-browser`'s postinstall rewrites `/opt/homebrew/bin/agent-browser`, clobbering the brew symlink for every session on the machine; deleting the worktree afterwards makes it a permanent dangling link. npm 11.17 prints `npm warn allow-scripts … not yet covered by allowScripts:` naming the package — **which reads as "these were skipped" and is advisory. The postinstall runs anyway.**

**Do not install; symlink `node_modules` from a long-lived worktree instead.** No install step, no postinstall, and faster. Repair, if it happens: `brew unlink agent-browser` then `brew link --overwrite agent-browser` — neither works alone.

## 8. INSTRUMENT DEFECTS THAT COST TIME IN M3

- **`grep` here is ugrep.** Use `-F` for literals. A non-`-F` grep for a pattern containing `^` or `---` is read as a regex and can return 0 for a string present many times. Negated bracket classes misbehave in POSIX modes; `-P` is the escape hatch.
- **`git grep` when the question is about tracked content.** `.claude/agent-team-tasks/` is gitignored **and** force-added, so recursive greps that honour ignore files skip it entirely. `--hidden` is the wrong axis and changes nothing. Plain `git check-ignore` fails open on a tracked file.
- **`grep -v` can eat every line and return empty**, which reads identically to "no matches".
- **Sweep for the CLAIM, not the wording.** A literal search for `docs/dev-conventions.md`'s *"There is no linter"* finds `specs/ai.md` and **misses `.claude/agents/coder.md` entirely**, which phrases the same claim as *"this repo has no linter yet"*. The lead hit this while verifying someone else's report and nearly reported them wrong.
- **`$?` after a pipe reads the last command.** Redirect to a file, read `$?` on the very next line, then grep the file. The lead hit this too, on `golangci-lint config verify | tail`.
- **zsh does not word-split unquoted variables.** A command held in a string variable will not exec; a space-joined list of paths arrives as one argument.

## 9. REPORTING DISCIPLINE FOR THE REMAINING WAVES

M3's validator reports were long and correct. They also cost the lead a great deal of context, and there are three milestones left.

**For M4/M5/M6, dispatches should ask for:** findings as a short ranked list; every measurement written to a file inside the worktree with the report naming the path; the tip SHA first; and no inline pasting of transcripts the lead can read. The evidence must still exist — it must simply not all arrive in a message.

---

# ADDENDUM — 2026-08-02, from M3's validation wave (after the design wave that produced items 1-9)

## 10. golangci-lint TAKES A HOST-GLOBAL LOCK, NOT JUST A HOST-GLOBAL CACHE

Item 4 covers the shared result *cache*. The **lock on the same directory** is the other half, and it is the one that bites a team running several agents at once:

```
[lint:api] Error: parallel golangci-lint is running
[lint:api] The command is terminated due to an error: parallel golangci-lint is running
[lint:api] exit status 3
```

It does not queue and does not retry — it exits immediately, which through `task` becomes rc 201 and **reads as a lint failure**. Hit live by the reviewer (which lost three measurement runs to it) and by the auditor, from different worktrees, during M3's validation wave.

It fails in the safe direction (false red, never false green) but three things make it worth documenting: this repo's layout is a bare clone with many sibling worktrees; this team runs concurrent agents by design, so the collision condition is the normal state; and **`exit status 3` sits outside the Taskfile's own convention** (2 = broken instrument, 1 = findings), so the status cannot discriminate either. An automated reader testing `!= 0` records a RED gate over code that is fine.

**Operationally, for M4/M5/M6: serialise anything that invokes golangci-lint across agents.** The lead must not have a tester calibrating while an auditor lints — and the tester's `cache clean` discipline (item 4) is defeated outright by a concurrent run from another worktree re-warming it.

## 11. A THIRD FOLD NOBODY NAMED: `issues.uniq-by-line` DEDUPS ACROSS LINTERS

Defaults to **true**. So a linter's reported count depends on **which other linters are enabled**:

```
shipped config, both cap flags cleared                        107   staticcheck 21
  ... plus --uniq-by-line=false                               108   staticcheck 22
with goconst additionally enabled, uniq on                   1284   staticcheck 20
with goconst additionally enabled, uniq off                  1622   staticcheck 22
```

One finding of 108 today, which is why M3 did not restate its figure. **The reason it matters is M4 and M5, which add linters**: this is exactly how a `staticcheck` finding disappears from an "unfiltered" total without anyone touching staticcheck. Three folds now, not two — quote a count with all three cleared, or say which are not.

## 12. A NEW devDependency SILENTLY BREAKS EVERY EXISTING CHECKOUT'S GATE

`lint:web`/`lint:agent` → `npm run lint` → `oxlint`, a devDependency added by the branch. `npm run` puts `node_modules/.bin` on PATH and fails closed with `command not found`, so every checkout that does not reinstall gets a red gate that names a missing binary rather than a lint finding.

**And in `agent/` the required remedy is the documented host-wide `agent-browser` clobber**, so a reader hits `oxlint: command not found`, finds the hazard documented, and finds no safe form — because the standing advice (symlink `node_modules` rather than install) **cannot apply to someone adding a devDependency**, which is precisely the operation that must write `package.json` and the lockfile.

**The safe form is `npm install --ignore-scripts --save-dev --save-exact <pkg>@<version>`.** Not a general-principle suggestion: this repo already settled that flag for npm with its own measurements in `agent/src/js-deps.ts` (PRD #121), including that **a repo `.npmrc` with `ignore-scripts=false` does not override the CLI flag**. Caveat to state alongside it: `--ignore-scripts` can leave *other* packages unbuilt, and the recovery for that is the symlink route, never an unguarded re-install in `agent/`.

**Any milestone adding a devDependency owes a CHANGELOG line and a `docs/dev-conventions.md` line saying so.** M4 adds `knip` to both packages; M6 adds `@vitest/coverage-v8` to `web/`.

## 13. THE FAILURE THIS WAVE ACTUALLY PRODUCED, TWICE, IN BOTH DIRECTIONS

Both instances are the same shape and neither was a careless agent.

- **A tester endorsed a wrong comment from a 2-cell measurement.** It measured the endpoints (56, 107), which are consistent with several mechanisms, and reported the attribution it found in the comment it was checking. A reviewer's 2×2 refuted it. **An instrument that cannot return the disconfirming answer is not evidence**, and the endpoints could not.
- **The lead wrote the identical error into item 1 of this file** — the section warning about carried-through numbers — hours after criticising it in others, and inherited it from the same Taskfile comment.

**The generalisable rule, which is not "measure more carefully":** when two agents disagree on a measured fact, do not pick the more credible agent. Ask which instrument could have produced the disconfirming answer, and re-derive it yourself. Both times here, the cheaper instrument agreed with the existing written claim — which is the direction that never gets re-checked.
