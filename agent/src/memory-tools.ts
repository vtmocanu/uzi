// The save_memory tool (PRD #90 M2). An in-process SDK MCP server the LEAD run
// registers so it can persist a durable cross-run learning for its (user, repo).
//
// Why an MCP tool and NOT a file write (the crux of PRD #90): the file-tool path
// guard (guardrails.ts) denies every write outside the run worktree, and the
// per-run agent-home/memory dir is torn down on terminal — so a lead that tried
// to Write a memory file was DENIED (run e2d7427b). save_memory is a network
// custom tool, not a file write, so it is NOT subject to that path guard and needs
// NO guard carve-out: the file guard stays a hard "deny everything outside the
// worktree", unchanged.
//
// Two invariants keep it safe:
//   1. Server-derived identity (Decision 2026-07-19). The worker POSTs only
//      {title, body}; the API derives (user_id, repo_id) from the run claim and
//      NEVER accepts them as parameters, so a compromised worker cannot write
//      arbitrary users' memory. The tool targets deps.runId (closure), never an
//      arbitrary run.
//   2. Bounded + capped. title ≤200 bytes, body ≤2048 bytes, ≤5 writes/run — all
//      enforced server-side; this tool mirrors the size caps client-side (a clear
//      tool error, never a throw) so an over-cap call is rejected before the POST,
//      and surfaces a 429/409/400 from the server as a concise, NON-FATAL message.
//
// The tool logic lives in makeMemoryToolHandlers (unit-testable directly);
// buildMemoryServer wraps it with the SDK tool() schema — the same shape as
// uzi-tools.ts (makeUziToolHandlers + buildUziToolsServer).

import { createSdkMcpServer, tool } from "@anthropic-ai/claude-agent-sdk";
import type { McpSdkServerConfigWithInstance } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";
import type { WorkerClient } from "./client.js";
import { RequestError } from "./client.js";
import type { MemoryBasis } from "./protocol.js";
import type { Logger } from "./log.js";
import { errMessage } from "./util.js";

/** The in-process MCP server name; the tool surfaces as `mcp__memory__save_memory`
 *  (a SECOND server alongside the run lane's `uzi` signal server, disjoint keys). */
export const MEMORY_SERVER_NAME = "memory";
export const SAVE_MEMORY_TOOL = "save_memory";

/** Client-side mirror of the server-enforced entry caps (PRD #90 M4). BOTH title
 *  and body are bounded in BYTES (utf-8), matching the server's byte caps exactly —
 *  a multibyte title ≤200 chars but >200 bytes would otherwise pass here and 400
 *  server-side. */
export const MEMORY_TITLE_MAX_BYTES = 200;
export const MEMORY_BODY_MAX_BYTES = 2048;
/** Cap for the optional writer-declared evidence pointer (PRD #266 M2), in BYTES
 *  (utf-8), same pattern as title/body. It is a short pointer (a `file:line`, a
 *  command, a tool name), not prose — 200 bytes matches the title cap. */
export const MEMORY_EVIDENCE_MAX_BYTES = 200;

/**
 * Obvious "volatile snapshot" shapes a durable memory should NOT be built around:
 * a test-pass/fail count ("1156 pass"), a ratio ("1156/1157"), or an "N of M"
 * tally. A match appends a NON-FATAL nudge to the (already-successful) save — it
 * NEVER rejects, because a legitimate numeric fact can wear the same shape; the
 * memory is stored either way. Complements the tool-description/prompt guidance.
 *
 * COST COUPLING: this alternation backtracks superlinearly on an adversarial digit
 * run, so its cost is bounded ONLY by the input length. The `bytes >
 * MEMORY_BODY_MAX_BYTES` early-return in saveMemory runs BEFORE this regex is ever
 * tested, so `body` here is always ≤ MEMORY_BODY_MAX_BYTES (2048) — that cap is the
 * bound (auditor measured ~48ms worst case at 2048 bytes, ~0.8s at 8192). Raising
 * MEMORY_BODY_MAX_BYTES therefore raises this per-call worst case: revisit this
 * regex (anchor it, or drop the superlinear `\d+\s*` prefixes) if the cap grows.
 */
const VOLATILE_SNAPSHOT_RE = /\d+\s*(?:pass|fail)|\d+\s*\/\s*\d+|\bof\s+\d+\b/i;

/** The qualified tool name to allow/deny by (extraTools / subagent deny). */
export function memoryToolNames(): string[] {
  return [`mcp__${MEMORY_SERVER_NAME}__${SAVE_MEMORY_TOOL}`];
}

/** The MCP text result shape a tool handler returns. The index signature keeps it
 *  assignable to the SDK's CallToolResult (an open record). Mirrors uzi-tools.ts. */
export interface ToolTextResult {
  content: { type: "text"; text: string }[];
  isError?: boolean;
  [key: string]: unknown;
}
const asText = (t: string, isError = false): ToolTextResult => ({ content: [{ type: "text", text: t }], isError });

/**
 * Format a caught error into model-facing guidance (never a raw stack), NON-FATAL:
 * save_memory failing must never fail the run — the model is told the memory was
 * not saved and can continue. A RequestError carries the server status, so each
 * documented failure becomes a clear sentence (429 cap / 409 repo-less / 400
 * empty-or-oversize).
 */
function memoryToolError(err: unknown): ToolTextResult {
  if (err instanceof RequestError) {
    if (err.status === 429) {
      return asText(
        "Memory not saved: this run has already reached its save_memory limit. Do not retry — continue the task without saving more.",
        true,
      );
    }
    if (err.status === 409) {
      return asText(
        "Memory not saved: this run is not associated with a repository, so there is nowhere durable to store it. Continue without saving.",
        true,
      );
    }
    if (err.status === 400) {
      return asText(`Memory not saved: ${err.body || "the entry was rejected (empty or too large)"}. Shorten it and try once more, or continue.`, true);
    }
    return asText(`Memory not saved: the uzi API returned an error (status ${err.status}). Continue without saving.`, true);
  }
  return asText(`Memory not saved: ${errMessage(err)}. Continue without saving.`, true);
}

export interface MemoryToolsDeps {
  client: WorkerClient;
  /** The CURRENT run id — save_memory persists for THIS run's claim only (the
   *  server derives (user, repo) from it), never an arbitrary run. */
  runId: string;
  log: Logger;
}

/** The raw tool handlers (unit-testable). save_memory validates the size caps
 *  client-side, then POSTs through deps.client for deps.runId only. */
export interface MemoryToolHandlers {
  saveMemory(args: { title: string; body: string; basis?: MemoryBasis; evidence?: string }): Promise<ToolTextResult>;
}

export function makeMemoryToolHandlers(deps: MemoryToolsDeps): MemoryToolHandlers {
  const { client, runId, log } = deps;
  return {
    async saveMemory(args) {
      // Client-side cap mirror (a clear tool error, NOT a throw): the SDK schema
      // also enforces these, but validating here means the unit suite — which calls
      // the handler directly, bypassing the schema — proves the rejection, and an
      // over-cap call never reaches the network.
      const title = (args.title ?? "").trim();
      const body = args.body ?? "";
      if (title.length === 0) return asText("save_memory needs a non-empty title.", true);
      const titleBytes = Buffer.byteLength(title, "utf8");
      if (titleBytes > MEMORY_TITLE_MAX_BYTES) {
        return asText(`save_memory title is too long (max ${MEMORY_TITLE_MAX_BYTES} bytes; got ${titleBytes}). Shorten it and try again.`, true);
      }
      if (body.trim().length === 0) return asText("save_memory needs a non-empty body.", true);
      const bytes = Buffer.byteLength(body, "utf8");
      if (bytes > MEMORY_BODY_MAX_BYTES) {
        return asText(`save_memory body is too long (max ${MEMORY_BODY_MAX_BYTES} bytes; got ${bytes}). Trim it and try again.`, true);
      }
      // Provenance (PRD #266 M2): default an omitted basis to `inferred` (never a
      // hard failure — PRD #90). Evidence is optional; normalize empty/whitespace to
      // undefined and byte-cap it with a clear tool error, like title/body.
      const basis: MemoryBasis = args.basis ?? "inferred";
      const evidenceTrimmed = (args.evidence ?? "").trim();
      const evidence = evidenceTrimmed.length === 0 ? undefined : evidenceTrimmed;
      if (evidence !== undefined) {
        const evidenceBytes = Buffer.byteLength(evidence, "utf8");
        if (evidenceBytes > MEMORY_EVIDENCE_MAX_BYTES) {
          return asText(`save_memory evidence is too long (max ${MEMORY_EVIDENCE_MAX_BYTES} bytes; got ${evidenceBytes}). Shorten it to a pointer (a file:line, command, or tool name) and try again.`, true);
        }
      }
      try {
        const entry = await client.saveMemory(runId, { title, body, basis, evidence });
        // The memory IS saved (isError stays false); this only appends an advisory
        // nudge when the body looks like a fast-decaying tally, never a rejection.
        const snapshotNudge = VOLATILE_SNAPSHOT_RE.test(body)
          ? " Note: this reads like a volatile snapshot figure (a count, ratio, or \"N of M\" tally). It is saved, but prefer recording the durable fact — the mechanism or command, not today's number, which decays and misleads a later run."
          : "";
        return asText(
          `Saved cross-run memory "${entry.title}" (id ${entry.id}). Future runs on this repository will see it as advisory context — it is NOT authoritative and will never override the current task.${snapshotNudge}`,
        );
      } catch (err) {
        log.warn("save_memory tool failed", { run_id: runId, error: errMessage(err) });
        return memoryToolError(err);
      }
    },
  };
}

/**
 * Build the save_memory MCP server for one run. Returns the server config (for
 * `options.mcpServers.memory`), the qualified tool name, and the raw handlers.
 * All three close over `deps`, so save_memory can only ever write THIS run's
 * (server-derived) (user, repo) memory. Mirrors buildUziToolsServer.
 */
export function buildMemoryServer(deps: MemoryToolsDeps): {
  server: McpSdkServerConfigWithInstance;
  toolNames: string[];
  handlers: MemoryToolHandlers;
} {
  const h = makeMemoryToolHandlers(deps);
  const server = createSdkMcpServer({
    name: MEMORY_SERVER_NAME,
    version: "1.0.0",
    tools: [
      tool(
        SAVE_MEMORY_TOOL,
        [
          "Save a DURABLE cross-run operational fact about THIS repository for your future",
          "runs (per-user + per-repo). Use it for a hard-won learning worth carrying forward",
          "— a build flag, a setup quirk, a non-obvious gotcha — NOT for task-specific state.",
          "It is persisted server-side; you cannot read it back this run. Do NOT save secrets",
          "or anything sensitive. The per-run home/memory directory is ephemeral and file",
          "writes outside the worktree are denied — this tool is the ONLY sanctioned way to",
          "persist a note. Record the DURABLE fact, not a volatile snapshot: prefer a mechanism",
          "or command over today's number (a test-pass count, a version tally, an \"N of M\" ratio",
          "all decay and mislead a later run). Keep the title short and the body a couple of sentences.",
          "Set basis to \"observed\" ONLY when the claim is backed by something you can name — a",
          "tool result, command output, or a file:line — and put that pointer in evidence; otherwise",
          "leave basis \"inferred\". A later run is told which, and re-verifies an inferred claim before",
          "acting on it. Prefer to READ runtime/config facts live (your roster, tools, and environment)",
          "rather than remembering them — they change as the product changes.",
        ].join(" "),
        {
          title: z
            .string()
            .min(1)
            .refine((s) => Buffer.byteLength(s, "utf8") <= MEMORY_TITLE_MAX_BYTES, {
              message: `title must be at most ${MEMORY_TITLE_MAX_BYTES} bytes`,
            })
            .describe(`A short label for the note (≤${MEMORY_TITLE_MAX_BYTES} bytes).`),
          body: z
            .string()
            .min(1)
            .refine((s) => Buffer.byteLength(s, "utf8") <= MEMORY_BODY_MAX_BYTES, {
              message: `body must be at most ${MEMORY_BODY_MAX_BYTES} bytes`,
            })
            .describe(`The durable fact to remember, not a volatile snapshot like a test-pass count or version tally (≤${MEMORY_BODY_MAX_BYTES} bytes).`),
          basis: z
            .enum(["observed", "inferred"])
            .default("inferred")
            .describe(
              'Provenance of the claim: "observed" only when backed by a tool result, command output, or file:line you can name (put the pointer in evidence); otherwise "inferred". Defaults to "inferred".',
            ),
          evidence: z
            .string()
            .optional()
            .describe(`Optional short pointer to what backs an "observed" claim — a file:line, command, or tool name (≤${MEMORY_EVIDENCE_MAX_BYTES} bytes).`),
        },
        (args) => h.saveMemory(args),
      ),
    ],
  });
  return { server, toolNames: memoryToolNames(), handlers: h };
}
