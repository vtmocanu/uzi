// The Claude Agent SDK executor (PRD #4 M3 + M4). Replaces the M2 stub behind the
// same `Executor` interface, so the runner's claim → worktree → work → report
// state machine is unchanged; only the "work" step drives a real agent.
//
// M4 turns the single M3 turn into the full workflow: a PLANNING turn whose plan
// is gated on human approval (ctx.gatePlan), then an implement⇄review LOOP of
// resumed turns (bottega's model — no mid-turn injection), capped at
// RUN_MAX_ITERATIONS, exiting when the lead calls `signal_done`. Follow-up
// corrections are injected between turns via SDK session resume. Cancel aborts
// the current turn's subprocess group. The WORKER performs the branch push + MR
// after this returns; the executor never holds a credential.
//
// Every agent subprocess spawned across the run's turns is group-killed before
// this returns (killAgentTree, run() finally) — the DONE path, not just a
// watchdog/cancel trip — so an agent-backgrounded process can never survive to
// read the PAT out of the worker's git-push subprocess env (M4 audit B1).
//
// Everything that touches the network is behind an injectable `queryFn` (default
// = the SDK's `query`). Tests pass a fake that returns a controllable message
// stream, so the gate, the loop, the guardrails, sparse env, session/resume, and
// watchdogs are all provable with NO live Anthropic session and NO real token
// (testing-credentials policy). The plan/done signals are observed from that
// stream (see signals.ts), so a scripted fake proves them without a live SDK.

import fs from "node:fs/promises";
import path from "node:path";
import { query as sdkQuery } from "@anthropic-ai/claude-agent-sdk";
import type { Options as SdkOptions, SDKMessage, SpawnOptions, SpawnedProcess } from "@anthropic-ai/claude-agent-sdk";
import type { Executor, ExecutorResult, RunContext } from "./executor.js";
import type { Logger } from "./log.js";
import { buildCheckEnv, buildSdkEnv } from "./sdk-env.js";
import type { DockerWiring } from "./docker-wiring.js";
import { provisionTools } from "./provision.js";
import { provisionRunTools } from "./provision-run.js";
import { installJsDeps, type JsDepsInstall, type JsDepsResult } from "./js-deps.js";
import { assembleAgents, selectSubagents } from "./agents.js";
import { resolveAgentSelection, type ClaimConfig } from "./protocol.js";
import { buildCIFixPlanPrompt, buildImplementPrompt, buildLeadSystemPrompt, buildPlanPrompt, buildRevisePlanPrompt, buildSelfImprovePlanPrompt, isNotCodePlan } from "./prompt.js";
import { buildPreToolUseHook, buildPathGuardHook, buildAgentGuardHook, NESTED_AGENT_TOOL, ASYNC_DEFERRAL_TOOLS } from "./guardrails.js";
import { buildSignalMcpServer, isSignalToolName, scanSignals, SIGNAL_SERVER_NAME } from "./signals.js";
import { buildMemoryServer, MEMORY_SERVER_NAME } from "./memory-tools.js";
import type { WorkerClient } from "./client.js";
import type { McpSdkServerConfigWithInstance } from "@anthropic-ai/claude-agent-sdk";
import { qualifiedSkillName, type SkillDrop } from "./skills-plugin.js";
import { prepareSkillPlugin, resolveSkillCaps } from "./skills-run.js";
import { killProcessGroup, spawnDetached } from "./sdk-spawn.js";
import { assistantUsageOf, isErrorResult, isResult, mapSdkMessage, orphanInstanceKind, sessionIdOf } from "./sdk-messages.js";
import { PlanRejectedError } from "./executor.js";
import { errMessage } from "./util.js";

// Fallbacks used only when the claim omits `config`. Wire units are SECONDS
// (PRD §Configuration); converted to ms at the timer.
const DEFAULT_RUN_TIMEOUT_SECONDS = 2 * 60 * 60; // 2h
const DEFAULT_IDLE_TIMEOUT_SECONDS = 10 * 60; // 10m
const DEFAULT_MAX_ITERATIONS = 5; // PRD: RUN_MAX_ITERATIONS default 5
// PRD #41: the plan-revision cap (PLAN_MAX_REVISIONS, default 3). The server also
// enforces it at submit time (rejects the 4th revise), so the worker counter is a
// belt-and-suspenders guard that rarely trips.
const DEFAULT_MAX_REVISIONS = 3;
// PRD #72 M4: transport clamp on a declared PRD path. Mirrors
// `prdpath.MaxPathLen` (api/internal/prdpath), which is the AUTHORITY — this
// copy only keeps an absurd string off the wire. Keep the two in step.
const PRD_DONE_PATH_MAX_LEN = 512;

// Static (content-free) failure reasons — safe to persist as failure_reason.
const REASON_WALL = "run exceeded its wall-clock timeout";
const REASON_IDLE = "run stalled: no agent activity within the idle timeout";
const REASON_CANCELLED = "run cancelled";
const REASON_NO_TOKEN = "no Anthropic OAuth token was provided for this run";
const REASON_NO_PLAN = "the agent ended the planning turn without submitting a plan";
const REASON_MAX_ITERATIONS = "run reached the maximum implement/review iterations without completing";

/**
 * Injectable seam over the SDK's `query`. The return only needs to be async
 * iterable for the executor; cancellation goes through `options.abortController`
 * and the process-group kill uses the pid captured by `spawnClaudeCodeProcess`.
 */
export type SdkQueryFn = (params: {
  prompt: AsyncIterable<unknown>;
  options: SdkOptions;
}) => AsyncIterable<SDKMessage>;

const defaultQueryFn: SdkQueryFn = (params) =>
  sdkQuery({ prompt: params.prompt as never, options: params.options });

export interface SdkExecutorOptions {
  /** Override the SDK entrypoint (tests inject a fake transport here). */
  queryFn?: SdkQueryFn;
  /** Spawn the SDK CLI (default = detached spawn). Injected in tests. */
  spawn?: (opts: SpawnOptions) => { pid?: number };
  /** Group-kill a pid (default = killProcessGroup). Injected in tests. */
  kill?: (pid: number | undefined) => boolean;
  /** Worker-credential file paths (UZI_WORKER_TOKEN_FILE) the Bash guard denies. */
  secretPaths?: readonly string[];
  /** Root for per-run tool-provisioning dirs (PRD #18 M3), OUTSIDE any clone.
   *  Default: a `provision/` sibling of the provisioning HOME on the data volume. */
  provisionRoot?: string;
  /**
   * SHARED (worker-lifetime) HOME for the tool-provisioning subprocess — the nix
   * single-user profile + devbox state that warm-start relies on (PRD #18; PRD #42
   * Decision 5). Deliberately distinct from the per-run SDK `homeDir`: a per-run
   * provisioning HOME would give every run a cold nix profile and fragment
   * warm-start state, while buying nothing (the per-run provision DIR under
   * `provisionRoot/<runId>` already isolates the synthesized devbox.json, and the
   * nix store is global). Defaults to `homeDir` for callers (tests) that pass one
   * HOME and don't provision; main.ts passes the shared root explicitly.
   */
  provisionHomeDir?: string;
  /** Devbox provisioning fn (PRD #18 M3). Injected in tests so no real nix egress
   *  happens; default = provisionTools. */
  provision?: typeof provisionTools;
  /** The worker's resolved docker wiring (PRD #83 M1 keystone), computed ONCE at
   *  startup (docker-wiring.ts) and the same for every run. Absent/`{}` ⇒ no daemon:
   *  the Bash guardrail denies docker and no DOCKER_HOST reaches the SDK env. Present
   *  ⇒ docker is allowed and DOCKER_HOST is injected. */
  dockerWiring?: DockerWiring;
  /** The worker→API client (PRD #90): threaded so the lead's `save_memory` MCP tool
   *  can POST a cross-run learning. Absent ⇒ no memory server is registered (tests/
   *  stubs that never call it), so the tool wiring is additive and back-compatible. */
  client?: WorkerClient;
  /** Install the cloned repo's JS dependencies (PRD #121 M2); default = installJsDeps.
   *  Injected in tests so no package manager is ever spawned and the kick-off/join
   *  ordering is drivable. */
  installDeps?: typeof installJsDeps;
}

/** What one turn observed: the session id, and any workflow signals. */
interface TurnResult {
  sessionId?: string;
  plan?: string;
  done: boolean;
  /** PRD #72 M4: the PRD path the lead declared on signal_done, if any. */
  prdDonePath?: string;
}

/** Run-level watchdog/cancel state shared across the plan turn and every loop turn. */
interface RunDrive {
  tripReason?: string;
  currentAbort?: AbortController;
  currentChild: { pid?: number };
  wallRemainingMs: number;
  wallArmedAt?: number;
  wallTimer?: NodeJS.Timeout;
  reportedSessionId: boolean;
}

export class SdkExecutor implements Executor {
  private readonly queryFn: SdkQueryFn;
  private readonly spawn: (opts: SpawnOptions) => { pid?: number };
  private readonly kill: (pid: number | undefined) => boolean;
  private readonly secretPaths: readonly string[];
  private readonly provisionRoot: string;
  private readonly provisionHomeDir: string;
  private readonly provision: typeof provisionTools;
  private readonly installDeps: typeof installJsDeps;
  /** Resolved once from the worker's docker wiring (PRD #83 M1). `dockerWired` gates
   *  the Bash guardrail; `dockerHost` is injected into the SDK env when present. */
  private readonly dockerWired: boolean;
  private readonly dockerHost?: string;
  /** The worker→API client for the lead's save_memory tool (PRD #90); undefined in
   *  tests/stubs that never register it. */
  private readonly client?: WorkerClient;
  /** Every pid spawned across the current run's turns, for the done-path reap.
   *  Private to THIS instance — one SdkExecutor is built per run (PRD #42 Decision
   *  4), so two concurrent runs can never wipe/kill each other's set. */
  private readonly spawnedPids = new Set<number>();

  /**
   * @param homeDir per-run SDK HOME (`agent-home/<runId>` on $UZI_DATA_DIR, PRD #42
   *   Decision 5) so the SDK's process-global $HOME/.claude state (session
   *   transcripts, history, todos, shell snapshots) can't race or leak between
   *   concurrent runs and survives a container restart for resume. The nix/devbox
   *   provisioning HOME is SEPARATE and shared (opts.provisionHomeDir).
   */
  constructor(
    private readonly log: Logger,
    private readonly homeDir: string,
    opts: SdkExecutorOptions = {},
  ) {
    this.queryFn = opts.queryFn ?? defaultQueryFn;
    this.spawn = opts.spawn ?? spawnDetached;
    this.kill = opts.kill ?? killProcessGroup;
    this.secretPaths = opts.secretPaths ?? [];
    // Provisioning HOME + root are SHARED worker-lifetime paths (Decision 5): they
    // must NOT be derived from the per-run SDK homeDir, or the nix profile/devbox
    // warm-start state would fragment per run. Per-run provisioning dirs live under
    // provisionRoot/<runId>, OUTSIDE any clone (Decision 3), so the synthesized
    // devbox.json is never repo-borne.
    this.provisionHomeDir = opts.provisionHomeDir ?? this.homeDir;
    this.provisionRoot = opts.provisionRoot ?? path.join(path.dirname(this.provisionHomeDir), "provision");
    this.provision = opts.provision ?? provisionTools;
    this.installDeps = opts.installDeps ?? installJsDeps;
    // Docker wiring (PRD #83 M1): derive the two consumer facts ONCE. `dockerHost` set
    // ⇒ a sidecar daemon is reachable, so the guardrail allows docker and DOCKER_HOST is
    // injected into the SDK env; absent ⇒ docker is denied and never injected.
    this.dockerHost = opts.dockerWiring?.dockerHost;
    this.dockerWired = this.dockerHost !== undefined;
    this.client = opts.client;
  }

  /**
   * Group-kill every subprocess the agent spawned across this run's turns (M4
   * audit B1). The default per-turn `abort()` only signals the SDK CLI pid, not
   * its process group, so an agent-backgrounded process (`nohup … &`) survives —
   * and could read the PAT out of the git child's /proc/environ during the
   * worker's push. The runner calls this before pushBranch; run()'s finally also
   * calls it so no path leaks an orphan. Idempotent: the set is cleared so a
   * recycled pid is never re-signalled.
   */
  killAgentTree(): void {
    for (const pid of this.spawnedPids) this.kill(pid);
    this.spawnedPids.clear();
  }

  /**
   * Log every dropped skill as a run message (PRD #16): the server's assembly
   * drops that rode the claim (ctx.skillsDropped — shadowed / over-limit) plus the
   * worker's own local cap drops (too_large / over_limit over the combined set).
   * The worker owns the gapless per-run seq, so the SERVER never writes these; it
   * hands the worker the {name, reason} list to emit. One status line per drop.
   */
  private emitSkillDrops(ctx: RunContext, localDrops: readonly SkillDrop[]): void {
    const all: SkillDrop[] = [...(ctx.skillsDropped ?? []), ...localDrops];
    for (const d of all) {
      ctx.emit({ kind: "status", agent: "worker", payload: { text: describeSkillDrop(d.name, d.reason) } });
    }
  }

  async run(ctx: RunContext): Promise<ExecutorResult> {
    this.spawnedPids.clear();
    const oauthToken = ctx.oauthToken?.trim();
    if (!oauthToken) {
      // OAuth is the sole supported credential (no API keys). Detect its
      // absence up front and fail fast rather than spawning a doomed CLI.
      throw new Error(REASON_NO_TOKEN);
    }

    await fs.mkdir(this.homeDir, { recursive: true });
    // The shared provisioning HOME lives elsewhere on the data volume; ensure it
    // exists before the install subprocess sets HOME to it (a no-op when it equals
    // the per-run homeDir, e.g. tests that don't split them).
    if (this.provisionHomeDir !== this.homeDir) {
      await fs.mkdir(this.provisionHomeDir, { recursive: true });
    }

    // Tool provisioning (PRD #18 M3): before the SDK starts, install the run's
    // tier-1 (∪ opted-in tier-2) packages in a secret-scrubbed subprocess and fold
    // the resulting (allowlisted) tool env into the SDK env. No packages ⇒ exactly
    // today's behavior. A provision failure FAILS the run — never silent
    // degradation. Shared with the stub executor (provision-run.ts) so the two
    // never drift. HOME here is the SHARED provisioning HOME (warm-start), NOT the
    // per-run SDK homeDir (Decision 5).
    const { toolEnv, provisionDir } = await provisionRunTools(ctx, {
      provisionRoot: this.provisionRoot,
      homeDir: this.provisionHomeDir,
      log: this.log,
      provision: this.provision,
    });

    // JS dependency provisioning (PRD #121 M2). Kicked off HERE — after
    // provisionRunTools, so the install resolves the RUN's provisioned node/npm off
    // toolEnv's PATH rather than the image's, and before the plan turn, so it overlaps
    // the plan turn and (on a human-gated run) the whole `awaiting_approval` wait.
    // JOINED before the first implement turn, below. NOT awaited here: awaiting would
    // throw the overlap away, which is the entire wall-clock argument for doing this.
    const depsAbort = new AbortController();
    const depsInstall = this.startDepsInstall(ctx, toolEnv, depsAbort.signal);
    // The install's per-dir verdicts, kept alive to the END of the run rather than
    // consumed and dropped at the join. The install fires BEFORE the plan turn, so by
    // the time anything downstream asks "were the deps actually there?" the answer is
    // long out of scope — and that question is precisely what gate honesty (PRD #121 M4,
    // split out) has to answer to write a reason line a reviewer can act on. Deliberately
    // INTERNAL: `ExecutorResult` gains no field for it while nothing in this PRD consumes
    // one. The precedent for opening that door cheaply is `toolEnv`, which rides
    // ExecutorResult for exactly this kind of executor-computed state (runner.ts).
    let depsResults: JsDepsResult[] = [];

    const env = buildSdkEnv(oauthToken, this.homeDir, toolEnv, this.dockerHost);
    const maxIterations = positive(ctx.config?.max_iterations, DEFAULT_MAX_ITERATIONS);
    const maxRevisions = planMaxRevisionsOf(ctx.config);

    // Skills (PRD #16 M4 + M6). Assemble the run's skill set and materialize a
    // local plugin dir OUTSIDE the clone (loads under `settingSources: []`, so the
    // injection defense never loosens), rebuilt on every claim incl. resume. The
    // same prepareSkillPlugin path the stub executor uses, so the two never drift.
    // The worker owns the gapless seq, so it logs every dropped skill.
    const prepared = await prepareSkillPlugin(ctx, resolveSkillCaps(ctx.config));
    const runSkills = prepared.runSkills;
    const skillsPluginPath = prepared.pluginPath;
    this.emitSkillDrops(ctx, prepared.drops);

    // Subagents: each def.skills is its allocated delivered skills (re-filtered to
    // the materialized survivors, so it never lists a uzi:<name> not in the plugin
    // dir) plus the all-templates repo survivors (PRD §Worker point 3).
    const survivorNames = new Set(runSkills.map((s) => s.name));
    const assembled = assembleAgents(ctx.agents ?? [], survivorNames, prepared.repoSurvivorNames);
    // The plan turn runs with the OWN subagents (PRD #37 Decision 5): the roster
    // choice only takes effect after the plan is approved.
    const ownSubagentNames = Object.keys(assembled.subagents);

    // The Bash + path-guard hooks are roster-independent, so build them once and
    // reuse; only the Agent-guard hook's allowSet is frozen at construction and so
    // must be REBUILT when the implement roster differs from the plan roster (PRD
    // #37 — an excluded or repo-sourced subagent must be denied by the guard).
    const bashHook = buildPreToolUseHook(this.log, this.secretPaths, this.dockerWired);
    const pathHook = buildPathGuardHook(ctx.worktreePath, this.log);
    const preToolUse = (allowedSubagents: string[]): NonNullable<SdkOptions["hooks"]>["PreToolUse"] => [
      { matcher: "Bash", hooks: [bashHook] },
      { matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep", hooks: [pathHook] },
      { matcher: NESTED_AGENT_TOOL, hooks: [buildAgentGuardHook(allowedSubagents, this.log)] },
    ];

    // In-process MCP servers the LEAD (full toolset) reaches. The dep-free signal
    // server (plan gate + done) is always present; the memory server (PRD #90) is
    // added ONLY when a client was threaded (main.ts), surfacing save_memory as
    // `mcp__memory__save_memory`. Registered under a DISTINCT key from the signal
    // server so neither disturbs the other; the lead sets no `tools` allowlist, so a
    // registered MCP tool is callable, and save_memory is not in disallowedTools.
    // PRD #72 M4: `prd_done_path` is exposed on signal_done for `issue` runs only
    // (Decision 13). `?? "issue"` matches runner.ts's own `kind: claim.kind ??
    // "issue"` default; a stricter fail-closed default here would silently break
    // every test that omits kind, and the AUTHORITATIVE gate is the api's, where
    // runs.kind is NOT NULL.
    const isIssueRun = (ctx.kind ?? "issue") === "issue";
    const mcpServers: Record<string, McpSdkServerConfigWithInstance> = {
      [SIGNAL_SERVER_NAME]: buildSignalMcpServer({ prdDonePath: isIssueRun }),
    };
    if (this.client) {
      mcpServers[MEMORY_SERVER_NAME] = buildMemoryServer({ client: this.client, runId: ctx.runId, log: this.log }).server;
    }

    const baseOptions: SdkOptions = {
      cwd: ctx.worktreePath,
      // Full replacement — only these keys reach the agent subprocess.
      env: env as unknown as Record<string, string | undefined>,
      // Repo-borne prompt-injection defense: nothing from the cloned repo's
      // .claude/{settings.json,agents,hooks} can grant the agent permissions.
      settingSources: [],
      // Skills (PRD #16 M4): a local plugin dir OUTSIDE the clone, loaded
      // independently of settingSources (SdkPluginConfig is a separate option),
      // so the isolation above never loosens. skipMcpDiscovery: this plugin ships
      // ONLY skills — the SDK host owns MCP, so never read a manifest/.mcp.json.
      plugins: [{ type: "local", path: skillsPluginPath, skipMcpDiscovery: true }],
      // ALWAYS an explicit list — the full plugin-qualified run union. Omitting it
      // is NOT "skills off" (sdk.d.ts:1872: CLI defaults would apply); `[]` when the
      // run has no skills disables all. Per-subagent scoping is each
      // AgentDefinition.skills; the lead is the main thread, covered by this union.
      skills: runSkills.map((s) => qualifiedSkillName(s.name)),
      systemPrompt: buildLeadSystemPrompt(assembled.leadSystemPrompt, { kind: ctx.kind }),
      agents: assembled.subagents,
      // In-process tools the lead calls: the signal server (gate the plan / mark
      // done, see signals.ts) plus, when a client is threaded, the memory server
      // (save_memory, PRD #90). Only the lead (full toolset) reaches them.
      mcpServers,
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      // Block the deferral tools so the lead can't background work to a future
      // turn that the per-turn reap would only wake to a killed subagent (#34).
      // Delegation is forced synchronous by the Agent guard hook below.
      disallowedTools: [...ASYNC_DEFERRAL_TOOLS],
      // The load-bearing deny layer: a PreToolUse deny blocks a tool even under
      // bypassPermissions. Bash screening, the file-tool path jail, AND the M4
      // hard-fail-on-unexpected-subagent guard (item 7) all live here. The plan
      // turn allows the OWN subagents; the implement turns rebuild this with the
      // selected roster (PRD #37).
      hooks: {
        PreToolUse: preToolUse(ownSubagentNames),
      },
      // Persist discrete blocks only; partial token deltas would flood the seq
      // stream (the live-partial channel is M5, not M3).
      includePartialMessages: false,
    };
    // Model precedence (PRD #17 Decision 6): the run owner's per-user default
    // model wins over the lead template's model for the main thread. Set the key
    // ONLY when a model is resolved — `model: undefined` must stay omitted (never
    // an explicit key), so an unset model falls back to the SDK/account default.
    const leadModel = resolveLeadModel(ctx.config?.default_model, assembled.leadModel);
    if (leadModel) baseOptions.model = leadModel;

    const state: RunDrive = {
      currentChild: {},
      wallRemainingMs: seconds(ctx.config?.run_timeout_seconds, DEFAULT_RUN_TIMEOUT_SECONDS),
      reportedSessionId: false,
    };
    const idleMs = seconds(ctx.config?.idle_timeout_seconds, DEFAULT_IDLE_TIMEOUT_SECONDS);

    // Cancel/shutdown spans the whole run (all turns + the gate). It trips the
    // current turn's abort; the gate also unblocks via the steering verdict. It also
    // reclaims the dependency install (PRD #121 M2) — a cancelled run must not sit at
    // the join waiting out an install whose results nobody will read.
    const onSignal = (): void => {
      this.trip(state, REASON_CANCELLED);
      depsAbort.abort();
    };
    if (ctx.signal) {
      if (ctx.signal.aborted) onSignal();
      else ctx.signal.addEventListener("abort", onSignal, { once: true });
    }

    // The SDK session id evolves across turns; resume each turn from the last.
    let resumeId = ctx.sessionId ?? undefined;

    try {
      // --- Phase 1: planning turn ------------------------------------------
      // A ci_fix run (PRD #6) diagnoses a failed pipeline from its frozen snapshot
      // (untrusted job logs) instead of a forge issue; everything else — the plan
      // gate, the implement⇄review loop, the guardrails — is identical.
      const isCIFix = ctx.kind === "ci_fix" && ctx.pipeline != null;
      const isSelfImprove = ctx.kind === "self_improve";
      let planPrompt: string;
      if (isCIFix) {
        planPrompt = buildCIFixPlanPrompt({
          ref: ctx.pipeline!.ref,
          branch: ctx.branch,
          pipelineWebURL: ctx.pipeline!.web_url,
          failedJobs: ctx.pipeline!.failed_jobs.map((j) => ({ name: j.name, stage: j.stage, logTail: j.log_tail })),
          subagentNames: ownSubagentNames,
          // PRD #90: a ci_fix run can WRITE memory, so it reads the same inert,
          // nonce-fenced cross-run memory back (empty/absent injects nothing).
          memory: ctx.memory,
          // Issue #105: only set when a dropped resume left this turn amnesiac on a
          // branch that already carries pushed work.
          priorWork: ctx.priorWork,
        });
      } else if (isSelfImprove) {
        // The self_improve run's issue_description carries the untrusted improve_uzi
        // backlog; the trusted "pick one / guardrails / tests" directive lives in the
        // prompt builder, outside the untrusted fence (PRD #46 Decision 10, audit C1).
        planPrompt = buildSelfImprovePlanPrompt({
          branch: ctx.branch,
          recommendations: ctx.issueDescription,
          subagentNames: ownSubagentNames,
          // PRD #90: a self_improve run can WRITE memory, so it reads the same inert,
          // nonce-fenced cross-run memory back (empty/absent injects nothing).
          memory: ctx.memory,
          // Issue #105: see above — the fixed self_improve branch's prior cycles.
          priorWork: ctx.priorWork,
        });
      } else {
        planPrompt = buildPlanPrompt({
          issueIid: ctx.issueIid ?? 0,
          issueTitle: ctx.issueTitle,
          issueDescription: ctx.issueDescription,
          branch: ctx.branch,
          subagentNames: ownSubagentNames,
          // PRD #90: inert, nonce-fenced, untrusted-advisory cross-run memory (the
          // runner fetched it at claim time; empty/absent injects nothing).
          memory: ctx.memory,
          // Issue #105: see above — prior pushed work on this issue's branch.
          priorWork: ctx.priorWork,
        });
      }
      const planningLabel = isCIFix ? "diagnosing CI failure" : isSelfImprove ? "planning self-improvement" : "planning";
      ctx.emit({ kind: "status", agent: "worker", payload: { text: `starting SDK agent (${planningLabel})` } });
      const plan = await this.driveTurn(ctx, baseOptions, resumeId, planPrompt, state, idleMs);
      resumeId = plan.sessionId ?? resumeId;
      // A planning turn that ends without a plan is an error — never push
      // un-gated work, even if the lead prematurely signalled done.
      if (plan.plan === undefined) throw new Error(REASON_NO_PLAN);

      // --- Plan gate (+ revision loop, PRD #41) -----------------------------
      // The gate can be re-entered N times under ONE approval budget: a `revise`
      // verdict carries the reviewer's feedback, and the worker runs a fresh planning
      // turn (resumed, so the planning context is retained) and re-gates the new plan.
      // approve/reject/cancel are terminal; the runner advances the steering epoch each
      // time a revise is taken. FAIL-CLOSED: the while-condition + the post-loop guards
      // guarantee the only way past this block is an `approve` (see the explicit guard).
      if (!ctx.gatePlan) throw new Error("plan gate is not wired for this run");
      let approvedPlan = plan.plan;
      let verdict = await ctx.gatePlan(approvedPlan);
      let revisions = 0;
      while (verdict.kind === "revise") {
        const feedback = verdict.feedback;
        // Record the reviewer's feedback on the feed. Ordered BEFORE the revision turn
        // (and thus before the next gatePlan flushes the new plan), so the feed never
        // lags the awaiting_approval re-report.
        ctx.emit({ kind: "plan_feedback", agent: "worker", payload: { feedback } });
        // Belt-and-suspenders (PRD #41 Decision 3c): the SERVER enforces the same cap at
        // submit time and won't enqueue a revise past it, so this should never trip. If it
        // does, DO NOT run another planning turn — record it and re-gate the current plan so
        // the run stays fail-closed rather than revising unbounded.
        if (revisions >= maxRevisions) {
          this.log.warn("plan revision budget exhausted; re-gating without a turn", { run_id: ctx.runId, max_revisions: maxRevisions });
          ctx.emit({ kind: "status", agent: "worker", payload: { text: "revision budget exhausted — not revising the plan further" } });
          verdict = await ctx.gatePlan(approvedPlan);
          continue;
        }
        revisions++;
        ctx.emit({ kind: "plan_revising", agent: "worker", payload: { round: revisions } });
        // A revision turn is a PLANNING turn (pre-approval), so it runs with the OWN
        // subagents (baseOptions), exactly like the first plan turn — the roster
        // selection only takes effect once a plan is APPROVED (PRD #37 Decision 5).
        const turn = await this.driveTurn(ctx, baseOptions, resumeId, buildRevisePlanPrompt(feedback), state, idleMs);
        resumeId = turn.sessionId ?? resumeId;
        // A revision turn that submits no plan fails, same as the first planning turn —
        // never push un-gated work.
        if (turn.plan === undefined) throw new Error(REASON_NO_PLAN);
        approvedPlan = turn.plan;
        verdict = await ctx.gatePlan(approvedPlan);
      }
      if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
      if (verdict.kind === "cancel") throw new Error(REASON_CANCELLED);
      // FAIL-CLOSED: the loop + guards above leave only `approve`; any other kind is a
      // bug (e.g. a future verdict variant) and must never fall through into implement.
      if (verdict.kind !== "approve") throw new Error(`unexpected plan verdict: ${(verdict as { kind: string }).kind}`);

      // A ci_fix run whose approved plan is a not_code verdict is done: no code to
      // implement, no branch to push. The run completes with the diagnosis as its
      // value (PRD #6). Detected AFTER approval so a human confirmed the verdict.
      if (isCIFix && isNotCodePlan(approvedPlan)) {
        ctx.emit({ kind: "status", agent: "worker", payload: { text: "diagnosis: not a code problem — completing with no fix" } });
        this.log.info("ci_fix run: not_code verdict", { run_id: ctx.runId });
        return { branch: ctx.branch, fixVerdict: "not_code" };
      }

      // --- Join the JS dependency install (PRD #121 M2) ---------------------
      // The IMPLEMENT phase must never race the install. Past this point the agent
      // runs `npm test` / `vitest` / `tsc`, and will run its own `npm ci` if it thinks
      // deps are missing; npm has NO cross-process `node_modules` lock, so a concurrent
      // worker-side install in the same dir would corrupt the tree. Joining here is what
      // makes that impossible FOR THE IMPLEMENT TURNS — placed after the not_code return
      // above, so a ci_fix that never implements does not wait for deps it will not use.
      //
      // RESIDUAL, AND IT IS NOT CLOSED BY THIS JOIN: the PLAN turn can do the same thing.
      // It runs under `permissionMode: "bypassPermissions"` with full Bash, and
      // guardrails.ts has no package-manager rule of any kind (verified: it screens git
      // push/history/config, credential reads, /proc, secret paths, `env` and docker —
      // nothing about npm/pnpm/yarn/bun). So a planning agent exploring the repo can run
      // `npm ci` in a dir this install is mid-flight in. That window is DELIBERATE — the
      // overlap is the entire wall-clock argument for starting before the plan turn — so
      // it is named here rather than fixed; closing it would mean giving the overlap up.
      //
      // Worth knowing what it costs, because it is worse than "the deps are missing": a
      // half-written `node_modules` still EXISTS, so `defaultCheckRunner`'s
      // `requires: "node_modules"` pre-flight passes, the check runs against a corrupt
      // tree, and it FAILS — accusing good code of failing, which is the one thing
      // self-improve.ts's status mapping exists to prevent.
      depsResults = await this.joinDepsInstall(ctx, depsInstall);

      // --- Apply the agent selection at the gate boundary (PRD #37 Decision 5) ---
      // The plan turn ran with the OWN subagents; the approved selection now decides
      // the roster for the implement phase. The lead ALWAYS stays uzi's builtin from
      // the claim, so only the subagents change: rebuild the agents map, the
      // Agent-guard hook (its allowSet is frozen at construction — an excluded or
      // repo-sourced subagent must be denied), the lead system prompt (a repo-source
      // run adds the untrusted-review passage), and the subagent names fed to the
      // implement prompt. Malformed selection resolves to `own`, never repo.
      const repoAvailable = (ctx.repoAgents?.length ?? 0) > 0;
      const resolved = resolveAgentSelection(verdict.selection, repoAvailable);
      if (resolved.note) ctx.emit({ kind: "status", agent: "worker", payload: { text: resolved.note } });
      const selection = resolved.selection;
      // Skill scoping is source-dependent from here (PRD #72 M1): `own` keeps the
      // per-template allocations assembled above; `repo` gives every subagent the
      // full survivor set, since a repo roster has no template rows to allocate
      // against. survivorNames is the filter in both cases, so a skill dropped by
      // the cap or a collision is unreachable either way.
      const selectedSubagents = selectSubagents(
        selection.source,
        assembled.subagents,
        ctx.repoAgents ?? [],
        selection.exclusions,
        survivorNames,
        prepared.repoSurvivorNames,
      );
      const selectedNames = Object.keys(selectedSubagents);
      const implementOptions: SdkOptions = {
        ...baseOptions,
        agents: selectedSubagents,
        systemPrompt: buildLeadSystemPrompt(assembled.leadSystemPrompt, { repoSourced: selection.source === "repo", kind: ctx.kind }),
        hooks: { PreToolUse: preToolUse(selectedNames) },
      };
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text:
            selection.source === "repo"
              ? `implementing with the repo's agents (${selectedNames.join(", ") || "none"})`
              : `implementing with your agent templates (${selectedNames.join(", ") || "none"})`,
        },
      });

      // --- Phase 2: implement ⇄ review loop --------------------------------
      let iteration = 0;
      let followUp: string | undefined;
      // Hoisted: `turn` is declared INSIDE the loop, so the return below cannot see
      // it and the terminating turn's declaration would be discarded by `break`.
      let declaredPrdPath: string | undefined;
      for (;;) {
        iteration++;
        ctx.reportIteration?.(iteration);
        ctx.emit({ kind: "status", agent: "worker", payload: { text: `implement/review iteration ${iteration}` } });
        const turn = await this.driveTurn(ctx, implementOptions, resumeId, buildImplementPrompt({
          branch: ctx.branch,
          subagentNames: selectedNames,
          first: iteration === 1,
          iteration,
          followUp,
        }), state, idleMs);
        resumeId = turn.sessionId ?? resumeId;
        if (turn.prdDonePath !== undefined) declaredPrdPath = turn.prdDonePath;
        if (turn.done) break;
        if (iteration >= maxIterations) throw new Error(REASON_MAX_ITERATIONS);
        // Fold any queued correction into the next turn (FIFO, one per turn).
        followUp = ctx.pullFollowUp?.();
      }

      // js_deps rides the completion log so a finished run's record says whether its
      // gates were runnable — the same question M4 will have to answer from a durable
      // source. It is also what keeps depsResults READ rather than merely assigned.
      this.log.info("SDK run completed", {
        run_id: ctx.runId,
        branch: ctx.branch,
        agent_source: selection.source,
        js_deps: depsResults.map((r) => ({ dir: r.dir, ok: r.ok })),
      });
      // toolEnv (PRD #46 M9): the allowlisted provisioned tool env, so the self_improve
      // check runner can put the run's provisioned toolchains on its subprocess PATH.
      const result: ExecutorResult = { branch: ctx.branch, agentSelection: { source: selection.source, agents: selectedNames }, toolEnv };
      // PRD #72 M4: forward the declared PRD path on `issue` runs only, clamped to
      // length. TRANSPORT HYGIENE ONLY — no path-shape checks here. The api owns
      // the grammar (api/internal/prdpath), and a second implementation of it would
      // drift silently in both directions.
      //
      // The key is OMITTED, never set to undefined: `runner.ts` spreads this into
      // the state report, and an own `prdDonePath: undefined` would both change the
      // shape every existing deepStrictEqual on this result asserts and blur the
      // wire distinction between "declared nothing" and "field present but empty".
      if (isIssueRun && declaredPrdPath !== undefined) {
        result.prdDonePath = declaredPrdPath.slice(0, PRD_DONE_PATH_MAX_LEN);
      }
      return result;
    } finally {
      this.disarmWall(state);
      if (ctx.signal) ctx.signal.removeEventListener("abort", onSignal);
      // PRD #121 M2: no install may outlive the run. On every path that never reached
      // the join — plan rejected, cancelled, no plan submitted, ci_fix not_code — the
      // install may still be in flight, and the runner tears the clone down and pushes
      // with the PAT the moment this returns. Abort FIRST so the await is bounded by a
      // kill rather than by the install's own 10-minute cap (that is what stops a
      // rejected plan blocking on deps nobody needs); the promise already carries its
      // own catch, so awaiting it can never throw here and mask the real failure.
      depsAbort.abort();
      await depsInstall;
      // Reap every agent subprocess before returning, so none survives into the
      // worker's PAT-bearing push (B1). Covers the failure/cancel/no-plan paths
      // too, not just the runner's explicit pre-push call.
      this.killAgentTree();
      // Remove the per-run provisioning dir (the synthesized devbox.json + profile
      // symlinks). The nix STORE is global (on the data volume), NOT here, so this
      // never evicts the warm-start cache. Best-effort.
      if (provisionDir) await fs.rm(provisionDir, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  /**
   * Start provisioning the clone's JS dependencies (PRD #121 M2), returning a promise
   * that NEVER rejects.
   *
   * The `.catch` is attached HERE, at creation, not at the join — between the two the
   * promise is floating, and a rejection reaching an empty microtask queue is an
   * unhandled rejection that kills the worker process. `installJsDeps` is contracted
   * never to throw, but that contract belongs to the module; this call site must not
   * depend on it holding.
   *
   * ENV: the same scrubbed REPLACEMENT env the self-improve checks use (`buildCheckEnv`)
   * — never a `process.env` spread. The install executes repo-authored package.json /
   * lockfile resolution, so the worker's join token, API URL, forge PAT and OAuth token
   * are absent by construction; PATH comes from the run's provisioned toolEnv so the
   * install uses the RUN's node/npm. HOME is the PER-RUN SDK home, matching what
   * runner.ts already passes for the self-improve checks. Per-run and not the shared
   * provisioning HOME on purpose: a shared HOME would warm the npm cache across runs,
   * but every run's install writes it under the same `runner` uid, so one run could seed
   * content a later run installs. A cold cache per run is the cheaper side of that
   * trade, and the run's HOME is torn down with the run.
   */
  private startDepsInstall(
    ctx: RunContext,
    toolEnv: Record<string, string> | undefined,
    signal: AbortSignal,
  ): Promise<JsDepsInstall> {
    ctx.emit({ kind: "status", agent: "worker", payload: { text: "installing the repo's JS dependencies (in the background)" } });
    return this.installDeps(ctx.worktreePath, buildCheckEnv(process.env, this.homeDir, toolEnv), { signal }).catch(
      (err: unknown) => {
        // Best-effort, always: provisioning can never fail the run. Unlike
        // provisionRunTools (whose failure DOES fail the run — the agent would be
        // missing its declared toolchain), a missing node_modules degrades to the
        // agent installing them itself, exactly as it does today.
        this.log.warn("JS dependency provisioning failed", { run_id: ctx.runId, error: errMessage(err) });
        return { results: [], truncated: false };
      },
    );
  }

  /**
   * Wait for the dependency install and report what it did on the run's feed. Never
   * throws: the promise carries its own catch, and a provisioning result — however bad —
   * is information for the user, not a run failure.
   */
  private async joinDepsInstall(ctx: RunContext, depsInstall: Promise<JsDepsInstall>): Promise<JsDepsResult[]> {
    const { results, truncated } = await depsInstall;
    if (results.length === 0) {
      ctx.emit({ kind: "status", agent: "worker", payload: { text: "no JS dependencies to install (no lockfile found)" } });
      return results;
    }
    // One line, naming every dir and — for anything that did not install — why. A
    // silent skip here resurfaces later as an inexplicable `vitest: not found`.
    const installed = results.filter((r) => r.ok).map((r) => safeDirLabel(r.dir));
    const skipped = results.filter((r) => !r.ok);
    const parts: string[] = [];
    if (installed.length > 0) parts.push(`installed JS dependencies in ${installed.join(", ")}`);
    for (const s of skipped) parts.push(`${safeDirLabel(s.dir)}: ${s.detail}`);
    // Truncation goes on the FEED, not just in a log: without it the line above reads as
    // full coverage, and a `vitest: not found` in dir 13 becomes unexplainable.
    if (truncated) parts.push("discovery hit its directory bound — some project dirs were not installed");
    ctx.emit({ kind: "status", agent: "worker", payload: { text: parts.join(" — ") } });
    this.log.info("JS dependency provisioning", { run_id: ctx.runId, results, truncated });
    return results;
  }

  /** Drive ONE SDK turn to its result frame, capturing signals + the session id. */
  private async driveTurn(
    ctx: RunContext,
    baseOptions: SdkOptions,
    resumeId: string | undefined,
    prompt: string,
    state: RunDrive,
    idleMs: number,
  ): Promise<TurnResult> {
    // A trip may already be pending (e.g. a cancel that landed during the gate).
    if (state.tripReason) throw new Error(state.tripReason);

    const abortController = new AbortController();
    state.currentAbort = abortController;
    state.currentChild = {};

    const options: SdkOptions = { ...baseOptions, abortController };
    if (resumeId) options.resume = resumeId;
    else delete options.resume;
    // Spawn the CLI in its own process group so a watchdog trip can group-kill
    // the whole tree (default SDK spawn is not detached).
    options.spawnClaudeCodeProcess = (spawnOpts: SpawnOptions): SpawnedProcess => {
      const proc = this.spawn(spawnOpts);
      if (typeof proc.pid === "number") {
        state.currentChild.pid = proc.pid;
        this.spawnedPids.add(proc.pid); // reaped on the done path (B1)
      }
      return proc as unknown as SpawnedProcess;
    };

    let idleTimer: NodeJS.Timeout | undefined;
    const armIdle = (): void => {
      if (idleTimer) clearTimeout(idleTimer);
      idleTimer = setTimeout(() => this.trip(state, REASON_IDLE), idleMs);
      idleTimer.unref?.();
    };

    const result: TurnResult = { done: false };
    let turnSessionId: string | undefined;
    let sawErrorResult = false;
    let errorSubtype = "unknown";

    this.armWall(state);
    // Budget already spent by earlier turns → fail now rather than run unbounded.
    if (state.tripReason) throw new Error(state.tripReason);
    try {
      armIdle();
      const queryInstance = this.queryFn({ prompt: promptStream(prompt), options });
      for await (const msg of queryInstance) {
        armIdle(); // any message is liveness

        const sid = sessionIdOf(msg);
        if (sid) {
          turnSessionId = sid;
          if (!state.reportedSessionId) {
            state.reportedSessionId = true;
            try {
              ctx.onSessionId?.(sid);
            } catch (err) {
              this.log.warn("onSessionId handler threw", { run_id: ctx.runId, error: errMessage(err) });
            }
          }
        }

        // Emit everything EXCEPT the signal tool_use blocks — the plan is
        // surfaced as a `plan` message by the runner, not duplicated as a raw
        // tool_use payload, and signal_done is infra noise. An assistant frame's
        // per-call usage (PRD #40 Decision 11) rides the FIRST message that
        // survives that filter — attached HERE, not in mapAssistant, which cannot
        // see this executor-side drop; every phase terminates on a signal frame, so
        // attaching earlier would systematically lose the lead's terminating-frame
        // usage. A frame whose messages are ALL filtered loses its usage (accepted).
        // Result-frame usage travels inside mapResult's payload instead, so it is
        // never re-attached here (assistantUsageOf is assistant-only, not results).
        // PRD #99: alarm on a frame that carries an invocation id but no role
        // field. Logged HERE rather than in the mapper because sdk-messages.ts is
        // a pure module with no logger, and this is the only executor that can
        // produce subagent frames at all (chat-executor and judge-runner both
        // prevent the Agent tool — chat via disallowedTools, judge via a deny-all
        // PreToolUse hook). The `kind` is the ONLY thing logged — no id, no label —
        // so nothing new reaches `docker logs`, keeping the deliberate omission at
        // batcher.ts's debug line intact.
        const orphanKind = orphanInstanceKind(msg);
        if (orphanKind !== undefined) {
          this.log.warn("frame carried parent_tool_use_id without subagent_type", {
            run_id: ctx.runId,
            kind: orphanKind,
          });
        }
        const frameUsage = assistantUsageOf(msg);
        let usageAttached = false;
        for (const em of mapSdkMessage(msg)) {
          if (em.kind === "tool_use" && isSignalToolName(em.payload["name"])) continue;
          if (frameUsage && !usageAttached) {
            em.payload["usage"] = frameUsage;
            usageAttached = true;
          }
          ctx.emit(em);
        }
        const sig = scanSignals(msg);
        if (sig.plan !== undefined) result.plan = sig.plan;
        if (sig.done) result.done = true;
        // Last-wins within the turn, mirroring `plan` rather than `done`'s latch:
        // if the lead somehow signals twice, the LAST declaration is the one that
        // describes the tree the worker is about to push (PRD #72 M4).
        if (sig.prdDonePath !== undefined) result.prdDonePath = sig.prdDonePath;

        if (isResult(msg)) {
          if (isErrorResult(msg)) {
            sawErrorResult = true;
            errorSubtype = ((msg as { subtype?: unknown }).subtype as string) ?? "unknown";
          }
          // The turn is done. Abort so a lingering background bash the agent left
          // running can't pin the iterator open (bottega's pattern).
          abortController.abort();
          break;
        }
      }

      if (state.tripReason) throw new Error(state.tripReason);
      if (sawErrorResult) throw new Error(`agent run failed: ${errorSubtype}`);
      result.sessionId = turnSessionId;
      return result;
    } catch (err) {
      // A watchdog/cancel trip surfaces as its static reason, not the raw
      // AbortError the aborted iterator throws.
      if (state.tripReason) throw new Error(state.tripReason);
      throw err instanceof Error ? err : new Error(errMessage(err));
    } finally {
      if (idleTimer) clearTimeout(idleTimer);
      this.disarmWall(state);
      state.currentAbort = undefined;
    }
  }

  /** Record a first-wins watchdog/cancel trip and stop the current turn. */
  private trip(state: RunDrive, reason: string): void {
    if (state.tripReason) return; // first trip wins
    state.tripReason = reason;
    this.log.warn("run watchdog/cancel tripped, aborting SDK", { reason });
    // abort() is the primary, asserted stop (SDK: stdin EOF → grace → signal);
    // the group kill is best-effort defense for orphaned children.
    state.currentAbort?.abort();
    this.kill(state.currentChild.pid);
  }

  /** Arm the wall-clock budget for the active turn (paused across the gate). */
  private armWall(state: RunDrive): void {
    if (state.wallTimer) return;
    if (state.wallRemainingMs <= 0) {
      this.trip(state, REASON_WALL);
      return;
    }
    state.wallArmedAt = Date.now();
    state.wallTimer = setTimeout(() => this.trip(state, REASON_WALL), state.wallRemainingMs);
    state.wallTimer.unref?.();
  }

  /** Pause the wall budget, debiting the elapsed active time. */
  private disarmWall(state: RunDrive): void {
    if (!state.wallTimer) return;
    clearTimeout(state.wallTimer);
    state.wallTimer = undefined;
    if (state.wallArmedAt !== undefined) {
      state.wallRemainingMs -= Date.now() - state.wallArmedAt;
      state.wallArmedAt = undefined;
    }
  }
}

/** One-shot prompt stream: the SDK consumes the lead's user turn. */
async function* promptStream(text: string): AsyncGenerator<unknown> {
  yield { type: "user", message: { role: "user", content: text }, parent_tool_use_id: null };
}

/** Convert an optional seconds value (any number tolerated) to ms. */
function seconds(value: number | undefined, fallback: number): number {
  const s = typeof value === "number" && value > 0 ? value : fallback;
  return Math.round(s * 1000);
}

/** A positive integer override, else the fallback. */
function positive(value: number | undefined, fallback: number): number {
  return typeof value === "number" && value > 0 ? Math.floor(value) : fallback;
}

/**
 * PRD #41: the worker-side plan-revision cap from the claim config. The server sends
 * `plan_max_revisions` in the claim config (workersvc claim.go) and ALSO enforces it
 * at submit time (rejects the 4th revise), so this worker counter is a belt-and-
 * suspenders guard. An explicit 0 (operator DISABLING revisions) is respected as 0 —
 * only an absent/undefined or negative/garbage value falls back to DEFAULT_MAX_REVISIONS,
 * so the worker counter mirrors operator intent (the server gates first, so this is
 * harmless today, but must not silently re-enable revisions a config disabled).
 */
function planMaxRevisionsOf(config: ClaimConfig | null | undefined): number {
  const v = config?.plan_max_revisions;
  if (typeof v === "number" && Number.isFinite(v) && v >= 0) return Math.floor(v);
  return DEFAULT_MAX_REVISIONS;
}

/** Human-readable run-message text for a dropped skill, by reason code. Unknown
 *  codes degrade to a generic line rather than dropping the log. */
function describeSkillDrop(name: string, reason: string): string {
  switch (reason) {
    case "shadowed":
      return `skill "${name}" was shadowed by a higher-precedence skill of the same name and will not be loaded`;
    case "over_limit":
      return `skill "${name}" was dropped: the run exceeded the maximum number of skills`;
    case "too_large":
      return `skill "${name}" was dropped: its body exceeds the maximum allowed size`;
    case "repo_collision":
      return `repo skill "${name}" was skipped: a higher-precedence skill of the same name is already loaded`;
    case "repo_invalid":
      return `repo skill "${name}" was skipped: invalid name, description, or body`;
    default:
      return `skill "${name}" was dropped (${reason})`;
  }
}

/**
 * Resolve the model for the lead/main thread (PRD #17 Decision 6): the run
 * owner's per-user default (`config.default_model`) wins over the lead template's
 * model. A blank config model falls back to the template (|| not ??), so a
 * defensively empty string can't blank out the template's model — in practice
 * the server sends NULL (omitted), never "". Returns undefined when nothing
 * resolves, so the caller omits the SDK `model` key entirely rather than sending
 * an explicit empty override (an unset model falls back to the SDK/account
 * default). Null-model subagents follow the main thread, so this governs them too.
 */
export function resolveLeadModel(configModel?: string, templateModel?: string): string | undefined {
  return configModel || templateModel || undefined;
}

/**
 * Render a discovered directory name for the run's activity feed. `dir` comes from
 * `readdir`, i.e. it is REPO-CONTROLLED text: a repo can commit a directory whose name
 * contains newlines, backticks, or instruction-shaped prose, and this string is
 * persisted to `run_messages` and rendered to a human. Not a path escape and React
 * escapes the HTML, but untrusted text should not be able to shape a status line, so the
 * charset is clamped to what a real project dir needs and the length is bounded.
 */
function safeDirLabel(dir: string): string {
  const cleaned = dir.replace(/[^A-Za-z0-9._/@-]/g, "?");
  return cleaned.length > 120 ? `${cleaned.slice(0, 120)}…` : cleaned;
}
