# PRD #76: Hosted worker row — AWS-style derived name + docker badge

**GitLab Issue**: [#76](https://github.com/vtmocanu/uzi/-/issues/76)
**Status**: Draft (created 2026-07-20)
**Priority**: Low
**Related**:
- PRD #58 (hosted k8s workers — this styles that feature's `WorkersSettings` row; the derived name + `hosted_size` badge it introduced are what Part 1 reworks).
- PRD #83 M3 (docker-capable hosted workers — the `docker_enabled` column + rootless-DinD sidecar this PRD's Part 2 finally surfaces on the row).
- PRD #64 (uzi CLI — the second API consumer that makes a `docker` DTO field, not a name substring, the right home for the capability).
- Issue #84 (capability vocabulary + persistence — owns declared-vs-reported docker drift, which stays out of scope here).

## Problem

The hosted worker card title reads `base (L)` while a `size L` badge sits on the
same row: the size shows **twice**, and the parenthetical reads like a footnote.
The name is derived server-side by `derivedHostedWorkerName`
(`api/internal/handler/hosted_workers.go:159-161`):

```go
return fmt.Sprintf("%s (%s)", template, strings.ToUpper(size))
```

Separately, since PRD #83 M3 a hosted worker can be **docker-capable** (a
rootless-DinD sidecar, `docker_enabled` column, migration `00070`). That is a
real, cost- and cluster-tier-bearing property, but it is **invisible on the row**:
`WorkerDTO` carries no docker field (`api/internal/apitypes/worker.go`) and there
is no docker badge (`web/src/pages/WorkersSettings.tsx`). A docker `base L` and a
plain `base L` are indistinguishable in the list. This PRD fixes both.

## Solution Overview

Two orthogonal changes to the hosted-worker row, split by how each datum should
live:

1. **Fold size into the name, drop the size badge (Part 1).** Adopt the AWS
   instance-type convention (`t3.large`): the derived name becomes
   **`base.l-a32f`** — template `.` t-shirt letter, plus a short random suffix —
   and the separate `size L` badge is removed so the size appears in exactly one
   place. The random suffix self-disambiguates two same-size workers
   (`base.l-a32f` / `base.l-9c71`) now that the badge no longer sits beside the
   name.
2. **Surface docker as its own badge, NOT in the name (Part 2).** Add a
   first-class `Docker *bool` DTO field, read from the existing `docker_enabled`
   column, and render a literal `docker` text badge on the row alongside the
   existing `hosted` / `template drift` badges when the worker is docker-capable.

Nothing else changes: the memory bar, the provision picker's size spell-out, cost
accounting, and the worker's stored lowercase size value are all untouched.

## Design Decisions

1. **Size is folded into the name; docker is not — a name is identity, docker is
   structured state.** Encoding docker into the display string is the "metadata in
   the filename" anti-pattern: unparseable as a contract, and it does not scale
   (a second orthogonal capability — GPU, arch — would turn the name into a
   mini-DSL `base.l.docker.gpu-a32f`). Size is different: it is a *provision-time
   fixed attribute of the name's own subject*, one dimension, so folding it in is
   the AWS convention, not state-in-a-string. Docker gets a queryable DTO field
   instead.
2. **Keep the t-shirt letter (`l`), not the AWS word (`large`).** The stored/wire
   size value is already the lowercase letter (`s`/`m`/`l`); the name mirrors it.
   The provision picker and memory bar still spell out the quantities, so the
   letter in the name is a compact identifier, not the only place the size is
   legible.
3. **Random suffix, not a counter.** `-a32f` is a short 4-char **base36**
   (`[0-9a-z]`) **random** id. Random means no new state, no reused or gap numbers
   after deletes, and no read-modify-write race at provision. The name is cosmetic
   and **not** unique-constrained (`hosted_workers.go:153-161`: the controller
   never reads it — cluster object names derive from the uuid), so a ~1/1.68M
   (36⁴) collision per same-prefix pair is harmless (two identical names are still
   two distinct uuid-keyed workers). Generate it from `crypto/rand` to avoid a
   seeded global `math/rand` dependency; it is a display token, not a secret.
4. **`Docker *bool`, mirroring `HostedSize *string`.** `null` for an external
   worker (docker not applicable) **and** for any hosted worker provisioned before
   migration `00070` (whose `docker_enabled` is NULL — rendering is identical to
   `false`: no badge); `false` for a post-`00070` hosted worker without the
   sidecar; `true` for a docker-capable one. Populated in **both** DTO builders
   (`workerDTOFromWorker`, `workerDTOFromRow`) from `w.DockerEnabled` (`pgtype.Bool`,
   guard `.Valid`). **No new query** — `ListWorkersByUser`, the three
   `GetWorkerBy{ID,IDForUser,TokenHash}` queries, the admin `ListAllWorkersWithOwner`,
   and the provision `RETURNING` all already select `docker_enabled` (verified:
   `store.Worker.DockerEnabled` at `models.go:436`, `store.ListWorkersByUserRow.DockerEnabled`).
   Every `WorkerDTO` construction site in `handler/` routes through these two
   builders, so populating both covers the whole API surface.
5. **Docker badge is literal text `docker`; absence renders nothing.** The row
   already uses text badges for attributes (`hosted`, `template drift`), so
   docker-as-text-badge follows the row's own idiom. A false/null worker gets no
   badge (absence needs no badge).
6. **The size badge removal is also an a11y win.** A bare letter (`L`) inside a
   `Badge` (generic-role `span`) is unnameable for a screen reader
   (`WorkersSettings.tsx:324-339`); the word `docker` is real text that reaches
   sighted, keyboard, and screen-reader users with no ARIA. So Part 1 removes an
   unnameable badge and Part 2 adds only a nameable one.
7. **The derived name freezes at provision and is never re-derived.** Part 1
   affects **new** provisions only; existing workers keep their `base (L)` names.
   This is exactly why docker must be a field, not name-encoded: a name that
   encoded a cost/security-relevant capability could silently go stale, whereas a
   field read from `docker_enabled` on every list cannot. It also means a
   worker provisioned with a custom `req.Name` (no derived name at all) still
   advertises docker uniformly.
8. **CLI/API parity (PRD #64).** The `uzi` CLI reads the same DTO; a `docker`
   field is consumable and filterable, a name substring is not.

## Touchpoints

**api/**
- `api/internal/handler/hosted_workers.go:159-161` — rewrite
  `derivedHostedWorkerName(template, size)` →
  `fmt.Sprintf("%s.%s-%s", template, strings.ToLower(size), shortRandom())`, with
  a `shortRandom()` helper (~4 chars, `crypto/rand`, base36/hex). Update the
  doc-comment above it (it currently describes the `%s (%s)` upper-cased form).
- `api/internal/apitypes/worker.go` — add `Docker *bool \`json:"docker"\`` next to
  `HostedSize *string` (line 19), with a comment mirroring `HostedSize`'s
  null/false/true semantics.
- `api/internal/handler/workers.go:100,122` — populate `Docker` in both
  `workerDTOFromWorker` (from `store.Worker.DockerEnabled`) and `workerDTOFromRow`
  (from `store.ListWorkersByUserRow.DockerEnabled`) via a `.Valid`-guarded
  `*bool` helper (mirror the existing `textPtrValue` used for `HostedSize`).

**web/**
- `web/src/lib/api.ts:595-596` — add `docker?: boolean` to the `Worker` type
  (next to `kind` / `hosted_size`).
- `web/src/pages/WorkersSettings.tsx:340` — **remove** the
  `{w.hosted_size && <Badge>size {sizeLabel(w.hosted_size)}</Badge>}` line. Drop
  the now-unused `sizeLabel` import if nothing else in the file uses it
  (`sizeLabel` stays exported for the provision picker's `workerSizes.ts:142`).
- `web/src/pages/WorkersSettings.tsx:319-342` — inside the `w.kind === "hosted"`
  block, render `<Badge>docker</Badge>` when `w.docker === true`; render nothing
  when false/null. Also refresh two now-stale `base (S)`/`base (M)` derived-name
  examples in comments (`:51` and the delete-confirm screen-reader label `:401`)
  to the new shape.
- `web/src/mocks/data.ts` + `web/src/mocks/mockApi.ts` — the demo hosted
  worker(s) gain a `docker` field so the mock/dev UI shows a populated docker
  badge. **`mockApi.ts:1339` replicates the server's name derivation**
  (`` name?.trim() || `${template} (${size.toUpperCase()})` ``); update it to the
  new `base.l-xxxx` shape so newly provisioned mock workers match the API, not
  just cosmetically.

**docs/**
- If any `docs/*.md` page (audience: user) documents the workers-settings row, add
  a one-line note on the new name shape + docker badge. No new doc page.

## Milestones

Dependency shape: **M1 (api) freezes the wire** — the `docker` JSON field name
and the new derived-name format; **M2 (web)** consumes the `docker` field. M1 and
M2 touch **disjoint packages** (`api/` vs `web/`) and, once the `docker` field
name is frozen by M1's DTO, can proceed in parallel (the derived-name change is
server-output-only — web never parses the name). M3 folds the test updates the two
UI changes force.

- [ ] **M1 — API: derived name + docker DTO field.** Rewrite
  `derivedHostedWorkerName` to `base.l-a32f` (+ `shortRandom` helper); add
  `Docker *bool` to `WorkerDTO` and populate it in both DTO builders from
  `docker_enabled`. Freezes the contract: `WorkerDTO.docker: bool|null` + name
  format. Rewrite the existing name-format assertions in
  `api/internal/handler/hosted_workers_test.go:150-156` (they pin the old
  `base (M)` / `jvm (L)` shape) and add a case for the new
  `template.size-<4×[0-9a-z]>` format. `cd api && go build ./... && go test ./internal/handler/...`.
- [ ] **M2 — Web: drop size badge + docker badge.** Remove the `size` badge; add
  `docker?: boolean` to the `Worker` type and render a `docker` text badge when
  `w.docker === true`; update the mock. `cd web && npm run typecheck`.
- [ ] **M3 — Tests updated + green.** api: name-format unit test (from M1). web:
  update `WorkersSettings.test.tsx` (the `size M` badge assertions at lines
  123/135/136 must flip to "no size badge"), add docker-badge shown/hidden cases
  keyed on `docker` true/false/absent. Full suites green
  (`cd api && go test ./...`; `cd web && npm test`).
- [ ] **M4 — Mock/docs + build.** Demo data shows a populated docker badge and the
  new name shape; any user-facing workers doc gains a one-line note.
  `cd web && npm run build` (check-docs + tsc) green.

## Out of Scope

- **Reported-capability drift.** The badge reflects the **declared**
  `docker_enabled` set at provision. The worker's self-reported
  `capabilities:["docker"]` is accept-and-ignore in M1
  (`worker_protocol.go:84-92`; no column, no storage) — issue #84 owns the
  capability vocabulary + persistence. A declared-vs-reported docker drift badge
  (the pattern `template drift` already uses) is a follow-up gated on #84.
- **Retroactive renaming.** Existing hosted workers keep their `base (L)` names;
  only new provisions get `base.l-xxxx`. No migration, no backfill.
- **Encoding any second capability (GPU, arch) into the name** — deliberately
  rejected (Decision 1); those would be their own DTO fields + badges.
- **Making the derived name unique-constrained** — it is cosmetic and uuid-backed;
  no constraint is added.

## Validation

- **Unit (api)**: `derivedHostedWorkerName` returns `template.size-<4×[0-9a-z]>`;
  the `*bool` DTO helper yields `null` for an external worker, `false`/`true` for
  hosted from `docker_enabled.Valid`/`.Bool`.
- **Unit (web)**: `WorkersSettings` renders **no** `size` badge for a hosted
  worker; renders a `docker` badge iff `w.docker === true`; renders neither for an
  external worker.
- **Visual**: the mock API's hosted worker shows `base.l-xxxx` with a `docker`
  badge (docker-capable) and without (plain), no size badge on either.
- **Regression**: a pre-feature worker DTO (no `docker` key) still renders the row
  with no docker badge and no thrown error; a worker with the old `base (M)` name
  still displays (names are not re-derived).
