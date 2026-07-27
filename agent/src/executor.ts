import { execFile } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import type { AgentSelection, AgentSource, AgentTemplate, ClaimConfig, ClaimPipeline, ClaimSkill, ClaimSkillDrop, FixVerdict, MemoryEntry, MessageKind, RunKind } from "./protocol.js";
import type { PlanVerdict } from "./steering.js";
import type { PriorWork } from "./prompt.js";
import { prepareSkillPlugin, resolveSkillCaps } from "./skills-run.js";
import { LimitReachedError } from "./limit.js";
import { provisionRunTools } from "./provision-run.js";
import type { provisionTools } from "./provision.js";
import { gitEnv } from "./git.js";
import { runnerCommand, runnerPath, runnerTmpdir } from "./runner-uid.js";

const execFileAsync = promisify(execFile);

/** A message the executor emits to the run stream (seq assigned downstream). */
export interface EmittedMessage {
  kind: MessageKind;
  agent?: string;
  /** The subagent INVOCATION id (SDK `parent_tool_use_id`) and the task it was
   *  given (SDK `task_description`), PRD #99. Both are pure per-frame fields —
   *  no correlation state — and absent when the frame carries neither field.
   *  "Absent" tracks the SDK fields, not the role name: an agent NAMED lead in a
   *  repo roster is a real subagent and does carry an instance id. */
  agentInstance?: string;
  agentLabel?: string;
  payload: Record<string, unknown>;
}

/**
 * Everything an executor needs to work one run.
 *
 * The fields below `emit` are consumed only by the SDK executor (M3); the M2
 * stub ignores them, so they are optional and the interface stays backward
 * compatible. The runner populates them from the claim.
 */
export interface RunContext {
  runId: string;
  /** Run kind (PRD #6). "issue" (default when absent) works issueIid's card;
   *  "ci_fix" diagnoses + fixes `pipeline`. */
  kind?: RunKind;
  /** The worked issue for an issue run; null for a ci_fix run (no issue). */
  issueIid: number | null;
  issueTitle: string;
  issueDescription: string;
  /** The failed-pipeline snapshot for a ci_fix run (PRD #6): what the agent
   *  diagnoses + fixes. Present only for kind="ci_fix". Log tails are UNTRUSTED. */
  pipeline?: ClaimPipeline | null;
  /** Checked-out worktree the executor edits and commits in (local only). */
  worktreePath: string;
  branch: string;
  /** The commit `branch` was cut from in the runner clone (git.ts `RunnerClone.baseCommit`).
   *  The executor states it in the lead's prompts so the lead does not have to infer the
   *  branch's parent from the clone's default branch — which is a FROZEN MIRROR taken when
   *  the worker first cloned the repo, and so may sit at, behind, or ahead of this commit.
   *  Optional: the M2 stub executor ignores it, like every field below `emit`. */
  baseCommit?: string;
  /** The default branch's tip (git.ts `RunnerClone.defaultBranchCommit`). Carried
   *  alongside `baseCommit` because on a RESUME the two differ and name different diffs;
   *  the prompt is wrong on exactly the prior-work runs if it only ever sees one. */
  defaultBranchCommit?: string;
  /** Append a message to the run's live stream. */
  emit(msg: EmittedMessage): void;
  /** Anthropic subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN) for the SDK. */
  oauthToken?: string;
  /** PRD #3 templates (lead + subagents) mapped to SDK AgentDefinitions. */
  agents?: AgentTemplate[];
  /** PRD #37: the roster the worker parsed out of the clone's `.claude/agents/`,
   *  with prompt bodies. Detected before the first turn but INERT until a selection
   *  activates it at the gate (M3); the lead always comes from `agents` above, so a
   *  repo file named `lead` is only ever a subagent candidate. Empty when the repo
   *  ships none. */
  repoAgents?: AgentTemplate[];
  /** M4 (PRD #16): the per-run skill union — materialized into a local plugin dir
   *  and enabled via the SDK `skills` list. */
  skills?: ClaimSkill[];
  /** M4 (PRD #16): skills the server dropped at claim assembly (shadowed /
   *  over-limit). The worker owns the gapless seq, so it logs these. */
  skillsDropped?: ClaimSkillDrop[];
  /** M6 (PRD #16): the repo owner opted in to loading skills from the clone's own
   *  .claude/skills. Only then does the worker enumerate them (skills only, lowest
   *  precedence); default off. */
  repoSkillsEnabled?: boolean;
  /** PRD #90: the run's (user, repo) cross-run memory, fetched at claim time. The
   *  executor composes it into the lead's plan prompt as inert, nonce-fenced,
   *  UNTRUSTED-advisory context. Absent/empty ⇒ no memory block is injected. */
  memory?: MemoryEntry[];
  /** Per-run caps (timeouts in SECONDS, iterations); converted at use sites. */
  config?: ClaimConfig | null;
  /** SDK session to resume; null/absent for a fresh run. The runner clears this when
   *  the named transcript is not on THIS worker's disk (issue #105) — the SDK resolves
   *  a resume locally, so passing an unresolvable id fails the whole run before the
   *  first turn instead of resuming anything. */
  sessionId?: string | null;
  /** Issue #105: set ONLY when a resume was dropped for that reason AND the branch
   *  already carries pushed work. The executor forwards it to the planning prompt so
   *  an amnesiac lead is told to read the existing commits rather than redo them —
   *  the honest degradation must not become silently duplicated work. */
  priorWork?: PriorWork;
  /** PRD #35 Decision 6b: this run's plan is ALREADY APPROVED and its SDK session is
   *  resumable here, so the executor skips the Phase-1 planning turn and the gate and
   *  goes straight to implement⇄review with `approvedPlan` below.
   *
   *  Set by the RUNNER, which is the only layer that knows all three facts: the
   *  server said plan_approved, a session id survived, and issue #105's transcript
   *  check did not drop it. Without this a park-and-resume re-plans, re-parks the run
   *  at awaiting_approval in front of a human who already approved, and can fail
   *  outright with REASON_NO_PLAN when the resumed session declines to re-emit
   *  signal_plan — the run's own approval turned into a dead end.
   *
   *  A park BEFORE approval (only reachable if the planning turn itself died on a
   *  limit) leaves this false and resumes into planning exactly as today. */
  planApproved?: boolean;
  /** The already-approved plan text to seed the implement loop with (PRD #35). Only
   *  meaningful with `planApproved`; the executor refuses to skip the gate without
   *  it, because an empty plan would enter implement with no instructions. */
  approvedPlan?: string;
  /** The run's persisted subagent selection, replayed on the claim (PRD #35).
   *
   *  Used on the pre-approved resume path, where there is no approve_plan verdict to
   *  carry it. Absent means the run never reached a gate (or the server predates the
   *  field), and the executor then applies resolveAgentSelection's absent-default —
   *  which is the CORRECT answer for a gate-less run, not a fallback to tolerate. */
  approvedSelection?: AgentSelection;
  /** Called once with the SDK session id when first observed (for /state). */
  onSessionId?(sessionId: string): void;
  /** Aborts the SDK subprocess when signalled (cancel/shutdown; wired in M4). */
  signal?: AbortSignal;
  /**
   * M4 plan gate. Called by the executor after the lead submits a plan: the
   * runner posts /state awaiting_approval with the plan and returns the user's
   * verdict (approve/reject/cancel), polled from /inputs. Absent in M2/M3.
   */
  gatePlan?(planMd: string): Promise<PlanVerdict>;
  /** M4: dequeue the next queued follow-up to inject into the next loop turn. */
  pullFollowUp?(): string | undefined;
  /** M4: report a running/iteration heartbeat (server persists via GREATEST). */
  reportIteration?(iteration: number): void;
}

export interface ExecutorResult {
  /** The branch to report as completed. The runner pushes it + opens the MR. */
  branch: string;
  /** ci_fix only (PRD #6): "not_code" when the agent judged the failure NOT a code
   *  problem (infra/flaky/secret/runner). The runner then completes the run with the
   *  diagnosis and NO push/MR — approving a no-op costs nothing and the diagnosis is
   *  the value. Absent ⇒ a real fix was committed and must be pushed. */
  fixVerdict?: FixVerdict;
  /** PRD #37: the resolved subagent roster the IMPLEMENT phase actually ran with —
   *  the source and the surviving agent names. The runner uses it for the MR
   *  description marker (a `repo`-source run states the internal review was repo-
   *  authored, Decision 3b). Absent for a stub/ci_fix-not_code run that assembled
   *  no roster. */
  agentSelection?: { source: AgentSource; agents: string[] };
  /** PRD #46 M9: the allowlisted provisioned tool env (PATH + nix TLS/locale vars)
   *  the run's tier-1 packages installed, so the self_improve check runner can put
   *  go/vitest/tsc on its subprocess PATH. Absent (or `{}`) when nothing was
   *  provisioned; the check env then falls back to the worker's base PATH. */
  toolEnv?: Record<string, string>;
  /** PRD #72 M4: the repo-relative path the lead declared its PRD moved to (e.g.
   *  `prds/done/72-x.md`), from `signal_done`. ISSUE RUNS ONLY (Decision 13) and
   *  forwarded VERBATIM — the worker clamps length and nothing else. The api
   *  validates the shape (`api/internal/prdpath`) and drops the value if it does
   *  not hold, without ever failing the terminal report. Absent when the run moved
   *  no PRD, which is the common case. StubExecutor never sets it. */
  prdDonePath?: string;
}

/**
 * Thrown by the executor when the human rejects the plan at the gate. The runner
 * catches it and reports `failed` with the (user-supplied) reason verbatim,
 * rather than treating it as an internal crash. The reason is human/user text,
 * never a secret.
 */
export class PlanRejectedError extends Error {
  constructor(readonly reason: string) {
    super(reason);
    this.name = "PlanRejectedError";
  }
}

/**
 * The unit M3 replaces with the Claude Agent SDK. Keeping it behind this
 * interface is what lets M2 exercise the full state machine (claim → worktree →
 * work → report) with no AI in the loop.
 */
export interface Executor {
  run(ctx: RunContext): Promise<ExecutorResult>;
  /**
   * Group-kill every subprocess the agent spawned for the last run. The runner
   * calls this BEFORE any PAT-bearing git op (push) so an agent-backgrounded
   * process cannot survive to read the PAT out of the git child's /proc/environ
   * (M4 audit B1). Optional so the M2 stub and test executors need not implement
   * it; the SDK executor also self-reaps in its own run() finally.
   */
  killAgentTree?(): void;
}

/**
 * Sentinel in an issue's title/description that makes the stub executor throw
 * after the plan gate, standing in for an agent that fails mid-implementation.
 * The E2E harness uses it to drive the autopilot failure path (a failed run →
 * exactly one terminal failure comment) with no live SDK. Off unless present.
 */
export const STUB_FAIL_SENTINEL = "UZI_STUB_FAIL";

/**
 * Sentinel in a ci_fix run's title/description that makes the stub executor return
 * a not_code verdict after the plan gate (PRD #6), standing in for an agent that
 * judged the failure not a code problem. Drives the E2E not_code path with no live
 * SDK. Off unless present.
 */
export const STUB_NOT_CODE_SENTINEL = "UZI_STUB_NOT_CODE";

/**
 * Sentinel that makes the stub die on a simulated Anthropic usage limit (PRD #35),
 * standing in for a turn whose result frame carried `blocking_limit`. The E2E needs
 * it because the park path is otherwise unreachable without a real exhausted
 * subscription — the same reason STUB_INTERLEAVE_SENTINEL exists for multi-agent
 * streams.
 *
 * It fires ONLY on the first attempt, keyed on `ctx.planApproved`. That is what makes
 * the headline scenario expressible end to end: attempt 1 plans, gates, then parks;
 * the resume arrives pre-approved, does NOT throw, and completes. A sentinel that
 * fired every time could only ever prove the park, never the recovery.
 *
 * Append `:noreset` to report NO reset time, which drives the server's exponential
 * fallback schedule instead of the reported-reset path. On a poller-disabled
 * deployment — which the E2E overlay is, `UZI_USAGE_POLL_INTERVAL: "0"` — that
 * fallback is the ORDINARY path rather than an edge case, because every gauge row
 * classifies stale and neither the cross-check nor NextAvailable can contribute.
 */
export const STUB_LIMIT_SENTINEL = "UZI_STUB_LIMIT";

/**
 * How far ahead the stub claims the window reopens. Deliberately small: the server
 * floors retry_not_before at `now + jitter` (60-180s), so anything under that is
 * equivalent, and the point is to exercise the REPORTED-RESET branch of the real
 * computation rather than to control the wait — which the server owns.
 */
const STUB_LIMIT_RESET_MS = 5_000;

/**
 * Sentinel that makes the stub emit a scripted INTERLEAVED multi-agent message
 * stream during the implement phase (PRD #43 M5). A real parallel-subagent run
 * produces messages from several agents woven together; the SDK is the only thing
 * that emits them, so the isolated E2E stack (stub, no live SDK) needs the stub to
 * stand in. The scripted stream lets the E2E prove the persistence/replay contract
 * — gapless per-run seq, strict order, and per-agent attribution surviving REST
 * `?after=<seq>` replay — with no Anthropic session. Off unless present.
 */
export const STUB_INTERLEAVE_SENTINEL = "UZI_STUB_INTERLEAVE";

/**
 * The scripted stream STUB_INTERLEAVE_SENTINEL emits, in emit order. Each frame
 * carries a 1-based `step` in its payload so a consumer can pin exact order and
 * attribution independently of the run's other (worker) messages. The agents
 * alternate lead → coder → reviewer, and each name recurs NON-ADJACENTLY (a second
 * `coder`, then `reviewer`, then `lead`): that is exactly the interleaving a
 * parallel run yields, and it is what makes name-based attribution non-trivial to
 * preserve across persistence and reconnect replay.
 */
/**
 * Run-health E2E sentinels (PRD #47 M6). In an issue's title/description they make
 * the stub reproduce, with no live SDK, the three telemetry shapes the server-side
 * detector keys off:
 *   UZI_STUB_STALL    — go quiet past the stall threshold, then resume.
 *   UZI_STUB_LOOP     — emit the SAME tool call four times (window-hash → looping).
 *   UZI_STUB_INFLIGHT — hold ONE tool call open (no result) past the stall
 *                       threshold, proving `stalled` is SUPPRESSED (working, not stuck).
 * Off unless present; a normal run skips the simulation entirely.
 */
export const STUB_STALL_SENTINEL = "UZI_STUB_STALL";
export const STUB_LOOP_SENTINEL = "UZI_STUB_LOOP";
export const STUB_INFLIGHT_SENTINEL = "UZI_STUB_INFLIGHT";

/**
 * Pause lengths for the health sentinels, in ms. STALL/INFLIGHT sit comfortably
 * above the 60s minimum health_stall_seconds the E2E sets, so the detector flags
 * (or, for in-flight, provably does NOT flag) the run before the stub moves on.
 * RESUME is one-plus sweep ticks so the self-clear is observable while still
 * running; LOOP_HOLD is shorter — looping is window-based, not time-based, so one
 * sweep tick after the fourth identical call suffices.
 */
// The stall/in-flight pause is 95s: with a 60s stall threshold and the sweeper's
// 15s tick, the detector flags by threshold+tick (≤75s), so a 95s pause leaves a
// ≥20s window in which `stalled` is observable before the stub resumes — comfortably
// above the E2E's poll interval, so the assertion is not tick-timing-flaky.
export const STUB_HEALTH_STALL_MS = 95_000;
export const STUB_HEALTH_RESUME_MS = 20_000;
export const STUB_HEALTH_LOOP_HOLD_MS = 35_000;

// PRD #99 M2: the same six frames now also carry the per-instance attribution a
// real parallel-subagent run would — `instance` = the SDK's `parent_tool_use_id`
// (the INVOCATION id), `label` = its `task_description`. The two `coder` frames
// are two DISTINCT invocations (`stub-inst-a` / `stub-inst-b`) with distinct
// labels, and they are NON-ADJACENT (a `reviewer` frame sits between them) —
// which is the whole point: under the old consecutive-author grouping they
// render as separate bars that a naive "group by agent name" refactor would
// merge into one garbled `coder` block. Two `reviewer` invocations double a
// second role, so the role-rollup ("coder ×2 · reviewer ×2") is exercisable too.
// The `lead` frames deliberately carry NEITHER field (it is the parentless
// actor), which is the NULL side of the fixture.
//
// FRAME COUNT AND AGENT ORDER ARE FROZEN. `e2e/run-e2e.sh` (PRD #43 M5) pins
// this stream as a literal `[agent, step]` vector (`EXPECT_IL`), and
// `test/executor.test.ts` asserts the emitted stream matches it exactly. The two
// new columns were therefore added to the EXISTING frames rather than by
// appending a new segment — appending would have broken an e2e assertion this
// PRD is not allowed to touch (PRD #97 is rewriting that harness).
export const STUB_INTERLEAVE_STREAM: ReadonlyArray<{
  agent: string;
  text: string;
  instance?: string;
  label?: string;
}> = [
  { agent: "lead", text: "parallel dispatch: units A (api) and B (web)" },
  { agent: "coder", text: "unit A: editing the api scope", instance: "stub-inst-a", label: "API wiring" },
  { agent: "reviewer", text: "review wave: auditing unit A", instance: "stub-inst-rev-a", label: "audit unit A" },
  { agent: "coder", text: "unit B: editing the web scope", instance: "stub-inst-b", label: "web gate UX" },
  { agent: "reviewer", text: "review wave: auditing unit B", instance: "stub-inst-rev-b", label: "audit unit B" },
  { agent: "lead", text: "integration: scopes disjoint, committing once" },
];

// PRD #40 M6: the stub stands in for the live SDK's usage-bearing frames. It emits
// a per-agent (coder) assistant message carrying PER-CALL usage (Decision 11) and a
// terminal `result` frame carrying CUMULATIVE usage + per-model breakdown (folded
// into run_usage by the API). The live SDK path emits both; without them the E2E
// could not prove usage lands on the run, aggregates into /api/usage, or surfaces a
// per-agent row. Numbers are arbitrary but FIXED so the E2E asserts them exactly.
export const STUB_RESULT_USAGE = {
  input_tokens: 21400,
  cache_read_input_tokens: 188000,
  cache_creation_input_tokens: 0,
  output_tokens: 6100,
};
export const STUB_RESULT_MODEL_USAGE = {
  "claude-fable-5": { inputTokens: 21400, outputTokens: 6100, cacheReadInputTokens: 188000, cacheCreationInputTokens: 0, costUSD: 0.24 },
};
export const STUB_RESULT_COST_USD = 0.24;
export const STUB_CODER_USAGE = {
  input_tokens: 12000,
  cache_read_input_tokens: 90000,
  cache_creation_input_tokens: 0,
  output_tokens: 3000,
};

/** Options for the stub executor. */
export interface StubExecutorOptions {
  /**
   * Drive the M4 plan-approval gate before committing (emit a plan → the runner
   * posts awaiting_approval → await the human verdict). Off by default so the
   * bare M2 stub goes straight to a local commit; the E2E harness turns it on
   * (UZI_STUB_PLAN_GATE) to exercise the full workflow with no live SDK.
   */
  planGate?: boolean;
  /**
   * When set, the stub exercises the SAME tool-provisioning path as the SDK
   * executor (PRD #18 M8): pinned HOME for the install subprocess. Omitted ⇒ no
   * provisioning (unit tests). The E2E sets it and stubs the `devbox` binary, so a
   * provisioned-tool run is observable with no live SDK and no substituter egress.
   */
  homeDir?: string;
  /** Injected in tests so no real devbox/nix egress happens; default = provisionTools. */
  provision?: typeof provisionTools;
  /**
   * E2E-only (PRD #47 M6): override the health-sentinel pause lengths (ms). Unit
   * tests set 0 so the stall / loop / in-flight simulation runs instantly; the real
   * E2E leaves it undefined to use the STUB_HEALTH_* defaults (which sit above the
   * server's stall threshold).
   */
  healthPauseMs?: number;
}

/**
 * M2 stand-in for the real agent: writes a marker file into the worktree and
 * makes a single local commit. No SDK, no network, no push (push + MR is M4).
 * The commit lands on `agent/issue-{iid}` in the shared bare object store, so a
 * test can assert the branch advanced exactly as a real run would.
 *
 * With `planGate` on it also drives the M4 plan gate: it submits a static plan,
 * calls ctx.gatePlan (parking the run at awaiting_approval), and honours the
 * verdict — approve continues to the commit, reject/cancel abort — so the whole
 * plan→approve→work→MR path is provable end-to-end without a live Anthropic
 * session (mirrors SdkExecutor's gate handling).
 */
export class StubExecutor implements Executor {
  constructor(
    private readonly log: Logger,
    private readonly opts: StubExecutorOptions = {},
  ) {}

  async run(ctx: RunContext): Promise<ExecutorResult> {
    ctx.emit({ kind: "status", agent: "worker", payload: { text: "stub executor starting" } });

    // Synthesize the skills plugin the SAME way the SDK executor does (shared
    // prepareSkillPlugin), then read it back and report exactly what landed on
    // disk. This makes skill delivery + the repo-skill opt-in observable in the M6
    // E2E, which runs the stub (no live SDK session). Dropped skills are logged
    // too. Non-fatal: a skills failure must never fail the stub's core commit path.
    try {
      const prepared = await prepareSkillPlugin(ctx, resolveSkillCaps(ctx.config));
      const loaded = await readPluginSkillNames(prepared.pluginPath);
      ctx.emit({ kind: "status", agent: "worker", payload: { text: `plugin skills: ${loaded.join(", ") || "(none)"}`, plugin_skills: loaded } });
      const drops = [...(ctx.skillsDropped ?? []), ...prepared.drops];
      for (const d of drops) {
        ctx.emit({ kind: "status", agent: "worker", payload: { text: `skill dropped: ${d.name} (${d.reason})`, dropped_skill: d.name, dropped_reason: d.reason } });
      }
    } catch (err) {
      this.log.warn("stub: skills plugin synthesis failed", { run_id: ctx.runId, error: String(err) });
    }

    // Delivered agent templates (PRD #18 M7/M8): report the claim's template set so
    // a user-scoped template's delivery — after the server's allocation +
    // shared-precedence resolution — is observable in the E2E (the stub runs no lead).
    const agentNames = (ctx.agents ?? []).map((a) => a.name);
    ctx.emit({ kind: "status", agent: "worker", payload: { text: `agents: ${agentNames.join(", ") || "(none)"}`, agents: agentNames } });

    // Tool provisioning (PRD #18 M8): exercise the SAME path as the SDK executor
    // (provision-run.ts), against the E2E's stubbed devbox. No packages ⇒ a no-op;
    // a provision failure fails the run, matching the SDK executor. The provisioned
    // env is unused by the stub — the point is to prove the install path end to end.
    if (this.opts.homeDir) {
      const provisionRoot = path.join(path.dirname(this.opts.homeDir), "provision");
      const { provisionDir } = await provisionRunTools(ctx, {
        provisionRoot,
        homeDir: this.opts.homeDir,
        log: this.log,
        provision: this.opts.provision,
      });
      if (provisionDir) await fs.rm(provisionDir, { recursive: true, force: true }).catch(() => undefined);
    }

    const isCIFix = ctx.kind === "ci_fix";
    const label = isCIFix ? `CI fix on \`${ctx.branch}\`` : `issue #${ctx.issueIid}`;
    // A ci_fix run can conclude that the failure is not a code problem (PRD #6):
    // the E2E drives that path via the sentinel.
    const notCode =
      isCIFix &&
      (ctx.issueDescription.includes(STUB_NOT_CODE_SENTINEL) ||
        ctx.issueTitle.includes(STUB_NOT_CODE_SENTINEL) ||
        (ctx.pipeline?.failed_jobs ?? []).some((j) => j.log_tail.includes(STUB_NOT_CODE_SENTINEL)));

    // PRD #35 Decision 6b, stub half: a resume PAST an approval that already happened
    // must not ask for it again. Both conditions are required for the same reasons
    // sdk-executor.ts gives at its own `preApproved`: without the plan text there are
    // no instructions to implement, and `planApproved` alone does not imply one.
    //
    // `ctx.sessionId` is deliberately NOT re-checked here, though SdkExecutor does
    // check it. The runner already ANDs the claim's plan_approved with a surviving
    // session id before setting ctx.planApproved (runner.ts), so a run whose
    // transcript was dropped never reaches this branch; and the stub has no SDK
    // conversation to resume, so a session id would be a proxy for nothing it uses.
    //
    // Without this the stub re-enters ctx.gatePlan on the resumed run and parks it at
    // awaiting_approval a SECOND time, waiting on a human verdict that the unattended
    // recovery path has no one left to give — measured 2026-07-28 on the M6 e2e, where
    // the sweeper-promoted run went limit_wait -> awaiting_approval with
    // limit_wait_count=1 and sat there until the harness timed out. The class doc above
    // already claims the gate handling "mirrors SdkExecutor's"; this is what makes that
    // true, and the M6 e2e asserts the resulting feed line appears exactly once.
    const preApproved = ctx.planApproved === true && !!ctx.approvedPlan?.trim();

    if (this.opts.planGate && ctx.gatePlan) {
      if (preApproved) {
        ctx.emit({
          kind: "status",
          agent: "worker",
          payload: { text: "resuming an already-approved plan — skipping the planning turn and the approval gate" },
        });
      } else {
        const planMd = [
          isCIFix ? `## Stub CI-fix diagnosis for \`${ctx.branch}\`` : `## Stub plan for issue #${ctx.issueIid}`,
          ``,
          ...(notCode
            ? [`VERDICT: not_code`, ``, `The failure is an infra/flaky problem, not a code bug (stub diagnosis).`]
            : [`- write a marker file documenting the run`, `- commit it on \`${ctx.branch}\``]),
          ``,
          `Generated by the stub executor to drive the plan-approval gate with`,
          `no live Anthropic session.`,
        ].join("\n");
        // Plan gate (+ revision loop, PRD #41). The stub synthesizes plan text instead
        // of driving a live SDK, so a `revise` verdict appends a version marker to the
        // canned plan and re-gates it — mirroring sdk-executor.ts's revise loop so the
        // E2E can assert on the plan_feedback + plan_revising feed contract. The server
        // enforces PLAN_MAX_REVISIONS at submit time, so the stub needs no local cap; it
        // stays fail-closed via the post-loop guards (only `approve` reaches implement).
        let revisedPlan = planMd;
        let verdict = await ctx.gatePlan(revisedPlan);
        let round = 0;
        while (verdict.kind === "revise") {
          // Record the reviewer's feedback on the feed BEFORE the next gatePlan flushes
          // the revised plan, so the feed never lags the awaiting_approval re-report —
          // same ordering as the real executor.
          ctx.emit({ kind: "plan_feedback", agent: "worker", payload: { feedback: verdict.feedback } });
          round++;
          ctx.emit({ kind: "plan_revising", agent: "worker", payload: { round } });
          revisedPlan = `${revisedPlan}\n\n(revision ${round}: applied feedback)`;
          verdict = await ctx.gatePlan(revisedPlan);
        }
        // Fail closed: only an approve may proceed to implement. revise exits the loop
        // above; reject/cancel throw here. `verdict` is now narrowed to `approve` — if a
        // future PlanVerdict variant is added, these lines stop type-checking, forcing it
        // to be handled rather than silently falling through to an unapproved plan.
        if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
        if (verdict.kind === "cancel") throw new Error("run cancelled");
      }
      if (notCode) {
        ctx.emit({ kind: "status", agent: "worker", payload: { text: "stub diagnosis: not a code problem" } });
        return { branch: ctx.branch, fixVerdict: "not_code" };
      }
      ctx.reportIteration?.(1);
      ctx.emit({ kind: "status", agent: "worker", payload: { text: "plan approved; implementing" } });
    }

    // E2E failure hook: throw AFTER the (auto-)approved plan so the run fails
    // during "implementation", exercising the worker-terminated failure path.
    if (ctx.issueDescription.includes(STUB_FAIL_SENTINEL) || ctx.issueTitle.includes(STUB_FAIL_SENTINEL)) {
      throw new Error(`stub executor: forced failure (${STUB_FAIL_SENTINEL} sentinel present)`);
    }

    // PRD #35 E2E hook: die on a simulated usage limit, but only on the FIRST
    // attempt. `planApproved` is the discriminator because it is exactly what the
    // server sets once a human approved and the run is resuming — so the resume runs
    // straight through and completes, which is the whole point of the scenario.
    const limitText = `${ctx.issueDescription}\n${ctx.issueTitle}`;
    if (limitText.includes(STUB_LIMIT_SENTINEL) && !ctx.planApproved) {
      const noReset = limitText.includes(`${STUB_LIMIT_SENTINEL}:noreset`);
      // Report a synthetic SDK session id, standing in for the live session exactly
      // as the stub already stands in for usage frames and interleaved streams.
      // Without it runs.session_id stays NULL, the resumed run has no session,
      // plan_approved is unusable, and every resume goes back through the plan gate —
      // so the gate-skip path would be untestable under the stub.
      //
      // Reported HERE rather than at the top of run(), deliberately: onSessionId
      // triggers an extra `running` state report, and doing it unconditionally
      // changed the observable report sequence of EVERY stub run (three existing
      // tests pinned the old sequence and went red). Scoped to the sentinel, an
      // ordinary stub run is byte-identical to before.
      //
      // Derived from the run id so it is stable across a resume of the SAME run.
      ctx.onSessionId?.(`stub-session-${ctx.runId}`);
      throw new LimitReachedError({
        rateLimitType: "five_hour",
        // Omitted for :noreset, which is what sends the server down its exponential
        // fallback rather than the reported-reset branch.
        resetsAtMs: noReset ? undefined : Date.now() + STUB_LIMIT_RESET_MS,
        detail: "stub sentinel",
      });
    }

    // PRD #43 M5: emit a scripted interleaved multi-agent stream so the E2E can
    // assert that persistence + REST replay keep it gapless, ordered, and
    // per-agent attributed — the piece the live SDK would otherwise produce. Emits
    // are synchronous and ordered, so the downstream batcher assigns them a gapless
    // seq run; the trailing "committed" message keeps them mid-stream, not last.
    if (ctx.issueDescription.includes(STUB_INTERLEAVE_SENTINEL) || ctx.issueTitle.includes(STUB_INTERLEAVE_SENTINEL)) {
      STUB_INTERLEAVE_STREAM.forEach((frame, i) => {
        // Spread the optional PRD #99 fields only when the frame has them, so a
        // lead frame emits no agentInstance/agentLabel key at all (matching the
        // live mapper, where the lead's null parent_tool_use_id probes to
        // undefined) and the API stores SQL NULL rather than "".
        ctx.emit({
          kind: "text",
          agent: frame.agent,
          ...(frame.instance !== undefined ? { agentInstance: frame.instance } : {}),
          ...(frame.label !== undefined ? { agentLabel: frame.label } : {}),
          payload: { text: frame.text, step: i + 1 },
        });
      });
    }

    // PRD #47 M6: reproduce a stall / loop / long in-flight tool call so the
    // server-side run-health detector is provable end to end with no live SDK.
    // Gated on the UZI_STUB_* sentinels; a normal run is unaffected.
    await this.simulateHealth(ctx);

    const markerPath = path.join(ctx.worktreePath, "UZI_RUN.md");
    const marker = [
      `# uzi stub run`,
      ``,
      `- run_id: ${ctx.runId}`,
      `- ${label} — ${ctx.issueTitle}`,
      `- branch: ${ctx.branch}`,
      ``,
      `This file is written by the M2 stub executor to prove the claim →`,
      `worktree → work → report loop end to end. M3 replaces it with the`,
      `Claude Agent SDK.`,
      ``,
    ].join("\n");
    await fs.writeFile(markerPath, marker, "utf8");

    // Local commit only. Identity is pinned per-invocation via -c so nothing is
    // written to the worktree's git config, and gpg signing is forced off so a
    // signing config on the host image can't block the commit.
    await this.git(ctx.worktreePath, ["add", "UZI_RUN.md"]);
    await this.git(ctx.worktreePath, [
      "-c", "user.name=uzi-agent",
      "-c", "user.email=uzi-agent@uzi.local",
      "-c", "commit.gpgsign=false",
      "commit", "-m", `uzi stub: work on ${label}`,
    ]);

    ctx.emit({ kind: "text", agent: "worker", payload: { text: "stub work committed locally" } });

    // PRD #40 M6: stand in for the live SDK's usage frames — a per-agent (coder)
    // message with per-call usage (Decision 11) and the terminal result frame with
    // cumulative usage + modelUsage (the API folds it into run_usage). Emitted on
    // every stub run, so a normal E2E run carries usage without a sentinel.
    ctx.emit({ kind: "text", agent: "coder", payload: { text: "coder: implemented the change", usage: STUB_CODER_USAGE } });
    ctx.emit({
      kind: "status",
      agent: "lead",
      payload: {
        event: "result",
        subtype: "success",
        num_turns: 9,
        duration_ms: 100000,
        total_cost_usd: STUB_RESULT_COST_USD,
        usage: STUB_RESULT_USAGE,
        modelUsage: STUB_RESULT_MODEL_USAGE,
      },
    });

    this.log.info("stub executor committed marker", { run_id: ctx.runId, branch: ctx.branch });
    return { branch: ctx.branch };
  }

  /**
   * PRD #47 M6 run-health simulation. Reproduces, with no live SDK, the three
   * telemetry shapes the server-side detector keys off, gated on the UZI_STUB_*
   * sentinels in the issue text. A normal run matches none and returns immediately.
   * All pauses are well under the worker's own idle/run timeouts.
   */
  private async simulateHealth(ctx: RunContext): Promise<void> {
    const has = (s: string) => ctx.issueDescription.includes(s) || ctx.issueTitle.includes(s);
    const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
    const override = this.opts.healthPauseMs;
    const stallMs = override ?? STUB_HEALTH_STALL_MS;
    const resumeMs = override ?? STUB_HEALTH_RESUME_MS;
    const loopHoldMs = override ?? STUB_HEALTH_LOOP_HOLD_MS;

    if (has(STUB_STALL_SENTINEL)) {
      // Go quiet past the stall threshold (detector flags `stalled`), then resume:
      // the activity bump self-clears the flag, and the short trailing pause keeps
      // the run running so the clear is observable before it completes.
      ctx.emit({ kind: "status", agent: "worker", payload: { text: "stub: pausing to simulate a stall" } });
      await sleep(stallMs);
      ctx.emit({ kind: "status", agent: "worker", payload: { text: "stub: resuming after the pause" } });
      await sleep(resumeMs);
      return;
    }

    if (has(STUB_LOOP_SENTINEL)) {
      // Emit the SAME tool call four times, each with its result (so none is "in
      // flight"): the detector's window hash counts four identical calls → looping.
      // Hold briefly so a sweep tick catches it before the run completes.
      for (let i = 0; i < 4; i++) {
        const id = `stub-loop-${i}`;
        ctx.emit({ kind: "tool_use", agent: "worker", payload: { id, name: "Bash", input: { command: "go test ./..." } } });
        ctx.emit({ kind: "tool_result", agent: "worker", payload: { tool_use_id: id, content: "same failing output", is_error: true } });
      }
      await sleep(loopHoldMs);
      return;
    }

    if (has(STUB_INFLIGHT_SENTINEL)) {
      // Emit ONE tool call and hold it OPEN (no result) past the stall threshold:
      // the newest message is an unmatched tool_use, so `stalled` is SUPPRESSED —
      // the run is working, not stuck. Close it out afterwards so the run completes.
      const id = "stub-inflight-0";
      ctx.emit({ kind: "tool_use", agent: "worker", payload: { id, name: "Bash", input: { command: "make provision" } } });
      await sleep(stallMs);
      ctx.emit({ kind: "tool_result", agent: "worker", payload: { tool_use_id: id, content: "provision finished", is_error: false } });
      return;
    }
  }

  private async git(cwd: string, args: string[]): Promise<void> {
    // Run AS the RUNNER uid (PRD #51 M5), mirroring GitCache.runGitAsRunner. The stub
    // commits into `ctx.worktreePath`, which under (b) is the RUNNER-owned clone (the
    // agent's checkout+commit tree — runner.ts wires runnerClone.path here). The real
    // SDK agent already commits there as `runner` (via runnerSpawn), so its git euid
    // matches the runner-owned store; the stub must too. A worker-uid git here hits
    // `fatal: detected dubious ownership` on the runner tree — and papering over it with
    // `safe.directory=*` would write worker-owned objects into a runner store (ownership
    // drift, diverging from the real path), so we run-AS-runner via runnerCommand instead.
    //
    // Env is gitEnv()'s REPLACEMENT env (M10 discipline: no process.env spread, so the
    // join token / API URL are absent by construction), with the RUNNER PATH + the
    // runner's private 0700 TMPDIR overriding the worker's (else the setpriv child would
    // inherit the worker's owner-only /tmp/uzi-worker and git scratch would EPERM).
    // Commit identity rides the caller's `-c` flags, which survive the setpriv `--`
    // passthrough. Single-uid (#58 / no split): runnerCommand is a passthrough and
    // runnerPath/runnerTmpdir fall back to the ambient PATH/TMPDIR — a plain git.
    const env: NodeJS.ProcessEnv = { ...gitEnv(), PATH: runnerPath() };
    const tmp = runnerTmpdir();
    if (tmp) env.TMPDIR = tmp;
    const wrapped = runnerCommand("git", ["-C", cwd, ...args]);
    await execFileAsync(wrapped.command, wrapped.args, { env, timeout: 60_000 });
  }
}

/** Read back the skill directory names actually materialized under a plugin dir
 *  (`<pluginPath>/skills/<name>/`), sorted. This reflects what is ON DISK, so the
 *  E2E asserting on it proves the plugin was truly synthesized (not just that the
 *  claim carried the names). Missing dir ⇒ no skills. */
async function readPluginSkillNames(pluginPath: string): Promise<string[]> {
  try {
    const entries = await fs.readdir(path.join(pluginPath, "skills"), { withFileTypes: true });
    return entries.filter((e) => e.isDirectory()).map((e) => e.name).sort();
  } catch {
    return [];
  }
}
