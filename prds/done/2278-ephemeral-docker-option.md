# PRD #2278: Docker-capable ephemeral workers

**Status**: Done (2026-10-05). Baseline facts below were read at `main` `721f8dd3`.

Implemented and validated on the issue branch. Acceptance evidence is recorded in the completion section below.

**Design mock**: `prds/mockups/2278-ephemeral-docker-mock.html` (open in a browser; current vs proposal). The mock is the agreed visual reference for the Workers page. It is static, never shipped. Where the mock and this PRD disagree, this PRD wins.

## Problem

A user with ephemeral auto-provisioning on has no way to say "my ephemeral workers should have Docker". Persistent hosted workers have a **Docker-capable** checkbox; ephemeral workers get Docker only when a run's `required_capabilities` contains `docker` at the moment the provisioner looks at it.

That is too late for the common case. A run on a repository with no static capability hint is created with `required_capabilities = '{}'`. When every capable worker is busy, the saturation trigger provisions a plain ephemeral worker for it. The lead then plans a quality gate that builds and smoke-tests a container image (the repo's normal source-change gate), the plan infers `docker`, and approval is refused with a 409:

> the plan requires capabilities the assigned worker cannot satisfy: docker

By then a full planning pass and its review rounds are spent. The remaining choices are all poor: approve with the capability override (skips the container checks; web only, the CLI has no flag), cancel and restart on a Docker-capable persistent worker, or revise the plan to drop the checks.

The repo-level hint (Repos → Tools → `docker`) already avoids this, but it is per repository and must be known before the first run. The user asked for a per-user switch that mirrors the persistent form.

## Outcome

The Workers page's hosted-worker card gains a persisted **Docker-capable** checkbox for ephemeral workers, on the same row as **Auto-provision on demand**. When it is ticked:

- for ordinary runs whose non-null `repo_id` is in the admin's `docker_repo_allowlist`, ephemeral workers created by the capability-gap and saturation triggers carry the Docker dimension, exactly as a Docker-capable persistent worker does;
- for those same allowlisted runs, a kept-warm (leased) ephemeral worker WITHOUT Docker no longer picks up the user's new runs, so the next run gets a Docker-capable worker instead.

The preference also requires the instance's docker tier. It does not apply to unallowlisted repositories, repo-less runs (including judges), jobs or isolated-lane runs. An allowlist read error means empty membership: discard any values returned alongside the error, add no preference-driven Docker and do not make a plain leased worker step aside. Capability-driven Docker is unchanged, including when the preference is off or inapplicable; `fn_worker_can_claim` remains the independent claim fence, with its existing repo-less judge exception unchanged.

Acceptance examples:

1. **Late Docker need no longer stalls the gate.** The instance offers the docker tier; user A has auto-provision on and **Docker-capable** ticked; repo R is explicitly in the admin's `docker_repo_allowlist` and has no capability hint; A's only persistent worker is busy. A starts an issue run on R. The saturation trigger provisions an ephemeral worker with `docker_enabled = true`; the lead plans a container build; approval succeeds because the owning worker's effective capabilities (`capability.EffectiveWorkerCaps`) include `docker`.
2. **Warm non-Docker worker steps aside.** The instance offers the docker tier; repo R is explicitly in the admin's `docker_repo_allowlist`. User A has auto-provision on and a leased, non-Docker ephemeral worker W (warm after a run on R, branch B). A ticks **Docker-capable**, then starts a follow-up run on R/B. W does not claim it; the provisioner treats the run as unplaced and creates a Docker-capable ephemeral worker, which claims it. W is reaped when its lease ends, or may be evicted early under the existing per-user cap policy.
3. **No docker tier, no checkbox.** On an instance whose chart has `workers.docker.enabled: false`, `GET /api/workers/hosted/config` reports `docker_enabled: false`, the checkbox is not rendered, and `PUT /api/me/ephemeral-workers` with `{"docker": true}` returns 409 with a clear message. `{"docker": false}` is always accepted.
4. **Preference does not bypass admin scope.** With A's preference on and the tier available, a run on an unallowlisted repo, or a repo-less judge, gets no preference-driven Docker. A plain lease does not step aside for an unallowlisted run. If reading the allowlist fails, even a returned list containing R is discarded: the preference neither provisions Docker nor excludes a plain lease. Capability-driven Docker and the independent `fn_worker_can_claim` fence retain their existing behaviour.

## Out of scope

- **Moving an already-bound run to another worker.** A run whose plan infers Docker on a non-Docker worker still gets today's 409 and today's choices. Transferring a gated run between workers touches binding, quota, claim fencing, plan revision, checkpoint restore and custody; it needs its own PRD if wanted.
- **Persistent-worker placement.** The setting does not force ordinary runs onto Docker-capable persistent workers, and does not change persistent provisioning.
- **Repo-less `job` runs and isolated-lane (profile-bound) runs.** Both stay non-Docker by their existing non-bypassable rules.
- **A CLI verb for the setting.** The existing auto-provision opt-in has no CLI verb (`api/cmd/uzi` has none for `/me/ephemeral-workers`); adding a settings-verb family is a separate surface. The CLI `run approve --override-capabilities` gap is likewise separate.
- **An admin "allow ephemeral Docker" switch.** The docker tier being configured, the existing hosting and ephemeral admin gates, and membership in the existing `docker_repo_allowlist` gate the preference; no new admin switch is added.

## Modules and seams

### Per-user setting: `users.ephemeral_docker_enabled`

- Additive migration: `ALTER TABLE users ADD COLUMN ephemeral_docker_enabled boolean NOT NULL DEFAULT false`. Number assigned at merge time (`task migration:renumber`); the live head at writing is `00294`.
- Written only through the existing session-scoped route `PUT /api/me/ephemeral-workers` (`api/internal/handler/ephemeral_workers_toggle.go`, mounted in `routes_me.go:188`). The request body gains an optional `docker` boolean beside `enabled`; each field is optional and an absent field leaves its column unchanged, so the existing web call keeps working. The target user is always the session user, never the body (as today).
- Enabling (`docker: true`) is refused with 409 when the instance has no docker tier (below). Disabling is always accepted, so a user can turn it off after an operator removed the tier.
- Turning auto-provision off does not clear `ephemeral_docker_enabled`; the stored choice returns when auto-provision is turned back on.
- The user DTO (`toDTO`) exposes `ephemeral_docker_enabled`; mirror it in the web `User` type and the mock-mode user fixtures (`web/src/mocks/data/users.ts`).

### Docker-tier availability: api config

- Today the api cannot tell whether the docker tier exists: `HostedConfig` (`api/internal/handler/hosted_workers.go:318`) returns only `enabled`, `quota`, `ephemeral_enabled`. The controller knows it (`WorkerDockerNamespace`, `controller/internal/config/config.go:232`), rendered from the chart's `workers.docker.enabled` / `workers.docker.namespace` (`deploy/chart/values.yaml:864-873`).
- Add an api env var, default false, rendered in `deploy/chart/templates/api-deployment.yaml` from `workers.docker.enabled` (only meaningful when both `workers.enabled` and `workers.controller.enabled`). Name it in the api config package beside `WorkerHostingEnabled`; compose leaves it unset (false), which is correct there.
- `HostedConfig` returns `docker_enabled` from it. The web reads it; the handler above enforces it server-side.
- Compatibility follows the existing singleton api with `Recreate`; no concurrent api-version assumption is needed. An old api config response missing `docker_enabled` hides the new checkbox. An old web request containing only `{enabled}` still works and leaves the Docker preference unchanged. If an api rollback rejects a `{docker}` write, the UI restores the prior checkbox value and shows the error in the existing ephemeral error slot.

### Provisioner: `EphemeralProvisioner.ProvisionPass`

- `api/internal/hostedsvc/ephemeral.go` (`ProvisionPass`'s candidate loop). Today `docker` comes only from `capability.ResolveEphemeralSpec(c.caps)`. New rule, applied after resolution: for a non-job, non-isolated candidate from the capability-gap or saturation trigger whose owner has `ephemeral_docker_enabled` AND the instance has the docker tier AND whose non-null `repo_id` belongs to the admin's `docker_repo_allowlist`, set `docker = true`. The two list queries (`ListUnplaceableQueuedRunsForEphemeral`, `ListSaturationQueuedRunsForEphemeral`) already JOIN `users`; return the preference, `r.repo_id` and `r.kind` from them and carry them in the candidate rather than adding a read per candidate.
- Read the admin allowlist for the pass and pass the same membership input to the preference-aware query predicates. On a read error, discard any accompanying values and use an empty allowlist: no preference-driven Docker provisioning, no preference-only gap candidate, and no plain-lease step-aside. There is no repo-less judge exception to the preference. This does not clear Docker resolved from required capabilities or weaken `fn_worker_can_claim`.
- Never for the isolated-lane trigger (the unchanged `isolated && docker` guard in `ProvisionPass`).
- Never for a `kind = 'job'` candidate: `ClaimRun` lets a job be claimed ONLY by a non-Docker worker (the PRD #1908 job clause in `ClaimRun`), so a Docker ephemeral bound to a job could never claim it. Both lists CAN return jobs today: the gap query has an explicit job arm and the saturation query has job-specific eligibility predicates. So both queries return `r.kind` beside the owner's preference, the candidate struct carries it, and the preference is applied only when `kind <> 'job'`. Planned tests cover both triggers with the preference on.
- Capability-driven Docker is untouched: preference off, an unallowlisted or absent repository, or an allowlist read error does not change `ResolveEphemeralSpec`'s Docker result. The existing `fn_worker_can_claim` allowlist/capability fence still governs claims independently; do not replace or relax it.
- Resource bound: none new. The per-user ephemeral cap and the advisory-locked count in `provisionOne` stay authoritative; Docker only changes pod shape (and the controller renders it into the existing docker namespace, as for `docker_enabled` persistent workers).

### Warm reuse: the lease arm

- A leased ephemeral worker may claim a same-owner, same-repo, same-branch queued run through `fn_ephemeral_lease_admits` (the PRD #2006 lease arm of `ClaimRun`; rebind via `RebindLeasedEphemeralWorker`, `api/internal/workersvc/ephemeral_lease.go`). The arm does not look at Docker.
- New condition on that arm: when the run owner has `ephemeral_docker_enabled`, the instance has the docker tier, and the run is non-job, non-isolated with a non-null `repo_id` in the admin's `docker_repo_allowlist`, the claimant must be `docker_enabled`. Otherwise the preference does not exclude a plain lease, including on an allowlist read error (empty membership, with returned values discarded). Existing bound-run admission, custody holds, lease expiry and cap eviction stay unchanged; W may be evicted before lease expiry under that existing cap policy. Preserve the independent `fn_worker_can_claim` fence.
- Mirror the same condition everywhere the lease arm is mirrored, or a non-Docker leased worker would read as a placement and suppress provisioning (or attract a deferral it can never claim):
  - the fleet-spread peer check in `ClaimRun`;
  - the "already placeable" `NOT EXISTS` in `ListUnplaceableQueuedRunsForEphemeral` and the eligible-worker `EXISTS` in `ListSaturationQueuedRunsForEphemeral`;
  - both lease checks in `CountOnlineWorkersClaimableForRun` and the second eligible-worker check in `ListSaturationQueuedRunsForEphemeral`.

  These are seven call sites: two in `ClaimRun`, two in `CountOnlineWorkersClaimableForRun`, one in `ListUnplaceableQueuedRunsForEphemeral`, and two in `ListSaturationQueuedRunsForEphemeral` (re-sweep with `git grep -n fn_ephemeral_lease_admits`; line numbers drift, use query names).
- **Mirroring the exclusion is not enough: the stepped-aside run must become provisionable.** Example: repo R is explicitly in the admin's `docker_repo_allowlist`, the owner has the preference on and the instance offers the docker tier; the new run on R has `required_capabilities = '{}'`, the user has no persistent worker online, and the only worker is a plain leased ephemeral, now excluded. The gap query skips it (its non-job arm requires `cardinality(r.required_capabilities) > 0`) and the saturation query skips it (its `EXISTS` finds no eligible worker), so nothing is provisioned and the run strands. Fix in `ListUnplaceableQueuedRunsForEphemeral`: the non-job arm's capability conjunct becomes "non-empty requirements, OR `fn_ephemeral_docker_preference_applies` is true (owner preference AND instance docker tier AND non-job, non-isolated run AND non-null `repo_id` in the admin allowlist)", with the existing "nothing online can place it" `NOT EXISTS` (now carrying the Docker lease condition) unchanged. Effect: with the preference applicable to an explicitly allowlisted repository, a capability-free run that no online worker can claim is provisioned for immediately (gap trigger), like a capability-gap run; one that a busy persistent worker could claim still waits for the saturation debounce. An allowlist read error makes membership empty and disables this preference-only widening, discarding values returned alongside the error. The per-user opt-in JOIN, the per-user cap, `provisionOne`'s locked count, the lane and job exclusions, and `fn_worker_can_claim` are unchanged.
- Add one non-STRICT SQL helper, `fn_ephemeral_docker_preference_applies`, for the preference/tier/kind/lane/repository-membership predicate. It returns false for NULL or empty membership inputs, including a NULL `repo_id`; it has no repo-less judge exception. Keep the old `fn_ephemeral_lease_admits` signature and body unchanged. At each of the seven lease call sites in `runtime.sql`, combine existing lease admission with "preference does not apply OR worker is Docker-enabled" using the helper; use the same helper for D8's gap widening. Allowlist readers discard values on error before supplying membership. The helper is additive and leaves `fn_worker_can_claim` intact.

### Web: `web/src/components/HostedWorkers.tsx` (the agreed UX)

Follow the mock. Concretely:

- The card keeps one **Hosted workers** heading and gets two labelled sub-sections, **Persistent worker** (the existing provision form, unchanged controls) and **Ephemeral workers**, separated by a rule. Each sub-section renders only when its gate holds today (`showManual` / `showEphemeral`).
- **Ephemeral workers**: the **Auto-provision on demand** switch and a **Docker-capable** checkbox sit on ONE row, side by side. The checkbox is never nested or indented under the switch.
- The checkbox renders only when `config.docker_enabled`. It persists on change through `api.setEphemeralWorkersEnabled`'s route (extend the client method or add a sibling that sends `{docker}`), shows a pending (disabled) state while the write is in flight, rolls back on failure, and shows its error in the ephemeral section's existing error slot. It stays enabled and keeps its value while auto-provision is off. A checkbox (not the `Toggle` primitive) is a deliberate user decision; update the component comments that describe the switch/checkbox convention accordingly.
- ONE short paragraph under that row covers both controls. Copy (the final sentence is exact):
  > uzi spins up a throwaway hosted worker when a run needs a capability no online worker has, or when every capable worker stays busy. It may be kept warm for up to 2 hours for a follow-up run on the same branch, and counts toward your ephemeral limit meanwhile. Docker-capable includes Docker for repositories your admin allows, at extra CPU and storage.
  - When the Docker checkbox is not rendered, drop the last sentence.
- Remove from the ephemeral copy: the word *experimental* and the "~2.6 GiB tool-cache cold start" sentence. The user's position: ephemeral workers work well; the label and figure no longer inform the choice. (PRD #649 required both; this PRD supersedes that copy rule.)
- Fix the trigger description: the current copy names only the capability-gap trigger; the provisioner also fires on saturation.
- **Persistent worker** paragraph, shortened per the mock: drop the "rootless, isolated" promise (the chart supports `workers.docker.rootless: false`, `values.yaml:858-887`), say it gives a Docker daemon for container builds and tests at extra CPU and storage.
- Mock mode: the hosted-config and user fixtures gain the new fields so mock mode (`.claude/rules/web.md`) shows the row.

### Docs

- `docs/scheduling.md` § "Auto-provisioning a worker for an unmet capability": describe the saturation trigger if absent, the Docker-capable option, its warm-reuse effect, admin allowlist scope, fail-closed membership reads, and exclusions (isolated lane, jobs, repo-less runs including judges). Drop any "experimental" framing there to match the UI. Also correct the stale "What you'll see" passage that says the worker claims only that one run and disappears afterwards: since PRD #2006 a finished ephemeral worker may be kept warm and claim a same-owner, same-repo, same-branch follow-up run.
- `docs/hosted-workers.md` and `docs/worker-docker.md`: one line each pointing at the ephemeral option; keep `worker-docker.md`'s accurate "rootless by default" wording.
- Run `task docs:sync` and commit the mirror (`TestEmbeddedDocsMatchSource`).
- `specs/human.md`: add one requirement line for the per-user ephemeral Docker option, tagged `(AI-synced YYYY-MM-DD)`.

## Testing decisions

- **Unit** (`api/internal/hostedsvc`): provisioner docker decision table over {preference off/on} × {tier on/off} × {trigger gap / saturation / isolated} × {run requires docker or not} × {kind job / judge / ordinary} × {repo allowlisted / unallowlisted / NULL} × {allowlist read success / error}. Error fixtures return both an error and values containing R: assert the values are discarded and preference-driven Docker is absent. Assert capability-driven Docker is unchanged when membership is empty, missing or unreadable. Follow `api/internal/capability/ephemeral_test.go` for table style.
- **Live DB** (`api/internal/handler/ephemeral_lease_provisioner_livedb_test.go` is the pattern): positive preference cases use repo R explicitly in the admin's `docker_repo_allowlist` and an available docker tier.
  - helper truth table: `fn_ephemeral_docker_preference_applies` returns false for NULL repository, NULL/empty allowlist, unallowlisted repo, preference/tier off, job or isolated lane; a repo-less judge has no preference exception. Verify the helper is non-STRICT and the old lease signature remains callable; test `fn_worker_can_claim`'s existing fence independently, including its unchanged repo-less judge exception;
  - unallowlisted repo: no preference-driven provisioning or D8 gap widening, and a plain lease retains existing admission;
  - allowlist read error, including returned values containing R: membership is empty, no preference-driven provisioning or D8 gap widening, and a plain lease does not step aside;
  - preference on: a leased non-Docker worker does not claim a same-branch run; the run is listed by the provisioning query; a Docker-capable ephemeral is provisioned and claims it;
  - **the stranding regression**: preference on, run with EMPTY `required_capabilities`, NO persistent workers, only a plain leased ephemeral: the warm claim is refused, `ListUnplaceableQueuedRunsForEphemeral` returns the run, `ProvisionPass` creates a `docker_enabled` ephemeral, and that worker claims it (the full path, one test);
  - both triggers with the preference on return jobs that the provisioner does NOT give Docker;
  - preference off: the leased non-Docker worker still claims it (today's behaviour, regression guard);
  - the fleet-spread peer mirror does not defer an allowlisted run to a non-Docker leased peer when the preference applies; cover all seven lease call sites, including both claimable-count checks and both saturation checks;
  - quota: the per-user cap still bounds provisioning with the preference applicable to R, including early eviction of W under the existing releasable-lease policy.
- **Handler**: `PUT /me/ephemeral-workers` partial bodies (only `enabled`, only `docker`, both); `docker: true` on a tier-less instance → 409, `docker: false` → 200; a session can only write itself; `GET /api/workers/hosted/config` reports `docker_enabled`; an old web `{enabled}` request leaves the Docker preference unchanged.
- **Chart**: a render assertion script modelled on `scripts/assert-ephemeral-lease-render.sh`, exposed as a standalone `render:*` target beside `render:ephemeral-lease-check` (offline, needs `helm`), proving `workers.docker.enabled: true` sets the api env var and `false` leaves it false.
- **Web** (vitest, `web/src/components/HostedWorkers.test.tsx` or the Workers page test): the checkbox is absent when `docker_enabled` is false or missing from an old api config response; present and on the same row as the switch with it; pending/rollback/error on a failed write, including a `{docker}` write rejected after api rollback; the ephemeral copy contains neither "experimental" nor "2.6"; neither section says "rootless". Update `apiContract` fixtures for the new DTO fields.
- **Mutation check** (`.claude/rules/go.md`): remove the new lease-arm conjunct and watch the "warm non-Docker worker steps aside" test fail.
- Gates and commands (each must run, not just compile):
  - `task gate:api`, `task gate:web`, `task gate:repo` (migration numbering, docs). `gate:api` does NOT run `*LiveDB` tests.
  - Live-DB: `./e2e/run-store-it.sh` (it covers `handler`, `workersvc`, `store`; a new `*LiveDB` test must live in one of its enumerated packages). Evidence per `.claude/rules/go.md`: each new test named as `--- PASS`, zero `--- SKIP`, `RUN > 0`.
  - Chart: the new standalone render target (below), run directly with `task render:<name>`.
- **Workflow scope**: implementation and validation must not create or modify anything under `.github/workflows/**`. The new chart-render target stays standalone in `Taskfile.yml`, like `render:ephemeral-lease-check`, and is NOT wired into CI here.

## Milestones

### M1: Ticking Docker-capable makes new ephemeral workers Docker-capable

Blocked by: none.

The vertical slice: migration, api config + chart env, `HostedConfig.docker_enabled`, the `PUT /me/ephemeral-workers` `docker` field with tier enforcement, the provisioner rule (gap + saturation only; non-null `repo_id` in the admin allowlist; lane, job and repo-less runs including judges excluded; allowlist read errors discard returned values and mean empty membership), the user DTO, the web card per the agreed UX (sub-sections, one row, one paragraph, copy removals and fixes, persisted checkbox with pending/rollback/error, hidden without the tier), mock-mode fixtures, docs + `docs:sync`, `specs/human.md`.

Acceptance:
- [x] Acceptance example 1 and 3 hold (live-DB/handler tests), evidenced by `./e2e/run-store-it.sh` output.
- [x] Both triggers restrict preference-driven Docker to explicitly allowlisted repositories, exclude jobs, lanes and repo-less judges, and discard allowlist values on error; capability-driven Docker and `fn_worker_can_claim` remain unchanged (tests).
- [x] The new `task render:*` chart target passes.
- [x] Preference off: provisioning is byte-identical to today (regression test).
- [x] The Workers page matches the mock's Proposal section; tests above pass.
- [x] `task gate:api`, `task gate:web`, `task gate:repo` green.

### M2: A warm non-Docker ephemeral worker steps aside when the preference is on

Blocked by: M1.

The additive non-STRICT `fn_ephemeral_docker_preference_applies` helper, preserving the old lease signature, the allowlist-scoped lease-arm condition, all seven lease call sites, D8 gap widening, and their tests. Empty membership on allowlist read error disables preference-only widening and plain-lease step-aside.

Acceptance:
- [x] Acceptance example 2 holds, including the stranding regression (empty requirements, no persistent workers), evidenced by `./e2e/run-store-it.sh` output; preference off keeps today's warm reuse.
- [x] Unallowlisted runs and allowlist read errors (discarding returned values) retain plain-lease admission and do not become preference-only gap candidates; helper NULL inputs fail closed and the old lease signature remains callable.
- [x] The mutation check fails without the new conjunct.
- [x] `git grep -n fn_ephemeral_lease_admits` shows every call site either carries the condition or is documented as not needing it.
- [x] `task gate:api` green.

## Completion evidence (2026-10-05)

The implementation at `3251f72c` passed `task gate:api`, `task gate:web`, `task gate:repo`, `./e2e/run-store-it.sh`, `task render:ephemeral-docker-check`, and `npm --prefix web run build`. The repository gate's first Semgrep instrument run exited 2; a direct scan then completed with zero findings, and the complete gate retry passed with its canary detected. No tracked scanner or workflow changes were made.

The full store harness recorded 4,531 RUN entries, all 17 new `TestEphemeralDocker*LiveDB` test names as PASS, and zero FAIL or SKIP entries at every indentation level. It used the scratch-only TCP-readiness shim for the pre-existing PostgreSQL socket-readiness issue; the harness itself was unchanged. In particular:

- `TestEphemeralDockerPreferencesLiveDB` exercises the partial session updates, retained choice, tier denial, no partial rejected write and session ownership; `TestEphemeralDockerPreferenceAppliesLiveDB` and `TestEphemeralDockerPreferenceGoSQLParityLiveDB` cover the shared helper, NULL/empty membership and the unchanged claim fence.
- `TestEphemeralDockerProvisionPassLiveDB`, `TestEphemeralDockerExcludedCandidatesLiveDB` and `TestEphemeralDockerLatePlanApprovalLiveDB` cover both triggers, allowlist fallbacks, exclusions, unchanged capability-driven Docker and late Docker plan approval.
- `TestEphemeralDockerWarmStepAsideLiveDB` executes the empty-requirement/no-persistent-worker regression: plain claim refused, gap returned, Docker worker provisioned and registered, same run claimed.
- `TestEphemeralDockerWarmClaimBaselineLiveDB`, `TestEphemeralDockerNoWorkersGapBaselineLiveDB`, `TestEphemeralDockerAvailabilityLeaseAdmissionLiveDB` and `TestEphemeralDockerPlainAlternativesClaimLiveDB` prove inapplicable-preference fallback, empty-capability baseline, both availability projections, and persistent/bound admission.
- `TestEphemeralDockerSaturationLeaseMirrorsLiveDB` isolates each saturation mirror; `TestEphemeralDockerFleetSpreadLeasePeerLiveDB` isolates the peer deferral; `TestEphemeralDockerPersistentSaturationDebounceLiveDB` observes provisioning before/after the delay; `TestEphemeralDockerEarlyCapEvictionLiveDB` observes the unexpired plain lease's replacement at cap=1 and a bounded second pass.

The two health error contracts also passed focused unit tests: eligibility returns the existing allowlist error without a count; the released-worker fallback discards returned values and queries with non-nil empty membership. The real empty-list availability cases retain plain lease admission.

The exhaustive tracked lease-call sweep found seven production calls in `queries/runtime.sql`: two in `ClaimRun`, two in `CountOnlineWorkersClaimableForRun`, one in `ListUnplaceableQueuedRunsForEphemeral`, and two in `ListSaturationQueuedRunsForEphemeral`. All seven carry the new condition inside their lease alternative, with seven generated copies. Direct calls in `ephemeral_lease_livedb_test.go` and `ephemeral_docker_preference_livedb_test.go` intentionally test the unchanged thirteen-argument helper itself; the migration's definition/drop signature and explanatory references are not placement callers.

Mutation calibration used two fresh exports of committed `2d546df0` and real throwaway databases. The unmodified named warm-step-aside control passed. Removing only the claimant preference conjunct from the executed generated `claimRun` SQL preserved the peer condition and compiled successfully; the named test then failed with **"plain warm worker claims when it should refuse"**. Both exports were removed; the shared checkout was never mutated. The later credential-helper extraction retained this assertion and passed the focused and full harness runs.

The Workers page was inspected in Chromium mock mode against the Proposal, including saved preference retention after reload. Web tests cover missing `docker_enabled` from an old API, visibility/layout, pending success/error rollback, off-state retention, mock persistence and confirmed-user reconciliation. The explicit chart target passed default, tier-on, tier-off, hosting-off and controller-off renders. It remains standalone, outside CI wiring. Docker controller pod shape is reused; no hosted Kubernetes runtime smoke was added.

## Decision Log

| # | Decision | Reason | Rejected |
|---|----------|--------|----------|
| D1 | A per-user preference, not a per-run or per-repo one | The user's explicit ask: mirror the persistent form's checkbox. Per-repo already exists (Repos → Tools) and needs foreknowledge per repo. | Copy-only nudge toward the repo hint (does not change behaviour); moving a gated run to a Docker worker (lifecycle change, own PRD). |
| D2 | Warm non-Docker leased workers stop claiming new runs when the preference applies to their explicitly allowlisted repository | Otherwise a warm plain worker serves the next same-branch run for up to the lease length and the reported plan-gate failure recurs. | "Applies to newly provisioned workers only": simpler, but does not reliably fix the reported failure. Chosen by the user (option a). |
| D3 | Exclude isolated-lane and `job` runs from the preference | Both have non-bypassable non-Docker claim rules (`ProvisionPass`'s lane guard, `ClaimRun`'s job clause); a Docker worker bound to them could never claim. | Applying it to all triggers (strands runs). |
| D4 | Gate enabling on an api env var derived from the chart's docker tier; disabling always allowed | The api has no view of the tier today; a preference that provisions pods the controller cannot render would silently strand runs. Disabling must work after an operator removes the tier. | A new admin toggle (more surface, same information as the chart); no gate (stranded runs). |
| D5 | A persisted checkbox, not the `Toggle` switch | The user asked for a checkbox like the persistent form's. The component's switch-vs-checkbox convention comment is updated. | `Toggle` (house convention for persisted settings). |
| D6 | Controls on one row, one shared paragraph; drop "experimental", the cold-start size and "rootless" | Agreed UX review: the nested, indented checkbox read badly; the user reports ephemeral workers work well; rootless is not guaranteed (`rootless: false` is a supported chart posture). | Nested checkbox with its own caveat paragraph (first mock). |
| D8 | With the preference applicable to a non-null `repo_id` in the admin allowlist, a capability-free run that no online worker can claim is a gap candidate (immediate provisioning) | Excluding plain leased workers otherwise strands such a run: neither trigger fires. The opt-in JOIN, tier, allowlist membership, cap and job/lane exclusions still bound it. Allowlist read errors discard returned values and mean empty membership: no preference-only gap widening or plain-lease step-aside. `fn_worker_can_claim` remains independent. | Mirroring the exclusion alone (strands the run); routing it through saturation (its `EXISTS` needs an eligible worker that does not exist). |
| D7 | No CLI verb | The existing ephemeral opt-in has none; a settings-verb family is its own surface. | Adding `uzi settings …` here. |
| D9 | Scope the preference to non-null `repo_id` membership in the admin's `docker_repo_allowlist`; fail closed on read errors | Docker preference must respect admin repository scope. Discard values returned alongside an error and treat membership as empty for provisioning, D8 and lease step-aside. No repo-less judge preference exception; capability-driven Docker and the existing `fn_worker_can_claim` fence stay unchanged. | User preference bypassing the allowlist; reusing the claim fence's repo-less judge exception for preference; trusting values returned with an error. |
| D10 | Add non-STRICT `fn_ephemeral_docker_preference_applies`; preserve the old `fn_ephemeral_lease_admits` signature and body | One predicate governs D8 and the seven lease call sites; explicit false on NULL repository or membership prevents NULL admission logic from stranding runs. The helper is additive and does not alter the claim fence. | Changing the lease signature; a STRICT helper that returns NULL for a repo-less run; seven hand-copied predicates. |
| D11 | Keep compatibility within the existing singleton/Recreate api model | Missing `docker_enabled` from old api config hides the new checkbox; old web `{enabled}` writes remain valid partial updates. A `{docker}` write rejected after api rollback restores the UI value and displays the error. | Assuming concurrent api versions; requiring new fields from old clients; leaving a rejected checkbox write displayed as saved. |
