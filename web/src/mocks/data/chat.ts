import type {
  IssueProposal,
  Run,
  RunMessage,
} from "../../lib/api";
import { minsAgo } from "./time";

// ── Chat conversations (PRD #39 M4) ──────────────────────────────────────────
// Chat rides the run machinery, so a conversation is a run with kind='chat' and
// no repo (repo_id "" stands in for the real backend's nullable repo_id — the
// chat UI never reads it). Their message logs seed the same store maps as issue
// runs so the streaming machinery renders them unchanged.

const CHAT_1 = "chat-uzi-1"; // active, answers a question about uzi's own source
const CHAT_2 = "chat-uzi-2"; // active, carries a pending issue proposal
const CHAT_3 = "chat-uzi-3"; // ended, offers Continue

function chatRun(over: Partial<Run> & { id: string; title: string; status: Run["status"] }): Run {
  const { title, ...rest } = over;
  return {
    repo_id: null, // a chat run has no repo (runDTO repo_id is nullable, PRD #39)
    kind: "chat",
    issue_iid: null,
    issue_title: title, // conversation title fallback (useRunStream reads the run)
    issue_description: "",
    title, // the runDTO's chat title
    resume_of_run_id: null,
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    stop_reason: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    repo_agents: null, // a chat run carries no agent roster (PRD #37 fields)
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(6),
    started_at: minsAgo(6),
    finished_at: null,
    created_at: minsAgo(7),
    updated_at: minsAgo(1),
    ...rest,
  };
}

export const mockChatRuns: Run[] = [
  chatRun({ id: CHAT_1, title: "How does the plan-approval gate work?", status: "running", updated_at: minsAgo(1) }),
  chatRun({ id: CHAT_2, title: "Add a worker metrics dashboard", status: "running", updated_at: minsAgo(3) }),
  chatRun({
    id: CHAT_3,
    title: "Why did run #57 fail?",
    status: "completed",
    finished_at: minsAgo(40),
    updated_at: minsAgo(40),
  }),
];

// makeChatLog assigns gapless per-conversation seq, like the real run log.
function makeChatLog(entries: [kind: string, agent: string | null, payload: unknown][]): RunMessage[] {
  return entries.map(([kind, agent, payload], i) => ({
    seq: i + 1,
    kind,
    agent,
    agent_instance: null,
    agent_label: null,
    payload,
    created_at: minsAgo(6 - i * 0.5),
  }));
}

// The pending proposal shown in CHAT_2. Its description deliberately contains a
// URL so the demo shows a model-supplied link rendered INERT (never an anchor).
export const mockProposals: IssueProposal[] = [
  {
    id: "prop-metrics-1",
    run_id: CHAT_2,
    repo_path: "vtmocanu/uzi",
    title: "Add a worker metrics dashboard",
    description:
      "Surface per-worker heartbeat freshness, active-run count, and template drift on one page, reading the existing /api/metrics/workers data. Idea sketched at https://example.com/metrics-notes — worth a quick spike first.",
    labels: ["uzi", "enhancement"],
    status: "pending",
    created_at: minsAgo(3),
  },
];

export const mockChatMessages: Record<string, RunMessage[]> = {
  [CHAT_1]: makeChatLog([
    ["user_message", null, { text: "How does the plan-approval gate work?" }],
    ["text", "chat", { text: "Let me check how the gate is wired in uzi's source before I answer." }],
    ["tool_use", "chat", { id: "c1-1", name: "Grep", input: { pattern: "awaiting_approval", path: "/opt/uzi-src/api" } }],
    ["tool_result", "chat", { tool_use_id: "c1-1", content: "api/internal/workersvc/service.go:388\napi/internal/store/queries/runtime.sql:154\napi/internal/poller/sweeper.go:41" }],
    ["text", "chat", {
      text:
        "The gate is a **run state**, not a flag. When the agent submits a plan the run moves to `awaiting_approval` and the worker parks the SDK session. The API refuses to resume it until you approve:\n\n1. The agent emits a `plan` message and the run enters `awaiting_approval` (`api/internal/workersvc/service.go`).\n2. Your **Approve** click posts an `approve_plan` input; only then does the worker resume the session and start implementing.\n3. A **Reject** sends your reason back as the next turn instead.\n\nSo nothing touches a branch before you approve. Want me to show the resume path in `runtime.sql`?",
    }],
  ]),
  [CHAT_2]: makeChatLog([
    ["user_message", null, { text: "Could you file an issue to add a worker metrics dashboard?" }],
    ["text", "chat", { text: "Here is a draft. Review it and click **Create issue** if it looks right — I can't open it myself." }],
    ["proposal", "chat", mockProposals[0]],
  ]),
  [CHAT_3]: makeChatLog([
    ["user_message", null, { text: "Why did run #57 fail?" }],
    ["tool_use", "chat", { id: "c3-1", name: "get_run", input: { run_id: "57" } }],
    ["tool_result", "chat", { tool_use_id: "c3-1", content: "status: failed\nfailure_reason: run timed out after 2h0m0s (RUN_TIMEOUT)" }],
    ["text", "chat", {
      text:
        "Run #57 hit the **2-hour wall-clock** (`RUN_TIMEOUT`) — it did not fail on a code error. The last activity was a benchmark that hung with no output for ~40 minutes, so the sweeper timed the run out. If you re-run it, cap or skip that benchmark first.",
    }],
    ["status", null, { text: "conversation ended (idle timeout)" }],
  ]),
};
