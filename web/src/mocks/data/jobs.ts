import type { Run } from "../../lib/api";
import { minsAgo } from "./time";
import { mockRuns } from "./runs";

// ── Job runs (PRD #1908) ─────────────────────────────────────────────────────
// Repo-less `job` runs: no repo, issue, branch or MR (repo_id null, forge_type ""), created
// over /api/v1/jobs by a CLI token or a product token. Two states cover the web surfaces:
// a queued job (the runs-list row and the "result appears when it finishes" panel) and a
// completed one with a result.
//
// The completed job is deliberately HOSTILE so the untrusted-render paths are visible in mock
// mode: the product-supplied requested_by_label carries an RTL override and markup, the report
// carries raw HTML, a script tag and a javascript: link, and one finding's URL is a
// javascript: URL. Every invisible character is a \u ESCAPE, never a raw byte, so this source
// file carries none. Rendered, each must read as inert text (see JobResultPanel).

const base = mockRuns.find((r) => r.id === "run-queued")!;

// Fields a job run never has: no repo, forge, issue, branch or MR; no plan gate.
const repoless: Partial<Run> = {
  repo_id: null,
  issue_iid: null,
  issue_web_url: null,
  forge_type: "",
  branch: null,
  mr_iid: null,
  mr_web_url: null,
  mr_state: null,
  plan_md: null,
  auto_approve: false,
  kind: "job",
};

const HOSTILE_LABEL = "ops\u202Etsil-kcehc <b>admin</b> (verified by uzi)";

const HOSTILE_REPORT = [
  "## Summary",
  "",
  "The three runbooks disagree on the **rollback window**: two say 30 minutes, one says 2 hours.",
  "",
  "- `deploy.md` and `oncall.md` agree on 30 minutes.",
  "- `legacy-rollback.md` still says 2 hours and predates the Q3 change.",
  "",
  "See the [change log](https://example.com/changelog) for the decision.",
  "",
  "<script>alert('report')</script>",
  "",
  "<img src=x onerror=\"alert('img')\">",
  "",
  "[Open the admin panel](javascript:alert('link'))",
].join("\n");

export const mockJobRuns: Run[] = [
  {
    ...base,
    ...repoless,
    id: "run-job-queued",
    issue_title: "Summarise the incident timeline from the attached notes",
    issue_description: "Summarise the incident timeline.",
    status: "queued",
    worker_id: null,
    budget_extension_cap_seconds: 57600,
    job: {
      type: "research",
      inputs: [
        { name: "incident-notes.md", size_bytes: 18_422 },
        { name: "pager-export.csv", size_bytes: 412 },
      ],
      origin: { requested_by_label: "dana@helpdesk", product_name: "Helpdesk assistant" },
      result: null,
    },
    created_at: minsAgo(2),
    updated_at: minsAgo(2),
    status_since: minsAgo(2),
  },
  {
    ...base,
    ...repoless,
    id: "run-job-done",
    issue_title: "Check the three runbooks for a consistent rollback window",
    issue_description: "Compare the rollback windows in the attached runbooks.",
    status: "completed",
    worker_id: "w-laptop",
    health: "ok",
    claimed_at: minsAgo(48),
    started_at: minsAgo(48),
    finished_at: minsAgo(41),
    job: {
      type: "research",
      inputs: [
        { name: "deploy.md", size_bytes: 6_140 },
        { name: "oncall.md", size_bytes: 3_902 },
        { name: "legacy-rollback.md", size_bytes: 1_210_000 },
      ],
      origin: { requested_by_label: HOSTILE_LABEL, product_name: "Helpdesk assistant" },
      result: {
        status: "completed",
        report_md: HOSTILE_REPORT,
        findings: [
          {
            severity: "error",
            message_md: "Rollback window is 2 hours here but 30 minutes in the other runbooks.",
            url: null,
            file: "legacy-rollback.md",
            line: 42,
          },
          {
            severity: "warning",
            message_md: "The on-call runbook links a dashboard that no longer exists.",
            url: "https://example.com/dashboards/rollback",
            file: null,
            line: null,
          },
          {
            severity: "info",
            message_md: "Reported link <script>alert('finding')</script> [click](javascript:alert(1))",
            url: "javascript:alert(document.cookie)",
            file: null,
            line: null,
          },
        ],
      },
    },
    created_at: minsAgo(49),
    updated_at: minsAgo(41),
    status_since: minsAgo(41),
  },
];
