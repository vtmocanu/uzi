// Issue #4574 M2: a provider-bound Codex cross-check, plan and code, through CrossCheckRunner ->
// CodexCrossCheck -> launchCodexRoot -> app-server auth -> the REAL pinned app-server, against the
// authenticated local HTTP Responses fake (the shape of e2e/codex-m4/fake-provider.ts, inlined because the
// agent typecheck does not include e2e sources). Only the OS-bound
// primitives are simulated (the supervisor spawn, runner-tree provisioning, the checker's fileop helper),
// as e2e/codex-m4/provider-advice-judge.test.ts does: this is not a UID-split, Landlock or
// kernel-drainage proof (the packaged confinement probes own those).
// The pinned runtime is baked at /opt/uzi-codex in the worker image. Where it is absent this file reports a
// skip with the reason. This is a local real-runtime check: CI (test-agent-shard, the image build) does
// not have /opt/uzi-codex, so it skips there. The CI real-runtime proof for the provider checker is the
// private-CA image fixture in `test:codex-m4` (e2e/codex-private-ca/fixture.test.ts).
import assert from "node:assert/strict";
import { existsSync, mkdirSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";
import { PassThrough, Writable } from "node:stream";
import { spawn as nodeSpawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { test } from "node:test";
import { CodexCrossCheck } from "../src/codex/cross-check.js";
import { launchCodexRoot, type CodexRootHandle, type SpawnSupervisor, type SupervisorProcess } from "../src/codex/launcher.js";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import { providerKeyForms } from "../src/redact.js";
import type { WorkerClient } from "../src/client.js";
import type { GitCache } from "../src/git.js";
import type { ClaimResponse } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";

const BAKED = "/opt/uzi-codex";
const bakedBin = (): string | undefined => {
  if (!existsSync(BAKED)) return undefined;
  const version = readdirSync(BAKED).find(v => existsSync(path.join(BAKED, v, "bin", "codex")));
  return version === undefined ? undefined : path.join(BAKED, version, "bin", "codex");
};
const runtimePresent = bakedBin() !== undefined;

/** A direct spawn of the pinned app-server standing in for the OS supervisor (the shape of
 *  e2e/codex-m4/direct-codex-spawn.ts): only the owned child's exit completes disposal. */
function directSpawn(codexBin: string): SpawnSupervisor {
  return (_command, _args, options): SupervisorProcess => {
    const child = nodeSpawn(codexBin, ["app-server"], { cwd: options.cwd, env: { ...options.env }, stdio: ["pipe", "pipe", "pipe"] });
    const control = new PassThrough(), evidence = new PassThrough(), emitter = new EventEmitter();
    let exited = false, disposing = false, disposalFinished = false;
    let realClose: [number | null, NodeJS.Signals | null] | undefined;
    const forwardClose = (): void => { if (realClose && disposalFinished) { emitter.emit("close", ...realClose); realClose = undefined; } };
    child.once("close", (code, signal) => { realClose = [code, signal]; forwardClose(); });
    const write = (frame: Record<string, unknown>): void => { evidence.write(JSON.stringify(frame) + "\n"); };
    queueMicrotask(() => write({ event: "started", supervisorPid: child.pid, childPid: child.pid, subreaper: true, nondumpable: true,
      uid: 10002, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true }));
    child.once("exit", (code, signal) => { exited = true; if (!disposing) emitter.emit("exit", code, signal); });
    child.once("error", (err) => emitter.emit("error", err));
    let buffer = "";
    control.on("data", (chunk: Buffer) => {
      buffer += chunk.toString("utf8");
      let nl: number;
      while ((nl = buffer.indexOf("\n")) !== -1) {
        const line = buffer.slice(0, nl); buffer = buffer.slice(nl + 1);
        let frame: Record<string, unknown>;
        try { frame = JSON.parse(line) as Record<string, unknown>; } catch { continue; }
        if (frame.op === "snapshot") write({ event: "snapshot", id: frame.id, processes: [] });
        else if (frame.op === "dispose") {
          disposing = true;
          const finish = (): void => {
            write({ event: "dispose", id: frame.id, state: "drained", authority: "ECHILD+__WALL", killed: [], reaped: exited && child.pid !== undefined ? [child.pid] : [] });
            evidence.end(); disposalFinished = true; emitter.emit("exit", 0, null); forwardClose();
          };
          if (exited) finish(); else { child.once("exit", finish); child.kill("SIGKILL"); }
        }
      }
    });
    const proc: SupervisorProcess = { pid: child.pid, stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
      stdio: [child.stdin, child.stdout, child.stderr, control, evidence],
      once(event: string, listener: (...args: never[]) => void): unknown { emitter.once(event, listener as (...a: unknown[]) => void); return proc; },
      on(event: string, listener: (...args: never[]) => void): unknown { emitter.on(event, listener as (...a: unknown[]) => void); return proc; } };
    return proc;
  };
}

/** A loopback Responses fake (the shape of e2e/codex-m4/fake-provider.ts): it authenticates one exact bearer,
 *  records every parsed request body and receipt, and streams one assistant message. */
class Gateway {
  readonly requests: Array<{ model?: unknown } & Record<string, unknown>> = [];
  readonly receipts: { authorization: string | undefined; status: number }[] = [];
  private constructor(private readonly server: Server, readonly port: number) {}
  get baseUrl(): string { return `http://127.0.0.1:${this.port}/v1`; }
  static async start(credential: string, text: () => string): Promise<Gateway> {
    const holder: { gw?: Gateway } = {};
    const server = createServer((req: IncomingMessage, res: ServerResponse) => {
      if (req.method !== "POST" || req.url !== "/v1/responses") { req.resume(); res.writeHead(404).end(); return; }
      let raw = "";
      req.setEncoding("utf8");
      req.on("data", (c: string) => { raw += c; });
      req.on("end", () => {
        const auth = req.headers.authorization;
        if (auth !== `Bearer ${credential}`) { res.writeHead(401).end(); return; }
        holder.gw!.requests.push(JSON.parse(raw) as { model?: unknown });
        const id = `gw-${holder.gw!.requests.length}`;
        const item = { type: "message", role: "assistant", content: [{ type: "output_text", text: text() }] };
        const frames = [{ type: "response.created", response: { id } }, { type: "response.output_item.done", item },
          { type: "response.completed", response: { id, usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0,
            input_tokens_details: null, output_tokens_details: null } } }];
        const data = frames.map(f => `event: ${f.type}\ndata: ${JSON.stringify(f)}\n\n`).join("");
        holder.gw!.receipts.push({ authorization: auth, status: 200 });
        res.writeHead(200, { "content-type": "text/event-stream", "content-length": Buffer.byteLength(data) });
        res.end(data);
      });
    });
    await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
    const address = server.address();
    if (address === null || typeof address === "string") throw new Error("gateway must bind 127.0.0.1");
    holder.gw = new Gateway(server, address.port);
    return holder.gw;
  }
  async close(): Promise<void> {
    this.server.closeAllConnections();
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
  }
}
const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");

// Assembled at runtime: no provider-token-shaped literal in source.
const KEY = ["gw", "fixture", "key-5d2a77c1"].join("/");
const FORMS = providerKeyForms(KEY);
const TARGET = "gateway/astra-target";
const MAP = { default: { model: "gateway/default-model", context_window: 8192 }, "gpt-6-astra": { model: TARGET, context_window: 32768 } };

function claim(stage: "plan" | "code", baseUrl: string): ClaimResponse {
  const cross_check = stage === "code"
    ? { stage: "code", lead_run_id: "lead", round: 1, candidate_digest: "c".repeat(64), base_commit: "a".repeat(40),
      head_commit: "b".repeat(40), plan_md: "Approved plan DATA", guidance_snapshot: "", milestones: [], required_capabilities: [],
      required_tools: [], size_class: "small", code_context: { task: "context DATA" }, code_diff: "diff DATA", deadline_at: "2099-01-01T00:00:00Z" }
    : { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest", deadline_at: "2099-01-01T00:00:00Z",
      plan_md: "Plan", milestones: [], required_capabilities: [], required_tools: [], size_class: "small",
      base_commit: "a".repeat(40), planning_diff: "diff" };
  return { kind: "cross_check", run_id: "child-" + stage, claim_generation: 7, last_seq: 0, agents: [], issue_iid: null,
    issue_title: "Issue", issue_description: "Issue DATA",
    repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/repo", clone_url: "https://example.test/repo.git", default_branch: "main" },
    config: { default_model: "gpt-6-astra", default_effort: "high" },
    secrets: { forge_pat: "", provider: { provider_id: "gw-1", revision: 2, engine: "codex", protocol: "openai_responses",
      base_url: baseUrl, auth_style: "bearer", credential: KEY, model_map: MAP } },
    cross_check } as unknown as ClaimResponse;
}

const verdictFor = (stage: "plan" | "code"): string => stage === "code"
  ? JSON.stringify({ findings: [{ id: "F_1", severity: "minor", path: "anchor.ts", line: 1, title: "echo", detail: `the gateway echoed ${KEY}` }] })
  : JSON.stringify({ verdict: "approve", summary: `the gateway echoed ${KEY}`, items: [] });

function collect(value: unknown, out: string[] = []): string[] {
  if (typeof value === "string") out.push(value);
  else if (Array.isArray(value)) for (const v of value) collect(v, out);
  else if (value !== null && typeof value === "object") for (const [k, v] of Object.entries(value)) { out.push(k); collect(v, out); }
  return out;
}

test("provider-bound Codex checker, plan and code, on the real app-server against the fake gateway",
  { skip: runtimePresent ? false : "pinned Codex runtime is not baked at /opt/uzi-codex" }, async (t) => {
    const codexBin = bakedBin()!;
    mkdirSync(scratch, { recursive: true });
    const parent = await fs.mkdtemp(path.join(scratch, "provider-cross-check-"));
    t.after(() => fs.rm(parent, { recursive: true, force: true }));
    const prior = process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    process.env.UZI_PROVIDER_ALLOW_LOOPBACK = "true";
    t.after(() => { if (prior === undefined) delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK; else process.env.UZI_PROVIDER_ALLOW_LOOPBACK = prior; });

    let stage: "plan" | "code" = "plan";
    const gateway = await Gateway.start(KEY, () => verdictFor(stage));
    t.after(() => gateway.close());

    // Any use of the worker client by the checker is a vendor release/refresh/admission call: there must be none.
    const clientCalls: string[] = [];
    const checkerClient = new Proxy({}, { get: (_t, prop) => { clientCalls.push(String(prop)); return undefined; } }) as unknown as WorkerClient;

    let launches = 0, disposals = 0;
    const fileop = (): CodexRootHandle => {
      const stdout = new PassThrough();
      let buffer = "";
      const stdin = new Writable({ write(chunk, _e, done) {
        buffer += chunk.toString();
        let nl: number;
        while ((nl = buffer.indexOf("\n")) >= 0) {
          const frame = JSON.parse(buffer.slice(0, nl)) as { id: number; op: string };
          buffer = buffer.slice(nl + 1);
          stdout.write(JSON.stringify({ id: frame.id, ok: frame.op === "stat" }) + "\n");
        }
        done();
      } });
      return { transport: { stdin, stdout, stderr: null }, whenFailed: new Promise<Error>(() => {}), failed: undefined,
        dispose: async () => { stdout.end(); return { clean: true }; } } as unknown as CodexRootHandle;
    };
    const { logger: log, lines } = recordingLogger();
    const checker = new CodexCrossCheck(checkerClient, log, {
      prepareHome: async () => {},
      launchFileop: async () => fileop(),
      launchProvider: async (spec, launcherDeps) => {
        launches++;
        const spawn = directSpawn(codexBin);
        const handle = await launchCodexRoot(spec, { ...launcherDeps,
          env: { UZI_UID_SPLIT: "1", UZI_PROVIDER_ALLOW_LOOPBACK: "true" },
          resolveRunnerUid: () => 10002, rootStartTime: () => 1,
          makeRunnerTrees: request => {
            for (const dir of request.dirs) mkdirSync(dir, { recursive: true });
            for (const file of request.files) writeFileSync(file.path, file.content, { mode: file.mode });
          },
          removeRunnerTree: request => { rmSync(request.root, { recursive: true, force: true }); },
          spawnSupervisor: (command, args, options) => {
            assert.equal(JSON.stringify(options.env).includes(KEY), false, "the key never rides the launcher env");
            return spawn(command, args, options);
          },
        });
        return { ...handle, dispose: async ms => {
          const result = await handle.dispose(ms);
          assert.equal(result.clean, true, JSON.stringify(result));
          disposals++;
          return result;
        } };
      },
    });

    for (const current of ["plan", "code"] as const) {
      stage = current;
      const root = path.join(parent, current);
      await fs.mkdir(path.join(root, "checkout"), { recursive: true });
      const posted: unknown[] = [];
      const verdicts: unknown[] = [];
      const runnerClient = {
        reportState: async () => ({ applied: true }), getInputs: async () => ({ inputs: [] }),
        postMessages: async (...args: unknown[]) => { posted.push(args); },
        reportCrossCheckVerdict: async (_id: string, _gen: number, v: unknown) => { verdicts.push(v); },
        reportCodeCrossCheckVerdict: async (_id: string, _gen: number, v: unknown) => { verdicts.push(v); },
        getCodeSnapshotCleanup: async () => { throw new Error("unused"); },
      } as unknown as WorkerClient;
      const git = {
        ensureClone: async () => "/bare", barePathFor: () => "/bare", existingBarePathFor: async () => "/bare",
        runnerCloneAtCommit: async () => path.join(root, "checkout"), readBare: async () => ({ text: "diff", truncated: false }),
        removeRunnerClone: async () => {},
      } as unknown as GitCache;
      const before = gateway.requests.length;
      const receiptsBefore = gateway.receipts.length;
      await new CrossCheckRunner(runnerClient, git, log, { homeRoot: path.join(root, "home"), pollMs: 1, batchMs: 5,
        modelTimeoutMs: 90_000, model: checker }).execute(claim(current, gateway.baseUrl));

      assert.equal(gateway.requests.length, before + 1, `${current}: one gateway request`);
      assert.equal(gateway.requests.at(-1)!.model, TARGET, `${current}: the gateway received the mapped target`);
      assert.deepEqual(gateway.receipts.slice(receiptsBefore), [{ authorization: `Bearer ${KEY}`, status: 200 }],
        `${current}: the frozen key is the only bearer`);
      assert.equal(verdicts.length, 1, `${current}: one verdict`);
      // Key echo, positive control: the gateway answered with the key (it reached the worker verbatim), and
      // the delivered verdict and frames carry no form of it.
      assert.ok(verdictFor(current).includes(KEY), "control: the gateway's answer carries the key");
      assert.deepEqual(collect(verdicts[0]).filter(s => FORMS.some(f => s.includes(f))), [], `${current}: delivered verdict is masked`);
      assert.ok(collect(verdicts[0]).some(s => s.includes("***REDACTED***")), `${current}: the verdict is kept, key masked`);
      assert.notEqual((verdicts[0] as { verdict?: string }).verdict, "failed", `${current}: the check completed`);
      assert.deepEqual(collect(posted).filter(s => FORMS.some(f => s.includes(f))), [], `${current}: delivered frames are masked`);
    }
    assert.deepEqual(clientCalls, [], "zero vendor credential operations (no release, refresh or admission)");
    assert.equal(launches, disposals, "every launched provider root was disposed cleanly");
    assert.equal(launches, 2);
    assert.equal(JSON.stringify(lines).includes(KEY), false, "no log line carries the key");
  });
