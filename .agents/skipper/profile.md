# Skipper profile

- Claude handover threshold: 55%; leave Codex buddies to auto-compact.
- Terms: run means a factory execution; land means drive its PR through
  `uzi-lander`; plan gate means approval or steering is required; RC cut means
  creating a release candidate through `uzi-release`.
- Delivery/landing skill: `uzi-lander`. Dispatch and plan steering: `uzi-watcher`.
- Resources: this repo's main branch for merges; the GitHub account's shared CI
  capacity for CI admission. Derive identities from the remote and operational
  reports; keep live owners and resource identifiers in private state.
- Hold triggers: the release lead requests a merges barrier while cutting an RC;
  request a separate pushes-ci barrier when the cut tip's CI is queued. Account
  CI capacity is shared across repositories; include other affected sessions.
- Coverage: include every merging lead and outstanding operation, and identify
  unenrolled actors and automation. Require quiescent acknowledgments before
  activating a barrier. Hold through the release lead's explicit release. Follow
  `uzi-release`: require successful main-push CI and KinD smoke on the exact
  pre-cut tip; queued, running, missing, cancelled or failed evidence blocks
  a cut. Publication must prove the tagged SHA or at most two strictly metadata
  hops to green coverage; tagged checks and real tagged smoke still apply.
- Irreversible exceptions: a release cut needs no buddy only under the user's
  explicit grant in the release lead; keep tag-push authorization and all
  `uzi-release` coverage gates.

## Patrol

- Interval: 30m.
- Scope: this repo's top-level PR-producing/updating delivery runs, including
  issue runs (autopilot/sweeps), `self_improve`, MR-output `prompt` schedules,
  `mr_rework` and `ci_fix`. Exclude repo-less `job`/`chat` runs. Report-only
  `judge`/`cross_check` children inherit the parent's recorded steering/landing
  owner; never count them as duplicate dispatches or require their own landing
  claim. Surface a stuck child as a finding on its parent.
- Checks (read-only; report changes to the owner, or the user when unowned):
  - Read `uzi run list --json` and the repo's landing board with
    `.agents/skills/uzi-lander/scripts/claims.sh list --json`. Every active delivery run
    needs a live landing owner: `run-<id>` before its PR, `#<PR>` after takeover.
    Check claim liveness; unreadable or unverified claims remain unknown.
  - Compare active runs by repository and issue against board claims and the
    skipper ledger. Flag a second run for an issue another session owns; route
    to both owners. Missing issue identity or incomplete visibility is unknown.
  - Flag runs reported stalled or unhealthy, or parked at an approval/input gate,
    when no accountable steering session is recorded. Use run health/state and
    owner reports; read no working transcript or partial plan.
  - Read open PRs with `gh pr list --state open --json number,url,headRefOid`;
    flag a PR without a live `#<PR>` landing claim.
  - Check the current main tip's CI using the landing/release skills' operational
    checks; report red CI to its owner. Keep pending or missing evidence distinct
    from failure; inspect no workflow code during patrol.
  - Compare queued runs with `uzi worker list --json`: flag full eligible online
    capacity from `active_runs` and `max_concurrent_runs`, accounting for harness,
    tier and repo eligibility. Missing eligibility or capacity fields are unknown.
    Also compare `active_cross_checks` / `max_cross_check_slots`; surface a child waiting on cross-check slots on its parent.
  - Read `uzi schedule list --json`; flag fired one-shot schedules that linger.
    Verify their single-fire state from schedule timing/history; do not delete
    schedules or classify cron schedules as spent one-shots.
