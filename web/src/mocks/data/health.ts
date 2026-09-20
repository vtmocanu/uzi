import type { AdminWorker, HealthCheck, HealthDoc } from "../../lib/api";
import { minsAgo } from "./time";

// Admin-health fixtures (PRD #1484 M4). These stand in for GET /api/admin/health in mock
// mode and back the Health-tab component tests. They mirror the real registry
// (api/internal/healthsvc): the SAME 14 check ids, in the SAME stable order the server
// emits (service.go Evaluate), so the demo and the tests exercise a well-formed document.
//
// Summaries here are illustrative demo copy, not byte-for-byte the server's templates — the
// server composes them from live numbers. Severity, id, group and shape are what must match.

// The fixed per-id metadata, in the server's emit order (Evaluate). group/title/doc mirror
// checkMeta in api/internal/healthsvc/checks.go.
const CHECK_META: { id: string; group: string; title: string; doc: string | null }[] = [
  { id: "fleet.roll", group: "workers", title: "Worker image roll", doc: "worker-upgrades" },
  { id: "fleet.capacity", group: "workers", title: "Worker capacity", doc: "hosted-workers" },
  { id: "fleet.disk", group: "workers", title: "Worker disk", doc: "hosted-workers" },
  { id: "queue.waiting", group: "queue", title: "Runs waiting for a worker", doc: null },
  { id: "queue.undispatched", group: "queue", title: "Undispatched task runs", doc: null },
  { id: "controller.report", group: "control", title: "Controller reporting", doc: "hosted-workers" },
  { id: "db", group: "control", title: "Database", doc: null },
  { id: "loops", group: "control", title: "Background loops", doc: null },
  { id: "forge.ciwatch", group: "integrations", title: "CI watch capacity", doc: null },
  { id: "slack.socket", group: "integrations", title: "Slack socket", doc: null },
  { id: "schedules.paused", group: "housekeeping", title: "Paused schedules", doc: null },
  { id: "board.drift", group: "housekeeping", title: "Board drift", doc: null },
  { id: "custody.holds", group: "housekeeping", title: "Recovery custody holds", doc: null },
  { id: "release.check", group: "housekeeping", title: "Upstream release", doc: null },
];

// The all-ok summary per id, so the healthy path reads like the server's green templates.
const OK_SUMMARY: Record<string, string> = {
  "fleet.roll": "All 4 hosted workers are rolling cleanly.",
  "fleet.capacity": "Every owner with queued work has a usable worker.",
  "fleet.disk": "No worker is under sustained disk pressure.",
  "queue.waiting": "No run is waiting for a worker.",
  "queue.undispatched": "No task run is stuck undispatched.",
  "controller.report": "The controller is reporting.",
  db: "Database reachable (2ms); schema at head.",
  loops: "All 4 background loops are ticking.",
  "forge.ciwatch": "Every repo's run branches fit within the CI watch cap.",
  "slack.socket": "Slack socket is connected.",
  "schedules.paused": "No user has paused schedules while owning enabled ones.",
  "board.drift": "No column move has been given up in the last 24 hours.",
  "custody.holds": "No owner is at the custody-hold admission limit.",
  "release.check": "This instance is on a recent release.",
};

// Fields a fixture overrides for a non-ok (or otherwise reshaped) check.
type CheckOverride = Partial<Pick<HealthCheck, "severity" | "summary" | "since" | "evidence" | "action" | "command">>;

function build(overrides: Record<string, CheckOverride>): HealthCheck[] {
  return CHECK_META.map((m) => ({
    id: m.id,
    group: m.group,
    title: m.title,
    severity: "ok",
    summary: OK_SUMMARY[m.id] ?? "",
    since: null,
    evidence: [],
    action: null,
    command: null,
    doc: m.doc,
    ...overrides[m.id],
  }));
}

function tally(checks: HealthCheck[]): HealthDoc["counts"] {
  const counts = { ok: 0, warn: 0, danger: 0, unknown: 0, na: 0 };
  for (const c of checks) {
    if (c.severity === "warn") counts.warn++;
    else if (c.severity === "danger") counts.danger++;
    else if (c.severity === "unknown") counts.unknown++;
    else if (c.severity === "na") counts.na++;
    else counts.ok++;
  }
  return counts;
}

// overall is the worst check: danger, then warn (unknown ranks as warn). Mirrors
// overallStatus in service.go — na never contributes.
function overall(counts: HealthDoc["counts"]): string {
  if (counts.danger > 0) return "danger";
  if (counts.warn > 0 || counts.unknown > 0) return "warn";
  return "ok";
}

function doc(checks: HealthCheck[], over: Partial<HealthDoc> = {}): HealthDoc {
  const counts = tally(checks);
  return {
    status: overall(counts),
    checked_at: new Date().toISOString(),
    counts,
    snoozed_until: null,
    episode_id: null,
    checks,
    ...over,
  };
}

// ── Named scenarios ───────────────────────────────────────────────────────────

// health-silent (and the default): every check passing.
export function healthySilentDoc(): HealthDoc {
  return doc(build({}));
}

// health-degraded: two warnings, nothing blocked. Work still flows.
export function degradedDoc(): HealthDoc {
  return doc(
    build({
      "forge.ciwatch": {
        severity: "warn",
        summary: "1 repo has more run branches than the CI watch cap of 20, so some go unwatched.",
        since: minsAgo(60 * 24 * 3),
        evidence: [
          { label: "Repos over the cap", value: "1" },
          { label: "Busiest repo", value: "acme/factory" },
          { label: "Eligible branches", value: "27 of 20 watched" },
        ],
        action: "Raise CI_WATCH_MAX_REFS, or close finished run branches, so every run branch is watched.",
      },
      "schedules.paused": {
        severity: "warn",
        summary: "1 user has paused all schedules while owning enabled schedules.",
        since: minsAgo(60 * 24 * 6),
        evidence: [{ label: "Users", value: "1" }],
        action: "Their labelled issues look queued but will not run until they unpause.",
      },
    }),
  );
}

// health-incident: the motivating incident — the fleet is pinned to a worker image that was
// never published, so three checks share one cause and uzi cannot run work. A danger episode
// is open, so episode_id is non-null (the banner/snooze machinery reads it in M5).
export function incidentDoc(): HealthDoc {
  return doc(
    build({
      "fleet.roll": {
        severity: "danger",
        summary: "4 of 4 hosted workers stuck rolling to 0.84.0-rc.2: ImagePullBackOff",
        since: minsAgo(38),
        evidence: [
          { label: "Target tag", value: "0.84.0-rc.2" },
          { label: "Blocking container", value: "worker" },
          { label: "Blocking reason", value: "ImagePullBackOff" },
          { label: "Worker", value: "base.l-7c2e" },
          { label: "Stuck workers", value: "4 of 4" },
        ],
        action: "Publish the image, or release a chart whose workers.image.tag names a published tag.",
        command: "kubectl -n <worker-namespace> describe pod -l uzi.dev/hosted-worker-id=<id>",
      },
      "fleet.capacity": {
        severity: "danger",
        summary: "2 owner(s) have queued runs and no usable worker (oldest waiting 36m).",
        since: minsAgo(36),
        evidence: [{ label: "Owners affected", value: "2" }],
        action: "Recover or provision a worker for the affected owners, or check fleet.roll for stuck pods.",
        command: "kubectl -n <worker-namespace> get pods",
      },
      "queue.waiting": {
        severity: "danger",
        summary: "A run has been waiting for a worker for 34m.",
        since: minsAgo(34),
        action: "Check fleet.capacity and fleet.roll — a stuck roll or zero capacity is the usual cause.",
      },
      "forge.ciwatch": {
        severity: "warn",
        summary: "1 repo has more run branches than the CI watch cap of 20, so some go unwatched.",
        since: minsAgo(60 * 24 * 3),
        evidence: [
          { label: "Repos over the cap", value: "1" },
          { label: "Busiest repo", value: "acme/factory" },
          { label: "Eligible branches", value: "27 of 20 watched" },
        ],
        action: "Raise CI_WATCH_MAX_REFS, or close finished run branches, so every run branch is watched.",
      },
    }),
    { episode_id: "b1f0c2ep" },
  );
}

// ── Extra states for the component tests (not named scenarios) ──────────────────

// unknown: a silent controller and a disabled run-health detector leave several checks
// unknown — a stale/absent signal reads Unknown, never green (D6). Overall is warn (unknown
// ranks as warn), never danger.
export function unknownDoc(): HealthDoc {
  return doc(
    build({
      "controller.report": {
        severity: "unknown",
        summary: "The controller's last report is 4m old (a few intervals late).",
        since: minsAgo(4),
      },
      "fleet.roll": {
        severity: "unknown",
        summary: "No fresh controller signal for the hosted fleet; roll health cannot be determined.",
        action: "Check that the worker controller is running and reporting.",
      },
      "fleet.capacity": {
        severity: "unknown",
        summary: "The run-health detector is disabled, so this signal is unavailable.",
      },
      "queue.waiting": {
        severity: "unknown",
        summary: "The run-health detector is disabled, so this signal is unavailable.",
      },
    }),
  );
}

// na: a compose stack with no hosted workers and no Slack — the Kubernetes/Slack-only checks
// are Not applicable, never green and never gone, and the overall verdict can still be ok.
export function noHostedWorkersDoc(): HealthDoc {
  return doc(
    build({
      "fleet.roll": { severity: "na", summary: "No hosted workers are configured on this deployment." },
      "controller.report": {
        severity: "na",
        summary: "No hosted workers are configured, so the controller does not report.",
      },
      "slack.socket": { severity: "na", summary: "Slack is not configured." },
    }),
  );
}

// ── Fleet, all users (incident) ─────────────────────────────────────────────────

// The stuck hosted fleet the incident scenario shows in the cross-user table: two owners,
// every hosted worker upgrade_failed on an unpullable image, so the Blocking/Upgrade columns
// (the ones the admin list lacked before this PRD) are populated across users.
function stuckHosted(over: Partial<AdminWorker>): AdminWorker {
  return {
    id: "w-stuck",
    name: "base.l-7c2e",
    status: "offline",
    kind: "hosted",
    hosted_size: "l",
    docker: false,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    version: "0.83.1",
    upgrade_status: "upgrade_failed",
    upgrade_detail: "worker: ImagePullBackOff",
    upgrade_target: "0.84.0-rc.2",
    upgrade_blocking_container: "worker",
    upgrade_blocking_reason: "ImagePullBackOff",
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(38),
    online_since: null,
    created_at: minsAgo(60 * 11),
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
    owner_email: "user.a@uzi.local",
    ...over,
  };
}

export function incidentFleetWorkers(): AdminWorker[] {
  return [
    stuckHosted({ id: "w-inc-1", name: "base.l-7c2e", owner_email: "user.a@uzi.local" }),
    stuckHosted({ id: "w-inc-2", name: "base.l-19fd", owner_email: "user.a@uzi.local" }),
    stuckHosted({ id: "w-inc-3", name: "base.m-40ab", owner_email: "user.c@uzi.local" }),
    stuckHosted({ id: "w-inc-4", name: "base.l-e215", owner_email: "user.c@uzi.local" }),
  ];
}

// ── Fleet, all users (healthy) ──────────────────────────────────────────────────

// The healthy hosted fleet the silent (all-ok) and degraded (warn) scenarios show: every
// worker online and up to date across three owners, so the cross-user table AGREES with the
// "all normal" / "warnings only, nothing is blocked" verdict rather than showing a stuck
// upgrade the verdict says nothing about. In production both endpoints read one DB and so
// always agree; this only keeps the DEMO scenarios coherent. No upgrade_failed row here.
function healthyHosted(over: Partial<AdminWorker>): AdminWorker {
  return {
    id: "w-ok",
    name: "base.l-7c2e",
    status: "online",
    kind: "hosted",
    hosted_size: "l",
    docker: false,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: 2,
    template_declared: "base",
    template_reported: "base",
    version: "0.84.0",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.84.0",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(1),
    online_since: minsAgo(60 * 11),
    created_at: minsAgo(60 * 11),
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
    owner_email: "user.a@uzi.local",
    ...over,
  };
}

export function healthyFleetWorkers(): AdminWorker[] {
  return [
    healthyHosted({ id: "w-ok-1", name: "base.l-7c2e", owner_email: "user.a@uzi.local" }),
    healthyHosted({ id: "w-ok-2", name: "base.l-19fd", owner_email: "user.a@uzi.local" }),
    healthyHosted({ id: "w-ok-3", name: "base.m-40ab", busy: true, active_runs: 1, owner_email: "user.b@uzi.local" }),
    healthyHosted({ id: "w-ok-4", name: "base.l-e215", owner_email: "user.c@uzi.local" }),
  ];
}
