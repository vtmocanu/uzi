import {
  type CreatedIssue,
  type IssueDraft,
  type IncidentalFinding,
  type IncidentalFindingBacklog,
  type IncidentalFindingBucket,
  type IncidentalFindingFileResult,
  type IncidentalFindingIssueDraft,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { recommendationLabel, verdictLabel } from "../../lib/judge";
import { mockFindings, type MockFinding, type MockReview } from "../data";
import { getRun } from "../store";
import { delay, requireSession } from "./shared";
import { repos } from "./forge";
import { reviews } from "./judge";

// Incidental-findings coordinates (PRD #333 M7). Mutable copy so file/dismiss persist in a
// demo session; the seed stays pristine so a module reload re-seeds a clean backlog.
const findings: MockFinding[] = mockFindings.map((f) => ({ ...f, labels: [...f.labels], run_ids: [...f.run_ids] }));

// findingDTO projects a mock coordinate to the wire DTO, omitting the optional keys exactly as
// the server's `omitempty` tags do (a null finding_id / iid / resolved_at is simply absent).
function findingDTO(f: MockFinding): IncidentalFinding {
  return {
    ...(f.finding_id ? { finding_id: f.finding_id } : {}),
    location: f.location,
    repo_id: f.repo_id,
    repo_path: f.repo_path,
    status: f.status,
    last_title: f.last_title,
    seen_in_runs: f.seen_in_runs,
    ...(f.filed_issue_iid != null ? { filed_issue_iid: f.filed_issue_iid } : {}),
    ...(f.filed_issue_url ? { filed_issue_url: f.filed_issue_url } : {}),
    ...(f.resolved_at ? { resolved_at: f.resolved_at } : {}),
  };
}

// matchFindingBucket maps a disposition status to the ?bucket= filter (D7): to_file shows only
// open, filed/dismissed show their own status, all shows everything (the transient `filing` is
// invisible to to_file, exactly like the server).
function matchFindingBucket(status: MockFinding["status"], bucket: IncidentalFindingBucket): boolean {
  switch (bucket) {
    case "to_file":
      return status === "open";
    case "filed":
      return status === "filed";
    case "dismissed":
      return status === "dismissed";
    case "all":
      return true;
    default:
      return false;
  }
}

// Monotonic iid for issues the preview files (PRD #68), above the seeded #71.
let nextFiledIssueIid = 90;

// mockIssueDraft mirrors the server's deterministic templating (PRD #68 M2): the
// category→repo default resolved against the connected repos (an empty default → mock
// state D), the fenced body, the server-side `uzi` label (PRD #764), and a provenance
// line. Faithful enough for the preview to render every state, not a byte-for-byte copy
// of the Go renderer (its fence/strip/scan is unit-tested there).
function mockIssueDraft(
  runId: string,
  rec: MockReview["recommendations"][number],
  review: MockReview,
): IssueDraft {
  const label = recommendationLabel(rec.category);
  const enabledRepoIds = new Set(repos.filter((r) => r.enabled).map((r) => r.id));
  let default_repo_id = "";
  let default_note = "";
  if (rec.category === "improve_agent" || rec.category === "add_agent") {
    const rid = getRun(runId)?.repo_id ?? "";
    if (enabledRepoIds.has(rid)) {
      default_repo_id = rid;
      default_note =
        "Defaulted to the judged run's repo — repo agents live in its .claude/agents/. Pick any repo you have connected.";
    } else {
      default_note = "The judged run's repo isn't one you've connected. Pick the repo to file this against.";
    }
  } else {
    // PRD #590 M2: the uzi-own-repo default now comes from the caller's enabled
    // self_improve default schedule (server-side); the preview does not model that
    // schedule, so it renders mock state D (no default, pick a repo).
    default_note =
      "No uzi repo is configured on this instance (or it isn't one you've connected), so there's no default. Pick the repo to file this against.";
  }
  const description = [
    "## What the judge found",
    "",
    "````",
    rec.rationale_md,
    "````",
    "",
    "## Context",
    "",
    `- Recommendation: **${label}**${rec.target ? " — `" + rec.target + "`" : ""}${
      rec.confidence ? ` (${rec.confidence} confidence)` : ""
    }`,
    `- Verdict on the judged run: **${verdictLabel(review.verdict)}**`,
    "",
    "## Judge's summary of the run",
    "",
    "````",
    review.summary_md,
    "````",
    "",
    "---",
    "Opened by uzi on behalf of @vlad, from a run retrospective. The quoted text above is LLM-authored and unverified.",
  ].join("\n");
  return {
    default_repo_id,
    title: rec.target ? `${label}: ${rec.target}` : label,
    description,
    labels: ["uzi"],
    provenance: `from vlad's worker, run ${runId.slice(0, 8)}`,
    default_note,
  };
}

export const findingsApi = {
  // ── Incidental Findings backlog (PRD #333 M7) ───────────────────────────────
  listFindings: async (bucket: IncidentalFindingBucket = "to_file", repo?: string, run?: string) => {
    const me = requireSession();
    const mine = findings.filter((f) => f.user_id === me.id);
    const byRepo = repo ? mine.filter((f) => f.repo_id === repo) : mine;
    // open_count is the D8 nav-badge count: open coordinates scoped by the ?repo= filter but NOT
    // the bucket or run — so it is stable across a tab switch, exactly like the server meta.
    const openCount = byRepo.filter((f) => f.status === "open").length;
    const byRun = run ? byRepo.filter((f) => f.run_ids.includes(run)) : byRepo;
    const rows = byRun.filter((f) => matchFindingBucket(f.status, bucket)).map(findingDTO);
    const backlog: IncidentalFindingBacklog = {
      bucket,
      repo: repo ?? "",
      run: run ?? "",
      open_count: openCount,
      findings: rows,
    };
    return delay(backlog, 80);
  },
  findingIssueDraft: async (id: string) => {
    const me = requireSession();
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    const draft: IncidentalFindingIssueDraft = {
      title: f.last_title,
      description: f.description_md,
      location: f.location,
      labels: [...f.labels],
      provenance: `Found by a run while working on ${f.repo_path}.`,
    };
    return delay(draft, 80);
  },
  fileFinding: async (id: string, body?: { title?: string; description?: string; labels?: string[] }) => {
    const me = requireSession();
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    // Claim-first: only an `open` coordinate can be filed — a second file is the 409 the stale
    // card / stale backlog row handles gracefully (the guarded claim, D4).
    if (f.status !== "open") throw new ApiError(409, "this finding is already filed or being filed");
    const iid = 900 + (parseInt(id.replace(/\D/g, ""), 10) || 0);
    f.status = "filed";
    f.filed_issue_iid = iid;
    f.resolved_at = new Date().toISOString();
    const res: IncidentalFindingFileResult = {
      issue: {
        iid,
        web_url: `https://gitlab.example.com/${f.repo_path}/-/issues/${iid}`,
        title: body?.title ?? f.last_title,
      },
    };
    return delay(res, 120);
  },
  dismissFinding: async (id: string, reason: "wont_do" | "not_an_issue") => {
    const me = requireSession();
    if (reason !== "wont_do" && reason !== "not_an_issue") {
      throw new ApiError(400, "reason must be wont_do or not_an_issue");
    }
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    if (f.status !== "open") {
      throw new ApiError(409, "cannot dismiss (already filed, being filed, or already dismissed)");
    }
    f.status = "dismissed";
    f.resolved_at = new Date().toISOString();
    return delay({ status: "dismissed", reason }, 80);
  },

  // ── File a forge issue from a recommendation (PRD #68 M4 preview) ────────────
  getIssueDraft: async (runId: string, recId: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    return delay({ draft: mockIssueDraft(runId, rec, review) }, 80);
  },
  fileIssue: async (
    runId: string,
    recId: string,
    body: { repo_id: string; title: string; description: string },
  ) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    if (review.filed_issues.some((f) => f.category === rec.category && f.target === rec.target)) {
      throw new ApiError(409, "this recommendation already has an issue, or one is being filed");
    }
    const repo = repos.find((r) => r.id === body.repo_id);
    if (!repo) throw new ApiError(404, "repo not found");
    // Demo hook for mock state E (forge rejected): filing against the atlas repo, which
    // the demo treats as write-protected, surfaces the draft-stays-open error path.
    if (repo.path_with_namespace.includes("atlas")) {
      throw new ApiError(502, "could not create the issue on the forge: the forge rejected the request (403)");
    }
    const iid = nextFiledIssueIid++;
    const web_url = `${repo.web_url}/-/issues/${iid}`;
    // Persist the link so a reload shows the filed row (mock C), just like the real API.
    review.filed_issues.push({
      category: rec.category,
      target: rec.target,
      issue_iid: iid,
      issue_url: web_url,
      filed_at: new Date().toISOString(),
    });
    const issue: CreatedIssue = { iid, web_url, title: body.title };
    return delay({ issue }, 200);
  },
};
