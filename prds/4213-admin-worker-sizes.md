# PRD #4213: Admin-defined hosted worker sizes

**Status**: Draft
**Priority**: Normal
**Umbrella**: #2415
**Blocked by**: #4212 for M2 and M3 only (the editor lives on Admin → Workers; the ephemeral default size setting is #4212's). M1 needs nothing from #4212.

## Problem

Hosted worker sizes (`s`, `m`, `l`, `l-highmem`) are Go constants in the controller (`controller/internal/preset/preset.go`, `sizes` map), with the names mirrored in `api/internal/workersize`, `web/src/lib/workerSizes.ts`, the TUI, the chart and several cross-module goldens. Every change to a size's CPU, memory or disk, or a new size, needs a code change and a release (#625, #1356, #2412, #3383, #3890). An admin who sees workers OOM at 20Gi, or wants a cheaper size for light repos, cannot act without a maintainer and a release.

## Outcome

An admin edits, adds and removes hosted worker sizes on **Admin → Workers**. Shipped sizes keep their current values as defaults with reset-to-default. Values the cluster's configured ceilings cannot honour are refused at save time; admission failures the api cannot predict are surfaced on the affected worker and in Admin health, never silently clamped or stranded.

Acceptance examples:

1. An admin raises `l` memory limit from 20Gi to 24Gi and saves. The provision form and size badges show 24Gi at once. Every idle `l` worker rolls once onto the new limit through the existing paced roll (ADR-1470); a busy one rolls when it goes idle. Reset-to-default returns `l` to the shipped values and rolls again. A disk-only edit (grow `m` data 25Gi → 40Gi, within the shipped 50Gi `maxPVCStorage`) rolls nothing: claims grow in place via ADR-3890 where the StorageClass sets `allowVolumeExpansion`.
2. An admin adds size `xs` (250m/1 CPU, 1Gi/2Gi memory, 5Gi data). It appears in the web provision form, `uzi worker list`/`get` and the TUI, and it can be chosen as the ephemeral default size (#4212). Provisioning an `xs` worker produces a pod with exactly those quantities.
3. An admin types a memory limit of 64Gi while the lowest configured LimitRange `max.memory` across enabled tiers is 32Gi. The save is refused with a field error naming the ceiling and its chart value (e.g. `workers.limitRange.max.memory`).
4. An admin tries to remove `xs` while a worker is `xs`, or `xs` is the ephemeral or new-worker default size. The removal is refused, naming what still uses it. Builtin sizes cannot be removed, only edited and reset.
5. An admin lowers `m` data disk from 40Gi back to 25Gi (or resets it). The save is accepted; workers provisioned afterwards get 25Gi claims, while existing claims keep 40Gi because the controller never shrinks a claim (ADR-3890). The editor says so before saving.
6. During a rolling upgrade where the controller does not yet support admin sizes, the editor is read-only and says why; no edit is accepted that the controller would ignore.

## Out of scope

- Changing the namespace ceilings (LimitRange, ResourceQuota) from the UI: Helm-only.
- Per-user or per-repo size restrictions.
- Templates (image families): stay name-only, resolved by the controller (the remaining half of PRD #58's authority rule; the ADR says so).
- `/nix` size (flat across sizes) and the DinD cluster override `UZI_WORKER_DIND_DATA_SIZE` (ADR-3890 D1).
- The isolated lane's run-bound `/data` (`ephemeralWorkerDataSize`): a flat replacement, not per size.
- Run slots per size: a size still buys headroom for one run, never parallelism (PRD #58).
- A CLI provisioning command: `uzi worker` has list/rm/set-token/cordon/uncordon/get and no create (`api/cmd/uzi/worker.go`); persistent provisioning stays web-only.
- `api/openapi/v1.yaml` carries no size references, so `check:api-v1-compat` is unaffected; `git grep -E 'l-highmem|hosted_size' -- e2e` found nothing on 2026-11-12, but the standard E2E contract sweep (CLAUDE.md) still runs over the changed literals, wire fields and DTOs.

## Design: what changes and what stays

This reverses PRD #58 Decision 7 ("the api sends the size NAME; the controller owns every mapping to a pod-spec value") for sizes only. PRD #58's Decision Log (2026-07-16 entry on configurable presets) deferred this for two reasons; the design answers both:

1. **"The api cannot validate a quantity"** (`k8s.io/apimachinery` is banned from the api by `api/internal/hostedsvc/no_kube_dependency_test.go`, Decision 1). Answer: the api never handles quantity strings. A size is stored and sent as **plain integers**: `cpu_request_millicores`, `cpu_limit_millicores`, `memory_request_mib`, `memory_limit_mib`, `data_gib`, `dind_data_gib`. Validation is integer arithmetic. The controller builds `resource.Quantity` from integers with fixed formats: **CPU as DecimalSI millicores, memory and storage as BinarySI** (`14336 MiB` must render as `14Gi`, never `15032385536`, or the spec-hash changes and the whole fleet rolls on deploy). The no-kube-dependency guard stays untouched.
2. **"A bad value strands workers with no error anywhere"** (v1 had no pod-phase status, Decision 10). Answer: (a) the api knows the configured ceilings (below) and refuses at write time; (b) the controller still refuses at render any size above its own env ceilings; (c) every admission failure the api cannot predict is reported per worker on the existing status report and shown in Admin health (ADR-1484): pod-level rejections already surface as ReplicaFailure / roll phase `stuck` (`controller/internal/kube/materializer.go` `replicaFailureReason`, `rollhealth.go`); **PVC-create rejections are not reported today** (`pvcceiling.go` header: "Nothing is reported to the api") and M2 adds that report.

What stays:

- The api holds no kube access and no kube credential (Decision 1).
- Size names stay a validated vocabulary: lowercase, `^[a-z][a-z0-9-]{0,15}$`, never free text on a pod spec.
- The controller remains the last check at render.
- Images and templates resolve controller-side by name.

## Modules and seams

### Size table store (`api`)

- New table `worker_sizes` (additive goose migration; **no seed rows in the migration**): `name` PK, the integer fields, `display_order`, `builtin` bool, `customized` bool, `updated_at`, `updated_by`. Boot (Go) inserts the four shipped sizes with today's exact values when absent and re-applies shipped values to builtin rows with `customized = false`, the builtin-agent-template pattern (`ReconcileBuiltinTemplates` / `RefreshPristineBuiltin`); an edited row is never overwritten; reset sets shipped values and `customized = false`.
- `api/internal/workersize` becomes a reader over the store (cache, like `settings.Cache`), keeping `Valid(name)` for its callers (`handler/hosted_workers.go`, the ephemeral provisioner `hostedsvc/ephemeral.go`, the #4212 `ephemeral_default_size` setting). `Valid` can no longer run before the store exists: `UZI_EPHEMERAL_DEFAULT_SIZE` (validated at `config.Load`, `config.go:~1464`, and by the chart's hardcoded `fail` list in `deploy/chart/templates/api-deployment.yaml:6-7`, asserted by `scripts/assert-ephemeral-lease-render.sh:~132-153`) may name **builtin sizes only**; an admin-set value (#4212 setting) may name any existing size.
- Reads: the web provision form gets the size list (names + integers) from the existing `HostedConfig` (`api/internal/handler/hosted_workers.go:~319`, cookie-only via `requireAuth`, `routes_workers.go:39-47`; update its comment that the lists stay web mirrors). The CLI and TUI only display existing workers, so the worker DTO they already read through the `RequireUser` worker routes (`api/internal/apitypes/worker.go`, `HostedSize`) gains the size's resolved integers for display. No new CLI-readable route; provision and token creation stay cookie-only; a test pins that `HostedConfig` still refuses a CLI bearer token.
- Writes: admin-only upsert, delete and reset (cookie + CSRF, same middleware as admin settings).
- Concurrency (pinned; the FK alternative is rejected because soft-deleted worker rows would keep a removed size referenced):
  - every size-table mutation (add, edit, reset, delete) takes one transaction-scoped advisory lock for the table, so the 16-size maximum and name uniqueness are checked against committed state;
  - delete additionally takes `FOR UPDATE` on its row;
  - every write that references a size takes `FOR SHARE` on that row inside its own transaction and checks membership against the database, not the cache: both hosted-worker provision inserts (`api/internal/store/queries/hosted_workers.sql:~182`, `:~498`) and both #4212 default-size settings writes (`hosted_default_size`, `ephemeral_default_size`);
  - live-DB tests race delete against each of those writes.
- Delete refuses while any non-deleted worker uses the size or the effective ephemeral default or new-worker default (#4212 `hosted_default_size`) names it; builtin rows refuse delete. Both #4212 default-size settings may name any existing size once M3 lands.
- Bounds: at most 16 sizes; each integer > 0; `request ≤ limit`; ceilings as below.

### Ceilings (`deploy/chart`, `api/internal/config`)

- The poll is a GET with no request body by design (`api/internal/hostedsvc/protocol.go:~18`), so ceilings do **not** travel from the controller. The chart renders them into the **api** env from the same values that feed the controller's LimitRanges (`workers.limitRange.max.{cpu,memory}`, `maxPVCStorage`, and the docker and isolated tiers' equivalents), as it already does for `HOSTED_WORKER_VERSION`. (The existing status POST was considered as a source; chart env was chosen because both components then read one set of values with no freshness question.)
- Normalization happens in the chart, not the api: chart quantity strings are converted to integers with the existing `uzi.quantityToBytes` helper (`deploy/chart/templates/_helpers.tpl:730`) plus a millicore equivalent for CPU, **rounded down** (a ceiling is never overstated), and a malformed or overflowing value fails the render. A disabled tier renders no ceiling and is excluded from the minimum. The api parses the env as base-10 integers at boot and refuses to start on a malformed one.
- The per-field absolute sanity caps (64000 millicores, 524288 MiB, 2048 GiB) always apply, in addition to any tier ceiling and whether or not a DinD override is set; on compose (no LimitRange) they are the only bound.
- Rules (sizes are global, not per tier, `values.yaml` quota comment):
  - cpu and memory limit ≤ the minimum `max` across **enabled** tiers (restricted, docker, isolated);
  - `data_gib` ≤ the minimum `maxPVCStorage` of the persistent tiers (restricted, docker); the isolated lane's `/data` is `ephemeralWorkerDataSize`, not the size's;
  - `dind_data_gib` ≤ the docker tier's `maxPVCStorage`, not validated when `workers.docker.dindDataSize` overrides it;
  - disk fields may go up or down: a lower desired size affects only claims created afterwards; existing claims never shrink (ADR-3890 converges upward only). Reset and the pristine refresh may therefore lower a desired disk size; the editor warns that existing claims keep their size.
- **ResourceQuota is advisory, not a refusal**: when `request × quota.deployments` exceeds the tier's `requestsCPU`/`requestsMemory` (`values.yaml:~935-947`), the editor warns; an actual quota rejection surfaces through ReplicaFailure / roll `stuck` in Admin health.

### Wire (`api/internal/hostedsvc/protocol.go`, `controller/internal/apiclient`)

- **Resolved integers ride inline on each `DesiredWorker`**, next to the existing `Size` name. No separate table, no digest, no table-skew case: the poll is already full desired state.
- Refusals and PVC admission failures ride the existing `POST /api/controller/status` as `WorkerStatus.BlockingKind` / `BlockingReason` (`protocol.go:~183,191`, raw JSON tolerant of unknown kinds). Display-only (Decision 10).
- **Skew rule (api first, ADR-1470 D7):**
  - New api, old controller: the old controller ignores the integers, so an edit would silently do nothing and a custom size would be `UnknownError` (a worker pending until its token expires). Therefore the api refuses **every** size mutation (add, edit, reset, delete) unless the latest `Kind: fleet` status report advertises admin-size support (a capability flag on that report) and was received by the api within a freshness window (2× the controller's report interval, measured on the api's receipt clock). A missing, stale or non-advertising report makes the editor read-only and says why (example 6). Refusal never touches existing workloads; once a fresh capable report arrives, edits are accepted again (tested).
  - Controller downgrade after a customization: the older controller ignores the integers and renders compiled-in values, so customized sizes roll back to defaults and custom-size workers stay pending; documented as unsupported beside the api downgrade.
  - New controller, old api: no integers on `DesiredWorker`; the controller uses compiled-in values, byte-identical to today.
  - The compiled-in fallback applies **only to a `DesiredWorker` carrying no integers**, never per field or when integers are present, so an edited `l` cannot flip back and forth between edited and compiled-in specs.
  - Downgrading the api after an edit is unsupported (workers roll back to compiled-in defaults); documented.

### Controller resolution (`controller/internal/preset`, `controller/internal/kube`)

- `preset.Resolver.Resolve(template, size)` takes the integers from the `DesiredWorker` when present; compiled-in values stay as the builtin defaults and the old-api fallback.
- Render-time refusal: a size above the controller's own env ceilings (`UZI_WORKER_MAX_PVC_STORAGE`, `UZI_WORKER_DOCKER_MAX_PVC_STORAGE`, plus cpu/memory max added to the controller env from the same chart values) is not rendered and is reported via `BlockingKind`.
- `kube.ValidatePVCCeilings` (startup refusal, `controller/cmd/controller/main.go:~201`) and `warnSharedCPU` (`controller/cmd/controller/cpu_slots.go`, iterates `preset.SizeNames()`) keep covering compiled-in builtins only; runtime sizes are covered by the render-time check. Consequence, kept deliberately and tested: an operator who lowers a chart ceiling below a **shipped** builtin's compiled-in value still gets the controller refusing to start, even if the admin customized that builtin to fit; the startup message says to keep ceilings at or above the shipped defaults. (Making the startup check read api values would need the api at controller boot; rejected.)
- Rolls: a quantity change changes the rendered pod template and its spec-hash (`render.go` `specHash`, resources included), which the paced drift arm already keys on (`materializer.go` drift arm), so no new roll trigger. Disk is not in the template: ADR-3890 grows claims in place with no roll (`pvc_expand.go`).

### Consumers of the size vocabulary and quantities

Read the dynamic list (name + integers) everywhere sizes are listed or described:

- web: `web/src/lib/workerSizes.ts` (`WORKER_SIZES`, quantities), `web/src/components/HostedWorkers.tsx` (provision form, badges), `web/src/lib/apiTypes.ts`, mock data (`web/src/mocks/data/overviewNext.ts`, others found by grep).
- TUI/CLI: `api/cmd/uzi/tui_workers.go` and its contract tests (`tui_workers_contract_test.go`, `tui_worker_highmem_test.go`; an unknown size renders `?`, acceptable for an old CLI against a new api), `api/cmd/uzi/demo.go`.
- api: `api/internal/apitypes/worker.go`, `handler/hosted_workers.go`, `hostedsvc/ephemeral.go`.
- Goldens replaced by a contract over the builtin seed values and the `DesiredWorker` wire shape, not deleted without a replacement: `api/internal/hostedsvc/testdata/hosted_sizes.json` + `size_contract_test.go`; `controller/internal/preset/testdata/hosted_size_specs.json` (read by `size_display_golden_test.go` and `web/src/lib/workerSizes.test.ts`); `preset_contract_test.go`. `TestPresetPVCSizesFitTheChartsLimitRangeMax`, `TestShippedPVCDefaultsFitEachTiersStorageQuota` and `TestShippedEphemeralDefaultsFitAWholeFleetOnRealNodes` stay builtins-only guards.
- Chart: comments in `values.yaml` and namespace templates that pin builtin quantities (e.g. "l-highmem = 4 CPU / 32Gi") are reworded as defaults.
- Not consumers: `capability.ResolveEphemeralSpec` (does not resolve size, `capability.go:~546`), `api/openapi/v1.yaml`. `e2e/` had no size literal on 2026-11-12; the contract sweep re-checks it.

### Admin → Workers sizes editor (`web`)

A table on the Workers tab (#4212): one row per size with the integer fields shown in human units (cores, GiB), inline edit, add, remove (admin-added only), reset per builtin; save errors map to fields; the quota advisory; a refused size shows its reason and links to Admin health; read-only with an explanation while the controller lacks support.

## Testing decisions

- api: store seed / refresh-pristine / never-overwrite-edited / reset (follow `agenttmpl` builtin reconcile tests); validation table (bounds, request ≤ limit, each ceiling rule, disk lowering accepted with existing claims unchanged, delete-in-use, builtin delete refused, name pattern); delete/provision concurrency (live-DB test); the read-only gate before the fleet capability arrives; no-kube-dependency guard green.
- Wire: `protocol_contract_test.go` against `controller_poll_wire.json` with fixtures for a `DesiredWorker` with and without integers and an old-controller status report.
- controller: resolve from integers; no-integers fallback; quantity format (BinarySI memory/storage, DecimalSI millicores); over-ceiling refusal reported, not rendered; PVC admission failure reported; a changed size changes the spec-hash and triggers the paced roll (existing `controller/internal/kube` roll tests).
- **M1 byte-identity instrument**: for every builtin size × tier × persistent/run-bound, the spec-hash computed from api-supplied integers equals the spec-hash from compiled-in presets, against a golden captured before the change; existing `render_*_test.go` stay green unmodified.
- web/TUI: an edited size's new quantities display; an admin-added size renders and can be chosen; editor Vitest.
- Chart: `scripts/assert-ephemeral-lease-render.sh` and `deploy/chart` `worker-invariants.yaml` extended for the ceiling env and the builtin-only default-size `fail`.
- New checks extend existing scripts or existing Go/Vitest/Taskfile tests; **no new CI job, no `.github/workflows/**` change**.
- Live-only behaviour (a real roll after an edit, PVC growth on a real StorageClass, a real quota rejection surfacing): a linked `acceptance` issue.

## Milestones

### M1: Builtin sizes come from the api's table, behaviour unchanged
Blocked by: none.
- Migration, Go seed/refresh, `workersize` over the store, integers on `DesiredWorker`, controller resolves from them with the no-integers fallback and fixed quantity formats, the fleet capability flag on the status report, goldens replaced by the new contract, ADR (reversal of Decision 7 for sizes; integers; ceilings via chart env; skew rule; templates stay name-only).
- Acceptance: the byte-identity instrument above passes; an old-api / new-controller pair and a new-api / old-controller pair both keep provisioning (wire fixtures).

### M2: Admins edit sizes; displays and workers follow
Blocked by: M1, #4212.
- Admin upsert/reset endpoints, ceilings in api env + validation incl. the disk-lowering warning, quota advisory, read-only gate on the fleet capability, the Workers-tab editor, web/TUI display reading quantities from the api list, render-time refusal + PVC admission failure reporting into Admin health, docs (`docs/hosted-workers.md`, `docs/admin-settings.md`, `docs/configuration.md`; `task docs:sync`).
- Acceptance: examples 1, 3, 5, 6.

### M3: Admins add and remove sizes
Blocked by: M2.
- Add/delete endpoints with the locking rule and in-use / builtin refusal, the user-readable size list driving the provision form, TUI and #4212's ephemeral default size, chart `fail` list restricted to builtins with its assert, docs.
- Acceptance: examples 2, 4.

## Landing obligations

- One PRD, one run, one PR; each milestone verifiable on its own within it.
- Full `test:api`, `test:controller`, `test:web`, `gate:repo`: pending exact-head PR CI.
- Migration renumbered at landing (`task migration:renumber`).
- No `.github/workflows/**` change.
- Linked `acceptance` issue (user-owned): live roll after an edit, PVC growth, a quota rejection surfacing in Admin health.

## Decision Log

- **Integers, not quantity strings, on the api side** (2026-11-12): dissolves PRD #58's "the api cannot validate a quantity" fork without touching the no-kube-dependency guard. Rejected: apimachinery in the api (removes a tested Decision 1 guardrail); a hand-rolled quantity parser (PRD #58: subtle wrongness worse than none); controller-only validation (a bad value accepted with no feedback).
- **Integers inline on each `DesiredWorker`, not a separate size table on the poll** (architect review): the poll is full desired state already; inline removes a digest, a "size missing from table" refusal and a table-skew case.
- **Ceilings reach the api as chart-rendered env, not a controller report**: the poll is a GET with no body by design (`protocol.go`); both components read the same chart values. Rejected: adding a controller-to-api ceiling report.
- **Refusals ride the existing status report's `BlockingKind`**: an existing display-only channel tolerant of unknown kinds.
- **Api-first skew with a read-only gate on a fleet capability**: an old controller would silently ignore an edit or strand a custom-size worker. Fallback only for integer-free `DesiredWorker`s, so an edited size never oscillates.
- **Builtins cannot be deleted** (edit and reset only): ADR-2240 preselects `l` and the ephemeral default is `m`; keeping builtins avoids defining a fallback for a deleted default.
- **Env `UZI_EPHEMERAL_DEFAULT_SIZE` may name builtins only**: it is validated at boot before the store exists; admin-set values may name any size.
- **ResourceQuota is an advisory, not a refusal**: quota is fleet-wide and depends on how many workers exist; the real rejection surfaces via ReplicaFailure / roll `stuck`.
- **Desired disk may go down; claims never shrink** (buddy review): ADR-3890 forbids shrinking existing claims, not lowering a desired size, so lowering affects only new claims. This also makes reset and pristine refresh well-defined after an enlargement. Rejected: grow-only desired values (reset to a shipped value below the current one would be impossible).
- **One advisory lock for size-table mutations, `FOR SHARE` on every size reference** (buddy review): serializes the 16-size maximum and makes delete safe against provisions and default-size writes at READ COMMITTED.
- **Mutation gate on a fresh fleet capability report, receipt-clocked**: the controller's clock is not trusted for freshness.
- **Builtins seeded and refreshed like builtin agent templates**: known pattern for "shipped default, admin may customise, reset returns to shipped".
- **Three milestones in one PR**: M1 is the risky reversal with zero visible change, proven by the spec-hash instrument; M2/M3 add the admin surface. M1 alone has no user value, so it stays a milestone (scope gate).
