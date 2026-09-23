// PRD #1349 M2 / #1531 / #1539 — the REAL Codex pre-settle-reap boundary fixture, shared between
// runner-recovery-generation.test.ts (the M1/M2 reap-before-report cases) and
// runner-terminal-journal.test.ts (the M3 permanent-failure end-to-end case). It builds a genuine
// Codex boundary — a real ExecutionRegistry + a tracked provider RegisteredRoot + the real
// buildRunLaneReconcile over a subscription binding — whose per-sink reconcile pushes "reconcile"
// onto a shared ordered event log and throws when `decideThrow()` says so (modelling the api's 409
// once a terminal state has been recorded). A real RecoveryCoordinator over fake archive/bundle
// clients records "reserve"/"release" into the SAME log, so a test can assert the split ordering
// `reconcile < failed < release/reserve`.

import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";

import { type RunContext, type ExecutorResult } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import type { CodexExecutionSafety } from "../src/harness.js";
import { RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer } from "../src/recovery.js";
import { type RecoveryBundleResult } from "../src/git.js";
import type {
  RecoveryCaptureStatusResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryHold,
  RecoveryHoldsResponse,
  RecoveryUploadManifest,
} from "../src/protocol.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { buildRunLaneReconcile } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch, type RegisteredRoot } from "../src/codex/registry.js";
import { selectCodexBinding, type CodexBinding } from "../src/codex/select.js";
import { nullLogger } from "./helpers.js";

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

export function commitInTree(treePath: string, file: string, content: string): string {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
  return execFileSync("git", ["-C", treePath, "rev-parse", "HEAD"], {
    env: GIT_ENV,
    encoding: "utf8",
  }).trim();
}

// ── fakes injected into the real coordinator ─────────────────────────────────────

export class FakeRecoveryClient implements RecoveryArchiveClient {
  reserveCalls: RecoveryReserveRequest[] = [];
  uploadCalls: Array<{ captureId: string; manifest: RecoveryUploadManifest }> = [];
  // PRD #1392 M1/M2 (fact 9): record the `releaseEvidence` class each release stamped, so the
  // completion path's publication/NULL mapping is observable ("publication" for a pushed branch,
  // undefined for a no-code completion).
  releaseCalls: Array<{ runId: string; generation?: number; evidence?: string }> = [];
  listCalls: string[] = [];
  holds: RecoveryHold[] = [];
  uploadShouldThrow = false;

  async reserveRecoveryCapture(_runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls.push(req);
    return { capture_id: "srv-capture-1", state: "preparing" };
  }
  async getRecoveryCaptureStatus(_runId: string, captureId: string): Promise<RecoveryCaptureStatusResponse> {
    return { capture_id: captureId, state: "preparing", manifest_bound: false };
  }
  async uploadRecoveryBundle(
    _runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
  ): Promise<RecoveryCaptureStatusResponse> {
    for await (const _ of bundle) {
      /* drain */
    }
    this.uploadCalls.push({ captureId, manifest });
    if (this.uploadShouldThrow) throw new Error("upload rejected");
    return { capture_id: captureId, state: "available", manifest_bound: true };
  }
  async releaseRecoveryCustody(
    runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse> {
    this.releaseCalls.push({ runId, generation, evidence: releaseEvidence });
    return { run_id: runId, released: true, holds_released: 1 };
  }
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> {
    this.listCalls.push(runId);
    return { run_id: runId, holds: this.holds };
  }
  /** The exact generations that were released, in call order. */
  releasedGenerations(): Array<number | undefined> {
    return this.releaseCalls.map((c) => c.generation);
  }
  /** The evidence class each release stamped, in call order (PRD #1392 M1/M2, fact 9). */
  releasedEvidence(): Array<string | undefined> {
    return this.releaseCalls.map((c) => c.evidence);
  }
}

export class FakeRecoveryGit implements RecoveryBundleProducer {
  fetchCalls = 0;
  produceCalls = 0;
  alreadyPublished = false;
  bytes = Buffer.from("m2-fake-bundle-bytes\x00ÿ");

  async fetchDefaultTip(): Promise<string> {
    this.fetchCalls++;
    return "3".repeat(40);
  }
  async produceRecoveryBundle(
    _barePath: string,
    opts: { sourceSha: string; outPath: string; forgeTip?: string; maxBytes?: number },
  ): Promise<RecoveryBundleResult> {
    this.produceCalls++;
    if (this.alreadyPublished) {
      return {
        bundlePath: "",
        byteSize: 0,
        checksum: "",
        chunkCount: 0,
        prerequisiteShas: [],
        sourceSha: opts.sourceSha,
        selfContained: false,
        alreadyPublished: true,
      };
    }
    await fsp.writeFile(opts.outPath, this.bytes);
    return {
      bundlePath: opts.outPath,
      byteSize: this.bytes.length,
      checksum: createHash("sha256").update(this.bytes).digest("hex"),
      chunkCount: 1,
      prerequisiteShas: [opts.forgeTip ?? "base"],
      sourceSha: opts.sourceSha,
      selfContained: false,
      alreadyPublished: false,
    };
  }
}

/** A real RecoveryCoordinator over the fake archive/bundle clients (recoveryRoot in a temp dir). */
export function makeRecoveryCoordinator(client: RecoveryArchiveClient, git: RecoveryBundleProducer): {
  coord: RecoveryCoordinator;
  root: string;
} {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-recov-"));
  const coord = new RecoveryCoordinator({
    client,
    git,
    log: nullLogger(),
    recoveryRoot: root,
    workerToken: "m2-worker-join-token-0123456789",
    now: () => 1_700_000_000_000,
  });
  return { coord, root };
}

// ── the real Codex boundary ──────────────────────────────────────────────────────

const SUBSCRIPTION = {
  auth_mode: "subscription",
  access_token: "claim-tok-XXXXXXXX",
  capability: "run-cap-XXXXXXXX",
  generation: 3,
  chatgpt_account_id: "verified-account",
  chatgpt_plan_type: null,
};
const RELEASE_TOK = "codex-release-tok-XXXXXXXX";
const REFRESH_TOK = "codex-refresh-tok-XXXXXXXX";

function bindingOf(block: Record<string, unknown>): CodexBinding {
  const sel = selectCodexBinding({ codex: block });
  if (sel.kind !== "codex") throw new Error("expected a codex selection");
  return sel.binding;
}

/** A registry-ownable provider root whose reap/dispose are idempotent no-ops. */
function trackedRoot(): RegisteredRoot {
  return {
    kind: "provider",
    reap: async () => ({ ok: true }),
    dispose: async () => {},
  };
}

function registerRoot(reg: ExecutionRegistry, root: RegisteredRoot): void {
  const reserved = reg.reserveLaunch(root.kind);
  if (reserved.kind === "reserved") reg.registerRoot(reserved.reservation, root);
}

/** A REAL Codex safety (via createCodexExecutionSafety over a real registry + a tracked provider
 *  root + the real run-lane reconcile) whose per-sink reconcile pushes "reconcile" on each call,
 *  then throws when `decideThrow()` says so — modelling either the api's 409 once a terminal
 *  state has been recorded, or a genuinely contended reconcile even while active. */
function codexReapSafety(events: string[], decideThrow: () => boolean): CodexExecutionSafety {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  registerRoot(registry, trackedRoot());
  const binding = bindingOf(SUBSCRIPTION);
  const reconcileClient = {
    releaseCodex: async () => {
      events.push("reconcile");
      if (decideThrow()) throw new Error("codex reconcile refused (409): run is terminal");
      return { access_token: RELEASE_TOK };
    },
    refreshCodex: async () => {
      events.push("reconcile");
      if (decideThrow()) throw new Error("codex reconcile refused (409): run is terminal");
      return { access_token: REFRESH_TOK, generation: 7, outcome: "advanced" };
    },
  };
  const reconcile = buildRunLaneReconcile("run-1531-codex", reconcileClient as never, binding, () => {});
  return createCodexExecutionSafety(
    registry,
    async () => {
      throw new Error("boundary-action spawn is not used by the reap-only path");
    },
    reconcile,
    undefined,
    async (request) => {
      const [command, ...args] = request.argv;
      if (!command) throw new Error("empty test process argv");
      const child = spawn(command, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const terminal = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, signal) => resolve({ code: code ?? (signal ? 128 : 1) }));
      });
      return {
        root: {
          kind: "boundary_action",
          reap: async () => {
            await terminal;
            return { ok: true };
          },
          dispose: async () => {
            if (child.exitCode === null) child.kill("SIGKILL");
            await terminal.catch(() => undefined);
          },
        },
        stdin: child.stdin,
        stdout: child.stdout,
        stderr: child.stderr,
        waitChild: async () => terminal,
      };
    },
  );
}

/** The injected recovery client that additionally records "reserve"/"release" into the SHARED
 *  ordered event log, so the settle's disposition is ordered relative to reconcile/failed. */
class EventRecoveryClient extends FakeRecoveryClient {
  constructor(private readonly events: string[]) {
    super();
  }
  override async reserveRecoveryCapture(
    runId: string,
    req: RecoveryReserveRequest,
  ): Promise<RecoveryReserveResponse> {
    this.events.push("reserve");
    return super.reserveRecoveryCapture(runId, req);
  }
  override async releaseRecoveryCustody(
    runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse> {
    this.events.push("release");
    return super.releaseRecoveryCustody(runId, generation, releaseEvidence);
  }
}

interface CodexReapFixture {
  coord: RecoveryCoordinator;
  client: EventRecoveryClient;
  git: FakeRecoveryGit;
  safety: CodexExecutionSafety;
  root: string;
}

export function fixture(events: string[], decideThrow: () => boolean): CodexReapFixture {
  const client = new EventRecoveryClient(events);
  const git = new FakeRecoveryGit();
  const { coord, root } = makeRecoveryCoordinator(client, git);
  const safety = codexReapSafety(events, decideThrow);
  return { coord, client, git, safety, root };
}

/** A Codex-shaped executor: carries `.safety`, NO killAgentTree, (optionally) commits, then
 *  throws a generic error — the first-turn Codex failure that reaches reportGenericFailure. */
export function codexFailFactory(homeRoot: string, safety: CodexExecutionSafety, commit: boolean): ExecutorFactory {
  return (runId) => {
    const runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        safety,
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(runHome, { recursive: true });
          if (commit) commitInTree(ctx.worktreePath, "WORK.txt", "committed before the codex failure\n");
          throw new Error("codex agent failed hard");
        },
      },
    };
  };
}
