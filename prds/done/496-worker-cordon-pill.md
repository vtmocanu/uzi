# PRD #496 — Surface worker cordon/drain state ("won't claim new runs") in the Workers list + CLI

**Issue**: [#496](https://github.com/vtmocanu/uzi/issues/496)
**Priority**: Medium
**Status**: Complete — shipped 2026-08-22 (all milestones M1–M5 landed)

> **Path convention**: every path in this PRD is relative to the repo root. The change spans **api** (`api/internal/apitypes`, `api/internal/handler`, `api/cmd/uzi`), **web** (`web/src/...`), and **docs** — but there is **no** DB migration, **no** new SQL query, and **no** controller change. Load `.claude/rules/go.md` before the api/CLI work and `.claude/rules/web.md` before the web work.
>
> **This PRD is destined for an offline uzi sweep worker.** Every fact below was verified against the codebase on 2026-08-21 (twice — a second adversarial review pass corrected several first-draft claims; see the Decision log), and is baked in as a resolved fact with its `file:line`, so the work needs **no** internet access, no external docs, and no live cluster. It also touches **no** file under `.github/workflows/**` in either implementation or validation (`.claude/rules/prds.md`).

## Problem

A **cordoned** (draining) worker finishes its in-flight runs but **claims nothing new**. This is a real, enforced backend state — the claim gate returns idle for such a worker (`api/internal/workersvc/service.go:1076-1078`) — but **the Workers list and `uzi worker list` give it no signal at all**. The read DTO simply does not carry the field.

The result is an invisible failure mode: a worker that is `online`, shows a **free run slot**, and yet never picks up a queued run looks like a bug. This was hit live during the **v0.50.0** roll — queued run **#365** sat unclaimed while a hosted worker reported `active_runs 1 / max 2` (a genuinely free slot). The controller had **cordoned** that worker for the roll and was deferring its pod restart until it went idle; the worker kept its old version (so it also read `outdated`) and kept finishing its one run. Nothing on screen explained why the queue was not moving. "Why won't my free-slot worker pick up my run?" had no answer in the UI.

## Solution

The state already exists first-class as the nullable column **`workers.draining_since`** (PRD #422; `draining == draining_since IS NOT NULL`). The **only** gap is that the row→DTO mappers never copy it into `WorkerDTO`, even though the queries **already select it**. So:

1. **api** — add `draining_since` to `WorkerDTO` and map it in **both** row→DTO mappers (`workerDTOFromRow` and `workerDTOFromWorker`; there are two, see Background). One field, plus a mapping line in each — no query, no migration.
2. **web** — render a **"draining / cordoned" pill** in the Workers list, keyed on the real field, so "won't claim new work" is legible. Design is already prototyped and reviewed with the user (dashed-border neutral pill; see Decision log).
3. **CLI** — annotate `uzi worker list`'s STATUS so a cordoned worker reads as more than its bare status (the CLI is a first-class second consumer; `api/cmd/uzi/worker.go:81` already records that parity obligation).
4. **docs** — a user-facing sentence + CHANGELOG entry.

---

## Background — current state (resolved facts)

### The state is first-class and enforced (not derived)

- **Column**: `workers.draining_since` (nullable `timestamptz`). Row model `store.ListWorkersByUserRow.DrainingSince pgtype.Timestamptz` at `api/internal/store/runtime.sql.go:3250`; also on the bare `store.Worker.DrainingSince` (`api/internal/store/models.go:633`). Canonical semantics (quoted from the query file): *"draining == draining_since != nil"*.
- **The queries already SELECT it**: `listWorkersByUser` at `api/internal/store/runtime.sql.go:3188` selects `w.draining_since`; the admin/single-worker paths scan `store.Worker.DrainingSince` too (`runtime.sql.go:2541`). **No query or schema change is needed to expose it.**
- **The claim gate enforces it** — `api/internal/workersvc/service.go:1076-1078`:
  ```go
  if wkr.DrainingSince.Valid {
      return nil, nil // idle: worker draining/cordoned
  }
  ```
  Comment (`:1071-1075`): *"a cordoned worker finishes its in-flight runs but claims nothing new … only NEW claims are refused."* Pinned by `api/internal/workersvc/drain_gate_test.go`.
- **The api already uses this exact predicate itself** to compute free-slot availability: `runtime.sql.go:541-543` (`-- A draining worker has no free slot for NEW work … AND w.draining_since IS NULL`), and the fleet-placement query filters on it too (`runtime.sql.go:248`).

### Who sets it, and the hosted-only scope

- **The controller cordons a busy, drifted hosted worker** instead of hard-killing it during a roll: `controller/internal/kube/materializer.go:613-621` (`if w.Busy && !rollDespiteBusy { … RequestDrain … }`), via `POST /api/controller/workers/{id}/drain` → `ControllerCordonWorker` (`api/internal/handler/controller_cordon.go:34`) → `CordonHostedWorker` (`api/internal/store/hosted_workers.sql.go:15-19`, `SET draining_since = COALESCE(draining_since, now())`, gated `WHERE id = $1 AND kind = 'hosted'`). It is **uncordoned** on roll (`RegisterWorker` clears it), on force/deadline roll, or on reverted drift (`UncordonHostedWorker`).
- **Every non-null write to `draining_since` is `WHERE … kind = 'hosted'`.** `UncordonHostedWorker` and `RegisterWorker` only ever set it to `NULL`, so `draining_since` **can never be non-null for an external worker via any code path** (verified by the reviewer). An external worker (one the owner runs by hand) is never cordoned; it stops claiming only when its own process stops polling, and then it simply goes `offline`. **Consequence for the pill: `draining_since` is non-null only for hosted workers, so the pill self-limits to them — no `kind` check is required, and none should be added.**

### A cordoned worker is USUALLY `online`, but `offline + draining` is a real state

`status ∈ {"online","offline"}`, heartbeat-driven. A draining worker normally **keeps heartbeating and stays `online`** — deliberate and doubly documented: `MarkStaleWorkersOffline` does **not** filter draining (`runtime.sql.go:3390-3392`), and `CountOnlineWorkersForUser` counts a draining worker as online (`runtime.sql.go:527-529`). **So `status="online"` does not imply "will claim".**

**But a cordoned worker whose pod actually dies before it re-registers is swept to `offline` WITHOUT clearing `draining_since`** — the same `MarkStaleWorkersOffline` comment says so verbatim (*"a draining worker whose pod actually dies is still swept offline like any other"*). So `offline + draining_since != null` is reachable, and every consumer of the field must **compose with the raw status, never assume `online`** (see Decisions 3 and 5).

### `upgrade_status: "outdated"` does NOT mean "won't claim"

`upgrade_status` is a read-time, display-only classification computed from version comparison (`ClassifyUpgradeWithTarget`, called at `api/internal/handler/workers.go:219`). It is **orthogonal** to `draining_since`; an `outdated` worker keeps claiming normally until it is actually cordoned. The #365 worker read `outdated` **and** was cordoned at the same time (cordoned for the roll, still on its old version) — two independent facts. **The pill must key on `draining_since`, not on `upgrade_status` or `busy`.** (This corrects the original mock's "derived from outdated + pending roll" framing.)

### The worker DTO and its TWO mappers (the whole api change)

- **Struct**: `apitypes.WorkerDTO` at `api/internal/apitypes/worker.go:6-128`. Timestamp fields use `*time.Time` with a `json` tag, e.g. `OnlineSince *time.Time \`json:"online_since"\`` (`:43`), `LastHeartbeatAt` (`:40`). **There is no draining/cordoned field today.** `AdminWorkerDTO` **embeds** `WorkerDTO` (`:132-135`), so it inherits the new JSON field automatically.
- **There are TWO row→DTO mappers, and both need the field** (this is the correction to a first-draft "one mapping line" claim):
  1. `workerDTOFromRow(w store.ListWorkersByUserRow, …)` — `api/internal/handler/workers.go:218-262` (the **user list** path). It already maps the sibling timestamp: `OnlineSince: timePtr(w.OnlineSince.Valid, w.OnlineSince.Time)` (`:255`).
  2. `workerDTOFromWorker(w store.Worker, …)` — `api/internal/handler/workers.go:189`. **Not admin-only**: it is the shared single-worker mapper for the **admin list** (`runs.go:122` via `AdminListWorkers`), **register/heartbeat** (`worker_protocol.go:302,347`), **hosted provision** (`hosted_workers.go:167`), and **set-token/rebind** (`workers.go:625,798`). A cordoned worker returns `draining_since: null` on **every** one of those paths — including the admin Agents-status view Success Criterion 1 requires — unless this mapper is updated too.
- `git grep -n 'apitypes.WorkerDTO{'` surfaces both mappers; the only other hits are zero-value error returns (`uzicli/client.go:837`, `uzicli/fake.go:398`), correctly ignored. **M1 must map AND test both mappers**, not just the list path.
- **Desirable side effect (state it so it is not read as a leak):** mapping `workerDTOFromWorker` means `draining_since` now also appears on the register/heartbeat/provision/set-token responses. That is correct — a bare `store.Worker` carries the column — and harmless.

### The CLI worker list (the CLI change)

- Command + table: `api/cmd/uzi/worker.go:17` (`newWorkerCmd`), rows rendered at `:88` with columns `["ID","NAME","STATUS","UPTIME","VERSION","UPGRADE","TOKEN"]`. `STATUS` today is the **raw** `w.Status` string (`:74`); there is **no** `statusCell` helper yet (contrast `uptimeCell` `:208` — which itself guards on `w.Status != "online"` at `:209` — `upgradeCell` `:253`, `bindModeCell` `:293`).
- The `--json` path serializes `apitypes.WorkerDTO` directly (`:37-39`), so **once M1 adds the field it appears in `uzi worker list --json` for free**; only the human table needs an explicit change.
- Tests live in `api/cmd/uzi/worker_test.go` (e.g. `TestWorkerListShowsUptime` `:276`, using `uzicli.FakeClient{Workers: []apitypes.WorkerDTO{…}}`) — the pattern to copy.

### The web surface

- **`Worker` type**: `web/src/lib/api.ts`, `interface Worker` at `:1019-1129`; timestamp fields are `string | null` (`last_heartbeat_at` `:1093`; `online_since?` optional at `:1095`). Add `draining_since: string | null`.
  - **Incidental fix in the same file**: `:1034` comments `busy: boolean; // derived: … (== active_runs > 0)`, which **contradicts the Go DTO** (`apitypes/worker.go:24-27`: busy is any-kind incl. **chat**, so a chat-only worker is `busy` with `active_runs: 0`). Correct that comment while adding the field, so the label logic below is not built on a false premise.
- **Badge system**: `web/src/components/ui.tsx` — `Badge` (`:267`), `BadgeTone` (`:250-251`). **`Badge` exposes no `className` prop** (`:267-289`), so a dashed border cannot be injected into it — a dedicated component is **mandatory**, not merely preferred.
- **The one structurally-mirrorable precedent is `WorkerUpgradeBadge.tsx:57`** — it builds its **own** `<span>` with an inline class string (imports no `Badge`). `WorkerRunBadge.tsx:17` delegates to `<Badge>` and therefore **cannot** carry a dashed border, so it is NOT a usable template here. Mirror `WorkerUpgradeBadge`'s span, not `WorkerRunBadge`.
- **Render site**: `web/src/pages/WorkersSettings.tsx` — the right-hand badge cluster opens at `:532`; hosted `Badge` `:540`, docker `:553`, template-drift `:556-563`, status `Badge` `:564-566`, `WorkerRunBadge` `:567`. Insert the cordon pill **between the status badge (`:566`) and `WorkerRunBadge` (`:567`)** so the row reads `online · draining · N/M runs` — adjacent to the status it contradicts.
- **Theme tokens**: two dark themes, `ember` (default) and `mission` (`[data-theme="mission"]`). `--edge`/`--edge-strong` (`index.css:27-28` and `:108-109`) and the neutral triple (`:64-66` and `:135-137`) exist in both and are exposed as Tailwind tokens `edge`/`edge-strong`/`neutral.{fg,border,surface}` (`tailwind.config.js:16-40`). **Use those token classes + `border-dashed`, NOT a fixed `slate-*`** — `slate-*` is a real (un-purged) Tailwind default, so it is not inert, but it ignores the ember/mission retint. (History: `WorkerUpgradeBadge.tsx:8-16` shipped inert `accent/hair/base` classes; name real tokens.)
- **Mock fixtures**: `web/src/mocks/data.ts` — `mockWorkers` at `:1751-1926` holds exactly **2 hosted** workers today (`w-stuck` `:1821`, `w-hosted-eu` `:1897`); `mockAdminWorkers` (`:1929-1932`) hand-spreads `mockWorkers[0..3]`. See M2 for the test that a new hosted fixture breaks.

---

## Design decisions

1. **api exposes the raw timestamp `draining_since: *time.Time` (JSON `draining_since`), not a derived bool.** Mirrors `online_since`/`last_heartbeat_at` exactly, and the timestamp is strictly more informative than a bool — the web tooltip can say "cordoned 6m ago". Web derives `draining = draining_since != null`. Map with the existing helper: `DrainingSince: timePtr(w.DrainingSince.Valid, w.DrainingSince.Time)` in **both** mappers (Background).

2. **The pill keys on `draining_since`, never on `upgrade_status`, `busy`, or `kind`.** `draining == draining_since != null`. Because the column is hosted-only, the pill self-limits to hosted workers with no `kind` check. Never infer draining from `outdated`/`busy`/`active_runs` (Background: they are orthogonal).

3. **One dashed-border neutral treatment, two labels split on `active_runs > 0`.** A new `WorkerCordonBadge` renders a **dashed-border** pill in the **neutral/edge** tokens (its own `<span>`, mirroring `WorkerUpgradeBadge`; classes `border border-dashed border-edge-strong` + the neutral text/surface tokens + `Badge`'s geometry `inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium`). Deliberately **not** a solid `BadgeTone`, so it cannot be confused with the amber `outdated` badge or the amber `N/M runs` badge a draining-but-busy worker shows in the same row.
   - **`draining`** when `active_runs > 0` — finishing current **runs**.
   - **`cordoned`** when `active_runs === 0` — held, no runs in flight.
   **Key the split on `active_runs`, NOT `busy`** (the reviewer's verdict): the tooltip talks about "current runs" and the neighbouring run badge keys on `active_runs`, and a **chat-only** cordoned worker is `busy: true` with `active_runs: 0` — keying on `busy` would label it `draining` / "finishing its current runs" while it holds **zero** runs. Same treatment and tooltip family for both labels; the word carries the difference.

4. **Tooltip states the FACT, not a guessed cause, and matches the label.** `draining_since` is set on **drift** (a version roll is common, but a template/config change also drifts), so the tooltip must **not** claim "for a version roll" — the api cannot guarantee the cause.
   - `active_runs > 0`: **"Cordoned — finishing its current runs, not claiming new ones."**
   - `active_runs === 0`: **"Cordoned — not claiming new runs."**
   Optionally prefix a client-derived duration from `now − draining_since` ("Cordoned 6m ago — …"), following the `online_since`/`uptimeCell` precedent. (Corrects the mock, which over-attributed the cause and wrongly called the state "derived".)

5. **CLI: annotate the existing STATUS column by composing from the RAW status, no new column.** Add a `statusCell(w apitypes.WorkerDTO) string` helper that appends `(draining)` / `(cordoned)` (per Decision 3's `active_runs` split) to `w.Status` when `draining_since` is set — so an offline-but-still-cordoned worker reads **`offline (draining)`**, never a hardcoded `online`. **Do not hardcode `online`** — `offline + draining` is a reachable state (Background). Use it for the STATUS column at `worker.go:88`; `--json` carries the raw field for scripting. (Alternative — a dedicated `DRAINING` column — rejected as more table width for a rarely-set flag.)

6. **Draining is a normal operational state, not an alert.** It must **not** enter `needsAttention` or the Fleet upgrade panel counts (`WorkerUpgradeBadge.tsx`), and the pill is neutral (dashed edge), not a warning colour. A cordon is a healthy, deliberate state; badging it as attention would train users to ignore real alerts.

7. **The reachable signal is the pill's LABEL WORD; the explanatory sentence stays a hover `title`, with no detail-strip.** This repo has been burned by hover-only explanations (`WorkerUpgradeBadge.tsx:203-214`'s `WorkerUpgradeDetail` text strip exists precisely because a bare-span `title` is unreachable by keyboard/touch/SR), so the decision is explicit: the pill renders a real **text label** ("draining"/"cordoned") that IS reachable (the docker-badge lesson at `WorkersSettings.tsx:543-552`), and that word carries the essential meaning. The fuller sentence lives in `title` only. **No detail-strip is built** — unlike `upgrade_failed` (which needs a diagnostic + kubectl affordance), a cordon needs no operator action, so the word plus the run badge is sufficient and a strip would be over-build. The pill carries **no worker-controlled string** (only static copy + an optional numeric duration), so it is not a bidi sink and needs no `stripUnsafeChars`; if the tooltip is ever extended to include the worker name, it must strip.

## Scope

**In scope**:
- **api**: `DrainingSince *time.Time \`json:"draining_since"\`` on `apitypes.WorkerDTO`; map it in **both** `workerDTOFromRow` and `workerDTOFromWorker` (`handler/workers.go`); unit coverage of **both** mappers.
- **web**: add `draining_since` to the `Worker` type and **fix the wrong `:1034` `busy` comment**; new `WorkerCordonBadge.tsx`; render it in `WorkersSettings.tsx` between the status badge and the run badge; add cordoned worker fixture(s) to `mockWorkers` **and** `mockAdminWorkers`, and update the `mockApi.hosted.test.ts` counts/quota (M2); component tests.
- **CLI**: `statusCell` draining annotation (composed from raw status) in `uzi worker list`; a `worker_test.go` case including the `offline + draining` shape.
- **docs**: a sentence on a relevant `audience: user` page (e.g. `docs/hosted-workers.md` or `docs/worker-upgrades.md`) and a CHANGELOG `[Unreleased]` entry.

**Out of scope**:
- **Any DB migration or SQL query change.** The column exists and is already selected.
- **Any controller change.** The cordon lifecycle is unchanged; this PRD only surfaces the existing state.
- **Rendering the pill on the admin Agents-status page.** The DTO field flows there via `AdminWorkerDTO` and must be **mapped** (in scope, so it is not null), but rendering the pill on that page is a later enhancement.
- **A detail-strip / manual-cordon UI / explaining *why* a worker drained** (Decision 7; roll vs config drift).
- **External-worker "won't claim" signals** beyond `offline` — external workers are never cordoned server-side.

## Milestones

Ordered by dependency: **M2/M3/M4 depend on M1's DTO field**; **M3 depends on M2's component**; **M5 is independent docs**. M1 (api) and the web type stub can start in parallel once the field shape is fixed by Decision 1.

- [x] **M1 — Expose `draining_since` on `WorkerDTO`, in BOTH mappers (api).** Add `DrainingSince *time.Time \`json:"draining_since"\`` to `apitypes.WorkerDTO` (`api/internal/apitypes/worker.go`), documented like `OnlineSince`. Map it as `DrainingSince: timePtr(w.DrainingSince.Valid, w.DrainingSince.Time)` in **both** `workerDTOFromRow` (`handler/workers.go:230-261`) **and** `workerDTOFromWorker` (`handler/workers.go:189`). Confirm the coverage with `git grep -n 'apitypes.WorkerDTO{'` (two mappers + two ignorable zero-value returns). **Validate** (`task gate:api`, plus focused unit tests): a `workerDTOFromRow` test AND a `workerDTOFromWorker` test, each asserting a source with `DrainingSince` set → DTO `draining_since` non-nil at the same instant, and null → null. Prove **each** non-vacuous by folding **that** mapper's line to `nil` and confirming only its own test reddens (`.claude/agent-team.md` mutation discipline — the fold goes at each call site, not in a shared helper; testing only one mapper leaves the admin/register/heartbeat paths silently null). No migration, no `sqlc generate`, no query edit. (Side effect to expect: `draining_since` now also appears on register/heartbeat/provision/set-token responses — correct and harmless.)

- [x] **M2 — `WorkerCordonBadge`, render, and mock fixtures (web).** Add `draining_since: string | null` to the `Worker` type (`web/src/lib/api.ts`) and **correct the `:1034` `busy` comment** (Background). Create `web/src/components/WorkerCordonBadge.tsx` mirroring `WorkerUpgradeBadge.tsx:57`'s own-`<span>` shape (NOT `WorkerRunBadge`, which delegates to `Badge` and can't carry a dashed border): renders nothing when `draining_since == null`; otherwise a **dashed-border neutral** pill (`border border-dashed border-edge-strong` + neutral text/surface tokens + Badge geometry) labelled `draining` when `active_runs > 0` else `cordoned` (Decision 3), with the `title` from Decision 4. Render it in `WorkersSettings.tsx` **between `:566` and `:567`** (Decision/Background). Add a **hosted, cordoned** worker to `mockWorkers` — appended at the END (so `mockApi.test.ts:568-569`'s `workers[0/1]` and `mockAdminWorkers`'s `[0..3]` spread do not shift) — carrying `draining_since` set with `active_runs: 1, max_concurrent_runs: 2` (the #365 shape), plus ideally a second idle-cordoned one (`active_runs: 0`, `draining_since` set) to exercise both labels; also add the cordoned worker to `mockAdminWorkers` (`data.ts:1929-1932`) if the admin path is to be demoed. **Update `web/src/mocks/mockApi.hosted.test.ts`** (`:44` `toHaveLength(2)`, `:49` `toHaveLength(3)`, `:53` `toHaveLength(2)`): the hosted count rises, and the test's quota-headroom demo (quota 3 vs seeded 2, `mockApi.ts:2709`) must be rebalanced (raise the mock quota, or make the new cordoned fixture(s) count against it deliberately) so the provision→at-quota→delete→release narrative the test asserts still holds — do not just bump literals. **Validate** under `VITE_UZI_MOCK=1`: the pill renders **per row** on the cordoned worker(s) and on no other (a real-DOM, browser-observable check); a non-draining worker shows nothing. **Do not assert the `title` copy in the browser** — a native `title` tooltip is structurally invisible to screenshot/snapshot (`.claude/rules/web.md`); the tooltip assertion belongs in M3 (jsdom). `task gate:web` must stay green (the `mockApi.hosted.test.ts` fix is part of THIS milestone, since the fixture breaks it).

- [x] **M3 — Component tests (web).** `web/src/components/WorkerCordonBadge.test.tsx`, constructing `Worker` objects inline (independent of the mock fixture): pill **absent** when `draining_since` null — **paired in the same `it` (or explicitly cross-referenced) with a positive** that it renders when set, so the negative is not vacuous (`.claude/rules/web.md`); label is `draining` when `active_runs > 0` and `cordoned` when `active_runs === 0`, **including the `busy: true, active_runs: 0` chat-only row → `cordoned`** (the case the wrong `:1034` premise would misclassify); the **`title` attribute** carries the Decision-4 copy (jsdom reads the attribute — the check the browser cannot do); an **ordering** assertion that the pill sits after the status badge in the rendered cluster. Prove each new assertion non-vacuous by a call-site mutation (`.claude/agent-team.md`). **Validate**: `task gate:web` passes (deps-check + lint + deadcode + check-docs + typecheck + test).

- [x] **M4 — `uzi worker list` STATUS annotation (CLI).** Add `statusCell(w apitypes.WorkerDTO) string` in `api/cmd/uzi/worker.go` that appends `(draining)`/`(cordoned)` (Decision 3 split) to **`w.Status`** when `draining_since` is set, else returns the raw status; use it for the STATUS column at `:88`. **Validate** (`task gate:api`; `worker_test.go` cases modelled on `TestWorkerListShowsUptime`): an `online` cordoned worker renders `online (draining)`; an **`offline` cordoned worker renders `offline (draining)`** (the hardcode-`online` bug guard); a non-cordoned worker renders the plain status; `--json` carries `draining_since` (free from M1). Non-vacuous via a call-site fold.

- [x] **M5 — Docs + CHANGELOG.** Add a sentence to a relevant `audience: user` page (`docs/hosted-workers.md` and/or `docs/worker-upgrades.md`) explaining the pill: a cordoned/draining worker finishes its current runs but claims nothing new, which is why an online worker with a free slot may not pick up a queued run (name the #365-style symptom in user terms). Add a CHANGELOG `[Unreleased]` entry ("Workers list and `uzi worker list` now show when a worker is draining/cordoned — finishing current runs but not claiming new ones"). **Validate**: `web/scripts/check-docs.mjs` (run in `npm run build`) passes; no duplicate `order`, no broken links.

## Success criteria

1. `GET /api/workers` **and** the admin Agents-status list **and** `uzi worker list --json` return `draining_since` for every worker: an RFC3339 timestamp for a cordoned worker, `null` otherwise — i.e. **both** mappers carry it.
2. The Workers list shows a **dashed-border neutral pill** on a cordoned worker — `draining` when `active_runs > 0`, `cordoned` when `active_runs === 0` (including a chat-only `busy` worker) — and **nothing** on a non-cordoned worker; the pill is visually distinct from the amber `outdated`/`N/M runs` badges and sits adjacent to the status it contradicts.
3. The pill's tooltip states the effect ("not claiming new ones") without asserting a specific cause; the pill never enters the attention/alert surfaces; the pill's **label word** is reachable text (not hover-only).
4. `uzi worker list`'s STATUS reads `<status> (draining)`/`<status> (cordoned)` — e.g. `offline (draining)`, never a hardcoded `online` — for a cordoned worker, and the plain status otherwise.
5. The #365 scenario is now legible: a hosted worker that is online, shows `1/2 runs`, and is cordoned for a roll displays the pill, so "free slot but not claiming" has an on-screen explanation.
6. No DB migration, no SQL query change, no controller change, and no `.github/workflows/**` file is created or modified in implementation or validation.
7. `task gate:api` and `task gate:web` both pass — including the updated `mockApi.hosted.test.ts` — with the new behaviour covered by non-vacuous unit/component tests.
8. `main` is never touched; delivered on a branch + PR.

## Risks & mitigations

- **Mapping only one of the two DTO mappers** leaves the admin/register/heartbeat/set-token paths returning `null` for a cordoned worker, with no test reddening. Mitigation: M1 maps AND tests both `workerDTOFromRow` and `workerDTOFromWorker`, with a per-mapper call-site fold.
- **`statusCell` / pill hardcoding `online`.** `offline + draining` is reachable (a cordoned worker whose pod died). Mitigation: Decision 5 composes from raw status; M4 asserts the `offline (draining)` case.
- **Keying the label on `busy` instead of `active_runs`.** A chat-only cordoned worker (`busy: true, active_runs: 0`) would mislabel as `draining`/"finishing current runs" while holding zero runs, and the wrong `:1034` comment invites exactly this. Mitigation: Decision 3 keys on `active_runs`; the `:1034` comment is corrected; M3 asserts the chat-only row → `cordoned`.
- **The new hosted mock fixture breaks `mockApi.hosted.test.ts`** at three `toHaveLength` assertions (`:44/:49/:53`) AND breaks its quota-headroom demo (quota 3 vs seeded 2). Mitigation: M2 owns the fixture + the test fix together (same milestone, or the gate is red), rebalances the quota rather than bumping literals, and appends the fixture so `workers[0/1]` / `mockAdminWorkers[0..3]` don't shift.
- **Hover-only explanation is unreachable by keyboard/touch/SR** — the gap `WorkerUpgradeDetail` was built to close. Mitigation: Decision 7 makes the reachable **label word** carry the meaning; the `title` is supplementary only, and no diagnostic action is needed for a cordon.
- **Browser instruments are structurally blind to a native `title`** (`.claude/rules/web.md`). Mitigation: M2's browser pass asserts pill **presence per row** (real DOM); the tooltip-copy assertion lives in M3's jsdom title-attribute test.
- **Keying the pill on `upgrade_status`/`busy` instead of `draining_since`** (the mock's original framing). Mitigation: Decision 2; M3 asserts a worker that is `outdated` but not draining shows no pill.
- **Vacuous negative assertion** ("pill absent") passing forever after a copy/label change. Mitigation: pair each negative with a positive on the current wording, in the same `it` (web.md's worked example spans two adjacent tests and is easy to misread as unpaired), and mutate the call site.
- **Dashed pill hardcoding a fixed `slate-*` colour** that ignores the ember/mission retint. Mitigation: Decision 3/Background name the exact `edge`/neutral token classes.
- **Mock realism**: a browser pass validates rendering, not data. Mitigation: the M3 unit tests are authoritative; the browser pass is secondary.

## Dependencies

- **No external / internet dependency.** Every fact is codebase-resolvable; the offline Planned-sweep worker can complete this fully. No forge, network, or live-cluster interaction.
- **`workers.draining_since` (PRD #422) must be present** — it is, on `main` (`runtime.sql.go:3188,3250`; `models.go:633`; `service.go:1076`). This PRD only reads it.
- **No shared-file collision expected** with the concurrent web work flagged in the original handoff (PRD #57 doclinks does not touch `WorkersSettings.tsx`). If both land near each other, the second rebases the disjoint `web/` hunks.
- **Milestone ordering**: M2/M3/M4 need M1's DTO field; M3 needs M2's component; M5 is independent.

## Decision log

- **2026-08-21**: Feature scoped from a browser design mock reviewed with the user (a dashed-border neutral "draining/cordoned" pill across worker states). The mock settled the pill treatment (dashed, distinct from the amber upgrade/run badges) and the draining-vs-cordoned label split.
- **2026-08-21**: **Backend investigation corrected the mock's premise.** The mock (and the peer-session handoff) framed cordon/drain as a *derived* state (`outdated + busy + pending roll`) needing a possible new backend field. In fact it is **first-class and enforced**: `workers.draining_since` (PRD #422), read by the claim gate (`service.go:1076-1078`) and **already selected** by the queries but never mapped into `WorkerDTO`. So the feature is *expose the existing field*, not *invent a derived one*; no migration.
- **2026-08-21**: **Pill keys on `draining_since`, hosted-only by nature** (the cordon writes are `WHERE kind='hosted'`), so no `kind` special-casing. `upgrade_status: outdated` is orthogonal and must not drive the pill.
- **2026-08-21**: **Tooltip states the effect, not the cause** (drift ≠ always a version roll), correcting the mock's "draining for a version roll" copy and its "derived state / follow-up" footer note (the mock was updated and republished).
- **2026-08-21**: **CLI annotates STATUS** (`<status> (draining)`), no new column; `--json` carries the raw field. Honours the CLI-parity convention (`worker.go:81`).
- **2026-08-21**: **Draining is a normal state, not an alert** — kept out of `needsAttention`/Fleet panel; neutral dashed, not a warning tone.
- **2026-08-21**: **Two adversarial review passes (skill-decided, 2 reviewers) corrected the first draft**, all facts re-verified against code: (1) there are **two** row→DTO mappers (`workerDTOFromRow` + `workerDTOFromWorker`), not one — the admin/register/heartbeat/set-token paths need the field and a test each, or they return null; (2) `offline + draining_since` is a **reachable** state (`MarkStaleWorkersOffline` sweeps a dead cordoned pod offline without clearing the column), so `statusCell`/pill must compose from the raw status, never hardcode `online`; (3) the label must key on **`active_runs > 0`, not `busy`** — a chat-only cordoned worker is `busy` with `active_runs 0`, and the web `:1034` comment claiming `busy == active_runs > 0` is **wrong** and is corrected here; (4) the new hosted mock fixture **breaks `mockApi.hosted.test.ts`** (3 counts + a quota-headroom demo), fixed within M2; (5) `Badge` has **no `className` prop**, so a dedicated own-`<span>` component (mirroring `WorkerUpgradeBadge`, not `WorkerRunBadge`) is mandatory, using `edge`/neutral tokens + `border-dashed` (never a fixed `slate-*`); (6) the explanatory sentence is hover-only — Decision 7 makes the reachable **label word** carry the meaning and declines a detail-strip; (7) the tooltip-copy check must be a jsdom title-attribute assertion, since browser instruments are blind to a native `title`.
- **2026-08-21**: Next step = **queue for the uzi `Planned` sweep** (deferred, offline worker). PRD authored fully internet-independent and workflow-file-free.
