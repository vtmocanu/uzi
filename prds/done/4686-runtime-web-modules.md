# PRD #4686: Runtime web modules

**Issue:** #4686.
**Status:** Implementation complete (2026-11-25); Kubernetes owner acceptance pending.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** web SPA, web nginx configs and images, Helm chart, compose, docs, specs and tests. No api, store, worker or CLI behaviour change (the generated `api/internal/uzidocs/embed` changes from `task docs:sync` are expected). No changes under `.github/workflows/**`, including validation.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`. Feasibility evidence: a local throwaway spike (Module Federation, built mode, the exact nginx CSP enforced) summarized in §8.

## Progress

- [x] M1 host runtime
- [x] M2 delivery (nginx, chart, compose)

## 1. Problem

An operator who runs uzi next to separately built web features has no way to show them inside uzi. Routes are a static array (`APP_ROUTES`, `web/src/App.tsx`), navigation is a static list (`NAV_SECTIONS`, `web/src/components/navItems.tsx`), and the CSP is `script-src 'self'` in all three nginx configs. The only options today are a fork of the web image, or an iframe that does not share uzi's shell, session, router or theme.

## 2. Outcome

An operator lists trusted web modules in deployment configuration. Each module is built and deployed independently, served same-origin under `/modules/<id>/`, and loaded by the browser at runtime as native React pages with uzi's shell, router, theme and session. The supported failure classes in §4 (load failure, render error, refused contract or shared version) are contained to the module's page. With no modules configured, uzi looks and behaves as today.

Acceptance examples:

1. An operator adds the example module to deployment configuration. Without rebuilding the web image, a signed-in user sees its nav entry and command-menu entry, opens `/m/example`, and the page renders with uzi's theme; its link navigates uzi's router.
2. The example module's server is stopped, or accepts connections but never answers. Every uzi page works; `/m/example` shows a "module unavailable" card with Retry within the load timeout; after the server returns, Retry renders the page without a document reload. If the Retry fails again (the module is still failing, or the browser keeps a failed module file for the page), the card also offers "Reload page", which recovers once the module is back (Retry, then Reload page, recovers).
3. A module declares a different host contract major, or was built requiring an incompatible React major. The host refuses to render it and never renders a second React tree. An admin sees the refusal reason (contract or shared-version mismatch, module id and version); any other user sees a generic "this module is not compatible with this version of uzi" message. Neither gets Retry.
4. No module is configured. No module code or nav entry appears, and the only added request is one small no-cache `/modules.json` fetch.
5. A module is redeployed from v1 to v2 while a tab holds v1. When the tab next needs a v1 chunk that is no longer served, the page shows the error card with a "Reload page" action; after the rollout completes, that reload renders v2.

## 3. Out of scope

- Module backends or any new api capability. Modules call the existing API with the signed-in user's session through the host SDK, exactly as the host does.
- Installing, enabling or upgrading modules from the UI or the api. Configuration is deployment-only.
- Sandboxing. Same-origin modules run with full page authority (§4 Security).
- Module content inside existing host pages (slots); modifying existing screens.
- Live refresh of `/modules.json` in an open tab: a list change takes effect on the next document load.
- A published npm SDK package. The contract types ship as a source file in the repo (§4 Host SDK).
- Browser-side integrity (SRI) for module code (§7).
- Module data in mock (demo) builds: `sdk.api` rejects there (§4 Host SDK).

## 4. Modules and seams

### Module list (deployment configuration)

The web tier serves the list at `GET /modules.json` (no auth, `Cache-Control: no-cache`). It is a JSON array; empty when nothing is configured. Each entry:

| Field | Rule |
|---|---|
| `id` | Required. `^[a-z][a-z0-9-]{0,31}$`, unique. Names the route `/m/<id>`, the asset prefix `/modules/<id>/` and the federation remote name |
| `version` | Required. Shown to admins in error details and diagnostics only; it is not part of any URL and does no cache busting (§ Same-origin delivery) |
| `page` | Required expose name rendered at `/m/<id>/*` |
| `label`, `icon`, `navGroup` | Required. `label` 1 to 40 characters; `icon` from a fixed host icon-name list; `navGroup` is `work`, `factory` or `modules` (mapping to the existing `"Work"` and `"Factory"` `NavGroupName`s, or a new `"Modules"` group rendered only when non-empty; as built it sits after Factory and before the unlabelled Settings/Admin/Docs cluster, see the Decision Log) |
| `adminOnly` | Optional boolean, default false. Nav and command-menu entries hidden from non-admins, route behind `AdminRoute`. Presentation only: the API still authorizes every call |

The upstream address of a module is nginx configuration only and never appears in `/modules.json`. Ignore unknown fields, so a later contract can add one additively.

**Visibility.** `/modules.json` and every proxied module file are unauthenticated and discoverable, including the id, label, version and `adminOnly` of admin-only modules. The docs state this: keep personal or confidential material out of both.

**Validation.** The chart (`fail`) and the compose generator (§ Delivery) reject: a malformed or duplicate id, a missing `version`, `page`, `label`, `icon` or `navGroup`, an unknown `navGroup` or icon, and an upstream that is not a valid `host:port` or `http://host:port` address (the only accepted forms; validated before it is interpolated into nginx config). A bad config never deploys.

The SPA re-validates on load, because the file can be hand-edited. It skips an entry with an invalid id or a missing required field; keeps the first entry of a duplicated id; maps an unknown `navGroup` to `modules`. Each skip or fallback logs one console error naming the entry. A malformed file means "no modules".

### Same-origin delivery (nginx)

For each module the web nginx gets a `location /modules/<id>/` proxying to that module's upstream, keeping the path prefix (a module is built with `base: "/modules/<id>/"`).

**Resolution.** Module upstreams always resolve at request time (`resolver` plus a variable `proxy_pass`), so nginx starts and serves uzi even when a module Service or container does not exist yet; such requests get 502 and the host shows the error card. Prerequisites: on the chart, configuring any module requires `web.nginx.resolver` set (the chart `fail`s otherwise; it is the same value `web.nginx.dynamicResolution: true` uses for the api upstream, whose own setting this does not change, and module resolution must work with `dynamicResolution` false); on compose, the generated include uses Docker's embedded DNS (`127.0.0.11`). Both modes are tested (§5).

**Request hygiene.** Module locations accept `GET` and `HEAD` only, and strip `Cookie` and `Authorization` from the proxied request: serving static module files never forwards the host session to another server.

**Response headers.** `proxy_hide_header` drops the upstream's `Cache-Control`, `Expires`, `Set-Cookie`, `Content-Security-Policy`, `X-Frame-Options`, `X-Content-Type-Options`, `Referrer-Policy` and every `X-Accel-*` header. `proxy_ignore_headers` makes nginx itself ignore the upstream's `X-Accel-Redirect`, `X-Accel-Expires`, `X-Accel-Limit-Rate`, `X-Accel-Buffering`, `X-Accel-Charset`, `Expires`, `Cache-Control` and `Set-Cookie`, so a module can never trigger an nginx internal redirect (hiding a header only changes what the client sees). Upstream redirects are passed through only as ordinary 3xx responses with `no-store`; `proxy_redirect` stays off so nginx does not rewrite them. Each module location then sets the full host security header set and the unchanged CSP, because nginx drops inherited `add_header`s when a location declares its own (`scripts/assert-web-nginx.sh` header). No duplicate security headers reach the browser.

**Caching.** Vite emits a module's hashed files under `assets/`. Every module response carries exactly one `Cache-Control`, chosen by path and status (for example through a `map` on `$status`; plain `add_header` without `always` applies to every 2xx/3xx status nginx lists, so it alone does not enforce this):

- `^/modules/<id>/assets/`, `200`, `206` and `304`: `public, max-age=31536000, immutable`.
- Every other module path (`remoteEntry.js`, `mf-manifest.json`, unhashed files), `200`, `206` and `304`: `no-cache`.
- Every other status on any module path (other 2xx, every 3xx redirect, 4xx, 5xx): `no-store`, so a redirect, 404 or 502 is never cached.
- `gzip` matches the existing static locations.

**Updates during a rollout.** A request for an old hashed chunk can reach a new module pod and 404. Retention is not guaranteed; recovery is a document reload. A browser keeps a successfully imported `remoteEntry.js` in its module map for the life of the document, so re-registering the remote cannot swap v1 for v2 in place. When a module that already loaded in this document later fails a chunk load, the error card offers "Reload page". While a rollout is still mixed, a reload may still reach either version; once it completes, a reload reaches v2. Module authors may additionally keep the previous build's `assets/` in their image; the docs recommend it but the host does not rely on it.

**Wiring.**

- All three configs (`web/nginx.conf`, `web/nginx.mock.conf`, the chart's `web-configmap.yaml`) gain an `include` of a modules directory inside the server block and a `location = /modules.json`.
- Both images (`web/Dockerfile`, `web/Dockerfile.mock`) ship an empty `[]` and an empty include directory, so with no modules the only behaviour change is that file.
- The chart renders the include and the file from `web.modules[]` (the fields above plus `upstream`) into the web ConfigMap. `web-deployment.yaml` today mounts only `default.conf` from that ConfigMap, so add explicit read-only mounts for the include and `/modules.json`, keeping the existing config-checksum rollout so a list change rolls the web pods. As built these are two `subPath` file mounts, not directory mounts (see the Decision Log).
- Compose takes the same list from a modules override file through a small generator script (`scripts/`), which validates as the chart does and writes the include and `modules.json` that an override compose file mounts.

### Loader

- Add `@module-federation/vite` and `@module-federation/runtime` (declared directly) to `web`. Host federation config: no build-time remotes, `shareStrategy: "loaded-first"`, shared singletons `react`, `react-dom`, `react-router-dom` with `requiredVersion` from `web/package.json` and `strictVersion: true`.
- On app start the SPA fetches `/modules.json` once (failure means "no modules", logged). It registers each valid entry with `registerRemotes(..., { force: true })`, `type: "module"`, entry `/modules/<id>/remoteEntry.js`. No remote entry is fetched until a module route renders.
- Loading uses the runtime's `loadRemote`, never `React.lazy`: a rejected lazy promise is cached, which defeats Retry. Retry is for transient failures before a module ever loaded in this document (upstream down, timeout): it re-registers the remote with `force` and loads again; after a Retry that fails again, the card also offers Reload page. The spike showed this recovering in Chromium; M1 proves it in the browser test. Retry is not an update mechanism (§ Updates during a rollout).
- Each load is bounded by a 15 s timeout (one constant, a test hook overrides it). On expiry the page shows the error card; a late arrival is discarded.
- Measure and report the host bundle and first-load delta the federation runtime adds with zero modules.

### Routes and navigation

- One generic route `/m/:id/*` inside the authenticated shell (`ProtectedRoute`, or `AdminRoute` for `adminOnly`; `AdminRoute` already implies authentication). A module renders its own nested `<Routes>` relative to `/m/<id>`.
- An id that is not in the loaded list renders a new in-shell "No module named `<id>` is configured" view. (Today's catch-all, `App.tsx` `path="*"`, redirects to `/`; that stays unchanged for non-module paths.)
- `NAV_SECTIONS` stays static. One merge function appends module entries to their groups and is the single source for both the sidebar (`AppShell`, which reads `NAV_SECTIONS` directly today) and `visibleNavItems()` (the command menu, built in a memoized item builder), so the two never disagree. The list arrives asynchronously: both consumers re-render when it resolves, and a test opens the command menu after resolution. Module nav keys are prefixed `module:` so they never collide with host keys or the motion-icon map.
- The nav entry stays visible when a module fails to load; the route shows the error card. Hiding it would require fetching every remote eagerly.

### Host SDK (contract v1)

A module's exposed page is a React component receiving one prop, `sdk: HostSdkV1`. Types live in `web/src/modules/sdk.ts` as the single source; a module repo vendors that file at the uzi tag it targets.

- `contract: 1`
- `user`: id, display name, admin flag.
- `api.request<T>(method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE", path: string, body?: unknown): Promise<T>`. `path` is relative to `/api` and must start with `/`; the host prepends `/api` as its own `request` does, and rejects any path whose normalized form leaves `/api/` (`..` segments, encoded separators, absolute URLs). It is a new exported wrapper in `web/src/lib/api.ts` over the private `request()`, so it keeps that function's behaviour unchanged: session cookie, CSRF header, passive handling, the 401 logout path, vault-lock handling, and rejection with the host's `ApiError` (HTTP status and server message). It is chosen by `MOCK_MODE` exactly like the exported `api` object: in a mock build it never touches the network and rejects with `ApiError` 503 "not available in demo mode". `createHostSdk({ transport })` lets tests inject a transport.
- `ui`: `Card`, `CardHeader`, `Button`, `Select`, `Badge`, `StatusPill`, `EmptyState`, `Spinner`, `Skeleton` (existing exports of `web/src/components/ui.tsx`; others are added only when a module needs them).
- `theme`: current mode and a subscribe function. Colours come from the existing CSS variables (`rgb(var(--brand))` etc.), which the spike showed follow a live theme switch.
- `navigate`: a router helper for links outside the module's own subtree.

**Contract check.** Each exposed component module also exports `hostContract: 1`. The host supports exactly one contract major and refuses to render a module whose major differs, with the reason in the error card. The check runs after the expose is imported, so it prevents rendering, not execution of the module's top-level code. Within a major, changes are additive only. A breaking change bumps the major in a minor uzi release whose changelog names it one release ahead; operators then upgrade their modules together with uzi.

### Failure isolation

Contained, per page:

- Load failures: network error, HTTP error, timeout, missing expose.
- Errors thrown while rendering or in lifecycle methods of the module's React tree (an error boundary per page).
- Refusals: contract major mismatch, shared-version refusal.

Each renders an error card in the module's page. Admins see the reason, the module id and its `version`; other users see a generic message. The action depends on the class:

| Class | Card action |
|---|---|
| Transient failure before the module ever loaded in this document (network or HTTP error, timeout) | Retry; after a Retry that fails again, also Reload page |
| A chunk load fails after the module already loaded in this document (stale chunk during a rollout) | Reload page |
| Contract major or shared-version refusal, missing expose | None; an incompatibility message (Retry cannot fix it) |
| Render or lifecycle error | Retry: resets the page's error boundary and remounts the already-loaded component; no re-registration or `loadRemote` |

Re-registration and `loadRemote` are reserved for the transient initial-load row. Tests pin both Retry paths.

Not contained, and stated in the operator docs: errors in a module's own async code outside React rendering (they surface as console errors and unhandled rejections), global CSS a module injects, a module blocking the main thread, and deliberate misuse of page authority. A trusted module is expected to scope its CSS (the spike's convention: a module-prefixed root class).

### Security

- Modules execute with the host page's full authority: they can call any API the user can and read the DOM. Module Federation is not a sandbox. Deploy only modules you trust, pinned by image digest.
- `script-src 'self'` and the rest of the CSP stay unchanged. The spike found no inline script in the host's built `index.html` and no `eval` or `new Function` in the module's build; M1 checks both builds for both (§5).
- Module proxies never forward the host session (§ Request hygiene).
- No new api endpoint, privilege or credential. `adminOnly` is presentation; the API remains the authority.

### Example module and docs

- `web/examples/ui-module/`: a minimal synthetic module (one page with a nested route and a lazily imported sub-view, uses `sdk.api` and `sdk.ui.Card`, a module-prefixed CSS root class), built with `base: "/modules/example/"` by its own Vite config that resolves dependencies from `web/node_modules`. It adds no `package.json`, so CI's existing `npm ci` in `web/` installs everything it needs and no gate installs from the network. It is not shipped in any image.
- Docs: an operator page (enabling modules in chart and compose, the resolver prerequisite, the visibility note, trust model, what is and is not contained, troubleshooting) and a developer page (build config, the contract, versioning, caching and the rollout recommendation), with frontmatter, `task docs:sync` and `task check-docs:web`. A terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`; never edit frozen `specs/ai.md`.
- No CLI change: modules are web-only and add no API.

## 5. Testing decisions

- **Unit (Vitest):** `/modules.json` validation and every SPA skip/fallback rule above; nav merge feeding both the sidebar and `visibleNavItems()`, including after asynchronous resolution; route guard selection; the "no module named" view; contract check; `sdk.api.request` path normalization and rejection, error, 401 and mock-build behaviour through an injected transport; Retry re-registration; load timeout via the test hook.
- **Built-mode browser test:** a new task target that `task test:web-browser` runs, so CI picks it up without a workflow edit. It builds the host and `web/examples/ui-module`, serves both on ephemeral ports behind a proxy carrying the exact CSP, waits for readiness by polling the served URLs with a bounded deadline (no fixed sleeps), reuses the Chromium launch configuration `vitest.browser.config.ts` selects (including its `UZI_WEB_BROWSER_CHROMIUM` override and launch flags), and kills its own process group on exit. It covers examples 1 (list served by the harness), 2, 3, 4 and 5, and asserts zero CSP violations, single origin, shared React identity (compare functions, not namespace objects) and no `eval`/`new Function`/inline script in either build.
- **Incompatibility from the remote side:** a fixture build of the example module declaring an incompatible React `requiredVersion` (and one declaring `hostContract: 2`) must be refused, proving the policy on the consumer side rather than only the host's `strictVersion` setting. The browser test checks the admin and non-admin card for each refusal, and that neither offers Retry.
- **Two remotes:** the browser test also serves a second, compatible fixture remote under its own id and upstream, and proves both load in one document with one shared React and independent failure (stopping one leaves the other working).
- **nginx:** extend `assert-web-nginx.sh` for all three configs on real nginx. Its chart path extracts only `default.conf` today: extend the fixture to mount the new include and `/modules.json` payload. Assert: module location security headers present once and unchanged CSP; upstream `Set-Cookie`, `Expires` and security headers stripped; `Cookie`/`Authorization` not forwarded (the stub upstream echoes received headers); only `GET`/`HEAD`; exactly one `Cache-Control` per response with the path/status split, including `206` and `304` asset responses, an upstream 302 carrying `no-store`, and a 404 under `assets/` carrying `no-store` and not `immutable`; an upstream `X-Accel-Redirect` neither followed by nginx nor visible to the client; `/modules.json` no-cache; nginx starting and serving uzi with an absent module upstream, under both the chart's resolver mode (with api `dynamicResolution` disabled) and the compose embedded-DNS mode.
- **Chart:** a fixture-render driver renders zero, one and two modules (checking the new mounts and the checksum annotation), and expects `helm template` to fail for each invalid case (malformed id, duplicate id, missing required field, unknown group or icon, invalid upstream, modules without `web.nginx.resolver`). The existing `assert-chart-render.sh` keeps its current single-render shape checks. The driver runs as an added command of `render:openshift-check`, which CI's helm-chart job already invokes, so no workflow edit is needed.
- **Compose generator:** the same valid and invalid fixtures, through a hermetic shell test like `assert-web-nginx.test.sh`, attached to an existing Taskfile entry point that CI already runs.
- **Mutation discipline:** for each refusal and fallback path (contract, shared version, timeout, path normalization, header strip, invalid upstream), watch the test fail with the check removed and pass with it restored.
- **In-run checks, M1:** `task check:web`, touched Vitest files, the new built-mode browser target, `check-docs:web` + `docs:sync` when docs change, `check:changelog-entry` with one changelog fragment per shipping PR. M1 changes no nginx, chart or compose delivery.
- **In-run checks, M2:** first preflight Docker, Docker Compose, Helm and `yq`, and say which are missing rather than skipping silently. Then `task test:web-nginx`, the chart fixture driver through `render:openshift-check`, the compose generator test, the hermetic shell tests, `lint:yaml` / `lint:shell` for touched files, `check-docs:web` + `docs:sync`, and `check:changelog-entry`. Any compose stack the run starts uses an isolated throwaway project name and ports outside the `uzi` namespace, per `.claude/rules/stack.md`.
- No E2E contract literal changes are expected; sweep `e2e/` with `git grep -F` for any renamed identifier anyway.

## 6. Milestones

### M1: Host runtime

**Blocked by:** none.

Loader, `/modules.json` client and validation, `/m/:id/*` route and the "no module named" view, nav merge for sidebar and command menu, failure isolation and load timeout, SDK v1 with `sdk.api` and the contract check, shared-version policy, `web/examples/ui-module`, unit tests and the built-mode browser target served by the test harness (not nginx). Bundle and first-load delta measured.

Acceptance: examples 1 (list served by the harness), 2, 3, 4 and 5 pass in the built-mode browser test, including the remote-side incompatibility fixtures, the per-role refusal cards and the two-remote case; zero-module behaviour unchanged.

### M2: Delivery (nginx, chart, compose)

**Blocked by:** M1.

nginx include, module locations and `/modules.json` in all three configs and both images; chart `web.modules[]` (id, version, page, label, icon, navGroup, adminOnly, upstream) with validation, the new mounts and the resolver requirement; the compose generator and override example running the example module; the `assert-web-nginx.sh` extension, the chart fixture driver, the generator test; operator and developer docs.

Acceptance (in-run): example 1 holds under compose with no web image rebuild between "no modules" and "example enabled"; the chart renders and validates as §5 states; example 2's absent-upstream case holds on real nginx in both resolution modes; headers, hygiene and cache split are asserted on real nginx.

### Landing obligations (not milestones)

- Before merge, the exact PR head must pass full `test:web`, `test:web-browser`, `gate:repo`, the helm-chart job and, when the delivery diff triggers it, the `test-web-nginx` job in PR CI. Report the measured time the built-mode browser target adds to its CI job and the critical path.
- Maintainer acceptance on a Kubernetes deployment (first-class): the example module as its own Deployment and Service, enabled through `web.modules[]` with no web image rebuild, example 1 and the absent-upstream case observed.

## 7. Decision Log

| Decision | Reason; rejected alternative |
|---|---|
| Runtime Module Federation | Native pages without rebuilding the host; spike passed under the unchanged CSP. Reject iframes (no shared shell/router/theme) and build-time composition (a host rebuild per module change). |
| Same-origin proxy under `/modules/<id>/` | Keeps `script-src 'self'`. Reject allowlisting module origins (weakens the CSP, needs CORS). |
| List served by web nginx from deployment config | Fewest moving parts: no api change, no new endpoint; chart and compose already own nginx config. Reject `GET /api/modules` (api config plus an endpoint for no added safety). |
| Deployment-only enablement | Modules run with full page authority; enabling one is the operator's trust decision. Reject an admin UI toggle. |
| Request-time resolution for module upstreams | A missing module must not stop uzi's web tier from starting. Reject following `web.nginx.dynamicResolution` (default false, which resolves at startup). |
| Strip session headers on module proxies | Static files need no credentials; forwarding them would hand the session to another server. |
| One status-aware `Cache-Control` per response | Entries, manifests, unhashed files and errors must revalidate or not cache; `add_header` alone cannot express the status split. |
| Stale-chunk recovery by document reload, not retention or in-place swap | A Service may route an old-chunk request to a new pod, and the browser's module map keeps the imported v1 entry for the document's life, so only a reload gets v2. Reject in-place swap (needs a fresh module identity for the entry and its whole graph; unproven complexity) and guaranteed retention (the host cannot enforce it). |
| SDK as props, `sdk.api` over the host request path, vendored `sdk.ts` | Smallest contract; keeps auth, CSRF, 401 handling and mock mode in one place. Reject exposing the private `request()` (bypasses mock mode) and a published npm package (premature). |
| One supported contract major | Simplest rule the host can check. Reject multi-major support until a second module repo needs it. |
| Runtime `loadRemote` with a timeout, not `React.lazy` | Retry must work after a failure, and a stalled upstream must end in the error state. |
| `loaded-first` share strategy, strict versions | Keeps remotes lazy; refuses rather than double-loads React. |
| Nav stays visible on module failure | Avoids eager remote fetches; the route explains the failure. |
| One PRD for runtime and delivery, despite the size | The runtime alone gives operators no way to enable a module, and delivery has nothing to serve without the runtime; chart and compose delivery share one nginx, manifest and validation contract. Reject a runtime/delivery split and a chart/compose split. |
| Pages only, no slots | Smallest useful contract; slots inside existing pages can be added additively later. |
| Example module inside `web/` with no own `package.json` | CI's existing `npm ci` covers it; no network install in a gate. |
| No browser SRI | Dynamic ES module imports cannot carry integrity without an inline import map, which needs a CSP hash or nonce. Provenance comes from pinning module images by digest. |
| Runtime-only host | `@module-federation/runtime@2.9.3` (`init`, `registerRemotes`, `loadRemote`), lazy-imported only when the list is non-empty; the plugin is used only for remotes. Reject the host plugin (eager runtime, build-time remote config). |
| CSP amendment: literal artifact scan plus runtime enforcement | "No eval/new Function" is verified by a literal artifact scan (`web/scripts/csp-build-scan.mjs`) with hit and no-hit controls, plus runtime enforcement: the exact CSP header asserted on the host document, a deliberate-violation control page that the listener must report, and zero violations in every other scenario. Literal hits in the pinned MF sdk are removed at build time by a `define` plus a version-pinned transform that fails the build on drift. Reject an AST scanner (owner simplification) and adding `'unsafe-eval'`. |
| nginx layout | `/etc/nginx/uzi-modules/{modules.json,locations.conf}` outside `conf.d`; one flat `location ^~ /modules/<id>/` per module, identical text from chart and generator; a `/modules/` catch-all 404 `no-store` with full security headers; one `map "$status:$uri" $uzi_modcc`, named to avoid colliding with a module id `cc`. Reject per-config copies and a shared `$cc` variable. |
| Strip every X-Accel-* by njs | An njs header filter deletes every `x-accel-*` response header (image main `nginx.conf` = the pinned image's file + `load_module` + `js_import`, with a drift guard). Reject a fixed list (cannot cover unknown names). |
| Expanded hidden-header and cleared-request-header lists | From the delivery (PRD M2) nginx security review: `Service-Worker-Allowed`, HSTS, `Clear-Site-Data` and others hidden; `Proxy-Authorization` and `X-CSRF-Token` cleared. Reject the narrower list in the first draft. |
| Shared upstream grammar and fixtures | Chart and generator validate against the same fixtures so they cannot drift; dotless-host expansion is in the chart only (compose resolves a service name as is). Reject separate grammars. |
| Chart `subPath` mounts, rollout via `checksum/nginx` | Each pod stays consistent with its own nginx; mid-rollout generation skew is accepted and documented. Reject directory mounts (kubelet updates files under a running nginx). |
| Simple compose generator | Validate, encode before write, temp file + rename, `locations.conf` first; no lock or crash-recovery protocol and no crash-safety claim; operator rules are documented. Reject a locking/recovery protocol (owner simplification). |
| Compose acceptance deviates from `stack.md`'s literal `env -i` start (owner-approved) | Render with `env -i` into a literal file, then build/up/down from that file under a unique per-invocation `-p mh-4686-<pid>-<nonce>` project with a ports override, a pre-start round-trip comparison, env and mount checks, image-id equality as the no-rebuild proof, and `down -v` only after verifying volume prefixes (unnamed volumes of the invocation's own containers are allowed, owner-approved at landing). Accepted residual: CLI config, proxies and build settings can affect pulls and builds. |
| Example module image is a fixture only | `web/examples/ui-module/Dockerfile` is a test and acceptance fixture, shipped in no uzi image and published by no workflow. Reject shipping it as a sample image. |
| Modules nav group placement | A "Modules" group after Factory and before Settings/Admin/Docs; `work` and `factory` entries append in manifest order; sidebar sections are keyed by stable ids. Reject a group at the end. |
| Manifest validation in the SPA | Invalid entries are skipped with one `console.error` each; a later duplicate id is skipped (first valid wins); an unknown `navGroup` falls back to `modules`. Reject failing the whole list. |
| Reload page after a failed Retry | Offered next to Retry once a Retry fails transient, per module per document (a success clears it). Chunk URLs are not rewritten across the import graph, so a child chunk keeps its URL; pinned Chromium (Playwright 1.64.1) was observed to re-fetch a failed child chunk on Retry, so the harness pins both the Chromium recovery and the card path. Owner-approved at landing (MR review, 2026-11-25). |
| Stale vs transient errors | After a module's first successful load, dynamic-import and MF chunk failures under its prefix are stale and show "Reload page"; others remount with Retry and a forced re-registration. Reject Retry for stale chunks (the browser's module map keeps v1). |
| Measurement (PRD M1) | One local environment, 5 cold `/dashboard` loads per subject, zero modules, hooks-off builds; noisy, report only. First run: base transferred 2,160,102 B / JS 2,110,074 B / 36 requests / loadEventEnd 389 ms / LCP 560 ms, gzip bundle JS 2,137,382 B, all 2,158,024 B; candidate then 2,196,877 B / 2,146,321 B / 38 requests / 443 ms / 584 ms, gzip bundle JS 2,195,972 B, all 2,216,654 B; `/m/example` cold load 2,248,988 B transferred, 35 requests, loadEventEnd 411 ms. The two extra requests were `/modules.json` and a bundler runtime chunk (`rolldown-runtime`) preloaded by the entry, split out because the lazy federation chunk imported React and the router from the entry; that broke example 4 and was fixed (shared libs handed to the federation chunk at call time), and the harness now refuses any preload or unexpected asset in a zero-module load. After the fix: candidate 2,204,654 B / 2,155,188 B / 36 requests / 348 ms / 476 ms, gzip bundle JS 2,203,786 B, all 2,224,435 B; `/m/example` 2,254,861 B, 33 requests, 338 ms (no base rerun in that run, so no like-for-like delta). Harness wall time 43 to 54 s locally; hosted CI time is a landing obligation. |
| Example 4 by logical asset roles | The harness builds with a Vite manifest, takes S = static closure of `index.html` and F = static closure of `src/modules/federation.ts`, and with zero modules asserts one `/modules.json` request, no `/modules/` request, no request for or preload of an F \ S chunk, and no preload or unexpected asset at all. Reject hashed file names as the check (they change every build). |
| Guard before load | `/m/:id/*` is a protected route; `adminOnly` entries render inside `AdminRoute`; `ModuleLoader` is the only caller of `loadRemote`, in a post-mount effect, so signed-out and non-admin visits fetch nothing under `/modules/`. Reject loading first and hiding after. |
| Theme subscription | `sdk.theme.mode` is the polarity (`light` or `dark`) of the validated `data-theme` stamp; `subscribe` uses a `MutationObserver` on `<html data-theme>` and fires only when the polarity changes. Reject exposing theme names (palettes are host-internal). |
| Vendorable contract file | `web/src/modules/sdk-contract.ts` imports only `react` types and carries `HostSdkV1` plus the nine `ui` prop types; the host assigns its real components to `HostSdkV1["ui"]` so prop drift fails the host type-check. Reject vendoring `sdk.ts` (it imports host runtime values). |

## 8. Spike evidence (local throwaway spike, built mode)

`@module-federation/vite` 1.24.1 with Vite 8.3.8, React 19.3.1, react-router-dom 7.18.5; host with no build-time remotes, list fetched at runtime; nginx's exact CSP enforced through the preview server. Observed: zero CSP violations; no inline script in the host's built `index.html`; no `eval` or `new Function` in the remote's build; remote entry fetched only on first route visit; all requests on one origin, remote files under its prefix; shared React and router (functions identical; namespace objects differ because of the share proxy); host theme tokens and a host component (via props) render and follow a live theme switch; a remote-only rebuild took effect on reload; a module id added to the list after the host build loaded after a document reload (a second registration of the same remote); remote down and a render throw were both contained, and Retry recovered without a reload.

Not covered by the spike and required in §5: cache headers, a redeploy with an open tab, a separately deployed second remote, incompatible shared versions, a stalled upstream, and the host build's own `eval` scan. Live list refresh is out of scope (§3).

**Measured during implementation** (replacing the earlier UNVERIFIED line):

- Bundle and first-load delta with zero modules, the gzip bundle size and the `/m/example` load cost: see the "Measurement (PRD M1)" Decision Log row (report only, one local environment).
- Built-mode browser harness: 21/21 scenarios (two interrupt self-test phases, ready and launch), 43 to 54 s wall time locally across runs. The hosted CI time is a landing obligation, not measured here.
- `strictVersion` on a React major mismatch: the module is refused, never double-loaded (#RUNTIME-015, verified).
- `loadRemote` is raced against the 15 s timeout (the runtime is not relied on to honour an abort).
