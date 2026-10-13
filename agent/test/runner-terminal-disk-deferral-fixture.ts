import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { DataVolumeGuard } from "../src/disk-full.js";
import { DiskGovernor } from "../src/cache-cap.js";
import { type CredentialFreeSettleOutcome, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { BoundaryPermit, CodexExecutionSafety } from "../src/harness.js";
import { type ExecutorFactory, type RunnerOptions } from "../src/runner.js";
import type { QuiesceRunOutcome } from "../src/run-quiescence.js";
import type { StatfsSample } from "../src/stats.js";
import { nullLogger } from "./helpers.js";
import { trustedExecutionCases, trustedEnvelopeCases } from "./fixtures/trusted-execution-refusals.js";
import { api, client, fakeGitlab, fx, gitlabClaim, homeDir, runnerWith } from "./runner-harness.js";

const GIB = 1024 ** 3;
export const ROOMY: StatfsSample = { bsize: 4096, blocks: 25 * GIB / 4096, bfree: 10 * GIB / 4096,
  bavail: 10 * GIB / 4096, files: 1_000_000, ffree: 900_000 };
export const FULL: StatfsSample = { ...ROOMY, bavail: 1, bfree: 1 };
export const QUIESCENT: QuiesceRunOutcome = {
  process: { state: "quiescent", processes: [], killed: [], detail: "fixture positively quiescent" },
  docker: { state: "not_wired", removed: [], detail: "not wired" },
};
export function readGit(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

export function fixture(opts: {
  error?: unknown; sample?: StatfsSample; unknown?: boolean; mismatch?: "home" | "clone";
  missingStat?: "home" | "clone"; features?: readonly string[]; status?: string; generation?: number | null;
  kind?: "issue" | "task"; beforeReject?: () => Promise<void> | void;
  reclaim?: () => Promise<void>; runner?: Partial<RunnerOptions>;
  result?: ExecutorResult; safety?: CodexExecutionSafety; observe?: (ctx: RunContext) => void;
  afterWork?: (ctx: RunContext) => Promise<void>;
  settleForCredentialFreeCapture?: () => Promise<CredentialFreeSettleOutcome>;
} = {}) {
  const claim = gitlabClaim(2201, { claim_generation: 4, ...(opts.kind ? { kind: opts.kind } : {}) });
  client.protocolFeatures = opts.features ? [...opts.features] :  ["recovery_cause_data_volume_full", "claim_generation_fence"];
  api.onState(claim.run_id, (body) => api.setOwnershipStatus(claim.run_id, body.status, claim.claim_generation));
  let full = false;
  let calls = 0;
  let reclaims = 0;
  let clone = "";
  const home = path.join(homeDir, claim.run_id);
  const error = Object.hasOwn(opts, "error") ? opts.error : new Error("opaque executor failure");
  const factory: ExecutorFactory = () => ({
    homeDir: home,
    executor: { safety: opts.safety, settleForCredentialFreeCapture: opts.settleForCredentialFreeCapture, run: async (ctx) => {
      opts.observe?.(ctx);
      calls++;
      clone = ctx.worktreePath;
      fs.mkdirSync(home, { recursive: true });
      fs.writeFileSync(path.join(home, "session"), "transcript");
      fs.writeFileSync(path.join(clone, "COMMITTED.txt"), "committed sentinel\n");
      readGit(clone, "add", "COMMITTED.txt");
      readGit(clone, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture committed work");
      fs.writeFileSync(path.join(clone, "DIRTY.txt"), "dirty sentinel\n");
      full = true; // Clone and preflight completed with room. Only executor rejection sees full.
      api.setOwnershipStatus(claim.run_id, opts.status ?? "running", opts.generation === null ? undefined : opts.generation ?? 4);
      await opts.beforeReject?.();
      await opts.afterWork?.(ctx);
      if (opts.result) return opts.result;
      throw error;
    } },
  });
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => {
      if (!full) return ROOMY;
      if (opts.unknown) throw new Error("statfs unavailable");
      return opts.sample ?? FULL;
    },
    stat: async (p) => {
      const which = p === home ? "home" : p === clone ? "clone" : "data";
      if (which === opts.missingStat) throw new Error("stat unavailable");
      return { dev: which === opts.mismatch ? 8 : 7 };
    },
    reclaim: async () => { reclaims++; await opts.reclaim?.(); },
  });
  const forge = fakeGitlab();
  const runner = runnerWith(factory, forge.gitlab, undefined, nullLogger(), {
    dataVolume: guard, recoveryRetryMs: 1, quiesceRun: async () => QUIESCENT, ...opts.runner,
  });
  return { claim, runner, guard, home, clone: () => clone, calls: () => calls, reclaims: () => reclaims,
    forge, setRoomy: () => { full = false; }, setFull: () => { full = true; } };
}
export function parks() { return api.states.filter((s) => s.body.status === "recovery_wait"); }
export function settlementGovernor(error: unknown): DiskGovernor {
  const gov = new DiskGovernor({
    config: { capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 1,
      hardStopEnabled: false, hardMargin: 0.03 },
    log: nullLogger(), volumeTotalBytes: () => 25 * GIB, thresholdOf: () => undefined,
  });
  gov.leftLoop = () => { throw error; };
  return gov;
}
export async function waitForAbort(signal: AbortSignal): Promise<void> {
  const end = Date.now() + 2000;
  while (!signal.aborted && Date.now() < end) await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(signal.aborted, true, "control reached the running flight");
}
export function supervisedSafety(mode: "empty" | "absent" | "incomplete"): CodexExecutionSafety {
  return {
    kind: "codex",
    withBoundary: async (request, action) => {
      if (mode === "incomplete") throw new Error("supervisor settlement incomplete");
      if (mode === "absent") return undefined as never;
      return await action({ epoch: 1, boundary: request.boundary, signal: new AbortController().signal } as BoundaryPermit);
    },
    spawnBoundaryProcess: async () => { throw new Error("fixture has no subprocess admission"); },
    dispose: async () => ({ kind: "disposed" }),
  };
}

// These capture cases require the runner's Linux process-proof branch, even when
// the proof itself is injected. Portable rejection-only cases remain unguarded.
export const LINUX_CAPTURE = process.platform !== "linux" ? "requires Linux terminal-capture process proof" : false;

export const terminalRefusalCases = [...trustedExecutionCases, ...trustedEnvelopeCases];
export const splitRefusalRepresentations = ["direct Error", "context + repeated Error"] as const;

export async function assertTrustedRefusalCase(entry: { error: () => unknown; display: string }): Promise<void> {
      const f = fixture({ error: entry.error() });
      await f.runner.execute(f.claim);
      assert.equal(f.calls(), 1, "one executor call");
      assert.equal(f.forge.calls.length, 0, "no finalize/MR");
      assert.equal(f.reclaims(), 0, "trusted rejection must precede reclaim");
      assert.equal(parks().length, 0, "trusted rejection must never park");
      const failures = api.states.filter((s) => s.body.status === "failed");
      assert.equal(failures.length, 1, "one terminal failure");
      assert.equal(failures[0]!.body.failure_reason, entry.display, "preserve original display reason");
      if (entry.display === "the planning turn ended without a plan or a structured question after a corrective nudge") {
        assert.equal(failures[0]!.body.fail_origin, "plan_missing", "retain plan-missing disposition");
      }
      if (entry.display === "tool provisioning failed before the agent could start: invalid run id") {
        assert.equal(failures[0]!.body.fail_origin, "provisioning_failed", "retain provisioning disposition");
      }
      if (entry.display === "no Anthropic OAuth token was provided for this run") {
        assert.equal(failures[0]!.body.fail_origin, "credential_unavailable", "retain credential disposition");
      }
      assert.equal(api.states.some((s) => s.body.status === "completed"), false);
}
