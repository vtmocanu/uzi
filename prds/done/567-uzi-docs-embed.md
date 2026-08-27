# PRD #567: uzi docs — embed the app docs corpus in the CLI binary + `uzi docs` verbs

**GitHub Issue**: [vtmocanu/uzi#567](https://github.com/vtmocanu/uzi/issues/567)
**Status**: Complete (2026-08-23)
**Priority**: Medium
**Created**: 2026-08-22
**Scope**: `api/` (CLI + a new `internal/uzidocs` package), `Taskfile.yml`, `scripts/`, `docs/cli.md`, `Formula/uzi-cli.rb`, and `api/internal/uzicli/skill/SKILL.md`. **No `.github/workflows/**` change** in implementation or validation (see *Offline-worker notes*). No DB, no forge-driver, no web-runtime change (web already renders these docs; this PRD does not alter that path).
**Anchors**: line numbers below are against `main` at `4e82fd5d7`. Re-derive them at implementation time (`grep -n` the quoted strings) rather than trusting the numbers.

## Problem

An agent (Claude Code or any consumer) that a user talks to for help with uzi has the **command surface** — the `uzi-cli` skill documents every verb — but not the product's **conceptual and onboarding knowledge**. That knowledge lives in `docs/*.md` (51 files, 39 with `audience: user`: `getting-started`, `anthropic-token`, `worker-setup`, `autopilot`, `chat`, `oidc`, and so on), and today it is reachable only two ways:

- rendered in the web app at `/docs/:slug` (web bundles `docs/*.md` at build time via `import.meta.glob` in `web/src/lib/docs.ts:8`), or
- read straight from the repo on disk.

Neither is available to an agent helping a user get started from a terminal. So the agent guesses, or reaches for the open web (stale, and unavailable to a uzi worker, whose egress excludes the open internet). There is also no shared retrieval path between the in-app docs and the CLI, so nothing guarantees a terminal answer matches the `/docs` page.

## Solution Overview

Ship the **same** `docs/*.md` corpus the web app renders **inside the CLI binary** via `go:embed`, and expose it through offline `uzi docs list|show|search` verbs. Then teach the `uzi-cli` skill to route onboarding / how-to / "what is X" questions to `uzi docs` instead of guessing.

The chain the user asked for:

1. user asks an agent a product question ("how do I connect my forge?", "what is the plan gate?");
2. the `uzi-cli` skill (already installed at `~/.claude/skills/uzi-cli/`) tells the agent: for concepts/onboarding, run `uzi docs search <query>` then `uzi docs show <slug>`;
3. the agent runs `uzi`, which reads the docs **embedded in its own binary** — no server, no token, offline (like `uzi version`);
4. the agent answers from that real, version-matched text.

**One source of truth stays `docs/*.md` at the repo root.** Web consumes it at build time; the CLI consumes a committed, drift-checked mirror embedded into the api module. Both derive from `docs/`, so the terminal answer and the `/docs` page cannot disagree.

**The skill holds a pointer, not the docs.** SKILL.md is already 61 KB and loads into every Claude Code session's context; inlining the docs corpus (≈ 564 KB) would bloat that. The skill documents *how to retrieve* (`uzi docs …`); the binary holds the corpus; retrieval is on demand.

### Why the docs must be embedded (not shipped as loose brew files, not fetched from a server) — the load-bearing facts

Three facts, each verified locally, settle the mechanism:

1. **`docs/` sits at the repo root, above the `api/` Go module.** `go:embed` patterns cannot reference paths outside the embedding package's directory tree (no `..`). So the docs cannot be embedded in place; they must be brought into the api module tree as a committed artifact for `go:embed` to reach them.
2. **Homebrew builds from a plain source tarball with no generate step.** `Formula/uzi-cli.rb:43-46` runs `cd "api" do … go build … ./cmd/uzi`. There is no `task`/`go generate` invocation. So the embedded docs **must already be committed** (present in the tag's source tarball), exactly the model the already-committed embedded `SKILL.md` (`api/internal/uzicli/skill/SKILL.md`, `//go:embed skill/SKILL.md` at `skill.go:19`) and the goose migrations use. A gitignored build-time copy would break `brew install`.
3. **Onboarding is the offline / pre-connection moment.** A server-side `GET /api/docs` would tie retrieval to a reachable, authenticated instance — the opposite of what onboarding needs. Embed-in-binary works with no URL and no token, like `uzi version`. (A connected `uzi docs` that *also* shows the deployed instance's docs is possible later; see *Non-goals*.)

## Design Decisions

- **D1 — SoT is root `docs/`, unchanged.** Do not move the canonical docs into the api module. Web (`web/src/lib/docs.ts`), `web/scripts/check-docs.mjs`, and `REPO_BLOB_BASE` links all depend on the `docs/` ↔ `web/` sibling layout; moving it is a far larger change for no gain. The CLI gets a **mirror**, and a drift test keeps the mirror byte-identical to the source.

- **D2 — Embed a mirrored directory of raw `.md`, not a generated Go string map.** `task docs:sync` copies `docs/*.md` → `api/internal/uzidocs/embed/` (committed), and `uzidocs` embeds it with `//go:embed embed/*.md`. **Rationale, and it is the deciding factor:** the docs are full of backticks (code fences), which makes a generated Go source file fragile — a raw-string-literal (`` ` ``) generator breaks on the first fenced block, and `strconv.Quote` output is unreadable and enormous. `go:embed` reads raw bytes, so backticks are a non-issue. The cost is ~51 duplicated files in git (**all** `.md`, `README.md` included — see below); the drift test (D4) makes that duplication safe, and it mirrors the existing "source + committed generated copy" patterns (the tap formula, the installed skill). Embed **all** `.md` (not just `audience: user`) so operator/design/contributor docs are reachable when asked; the default *list* is still `user`-only (D5). **`README.md` is embedded** (so the byte-equality drift test covers the whole `docs/` set) **but never surfaced by `list`/`show`** — slug `README` is skipped in the loader, exactly as web does (`docs.ts:113`). Skip `docs/img/*` — CLI output is text; `uzi docs show` leaves markdown image syntax inline.

- **D3 — Runtime frontmatter parser is a Go port of `web/src/lib/docs.ts:parseFrontmatter`.** Same constrained rules: only a leading `---\n` fence at byte 0 is consumed; `title`/`order`/`audience` keys; unknown/missing/malformed frontmatter falls back to `audience: "design"` (repo-only) rather than erroring, so a pre-content or odd file never breaks the command. Audiences are the same closed set: `user | operator | design | contributor` (`docs.ts:28-29`). Slug = filename without `.md` (`docs.ts:106-108`), and `README` is skipped (`docs.ts:113`) — so `uzi docs show getting-started` maps to `getting-started.md`, matching the web route exactly.

- **D4 — Drift is caught by an existing gate, with NO new CI job. The Go test IS the drift mechanism; a `gate:repo` script is out of scope (recommended).** A Go test in `api/internal/uzidocs` reads the source docs across the module boundary (`../../../docs/*.md`, the precedented cross-module read pattern) and asserts byte-equality with the embedded mirror; it runs under the existing `task test:api` CI step (`.github/workflows/ci.yml:129`). Because it reads files the Go toolchain does not treat as source inputs, it is **cache-invisible** and must run under `-count=1` — which `test:api` already carries (see `.claude/rules/go.md`). That test alone satisfies Success Criterion 4, so **do not** also add a `check:docs-embed-sync` script unless there is a specific reason; if one is ever added under `scripts/`, it is itself swept by `gate:repo` (`lint:shell` via `git ls-files -- '*.sh'`, `lint:yaml`, `scan:secrets`) and must be shellcheck/yamllint-clean or it reddens the very gate it rides. Net: editing a `docs/*.md` without re-running `task docs:sync` reddens CI.

- **D5 — Verb surface, offline, exit-code conventions.** All three verbs work with no server and no token, print a human table by default and a stable document under `--json`. **`--json`, `--quiet`, `--no-color`, `--url`, `--context` are already persistent global flags (`root.go:203`); the verbs consume the existing `gf.json` — do NOT redeclare a local `--json`. `--audience` is the only new flag.**
  - `uzi docs list [--audience user|operator|design|contributor|all]` — default `user`; table `SLUG · TITLE · AUDIENCE · ORDER`, sorted by `order` (nulls last) then title; `--json` is a top-level array.
  - `uzi docs show <slug>` — prints the doc body (raw markdown) to stdout; `--json` returns `{slug, meta:{title,order,audience}, body}`. An unknown slug is **exit 4** (not found) with a "did you mean" suggestion computed from nearest slugs.
  - `uzi docs search <query> [--audience …]` — **whole-query, case-insensitive substring** match over title + body (deterministic; not tokenized AND/OR); ranks title hits above body hits; prints `SLUG · TITLE · snippet`; `--json` is an array with match context. This is the primary agent-retrieval verb.
  - A missing required arg is **exit 2** (usage), consistent with the rest of the CLI (`ExitUsage=2`, `ExitNotFound=4` in `api/internal/uzicli/output.go`).

- **D6 — Doc content is repo-controlled (trusted), so no `termsafe` bounding.** Unlike a hostile server's version string (the `cellText` vs `CellText` hazard in `.claude/rules/go.md`), embedded docs ship in the binary from our own repo. `uzi docs show` prints the body verbatim. `list`/`search` table cells derive from the same trusted content; use the bounded local `cellText` for table cells for tidy width, not for safety.

- **D7 — Documenting the verbs in SKILL.md is mandatory, not optional.** `TestSkillMatchesCommandTree` (`api/cmd/uzi/skill_drift_test.go`) asserts **both directions**: every runnable **leaf** command must be documented in `SKILL.md`, and every flag SKILL.md names must exist. The bare `uzi docs` parent is a non-runnable group (not asserted), but the three leaves (`list`/`show`/`search`) and any `--audience` the skill names MUST be documented, or the api build goes red — which happily coincides with the feature's own goal (route agents to `uzi docs`). So the verb reference lands in M2 (with the code), not deferred.

- **D8 — Exempt `docs` from the version-skew probe so it is truly offline on every invocation, not just when unconfigured.** The root `PersistentPreRun` (`root.go:184-189`) runs a version-skew check and a best-effort skill auto-upgrade before every command. `uzi version` is explicitly exempt from the skew probe (`exemptFromVersionCheck`, `versioncheck.go:83-102`) precisely because it "makes no network call of its own"; `uzi docs` is the same kind of command and must be added to that exemption, or a **connected** user (stamped binary + configured URL) would fire a `GET /api/version` (2s timeout) on every `uzi docs`. Note the auto-skill-upgrade `$HOME` write still runs for `docs` exactly as it does for `version` (it is tolerant of a read-only `$HOME`); that is existing behavior, not new to this PRD. With D8, `uzi docs` makes **no network call of its own** whether or not a server is configured. (The strict no-URL/no-token case is already offline regardless — `maybeWarnVersionSkew` early-returns with no store/URL — but D8 makes the connected case offline too, which is what "like `uzi version`" should mean.)

## Milestones

Each milestone is a vertical slice that lands **green on its own** (tests included, per this repo's "not done without tests" convention). Five milestones:

- [x] **M1 — Embedded docs corpus + loader (+ its tests + drift guard).** New `api/internal/uzidocs` package: `task docs:sync` mirrors `docs/*.md` → `embed/` (committed), `//go:embed embed/*.md`, a Go frontmatter parser mirroring `docs.ts` (D3), slug derivation matching web (`README` skipped), and a loader API (`List(audience)`, `Get(slug)`, `Search(query, audience)`). Corpus embeds ≈ 564 KB of markdown (measured: `cat docs/*.md | wc -c` = 564454). **Tests in this milestone:** loader units (frontmatter edge cases incl. the `design` fallback, audience filter, whole-query substring search + title-over-body ranking, unknown-slug suggestion), **and the drift guard** `TestEmbeddedDocsMatchSource` — byte-equality of `embed/` against `../../../docs/*.md` across the module boundary under `-count=1` (D4). Prove the guard reddens by a real mutation (edit a source doc without re-syncing), not by an empty diff.
- [x] **M2 — `uzi docs` command family (+ its tests + the SKILL.md verb reference).** `list` / `show` / `search` (D5) under a new `docs` cobra group, consuming the existing global `gf.json` (no local `--json`), `--audience` as the only new flag, exit-code conventions (2 usage, 4 unknown slug with suggestion). Add `docs` to `exemptFromVersionCheck` so it makes no network call of its own (D8). **Because the drift test `TestSkillMatchesCommandTree` gates the api build (D7), this milestone also adds the three verbs' reference lines to `api/internal/uzicli/skill/SKILL.md`** — otherwise M2 lands red. **Tests:** command-layer tests in the `commands_test.go` style (`--json` envelope shape per verb, exit codes 2/4, unknown-slug suggestion), live-DB-free, `-count=1`.
- [x] **M3 — Route agents to `uzi docs` in the skill.** Add an "Onboarding & concepts → `uzi docs`" routing section to `SKILL.md` (the *prose* that tells an agent to use `uzi docs search`/`show` for how-to/what-is questions; the verb *reference* already landed in M2). Pointer only — do NOT inline doc content (context-size discipline). Re-run `TestSkillMatchesCommandTree`.
- [x] **M4 — Docs + brew touch-up.** Add a `uzi docs` section to `docs/cli.md` (keep `check-docs.mjs` green — it is itself `audience: user`), noting the corpus is embedded and offline. Mention `uzi docs` in the formula `caveats` (`Formula/uzi-cli.rb:59`).
- [x] **M5 — End-to-end verification.** Confirm the offline chain: with no `UZI_URL`/`UZI_TOKEN` and no config, `uzi docs list|show|search` succeed with no network call; a parity spot-check that `uzi docs show <slug>` body equals the source `docs/<slug>.md` body (post-frontmatter) for several slugs; and `task gate:api`, `task gate:repo`, `task test:api`, `task gate:web` (docs unaffected but `docs/cli.md` changed) all green, with `git diff --name-only <base>..HEAD` showing zero `.github/workflows/**` entries.

## Success Criteria

1. `uzi docs search "connect a forge"` returns `getting-started` in the top results (it is a verbatim H2 in `docs/getting-started.md`) with **no server and no token set** (offline).
2. `uzi docs show getting-started` prints the same body the web app renders at `/docs/getting-started` (same source file, same slug, same frontmatter-stripping).
3. `uzi docs list` with no flag returns **exactly** the `audience: user` docs and nothing else (39 today, as of `4e82fd5d7` — asserted as a property, not a literal); `--audience all` returns every non-`README` doc; `README` is never listed.
4. Editing any `docs/*.md` without re-running `task docs:sync` **reddens CI** (drift guard), demonstrated by a real mutation (not an empty diff).
5. Adding the `uzi docs` verbs leaves `TestSkillMatchesCommandTree` green (verbs documented in SKILL.md source).
6. Offline behavior holds like `uzi version`: `uzi docs` exits 0 with no URL/token/`$HOME` credential, an unknown slug is exit 4 with a suggestion, and — per D8 — it makes **no network call of its own even when a server IS configured**.
7. `task gate:api`, `task gate:repo`, `task test:api`, and `task gate:web` are green; `git diff --name-only <base>..HEAD` shows zero `.github/workflows/**` entries.

## Risks & Mitigations

- **R1 — Cross-module `go:embed` (no `..`).** Mitigated by the committed mirror (D2). Resolved fact, not an open question.
- **R2 — Brew builds from a plain tarball with no generate step.** The mirror is committed, so it is in the tarball; `cd api && go build ./cmd/uzi` embeds it unchanged. Resolved fact.
- **R3 — Source and mirror drift.** The drift test (D4/M3) under `-count=1` catches it on an existing CI job; a `git diff` alone is **not** sufficient evidence the sync ran (empty diff also results from a no-op that never executed) — assert byte-equality against the read source, and validate the guard with a mutation.
- **R4 — Binary size grows ≈ 564 KB (uncompressed markdown).** Acceptable for a CLI that already embeds a 61 KB SKILL.md and the migration set; note it, do not compress (adds decode complexity for no real benefit).
- **R5 — Duplicated docs in git (the mirror).** Noise, not a correctness risk; the drift test keeps them identical. The single-generated-`.go` alternative was rejected (D2) because markdown backticks make it fragile.
- **R6 — Skill context bloat.** Avoided by design: the skill is a pointer to `uzi docs`, never an inlined corpus (D-Solution).
- **R7 — Free-text in docs treated as instructions.** Doc bodies are repo-controlled and trusted (D6); still, an agent rendering `uzi docs show` output to a user should present it as reference content, not execute it. No new untrusted-input surface is introduced (unlike a server DTO).
- **R8 — The root `PersistentPreRun` runs a version-skew probe + skill auto-upgrade on every command, so "offline like `uzi version`" is not automatic.** For a *connected* user, an un-exempted `uzi docs` would fire `GET /api/version` (2s) on every call. Mitigated by D8 (add `docs` to `exemptFromVersionCheck`, matching `uzi version`). The auto-skill-upgrade `$HOME` write is unchanged existing behavior for all commands and is tolerant of a read-only `$HOME`.
- **R9 — `--json` is already a persistent global flag.** Redeclaring it locally on the verbs would shadow the global and confuse parsing; consume `gf.json` (D5). `--audience` is the only genuinely new flag.

## Non-goals

- **Server-side `GET /api/docs`.** A connected `uzi docs` that also reports the *deployed instance's* docs (the way `uzi version` shows both CLI and server) is a natural phase 2, but not required for onboarding and explicitly out of scope here. No new API route in this PRD.
- **Moving the canonical docs** out of `docs/` (D1).
- **Rendering markdown** (ANSI formatting, images) in the terminal — `uzi docs show` prints raw markdown; prettifying is a later nicety.
- **Any `.github/workflows/**` change** — see below.

## Validation strategy

- Unit + command tests (M1/M2), run under the ordinary api gate with `UZI_TEST_DATABASE_URL` **unset** (these are not live-DB tests).
- Manual offline check: with no `UZI_URL`/`UZI_TOKEN` and no config file, `uzi docs list`, `uzi docs show getting-started`, `uzi docs search forge` all succeed.
- Parity check: `uzi docs show <slug>` body equals the source `docs/<slug>.md` body after frontmatter stripping, for a sample of slugs, matching what web renders.
- Drift guard proven by mutation (M3), not by an empty diff.

## Offline-worker notes (sweep handoff)

This PRD is queued for the nightly `Planned` sweep, whose worker runs with **restricted egress (no open internet)** and a bot PAT that **lacks `workflow` scope**. Both constraints are satisfied by design:

- **No open-web dependency.** Every fact this PRD relies on was resolved locally at authoring time from the codebase: the `go:embed` cross-module limitation, the brew plain-`go build` (`Formula/uzi-cli.rb:43-46`), the docs frontmatter/slug rules (`web/src/lib/docs.ts`), the drift-test cache rules (`.claude/rules/go.md`), and the CI wiring (`.github/workflows/ci.yml:129` runs `task test:api`; `:391` runs the composed `task gate:repo`). No milestone requires the worker to fetch anything from the internet.
- **No `.github/workflows/**` edit.** The drift guard rides existing CI steps (a Go test under `test:api`, and/or a `gate:repo` sub-check in `Taskfile.yml`, which CI invokes composed). The implementation and validation touch only `api/`, `Taskfile.yml`, `scripts/`, `docs/cli.md`, `Formula/uzi-cli.rb`, and `SKILL.md` — none under `.github/workflows/`. Before finalize, `git diff --name-only <base>..HEAD` must show zero entries there (per `.claude/rules/prds.md`).

## Decision Log

- **2026-08-22** — PRD created. Chosen mechanism: committed mirror + `go:embed` (D2) over loose brew files (path-discovery fragility across upgrades) and over server-fetch (breaks the offline onboarding moment). Skill is a pointer, corpus lives in the binary, retrieval on demand (context-size discipline). Drift guarded on existing CI jobs to keep the change workflow-free for the sweep worker.
- **2026-08-22** — Reviewed by two agents (scope/milestones; technical-facts/risks). All 8 load-bearing facts verified against `4e82fd5d7`. Applied: corrected the user-doc count (39, not 41; the "41" was a naive substring grep over-counting README prose and a body mention in `dev-conventions.md`); restructured to five vertical-slice milestones so each lands green (tests folded into M1/M2, the SKILL.md verb reference moved into M2 to avoid the M2-red/M4-green coupling the drift test creates); added **D8** (exempt `docs` from the version-skew probe — `uzi docs` is otherwise not offline-on-every-invocation for connected users) and **R8/R9**; pinned search semantics to whole-query substring; made SC3 property-based; stated the Go test is the sole drift mechanism (script out of scope) and that any such script is itself gate:repo-swept.
