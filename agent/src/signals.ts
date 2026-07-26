// Workflow signalling tools (PRD #4 §Workflow: plan gate + implement⇄review loop).
//
// The lead orchestrates the whole run inside ONE SDK session; the worker needs
// two out-of-band signals from it — "here is the plan, gate on approval" and
// "the work is done, open the MR". bottega does this with CLI scripts that flip
// DB flags; uzi's agent has no DB/network, so instead the lead calls two
// in-process SDK MCP tools the worker registers. The tool HANDLERS only return a
// short instruction back to the model; the authoritative capture is the worker
// observing the tool_use in the message stream (`scanSignals`). That dual shape
// is deliberate: the same stream-observation works for the real SDK AND for the
// faked `queryFn` in tests (which just yields a scripted tool_use), so the plan
// gate, the loop, and the done→MR handoff are all provable with NO live session
// and NO real token (testing-credentials policy).

import { createSdkMcpServer, tool } from "@anthropic-ai/claude-agent-sdk";
import type { McpSdkServerConfigWithInstance } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";

/** The in-process MCP server name; tools surface as `mcp__uzi__<tool>`. */
export const SIGNAL_SERVER_NAME = "uzi";
export const SUBMIT_PLAN_TOOL = "submit_plan";
export const SIGNAL_DONE_TOOL = "signal_done";

const SUBMIT_PLAN_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${SUBMIT_PLAN_TOOL}`;
const SIGNAL_DONE_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${SIGNAL_DONE_TOOL}`;

/** What the worker extracts from one SDK message's tool_use blocks. */
export interface ScannedSignals {
  /** plan_md from a submit_plan call, if the message carried one. */
  plan?: string;
  /** true if the message carried a signal_done call. */
  done?: boolean;
  /** PRD #72 M4: the repo-relative path the lead says it moved its PRD to.
   *  Forwarded verbatim; the api validates it (`api/internal/prdpath`) and drops
   *  it if it does not hold. Absent unless signal_done carried a string. */
  prdDonePath?: string;
}

/** Options for the signal server's tool schemas. */
export interface SignalServerOptions {
  /** PRD #72 M4: expose `prd_done_path` on signal_done. Issue runs only — a
   *  `ci_fix` run has no issue and a `self_improve` run's issue is a reused
   *  backlog container whose description must never be rewritten (Decision 13).
   *  Gating the SCHEMA is the strongest layer available: the model never sees the
   *  parameter on a non-issue run, rather than seeing it and having the value
   *  silently dropped two hops downstream. */
  prdDonePath?: boolean;
}

/**
 * Build the in-process MCP server exposing the two signalling tools. Passed to
 * the SDK via `options.mcpServers`. The handlers return terse guidance; the
 * worker's stream scan is what actually drives the workflow.
 */
export function buildSignalMcpServer(opts: SignalServerOptions = {}): McpSdkServerConfigWithInstance {
  // PRD #72 M4. The describe text carries Decision 5's CONDITIONAL in its own
  // wording: an unconditional instruction handed to a PRDLESS run in a repo that
  // nonetheless has a prds/ directory invites the model to pick one and declare it.
  const doneShape: Record<string, z.ZodTypeAny> = {
    summary: z.string().optional().describe("One-line summary of what was implemented."),
  };
  if (opts.prdDonePath) {
    doneShape["prd_done_path"] = z
      .string()
      .optional()
      .describe(
        "ONLY if the issue description links a prds/*.md file AND you moved that file to prds/done/ in this run: " +
          "its new repo-relative path, e.g. prds/done/72-thing.md. Omit it entirely otherwise — including when the " +
          "PRD is only partially complete and stayed where it was, and when the issue links no PRD at all.",
      );
  }
  return createSdkMcpServer({
    name: SIGNAL_SERVER_NAME,
    version: "1.0.0",
    tools: [
      tool(
        SUBMIT_PLAN_TOOL,
        "Submit your implementation plan for human approval. Call this EXACTLY ONCE when the plan is ready, then STOP and end your turn — do not begin implementing. A human approves or rejects the plan out of band; you will be re-prompted to implement only after approval.",
        { plan_md: z.string().describe("The full implementation plan, as Markdown.") },
        async () => ({
          content: [
            {
              type: "text",
              text: "Plan submitted for human approval. Stop now and end your turn — do not implement until you are re-prompted with the approval.",
            },
          ],
        }),
      ),
      tool(
        SIGNAL_DONE_TOOL,
        "Signal that the implementation is complete and has passed review. Call this once — and only once — the work is committed locally and the reviewer is satisfied. The worker then pushes the branch and opens the merge request; you never push.",
        doneShape,
        async () => ({
          content: [
            {
              type: "text",
              text: "Completion recorded. The worker will push the branch and open the merge request. End your turn.",
            },
          ],
        }),
      ),
    ],
  });
}

/** Whether a tool name is one of the workflow signalling tools (to filter it
 *  out of the persisted run stream — plan_md is surfaced via the `plan` message,
 *  not duplicated as a raw tool_use payload). */
export function isSignalToolName(name: unknown): boolean {
  return name === SUBMIT_PLAN_QUALIFIED || name === SIGNAL_DONE_QUALIFIED;
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}

/**
 * Whether an assistant frame was produced by a subagent rather than the lead's
 * main thread. Subagent frames carry a `subagent_type` label (the same field
 * sdk-messages.ts attributes by, sdk.d.ts:2777) and a non-null
 * `parent_tool_use_id` (the id of the Agent tool_use that spawned them); the
 * lead's own main-thread frames carry neither. Either marker present ⇒ NOT the
 * main thread. Narrow probes only, so an SDK reshape degrades to "treat as main
 * thread" (fail toward the existing behavior) rather than throwing.
 */
function isSubagentFrame(msg: Record<string, unknown>): boolean {
  const subagentType = msg["subagent_type"];
  if (typeof subagentType === "string" && subagentType.length > 0) return true;
  const parentToolUseId = msg["parent_tool_use_id"];
  return typeof parentToolUseId === "string" && parentToolUseId.length > 0;
}

/**
 * Scan one SDK message for workflow signals. Defensive (narrow probes, no SDK
 * types) so an SDK reshape degrades to "no signal" rather than throwing. Only
 * assistant `tool_use` blocks carry signals.
 *
 * MAIN-THREAD ONLY (PRD #43 M2 / Decision 3): submit_plan/signal_done gate the
 * run and end the implement loop, so only the lead's main-thread frames may carry
 * them. A subagent frame reaching either signal — prompt-injected, buggy, or via
 * some future tool leak — must NOT latch done or the plan (that would hand a
 * partial, unreviewed tree to the worker's push+MR). This worker-side scan is the
 * LOAD-BEARING guarantee for that: it holds regardless of the SDK's tool gating.
 * The server-level `mcp__uzi` denial on every subagent (agents.ts) is an
 * additional layer that SHOULD stop the tool_use from ever being made, but whether
 * disallowedTools wins over a custom template's explicit `tools` allowlist is
 * unproven from the SDK types — so do not treat this scan as redundant to it.
 */
export function scanSignals(message: unknown): ScannedSignals {
  const msg = asRecord(message);
  if (!msg || msg["type"] !== "assistant") return {};
  if (isSubagentFrame(msg)) return {};
  const inner = asRecord(msg["message"]);
  const content = inner?.["content"];
  if (!Array.isArray(content)) return {};
  const out: ScannedSignals = {};
  for (const raw of content) {
    const block = asRecord(raw);
    if (!block || block["type"] !== "tool_use") continue;
    const name = block["name"];
    if (name === SUBMIT_PLAN_QUALIFIED) {
      const input = asRecord(block["input"]);
      const plan = input?.["plan_md"];
      if (typeof plan === "string") out.plan = plan;
      else out.plan = out.plan ?? ""; // a submit_plan with no/blank body still counts as "a plan was submitted"
    } else if (name === SIGNAL_DONE_QUALIFIED) {
      out.done = true;
      // PRD #72 M4. THE ONLY extraction point for prd_done_path, deliberately.
      // It sits inside this branch so it inherits the main-thread guard above
      // (isSubagentFrame returns {} before the content loop is reached) — the same
      // guarantee that keeps a prompt-injected subagent from latching `done`. This
      // field drives a forge write against the run's issue, so a second extraction
      // point anywhere in the stream re-opens that whole threat model from inside
      // the run. Do not add one.
      //
      // Defensive in the same register as submit_plan: a non-string or absent
      // value leaves prdDonePath undefined and must never throw, and must never
      // affect `done` — a malformed declaration still means the run finished.
      const input = asRecord(block["input"]);
      const declared = input?.["prd_done_path"];
      if (typeof declared === "string") out.prdDonePath = declared;
    }
  }
  return out;
}
