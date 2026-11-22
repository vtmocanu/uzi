// Real executor/default adapter/launcher/auth/transport and pinned app-server HTTP proof.
// OS provisioning and supervisor evidence are simulated, as in the M4 P harness.
// This is NOT a production UID split, kernel drainage, or session-staging proof.
import assert from "node:assert/strict";
import { test } from "node:test";
import { promises as fs, mkdirSync, writeFileSync, rmSync } from "node:fs";
import path from "node:path";
import { spawn as nodeSpawn, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { createRequire } from "node:module";
// M4 lives outside the agent package; resolve its dependencies from that package.
const { parse } = createRequire(path.resolve(__dirname, "../../agent/package.json"))("smol-toml") as {
  parse(text: string): Record<string, unknown>;
};
import { CodexExecutor, type CodexExecutorDeps } from "../../agent/src/codex/codex-executor.js";
import { createCodexProviderExecutionAuthority } from "../../agent/src/codex/select.js";
import { launchCodexRoot, CODEX_BIN, type SpawnSupervisor, type SupervisorProcess } from "../../agent/src/codex/launcher.js";
import { nullLogger } from "../../agent/test/helpers.js";
import { createServer } from "node:http";
import { resolveCodexBin } from "./provision.js";
import { P_LAYER_SKIP } from "./p-platform.js";

// Adapted from M4 makeDirectCodexSpawn: only the owned real child's exit completes disposal.
function makeDirectCodexSpawn(codexBin: string, expectUid: number, envSink: { env?: NodeJS.ProcessEnv }): SpawnSupervisor {
  return (_command, _args, options): SupervisorProcess => {
    const env = { ...options.env };
    envSink.env = env;
    const child: ChildProcess = nodeSpawn(codexBin, ["app-server"], {
      cwd: options.cwd,
      env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    const control = new PassThrough();
    const evidence = new PassThrough();
    const emitter = new EventEmitter();
    let disposing = false;
    let exited = false;
    const ownedPid = child.pid;
    let killDelivered = false;
    let disposalFinished = false;
    let realClose: [number | null, NodeJS.Signals | null] | undefined;
    const forwardClose = (): void => {
      if (realClose && disposalFinished) {
        emitter.emit("close", ...realClose);
        realClose = undefined;
      }
    };
    child.once("close", (code, signal) => {
      realClose = [code, signal];
      forwardClose();
    });

    const writeEvidence = (frame: Record<string, unknown>): void => {
      evidence.write(`${JSON.stringify(frame)}\n`);
    };

    queueMicrotask(() => {
      writeEvidence({
        event: "started",
        supervisorPid: child.pid,
        childPid: ownedPid,
        subreaper: true,
        nondumpable: true,
        uid: expectUid,
        liveCapsZero: true,
        capBoundingSet: "0xc0",
        noNewPrivs: true,
      });
    });

    child.once("exit", (code, signal) => {
      exited = true;
      if (!disposing) emitter.emit("exit", code, signal);
    });
    child.once("error", (err) => emitter.emit("error", err));

    let controlBuf = "";
    control.on("data", (chunk: Buffer) => {
      controlBuf += chunk.toString("utf8");
      let nl = controlBuf.indexOf("\n");
      while (nl !== -1) {
        const line = controlBuf.slice(0, nl);
        controlBuf = controlBuf.slice(nl + 1);
        let frame: Record<string, unknown>;
        try {
          frame = JSON.parse(line) as Record<string, unknown>;
        } catch {
          nl = controlBuf.indexOf("\n");
          continue;
        }
        if (frame.op === "snapshot") {
          writeEvidence({ event: "snapshot", id: frame.id, processes: [] });
        } else if (frame.op === "dispose") {
          disposing = true;
          const finish = (): void => {
            // Only the exact owned child's observed exit can complete this report.
            writeEvidence({
              event: "dispose", id: frame.id, state: "drained", authority: "ECHILD+__WALL",
              killed: killDelivered ? [ownedPid] : [],
              reaped: exited && ownedPid !== undefined ? [ownedPid] : [],
            });
            evidence.end();
            disposalFinished = true;
            // Synthetic supervisor success is separate from the real child's exit/signal.
            emitter.emit("exit", 0, null);
            // PassThrough data parsing is synchronous: the final response is consumed first.
            forwardClose();
          };
          if (exited) finish();
          else {
            child.once("exit", finish);
            try {
              killDelivered = child.kill("SIGKILL");
              if (!killDelivered) emitter.emit("error", new Error("owned child SIGKILL was not delivered"));
            } catch (error) {
              emitter.emit("error", error);
            }
          }
        }
        nl = controlBuf.indexOf("\n");
      }
    });

    const proc: SupervisorProcess = {
      pid: child.pid,
      stdin: child.stdin,
      stdout: child.stdout,
      stderr: child.stderr,
      stdio: [child.stdin, child.stdout, child.stderr, control, evidence],
      once(event: string, listener: (...args: never[]) => void): unknown {
        emitter.once(event, listener as (...a: unknown[]) => void);
        return proc;
      },
      on(event: string, listener: (...args: never[]) => void): unknown {
        emitter.on(event, listener as (...a: unknown[]) => void);
        return proc;
      },
    };
    return proc;
  };
}


// Linux executes here; macOS must execute this P proof in the existing Linux runner.
// Missing or invalid pinned runtime is a provisioning failure, never a skip.
test("provider fake issue composes executor to pinned app-server and authenticated HTTP destination", {
  skip: P_LAYER_SKIP,
}, async (t) => {
  const resolved = resolveCodexBin();
  assert.ok(process.env.TMPDIR && path.isAbsolute(process.env.TMPDIR), "absolute scratch TMPDIR required");
  const parent = await fs.mkdtemp(path.join(process.env.TMPDIR, "provider-production-root-"));
  t.after(() => fs.rm(parent, { recursive: true, force: true }));
  const priorLoopback = process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
  process.env.UZI_PROVIDER_ALLOW_LOOPBACK = "true";
  t.after(() => { if (priorLoopback === undefined) delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    else process.env.UZI_PROVIDER_ALLOW_LOOPBACK = priorLoopback; });
  const credential = "production-root-managed-fixture-key";
  // Same deterministic SSE vocabulary as M4, kept local to the agent TS project.
  // This listener's exact bearer and recorded requests prove the responder is ours.
  const requests: Record<string, unknown>[] = [];
  const requestCap = 4 * 1024 * 1024;
  const server = createServer((request, response) => {
    if (request.url !== "/v1/responses" || request.method !== "POST" ||
        request.headers.authorization !== `Bearer ${credential}`) {
      response.writeHead(401, { connection: "close" }).end();
      return;
    }
    const chunks: Buffer[] = [];
    let bytes = 0;
    let rejected = false;
    request.on("error", () => {});
    request.on("data", (chunk: Buffer) => {
      if (rejected) return;
      bytes += chunk.length;
      if (bytes > requestCap) {
        rejected = true;
        chunks.length = 0;
        response.writeHead(413, { connection: "close" }).end();
        return;
      }
      chunks.push(chunk);
    });
    request.on("end", () => {
      if (rejected) return;
      let parsed: Record<string, unknown>;
      try { parsed = JSON.parse(Buffer.concat(chunks).toString("utf8")); }
      catch { response.writeHead(400).end(); return; }
      requests.push(parsed);
      const id = `focused-root-${requests.length}`;
      const item = requests.length === 1
        ? { type: "custom_tool_call", call_id: "root-done", name: "exec", input: "text(await tools.signal_done({}));" }
        : { type: "message", role: "assistant", content: [{ type: "output_text", text: "focused fake issue complete" }] };
      const frames = [
        { type: "response.created", response: { id } },
        { type: "response.output_item.done", item },
        { type: "response.completed", response: { id, usage: { input_tokens: 0, output_tokens: 0,
          total_tokens: 0, input_tokens_details: null, output_tokens_details: null } } },
      ];
      response.writeHead(200, { "content-type": "text/event-stream" });
      response.end(frames.map(frame => `event: ${frame.type}\ndata: ${JSON.stringify(frame)}\n\n`).join(""));
    });
  });
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  t.after(() => new Promise<void>(resolve => server.close(() => resolve())));
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  const fake = { baseUrl: `http://127.0.0.1:${address.port}/v1`, requests };
  const unauthorized = await fetch(`${fake.baseUrl}/responses`, { method: "POST", body: "{}" });
  assert.equal(unauthorized.status, 401);
  await unauthorized.arrayBuffer();
  const oversized = await fetch(`${fake.baseUrl}/responses`, { method: "POST",
    headers: { authorization: `Bearer ${credential}` },
    body: new ReadableStream<Uint8Array>({ start(controller) {
      for (let i = 0; i <= requestCap / 65536; i++) controller.enqueue(new Uint8Array(65536));
      controller.close();
    } }), ...{ duplex: "half" } });
  assert.equal(oversized.status, 413);
  await oversized.arrayBuffer();
  assert.equal(requests.length, 0, "refused input never reaches the responder");
  const input = { provider_id: "focused-root", revision: 7, engine: "codex" as const,
    protocol: "openai_responses" as const, base_url: fake.baseUrl, auth_style: "bearer" as const,
    credential, model_map: { default: { model: "gpt-6-astra", context_window: 65536 } } };
  const binding = createCodexProviderExecutionAuthority(input);
  input.base_url = "https://mutated.invalid/v1";
  input.credential = "mutated-key";
  input.model_map.default.model = "mutated-model";
  input.model_map.default.context_window = 8192;
  const vendorCalls: string[] = [];
  const forbidden = (name: string) => () => { vendorCalls.push(name); throw new Error(name); };
  const client = { releaseCodex: forbidden("release"), refreshCodex: forbidden("refresh"),
    codexAdmissionReporter: forbidden("admission-reporter"),
    checkCodexOperationAdmission: forbidden("operation-admission") };
  const home = path.join(parent, "home"), workspace = path.join(parent, "work");
  await fs.mkdir(workspace);
  const configs: Record<string, unknown>[] = [];
  const methods: { method?: string; params?: unknown }[] = [];
  let launches = 0, removals = 0, disposals = 0;
  const streams: PassThrough[] = [];
  t.after(() => { for (const stream of streams) stream.destroy(); });
  const deps: CodexExecutorDeps = {
    installDeps: async () => ({ results: [], truncated: false }),
    provisionRunTools: async () => ({ toolEnv: {} }),
    spawnCommand: async () => ({ code: 0, stdout: JSON.stringify({ uzi_envprobe: 1, proc: "ok", home: "ok", tmp: "ok" }) + "\n", stderr: "" }),
    rootStartTime: pid => pid * 10,
    launchEffectRoot: async () => {
      const stdin = new PassThrough(), stdout = new PassThrough(), stderr = new PassThrough();
      streams.push(stdin, stdout, stderr);
      return { started: { event: "started", supervisorPid: 200, childPid: 201, subreaper: true,
        nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
        supervisorPid: 200, transport: { stdin, stdout, stderr },
        snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
        observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
        waitChild: async () => ({ event: "child_exit", code: 0 }),
        failed: undefined, whenFailed: new Promise<Error>(() => {}),
        dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }) };
    },
    wireFileop: () => ({ client: { op: async () => ({ ok: true }) }, dispose: async () => {} }),
    sessionStore: { adopt: async () => ({ files: 0 }), inspect: async () => "absent",
      remove: async () => {}, persist: async () => ({ files: 0, bytes: 0 }) },
    launchCodexRootForTest: async (spec, launcherDeps) => {
      launches++;
      assert.equal(spec.authMode, "provider");
      assert.equal(spec.codexBin, CODEX_BIN);
      assert.deepEqual(spec.providerBinding, binding.providerBinding);
      const envSink = {};
      const spawn = makeDirectCodexSpawn(resolved.codexBin, 10002, envSink);
      const handle = await launchCodexRoot(spec, { ...launcherDeps,
        env: { UZI_UID_SPLIT: "1", UZI_PROVIDER_ALLOW_LOOPBACK: "true" },
        resolveRunnerUid: () => 10002, rootStartTime: () => 1,
        makeRunnerTrees: request => {
          for (const dir of request.dirs) mkdirSync(dir, { recursive: true });
          for (const file of request.files) {
            writeFileSync(file.path, file.content, { mode: file.mode });
            if (path.basename(file.path) === "config.toml") configs.push(parse(file.content));
          }
        },
        removeRunnerTree: request => { removals++; rmSync(request.root, { recursive: true, force: true }); },
        spawnSupervisor: (command, args, options) => {
          assert.equal(options.env.CODEX_EXEC_SERVER_URL, "none", "generated launcher env preserves native exec guard");
          assert.equal(JSON.stringify(options.env).includes(credential), false);
          const peer = spawn(command, args, options);
          // Passive observation only: bytes still go to the real pinned app-server.
          let pending = "";
          const outbound = new PassThrough();
          streams.push(outbound);
          outbound.pipe(peer.stdin!);
          outbound.on("data", chunk => {
            pending += chunk.toString();
            let newline: number;
            while ((newline = pending.indexOf("\n")) >= 0) {
              methods.push(JSON.parse(pending.slice(0, newline)));
              pending = pending.slice(newline + 1);
            }
          });
          return { ...peer, stdin: outbound, stdio: [outbound, ...peer.stdio.slice(1)] };
        },
      });
      return { ...handle, dispose: async ms => {
        const result = await handle.dispose(ms);
        assert.equal(result.clean, true, JSON.stringify(result));
        disposals++;
        return result;
      } };
    },
    idleMs: 15000, wallMs: 30000, boundaryDeadlineMs: 5000, commandTmpdir: parent,
  };
  const emitted: unknown[] = [];
  const executor = new CodexExecutor(nullLogger(), home, { binding, client: client as never,
    provider: { name: "openai", baseUrl: "https://unused.invalid/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" } }, deps);
  await executor.run({ runId: "focused-provider-root", issueIid: 42, issueTitle: "Fake issue",
    issueDescription: "Complete the approved focused proof", worktreePath: workspace, branch: "agent/issue-42",
    planApproved: true, approvedPlan: "Signal completion", agents: [], emit: m => { emitted.push(m); } });
  assert.equal(launches, 1);
  assert.ok(disposals >= 1);
  assert.equal(removals, 1);
  assert.deepEqual(vendorCalls, []);
  assert.deepEqual(methods.find(m => m.method === "account/login/start")?.params, { type: "apiKey", apiKey: credential });
  assert.equal(configs.length, 1);
  assert.equal(configs[0]!.model, "gpt-6-astra");
  assert.equal(configs[0]!.model_context_window, 65536);
  const providers = configs[0]!.model_providers as Record<string, { base_url: string; requires_openai_auth: boolean }>;
  assert.equal(Object.keys(providers).length, 1);
  const selected = providers[configs[0]!.model_provider as string]!;
  assert.equal(selected.base_url, fake.baseUrl);
  assert.equal(selected.requires_openai_auth, true);
  assert.ok(fake.requests.length >= 2, "authenticated real HTTP request and callback continuation");
  for (const request of fake.requests) assert.equal(request.model, "gpt-6-astra");
  assert.match(JSON.stringify(fake.requests.slice(1)), /root-done/, "callback result returns to our HTTP listener");
  assert.ok(methods.some(m => m.method === "turn/start"));
  assert.ok(emitted.length > 0);
});
