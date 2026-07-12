// PRD #43 M0 — SDK concurrency spike (THROWAWAY probe).
//
// Question: does the Claude Agent SDK execute two same-turn FOREGROUND subagents
// concurrently when the run's real agent-guard hook (which rewrites
// `run_in_background: false` on every allowed Agent call) is active — or does it
// serialize them?
//
// Method: a lead is asked to dispatch two subagents (subagent-a, subagent-b) in a
// SINGLE turn. Each subagent runs ONE Bash command that stamps a high-res START
// marker, `sleep 20`, then stamps an END marker into a shared markers file. The
// markers file is filesystem GROUND TRUTH of when each sleep actually ran,
// independent of how the SDK interleaves its message stream:
//   overlap  = A's [START,END] and B's [START,END] intervals overlap
//              (concurrent execution; total wall ≈ 20s, not ≈ 40s)
//   serial   = B_START ≈ A_END (one after the other; total wall ≈ 40s)
//
// The real `buildAgentGuardHook` behavior is replicated VERBATIM below (name
// allowlist + run_in_background:false rewrite) — the guardrails.ts source could
// not be imported directly (it pulls in ./log.js and is in a read-only worktree
// whose agent/ is being edited concurrently), so it is inlined and instrumented.
// Every other Option that could affect scheduling is mirrored from
// sdk-executor.ts: settingSources:[], permissionMode 'bypassPermissions',
// allowDangerouslySkipPermissions, disallowedTools (async-deferral tools),
// includePartialMessages:false, and each subagent disallowing the Agent tool.
// The Bash/path guards from the executor are NOT wired — they gate WHICH commands
// run, never scheduling, and would only get in the way of the marker writes.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type {
  Options,
  AgentDefinition,
  HookInput,
  HookJSONOutput,
  SDKMessage,
  SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";

const NESTED_AGENT_TOOL = "Agent";
const ASYNC_DEFERRAL_TOOLS = ["ScheduleWakeup", "CronCreate"] as const;
const REASON_UNKNOWN_SUBAGENT =
  "denied by guardrail: only the run's assembled subagents may be invoked";

const runLabel = process.argv[2] ?? "run-1";
const here = path.dirname(fileURLToPath(import.meta.url));
const outDir = path.join(here, "out");
fs.mkdirSync(outDir, { recursive: true });
const markersPath = path.join(outDir, `${runLabel}-markers.log`);
const transcriptPath = path.join(outDir, `${runLabel}-transcript.jsonl`);
const hookLogPath = path.join(outDir, `${runLabel}-hookfires.jsonl`);
const findingsPath = path.join(outDir, `${runLabel}-findings.json`);
// Fresh markers file per run.
fs.writeFileSync(markersPath, "");
fs.writeFileSync(transcriptPath, "");
fs.writeFileSync(hookLogPath, "");

const t0 = Date.now();
const rel = (ms: number): number => +((ms - t0) / 1000).toFixed(3);
const nowRel = (): number => rel(Date.now());

function appendJsonl(file: string, obj: unknown): void {
  fs.appendFileSync(file, JSON.stringify(obj) + "\n");
}

// --- the REAL agent-guard hook, inlined verbatim + instrumented --------------
function subagentTypeOf(toolInput: unknown): string | undefined {
  if (toolInput && typeof toolInput === "object" && "subagent_type" in toolInput) {
    const t = (toolInput as { subagent_type?: unknown }).subagent_type;
    if (typeof t === "string" && t.length > 0) return t;
  }
  return undefined;
}

function buildAgentGuardHook(
  allowed: Iterable<string>,
): (input: HookInput) => Promise<HookJSONOutput> {
  const allowSet = new Set(allowed);
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || input.tool_name !== NESTED_AGENT_TOOL) return {};
    const sub = subagentTypeOf(input.tool_input);
    const original =
      input.tool_input && typeof input.tool_input === "object"
        ? (input.tool_input as Record<string, unknown>)
        : {};
    // Instrumentation: record WHEN each Agent invocation is screened, its target,
    // and the run_in_background value we see BEFORE the rewrite.
    appendJsonl(hookLogPath, {
      t: nowRel(),
      event: "agent_guard_fire",
      subagent_type: sub ?? null,
      run_in_background_before: original["run_in_background"] ?? null,
      allowed: sub !== undefined && allowSet.has(sub),
    });
    if (sub === undefined || !allowSet.has(sub)) {
      return {
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: "deny",
          permissionDecisionReason: REASON_UNKNOWN_SUBAGENT,
        },
      };
    }
    if (original["run_in_background"] === false) return {};
    return {
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        updatedInput: { ...original, run_in_background: false },
      },
    };
  };
}

// --- subagent definitions ----------------------------------------------------
// One prescriptive Bash command: stamp START, sleep 20, stamp END. perl's
// Time::HiRes gives sub-second wall time (macOS BSD `date` has no %N).
function markerCommand(tag: string): string {
  const stamp = (label: string) =>
    `printf '${label} %s\\n' "$(perl -MTime::HiRes=time -e 'printf \"%.3f\", time')" >> ${markersPath}`;
  return `${stamp(`${tag}_START`)}; sleep 20; ${stamp(`${tag}_END`)}; echo ${tag}_DONE`;
}

function subagentDef(tag: string): AgentDefinition {
  return {
    description: `Concurrency probe worker ${tag}. Runs one timed Bash command.`,
    model: "haiku",
    disallowedTools: [NESTED_AGENT_TOOL],
    prompt:
      `You are probe worker ${tag}. Your ONLY job: make EXACTLY ONE Bash tool call ` +
      `with this command, verbatim, then reply "done". Do not run any other command, ` +
      `do not read or write any file, do not explain.\n\nCommand:\n${markerCommand(tag)}\n`,
    maxTurns: 4,
  };
}

const agents: Record<string, AgentDefinition> = {
  "subagent-a": subagentDef("A"),
  "subagent-b": subagentDef("B"),
};

const leadSystemPrompt =
  "You are a concurrency-probe orchestrator. You have exactly two subagents: " +
  "`subagent-a` and `subagent-b`. Your task is to dispatch BOTH of them in the " +
  "SAME turn so they run at the same time. In ONE assistant message, call the " +
  "Agent tool TWICE — once with subagent_type \"subagent-a\" and once with " +
  "subagent_type \"subagent-b\" — each with the task \"run your probe command\". " +
  "Do NOT call one, wait for its result, then call the other. Dispatch both " +
  "together. After both subagents have returned, reply with the single word DONE.";

const userPrompt =
  "Dispatch subagent-a and subagent-b together in one turn now. Both at once.";

async function* promptStream(): AsyncGenerator<SDKUserMessage> {
  yield {
    type: "user",
    message: { role: "user", content: userPrompt },
    parent_tool_use_id: null,
    session_id: "",
  } as SDKUserMessage;
}

// --- frame extraction --------------------------------------------------------
function summarizeBlocks(msg: SDKMessage): unknown {
  const anyMsg = msg as unknown as {
    message?: { content?: unknown };
  };
  const content = anyMsg.message?.content;
  if (!Array.isArray(content)) {
    return typeof content === "string" ? { text: content.slice(0, 120) } : undefined;
  }
  return content.map((b: unknown) => {
    const bb = b as Record<string, unknown>;
    const type = bb["type"];
    if (type === "tool_use") {
      const input = bb["input"] as Record<string, unknown> | undefined;
      return {
        type,
        name: bb["name"],
        tool_use_id: bb["id"],
        subagent_type: input?.["subagent_type"] ?? undefined,
        run_in_background: input?.["run_in_background"] ?? undefined,
        command:
          typeof input?.["command"] === "string"
            ? (input["command"] as string).slice(0, 80)
            : undefined,
      };
    }
    if (type === "tool_result") {
      const c = bb["content"];
      return {
        type,
        tool_use_id: bb["tool_use_id"],
        result: typeof c === "string" ? c.slice(0, 80) : Array.isArray(c) ? "[blocks]" : undefined,
      };
    }
    if (type === "text") {
      return { type, text: String(bb["text"]).slice(0, 100) };
    }
    return { type };
  });
}

// --- run ---------------------------------------------------------------------
async function main(): Promise<void> {
  const options: Options = {
    cwd: outDir,
    settingSources: [],
    systemPrompt: leadSystemPrompt,
    agents,
    model: "sonnet", // capable lead so parallel DISPATCH is reliable; subagents are haiku
    permissionMode: "bypassPermissions",
    allowDangerouslySkipPermissions: true,
    disallowedTools: [...ASYNC_DEFERRAL_TOOLS],
    includePartialMessages: false,
    hooks: {
      PreToolUse: [
        { matcher: NESTED_AGENT_TOOL, hooks: [buildAgentGuardHook(Object.keys(agents))] },
      ],
    },
  };

  appendJsonl(transcriptPath, { t: 0, event: "probe_start", runLabel, t0_iso: new Date(t0).toISOString() });
  let seq = 0;
  const q = query({ prompt: promptStream(), options });
  for await (const msg of q) {
    seq++;
    const m = msg as unknown as {
      type: string;
      subagent_type?: string;
      parent_tool_use_id?: string | null;
      session_id?: string;
      subtype?: string;
    };
    appendJsonl(transcriptPath, {
      t: nowRel(),
      seq,
      type: m.type,
      subtype: m.subtype,
      subagent_type: m.subagent_type ?? null,
      parent_tool_use_id: m.parent_tool_use_id ?? null,
      blocks: summarizeBlocks(msg),
    });
    if (m.type === "result") break;
  }
  const tEnd = nowRel();
  appendJsonl(transcriptPath, { t: tEnd, event: "probe_end", frames: seq });

  analyze(tEnd);
}

// --- analysis ----------------------------------------------------------------
function analyze(tEnd: number): void {
  const raw = fs.readFileSync(markersPath, "utf8").trim();
  const marks: Record<string, number> = {};
  for (const line of raw.split("\n").filter(Boolean)) {
    const [k, v] = line.split(/\s+/);
    if (k && v) marks[k] = parseFloat(v);
  }
  const aStart = marks["A_START"];
  const aEnd = marks["A_END"];
  const bStart = marks["B_START"];
  const bEnd = marks["B_END"];

  let verdict = "INCONCLUSIVE";
  let overlapSec: number | null = null;
  let detail = "";
  if ([aStart, aEnd, bStart, bEnd].every((x) => typeof x === "number")) {
    const oStart = Math.max(aStart!, bStart!);
    const oEnd = Math.min(aEnd!, bEnd!);
    overlapSec = +(oEnd - oStart).toFixed(3);
    const aWall = +(aEnd! - aStart!).toFixed(3);
    const bWall = +(bEnd! - bStart!).toFixed(3);
    const totalWall = +(Math.max(aEnd!, bEnd!) - Math.min(aStart!, bStart!)).toFixed(3);
    if (overlapSec > 5) {
      verdict = "OVERLAP";
      detail = `A and B slept concurrently: overlap ${overlapSec}s (each ~${aWall}/${bWall}s; total wall ${totalWall}s ≈ one sleep, not two).`;
    } else if (overlapSec <= 0) {
      verdict = "SERIALIZED";
      const gap = +(Math.max(aStart!, bStart!) - Math.min(aEnd!, bEnd!)).toFixed(3);
      detail = `No overlap: intervals disjoint (gap ${gap}s; total wall ${totalWall}s ≈ two sleeps back to back).`;
    } else {
      verdict = "MARGINAL_OVERLAP";
      detail = `Small overlap ${overlapSec}s — inspect timings.`;
    }
  } else {
    detail = `Missing markers (got: ${Object.keys(marks).join(",") || "none"}). Subagents may not have both run.`;
  }

  // Guard-hook fire timing.
  const hookFires = fs
    .readFileSync(hookLogPath, "utf8")
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l) as { t: number; subagent_type: string | null; run_in_background_before: unknown });
  const dispatchTimes = hookFires.map((h) => ({ subagent: h.subagent_type, t: h.t, run_in_background_before: h.run_in_background_before }));
  const dispatchGap =
    hookFires.length >= 2 ? +(hookFires[1]!.t - hookFires[0]!.t).toFixed(3) : null;

  const findings = {
    runLabel,
    t0_iso: new Date(t0).toISOString(),
    verdict,
    overlapSec,
    detail,
    markers: { A_START: aStart, A_END: aEnd, B_START: bStart, B_END: bEnd },
    markers_rel_to_t0: {
      A_START: aStart != null ? +(aStart - t0 / 1000).toFixed(3) : null,
      A_END: aEnd != null ? +(aEnd - t0 / 1000).toFixed(3) : null,
      B_START: bStart != null ? +(bStart - t0 / 1000).toFixed(3) : null,
      B_END: bEnd != null ? +(bEnd - t0 / 1000).toFixed(3) : null,
    },
    agent_guard_fires: dispatchTimes,
    dispatch_gap_between_guard_fires_sec: dispatchGap,
    total_probe_wall_sec: tEnd,
    artifacts: {
      transcript: transcriptPath,
      markers: markersPath,
      hookfires: hookLogPath,
    },
  };
  fs.writeFileSync(findingsPath, JSON.stringify(findings, null, 2));
  console.log(`\n===== ${runLabel} VERDICT: ${verdict} =====`);
  console.log(detail);
  console.log(`overlapSec=${overlapSec}  dispatch_gap=${dispatchGap}  probe_wall=${tEnd}s`);
  console.log(`findings: ${findingsPath}`);
}

main().catch((err) => {
  appendJsonl(transcriptPath, { t: nowRel(), event: "probe_error", error: String(err?.stack ?? err) });
  console.error("PROBE ERROR:", err);
  process.exitCode = 1;
});
