// The incidental-findings MCP server (PRD #333 M2, Decision 2). An in-process SDK MCP
// server the RUN lane (issue / ci_fix / prompt / self_improve) registers so a worker
// mid-run can flag an OFF-TASK bug it noticed without blocking or ending its turn.
//
// This is a PLAIN working tool, NOT a signal (contrast signals.ts, whose tools are
// captured out-of-band by scanSignals). It POSTs to the api, emits a `finding` stream
// card, returns a terse ack, and the turn continues — the propose_issue shape
// (uzi-tools.ts), not the report_progress fire-and-forget shape. It is therefore
// deliberately ABSENT from isSignalToolName / scanSignals: it can never be promoted to
// turn-ending.
//
// The structure is forge-tools.ts's token-less, run-scoped `{client, runId, log}`
// precedent PLUS an `emit` (new plumbing, D2): forge-tools emits nothing, and
// propose_issue's emit lives in the chat executor, so no single precedent is copied.
// The run id is a closure (deps.runId), never a tool parameter, so a subagent cannot
// report against another run. The agent holds NO forge token: capture goes worker→api
// (RequireWorker) exactly like proposals, and filing stays human-gated (R3).

import { createSdkMcpServer, tool } from "@anthropic-ai/claude-agent-sdk";
import type { McpSdkServerConfigWithInstance } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";
import { RequestError, type WorkerClient } from "./client.js";
import type { EmittedMessage } from "./executor.js";
import type { Logger } from "./log.js";
import { asText, type ToolTextResult } from "./tool-evidence.js";
import { errMessage } from "./util.js";

/** The in-process MCP server name; the tool surfaces as
 *  `mcp__findings__report_incidental_issue` (a DISTINCT key from the run lane's `uzi`
 *  signal server, the `memory` server, and the `forge` read server). */
export const FINDINGS_SERVER_NAME = "findings";
export const REPORT_INCIDENTAL_ISSUE_TOOL = "report_incidental_issue";

/** The qualified tool name (`mcp__findings__report_incidental_issue`). */
export function reportIncidentalIssueToolName(): string {
  return `mcp__${FINDINGS_SERVER_NAME}__${REPORT_INCIDENTAL_ISSUE_TOOL}`;
}

// Client-side HYGIENE caps (defense-in-depth only — the api is the authoritative
// control, and it re-sanitises + re-bounds every field). Mirrors signals.ts's MAX_*
// clamps: a garbage payload is clamped rather than rejected so the tool never fails a
// turn on cosmetic input.
const MAX_TITLE_LEN = 255;
const MAX_DESCRIPTION_LEN = 8192;
const MAX_LOCATION_LEN = 512;
const MAX_LABELS = 20;
const MAX_LABEL_LEN = 255;
const MAX_CONFIDENCE_LEN = 32;

/** Clamp a string to at most `n` UTF-16 code units (rune-agnostic; the api re-bounds
 *  by bytes authoritatively). Trims first. */
function clampStr(s: string, n: number): string {
  const t = s.trim();
  return t.length <= n ? t : t.slice(0, n);
}

export interface FindingsToolsDeps {
  client: WorkerClient;
  /** The CURRENT run id — every finding is recorded against THIS run (closure), never
   *  an arbitrary one. */
  runId: string;
  /** Append a message to the run's live stream (the worker owns the gapless seq). The
   *  tool emits the `finding` CARD through this so the UI can render File / Dismiss;
   *  without it the finding would only reach the api, never the stream. */
  emit(msg: EmittedMessage): void;
  log: Logger;
}

export interface FindingsToolHandlers {
  reportIncidentalIssue(args: {
    title: string;
    description: string;
    location: string;
    labels?: string[];
    confidence?: string;
  }): Promise<ToolTextResult>;
}

/** The raw tool handler (unit-testable). It records the finding against deps.runId
 *  only, emits the card, and returns an ack — the whole body is wrapped in try/catch so
 *  a non-2xx from the api (cap reached, etc.) becomes a SOFT ACK text and NEVER throws
 *  (a thrown tool error could disrupt the turn). */
export function makeFindingsToolHandlers(deps: FindingsToolsDeps): FindingsToolHandlers {
  const { client, runId, emit, log } = deps;
  return {
    async reportIncidentalIssue(args) {
      try {
        const title = clampStr(args.title ?? "", MAX_TITLE_LEN);
        const description = clampStr(args.description ?? "", MAX_DESCRIPTION_LEN);
        const location = clampStr(args.location ?? "", MAX_LOCATION_LEN);
        const labels = (args.labels ?? [])
          .slice(0, MAX_LABELS)
          .map((l) => clampStr(String(l), MAX_LABEL_LEN))
          .filter((l) => l.length > 0);
        const confidence = args.confidence ? clampStr(String(args.confidence), MAX_CONFIDENCE_LEN) : undefined;

        const id = await client.reportFinding(runId, {
          title,
          description,
          location,
          labels,
          ...(confidence ? { confidence } : {}),
        });

        // Emit the `finding` CARD to the live stream, keyed on the returned id so the
        // UI's File / Dismiss buttons act on the persisted coordinate (mirrors the
        // proposal emit). All fields are model-authored, escaped inert by the renderer.
        emit({
          kind: "finding",
          payload: {
            id,
            title,
            location,
            labels,
            ...(confidence ? { confidence } : {}),
          },
        });

        return asText(
          `Finding recorded (#${id}). This does NOT end your turn — keep working on your task.`,
        );
      } catch (err) {
        // A non-2xx (cap reached, transient api error) must NOT throw: surface a soft
        // ack and let the turn continue. The api is the authoritative cap control.
        log.warn("findings tool report_incidental_issue failed", { run_id: runId, error: errMessage(err) });
        if (err instanceof RequestError && err.status === 429) {
          return asText("finding cap reached for this run; keep working on your task.");
        }
        return asText("Could not record the finding right now; keep working on your task.");
      }
    },
  };
}

/**
 * Build the incidental-findings MCP server for one run. Returns the server config (for
 * `options.mcpServers.findings`), the qualified tool name, and the raw handler. All
 * three close over `deps`, so a finding can only ever be recorded on THIS run.
 */
export function buildFindingsToolsServer(deps: FindingsToolsDeps): {
  server: McpSdkServerConfigWithInstance;
  toolName: string;
  handlers: FindingsToolHandlers;
} {
  const h = makeFindingsToolHandlers(deps);
  const server = createSdkMcpServer({
    name: FINDINGS_SERVER_NAME,
    version: "1.0.0",
    tools: [
      tool(
        REPORT_INCIDENTAL_ISSUE_TOOL,
        [
          "Record an INCIDENTAL FINDING: a genuinely off-task, actionable bug you noticed",
          "in the user's code on the way to your task — a leaked ticker in a sweeper you",
          "read past, a retry that can never succeed, a boot check with a hole. This does",
          "NOT end your turn: it files nothing, records the finding for the user to review",
          "asynchronously, returns immediately, and you keep working on YOUR task.",
          "",
          "Report ONLY real bugs that are OUTSIDE your current task and worth a human's",
          "attention. Do NOT report: style nits, formatting, naming preferences, anything",
          "about the task you are already doing, or speculation you have not seen in the",
          "code. When in doubt, keep working and do not report.",
          "",
          "`location` must be a SYMBOL-ANCHORED, repo-root-relative coordinate with NO line",
          "number (line numbers drift and defeat dedup) — e.g. `api/internal/foo.go#sweepLoop`",
          "or `web/src/lib/api.ts#listRuns`. The server canonicalises it.",
        ].join("\n"),
        {
          title: z.string().min(1).describe("A short, specific one-line summary of the bug (what is wrong)."),
          description: z
            .string()
            .min(1)
            .describe("What the bug is, why it is a bug, and its impact — enough for the user to triage without the run open. Markdown."),
          location: z
            .string()
            .min(1)
            .describe(
              "A symbol-anchored, repo-root-relative location with NO line number, e.g. api/internal/foo.go#sweepLoop.",
            ),
          labels: z
            .array(z.string().min(1))
            .optional()
            .describe("Optional labels the user MIGHT apply when filing (e.g. bug, security). Suggestions only — the user assembles the final set."),
          confidence: z
            .enum(["low", "medium", "high"])
            .optional()
            .describe("How sure you are this is a real, actionable bug. Omit if unsure."),
        },
        (args) => h.reportIncidentalIssue(args),
      ),
    ],
  });
  return { server, toolName: reportIncidentalIssueToolName(), handlers: h };
}
