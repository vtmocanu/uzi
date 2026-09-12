// PRD #1287 C3 — the shared U-layer (unit/adapter) driver for the Codex conformance cases that
// exercise the REAL production broker / registry / renderer / config builders directly (no app-
// server). These are D7 layer U: recorded payloads and real broker/renderer/reducer policy, run
// under the ordinary `node --test`.
//
// It loads the production modules through the SAME src-root resolution as packaged-modules.ts (host
// `agent/src`, or an image/CI run pointing CODEX_M4_SRC at /app/src), then offers small spies + a
// broker factory + the render request builders, so a U case reads like a policy assertion rather
// than boilerplate.

import path from "node:path";
import { pathToFileURL } from "node:url";

import type {
  CodexCallbackBroker as CodexCallbackBrokerT,
  CodexCallbackBrokerOptions,
  FileopClient,
  FileopRequest,
  FileopResponse,
  RunGrants,
  SpawnCommandResult,
  SpawnCommandSeam,
  ScreenPolicy,
} from "../../agent/src/codex/broker.js";
import type {
  ExecutionRegistry as ExecutionRegistryT,
  newLocalExecutionEpoch as newLocalExecutionEpochT,
} from "../../agent/src/codex/registry.js";
import type { renderCodexRun as renderCodexRunT } from "../../agent/src/codex/render.js";
import type { buildCodexRunPlan as buildCodexRunPlanT } from "../../agent/src/codex/run-builder.js";
import type * as ConfigModuleT from "../../agent/src/codex/config.js";
import type { buildCodexDynamicTools as buildCodexDynamicToolsT } from "../../agent/src/codex/dynamic-tools.js";
import type { RunTurnRequest, HarnessAgent, HarnessToolSet } from "../../agent/src/harness.js";

/** The worktree root every U broker is anchored at (never touched on disk; lexical screening). */
export const U_WORKTREE = "/work/repo";

function srcDir(): string {
  const override = process.env.CODEX_M4_SRC;
  if (override && override.trim().length > 0) return override;
  return path.resolve(process.cwd(), "src");
}
async function load<T>(relative: string): Promise<T> {
  return (await import(pathToFileURL(`${srcDir()}/${relative}`).href)) as T;
}

/** The production modules the U cases exercise directly, resolved once in a `before()`. */
export interface UModules {
  CodexCallbackBroker: typeof CodexCallbackBrokerT;
  ExecutionRegistry: typeof ExecutionRegistryT;
  newLocalExecutionEpoch: typeof newLocalExecutionEpochT;
  renderCodexRun: typeof renderCodexRunT;
  buildCodexRunPlan: typeof buildCodexRunPlanT;
  buildCodexDynamicTools: typeof buildCodexDynamicToolsT;
  config: typeof ConfigModuleT;
}

export async function loadUModules(): Promise<UModules> {
  const broker = await load<typeof import("../../agent/src/codex/broker.js")>("codex/broker.ts");
  const registry = await load<typeof import("../../agent/src/codex/registry.js")>("codex/registry.ts");
  const render = await load<typeof import("../../agent/src/codex/render.js")>("codex/render.ts");
  const runBuilder = await load<typeof import("../../agent/src/codex/run-builder.js")>("codex/run-builder.ts");
  const dynamicTools = await load<typeof import("../../agent/src/codex/dynamic-tools.js")>("codex/dynamic-tools.ts");
  const config = await load<typeof ConfigModuleT>("codex/config.ts");
  return {
    CodexCallbackBroker: broker.CodexCallbackBroker,
    ExecutionRegistry: registry.ExecutionRegistry,
    newLocalExecutionEpoch: registry.newLocalExecutionEpoch,
    renderCodexRun: render.renderCodexRun,
    buildCodexRunPlan: runBuilder.buildCodexRunPlan,
    buildCodexDynamicTools: dynamicTools.buildCodexDynamicTools,
    config,
  };
}

/** A command-identity spawn seam that records every argv it is asked to run. */
export interface SpawnSpy {
  readonly seam: SpawnCommandSeam;
  readonly calls: { argv: readonly string[]; cwd?: string }[];
}
export function spawnSpy(result: SpawnCommandResult = { code: 0, stdout: "ok", stderr: "" }): SpawnSpy {
  const calls: SpawnSpy["calls"] = [];
  return {
    calls,
    seam: async (argv, opts) => {
      calls.push({ argv, cwd: opts.cwd });
      return result;
    },
  };
}

/** An openat2 fileop client that records every request it is asked to run. */
export interface FileopSpy {
  readonly client: FileopClient;
  readonly ops: FileopRequest[];
}
export function fileopSpy(respond: (request: FileopRequest) => FileopResponse = () => ({ ok: true })): FileopSpy {
  const ops: FileopRequest[] = [];
  return {
    ops,
    client: {
      op: async (request) => {
        ops.push(request);
        return respond(request);
      },
    },
  };
}

/** Default lead root grants (full effect vocabulary, isRoot). Override per case. */
export function leadGrants(overrides: Partial<RunGrants> = {}): RunGrants {
  return {
    role: "lead",
    phase: "implement",
    allowedTools: new Set(["Bash", "apply_patch", "Read", "Skill", "spawn_agent", "submit_plan", "signal_done"]),
    allowedSkills: new Set<string>(),
    isRoot: true,
    ...overrides,
  };
}

export interface UBroker {
  broker: InstanceType<typeof CodexCallbackBrokerT>;
  registry: InstanceType<typeof ExecutionRegistryT>;
  spawn: SpawnSpy;
  fileop: FileopSpy;
}

/** Build a REAL broker over a REAL registry with recording spawn/fileop spies. */
export function makeUBroker(mods: UModules, opts: Partial<CodexCallbackBrokerOptions> & { screenPolicy?: ScreenPolicy } = {}): UBroker {
  const registry = (opts.registry as InstanceType<typeof ExecutionRegistryT> | undefined)
    ?? new mods.ExecutionRegistry(mods.newLocalExecutionEpoch(1));
  const spawn = spawnSpy();
  const fileop = fileopSpy();
  const broker = new mods.CodexCallbackBroker({
    registry,
    spawnCommand: opts.spawnCommand ?? spawn.seam,
    fileop: opts.fileop ?? fileop.client,
    worktreePath: opts.worktreePath ?? U_WORKTREE,
    grants: opts.grants ?? leadGrants(),
    delegate: opts.delegate ?? (async () => ({ ok: true, output: { child: "settled" } })),
    toolHandlers: opts.toolHandlers,
    allowedRoles: opts.allowedRoles ?? new Set<string>(),
    screenPolicy: opts.screenPolicy,
    signal: opts.signal,
  });
  return { broker, registry, spawn, fileop };
}

/** A distinct (thread, turn, call) identity per invocation, so each case's callbacks are fresh. */
let callCounter = 0;
export function rt(callId?: string): { threadId: string; turnId: string; callId: string } {
  callCounter += 1;
  return { threadId: "th-1", turnId: "tn-1", callId: callId ?? `c-${callCounter}` };
}

// ─── render request builders (mirroring codex-render.test.ts) ────────────────────
export const allow = (names: readonly string[]): HarnessToolSet => ({ kind: "allow", names });

export function agent(overrides: Partial<HarnessAgent> = {}): HarnessAgent {
  return {
    description: "an agent",
    prompt: "do the thing",
    tools: { kind: "inherit" },
    deniedTools: [],
    toolServers: [],
    skills: [],
    ...overrides,
  };
}

export function runRequest(overrides: Partial<RunTurnRequest> = {}): RunTurnRequest {
  return {
    prompt: "user turn",
    systemPrompt: "lead system prompt",
    signal: new AbortController().signal,
    phase: "implement",
    agents: {},
    leadSkills: [],
    ...overrides,
  };
}
