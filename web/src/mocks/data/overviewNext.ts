import type { Run, Worker } from "../../lib/api";
import { minsAgo, minsAhead } from "./time";
import { mockRuns } from "./runs";
import { mockWorkers } from "./workers";
import { mockOtherUserRuns } from "./runHistories";

// ?mock=overview-next (PRD #3878): extra runs for the /overview/next preview, seeded only
// under that scenario so every other mock test and scenario sees the unchanged fixture set.
// Open /overview/next?mock=overview-next signed in as the demo admin.
//
//   - Other users' runs (owners below): one awaiting approval, one running, one in a usage-limit
//     wait. The existing run-mira-embed and run-andrei-queued fixtures add a running and a
//     queued one. They show up only in the admin all-users list.
//   - One of the viewer's OWN completed issue runs whose open MR shares repo_id + mr_iid with
//     run-ci-halted, so the pull-request list has a duplicate to collapse.

const template = mockOtherUserRuns[0];

function otherUserRun(over: Partial<Run> & Pick<Run, "id" | "issue_iid" | "issue_title" | "status">): Run {
  return {
    ...template,
    issue_description: "Another user's run, overview-next demo fixture.",
    worker_id: null,
    iteration_count: 0,
    branch: `agent/issue-${over.issue_iid}`,
    started_at: null,
    claimed_at: null,
    created_at: minsAgo(40),
    updated_at: minsAgo(5),
    ...over,
  };
}

export const overviewNextOtherRunOwners: Record<string, string> = {
  "run-ov-priya-approval": "priya@uzi.local",
  "run-ov-sam-running": "sam@uzi.local",
  "run-ov-jordan-limit": "jordan@uzi.local",
};

const otherRuns: Run[] = [
  otherUserRun({
    id: "run-ov-priya-approval",
    issue_iid: 88,
    issue_title: "Cache the board column counts",
    status: "awaiting_approval",
    updated_at: minsAgo(55),
  }),
  otherUserRun({
    id: "run-ov-sam-running",
    issue_iid: 91,
    issue_title: "Paginate the audit log export",
    status: "running",
    worker_id: "w-mira",
    iteration_count: 1,
    claimed_at: minsAgo(12),
    started_at: minsAgo(12),
  }),
  otherUserRun({
    id: "run-ov-jordan-limit",
    issue_iid: 94,
    issue_title: "Retry forge webhooks with backoff",
    status: "limit_wait",
    worker_id: "w-mira",
    iteration_count: 2,
    limit_resets_at: minsAhead(95),
    limit_wait_count: 1,
    rate_limit_type: "five_hour",
    claimed_at: minsAgo(70),
    started_at: minsAgo(70),
  }),
];

// The viewer's own completed run for issue 22, a second run on the same MR as run-ci-halted.
const ciHalted = mockRuns.find((r) => r.id === "run-ci-halted")!;
const ownDuplicateMrRun: Run = {
  ...ciHalted,
  id: "run-ov-ci-halted-retry",
  issue_title: "Per-run cost budget with hard stop (follow-up pass)",
  updated_at: minsAgo(30),
  finished_at: minsAgo(30),
};

export const overviewNextRuns: Run[] = [...otherRuns, ownDuplicateMrRun];

// The owner fleet for the Capacity rail (PRD #3880 M1): full, draining mid-roll, manually
// cordoned, offline, with no advertised cap and with no stats. Replaces the default seeded
// workers under this scenario only.
const fleetBase = mockWorkers[0];
const noStats = {
  stats_cpu_pct: null,
  stats_mem_bytes: null,
  stats_mem_limit_bytes: null,
  stats_source: null,
  stats_disk_nix_bytes: null,
  stats_disk_nix_total_bytes: null,
  stats_disk_data_bytes: null,
  stats_disk_data_total_bytes: null,
} satisfies Partial<Worker>;

function fleetWorker(over: Partial<Worker> & Pick<Worker, "id" | "name">): Worker {
  return {
    ...fleetBase,
    forge_origin_cooldowns: [],
    retaining_unpublished_work: false,
    custody_decisions_needed: 0,
    pending_outcome_count: 0,
    pending_overflow: false,
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: 2,
    last_heartbeat_at: minsAgo(0.2),
    online_since: minsAgo(120),
    draining_since: null,
    manual_cordoned_at: null,
    effective_cordoned: false,
    ...over,
  };
}

export function overviewNextOwnerWorkers(): Worker[] {
  return [
    fleetWorker({ id: "w-ov-full-a", name: "base.l-highmem-3a1f", busy: true, active_runs: 2, max_concurrent_runs: 2, stats_cpu_pct: 30, stats_mem_bytes: 6442450944, stats_mem_limit_bytes: 34359738368, stats_source: "cgroup" }),
    fleetWorker({ id: "w-ov-full-b", name: "base.l-highmem-7c2e", busy: true, active_runs: 2, max_concurrent_runs: 2, stats_cpu_pct: 100, stats_mem_bytes: 7194132480, stats_mem_limit_bytes: 34359738368, stats_source: "cgroup" }),
    fleetWorker({
      id: "w-ov-rolling",
      name: "base.l-highmem-b54d",
      kind: "hosted",
      hosted_size: "l-highmem",
      busy: true,
      active_runs: 1,
      max_concurrent_runs: 2,
      draining_since: minsAgo(8),
      effective_cordoned: true,
      upgrade_status: "upgrading",
      upgrade_target: "v0.86.1-rc.2",
      stats_cpu_pct: 78,
      stats_mem_bytes: 7405568000,
      stats_mem_limit_bytes: 34359738368,
      stats_source: "cgroup",
    }),
    fleetWorker({ id: "w-ov-cordoned", name: "bench-cordoned", max_concurrent_runs: 1, manual_cordoned_at: minsAgo(30), manual_cordoned_by_label: "you", effective_cordoned: true, stats_cpu_pct: 2, stats_mem_bytes: 536870912, stats_mem_limit_bytes: 8589934592, stats_source: "cgroup" }),
    fleetWorker({ id: "w-ov-offline", name: "ci-runner-offline", status: "offline", max_concurrent_runs: 2, last_heartbeat_at: minsAgo(240), online_since: null, ...noStats }),
    fleetWorker({ id: "w-ov-nocap", name: "legacy-no-cap", max_concurrent_runs: null, active_runs: 1, busy: true, stats_cpu_pct: 12, stats_mem_bytes: 1073741824, stats_mem_limit_bytes: null, stats_source: "process" }),
    fleetWorker({ id: "w-ov-nostats", name: "quiet-worker", ...noStats }),
  ];
}
