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

import type {
  AskUserQuestion,
  Milestone,
  MilestoneProgress,
} from "./protocol.js";

/** The in-process MCP server name; tools surface as `mcp__uzi__<tool>`. */
export const SIGNAL_SERVER_NAME = "uzi";
export const SUBMIT_PLAN_TOOL = "submit_plan";
export const SIGNAL_DONE_TOOL = "signal_done";
export const ASK_USER_TOOL = "ask_user";
export const REPORT_PROGRESS_TOOL = "report_progress";
export const CHECKPOINT_TOOL = "checkpoint";

/** issue #279: transport-hygiene cap on the `summary` captured from signal_done and
 *  forwarded as `report_md` on a report-only completion. HYGIENE ONLY — the api is the
 *  authoritative control (it will also scrub+clamp); this just keeps a garbage payload
 *  from ballooning worker memory before it is reported. */
export const REPORT_MD_MAX_LEN = 32_768;

const SUBMIT_PLAN_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${SUBMIT_PLAN_TOOL}`;
const SIGNAL_DONE_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${SIGNAL_DONE_TOOL}`;
const ASK_USER_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${ASK_USER_TOOL}`;
const REPORT_PROGRESS_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${REPORT_PROGRESS_TOOL}`;
const CHECKPOINT_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${CHECKPOINT_TOOL}`;

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
  /** PRD #265 M1: the frozen-milestone ids the lead declares it FINISHED, carried on
   *  signal_done and unioned server-side into `milestones_completed` on the completion
   *  path — so a run that never emitted a mid-run progress report still reconciles its
   *  tracker at completion. Extracted behind the same main-thread guard as `prdDonePath`
   *  (a subagent frame never reaches the content loop). Defensively parsed (ids coerced,
   *  deduped, bad entries dropped, never throws); membership against the frozen list is
   *  re-checked server-side (progressParams). Present only when signal_done carried a
   *  non-empty, parseable id list. */
  milestonesCompleted?: string[];
  /** issue #279: the `summary` a signal_done call carried, clamped to REPORT_MD_MAX_LEN.
   *  MAIN-THREAD-ONLY, behind the same isSubagentFrame guard as `prdDonePath` — a subagent
   *  frame never reaches the content loop. Persisted as `report_md` on a report-only
   *  completion. Present only when signal_done carried a string `summary`. */
  summary?: string;
  /** issue #279: true when a signal_done call declared `report_only: true`. MAIN-THREAD-ONLY,
   *  behind the same isSubagentFrame guard as `prdDonePath`. Signals an evidence/report-only
   *  run that completes with NO push/MR. Present only when signal_done carried `report_only:
   *  true`; absent otherwise so a plain signal_done still scans to exactly `{ done: true }`. */
  reportOnly?: boolean;
  /** PRD #88: the questions an ask_user call carried, if the message made one.
   *  Present and non-empty ⇒ the executor parks the run. */
  questions?: AskUserQuestion[];
  /** PRD #122 M1: the milestone breakdown extracted from a submit_plan call, if the
   *  message carried one. Main-thread-only (the extraction sits behind the same
   *  isSubagentFrame guard as the plan) and defensive — a malformed list yields
   *  nothing rather than throwing, and is only set when at least one entry parsed. */
  milestones?: Milestone[];
  /** PRD #122 M2: the milestone progress a report_progress call carried, if the message
   *  made one. MAIN-THREAD-ONLY (extracted behind the same isSubagentFrame guard as the
   *  plan and milestones), so a subagent frame can never move the run's progress. Both
   *  arrays are defensively parsed — malformed input yields empty arrays, never a throw.
   *  Present whenever a report_progress tool_use was seen on a main-thread frame. */
  progress?: MilestoneProgress;
  /** PRD #122 M6: true when a `checkpoint` tool_use was seen on a MAIN-THREAD frame.
   *  MAIN-THREAD-ONLY, behind the same isSubagentFrame guard as `progress`, and for the
   *  same load-bearing reason: a checkpoint reaps the agent tree and fetches the branch
   *  back, so a subagent frame — prompt-injected or buggy — must NEVER be able to force
   *  one. Absent unless the tool fired on the lead's own frame. */
  checkpoint?: boolean;
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
  /** PRD #122 M1: expose the `milestones` param on submit_plan. Issue runs only
   *  (Decision 13) — a `ci_fix` diagnoses one failure and a `self_improve` cycle
   *  picks one improvement, neither is milestone-shaped, and neither should be able
   *  to self-scale its budget off a milestone count. Gating the SCHEMA keeps the
   *  model from ever seeing the parameter on a non-issue run. */
  milestones?: boolean;
  /** PRD #122 M2: expose the `report_progress` tool. Issue runs only (Decision 13),
   *  gated on the same discriminator as `milestones` — a non-issue run has no milestone
   *  list to progress against, so the tool is invisible to the model there rather than
   *  present-and-inert. */
  progress?: boolean;
  /** PRD #122 M6: expose the `checkpoint` tool. Issue runs only (Decision 13), gated on
   *  the same discriminator as `progress` — a non-issue run has no milestone boundary to
   *  checkpoint at, so the tool is invisible to the model there rather than present. */
  checkpoint?: boolean;
  /** issue #279: expose `report_only` on signal_done. Issue runs only — gated on the same
   *  isIssueRun discriminator as prdDonePath/milestones. A `ci_fix` run already has its own
   *  `not_code` no-MR terminal path and a `self_improve`/`prompt` run is never report-only,
   *  so the parameter is invisible to the model there rather than present-and-inert. */
  reportOnly?: boolean;
}

/**
 * Build the in-process MCP server exposing the two signalling tools. Passed to
 * the SDK via `options.mcpServers`. The handlers return terse guidance; the
 * worker's stream scan is what actually drives the workflow.
 */
export function buildSignalMcpServer(
  opts: SignalServerOptions = {},
): McpSdkServerConfigWithInstance {
  // PRD #72 M4. The describe text carries Decision 5's CONDITIONAL in its own
  // wording: an unconditional instruction handed to a PRDLESS run in a repo that
  // nonetheless has a prds/ directory invites the model to pick one and declare it.
  const doneShape: Record<string, z.ZodTypeAny> = {
    summary: z
      .string()
      .optional()
      .describe("One-line summary of what was implemented."),
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
  // PRD #265 M1: on a milestone'd issue run the lead declares WHICH approved milestone
  // ids it actually finished. The server unions these into `milestones_completed` at
  // completion (monotone, dedup) so a run that never emitted a mid-run progress report
  // still reconciles its tracker. Gated on the SAME issue-run discriminator as the
  // `milestones` submit_plan param (opts.milestones) — invisible to the model on a
  // non-issue run rather than present-and-dropped. The describe text frames it as "what
  // you finished", never "all of them" (Decision 2): an undeclared id stays not-complete.
  if (opts.milestones) {
    doneShape["milestones_completed"] = z
      .array(z.string())
      .optional()
      .describe(
        "OPTIONAL. On a run with an approved milestone breakdown, the ids of the milestones " +
          "you ACTUALLY FINISHED this run (e.g. [\"m1\", \"m2\"]). List only what you truly " +
          "completed — omit any milestone you deliberately left undone, and omit the field " +
          "entirely on a run with no milestones.",
      );
  }
  // issue #279: on an issue run the lead can declare the run REPORT-ONLY — its deliverable
  // is a report/command-output/verification result with no code change to land, so the
  // worker records the findings and opens no merge request. Gated on the SAME issue-run
  // discriminator as prd_done_path/milestones (opts.reportOnly), invisible to the model on
  // a non-issue run rather than present-and-dropped.
  if (opts.reportOnly) {
    doneShape["report_only"] = z
      .boolean()
      .optional()
      .describe(
        "Set true ONLY when the run's deliverable is a report, command output, or a " +
          "verification result with NO code change to land, so the worker records the findings " +
          "and opens no merge request. Omit it (or set false) when a code change was committed.",
      );
  }
  // PRD #122 M1. Built the same way as doneShape: a Record mutated only when the
  // option is set, so the `milestones` param is invisible to the model on a
  // non-issue run (Decision 13) rather than present-and-silently-dropped.
  const planShape: Record<string, z.ZodTypeAny> = {
    plan_md: z.string().describe("The full implementation plan, as Markdown."),
  };
  if (opts.milestones) {
    planShape["milestones"] = z
      .array(z.object({ id: z.string(), title: z.string() }))
      .optional()
      .describe(
        "An OPTIONAL breakdown of the work into milestones, each {id, title} with a " +
          "short stable id like m1. The human approves this breakdown at the gate. " +
          "Omit it entirely for small single-unit work.",
      );
  }
  return createSdkMcpServer({
    name: SIGNAL_SERVER_NAME,
    version: "1.0.0",
    tools: [
      tool(
        SUBMIT_PLAN_TOOL,
        "Submit your implementation plan for human approval. Call this EXACTLY ONCE when the plan is ready, then STOP and end your turn — do not begin implementing. A human approves or rejects the plan out of band; you will be re-prompted to implement only after approval.",
        planShape,
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
        ASK_USER_TOOL,
        // The turn-boundary contract is the load-bearing half of this description, and
        // it is worded like submit_plan's for the same reason: the park happens
        // BETWEEN turns (the executor calls ctx.askUser after driveTurn returns), and
        // the answer arrives as a resumed user turn, not as this tool's return value.
        // A lead that keeps working after calling this is acting on a guess it just
        // said it could not make.
        "Ask the human a question you cannot answer yourself, then STOP and end your turn. " +
          "The run pauses until they reply; their answer comes back as a new user message. " +
          "Ask ONLY when a wrong guess would be costly AND the answer is not inferable from the repo, " +
          "the issue, or the plan — never for trivia, and never for something a grep would settle. " +
          "Batch related questions into ONE call. NEVER ask for a credential, token, password or key: " +
          "the worker holds every secret the run needs, and no answer here is a place to put one.",
        {
          questions: z
            .array(
              z.object({
                question: z.string().describe("The question, in full."),
                header: z
                  .string()
                  .describe("A short label for it (a few words)."),
                options: z
                  .array(
                    z.object({
                      label: z.string().describe("A short answer choice."),
                      description: z
                        .string()
                        .describe("What picking it means."),
                    }),
                  )
                  .optional()
                  .describe(
                    "Suggested answers. The human may always reply with free text instead.",
                  ),
                multiSelect: z
                  .boolean()
                  .optional()
                  .describe("Whether several options may be chosen."),
              }),
            )
            .describe(
              "The questions to ask. Keep the list short and each question self-contained.",
            ),
        },
        async () => ({
          content: [
            {
              type: "text",
              text: "Question sent to the human. Stop now and end your turn — their answer will arrive as a new message.",
            },
          ],
        }),
      ),
      tool(
        SIGNAL_DONE_TOOL,
        "Signal that the implementation is complete and has passed review. Call this once — and only once — the work is committed locally and the reviewer is satisfied. The worker then finalizes the run — pushing the branch and opening the merge request, UNLESS you set report_only, in which case it records your summary and transcript and opens no merge request; you never push.",
        doneShape,
        async () => ({
          content: [
            {
              type: "text",
              text: "Completion recorded. The worker will finalize the run — pushing the branch and opening the merge request, unless you set report_only, in which case it records your summary and transcript and opens no merge request. End your turn.",
            },
          ],
        }),
      ),
      // PRD #122 M2. Issue runs only (opts.progress, gated on kind==="issue"). Unlike
      // submit_plan/signal_done, this does NOT end the turn — it is a pure progress
      // signal the lead fires WHILE it keeps working. The handler only acknowledges; the
      // authoritative capture is the worker observing the tool_use in scanSignals.
      ...(opts.progress
        ? [
            tool(
              REPORT_PROGRESS_TOOL,
              "Report which milestones from your approved plan are complete or in progress, so the run's progress is visible. " +
                "Call this whenever a milestone COMPLETES or a new one STARTS. Pass milestone ids exactly as you gave them in your plan: " +
                "`completed` is every id finished so far (it is fine to repeat ids you already reported — the server unions them), and " +
                "`in_progress` is the ids you are actively working on right now. This does NOT end your turn — keep working after calling it. " +
                "It is purely informational: it does not commit, checkpoint, or gate anything.",
              {
                completed: z
                  .array(z.string())
                  .optional()
                  .default([])
                  .describe("Milestone ids reported complete (cumulative; repeats are fine)."),
                in_progress: z
                  .array(z.string())
                  .optional()
                  .default([])
                  .describe("Milestone ids currently being worked on (a snapshot, replaced each call)."),
              },
              async () => ({
                content: [
                  {
                    type: "text",
                    text: "Progress recorded. Keep working — this did not end your turn.",
                  },
                ],
              }),
            ),
          ]
        : []),
      // PRD #122 M6. Issue runs only (opts.checkpoint, gated on kind==="issue"). A
      // TURN-ENDING tool modelled on submit_plan: the worker acts on it BETWEEN turns
      // (reap + fetch-back the committed work durably), so the lead must stop after
      // calling it and will be re-prompted for the next milestone. The handler only
      // acknowledges; the authoritative capture is the worker observing the tool_use in
      // scanSignals. Decision 6: this is a DURABILITY boundary, NOT a quality gate — the
      // description must not imply it verifies or tests anything.
      ...(opts.checkpoint
        ? [
            tool(
              CHECKPOINT_TOOL,
              "Save the work you have committed for the current milestone durably, so it survives a worker crash. " +
                "Call this once a milestone's work is committed locally, then STOP and end your turn — you will be " +
                "re-prompted to continue with the next milestone. This is a durability checkpoint, NOT a quality gate: " +
                "it does not run tests or verify that review passed.",
              {},
              async () => ({
                content: [
                  {
                    type: "text",
                    text: "Checkpoint saved. Stop now and end your turn.",
                  },
                ],
              }),
            ),
          ]
        : []),
    ],
  });
}

/** Whether a tool name is one of the workflow signalling tools (to filter it
 *  out of the persisted run stream — plan_md is surfaced via the `plan` message,
 *  not duplicated as a raw tool_use payload). */
export function isSignalToolName(name: unknown): boolean {
  // ask_user belongs here for a REASON beyond tidiness (PRD #88): without it the
  // model-authored question JSON also persists as an ordinary tool_use run-message,
  // alongside the `question` message. That is a second, unpinned sink for
  // attacker-influenceable text — a test pinning escaped render for kind=question
  // would stay green while the same strings rendered through the tool rail.
  return (
    name === SUBMIT_PLAN_QUALIFIED ||
    name === SIGNAL_DONE_QUALIFIED ||
    name === ASK_USER_QUALIFIED ||
    // PRD #122 M2: report_progress is surfaced via the `running` state report, not the
    // feed, so its raw tool_use payload is filtered out of the persisted stream like the
    // other signalling tools — otherwise the model-authored id arrays would persist twice.
    name === REPORT_PROGRESS_QUALIFIED ||
    // PRD #122 M6: checkpoint carries no payload and drives the fetch-back out of band,
    // so its raw tool_use is filtered from the persisted stream like the other signals.
    name === CHECKPOINT_QUALIFIED
  );
}

/** Bounds on model-authored question text (PRD #88). The strings reach three
 *  untrusted-text sinks (web feed, Slack DM, CLI render), so they are clamped where
 *  they enter the worker rather than at each sink. */
const MAX_QUESTIONS = 10;
const MAX_OPTIONS = 10;
const MAX_QUESTION_RUNES = 2000;
const MAX_HEADER_RUNES = 60;
const MAX_OPTION_RUNES = 200;

/** Bounds on model-authored milestone data (PRD #122 M1). HYGIENE ONLY, not the
 *  authoritative cap — the api is the real control (Decision 12); these keep a
 *  garbage payload from ballooning worker memory before it is even reported. */
const MAX_MILESTONES = 200;
const MAX_MILESTONE_ID_RUNES = 64;
const MAX_MILESTONE_TITLE_RUNES = 200;

/** Bounds on model-authored progress ids (PRD #122 M2). HYGIENE ONLY, mirroring the
 *  milestone caps above — the api validates membership + shape and is the authoritative
 *  control (Decision 12); these just keep a garbage payload from ballooning worker memory
 *  before it is reported. A progress list can name at most the whole milestone set on
 *  each side, so the count cap matches MAX_MILESTONES and the id cap matches ids. */
const MAX_PROGRESS_IDS = MAX_MILESTONES;
const MAX_PROGRESS_ID_RUNES = MAX_MILESTONE_ID_RUNES;

function clamp(s: string, n: number): string {
  const runes = [...s];
  return runes.length <= n ? s : runes.slice(0, n).join("");
}

/**
 * Parse + clamp the `questions` argument of an ask_user call. Defensive in the same
 * register as the other extractions: anything malformed yields fewer (or no)
 * questions and must never throw. A call whose questions all fail to parse yields an
 * empty array, which the executor treats as "no park" rather than as an empty park.
 */
function parseQuestions(raw: unknown): AskUserQuestion[] {
  if (!Array.isArray(raw)) return [];
  const out: AskUserQuestion[] = [];
  for (const item of raw.slice(0, MAX_QUESTIONS)) {
    const q = asRecord(item);
    if (!q) continue;
    const question =
      typeof q["question"] === "string" ? q["question"].trim() : "";
    if (question === "") continue; // a question with no text is not a question
    const header = typeof q["header"] === "string" ? q["header"].trim() : "";
    const parsed: AskUserQuestion = {
      question: clamp(question, MAX_QUESTION_RUNES),
      header: clamp(header, MAX_HEADER_RUNES),
    };
    const rawOptions = q["options"];
    if (Array.isArray(rawOptions)) {
      const options: { label: string; description: string }[] = [];
      for (const rawOpt of rawOptions.slice(0, MAX_OPTIONS)) {
        const o = asRecord(rawOpt);
        if (!o) continue;
        const label = typeof o["label"] === "string" ? o["label"].trim() : "";
        if (label === "") continue;
        const description =
          typeof o["description"] === "string" ? o["description"] : "";
        options.push({
          label: clamp(label, MAX_OPTION_RUNES),
          description: clamp(description, MAX_OPTION_RUNES),
        });
      }
      if (options.length > 0) parsed.options = options;
    }
    if (q["multiSelect"] === true) parsed.multiSelect = true;
    out.push(parsed);
  }
  return out;
}

/**
 * Parse + clamp the `milestones` argument of a submit_plan call (PRD #122 M1).
 * Modelled exactly on parseQuestions: defensive, never throws. A non-array yields
 * []; entries that are not `{id: string, title: string}` with non-empty trimmed id
 * and title are dropped; id is clamped to 64 runes and title to 200; the count is
 * capped at 200. The cap here is HYGIENE, NOT the authoritative control — the api
 * validates and is the real cap (Decision 12).
 */
function parseMilestones(raw: unknown): Milestone[] {
  if (!Array.isArray(raw)) return [];
  const out: Milestone[] = [];
  for (const item of raw.slice(0, MAX_MILESTONES)) {
    const m = asRecord(item);
    if (!m) continue;
    const id = typeof m["id"] === "string" ? m["id"].trim() : "";
    if (id === "") continue;
    const title = typeof m["title"] === "string" ? m["title"].trim() : "";
    if (title === "") continue;
    out.push({
      id: clamp(id, MAX_MILESTONE_ID_RUNES),
      title: clamp(title, MAX_MILESTONE_TITLE_RUNES),
    });
  }
  return out;
}

/**
 * Parse + clamp one side (`completed` or `in_progress`) of a report_progress call
 * (PRD #122 M2). Defensive in the same register as parseMilestones: a non-array yields
 * []; each entry is coerced to a trimmed string, blanks are dropped, ids are clamped to
 * 64 runes and deduped, and the list is capped at MAX_PROGRESS_IDS. Never throws. The cap
 * here is HYGIENE — the api validates membership against the frozen list (Decision 12).
 */
function parseProgressIds(raw: unknown): string[] {
  if (!Array.isArray(raw)) return [];
  const out: string[] = [];
  const seen = new Set<string>();
  for (const item of raw) {
    if (out.length >= MAX_PROGRESS_IDS) break;
    // Coerce only actual scalars to a string — an object/array id is garbage, not a
    // stringifiable id, so it is dropped rather than turned into "[object Object]".
    const id =
      typeof item === "string"
        ? item.trim()
        : typeof item === "number" || typeof item === "boolean"
          ? String(item).trim()
          : "";
    if (id === "") continue;
    const clamped = clamp(id, MAX_PROGRESS_ID_RUNES);
    if (seen.has(clamped)) continue;
    seen.add(clamped);
    out.push(clamped);
  }
  return out;
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object"
    ? (v as Record<string, unknown>)
    : undefined;
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
 * additional layer that stops the tool_use from ever being made — a PRD #158 M0
 * spike (2026-08-10) measured that disallowedTools does override a custom
 * template's explicit `tools` allowlist on the pinned SDK. But that ran on one
 * claude CLI build, and this scan holds regardless of the SDK's tool gating, so do
 * not treat it as redundant to that layer.
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
      // PRD #122 M1. Extracted HERE, inside the submit_plan branch and therefore
      // behind the isSubagentFrame guard at the top of scanSignals — that guard is
      // what gives milestones the main-thread-only guarantee, exactly as it does for
      // the plan itself. A subagent frame never reaches this loop, so no rejection or
      // latch handling is needed here. Only set out.milestones when at least one entry
      // parsed, so a submit_plan with no/garbage milestones stays additive-absent.
      const milestones = parseMilestones(input?.["milestones"]);
      if (milestones.length > 0) out.milestones = milestones;
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
      // PRD #265 M1. Extracted HERE, in the same signal_done branch and therefore behind
      // the same main-thread guard as prd_done_path above: the completed milestone
      // declaration reconciles the run's tracker, so a subagent frame must never be able
      // to move it. Parsed with the SAME defensive parseProgressIds report_progress uses
      // (ids coerced, deduped, bad entries dropped, never throws); membership against the
      // frozen list is re-checked server-side. Only set when at least one id parsed, so a
      // signal_done with no/garbage milestones_completed stays additive-absent and must
      // never affect `done` — a malformed declaration still means the run finished.
      const completed = parseProgressIds(input?.["milestones_completed"]);
      if (completed.length > 0) out.milestonesCompleted = completed;
      // issue #279. Extracted HERE, in the same signal_done branch and therefore behind the
      // same main-thread guard as prd_done_path above: `summary` is persisted as report_md and
      // `report_only` drives a NO-push/NO-MR terminal path, so a subagent frame must never be
      // able to declare either. Both are set ONLY when present (omitted-not-undefined) so a
      // plain signal_done still scans to exactly `{ done: true }`, and neither ever affects
      // `done` — a malformed declaration still means the run finished. The summary clamp is
      // transport hygiene only; the api is the authoritative control (it also scrubs+clamps).
      const summary = input?.["summary"];
      if (typeof summary === "string")
        out.summary = summary.slice(0, REPORT_MD_MAX_LEN);
      if (input?.["report_only"] === true) out.reportOnly = true;
    } else if (name === ASK_USER_QUALIFIED) {
      // PRD #88. Extracted HERE, inside the content loop that isSubagentFrame already
      // guards, for the same reason prd_done_path is nested inside signal_done's
      // branch: a subagent must not be able to ask the user. agents.ts denies the
      // whole mcp__uzi prefix, and a PRD #158 M0 spike (2026-08-10) measured that
      // this deny does override a template's explicit `tools` allowlist on the pinned
      // SDK — but this main-thread scan is the guarantee independent of SDK gating,
      // and a prompt-injected subagent reaching ask_user would post
      // attacker-chosen text into the owner's Slack DM under uzi's bot identity. That
      // is a phishing primitive, not merely a spurious park.
      const parsed = parseQuestions(asRecord(block["input"])?.["questions"]);
      if (parsed.length > 0)
        out.questions = [...(out.questions ?? []), ...parsed];
    } else if (name === REPORT_PROGRESS_QUALIFIED) {
      // PRD #122 M2. Extracted HERE, inside the content loop that isSubagentFrame already
      // guards, for the SAME reason milestones is nested inside submit_plan's branch: a
      // subagent frame never reaches this loop, so a prompt-injected or buggy subagent can
      // NEVER move the run's progress. Both sides are defensively parsed (ids coerced,
      // deduped, bad entries dropped, never throws).
      //
      // PRD #390 D3: an all-empty report_progress is NO SIGNAL. If neither side carries a
      // real id after parsing (a `{}`, a defaulted, or a non-array-sided call), we assign
      // nothing so out.progress stays undefined — the call never overwrites real progress
      // and never persists a misleading `[]`. A call carrying ANY real id is still a
      // signal. Last-wins within the turn holds for real reports (a later real call still
      // overwrites), and a later all-empty call cannot wipe an earlier real one — that
      // falls out naturally here because the empty call no longer assigns.
      const input = asRecord(block["input"]);
      const completed = parseProgressIds(input?.["completed"]);
      const in_progress = parseProgressIds(input?.["in_progress"]);
      if (completed.length > 0 || in_progress.length > 0)
        out.progress = { completed, in_progress };
    } else if (name === CHECKPOINT_QUALIFIED) {
      // PRD #122 M6. Extracted HERE, inside the content loop that isSubagentFrame already
      // guards (the early return at the top of scanSignals), so it inherits the SAME
      // main-thread-only guarantee as milestones/progress — the load-bearing property
      // that a subagent frame can NEVER force a checkpoint (which reaps the agent tree
      // and fetches the branch back). The tool takes no arguments, so there is nothing to
      // parse: seeing the tool_use on a main-thread frame IS the signal.
      out.checkpoint = true;
    }
  }
  return out;
}
