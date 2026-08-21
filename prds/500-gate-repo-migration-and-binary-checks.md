# PRD #500 — Two gate:repo structural checks: migration-number collision + binary/control-byte text files

**Issue**: [#500](https://github.com/vtmocanu/uzi/issues/500)
**Priority**: Medium
**Status**: Draft — ready for implementation

> **Path convention**: every path is relative to the repo root. The change adds **two POSIX-sh scripts + their canary fixtures** and wires each into the repo-wide gate in `Taskfile.yml`. No Go, no migration, no SQL, no web, no worker change. Load `.claude/rules/go.md` (it owns `Taskfile.yml`) before starting.
>
> **This PRD is destined for an offline uzi worker.** Both checks use only `sh`/`git`/`awk`/`sort`/`perl` (all present on the worker and the dev host), run sub-second with no DB, no Docker, and **no network**. Every fact below was verified on 2026-08-21 with its `file:line`. The checks ride the **existing** `task gate:repo` CI step, so **no file under `.github/workflows/**` is created or modified** in implementation or validation (`.claude/rules/prds.md`) — this is a hard requirement for a uzi run and is the reason a `gate:repo` script is the right home rather than a new CI job.

## Problem

Two classes of defect reach the tree today with **no mechanical guard**, and both were found by a worker improvising a fragile manual workaround:

1. **Duplicate goose migration numbers.** goose (`v3.27.3`, `api/go.mod:25`) **panics** `goose: duplicate version … detected` on two migrations sharing a prefix — at API boot **and** in every `*LiveDB` test that calls `store.Migrate`. A run whose new migration collided with the base tree's numbering had to hand-`mv` the file above the live head under a shell trap and revert it just to run the LiveDB suite. The numbering convention ("assigned at merge time, renumber above the live head", CLAUDE.md) is documented as prose and enforced **nowhere**.

2. **Binary / control-byte source files.** A NEW text-extension file that git treats as **binary** (a raw NUL in the first ~8000 bytes) — or that carries other control bytes — passes lint, typecheck, all tests, and check-styles, because the bytes are behaviorally invisible. One such file was caught **only** because the lead happened to notice `git diff --stat` reporting it as `Bin 0 -> N`. There is no `.gitattributes` and no `--numstat`/`check-attr`/NUL scan anywhere in the repo, so git classifies purely by its content heuristic and nothing gates on the result.

Both are the same species as the checks already in `gate:repo`: offline, git/awk-only, whole-tracked-tree structural checks with an in-band liveness canary.

## Solution

Add two scripts under `scripts/`, each cloned byte-for-byte in structure from `scripts/check-spec-numbering.sh` (POSIX `sh`, `set -eu`, `cd "$(git rev-parse --show-toplevel)"`, **exit codes 2 = instrument broken / 1 = findings / 0 = clean-and-canary-fired**, an in-band liveness canary passed as an explicit arg so the Taskfile echo shows it), and wire each into `gate:repo`'s `cmds:` list:

1. **`scripts/check-migration-numbering.sh`** — uniqueness-only over the 5-digit prefix of `api/internal/store/migrations/*.sql`.
2. **`scripts/check-binary-text.sh`** — `git diff --numstat` empty-tree floor for git-binary files + a control-byte content scan for the non-NUL case.

They are bundled into one PRD because they are the same pattern and both edit the same `gate:repo` cmds block in `Taskfile.yml` — shipping them separately would merge-conflict on that block.

---

## Background — current state (resolved facts)

### The `gate:repo` seam and its two sibling checks

- `gate:repo` — `Taskfile.yml:213-258` — is the repo-wide "tree-fact" gate for checks that belong to no component. CI runs it verbatim via the `lint-repo` job's `task gate:repo` step (`.github/workflows/ci.yml:386-391`), so **adding a `- task:` line to `gate:repo`'s `cmds:` gates it in CI with zero workflow-file edit** (the documented "seam held" property).
- **`scripts/check-spec-numbering.sh`** + `check:spec-numbering` (`Taskfile.yml:703-709`, wired at `:252`): a uniqueness-only duplicate detector for `specs/ai.md` section numbers. All-`awk`, a **liveness canary** (`scripts/spec-numbering-canary.md`) with a planted duplicate that must fire, exit codes **2/1/0**. **This is the template to clone for both new scripts.**
- **`scripts/check-migration-additive.sh`** + `check:migration-additive` (`Taskfile.yml:711-720`, wired at `:257`): scans migration Up sections for destructive changes (PRD #422 M6); takes a **canary file + the migrations dir as explicit args** — the precedent for a script that walks `api/internal/store/migrations`.
- `gate:repo` needs only sh/git/awk-class tooling for these checks (no `*_REQUIRED` env, no download), unlike `scan:secrets` (gitleaks) / `lint:shell` (shellcheck).

### Migration numbering — current facts

- `api/internal/store/migrations/` — convention `NNNNN_slug.sql`; **current head `00140_github_project_sync.sql`**; **no duplicate prefixes on `main` today** (verified). Gaps are **intentional** (e.g. `00002→00010`, `00021→00029`), so the check must be **uniqueness-only, never contiguity/order**.
- `api/internal/store/migrate.go:14-15` `//go:embed migrations/*.sql` — the running binary reads the **embedded** FS. `Migrate` → `goose.UpContext` (`:81`), legacy strict API, no allow-missing/out-of-order.
- goose `v3.27.3` behavior (verified in the module cache): duplicate prefix → **panic** `goose: duplicate version %v detected` (sort comparator); version below applied head → returned error `found N missing migrations before current version`. A fresh throwaway Postgres only trips the **duplicate panic**.
- CLAUDE.md convention (verbatim): *"Goose migration numbers are assigned at merge time … rename each new migration to the next free number above the live head … The boot runner is strict goose."* `.claude/rules/go.md` covers migrations only for the `+goose` parser hazard, **not** numbering.

### Binary/control-byte — current facts

- **No `.gitattributes` anywhere** (`git ls-files | grep gitattributes` → nothing). So git classifies by content heuristic, and **git's heuristic is NUL-presence only**: a file with a raw NUL in the first ~8000 bytes is "binary"; a file with other control bytes but no NUL is **not** binary to git. This is why the second clause needs a supplementary scan.
- **No existing `--numstat`/`--stat`/`check-attr`/`grep -I`/NUL scan** in `Taskfile.yml` or `scripts/` (the only "binary" hits are about downloaded tool binaries in `govulncheck-gate.sh`/`golangci-lint.sh`).
- The three diff-inspection surfaces that DO exist inspect **names**, not bytes: the worker guard flow (`agent/src/git.ts:783` `changedFiles`, `--name-only`, fail-open), the LLM reviewer (`review-runner.ts:135` → `git.ts:808`, renders a binary hunk as `Binary files … differ` so the model sees nothing), and `self-improve.ts:104` (`go test`/`npm test`/`build`/`typecheck` — the exact green set the escape passed). Server-side `sanitize.go`/`agent/src/sanitize.ts:37` strip NULs from **tool_result JSON payloads** (so Postgres accepts them), unrelated to source files in a diff.
- **Verified offline primitives**: `git diff --numstat 4b825dc642cb6eb9a060e54bf8d69288fbee4904 HEAD -- <pathspecs>` (empty-tree constant) currently reports **zero** binary-treated text files, so a zero-baseline floor passes today and reddens the moment a NUL-bearing `.go`/`.ts` is committed; `--numstat` marks a binary row with `-` in both count columns and needs no external diff driver. `perl -ne 'exit 1 if /[\x00-\x08\x0b\x0c\x0e-\x1f]/'` returns rc=1 on a control-byte file (prefer perl / `git grep -P` over plain `grep` — the root CLAUDE.md documents ugrep's negated-class misbehavior in POSIX modes).

---

## Design decisions

1. **Both checks are `gate:repo` scripts, not CI jobs and not agent-side checks.** The escape happened in uzi's own repo gate ("lint, typecheck, tests, check-styles green"), the `gate:repo` seam auto-wires to CI with no workflow edit (mandatory for a uzi run), and the two sibling scripts prove the exact shape. An agent-side check would be weaker (the worker guard flow is `--name-only`, fail-open; the LLM review is non-deterministic) and would not protect uzi's own contributor/CI loop where the escape occurred. A worker-side check for **user** repos is a legitimate future extension, noted, not built here.

2. **Whole-tracked-tree floor, empty-tree base — NOT a `new-from-merge-base` ratchet.** A "NEW-only" base (`origin/main...HEAD`) would need `git fetch origin main` to resolve, a **network op** the offline worker cannot do. The empty-tree whole-tree floor (like `scan:secrets`/`check:spec-numbering`) is offline and, because the tree is clean today, is a zero-baseline that only reddens on a new offender. Same reasoning for the migration check: it scans the whole `migrations/` dir.

3. **Uniqueness only for migrations; never gaps/order.** Intentional gaps exist; the check reports a prefix appearing on >1 file and nothing else. On a duplicate, the message names both files and instructs *renumber one above the live head* (the CLAUDE.md convention), not "close the gap". On clean, it prints the head number and `next landing number = head+1` as a positive observation (mirroring `check-spec-numbering.sh:146-149`). Canonicalize the prefix with arithmetic (`+ 0`) so a mis-padded `0109` collides with `00109`.

4. **The binary check has two clauses because git only sees NUL.** Clause 1: `git diff --numstat <empty-tree> HEAD -- <text-extension pathspecs>` flags any row with `-`/`-` counts (git-binary). Clause 2: a `git ls-files`-driven content scan over the same text-extension set for bytes outside `\t \n \r` + printables (`perl` or `git grep -P`), catching the non-NUL control-byte case git's heuristic misses. Both are index-aware (`git diff`/`git ls-files`), so a force-added-but-ignored canary is naturally in scope (the root CLAUDE.md ignored-but-tracked hazard). **Clause 2 subsumes clause 1's NUL detection** (it catches a NUL anywhere, not just in the first 8KB); clause 1 is kept as defense-in-depth that mirrors git's own `diff --stat` view.
   - **🔴 Canary-scope hazard, unique to THIS check.** Unlike the migration/spec checks — whose real scan targets a directory/file **disjoint** from their canary — this check's real clauses scan the **whole tree**, so a committed NUL-bearing `scripts/binary-text-canary.<ext>` whose extension is in the pathspec set would be reported by clause 1 (verified: `*.go` matches at any depth) → the gate goes **permanently red on its own canary**, and Success Criterion 2's conjunction ("exits 0 on the current tree" AND "canary fires every run") becomes unsatisfiable. **The whole-tree clause-1/clause-2 scans MUST exclude the canary path** (append `:(exclude)scripts/binary-text-canary.*` to the pathspecs), while the canary-first liveness step scans the canary **explicitly**. (Alternative: give the canary an extension outside the pathspec set; the explicit-scan step then hard-codes that path. The `:(exclude)` form is preferred — it keeps the canary a real text-extension file so it exercises the true detector.)

5. **Each script carries its own liveness canary as an explicit arg.** Following `check-spec-numbering.sh:95-110`: the script runs its detector over a committed canary carrying the planted defect **first**, and exits 2 if the canary does not fire — so a green is a positive observation, never a vacuous pass. No separate Go test is needed (spec-numbering and migration-additive rely solely on the in-band canary). Pass the canary path(s) as explicit args so the Taskfile echo shows them (house convention).

## Scope

**In scope**:
- **Add `scripts/check-migration-numbering.sh`** + a canary directory `scripts/migration-numbering-canary/` holding two colliding-numbered stubs (e.g. `00001_a.sql`, `00001_b.sql`), force-added if ignored.
- **Add `scripts/check-binary-text.sh`** + a canary fixture `scripts/binary-text-canary.<ext>` (a text-extension file with a planted NUL/control byte), force-added if ignored.
- **`Taskfile.yml`**: two new targets `check:migration-numbering:` and `check:no-binary-text:` (alongside `check:spec-numbering`/`check:migration-additive`, ~`:703-720`), each invoked with explicit canary + dir args; add both `- task:` lines to the **`gate:repo`** `cmds:` list (~`:246-258`, in the sub-second-first group before `scan:secrets`).
- **Doc pointer**: a one-line note in `.claude/rules/go.md`'s migrations area (and beside the CLAUDE.md numbering convention if concise) that the collision is now gated by `check:migration-numbering`; a one-line note that new binary/control-byte text files are gated by `check:no-binary-text`. **Same-file doc correction** (since the worker is editing `.claude/rules/go.md` anyway): its `+goose` paragraph cites goose **`v3.27.2`**, but `api/go.mod:25` pins **`v3.27.3`** — fix the stale version to `v3.27.3` in the same edit (the CLAUDE.md rule: correct a doc the moment work proves it stale).

**Out of scope**:
- Any **provisional-head migration runner** (rejected — migrations are `go:embed`-compiled, so it would have to rename on disk and recompile, i.e. automate exactly the fragile `mv`-under-trap; preventing the collision early is the clean fix).
- A worker-side binary/control-byte check for **user** repos (future extension).
- A `.gitattributes` overhaul.
- Any CI-job / `.github/workflows/**` change (the checks ride the existing `gate:repo` step).

## Milestones

- [ ] **M1 — `check:migration-numbering` (script + canary + wire).** Add `scripts/check-migration-numbering.sh` cloned from `check-spec-numbering.sh`: walk `api/internal/store/migrations/*.sql`, extract **the leading digit run before the first `_`** per filename and canonicalize it with `+ 0` (so a mis-padded `0109` still collides with `00109` — do NOT grab a fixed-width `[0-9]{5}`, which would miss a mis-padded number and defeat Decision 3), report any prefix on >1 file with both filenames and the "renumber above head" instruction; print head + `next = head+1` on clean; exit **2/1/0**; run its detector over `scripts/migration-numbering-canary/` (two colliding stubs) first and exit 2 if the planted duplicate does not fire. Add `check:migration-numbering:` to `Taskfile.yml` with explicit canary-dir + migrations-dir args, and `- task: check:migration-numbering` to `gate:repo`. **Validate**: `task check:migration-numbering` is green on the current tree (head `00140`, no duplicates) and its canary fires; drop a temporary second `001XX_*.sql` sharing a live prefix into a **scratch copy** (never the repo) and confirm the script exits 1 naming both files; `task gate:repo` stays green.

- [ ] **M2 — `check:no-binary-text` (script + canary + wire).** Add `scripts/check-binary-text.sh` cloned from `check-spec-numbering.sh`: clause 1 `git diff --numstat 4b825dc… HEAD -- <text-extension pathspecs> ':(exclude)scripts/binary-text-canary.*'` flags `-`/`-` rows; clause 2 a `git ls-files -- <text-extension pathspecs> ':(exclude)scripts/binary-text-canary.*'`-driven `perl`/`git grep -P` scan for control bytes outside `\t\n\r`+printables over the same set; report offending files; exit **2/1/0**; run its detector over `scripts/binary-text-canary.<ext>` (planted NUL) **explicitly** first and exit 2 if it does not fire (force-add the canary if `.gitignore` would drop it, and verify with `git ls-files` that it is tracked). **The `:(exclude)` on both whole-tree clauses is load-bearing** (Decision 4): without it the committed canary flags itself and the gate is permanently red. Add `check:no-binary-text:` to `Taskfile.yml` with the explicit canary arg, and `- task: check:no-binary-text` to `gate:repo`. **Validate**: `task check:no-binary-text` is green on the current tree (zero binary-treated text files) and its canary fires; write a NUL-bearing `foo.go`/`foo.ts` into a **scratch copy** and confirm the script exits 1 naming it; `task gate:repo` stays green.

- [ ] **M3 — Doc pointers.** Add the one-line pointers per Scope so a future reader learns the collision/binary classes are now gated (and by which target). **Validate**: `task gate:repo` and `task gate` pass end-to-end with both new checks in the chain.

## Success criteria

1. `task check:migration-numbering` exits 0 on the current tree, prints the head number and next landing number, and its liveness canary fires on every run; it exits 1 naming both files when two migrations share a prefix (proven in a scratch copy), and never fails on the intentional numbering gaps.
2. `task check:no-binary-text` exits 0 on the current tree (its committed canary is excluded from the whole-tree clauses via `:(exclude)scripts/binary-text-canary.*`), its liveness canary fires on every run (scanned explicitly), and it exits 1 naming the file when a new text-extension file contains a NUL (clause 1) or a non-NUL control byte (clause 2) — the latter being the case git's own binary heuristic misses.
3. Both checks are members of `gate:repo`'s `cmds:` list, so `task gate:repo` (and therefore the `lint-repo` CI job) runs them with **no** `.github/workflows/**` change.
4. Both scripts are offline (sh/git/awk/sort/perl only), sub-second, need no DB/Docker/network, and carry no `*_REQUIRED` fail-open guard.
5. `main` is never touched; delivered on a branch + PR.

## Risks & mitigations

- **Vacuous green** (a check that passes because its detector silently found nothing to look at). Mitigation: Decision 5's in-band canary + exit-2-on-canary-miss, cloned from `check-spec-numbering.sh`.
- **Migration check flagging intentional gaps.** Mitigation: Decision 3 — uniqueness only, gaps allowed; validated against the current tree's real gaps.
- **git's binary heuristic missing non-NUL control bytes.** Mitigation: Decision 4's clause 2 supplementary scan.
- **ugrep negated-class misbehavior** if plain `grep` were used for clause 2. Mitigation: use `perl`/`git grep -P` (root CLAUDE.md).
- **Canary invisible to the sweep** if force-added into an ignored path. Mitigation: the scripts use index-aware `git diff`/`git ls-files`, and M2 verifies the canary with `git ls-files` (the ignored-but-tracked hazard, root CLAUDE.md).
- **Adding a file under `probes/`** could redden `gate:repo` (root CLAUDE.md: `scan:secrets`/`lint:shell`/`lint:yaml` walk tracked files there). Mitigation: canaries live under `scripts/`, not `probes/`, and are plain fixtures; `lint:shell`'s `git ls-files -- '*.sh'` will lint the two new scripts, so they must be shellcheck-clean.
- **`--numstat` empty-tree base drift.** The `4b825dc…` empty-tree hash is a git constant; no risk. `--numstat` needs no `--no-ext-diff` (it computes counts internally).

## Dependencies

- **No external / internet dependency.** Every primitive is local git/perl/awk; the target tree today is clean for both checks (zero-baseline), so the offline worker can complete and verify this fully.
- **No shared-file collision** with the other batch PRDs: this touches only `scripts/` and `Taskfile.yml` (plus a doc line); no other PRD in the batch edits `Taskfile.yml`.
- The migration check's canary must NOT be picked up by goose: keep the canary stubs under `scripts/migration-numbering-canary/`, **not** under `api/internal/store/migrations/` (goose `//go:embed`s only the latter, so a canary outside it cannot reach the runner).

## Decision log

- **2026-08-21**: Scoped from two `improve_uzi` recommendations ("migration number collision detection against live main", seen in 2 runs; "review dispatch new text file binary nul byte detection", seen in 1 run). Bundled into one PRD because both are `gate:repo` scripts editing the same `cmds:` block — separate PRDs would merge-conflict there.
- **2026-08-21**: **Rejected the provisional-head migration runner** in favor of an early collision gate: migrations are `go:embed`-compiled, so a runner would have to rename on disk and recompile (automating the fragile manual `mv`-under-trap); a gate that catches the collision up front makes the agent renumber once, correctly.
- **2026-08-21**: **Whole-tree empty-tree floor, not a `new-from-merge-base` ratchet** — the ratchet needs `git fetch origin main` (network), which the offline worker cannot do; the tree is clean today so a zero-baseline floor is equivalent and offline.
- **2026-08-21**: **Binary check needs two clauses** — git's binary heuristic is NUL-only, so a `--numstat` floor alone misses non-NUL control bytes; clause 2 adds a content scan.
- **2026-08-21**: Both scripts modeled byte-for-byte on `check-spec-numbering.sh` (exit-code convention + in-band canary), the lightest sufficient precedent.
- **2026-08-21**: Next step = send to uzi (Auto). PRD authored fully internet-independent and workflow-file-free.
