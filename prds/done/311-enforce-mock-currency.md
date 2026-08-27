# PRD #311: Enforce mock/demo currency

**Issue**: [#311](https://github.com/vtmocanu/uzi/-/issues/311)
**Priority**: Medium
**Status**: Complete (2026-08-16)
**Labels**: PRD, Night-Shift (nightly 02:00 sweep)

## Problem

The `VITE_UZI_MOCK=1` demo/dev build is a first-class surface: it is the backend-free
demo image (`web/Dockerfile.mock`), the offline dev loop, and the surface web-ux
validation runs against. Today it is kept current by **convention plus a per-endpoint
test suite**, not by enforcement, and that leaves three concrete gaps:

1. **The mock image is not built in CI.** `.gitlab-ci.yml` builds `web/Dockerfile`
   (job `build:web`) but nothing builds `web/Dockerfile.mock`. A broken fixture, a
   mock-only import error, or any failure of `VITE_UZI_MOCK=1 npm run build` that the
   unit tests do not bundle-check can land on `main` and the deployable demo silently
   stops building.
2. **No test asserts every route renders in mock mode.** The mock test suites
   (`web/src/mocks/mockApi.*.test.ts`, ~16 files) cover endpoints in isolation; none
   mounts the app under mock mode and walks the routes. A page that **throws** on the
   mock fixtures (dereferences a field a fixture does not supply) can ship and nothing
   fails. (Note: it cannot reach a truly *unmocked endpoint* — the type system already
   prevents that, see Background — so the gap is "renders without throwing", not
   "missing endpoint".)
3. **Data-realism staleness has no guard.** The mock fixtures are documented (in
   `.claude/rules/web.md`) as diverging from real shapes for whole classes of bug: the
   five result frames emit top-level `usage`/`modelUsage` with identical numbers and a
   single model key, where real frames diverge 2.5x-229x and drop model keys between
   frames. That divergence class (issue **#195**) is invisible to a mock browser pass,
   and nothing keeps a fixture from drifting further.

## Solution

Add mechanical enforcement where it is cleanly possible, and honestly document the
ceiling for the part that is not.

- **M1** builds the mock image in CI (closes gap 1) — the strongest, lowest-risk lever.
- **M2** adds a mock-mode route smoke test (closes gap 2 for the "renders without
  throwing" property).
- **M3** corrects the divergent result frames and adds targeted data-realism guards
  (closes gap 3 for the specific, documented staleness).
- **M4** writes the convention doc that states what is enforced, what is not, and the
  rule for the un-gateable part.

**What is NOT mechanically enforceable, and must stay convention (documented in M4):**
"every new user-facing feature has a demo scenario a human can reach" and "the mock
data is realistic" in general. There is no mechanical signal for "this is a new
feature." The type system already forces new *endpoints* to be mocked
(`api: typeof realApi`, see Background) but cannot force a new *scenario* or *realistic*
data. M2 (renders without throwing) and M3 (a specific realism invariant on named
fixtures) are the best available proxies; they do not replace the convention. M4 must
word the "enforced" list to match exactly what M2/M3 check, not broader.

## Background (baked facts — no external lookup needed)

Everything an implementer needs is in this repo. Coordinates drift; where a line number
is given, treat the **invariant** as authoritative and re-locate the line.

- **Mock wiring**: `web/src/lib/api.ts` — `MOCK_MODE = import.meta.env.VITE_UZI_MOCK === "1"`
  (line 13); the single client is `export const api: typeof realApi = MOCK_MODE ? mockApi : realApi`
  (line 2992). Because `mockApi` is typechecked against `typeof realApi`, a new or
  changed **endpoint** already forces a mock implementation or `tsc --noEmit` fails, and
  `typecheck:web` runs in `task gate:web`. This is the ONE thing already enforced; M1-M3
  add the rest. **Consequence for M2**: in mock mode every endpoint exists, so there is
  no "unmocked call" to catch — M2 catches a page that throws while rendering.
- **`MOCK_MODE` is evaluated once at module import from `import.meta.env`.** There is no
  "build" in a vitest run and nothing in the test tree sets `VITE_UZI_MOCK` (no
  `vi.stubEnv` usage today), so in a plain test `MOCK_MODE` is `false` and `api` is
  `realApi`. **M2 must force mock mode per-file** (see M2), because ~60 of the ~133 test
  files import from `../lib/api` and a global flip would move them onto `mockApi`.
- **The mock boots signed-in as admin**: `web/src/mocks/store.ts` (~lines 105-109,
  `session: { ...mockAdmin }`). So `ProtectedRoute`/`AdminRoute` pages are reachable by
  default, but `GuestRoute` pages (`/`, `/login`, `/register`) **redirect to `/dashboard`**
  (`web/src/components/RouteGuards.tsx`). M2 needs a signed-out context to actually
  render those.
- **Socket**: `web/src/mocks/socket.ts` (`MockRunSocket`), swapped in by `api.ts` when `MOCK_MODE`.
- **Mock impl + fixtures**: `web/src/mocks/mockApi.ts` (`export const mockApi` line 1279;
  the `truncated-backlog` scenario / `backlogMaxRows` at ~612-624 is the worked example of
  a demo toggle added so a state is human-reachable, not only test-covered). Fixtures live
  in `web/src/mocks/data.ts` **and** `web/src/mocks/engine.ts`. Scenario selection is
  `mockScenario()` reading `?mock=<name>` or the sticky `uzi_mock_scenario` localStorage key.
- **Mock image**: `web/Dockerfile.mock` — build context is the **repo root** (it COPYs
  both `web/` and `docs/`), build stage `node:24-alpine`, runs `VITE_UZI_MOCK=1 npm run build`,
  runtime `nginxinc/nginx-unprivileged` serving static `dist/`, with `web/nginx.mock.conf`
  404ing any stray `/api/` call.
- **CI build to mirror**: `.gitlab-ci.yml` job `build:web` (line 1493) `extends: .kaniko_build`
  (template line 1403; `.kaniko_rules_head` line 1393). It sets `UZI_BUILD_CONTEXT: $CI_PROJECT_DIR`,
  `UZI_DOCKERFILE: $CI_PROJECT_DIR/web/Dockerfile`, `needs: [validate:web, test:web]`, and
  rules that gate on `web/**/*` + `docs/**/*` changes on MRs. **`.kaniko_rules_head` makes
  protected refs (`main`, tags) ALWAYS build**, so a mirrored `build:web-mock` runs on every
  `main` push regardless of changed paths, and on web/docs MRs it is a cache-less
  (`KANIKO_AUTH:false`) full `npm ci` + `npm run build` — roughly a second web build, not a
  cheap delta. `build:web` also emits `uzi-web.tar` for `e2e:kind-smoke` (only when
  `UZI_TAR_DEST` is set); the mock build needs NO such artifact — leave `UZI_TAR_DEST` unset
  and add no `artifacts:` block.
- **Routes**: `web/src/App.tsx` — React Router `<Routes>` with ~32 `<Route path=...>` entries
  (public `/docs`, `/docs/:slug`, `/cli-auth`; authenticated shell routes wrapped in
  `ProtectedRoute`/`AdminRoute`; guest routes in `GuestRoute`; a `<Route path="*">` catch-all
  at line 275). Six routes take params: `/docs/:slug`, `/runs/:id`, `/repos/:id/board`,
  `/repos/:repoId/issues/:iid`, `/agents/:id`, `/chat/:id` — each needs a **fixture-valid**
  param from the mock store, or `mockApi` returns 404 and the page shows an error state that
  M2 must not misread as a crash.
- **The gate**: `task gate:web` runs deps-check, lint (oxlint), deadcode (knip), check-docs,
  typecheck, and `test:web` (vitest). New tests land in `test:web`; a new CI job lands in
  `.gitlab-ci.yml`. `npm run build` is deliberately NOT in `gate:web`; the mock bundle is
  validated by the M1 image build, mirroring how `build:web` validates the real bundle.
- **Data-realism weakness, precisely**: the divergence-prone result frames are **five, in
  two files** — `web/src/mocks/data.ts` (×3, around lines 3029-3143) and
  `web/src/mocks/engine.ts` (×2, around lines 142-145 and 241-250; the `engine.ts` 9→61
  cumulative-`num_turns` frame is the one that produced a false bug report per
  `.claude/rules/web.md`). All five currently carry a **single** model key (`claude-sonnet-5`)
  and top-level `usage` identical to `modelUsage`. The `.claude/rules/web.md` header cites
  stale `data.ts:2291-2292 / :2319-2320 / :2402-2403` line numbers (those lines now hold
  unrelated `health_reason`/`health_since` content); M4 corrects them.

## Milestones

### M1 — CI validation build of the mock image
- [x] Add a `build:web-mock` job to `.gitlab-ci.yml` that `extends: .kaniko_build`, sets
      `UZI_BUILD_CONTEXT: $CI_PROJECT_DIR` and `UZI_DOCKERFILE: $CI_PROJECT_DIR/web/Dockerfile.mock`,
      `needs: [validate:web, test:web]`, and mirrors `build:web`'s rules (gate on `web/**/*` +
      `docs/**/*` on MRs; always on protected refs). Validation only: `--no-push`, **no**
      `UZI_TAR_DEST` and **no** `uzi-web.tar` artifact.
- **Acceptance**: the job runs on an MR touching `web/**` and goes green on a healthy tree;
  and it is proven to go **red** when the mock bundle is broken (e.g. a temporary bad import
  in a mock-only file), then reverted. A green `build:web-mock` means `VITE_UZI_MOCK=1 npm run build`
  succeeds and the static image assembles. The RED proof requires a CI round-trip (push +
  watch the pipeline); a local `VITE_UZI_MOCK=1 npm run build` is the fast pre-check.
- **Landed (2026-08-16)**: `build:web-mock` added (`.gitlab-ci.yml`), a faithful mirror of
  `build:web` pointing at `web/Dockerfile.mock`; verified structurally (reviewer + auditor)
  and by a green local `VITE_UZI_MOCK=1 npm run build`. Also hardened `web/Dockerfile.mock`'s
  `npm ci` with `--ignore-scripts` to match `web/Dockerfile`, since this job is what first
  builds that Dockerfile in CI (including the protected-ref Harbor-cache path). The
  live-pipeline **red/green proof runs on this branch's MR pipeline** — it cannot be
  reproduced pre-merge (no push in-run).

### M2 — Mock-mode route smoke test
- [x] Add a vitest (jsdom) that forces mock mode **for this file only** — either
      `vi.mock("../lib/api")` returning the mock `api`/socket, or `vi.stubEnv("VITE_UZI_MOCK","1")`
      + `vi.resetModules()` + dynamic `import()` — so the rest of `test:web` stays on `realApi`.
      Mount each top-level route from `web/src/App.tsx` and assert it renders **without throwing**.
      Handle the two realities from Background: (a) drive a **signed-out** context for the
      `GuestRoute` pages (`/`, `/login`, `/register`) so they render instead of redirecting, and
      the default **signed-in-admin** context for Protected/Admin routes; (b) supply
      **fixture-valid params** for the six parameterized routes so a 404 error state is not
      misread as a crash. Enumerate routes from a single source (export the list from `App.tsx`,
      or assert a maintained list matches the router so a new route fails loudly).
- **Acceptance**: passes for every current route in both auth contexts; and proven **red** by a
  temporary probe route/component that dereferences a field the fixture does not supply (or calls
  `fetch` directly), then reverted. The negative must be non-vacuous per `.claude/rules/web.md`
  (assert on a real render signal / a thrown error, not the absence of a string that can never
  appear). Runs inside `test:web`.

### M3 — Correct the divergent frames + data-realism guard
- [x] **Own the fixture correction** (do not defer it — see Decision D5). Correct the five
      divergent result frames (`data.ts` ×3, `engine.ts` ×2) so a frame that a cost/usage
      surface consumes carries **more than one** model key and top-level `usage` that diverges
      from `modelUsage`, matching how real frames behave. Then add a guard test asserting that
      invariant **on the specific fixture(s) the cost/usage surfaces read** — name them (the
      run-view / usage-card frames), and do **not** assert "more than one model key" as a
      universal (real single-model runs are legitimate; the guard is about the fixture the
      divergence-dependent feature exercises).
- **Acceptance**: the guard passes on the corrected frames and is proven **red** on a frame
  reverted to single-key/identical-usage. Correcting the frames may redden existing numeric
  usage assertions (UsageCards / run-view) — update those in the same milestone. Runs in `test:web`.

### M4 — Convention doc + the honest ceiling
- [x] Document the enforcement in `docs/dev-conventions.md` (the existing "## The mock/demo
      build" section, ~line 551 — this is an *extend*, not a new page) and, where it is a working
      rule for contributors, in `.claude/rules/web.md`: what is now enforced (endpoint parity via
      types, the mock image build, the route smoke test = *routes mount without throwing in mock
      mode*, the realism guard = *the #195 divergence invariant on the fixtures it covers*), and
      the one thing that is NOT gate-able and stays convention — **"a new user-facing feature must
      add or extend a mock scenario so the state is reachable in the demo, not only covered by a
      unit test"** — referencing the `truncated-backlog` pattern as the worked example.
- [x] **Correct the stale fixture line-number citations in `.claude/rules/web.md`** (the
      `data.ts:2291…` refs) while the doc is open, and note the frames now span `data.ts` +
      `engine.ts` (fix-the-doc rule).
- **Acceptance**: `web/scripts/check-docs.mjs` passes (frontmatter, order, links); the doc states
  the enforced set and the convention precisely, with **no** claim that the un-gateable part is
  enforced and no wording that overstates M2/M3 beyond what they check.

## Phases (parallelization)

| Phase | Milestones | Depends on | Files touched | Parallel? |
|---|---|---|---|---|
| 1 | M1 | none | `.gitlab-ci.yml` | Independent of everything |
| 1 | M3 | none | `web/src/mocks/data.ts` + `engine.ts` (fixtures) + affected usage tests | Runs before/with M2's awareness |
| 2 | M2 | M3 (soft) | `web/src/**` route smoke test | Renders the fixtures M3 edits, so run after M3 or pin its fixtures — **not blindly parallel with M3** |
| 3 | M4 | M1, M2, M3 | `docs/dev-conventions.md`, `.claude/rules/web.md` | Sequential — documents what the rest built |

M1 is fully independent. M2 and M3 both touch `web/src` and M2 renders the fixtures M3
corrects, so they are coupled (the original "disjoint" framing was wrong): sequence M3
before M2, or have M2 assert against the corrected fixtures. M4 lands last. A single
nightly worker does them in order regardless.

## Risks

- **Route smoke test coupled to the router shape becomes brittle.** Mitigation: enumerate
  routes from one source and assert the list matches the router, so adding a route fails loudly.
- **Forcing mock mode leaks into other suites.** Mitigation: isolate to the one file
  (`vi.mock`/`stubEnv`+`resetModules`); never a global `VITE_UZI_MOCK`.
- **Over-asserting data realism** turns M3 into a test that fights every fixture edit.
  Mitigation: guard only the #195 divergence, scoped to the named fixtures the feature reads.
- **Correcting the frames reddens existing usage assertions.** Mitigation: M3 updates those
  tests in the same change (they are coupled by construction).
- **The mock image build adds real CI minutes** — a near-duplicate cache-less web build on
  every `web/**`/`docs/**` MR, and it runs on **every `main` push** (protected refs always
  build). Accepted; validation-only (`--no-push`), gated on web/docs paths on MRs.
- **check-docs breakage** if M4 touches a user-facing page with bad frontmatter. Mitigation:
  M4's acceptance runs `check-docs`.

## Decision log

- **D1 — Enforce the buildable image, not "every feature demoable".** The image build is the
  one clean, high-value gate; feature/scenario fidelity is a convention because no mechanical
  signal defines "a feature". Stated honestly in M4 rather than pretending M2/M3 cover it.
- **D2 — Validation-only mock image.** `--no-push` is hardcoded in `.kaniko_build`, so the
  image is never published and never consumed by e2e (no `uzi-web.tar`). On protected refs the
  job still authenticates to Harbor for *cache layers* (like `build:web`); it never pushes the image.
- **D3 — Guards must be proven RED before GREEN.** Every M1-M3 acceptance requires demonstrating
  the guard fails on a broken input, so it cannot be a vacuous check that passes forever.
- **D4 — Do not weaken the existing type enforcement.** `api: typeof realApi` already forces
  endpoint parity; nothing here replaces it, and M4 records it as the first line of enforcement.
- **D5 — M3 owns the fixture fix; it is NOT deferred.** An earlier framing pointed the fixture
  correction at "PRD #194 M3 as a prerequisite". PRD #194 is **concluded and archived to
  `prds/done/` with its milestones unchecked** — that work will not happen there. So #311 M3 is
  the real owner of correcting the divergent frames; issue **#195** is the divergence-class tracker.
  The "guard the already-correct fixtures" escape is removed because every frame is single-key
  today, which would make the guard vacuous.

## Validation strategy

- **M1**: run the pipeline on an MR; confirm `build:web-mock` runs and is green, and red on a
  deliberately broken mock bundle (CI round-trip). Local `VITE_UZI_MOCK=1 npm run build` is the
  fast pre-check.
- **M2, M3**: `task gate:web` locally (note the pre-existing Node-version `AuthContext`
  localStorage flake that passes on CI Node 24 is unrelated); each new test shown red on a
  broken input, then green.
- **M4**: `web/scripts/check-docs.mjs` (via `npm run build` or `check-docs:web`) green; a human
  read confirms the enforced/convention split is stated without overclaiming.
