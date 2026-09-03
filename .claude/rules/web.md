---
paths:
  - "web/**"
---

# web (Vite + React + TS)

Loaded when you touch `web/`. The repo-wide map is the root `CLAUDE.md`.

## Commands

```sh
task gate:web          # deps-check + lint + deadcode + check-docs + typecheck + test
task lint:web          # lint slot alone (oxlint; NOT ratcheted)
task deadcode:web      # dead-code slot alone (knip)
task test:web          # vitest run
task typecheck:web     # or check-docs:web, individually
cd web && npx vitest run src/pages/Foo.test.tsx   # single file — no Task target
cd web && npm run build                 # check-docs + tsc --noEmit + vite build
cd web && VITE_UZI_MOCK=1 npm run dev   # mock mode — no backend, no network at all
```

- knip gates every rule at `error`, including the unused-export/type family (#596): a new unused export reddens `task deadcode:web`. A missing knip loud-SKIPs (exit 0) locally, exits 2 in CI (`UZI_DEADCODE_WEB_REQUIRED=1`).
- `npm run build` is deliberately not in `task gate:web`; the delta is exactly `vite build` (CI's `validate:web` runs check-docs + typecheck directly). Run it by hand after touching anything the bundler resolves.
- No `build:web` Taskfile target exists; the CI job of that name is a kaniko image build.
- The suite-wide vitest `testTimeout` lives in `web/vite.config.ts`; read it there.

## DTO contract fixtures

- A DTO field change is a three-file edit: the Go struct, `fixtures/api-contract/<dto>.{zero,full}.json`, `web/src/lib/apiTypes.ts`. `apiContract.test.ts` checks `apiTypes.ts` against the recorded fixture, not against the Go struct, so a type-only edit leaves the fixture stale.
- Never hand-author a fixture: re-record it from the JSON the Go contract test prints on mismatch (`fixtures/api-contract/README.md`).

## Mock mode

- `VITE_UZI_MOCK=1` is read once at build time (`web/src/lib/api.ts`), swapping `src/mocks/mockApi/` + `MockRunSocket` for the real `api`/socket: a mock bundle has no code path to a live backend. No npm script for it; set the var on `dev` or `build`.
- Pick a demo scenario with `?mock=<name>` or the sticky `uzi_mock_scenario` localStorage key (`mockScenario()`); one string, so scenarios are mutually exclusive.
- `web/Dockerfile.mock` builds the backend-free static image (context = repo root); `web/nginx.mock.conf` 404s stray `/api/` calls as a tripwire.
- Scenario names, and the E2E bot env vars `UZI_E2E_BOT_PAT` / `UZI_E2E_BOT_USERNAME` / `UZI_E2E_PROJECT` (no test reads them yet), are in [docs/dev-conventions.md](../../docs/dev-conventions.md#the-mockdemo-build).

## What a mock browser pass proves

- Rendering, not population: layout, focus, contrast, a11y, copy and responsive findings stay fully valid.
- Any other data finding is about the fixture, not the product. One invariant is pinned, over five frames — `mockDoneMessages` (x2) and `mockFailedMessages` (`web/src/mocks/data/runHistories.ts`), `PLAN_RESULT_FRAME` and `RUN_RESULT_FRAME` (`web/src/mocks/engine.ts`) — each with at least 2 model keys and a `modelUsage` summing above the under-reading top-level `usage`, guarded by `web/src/mocks/data.realism.test.ts`. Issue #195 tracks that divergence class.
- Open fixture quirk: `engine.ts`'s `num_turns` goes 9 to 61 and reads cumulative, but real `num_turns` is per-invocation (live runs go 13 to 2). Do not file a `deriveRunUsage` bug from it.

## `vite dev` / `vite preview` talk to your live stack

- `web/vite.config.ts` proxies `/api` to `http://127.0.0.1:8080` via `server.proxy` and has no `preview` override, so `vite preview` inherits it (`resolvePreviewOptions` returns `proxy: preview?.proxy ?? server.proxy`, vite 6.4.3). `web/package.json` ships `"preview": "vite preview"`, so a page that POSTs on mount writes to real uzi. Same class as the bare-`docker compose up` hazard in `.claude/rules/stack.md`.
- Mitigate with `VITE_UZI_MOCK=1`: it replaces `api` wholesale, so the app makes no network calls at all.
- For the shipped `realApi` path, register every interception route before the first `open`. Precedence is not last-registered-wins: `unroute` a broad pattern rather than layering a narrow one over it.
- Stub `/api/repos`, `/api/forge/connections` and `/api/runs` besides the endpoint under test, or their 401s trip the global logout redirect to `/login` before the surface renders.

## Blind browser instruments

- A screenshot cannot show a native `title` tooltip: the platform widget layer is outside what `Page.captureScreenshot` composites, as are `<select>` popups, autofill dropdowns and print dialogs. Absence there carries no information.
- `agent-browser snapshot` prints the tooltip element's accessible NAME, not the describing element's DESCRIPTION, and the two disagree; use `Accessibility.getPartialAXTree` on the describing element.
- A blind instrument is exposed by a control (an empty, untitled row), never by a re-run.

## Selecting focus and status regions in a test

- Assert focus by IDENTITY, not text: `expect(document.activeElement?.textContent).toMatch(…)` is vacuous, since on `<body>` `textContent` is the whole page and matches anything. Use `toBe(el)` (the codebase convention); identity gives a false negative when a selector drifts and text a false positive, so their disagreement is the signal.
- `getByRole("status")` / `querySelector("[role=status]")` is ambiguous here: several regions carry `role="status"`, including the app-wide, always-present, usually-empty `RateLimitAnnouncer` (`RateLimitMeters.tsx`, rendered in `AppShell.tsx`), which a test that mounts the shell grabs first. Scope the query to the surface under test, or select by a more specific handle.

## Copy changes disarm negative assertions

- Retiring a string makes every negative assertion about it vacuous: `expect(queryByText(/old copy/)).toBeNull()` passes forever once nothing can render that string, while a positive assertion goes red. Only negatives rot, and review-by-reading misses them.
- On any copy change, grep the OLD string across the test tree and repoint each negative assertion at the current wording.
- Repoint only unpaired ones: a negative paired with a positive on the NEW string is a deliberate did-the-old-copy-come-back guard, and both halves stay. In `web/src/components/WorkerUpgradeBadge.test.tsx` that pair spans two adjacent tests, so a check confined to the negative's own block misreads it as unpaired.

## Untrusted values rendered into attributes

- `title=`, `aria-label=`, `alt=` and `placeholder=` are render sinks `container.textContent` cannot see. Assert on the attribute (`getByTitle`, `toHaveAttribute("title", …)`, `.title`), never on `textContent` alone.
- A field is exempt from control/`unicode.Cf` sanitizing at the write gate only if it renders exclusively to the principal who authored it; "owner-supplied" is not sufficient (`w.name` reaches an admin's cross-user fleet list; admin-authored builtin/global skill and template descriptions render in every other user's tooltip, `SkillAllocationPanel.tsx` / `AgentPicker.tsx` `title=`).
- The write-side gate is `termsafe.Validate` (`api/internal/termsafe/termsafe.go`): reject on `IsControl || unicode.In(r, Cf)`, never strip. `api/internal/handler/agent_templates.go` and `skills.go` route through it; the repo-agent description is `Cf`-gated by `hasUnsafeChar` (`api/internal/workersvc/agent_selection.go`).

## Picking an instrument

"Did anything but whitespace change?" is three questions:

| the question | the instrument |
|---|---|
| changed outside whitespace runs | `git diff -w --ignore-blank-lines` — primary line-structural |
| changed in non-whitespace bytes | a whitespace-strip hash — blind to a pure line split, under-reporting by the number of re-wraps |
| changed semantically | a `go/scanner` token stream, semicolon-normalised |

- Pick the one whose question is yours, and never settle a disagreement between two by trusting the cheaper method. The discriminator is a second method with a different blind spot.
- A blind instrument answers cleanly, repeatably and looks like the natural choice: a re-run is not a check, a control is.
- State a measurement's denominator when it differs between rows (16 changed files vs all 510 `.go` files in the module).
- A negative result needs a positive control: `sqlc generate` being a no-op was proven by `find internal/store -name '*.sql.go' -newer <marker>` returning 29 of 29, not by an empty `git diff`, which a run that never executed also produces.
- An identical-hash result is what an instrument that read nothing returns (`da39a3ee…` is the SHA-1 of empty input); add a byte-count control.
- A uniform result across every cell of a matrix is an instrument failure, not a measurement (`127` in all twelve cells: a command held in a variable put into command position).

## zsh

- `git show $sha:api/internal/foo.go` applies the `:a` modifier, because the path is a literal after the colon, and expands to a nonexistent path; write `git show $sha:$f` with the path in a second variable and nothing fires.
- zsh does not word-split unquoted variables: `files=$(git diff --name-only …)` then `cmd $files` passes every path as one argument. Use `files=("${(@f)$(…)}")` and `"${files[@]}"`.
