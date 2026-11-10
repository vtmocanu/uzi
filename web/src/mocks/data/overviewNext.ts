import type { Run, RunMessage, Worker } from "../../lib/api";
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
//   - More other-user runs for the issue-work line (PRD #3879): one planning, one asking, one
//     Other work (a prompt run). With the base fixtures and the viewer's own planning, revising,
//     account-cooldown and ci_fix runs below, every stage of the line holds at least one run.
//   - One of the viewer's OWN completed issue runs whose open MR shares repo_id + mr_iid with
//     run-ci-halted, so the pull-request list has a duplicate to collapse.
//   - One of the viewer's OWN runs waiting on a question (PRD #4040, the mock's #409), whose
//     pending-question excerpt the owner run list carries (overviewNextPendingQuestions).

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
  "run-ov-lena-planning": "lena@uzi.local",
  "run-ov-omar-asking": "omar@uzi.local",
  "run-ov-ines-prompt": "ines@uzi.local",
};

const otherRuns: Run[] = [
  otherUserRun({
    id: "run-ov-priya-approval",
    issue_iid: 88,
    issue_title: "Cache the board column counts",
    status: "awaiting_approval",
    updated_at: minsAgo(55),
    // #4082: no checker could run on its worker, so the plan went to its owner's gate.
    plan_cross_check_required: true,
    plan_cross_check_gate_reason: "worker_unsupported",
  }),
  otherUserRun({
    id: "run-ov-sam-running",
    issue_iid: 91,
    issue_title: "Paginate the audit log export",
    status: "running",
    code_cross_check_required: true,
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
  otherUserRun({
    id: "run-ov-lena-planning",
    issue_iid: 97,
    issue_title: "Add a dry-run flag to the importer",
    status: "running",
    plan_cross_check_required: true,
    is_planning: true,
    worker_id: "w-mira",
    claimed_at: minsAgo(6),
    started_at: minsAgo(6),
  }),
  otherUserRun({
    id: "run-ov-omar-asking",
    issue_iid: 99,
    issue_title: "Decide the retention window for audit rows",
    status: "awaiting_input",
    updated_at: minsAgo(20),
  }),
  otherUserRun({
    id: "run-ov-ines-prompt",
    issue_iid: null,
    issue_title: "Summarise last week's failed runs",
    kind: "prompt",
    branch: null,
    status: "running",
    worker_id: "w-mira",
    claimed_at: minsAgo(3),
    started_at: minsAgo(3),
  }),
];

// The viewer's own runs for the issue-work line: one planning its first plan with the plan
// cross-check on (#4082), one re-planning after a revise (both count under Planning, not at
// the gate), one holding for an account cooldown (Parked, though its status is running), and
// one CI fix (Other work).
const queuedBase = mockRuns.find((r) => r.id === "run-queued")!;

function ownRun(over: Partial<Run> & Pick<Run, "id" | "issue_iid" | "issue_title" | "status">): Run {
  return {
    ...queuedBase,
    issue_web_url: null,
    model: null,
    worker_id: null,
    created_at: minsAgo(40),
    updated_at: minsAgo(4),
    ...over,
  };
}

const ownLineRuns: Run[] = [
  // #4082: mid first turn, no plan yet; its first plan is with the checker (the seed below).
  ownRun({
    id: "run-ov-own-planning",
    issue_iid: 95,
    issue_title: "Warn before a run's budget runs out",
    status: "running",
    is_planning: true,
    iteration_count: 0,
    plan_md: null,
    plan_cross_check_required: true,
    branch: "agent/issue-95",
    worker_id: mockWorkers[0].id,
    claimed_at: minsAgo(9),
    started_at: minsAgo(9),
  }),
  ownRun({
    id: "run-ov-own-revising",
    issue_iid: 96,
    issue_title: "Show the cost cap on the run page",
    status: "awaiting_approval",
    is_revising: true,
    worker_id: mockWorkers[0].id,
    claimed_at: minsAgo(30),
    started_at: minsAgo(30),
  }),
  ownRun({
    id: "run-ov-own-cooldown",
    issue_iid: 98,
    issue_title: "Batch the webhook retries",
    status: "running",
    health: "waiting_account_cooldown",
    worker_id: mockWorkers[0].id,
    claimed_at: minsAgo(15),
    started_at: minsAgo(15),
  }),
  ownRun({
    id: "run-ov-own-cifix",
    issue_iid: null,
    kind: "ci_fix",
    issue_title: "Fix the lint job on the cost-cap branch",
    status: "running",
    worker_id: mockWorkers[0].id,
    claimed_at: minsAgo(5),
    started_at: minsAgo(5),
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

// PRD #4040: the viewer's own run asking a question, #409 in the design mock.
const OWN_ASKING_QUESTION = "Should the cap count exports that are still queued?";
const ownAskingRun: Run = ownRun({
  id: "run-ov-own-asking",
  issue_iid: 409,
  issue_title: "Let admins cap concurrent exports per tenant",
  status: "awaiting_input",
  status_since: minsAgo(25),
  worker_id: mockWorkers[0].id,
  claimed_at: minsAgo(50),
  started_at: minsAgo(50),
});

// The question message behind it, in the shape the worker emits (question_id + questions).
export const overviewNextOwnAskingMessages: RunMessage[] = [
  { seq: 1, kind: "status", agent: null, agent_instance: null, agent_label: null, payload: { event: "init", model: "claude-sonnet-4-6" }, created_at: minsAgo(50) },
  {
    seq: 2,
    kind: "question",
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload: {
      question_id: "q-ov-own-asking",
      questions: [{ question: OWN_ASKING_QUESTION, header: "Queued exports" }],
    },
    created_at: minsAgo(25),
  },
];

// PRD #4040: the static seed the mock's owner run list (mockListRuns) reads for
// `pending_question`, standing in for the server's excerpt of the newest question message. It
// applies only to the caller's own awaiting_input rows, and the admin list never sets it.
// run-unreadable-question has no entry on purpose: its question payload is malformed, and the
// server gives a malformed payload no excerpt.
export const overviewNextPendingQuestions: Readonly<Record<string, string>> = {
  [ownAskingRun.id]: OWN_ASKING_QUESTION,
};

export const overviewNextRuns: Run[] = [...otherRuns, ...ownLineRuns, ownDuplicateMrRun, ownAskingRun];

// Issue #4082: the list-only `cross_check_active` value the server joins from a live checker
// child, standing in for that join. Issue runs only; mockListRuns and mockAdminListRuns read
// it under this scenario and set null on every other row. Each seeded run is in a state the
// server's live predicates can match (the mock test pins this): a plan check on a claimed or
// running run with plan_cross_check_required, a code check on a running run with
// code_cross_check_required (run-seeded gets that flag in store.ts under this scenario only).
// The chips it drives:
//   - Planning: run-ov-own-planning (yours, its first plan is being checked) and
//     run-ov-lena-planning (another user's) read "cross-checking";
//   - Building: run-seeded (yours, a base fixture, overlaid only here) and run-ov-sam-running
//     (another user's) read "cross-checking";
//   - Plan review: run-awaiting (yours, base gate reason "revise") reads "cross-check: revise"
//     and run-ov-priya-approval (gate reason "worker_unsupported") "cross-check: unavailable".
// No `kind: "cross_check"` child is seeded: the lists exclude them, as the server does.
export const overviewNextCrossCheckActive: Readonly<Record<string, "plan" | "code">> = {
  "run-ov-own-planning": "plan",
  "run-ov-lena-planning": "plan",
  "run-seeded": "code",
  "run-ov-sam-running": "code",
};

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
    // A coherent fleet release: current unless a row says otherwise.
    version: "0.87.0-rc.4",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.87.0-rc.4",
    ...over,
  };
}

const GiB = 1024 ** 3;
const gib = (n: number) => Math.round(n * GiB);
// A worker with no /nix volume and only the volumes named, in GiB (used, total). fleetBase
// carries /nix and no dind, so every row that sets disk goes through here.
function disks(over: { data?: [number, number]; dind?: [number, number] }): Partial<Worker> {
  return {
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: over.data ? gib(over.data[0]) : null,
    stats_disk_data_total_bytes: over.data ? gib(over.data[1]) : null,
    stats_disk_data_inodes: null,
    stats_disk_data_total_inodes: null,
    stats_disk_dind_bytes: over.dind ? gib(over.dind[0]) : null,
    stats_disk_dind_total_bytes: over.dind ? gib(over.dind[1]) : null,
    stats_disk_dind_inodes: null,
    stats_disk_dind_total_inodes: null,
  };
}

// Reported runs join to the viewer's own mock runs (run-task-handoff, run-seeded and
// run-mr-rework are seeded as running in every scenario; run-ov-own-* come from this file).
// "5e44c9d7-..." is a uuid-looking id with no run in the list, so it renders as "run 5e44c9d7".
const UNJOINED_RUN_ID = "5e44c9d7-3b1a-4c8e-9f20-6a7d81c4e2b0";
const reported = (run_id: string, phase = "running") => ({ run_id, phase, claim_generation: 1 });
const runDisk = (run_id: string, home: number, cache: number, truncated = false) => ({
  run_id,
  home_bytes: gib(home),
  cache_bytes: gib(cache),
  truncated,
  sampled_at: minsAgo(1),
});

export function overviewNextOwnerWorkers(): Worker[] {
  return [
    // Healthy: data volume in the warn band, two joined runs with sizes, version and uptime.
    fleetWorker({
      id: "w-ov-full-a",
      name: "base.l-highmem-3a1f",
      busy: true,
      active_runs: 2,
      max_concurrent_runs: 2,
      stats_cpu_pct: 30,
      stats_mem_bytes: 6442450944,
      stats_mem_limit_bytes: 34359738368,
      stats_source: "cgroup",
      ...disks({ data: [33.6, 48.9], dind: [15.5, 50] }),
      version: "0.87.0-rc.4",
      online_since: minsAgo(2 * 24 * 60 + 4 * 60),
      reported_runs: [reported("run-task-handoff"), reported("run-seeded")],
      run_disk: [runDisk("run-task-handoff", 14.2, 6.1), runDisk("run-seeded", 1.1, 0.4)],
    }),
    // Disk danger: dind at 91%, a held-work decision, one joined and one unjoined run.
    fleetWorker({
      id: "w-ov-full-b",
      name: "base.l-highmem-7c2e",
      busy: true,
      active_runs: 2,
      max_concurrent_runs: 2,
      stats_cpu_pct: 100,
      stats_mem_bytes: 7194132480,
      stats_mem_limit_bytes: 34359738368,
      stats_source: "cgroup",
      ...disks({ data: [22.8, 29.4], dind: [45.6, 50] }),
      custody_decisions_needed: 1,
      version: "0.87.0-rc.4",
      online_since: minsAgo(19 * 60 + 40),
      reported_runs: [reported("run-mr-rework"), reported(UNJOINED_RUN_ID)],
      run_disk: [runDisk("run-mr-rework", 9.8, 3.2), runDisk(UNJOINED_RUN_ID, 21.7, 8.5, true)],
    }),
    // Memory authority lost: a hosted ephemeral worker the api withholds guard-bound runs from.
    fleetWorker({
      id: "w-ov-memauth",
      name: "ephemeral-4b2d9e70",
      kind: "hosted",
      hosted_size: "l-highmem",
      ephemeral: true,
      busy: true,
      active_runs: 1,
      max_concurrent_runs: 2,
      memory_authority_lost_at: minsAgo(30),
      stats_cpu_pct: 41,
      stats_mem_bytes: 9663676416,
      stats_mem_limit_bytes: 34359738368,
      stats_source: "cgroup",
      ...disks({ data: [11.2, 48.9] }),
      version: "0.87.0-rc.4",
      online_since: minsAgo(95),
      reported_runs: [reported("run-ov-own-cifix")],
      run_disk: [runDisk("run-ov-own-cifix", 3.4, 1.2)],
    }),
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
      ...disks({ data: [18.4, 48.9], dind: [9.1, 50] }),
      version: "0.86.0",
      online_since: minsAgo(3 * 24 * 60),
      reported_runs: [reported("run-ov-own-cooldown")],
      run_disk: [runDisk("run-ov-own-cooldown", 5.3, 2.0)],
    }),
    // Ephemeral with a lease: idle, kept warm until the lease ends.
    fleetWorker({
      id: "w-ov-leased",
      name: "ephemeral-9e17a0d3",
      kind: "hosted",
      hosted_size: "l-highmem",
      ephemeral: true,
      max_concurrent_runs: 1,
      ephemeral_lease_expires_at: minsAhead(75),
      stats_cpu_pct: 1,
      stats_mem_bytes: 805306368,
      stats_mem_limit_bytes: 34359738368,
      stats_source: "cgroup",
      ...disks({ data: [4.2, 48.9] }),
      version: "0.87.0-rc.4",
      online_since: minsAgo(50),
    }),
    fleetWorker({ id: "w-ov-cordoned", name: "bench-cordoned", max_concurrent_runs: 1, manual_cordoned_at: minsAgo(30), manual_cordoned_by_label: "you", effective_cordoned: true, stats_cpu_pct: 2, stats_mem_bytes: 536870912, stats_mem_limit_bytes: 8589934592, stats_source: "cgroup" }),
    // Offline: last-known readings and the run it last reported.
    fleetWorker({
      id: "w-ov-offline",
      name: "ci-runner-offline",
      status: "offline",
      max_concurrent_runs: 2,
      last_heartbeat_at: minsAgo(120),
      online_since: null,
      stats_cpu_pct: 4,
      stats_mem_bytes: gib(1.4),
      stats_mem_limit_bytes: gib(16),
      stats_source: "cgroup",
      ...disks({ data: [23, 48.9] }),
      version: "0.86.1",
      upgrade_status: "outdated",
      upgrade_detail: "running 0.86.1, target 0.87.0-rc.4",
      reported_runs: [reported("run-ov-own-revising", "awaiting_approval")],
      run_disk: [runDisk("run-ov-own-revising", 2.6, 0.9)],
    }),
    fleetWorker({ id: "w-ov-nocap", name: "legacy-no-cap", max_concurrent_runs: null, active_runs: 1, busy: true, stats_cpu_pct: 12, stats_mem_bytes: 1073741824, stats_mem_limit_bytes: null, stats_source: "process" }),
    fleetWorker({ id: "w-ov-nostats", name: "quiet-worker", ...noStats }),
  ];
}
