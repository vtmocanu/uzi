# PRD #4212: Admin → Workers tab with worker capacity settings

**Status**: Draft
**Priority**: Normal
**Umbrella**: #2415
**Blocks**: #4213 (admin-defined hosted worker sizes renders inside this tab)

## Problem

An admin tuning hosted worker capacity has to look in two places, and for some knobs cannot use the UI at all:

- Worker settings are scattered across the long **Admin → Instance** page (`web/src/pages/AdminSettings.tsx`), between judge, Slack, SSO and forge-label cards: the hosted worker quota, the ephemeral workers switch, the custody hold limit, the Docker worker repo allowlist and capability-aware scheduling.
- The ephemeral worker per-user cap, default size and idle lease are environment-only (`UZI_EPHEMERAL_MAX_PER_USER`, `UZI_EPHEMERAL_DEFAULT_SIZE`, `UZI_EPHEMERAL_LEASE`, read once in `api/internal/config/config.go` `Load`). On k8s the chart always renders the last two (`deploy/chart/templates/api-deployment.yaml:117-120`, defaults in `deploy/chart/values.yaml` `workers.ephemeralLease` / `workers.ephemeralDefaultSize`). Changing one needs a Helm or compose edit and an api restart.

## Outcome

A new admin-only page, **Admin → Workers** (`/admin/workers`; always named "Admin → Workers" to keep it distinct from the user-facing Workers page `/workers`), holds every instance-level worker capacity control. The three ephemeral knobs become admin settings that take effect on the next provisioner pass, with no restart.

Acceptance examples:

1. An admin opens Admin → Workers and sees the hosted worker quota, ephemeral workers switch, ephemeral per-user cap, ephemeral default size, ephemeral lease, custody hold limit, Docker worker repo allowlist and capability-aware scheduling. None of them appears on Admin → Instance any more, nor in its on-page table of contents.
2. On a Helm install with `workers.ephemeralLease`, `workers.ephemeralDefaultSize` and `UZI_EPHEMERAL_MAX_PER_USER` all unset, an admin sets the ephemeral per-user cap to 3 and saves. A user already at 2 ephemeral workers gets a third provisioned on the next provisioner pass, without an api restart. "Reset to default" returns it to 2.
3. With `UZI_EPHEMERAL_LEASE=30m` set in the api environment (or `workers.ephemeralLease: 30m` in the chart), the lease field shows `30m`, labelled as set by the environment and disabled; a PUT to that key is refused with the same error the other env-sourced keys return.
4. An admin sets the default size for new workers to `m`. A user opening the provision form sees `m` preselected; workers that already exist keep their size.
5. Saving an ephemeral or new-worker default size that is not a known size, a negative or non-integer cap, or a lease above 2h is refused at write time with a field error; the stored value is unchanged.

## Out of scope

- Editing, adding or removing worker sizes: #4213.
- Run time budgets (`RUN_TIMEOUT`, wall ceiling, requeues): split out of #2415 into #4211.
- `UZI_EPHEMERAL_SATURATION_DELAY` and `UZI_EPHEMERAL_PROVISION_DEADLINE`: anti-churn internals, stay environment-only.
- Per-namespace ceilings (LimitRange, ResourceQuota): Helm-only.
- `completion_interlock_rollout`: a run-protocol rollout switch, not a capacity control; stays on Instance.
- The per-user ephemeral opt-in (`users.ephemeral_workers_enabled`, `web/src/components/HostedWorkers.tsx`): a user setting, unchanged.
- CLI: admin settings writes are cookie-only and `uzi settings get` reads account settings (`api/internal/uzicli/client_account.go:158`), so there is no CLI admin-settings surface to extend; no CLI change. (State this in the PR.)
- Evicting workers when the cap is lowered: an owner above a lowered cap gets no new ephemeral worker (`ephemeral.go:424` refusal) and keeps the ones they have until they are released normally. Documented, not changed.

## Modules and seams

### Settings keys (`api/internal/settings`)

Four new non-secret keys, each in `Defaults` and an explicit `Validate` case; the three ephemeral keys also join the env overlay (PRD #25 precedence, `settings.go` `get`/`effective`/`source`, sources `env|db|default`, shown in the UI as environment / saved / default):

| key | env (overlay source) | default | validation (write time) |
|---|---|---|---|
| `ephemeral_max_per_user` | `UZI_EPHEMERAL_MAX_PER_USER` | `2` | base-10 integer, 0..20 (0 = no ephemeral provisioning for anyone; same typo bound as `maxHostedWorkerQuota`) |
| `ephemeral_default_size` | `UZI_EPHEMERAL_DEFAULT_SIZE` | `m` | `workersize.Valid` |
| `hosted_default_size` | none | `l` | `workersize.Valid` |
| `ephemeral_lease` | `UZI_EPHEMERAL_LEASE` | `2h` | Go duration, 0..2h (0 = no lease; the same range `parseEphemeralLease` accepts from env today). The 2h bound is a constant in `settings` equal to `config.MaxEphemeralLease`, pinned by a test, not an import (settings must not import config). |

- **Precedence is the existing PRD #25 rule: env wins and locks the field.**
- **The overlay is built from the raw environment, not from `cfg`.** `api/cmd/server/main.go` (where `slackEnv` is built for `ConfigureSecrets`, lines ~286-296) adds each of the three keys only when `os.LookupEnv` finds a non-empty, trimmed value, stored in canonical form. `cfg.Ephemeral*` always holds a value (defaults included), so building from `cfg` would lock every field.
- **Env values are interpreted exactly as today** (user decision 2026-11-12): the cap via lenient `parseInt` (`config.go:2385`; a malformed value or `0` becomes 2, values above 20 are honoured), the default size with its warn-and-fall-back-to-`m` (`config.go:~1464`), the lease via strict `parseEphemeralLease` (any 0..2h). The overlay stores the **canonical value `config.Load` produced** for a key whose raw env is non-empty, so an env-locked field shows exactly the value the api runs with, and the strict accessors never see a raw unparsed string. Only admin writes are strictly validated by `Validate`. An env value outside the admin bounds (e.g. cap 25) is honoured and shown env-locked. No install changes behaviour on upgrade.
- Read accessors follow `HostedWorkerQuota`: strict (error returned, not silently defaulted).

### Chart and compose (`deploy/chart`, `docker-compose.yml`)

- `workers.ephemeralLease` and `workers.ephemeralDefaultSize` default to null in `values.yaml`, and `api-deployment.yaml` renders each env entry **only when its value is set** (an explicit `0` lease still renders, as today). Keep the `api.config`/`api.secretEnv` duplicate-key fail guards (`api-deployment.yaml:9-13`). Update `scripts/assert-ephemeral-lease-render.sh` for the unset case.
- `docker-compose.yml` already passes `UZI_EPHEMERAL_LEASE: ${..:-}` (empty when unset), which the overlay's non-empty check ignores; keep that, and confirm the other two are not set to non-empty defaults there.
- Upgrade note (docs + changelog): an install that sets either chart value keeps its value and sees the field env-locked until the operator unsets it; an install that never set them gets editable fields with the same defaults (`2h`, `m`), so effective behaviour is unchanged on upgrade.

### Default size for new persistent workers

- `hosted_default_size` replaces the hard-coded preselection `DEFAULT_WORKER_SIZE = "l"` (`web/src/lib/workerSizes.ts:67`, ADR-2240) in the persistent provision form (`web/src/components/HostedWorkers.tsx:53,115`).
- Users read it through the existing session-authenticated `HostedConfig` (`api/internal/handler/hosted_workers.go:~319`), which the form already fetches on mount; it gains a `default_size` field. A settings read error falls back to `l` in that response (display-only preselection, not policy) and is logged.
- The persistent provision API keeps requiring an explicit size (ADR-2240); only the form's preselection changes. Existing workers are unaffected.
- Form behaviour, each tested: the initial preselection is `default_size` once `HostedConfig` arrives; a size the user already picked before the fetch resolves is not overwritten; after a successful provision the form resets to `default_size` (today it resets to the constant, `HostedWorkers.tsx:115`); a `HostedConfig` without `default_size` (new web against an old api during a rolling upgrade) preselects `l` as today.
- ADR-2240's current-policy statement ("preselect `l`") is amended to "preselect the admin `hosted_default_size`, shipped default `l`".
- No env source: it was never env-configurable.

### Ephemeral lease and cap consumers (`api/internal/hostedsvc`, `api/internal/workersvc`, `api/internal/handler`)

All readers of the three values route through the settings accessors, so they never disagree:

- Provisioner (`api/internal/hostedsvc/ephemeral.go`): add `EphemeralMaxPerUser`, `EphemeralDefaultSize`, `EphemeralLease` to the `EphemeralSettings` interface (`ephemeral.go:50-53`, which already serves `EphemeralWorkersEnabled` and `DockerRepoAllowlist` per pass) and remove the matching fields from `EphemeralConfig` (update its test construction sites, e.g. `api/internal/handler/*ephemeral*_livedb_test.go`). Read once at the start of each pass and use that snapshot for the three trigger queries (`ephemeral.go:205,223,243`), the cap check under the per-user advisory lock (`ephemeral.go:414`), and the reaper (`:347`). A read error skips the whole pass, logged once (fail closed: nothing provisioned, nothing reaped on a blip).
- Lease entry at completion and ClaimRun's lease arm: `workersvc.Service.ephemeralLease` (`service.go:~2006`, set by `wsvc.SetEphemeralLease` in `main.go:~605`; read in `completion_permit.go:~924`, `ephemeral_lease.go:~195`) reads the accessor per decision instead of a boot-time field.
- `handler/workers.go:~264` (`ephemeral_lease_expires_at`) reads the accessor.
- **One lease policy: the current effective duration, everywhere.** Lease expiry is always computed as lease start + the current effective `ephemeral_lease`, consistently in the expiry display, ClaimRun/rebind, completion lease entry, Admin health and the reaper. Shortening releases leased-idle workers whose new expiry has passed on the next reaper pass; lengthening extends existing leases; `0` releases every leased-idle worker on the next pass (never strands one); `0` → >0 applies to completions after the change. A settings read error makes the reaper skip the pass (no release on a blip). Existing transaction and custody fences around lease release are unchanged. Each case is tested.

### Admin → Workers page (`web`)

- New route `/admin/workers` (`web/src/App.tsx`, guard `admin`) and an "Admin → Workers" entry in `AdminShell` nav (`web/src/components/AdminShell.tsx`), after `Instance`.
- Moves, unchanged in behaviour, from `AdminSettings.tsx`: `HostedWorkerQuotaCard`, `EphemeralWorkersCard`, `CustodyHoldLimitCard`, `DockerAllowlistCard`, `CapabilitySchedulingCard`, keeping each card's `id` (`hosted-worker-quota`, `ephemeral-workers`, `custody-hold-limit`, `docker-allowlist`, `capability-scheduling`). Remove them from Instance's on-page table of contents (`AdminSettings.tsx:~307-311`; its test `AdminSettings.test.tsx:~1846`) and give the new page its own.
- Adds controls for the four new keys (the three ephemeral ones grouped with `EphemeralWorkersCard`, `hosted_default_size` with `HostedWorkerQuotaCard`), showing effective value, source and reset-to-default, following the existing `sources`/`defaults` card props.
- Reset sends the existing admin settings PUT with an empty value for the key (the repo's reset convention, verify against `AdminSettings.tsx` cards); the response reports source `default` (or `env` when the env overlay holds the key, in which case the control is disabled and reset is not offered).
- Saves go through the existing admin settings PUT; no new endpoint.
- **Old anchors redirect client-side**: `AdminSettings` checks `location.hash` on mount and, for a moved card's id, navigates to `/admin/workers#<id>` (hash fragments never reach the server).

### Docs

- `docs/admin-settings.md`: keep the existing section anchors (`#hosted-worker-quota`, `#custody-hold-limit`, …) so inbound links from `docs/configuration.md:~56`, `docs/hosted-workers.md:~99`, `docs/cli.md:~1471`, `docs/run-recovery.md:~1150` keep resolving; change the "Edit it on Admin → Instance" wording to Admin → Workers.
- Location wording in: `docs/admin-settings.md`, `docs/hosted-workers.md` (~25, ~99), `docs/capability-scheduling.md` (~182), `docs/cli.md` (~1502), `docs/configuration.md` (env table: the three keys become admin settings with env precedence; env interpretation unchanged). Sweep with `git grep -n -F 'Admin → Instance' -- docs web/src api/cmd/uzi` and read each hit; only those about a moved card change. `task docs:sync`, `task check-docs:web`.

## Testing decisions

- Settings: per key, valid / invalid / reset / env-locked-PUT-refused, following `settings_hosted_test.go`; a test pinning the settings lease bound to `config.MaxEphemeralLease`.
- Config: existing `config.Load` behaviour for the three env values is pinned unchanged (malformed cap → 2, `0` cap → 2, cap 25 honoured, unknown size → `m`, lease bounds).
- Overlay: a key appears in the overlay only when the raw env is non-empty, holding the canonical `cfg` value (main.go wiring test or a small extracted helper).
- Lifecycle: the lease-policy cases above and the cap change run as LiveDB tests through `./e2e/run-store-it.sh` (live-DB rules in `.claude/rules/go.md`).
- E2E contract sweep (CLAUDE.md "A product contract change updates the E2E harness"): `git grep -F` the changed literals and settings keys across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`; run `task test:e2e-lib` and `task test:forge-fake` if anything there changes; name unrun phases in the PR.
- Provisioner: a changed cap takes effect on the next pass without rebuilding the provisioner; a settings read error provisions and reaps nothing; lease 0 releases leased-idle workers on the next reaper pass. Follow the existing ephemeral provisioner/reaper tests.
- workersvc: lease entry at completion follows a changed setting.
- Chart: `scripts/assert-ephemeral-lease-render.sh` covers unset (no env entry), `0`, and a duration; same for default size.
- Web: Vitest for the Workers page (cards render, new controls save, env-sourced field disabled); route-smoke entry in `web/src/App.routes.test.tsx`; the moved cards' tests pass at their new home; the hash-redirect test; Instance's TOC test updated.
- Mock mode: the page renders with mock data (`web/src/mocks`).

## Milestones

### M1: Admin → Workers holds the existing worker cards
Blocked by: none.
- Route, nav entry, the five moved cards, both TOCs, client-side hash redirect, web tests, docs location wording (list above; `task docs:sync`).
- Acceptance: example 1 minus the four new controls; every listed doc site names Admin → Workers; old anchors land on the moved card.

### M2: Ephemeral cap, default sizes and lease are admin settings
Blocked by: M1.
- The four keys, `HostedConfig.default_size` and the form preselection, raw-env overlay with canonical values, all consumers on the accessors with the one lease policy, chart render-when-set + assert script, the controls on the Workers tab, ADR-2240 amendment, docs (`docs/admin-settings.md`, `docs/configuration.md`, upgrade note), changelog fragment.
- Acceptance: examples 2-5; lease change semantics above, tested.

## Landing obligations

- Full `test:api`, `test:web`, `test:web-browser`, `gate:repo`: pending exact-head PR CI.
- No `.github/workflows/**` change.
- Linked `acceptance` issue (user-owned): on the hosted cluster, after the chart upgrade, the three fields are editable and a cap change takes effect without an api restart.

## Decision Log

- **One PRD for the tab and the ephemeral settings** (umbrella split, 2026-11-12): moving cards alone has little standalone value, and the new controls need the tab; two milestones, one PR. Rejected: a separate issue for the tab.
- **Env wins and locks the field** (PRD #25 precedent, `settings.go` `source`): keeps upgrade behaviour identical for installs that set the value. Rejected: admin value overrides env (two sources of truth; an operator's Helm value silently ignored).
- **Chart renders the two env entries only when set** (review finding, 2026-11-12): the chart rendered them unconditionally since #2412, which under env-wins would lock both fields on every Helm install, the primary runtime. Defaults stay identical (`2h`, `m`), now supplied by settings. Rejected: an "env is only a seed" exception for these keys (inconsistent with every other env-sourced key).
- **Overlay keyed on raw env, valued from `cfg`; env interpretation unchanged** (user decision 2026-11-12): `cfg` always carries defaults, so presence comes from the raw env; the value is the canonical one `config.Load` produced, so the UI shows what the api runs with. Rejected: strict env at boot (an existing install with a lenient value would fail to start on upgrade).
- **One lease policy, current effective duration** (buddy review): retaining each lease's entry duration while reaping on the current one would make display and release disagree.
- **One accessor for every lease reader**: the lease gates provisioner queries, reaper, completion lease entry, ClaimRun and the API's expiry field; per-pass reads in only one of them would make them disagree.
- **Default size for new persistent workers is an admin setting** (user ask, 2026-11-12): replaces ADR-2240's hard-coded `l` preselection; served via `HostedConfig` because users, not only admins, open the provision form. The provision API still requires an explicit size. Rejected: a server-side default when the size is omitted (would change the API contract ADR-2240 kept).
- **Saturation delay and provision deadline stay env-only**: they guard against pod churn and stranded pods; not capacity policy.
- **Completion interlock stays on Instance**: a run-protocol rollout, not a capacity control.
- **No CLI change**: there is no CLI admin-settings read/write path for instance settings today; adding one is out of scope.
- **Cap bound 0..20**: mirrors `maxHostedWorkerQuota`; the namespace ResourceQuota remains the real backstop.
