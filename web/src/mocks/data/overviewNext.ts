import type { Run } from "../../lib/api";
import { minsAgo, minsAhead } from "./time";
import { mockRuns } from "./runs";
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
