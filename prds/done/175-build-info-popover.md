# PRD #175: Build info in the UI — the version badge stops being a string and becomes a coordinate set

**GitLab Issue**: [#175](https://github.com/vtmocanu/uzi/-/issues/175)
**Status**: **Complete 2026-07-28.** Created 2026-07-27. Merged as [!144](https://github.com/vtmocanu/uzi/-/merge_requests/144). All five milestones shipped, including M3, which was explicitly droppable and was not dropped.

> **Delivered beyond the milestone list**, on the owner's approval after review: the `useAppVersion` dead-arm fix (OQ-C/D's inherited-open item — a failed `/api/version` had shown "version pending" forever instead of a degraded message), and the correction to the fleet-panel copy that fix made visible for the first time. Both are recorded in `specs/ai.md` §453-§454.
>
> **Two constraints were ratified into `specs/human.md`** (Feature #175, `[user 2026-07-28]`): that `uptime_seconds` is published on an unauthenticated endpoint at accepted severity Low, and that `uzi version --json` gained a `server` key. Everything else this PRD decided was the team's and lives in `specs/ai.md` §448-§454.
>
> **Two residuals are open by design, not by omission.** The CI stamping path (`$CI_JOB_STARTED_AT` and `$UZI_COMMIT_COUNT` expanding inside a `variables:` value) is established from GitLab's implementation source at two layers but has never been observed on our runner; a permanent echo-only `before_script` on `publish:api` answers it at the first tag pipeline, and a silent non-expansion degrades to an omitted field that looks identical to a dev build. Instance-level CI variables were not enumerated (admin scope required), so a same-named variable shadowing `UZI_COMMIT_COUNT` is unchecked.
>
> **Two findings were split out rather than fixed here**: [#180](https://github.com/vtmocanu/uzi/-/issues/180) (the CLI renders server-supplied strings raw, ESC sequences included — house-wide, `uzi version` was only the command under review) and [#181](https://github.com/vtmocanu/uzi/-/issues/181) (`specs/ai.md` has §403-§416 buried mid-file, invisible to the tail-based sweep its own numbering rule prescribes).
**Priority**: Low — nothing is broken. `GET /api/version` works and the footer renders it. This buys instance-debuggability ("what exactly is deployed on dev-cluster right now") and a small amount of project identity.
**Mockup**: [`prds/mockups/175-build-info-popover-mock.html`](../mockups/175-build-info-popover-mock.html) — four surfaces mocked in uzi's own `ember`/`mission` chrome. Variant A (badge popover) is the one this PRD implements; the other three are kept in the file deliberately, as the record of what was rejected and why.

> Reviewed against `7bb07572` on 2026-07-27 by a fact-checker and an architect before first commit. Between them they refuted five factual claims and reshaped the milestone graph. Corrections are folded in; the ones that changed a *decision* rather than a sentence are marked **(revised)** in the Decision Log.

## Problem

`GET /api/version` returns exactly one field:

```go
// api/internal/handler/handler.go:276
httpx.JSON(w, http.StatusOK, map[string]string{"version": h.version})
```

so the SPA footer (`web/src/components/AppShell.tsx:506`) can render `v0.11.12` and nothing else. Three consequences:

1. **A deployed instance cannot say what it is.** The version is the Model-B coordinate (== image tag == chart `appVersion`, enforced by `publish:assert-version`, `.gitlab-ci.yml:601-608`), which is the right *release* identity but resolves to a range of commits. Debugging "is dev-cluster running the fix?" means correlating a tag against `git log` by hand, and a `dev` build — every local `docker compose` stack, since `docker-compose.yml:29` is a bare `build: ./api` with no `args:` — reports the literal string `dev`, which identifies nothing at all.
2. **The build has no timestamp and no commit.** `api/Dockerfile:20` stamps `main.version` and nothing else, so neither the image's source commit nor its build time survives into the running binary, despite both being free in the CI environment. **The repo already solves this for another image** — see M1.
3. **The project's own age is nowhere.** First commit is `366a282d`, 2026-07-03; the repo is 24 days and 2060 commits old at `7bb07572`. Not operational data, but the kind of thing a factory should be able to state about itself, and it costs nothing to carry.

The single-string shape was correct for what it was built for. This PRD widens it without disturbing that contract.

### Who actually reads this endpoint

Enumerated because the back-compat claim depends on it being complete, and because the first draft got it wrong in both directions:

| Consumer | Where |
|---|---|
| SPA API client | `web/src/lib/api.ts:1660` |
| `useAppVersion` (memoised at module scope) | `web/src/components/AppShell.tsx:61-79` |
| → footer badge | `AppShell.tsx:249`, rendered at `:506` |
| → **worker upgrade classification** (PRD #113) | `web/src/pages/WorkersSettings.tsx:41` → `WorkerUpgradeBadge.tsx:353` (`cpVersion`) |
| Handler test decoding the key directly | `api/internal/handler/version_test.go:23-29` |
| Route classification tests | `tls_listener_routes_test.go:75`, `route_limiter_mounts_test.go:198` |

**The `uzi` CLI is *not* a consumer** — `api/internal/uzicli/` contains no `/version` call at all; `uzi version` prints its own ldflags stamp. The two coordinates agree only because the same tag stamps both.

> **SUPERSEDED 2026-07-28 by M4, which is this PRD's own milestone. Every clause of the sentence above is now false.** `api/internal/uzicli/client.go` issues `c.get(ctx, "/api/version", &out)` inside `(*HTTPClient).BuildInfo`, reached from `api/cmd/uzi/version.go`. The CLI is a real consumer, and the two coordinates no longer agree merely because one tag stamps both: `uzi version` reports the server's coordinate alongside its own.
>
> **Kept rather than rewritten, because the sentence is the PRD's founding premise.** The problem statement is built on the CLI *not* reading this endpoint — that is what made `handler.go`'s "so the SPA footer and the uzi CLI report one coordinate" a false claim worth a milestone. Deleting it would erase the reason M4 exists.
>
> **This is the fourth site of one claim, and the first three were caught while this one was missed.** `.claude/agent-team-tasks/prd-175-brief.md`'s "Cross-milestone: one owner for the doc end state" reasoned that an M1 replacement worded *"the CLI is not a consumer"* would ship wrong in the same MR, and assigned the end state for `handler.go`, `main.go` and `ARCHITECTURE.md`. Nobody applied that reasoning to the PRD's own table. A planning doc is exactly the artifact with no gate on it — nothing compiles it, no test reads it, and it is what the next person consults first.
>
> The line-number citations in the table above are **not** affected: they were verified accurate against `7bb07572`, the SHA this PRD anchors itself to, and their drift since is ordinary. Only this sentence is a content claim rather than a coordinate, and only it went stale.

That matters more than a pedantic correction. `useAppVersion` feeds the **worker upgrade badge**, so this endpoint gates PRD #113's classification and is not merely cosmetic — see M2, where the tri-state that badge depends on is the single largest regression risk in this PRD.

**Doc rot to fix in the same MR** (CLAUDE.md's fix-the-doc rule): `api/internal/handler/handler.go:99-100` and `api/cmd/server/main.go:51-54` both describe the single-string contract, and the former states the endpoint exists *"so the SPA footer and the uzi CLI report one coordinate"* — false today, and the likely source of the first draft's error. `ARCHITECTURE.md:34` calls it an *"unauthenticated build-version string"*.

## Solution

Widen `GET /api/version` from `{"version": "..."}` to a build-info object, and render it as a **popover on the existing footer badge**: the `div` at `AppShell.tsx:506` becomes a button, and hover/focus/tap opens the full coordinate set above it.

The fields split by **what it costs to keep them true**:

| Field | Source | Cost |
|---|---|---|
| `version` | already stamped (`-X main.version`) | none — unchanged |
| `founded` | Go const, `2026-07-03` | none — it is never going to change |
| age | **computed by the consumer** from `founded` | none — and it stays correct between releases |
| `uptime_seconds` | `h.startedAt`, which `handler.go:208` already tracks | none — but see the zero-value hazard in M1 |
| `commit`, `built_at` | two more `-X` on the ldflags line that already exists | one CI line |
| `commits` | needs git history the publish jobs structurally do not have | **new CI plumbing** — see M3 |

Everything except `commits` is available with no new infrastructure, which is why M3 is explicitly droppable.

**Nothing about the endpoint's trust properties changes.** It stays unauthenticated (mounted under `r.Route("/api")` with only `Recoverer` + `RequestID` above it, `handler.go:286-296`) and unrate-limited, because everything it now carries is already public: the image tag is in the chart, the commit is in the repo. **No new field may be added to this response without checking it against that rule** — this is the one place a build-info endpoint conventionally leaks (hostnames, env, paths, dependency inventories), and this response must carry none of them.

## Milestones

- [ ] **M1 — Server build info, end to end.** The Go vars, the handler, the Dockerfile and the CI args ship together, because ldflags are inert without the vars and the vars are meaningless without the ldflags.
  - `api/cmd/server/main.go` (`var version = "dev"` at `:54`, `SetVersion` at `:520`) gains sibling `commit` / `builtAt` / `commits` vars and a `SetBuildInfo` seam. **`-X main.commit=…` targets *this* package, not `handler.go`** — the first draft's file map had this wrong.
  - Response: `version`, `founded`, `built_at`, `commit`, `commits`, `uptime_seconds`. **`version` keeps its exact current key and value.** `founded` is a const carrying `366a282d` in a comment as its evidence.
  - Unstamped fields are **omitted**, not faked. **Hazard, verified:** `handler.go:113-120` documents that many tests construct `Handler` as a struct literal rather than through `New`, leaving `startedAt` as the zero time — a naive `uptime_seconds` renders roughly two millennia there. Treat zero `startedAt` as unknown and omit, exactly as for an unstamped commit.
  - `api/Dockerfile:20` gains two `-X` flags; `publish:api`'s existing `UZI_BUILD_ARGS` (`.gitlab-ci.yml:674`) gains two `--build-arg`s. **`built_at` must be RFC3339 UTC (`date -u +%Y-%m-%dT%H:%M:%SZ`)**: `$UZI_BUILD_ARGS` is expanded **unquoted** at `.gitlab-ci.yml:661`, so `date`'s default output (`Mon Jul 27 21:12:38 UTC 2026`) would word-split and shred the kaniko command line.
  - Stamp the **full 40-char SHA** and truncate for display, so the stored value stays greppable and linkable.
  - Precedent to copy: `agent/templates/base/Dockerfile:294-299` already writes a `BUILD_INFO` from `ARG UZI_SRC_SHA` + `$(date -u …)`, fed by `--build-arg UZI_SRC_SHA=$CI_COMMIT_SHA` in both `publish:agent` (`:756`) and `build:agent` (`:541`). Note the trade it accepts and this endpoint must not: a `$(date)` inside a `RUN` needs no build arg but reports the **cached layer's** date when the layer is reused — for a field named `built_at`, that is a lie. Pass it as an arg.
  - Docs fixed here: `ARCHITECTURE.md:34`, `handler.go:99-100`, `main.go:51-54`. Tests ship in this milestone.
- [ ] **M2 — `useBuildInfo` seam + footer popover.**
  - **Add `useBuildInfo(): BuildInfo | null` backed by the *same* module-scope promise, and keep `useAppVersion()` as a `.version` projection over it.** One request, two shapes. This is not a style preference: `WorkerUpgradeBadge.tsx:353` carries a comment recording a *measured* bug where `null` (in flight) and `""` (resolved, unstamped) were conflated and the panel rendered a full bar under a heading saying classification was off — visible at T+270ms, flipping at T+670ms. Collapsing the hooks reopens it. **A coder must not "simplify" this into one hook.**
  - `AppShell.tsx:506` becomes a `button` with a popover on hover **and** focus **and** tap — hover is not a touch affordance. Drops the existing `title` (a native tooltip firing alongside a custom popover is a known annoyance). Two `SidebarContent` mounts exist simultaneously (desktop `aside` + mobile drawer), so any popover `id`/`aria-describedby` must be **instance-scoped**; state the anchoring direction and z-index against the mobile overlay (`AppShell.tsx:686`, `fixed … z-30`, no `overflow-hidden` above the footer, so no clipping).
  - **Two mock fixtures, fully-stamped and unstamped `dev`.** `api: typeof realApi` (`web/src/lib/api.ts:2115`) will **not** catch a thin mock: the moment `commit`/`built_at`/`commits` are optional — which the omit-never-zero rule requires — a mock returning only `{version}` typechecks fine. So the degraded shape (the common case on any laptop) has no type enforcement and needs an explicit fixture plus a vitest asserting the omitted-field render. Browser-verify via `VITE_UZI_MOCK=1` only (see the `vite preview` warning in CLAUDE.md).
- [ ] **M3 — Commit count.** The only field needing history the publish jobs do not have (OQ-A). Touches `.gitlab-ci.yml`, `api/Dockerfile`, `api/cmd/server/main.go`. **Independently droppable** — nothing else may depend on it, and the popover must render correctly with the field absent.
- [ ] **M4 — CLI parity.** `uzi version` is **already a custom command** (`api/cmd/uzi/version.go`, registered `root.go:146`) and **already supports `--json`** (`version.go:18-19` branches on `uzicli.FormatJSON`; `--json` is a root persistent flag at `root.go:126`). What it does not do is contact the server. M4 extends `newVersionCmd` to report both coordinates.
  - **Two hard constraints, both verified.** `Formula/uzi-cli.rb:56` runs `assert_match "v#{version}", shell_output("#{bin}/uzi version")` inside brew's sandboxed `test do` with no server reachable, and `scripts/brew-local-test.sh:69` runs it too — so the default output must still contain a literal `vX.Y.Z` **and must not block on a connect** (a 30s dial timeout in a brew test is a failed release). And `api/cmd/uzi/skill_drift_test.go`'s `TestSkillMatchesCommandTree` asserts **both directions** against `api/internal/uzicli/skill/SKILL.md`, so any new flag goes red until `SKILL.md:312-313` is updated. That is a gate, not a nicety.
  - Also update `docs/cli.md:19,89`.
- [ ] **M5 — Specs.** `specs/ai.md` records the endpoint contract, the compute-age-client-side decision, and the `useBuildInfo`/`useAppVersion` seam. Hand to spec-keeper.

**Tests ship inside each milestone, not in a terminal one.** A trailing test milestone would touch files owned by every other milestone simultaneously — the one shape guaranteed to collide with all of them — and would leave everything unverified until the end.

### Phases / parallelism

| Phase | Milestones | Depends on | Files touched | Parallel? |
|---|---|---|---|---|
| 1 | **M1** | — | `api/cmd/server/main.go`, `api/internal/handler/handler.go`, `api/Dockerfile`, `.gitlab-ci.yml`, `ARCHITECTURE.md` | no (foundation) |
| 2 | **M2**, **M3**, **M4** | M1 | M2: `web/src/components/AppShell.tsx`, `web/src/lib/api.ts`, `web/src/mocks/` · M3: `.gitlab-ci.yml`, `api/Dockerfile`, `api/cmd/server/main.go` · M4: `api/cmd/uzi/`, `SKILL.md`, `docs/cli.md` | **yes** — disjoint |
| 3 | **M5** | all | `specs/ai.md` | no |

**M1 alone is a complete, correct server change**, and M1+M2 is the shippable user-facing feature. Note the honest limit: without M1's SHA stamping, a version-and-age-only popover addresses *neither* Problem 1 nor Problem 2 — which is exactly why the ldflags work was folded into M1 rather than deferred as an enrichment.

## Decision Log

- **2026-07-27 — Variant A (badge popover), over three alternatives.** All four were mocked in uzi's real chrome before choosing (see Mockup). **Two-line footer** was rejected for permanently spending rail height on a number nobody reads twice, and for having no room for the SHA — the field with the actual debugging value. **`/about` page** was rejected as premature, not wrong: it is the only surface with room for links out to the changelog, and stays the natural follow-up if instance debugging outgrows a popover. **Dashboard stat tile** was rejected on information design: it would sit in a row of numbers you *act on* (active runs, workers awaiting approval) with a number you merely enjoy, and it is the only tile in that row with nowhere to link. The popover puts the data where people already look for the version and costs no layout anywhere.
- **2026-07-27 — Age is computed by the consumer, not sent by the server.** Sending both `founded` and `age_days` creates two sources of truth that disagree the moment a long-lived SPA session crosses midnight. Sending `founded` alone is declarative and means the age never needs a release to stay correct. The trade is a dependency on the client clock, acceptable at day granularity.
- **2026-07-27 — `founded` is served by the API, not hardcoded in the web bundle.** A web-side const would let M2 ship with no server change at all and would delete Phase 1 — a real simplification, deliberately declined: the CLI (M4) needs the same fact, and two hardcoded copies of a project's birth date is exactly the kind of duplication that survives long enough to disagree. Recorded because it decides the shape of the whole milestone graph.
- **2026-07-27 — The `version` key is not renamed, reshaped, or nested.** Widening is purely additive. The consumer table is why: the endpoint feeds PRD #113's worker upgrade classification, so a reshape is a coordinated change across the SPA, two test files, and a feature that gates fleet upgrades.
- **2026-07-27 — Unstamped fields are omitted, never zero-valued.** A `dev` build with `commit: ""` and `built_at: "0001-01-01T00:00:00Z"` renders as a build that claims to know things it does not. Omission keeps "we don't know" distinguishable from "the value is empty" — and the zero-`startedAt` hazard in M1 is the same rule applied to a field that *looks* always-available.
- **2026-07-27 — Rejected: dropping `-buildvcs=false` and reading `debug.ReadBuildInfo()`.** Go would hand over `vcs.revision` and `vcs.time` for free — no build args, no CI line, no ldflags work at all. It cannot work here and every implementer will ask on day one: `publish:api`'s kaniko context is `$CI_PROJECT_DIR/api` (`.gitlab-ci.yml:666`), so **there is no `.git` in the build context to stamp from** — the same fact that blocks the commit count in OQ-A. `api/Dockerfile:20`'s `-buildvcs=false` is additionally a deliberate reproducibility choice, documented in its own comment.
- **2026-07-27 (revised) — Build args stay tag-only, but not for the reason first written.** The first draft argued a per-commit arg would cost MR kaniko cache. **Wrong: MR builds have no cache to lose.** `.kaniko_build` gates auth *and* cache on `$KANIKO_AUTH`, which only the protected-ref rule sets (`.gitlab-ci.yml:436-442`); the job's own header says so at `:418-423`. The real cost would land on the protected default-branch `build:api`, the run that *warms* the shared cache — and the house pattern already tolerates that, since `build:agent` passes a per-commit `UZI_SRC_SHA` on every MR build. So the decision stands on a narrower argument: tag-only is where these fields are *meaningful* (a `dev` build's commit is not a release coordinate), not where they are affordable.
- **2026-07-27 (revised) — `useAppVersion` is kept, not replaced.** The obvious refactor — one hook returning the whole object — would collapse a tri-state (`null` in flight / `""` resolved-unstamped / value) that `WorkerUpgradeBadge` depends on, reopening a measured rendering bug. The new hook is layered over the same promise instead.
- **2026-07-27 — Uptime comes from `h.startedAt`, which already exists.** `handler.go:208` initialises it and `workerDTOFromWorker` already consumes it (`workers.go:476,496,649`, `runs.go:118`, `hosted_workers.go:157`). No new state — it was simply never surfaced here.
- **2026-07-27 — `commits` is separated because its cost is categorically different.** Every other field is a const, an existing struct field, or a CI variable. This one needs git history at image-build time, which the publish path structurally does not have. Bundling it into M1 would make a one-line change hostage to a CI decision.

## Open questions

- **OQ-A — How does the commit count reach the binary? (blocks M3, blocks nothing else.)** **Recommendation: option 2.** Two structural obstacles, both verified: kaniko's context is `$CI_PROJECT_DIR/api` (`:666`) so `.git` is not in it, and `.publish_image` sets no `GIT_DEPTH` (`:640-661`) so the job clones at the project default — the only `GIT_DEPTH: 0` in the file is `publish:assert-changelog` (`:628`), whose comment reads *"git describe needs tags and real history; the default shallow clone has neither."*
  1. **`GIT_DEPTH: 0` on the publish job(s).** Settable per-job — `publish:api` has its own `variables:` block (`:665`) — so the first draft's "pays a clone on every publish job" was overstated. **But it is probably infeasible for a different reason: the count would have to be computed in `publish:api`'s `script:`, whose image is `gcr.io/kaniko-project/executor:v1.24.0-debug` (`:642`) — busybox + kaniko, no `git`, and no `apk` to add one.** Verify that before considering this option; if it holds, option 1 is dead.
  2. **Compute it in `publish:assert-changelog` and pass a dotenv artifact.** That job already runs on tags, already sets `GIT_DEPTH: 0`, already uses the golang image (which carries git), and is **already in the `.publish_needs` anchor (`:96`) that `publish:api` consumes at `:645`** — so the `needs:` edge and the full-history clone are both already paid for. A `artifacts: reports: dotenv` line is the whole change. One real cost to name: `needs: *publish_needs` is a YAML **alias, not a merge**, so any job-specific edge means writing the list out or extending the shared anchor.
  3. **Drop the field.** Age and commit SHA carry most of the value; the count is the one number here that is pure flavour.
  **If none is attractive, M3 is dropped and the PRD still delivers.**
- **OQ-B — `uzi version`'s output shape is a contract change and needs confirmation before M4 starts.** Larger than the first draft framed it. Today the command is offline and instantaneous (`version.go:16-23`). Three affected audiences: agents parsing `--json`, which `docs/cli.md:13` sells as a contract; the Homebrew formula asserting on stdout; and humans. **Proposed shape: keep top-level `version` as the CLI's own coordinate and nest the server's under a `server` key**, so existing parsers are untouched. Confirm before implementing, not after.
- **OQ-C + OQ-D — the signed-out shell and `uptime_seconds`, resolved together.** They are coupled and the first draft treated them as independent. `SidebarContent` is reachable only past `if (!user) return <PublicShell>` (`AppShell.tsx:670`), so today `uptime_seconds` would have exactly one consumer and it is authenticated. Answering "show the popover signed-out too" is precisely what makes "is uptime public?" a real question. **Recommendation: no signed-out popover** — it keeps the blast radius at zero and makes the uptime question moot. If it is yes, then either accept uptime as public and say so, or move it to an authenticated route and keep `/api/version` to build facts only.

  > **RESOLVED 2026-07-28, and the sentence above is WRONG about why. Read the resolution, not the recommendation.**
  >
  > **Decision: no signed-out popover, and `uptime_seconds` is kept and declared public.** The *decision* is what this bullet recommended. The *reason* it gives does not hold, and an auditor and a reviewer refuted it independently, from different starting points, before any code was written.
  >
  > "It keeps the blast radius at zero and makes the uptime question moot" is a claim about the **UI**. The property is enforced at the **endpoint**, and the endpoint is unauthenticated. Re-derived at `0c56bcee`: `r.Get("/version", h.Version)` mounts directly under `r.Route("/api")` with only `Recoverer` + `RequestID` above it; `route_limiter_mounts_test.go:203` pins it `noLimiter`; `web` same-origin-proxies `/api/*`; and `deploy/chart/templates/web-ingress.yaml` publishes it at `path: /` with no auth annotation. So uptime in that body is world-readable, credential-free and unrate-limited **whether or not a signed-out popover exists**. Blast radius is "everyone who can reach the ingress", never zero. The signed-out popover changes **discoverability**, not exposure — so it was never the thing that made uptime public, and declining it does not make the question moot.
  >
  > **The real reason the field is kept: uptime is acceptable as public.** Severity Low. It discloses restart and patch cadence, and paired with `version` gives a freshness oracle ("this instance has been running build X unpatched for N days") plus an unauthenticated liveness probe. That is a judgement, and it is recorded here as one.
  >
  > **Every other field passes the already-public test on its own**, which is what isolates uptime as the single case needing a judgement: `version` == the chart's image tag; `commit` is already pushed as the Harbor tag `:$CI_COMMIT_SHORT_SHA` (`.gitlab-ci.yml:687`); `built_at` is implied by the release tag; `founded` and `commits` are consts and counts. **`uptime_seconds` is the only RUNTIME fact in a response otherwise made entirely of BUILD facts** — which is exactly the line this PRD's own rule at :63 draws, and the reason it needed answering rather than assuming.
  >
  > **Where the statement lives, and why not here.** In `api/internal/handler/handler.go`'s `Version` doc comment (the enforcement point), in `BuildInfoDTO`'s type doc, in `ARCHITECTURE.md`'s trust-boundaries section, and in `TestVersionEndpointCarriesNothingPrivate`, whose public-list comment splits "already public" from "considered disclosure" with uptime as the sole member of the second class. **A rule recorded only in a PRD has no gate on it** — and this PRD's own named follow-ups (an `/about` page, a signed-out footer) would silently republish the field with nothing in the code noticing.
  >
  > **The cost that is real, and is not about this decision:** `uptime_seconds` establishes a *category*. Once one runtime fact lives on this endpoint, "which replica", "how many replicas", "what region", "what pod" all become the same kind of request rather than obviously out of scope — and every one of those is identity- or topology-bearing. The counterweight is the closed-key-set gate, which is why that gate being genuinely closed matters more after this decision than before it.
