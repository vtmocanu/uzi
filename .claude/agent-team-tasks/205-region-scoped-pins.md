# Issue #205 — region-scope the lead.md phrase pins

Brief for the agent-team run. Branch `fix/205-region-scoped-pins`, worktree
`/home/user/repos/myorg/vtmocanu/uzi/fix-205`, cut from `main` at `cb14a835`
(which includes #197, merged as `cb14a835`).

**Force-added to git** (`.gitignore:44` ignores `.claude/agent-team-tasks/`). Two
consequences, both documented in `CLAUDE.md`: a recursive `rg`/`grep` will **not** find it
(use `git grep`), and **`git add` needs `-f` every time even though the file is tracked** —
a plain `git add` prints the ignore hint and silently does nothing.

## The spec

**The issue is the spec.** It carries the full measurement set, the prototype's results, the
guard, and the reasoning for every choice:

```sh
env -u GITLAB_TOKEN glab issue view 205 --repo vtmocanu/uzi
```

Read it before anything else. This brief adds only the run's shape.

## One-paragraph summary

`render_test.go` asserts each pinned phrase with `strings.Contains` over the **whole
flattened template**, so a phrase satisfies its pin from anywhere. That makes the pins blind
to a clause being *moved* between the plan-turn paragraph and the post-implementation
bullet. Scope each assertion to its **region** and relocation fails by construction.

## What #197 already established, so nobody re-derives it

- The class was found in **three consecutive rounds**, each time on phrases anchored in the
  round before. The last miss was the pin guarding the previous round's blocking fix.
- **Anchoring is a manual per-phrase property.** #197 made it machine-checked (an `anchor`
  field plus four asserted table properties), which closed the *quality* half. It cannot
  close the *expressiveness* half: a substring is position-independent by definition.
- The residual is recorded in `specs/ai.md` §467 as one root with two consequences. **This
  change closes the second (expressiveness); it does NOT close insertion.**
- #197's three remaining hand-anchors are **expected throwaway** — this change deletes them.

## Design decisions, already settled by measurement — do not re-litigate

**D1 — three guard clauses, all measured necessary, none redundant.**
1. `strings.Count(body, marker) != 1` → `Fatalf`. Catches absent **and** duplicated boundary.
2. Both regions above a floor length → `Fatalf`. Catches degenerate splits clause 1 allows.
3. Cross-contamination: the plan region must not contain a bullet landmark, and vice versa.
   **This is the only clause that catches a boundary that MOVED rather than vanished** —
   count==1 holds and both sizes look plausible, so 1 and 2 both miss it.

**D2 — the naive form is STRICTLY WORSE than what ships today. It must not land.**
`strings.Cut` on a missing separator returns `(whole, "", false)`, so the before-region
silently becomes the **entire body** — every plan assertion reverts to today's whole-body
semantics, still passing, no longer scoped, and nothing says so — while the after-region is
empty and reds. One correct-looking red naming the *bullet* case, concealing seven quietly
disarmed assertions. **Expect a reviewer to propose deleting clause 3 as belt-and-braces.
Refuse, and say why in the comment.**

**D3 — phrases shrink and the overlaps go.** Measured 717 → 375 chars (−48%), pairwise
disjoint restored, every phrase occurring exactly once **in its own region**, no containment,
and `D2 ⊂ P1` dissolving on its own. Once the region carries position, the phrase carries
meaning alone — so **drop the anchors that exist only to carry position**, including the
`anchor` field if it no longer earns its place. State that in the comment rather than leaving
a reader to wonder why #197's machinery vanished.

**D4 — this is a STRICTER definition of correct, and it must be declared.** A
relocated-but-present rule is stated *somewhere*; region-scoping treats wrong-section as a
failure. Coherent for an issue about which turn the wave runs in, but it is a real change in
what the test means. Say so in the comment rather than letting it be discovered.

**D5 — insertion stays open under this design too** (measured: the prototype passes the
insertion mutant). Keep #197's insertion note and do not let "regions closed relocation" read
as "regions closed the problem".

## The prototype

`/private/tmp/claude-1542763654/-Users-vmocanu-stuff-gitrepos-mm-vtmocanu-uzi/054a9d40-be99-489e-b412-d4d02e98f40e/scratchpad/tester-region_proto_test.go.candidate`

**Evaluation code, guard included, NOT merge-ready.** It lives in a session scratchpad and
may not survive. **Re-derive from the issue rather than assuming it is still there or
correct** — the issue records what it measured, which is the durable part.

## Verification plan — and who does what

**The tester wrote the prototype and its guard, so it cannot be the independent check on
them.** That is the whole reason #205 exists as a separate change rather than riding #197.

- **auditor — the independent guard fold round.** It volunteered and already has its first
  fold designed: *delete the region boundary and confirm the guard reds rather than
  degrading to whole-body matching.* If it degrades quietly, the guard is worse than no
  guard, because it converts a known limit into a believed-closed property.
- **tester — independent relocation folds** against the new instrument, including its own
  R1/R2/R3 from #197, plus whether region-scoping opens a class the whole-body form did not
  have.
- **reviewer / fact-checker** — normal scope; the comment's claims are the thing to check,
  since this change's whole product is a claim about what the test can detect.

## Gate

```sh
task gate:api      # ~43-66s; this change is api-only
```

Not `gate:web` — no docs change is expected. Recipes live in the root `Taskfile.yml`; never
restate one. **`task` exits 201 on any failure**, never the underlying code — test non-zero,
and the composite verdict is the exit code. Redirect to a file and read `$?` on the next
line; `cmd | tail -3 && echo OK` reports green over a failure on this host, and
`${PIPESTATUS[0]}` is bash-only while this shell is zsh.

## Standing rules

- No amends once a SHA is dispatched for review; fixes land as follow-ups.
- Lead every report with the tip SHA.
- **Mutate in a throwaway detached worktree**, never the shared one — three agents collided
  in #197's tree and one red was briefly read as a regression.
- **Namespace every scratchpad log** with your role; `gate-api.log` was clobbered three times.
- `cp` backups for mutation restore, never `git checkout --`.
- Commit locally on `fix/205-region-scoped-pins`. Never push, never touch `main`.

## Amendments

- *(none yet)*
