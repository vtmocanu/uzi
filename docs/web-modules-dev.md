---
title: Building a runtime web module
order: 4686
audience: contributor
---

# Building a runtime web module

How to build a module that uzi loads at runtime. Operators enabling a module: see [web-modules.md](./web-modules.md), which also covers the trust model (a module runs with full page authority as the signed-in user). Design rationale: `prds/done/4686-runtime-web-modules.md`.

A module is a React app built as a Module Federation **remote**. It exposes one page component, which the host renders at `/m/<id>/*` and which owns its own nested `<Routes>` relative to `/m/<id>`. The host loads it with the federation runtime; the module shares the host's React, so it renders as native uzi UI, not an iframe.

## The reference build

`web/examples/ui-module/` is the reference module. It is a test and acceptance fixture only: it ships in no uzi image and no workflow publishes it. Use its `vite.config.ts` as a guide (the file comments say what a module must do), not as a drop-in file: it builds several test variants, reads `web/package.json` by relative path and imports the plugin from `web/scripts/`. In your own repo, vendor `web/scripts/vite-mf-no-new-function.mjs` and `web/scripts/csp-build-scan.mjs` alongside `sdk-contract.ts`, take `requiredVersion` from the uzi release you target, and drop the variant switches and test-only defines. It has no `package.json` of its own: it resolves dependencies from `web/node_modules`, so a real module repo adds its own.

What a module build must set:

- **`@module-federation/vite`** as a remote named after the module id, with `filename: "remoteEntry.js"` and the page under `exposes` (the example exposes `./Page`; the list entry's `page` names it).
- **`base: "/modules/<id>/"`.** The web nginx proxies that prefix to the module's server and keeps the path.
- **Shared singletons** `react`, `react-dom`, `react-router-dom` and `react/jsx-runtime`, each `singleton: true`, `strictVersion: true`, with `requiredVersion` equal to the host's range (`web/package.json`), and `shareStrategy: "loaded-first"`.
- **The same CSP-safe build as the host.** uzi's CSP has no `'unsafe-eval'`, so a module's output must carry no dynamic-code literal:
  - `define: { FEDERATION_ALLOW_NEW_FUNCTION: "undefined", __system_context__: "undefined" }` removes the federation runtime's own uses of the Function constructor.
  - The `mfNoNewFunction()` plugin (`web/scripts/vite-mf-no-new-function.mjs`) stubs the three unguarded Function-constructor sites in `@module-federation/sdk`, so the stubbed code throws only if called. It is pinned to `@module-federation/sdk@2.9.3` and fails the build if the installed sdk version differs or a snippet is not found exactly once; an sdk bump needs the pin and snippets bumped together.
  - `node web/scripts/csp-build-scan.mjs <dir>...` is a literal text scan of a build output for eval calls, the Function constructor (with or without `new`) and inline `<script>` tags without a `src` attribute (exit 0 clean, 1 hits, 2 missing or empty directory). It is a literal scan, not a proof: dynamically constructed code is also stopped at runtime by the CSP.

The example's `Dockerfile` and `nginx.conf` show a static server for a module image; they are also fixtures, not a supported base image.

## Contract v1

The exposed component receives one prop, `sdk: HostSdkV1`, and its module file also exports `hostContract = 1`. The host supports exactly one contract major and refuses to render a module that exports a different one (or none).

`web/src/modules/sdk-contract.ts` is the single source of the contract types. **Vendor that one file** at the uzi tag your module targets: it imports only types from `react` and nothing from the host, so it compiles against your module's own React. It declares `HostSdkV1`, `HOST_CONTRACT` and the prop types of the nine `ui` components; the host type-checks its real components against those prop types, so a host-side prop change cannot drift silently. `sdk.ts` (the host's factory) and the other host files are not for vendoring. There is no published npm package.

The example module imports its types from `sdk-contract.ts` the same way.

| Field | What it gives you |
|---|---|
| `contract` | `1`. |
| `user` | `id`, `displayName`, `isAdmin` of the signed-in user. |
| `api.request<T>(method, path, body?)` | Calls the uzi API as the user; see below. |
| `ui` | `Card`, `CardHeader`, `Button`, `Select`, `Badge`, `StatusPill`, `EmptyState`, `Spinner`, `Skeleton`, the host's own components. |
| `theme` | `mode` (`"light"` or `"dark"`, read live) and `subscribe(cb)` which calls `cb` only when the polarity changes and returns the unsubscribe function. |
| `navigate(to)` | A router helper for links outside your own `/m/<id>` subtree. |

`sdk.api.request` runs the host's own request path: the session cookie, CSRF header, the 401 logout path and vault-lock handling all apply, and it rejects with the host's `ApiError` (HTTP status and server message). Restrictions:

- `method` is one of `GET`, `POST`, `PUT`, `PATCH`, `DELETE`.
- `path` is relative to `/api` and must start with `/`; the host prepends `/api`.
- A path that could leave `/api/` is rejected with a 400 `ApiError` and no request is made: backslashes, whitespace or control characters anywhere; `//` empty segments, encoded separators or dots (`%2f`, `%5c`, `%2e`, any case) and `.` or `..` segments in the path part (before `?` or `#`, so query values such as `branch=a%2Fb` stay legal).
- In a mock (demo) build it never touches the network and rejects with 503 "not available in demo mode".

A module can call any API route the user can; `sdk.api` is a convenience that keeps auth in one place, not a permission boundary.

## Shared libraries

The host provides `react`, `react-dom`, `react-router-dom` and `react/jsx-runtime` as strict singletons with `loaded-first` sharing. If your build requires a version the host's does not satisfy (for example React 18 against React 19), the host **refuses** to render the module with a "not compatible" card; it never loads a second copy of React. Match the host's ranges from `web/package.json` at the uzi tag you target.

## Caching and rollouts

The host's nginx sets `Cache-Control` for every module response from the path and status, ignoring what your server sends:

- Hashed files under `assets/` (Vite's output): `public, max-age=31536000, immutable`.
- Everything else (`remoteEntry.js`, `mf-manifest.json`, unhashed files): `no-cache`.
- Any other status (redirects, 404, 502): `no-store`.

So emit hashed chunks under `assets/` and nothing mutable there. A browser keeps an imported `remoteEntry.js` for the life of the document, so an open tab keeps running the old version. When you redeploy, a tab that later asks for an old hashed chunk may get a 404 and shows a "Reload page" card. Recommendation: **keep the previous build's `assets/` in your new image** so open tabs finish cleanly; the host does not rely on it. Redirects from your server must be relative (the host does not rewrite `Location`).

## CSS

Your CSS lives in the host document. Scope every rule under a module-prefixed root class (the example uses `.uzi-mod-example`) and do not inject global CSS. Read colours from the host's CSS variables (space-separated RGB triplets, for example `rgb(var(--brand))`, `rgb(var(--surface))`, `rgb(var(--fg))`) so the page follows a live theme switch; use `sdk.theme` only when you need the polarity in script.

## Versioning policy

Within a contract major, changes to `HostSdkV1` are additive only. A breaking change bumps the major in a **minor** uzi release whose changelog names it one release ahead; operators then upgrade their modules together with uzi. The host supports exactly one major at a time, so a module built for another major is refused.

## Testing locally

```sh
task test:web-modules-browser
```

builds the host and the example module (and fixture variants: a second module, an incompatible React range, `hostContract: 2`, a missing expose, a v1/v2 rebuild) and drives real Chromium through load, failure, retry, refusal, stale-chunk and two-remote scenarios. It is part of `task test:web-browser`. Other checks: `task test:web-modules-gen` (the compose generator), `task render:web-modules-check` (the chart's list validation and rendering; needs helm and yq) and `task test:web-modules-compose` (an opt-in live compose acceptance that builds the stack from source and takes many minutes).
