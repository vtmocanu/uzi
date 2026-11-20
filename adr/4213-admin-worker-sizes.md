# ADR-4213: Admin-defined hosted worker sizes

**Status**: Accepted
**Date**: 2026-11-19
**PRD**: [PRD #4213](../prds/4213-admin-worker-sizes.md)

## Context

Hosted worker sizes (`s`, `m`, `l`, `l-highmem`) were Go constants in the
controller (`controller/internal/preset/preset.go`, the `sizes` map). Every change
to a size's CPU, memory or disk, and every new size, needed a code change and a
release. [PRD #58](../prds/done/58-hosted-k8s-workers.md) Decision 7 set the rule
behind that: the api sends the size name and the controller owns every mapping to
a pod-spec value. PRD #58 deferred configurable presets because the api cannot
validate a Kubernetes quantity (`k8s.io/apimachinery` is banned from the api by
`api/internal/hostedsvc/no_kube_dependency_test.go`) and because a bad value
could strand a worker with no error anywhere.

## Decision

Reverse Decision 7 for sizes only. The api now owns the size definitions, and the
controller still renders them.

- **Plain integers, never quantity strings.** The api stores sizes in the table
  `worker_sizes` as `cpu_request_millicores`, `cpu_limit_millicores`,
  `memory_request_mib`, `memory_limit_mib`, `data_gib` and `dind_data_gib`.
  Validation is integer arithmetic, so the no-kube-dependency guard stays
  untouched.
- **Builtins.** `s`, `m`, `l` and `l-highmem` are seeded at boot with the shipped
  values (the migration carries no seed rows) and refreshed when still pristine,
  following the builtin agent-template pattern. An edited builtin is never
  overwritten, can be reset to shipped values, and is never deletable. Admins may
  add further sizes (names match `^[a-z][a-z0-9-]{0,15}$`, at most 16 in total).
- **Wire.** The integers ride inline on each `DesiredWorker` (`resources`,
  `omitempty`) beside the size name, so the poll stays full desired state with no
  separate table or digest. The controller builds quantities from the integers
  with fixed formats: CPU as DecimalSI millicores, memory and storage as
  BinarySI. Fixed formats keep the builtin spec-hashes byte-identical to the
  compiled-in presets, so deploying does not roll the fleet. The compiled-in
  values apply only to a `DesiredWorker` that carries no integers, never per
  field, so an edited size cannot flip between edited and compiled-in specs.
- **Ceilings.** The poll is a GET with no body, so ceilings reach the api as
  chart-rendered env from the same values that feed the namespace LimitRanges,
  per tier. The api takes the minimum over enabled tiers and names the chart value
  in a refusal. The chart converts quantities to integers and rounds down, so a
  ceiling is never overstated. ResourceQuota is advisory only: a zero quota value
  is accepted (render and boot never fail on it), and a missing or unparsable
  quota is shown as "unavailable", never as unlimited or as fitting. Ceilings
  themselves keep strict parsing and refusal.
- **Controller refusal stays.** The controller still refuses, at render, a size
  above its own env ceilings and reports it through the status report's
  `BlockingKind` (`size_refused`, `pvc_rejected`, and the existing roll phase
  `stuck`). The channel is display-only.
- **Skew rule (api first, [ADR-1470](1470-paced-hosted-worker-rolls.md) D7).** The
  api refuses every size mutation (add, edit, reset, delete) unless the latest
  fleet status report advertises `admin_sizes` at version 1 or higher and was
  received within twice the controller poll interval on the api's own clock. A
  stale, absent or version-0 report makes the editor read-only. A new controller
  with an old api sees no integers and uses compiled-in values. Downgrading the
  api or the controller after a customization is unsupported: workers fall back to
  compiled-in defaults and custom-size workers stay pending.
- **Templates stay name-only** and controller-resolved. This is the remaining half
  of PRD #58's authority rule.

### Concurrency

- One transaction-scoped advisory lock serializes every size mutation, so the
  16-size maximum and name uniqueness are checked against committed state.
- Delete takes `FOR UPDATE` on its row, then reads in-use state in separate
  queries.
- Every write that references a size (both hosted-worker provision inserts and
  both default-size settings writes) takes `FOR SHARE` on that row inside its own
  transaction and checks membership against the database, not the cache.
  `FOR KEY SHARE` was considered; it would not block edits to the row, so it is
  the recorded alternative.
- There is no foreign key from `workers.hosted_size`. The migration has no seed
  rows, so a key would not hold before boot seeding, and the default-size settings
  in `app_settings` could not use one anyway and need row locks regardless. The
  PRD's soft-delete rationale for rejecting a key is wrong: worker rows are
  hard-deleted.

## Consequences

- An admin can change a size without a release. A quantity change alters the
  rendered pod template and its spec-hash, which the existing paced drift arm
  already keys on, so no new roll trigger exists. Disk is not in the template.
- Lowering a desired disk size affects only claims created afterwards; existing
  claims never shrink ([ADR-3890](3890-worker-pvc-expansion.md)). Reset and the
  pristine refresh may therefore lower a desired size.
- If replicas with different shipped values run during a rolling deploy, the
  pristine refresh can flip-flop between them and roll the fleet twice. The risk
  is low and bounded by the deploy window.
- A size the controller refuses leaves that worker in roll pacing's `unplanned`
  set, which holds canary hibernation completion for its namespace until the
  worker renders.
- The startup refusal (`ValidatePVCCeilings`) still covers compiled-in builtins
  only. An operator who lowers a chart ceiling below a shipped default still stops
  the controller from starting, even if an admin customized that builtin to fit,
  so operators keep chart ceilings at or above the shipped defaults. Runtime sizes
  are covered by the render-time check.
- Web, TUI and CLI read the size list and resolved integers from the api instead
  of mirroring constants. The goldens that pinned the builtin values are replaced
  by a contract over the seed values and the `DesiredWorker` wire shape.
