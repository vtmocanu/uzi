---
title: Runtime web modules
order: 70
audience: operator
---

# Runtime web modules

A **module** is a separately built and deployed web feature that appears inside uzi as a native page: it gets uzi's shell, router, theme and session, a sidebar entry, a command-menu entry and a route at `/m/<id>`. You enable modules in deployment configuration (the Helm chart or the compose generator); no web image rebuild is needed. This page is for operators. To build a module, see [web-modules-dev.md](./web-modules-dev.md). Design rationale: `prds/done/4686-runtime-web-modules.md`.

With no modules configured, uzi looks and behaves as before. The host fetches one small, no-cache `GET /modules.json` (a `[]` file) and never loads the federation runtime. One local measurement of a cold `/dashboard` load counted about 36 KB more JavaScript (about 37 KB more transferred) and two more requests than before the feature; that was a single noisy run, not a guarantee.

## Trust model

Read this before enabling anything.

- **A module runs with full page authority as the signed-in user.** Its JavaScript executes in uzi's origin: it can read the page, and call any API the user can call. Module Federation is not a sandbox, and nothing here contains a malicious module.
- **Enabling a module is your trust decision.** Configuration is deployment-only; there is no UI or API to add one. Deploy only modules you trust, and pin each module image by digest.
- **Module files are public to anyone who can reach the web tier.** `/modules.json` and every file under `/modules/<id>/` need no authentication, including the id, label, version and `adminOnly` flag of admin-only modules. `adminOnly` hides the nav entry and gates the route in the UI only; the API still authorizes every call. Keep secrets and personal or confidential material out of a module and out of the list.
- **The CSP is unchanged.** Modules are served same-origin, so `script-src 'self'` stays as it is and no module origin is allowlisted.
- **Module discovery and asset serving have no api involvement.** The list and the module files are served by the web nginx, with no api call, privilege or credential.
- **The one api surface for modules is per-user storage.** A module can keep one small JSON document per module id in the signed-in user's own account (see [Per-user storage](#per-user-storage)). The module id is a storage namespace, not a permission boundary: any module or page script can read and write any valid id for that user. Use it for preferences, not secrets. It is separate from the public module files above, which need no authentication.
- **Module code is not integrity-checked in the browser** (no SRI). Provenance comes from pinning the module image by digest.

## The module list

Each entry has these fields.

| Field | Rule |
|---|---|
| `id` | Required. `^[a-z][a-z0-9-]{0,31}$`, unique. Names the route `/m/<id>`, the asset prefix `/modules/<id>/` and the federation remote name. |
| `version` | Required, a string (quote it: `1.0` is a number in YAML). Shown to admins in error cards; it is not part of any URL and does no cache busting. |
| `page` | Required. The expose name the module's build exposes (the example uses `./Page`; a leading `./` is optional). Rendered at `/m/<id>/*`. |
| `label` | Required. 1 to 40 characters. Control and invisible formatting characters are refused in `label`, `version` and `page`. |
| `icon` | Required. One of `activity`, `board`, `book`, `bot`, `bug`, `chat`, `clock`, `factory`, `file-text`, `gear`, `git`, `home`, `key`, `package`, `scale`, `search`, `server`, `shield`, `skill`, `terminal`. |
| `navGroup` | Required. `work` or `factory` appends the entry to that existing sidebar section; `modules` puts it in a new section after Factory (shown only when it has an entry), headed "Modules" unless the entry sets `navSection`. |
| `navSection` | Optional. The sidebar heading for a `navGroup: modules` entry; refused on a `work` or `factory` entry. A string, 1 to 24 characters (Unicode code points) after surrounding whitespace is trimmed; control and invisible formatting characters in the raw value are refused, as for `label`. The trimmed value is both the heading and the grouping key, so `Tools` and `Tools ` share one section. Entries with the same heading share one section, in list order; sections appear in order of their first entry, after Factory and before the unlabelled bottom cluster. Absent and an explicit `Modules` both mean the default "Modules" section. The command-menu hint shows the heading. `/modules.json` carries `navSection` (trimmed) only when set, so a list without it is byte-identical to before. A heading may reuse a built-in name such as "Work"; it is still a separate section. |
| `adminOnly` | Optional boolean, default `false`. Hides the nav and command-menu entries from non-admins and puts the route behind the admin gate. Presentation only. |
| `upstream` | Required in chart values and in the generator input; never appears in `/modules.json`. `[http://]HOST:PORT`: `HOST` is a strict IPv4 address or a lowercase DNS-1123 name, `PORT` is 1 to 65535, nothing else (no path, query, userinfo or other scheme). |

In the chart only, a dotless `HOST` (a Service name such as `example-module`) expands to `<host>.<release namespace>.svc.cluster.local`. The compose generator does not expand: Docker resolves a service name as it is.

A bad list never deploys: the chart fails the render, and the generator exits non-zero and writes nothing; a per-entry error names `modules[<index>]` and the field. The generator reports the same `navSection` messages as the chart, plus `navSection is not valid UTF-8 text`. Unknown input fields are ignored and never published.

The browser re-validates `/modules.json` because the file can be hand-edited: it skips an entry with an invalid id, a missing required field, a bad label, icon or `adminOnly`, a `label`, `version` or `page` containing control or invisible characters, or a duplicate id (the first valid entry wins), and maps an unknown `navGroup` to `modules`. Each skip logs one `console.error` naming the entry. An invalid `navSection` does not skip the entry: the value is dropped with one `console.error` and the entry falls back to the default "Modules" section. `navSection` is ignored on a `work` or `factory` entry, and an entry with an unknown `navGroup` keeps its warning and lands under "Modules" even with a valid `navSection`. A file that is not a JSON array means no modules.

## Enable modules with the Helm chart

Set `web.modules[]` and the cluster DNS resolver in your values (see `deploy/chart/values.yaml` for the commented reference):

```yaml
web:
  nginx:
    resolver: 10.96.0.10   # your cluster DNS ClusterIP: kubectl -n kube-system get svc kube-dns -o jsonpath={.spec.clusterIP}
  modules:
    - id: example
      version: "1.0.0"
      page: Page
      label: Example
      icon: package
      navGroup: modules
      navSection: Tools      # optional sidebar heading; omit for "Modules"
      adminOnly: false
      upstream: example-module:8080
```

- **`web.nginx.resolver` is required whenever `web.modules` is non-empty.** The chart fails the render without it. nginx resolves each module upstream per request, which needs a resolver. This is the same value `web.nginx.dynamicResolution` uses for the api upstream; you do not need to turn `dynamicResolution` on, and enabling modules does not change how the api upstream resolves.
- The module itself is your own Deployment and Service, serving its build at `/modules/<id>/`. The chart does not create it.
- The list and the nginx locations are rendered into the web ConfigMap and mounted into each web pod as individual files (`subPath`), so a pod's files always match what its own nginx loaded at start. A change to the list rolls the web pods through the `checksum/nginx` annotation. While that rollout is mixed, different pods can serve different generations of the list; a page load may see either until it completes.
- Chart render failures name `web.modules[<index>]` and the rule that failed: malformed or duplicate `id`, a missing required field, a `label` outside 1 to 40 characters, an unknown `icon` or `navGroup`, an invalid `navSection` (`navSection must be a string`, `navSection contains control or invisible formatting characters`, `navSection must be 1 to 24 characters after trimming whitespace`) or one on a `work` or `factory` entry (`navSection is only valid with navGroup modules`), a non-boolean `adminOnly`, an invalid `upstream`, or modules set without `web.nginx.resolver`.

## Enable modules with compose

The compose stack takes the same list through a generator and an override file.

1. Write the list as JSON with the chart's field names, including `upstream` as the compose service name and port. `deploy/compose/web-modules.example.json` runs the example module.
2. Generate the two files the web nginx reads:

   ```sh
   scripts/web-modules-gen.py deploy/compose/web-modules.example.json .web-modules
   ```

   The output directory gets `locations.conf` (one proxy location per module) and `modules.json` (the public list). The override bind-mounts `${UZI_WEB_MODULES_DIR:-./.web-modules}`; if you set `UZI_WEB_MODULES_DIR`, pass that same directory to the generator (use an absolute path, or run both from the repo root: compose resolves a relative path against the project directory, the generator against the current directory). The directory must exist before `up` (the mount does not create it), and its files must be world-readable.
3. Start the stack with the override and recreate `web`:

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.modules.yml \
     up -d --force-recreate web example-module
   ```

   `docker-compose.modules.yml` bind-mounts the directory read-only into the web container and also defines `example-module`, the example module as its own container. That service is an acceptance fixture only; it publishes no port and is reached through the web nginx. For your own modules, add your own services to your override.

Generator rules. There is no lock and no crash-recovery protocol, and the generator makes no crash-safety claim, so follow these:

1. Run one generator invocation at a time.
2. Recreate the web container only after the generator has exited 0.
3. After an interruption (a killed run, a full disk), rerun the generator first.

**The bind-mounted window.** The directory is bind-mounted, so a running web container serves a new `modules.json` as soon as the generator renames it into place, while its nginx keeps the old locations until the container is recreated. A module you just added therefore shows its error card, and a removed module still proxies, until you recreate `web`. nginx reads the two files only at start, so a container that is never recreated never changes its routes.

The generator exits 0 when written, 1 on invalid input (nothing written), 2 on a usage, input-read (including malformed JSON) or output failure. `scripts/web-modules-gen.py --help` has the full text.

## What nginx does on module paths

Every module gets one `location ^~ /modules/<id>/` proxying to its upstream with the path unchanged. The chart and the generator emit identical text.

- **Methods.** `GET` and `HEAD` only; anything else is refused with 403 before it reaches the module.
- **Request hygiene.** `Cookie`, `Authorization`, `Proxy-Authorization` and `X-CSRF-Token` are not forwarded, so the host session never reaches a module server. The module receives the raw request URI.
- **Response headers.** The upstream's `Cache-Control`, `Expires`, `Set-Cookie`, `Content-Security-Policy`, `Content-Security-Policy-Report-Only`, `X-Frame-Options`, `X-Content-Type-Options`, `Referrer-Policy`, `Service-Worker-Allowed`, `Clear-Site-Data`, `Strict-Transport-Security`, `Refresh`, `Permissions-Policy`, the five `Access-Control-*` headers, `Report-To` and `NEL` are hidden. The host's own security headers and CSP are then set on every module response. nginx ignores the upstream's `X-Accel-*` headers, and every response header starting with `X-Accel-` is stripped by an njs header filter, so a module cannot trigger an nginx internal redirect.
- **Caching.** Each module response carries exactly one `Cache-Control`, chosen on the final status and path: `public, max-age=31536000, immutable` for a 200, 206 or 304 under `/modules/<id>/assets/`; `no-cache` for any other 200, 206 or 304 (`remoteEntry.js`, `mf-manifest.json`, unhashed files); `no-store` for every other status, so a redirect, 404 or 502 is never cached.
- **Unknown ids.** A path under `/modules/` with no configured module (a removed module, a typo) is a 404 with `no-store` and the full security headers.
- **Request-time DNS.** Upstreams resolve per request. nginx starts and serves uzi even when a module Service or container does not exist yet; requests for that module get 502 and its page shows the error card.
- **`/modules.json`.** Served from the generated file with `no-cache`, so an edited list is seen on the next document load. An open tab does not refresh it.
- **The image.** The web images ship a custom main `nginx.conf` (the pinned base image's file plus the njs `load_module` and `js_import`), an empty `[]` list and an empty locations file. They need no rebuild to enable a module. The chart's web pod mounts the generated files over those defaults.

### Accepted limits

- **A module must use relative redirects.** `proxy_redirect` is off and the module's origin is not the browser's, so an absolute `Location` would point at the module server. Redirects pass through as ordinary 3xx responses with `no-store`.
- **The raw request URI is forwarded, but the cache policy is decided on the normalized path.** A crafted path can pick the immutable policy for its own URL, which affects only that URL in that browser's cache.
- **The upstream sees the proxy's `Host`** (the upstream host and port), speaks HTTP/1.0 to it and gets no keepalive. A static module needs none of these.

## What is not contained

The host contains these failures to the module's own page: a load failure (network error, HTTP error, timeout), an error thrown while rendering, and a refusal (contract, shared-version or missing-expose mismatch). It does not contain:

- A module's JavaScript. It is not sandboxed and can call any API as the signed-in user.
- Errors in a module's own async code outside React rendering. They surface as console errors and unhandled rejections.
- Global CSS a module injects, a module blocking the main thread, or deliberate misuse of page authority. A trusted module scopes its CSS.
- Module code that runs before the compatibility check. The check runs after the page is loaded, so it prevents rendering, not execution of the module's top-level code.

## Per-user storage

A signed-in user can keep one small JSON document per module id. Modules reach it through `sdk.api.request`, which already sends the session cookie and the CSRF header (see [web-modules-dev.md](./web-modules-dev.md)). No CLI command uses these routes.

Routes: `GET`, `PUT` and `DELETE /api/modules/{module_id}/user-data`.

- **Cookie session only.** A Bearer CLI token gets 401. `PUT` and `DELETE` also need the CSRF header.
- **Module id.** It must match the manifest rule `^[a-z][a-z0-9-]{0,31}$`, else 400 `invalid module`. The api does not check that the module is configured, so a document can exist for an id that is not in the list.
- **No caching.** Every api response on these paths carries `Cache-Control: no-store`.
- **Own documents only.** A user sees only their own documents; there is no admin or cross-user route.
- **Backups.** A full-database operator backup (a full `pg_dump`, or the chart's database backups when enabled; they are off by default) includes these documents.

| Call | Request | Success | Other outcomes |
|---|---|---|---|
| `GET` | none | 200 `{"data": <value>\|null, "revision": "<string>"\|null}` | none stored: `data` and `revision` are `null` |
| `PUT` | `{"data": <any JSON except null>, "expected_revision": <string\|null>}` | 200 `{"revision": "<r>"}` | 409 `{"revision": <current or null>}` on a stale or unknown revision, nothing changes; 409 `too many module documents`; 413; 400 |
| `DELETE` | `?expected_revision=<r>` (required) | 204 | 400 when the parameter is absent; 409 with the current revision when it is present but empty or stale |

`PUT` rules:

- Both keys are required, with exactly these names, and no others are allowed. A `null` `expected_revision` means create: no document may exist.
- **Limits.** At most 32 documents per user: a new id past that gets 409 `too many module documents`, while existing ids stay updatable. The whole request body, wrapper included, may be at most 64 KiB, else 413.
- **400** for malformed, missing or extra fields; values PostgreSQL cannot store (for example `\u0000`, invalid UTF-8, lone surrogates); numbers outside the finite float64 (JavaScript number) range or nonzero numbers that underflow to zero; and `data` nested deeper than 10,000 levels. Zero-valued numbers (for example `0e-400`, `-0.0`) are stored as `0`. If a key repeats, the last wins.
- **Round trip.** Values round-trip at value level with `jsonb` normalisation: key order, whitespace and duplicate keys are not preserved. A Go client using `encoding/json` cannot unmarshal a `GET` response whose `data` is 10,000 levels deep, because the wrapper adds a level.

Revisions are opaque strings drawn from one database sequence. Compare them only for equality; there is no ordering promise. A revision is never reused, not even after a delete and re-create, on the same database primary with PostgreSQL's default durability settings (`synchronous_commit` on, `fsync` on, storage that honours flushes). There is no guarantee across a failover, a point-in-time restore, a manual sequence reset or a rollback of the migration that created the storage.

**Unknown outcome.** A 503 `{"error":"outcome unknown"}` means the commit outcome is not known. `GET` and decide again from what you read, which may include other writers' changes. Never write unconditionally, and do not blindly re-create after an uncertain create.

**Data is user-controlled.** Validate it when you read it back. As in the [trust model](#trust-model), the module id is a namespace and not a permission boundary, and modules share the page's authority, so keep secrets out of it.

**What guarantees what.** The api guarantees the 64 KiB streaming cap, the choice between 400 and 413, and `no-store`. The web tier's nginx proxy may answer with its own error page first, for example its own 413 above 1 MiB, before the api runs.

## Troubleshooting

Admins see the module id, its `version` and the reason in the error card; other users see only a generic message.

| What you see | Meaning and fix |
|---|---|
| "Module unavailable", "This module could not be loaded", **Retry** | A transient failure before the module ever loaded in this tab: its server is down or unreachable, or it did not answer within 15 seconds. Check the module's Deployment or container and the 502 below, then Retry. Retry loads the module again without a page reload. If a Retry fails again, the card also offers **Reload page**; once the module is back, reload to recover. |
| "Module unavailable", "This module was updated while you had the page open", **Reload page** | After the module loaded, one of its files was replaced (a rollout) and the tab asked for an old file. Reload the page. During a mixed rollout a reload can still reach the old version; once the rollout completes it reaches the new one. Module authors can keep the previous build's hashed files to avoid this. |
| "Module unavailable", "This module is not compatible with this version of uzi", no action | The module targets a different host contract major, was built against an incompatible React, react-dom or react-router-dom version, or does not expose the configured `page`. Retry cannot help: rebuild the module for this uzi version. Admins see which. |
| "Module unavailable", "This module hit an error and stopped", **Retry** | The module threw while rendering. Retry remounts the page. |
| "No module named `<id>` is configured" | The route `/m/<id>` is not in the loaded list: a typo, a removed module, or a list change not yet seen (reload the page). On compose also see the bind-mounted window above. |
| 502 for `/modules/<id>/...` | nginx could not reach the upstream: the module is not deployed, the Service or container name or port is wrong, or the resolver cannot resolve it. Check `upstream`, and on the chart `web.nginx.resolver`. The rest of uzi keeps working. |
| 404 for `/modules/<id>/...` | No location for that id in this web container (see the bind-mounted window on compose, or the rollout on the chart), or the module server has no such file. |
| A newly added module shows its error card on compose | You have not recreated `web` since the generator ran. |
| A nav entry shows but the page fails | The entry stays visible when a module fails to load so uzi does not fetch every module up front; the page explains the failure. |
| `helm template` / `helm upgrade` fails naming `web.modules[N]` | An invalid list or a missing resolver; see the chart render failures above. |
| The generator exits 1 or 2 | 1: an invalid list (a per-entry error names `modules[N]` and the field). 2: a usage error, unreadable or malformed JSON input, or an output failure. Nothing was written on invalid input. Fix the cause and rerun. |
| Nothing changes after editing the list | A list change takes effect on the next document load, and on compose only after `web` is recreated. |
