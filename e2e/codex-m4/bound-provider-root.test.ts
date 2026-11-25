// Real executor/default adapter/launcher/auth/transport and pinned app-server HTTP proof.
// OS provisioning and supervisor evidence are simulated, as in the M4 P harness.
// This is NOT a production UID split, kernel drainage, or session-staging proof.
import assert from "node:assert/strict";
import { test } from "node:test";
import { promises as fs, mkdirSync, writeFileSync, rmSync } from "node:fs";
import path from "node:path";
import { PassThrough } from "node:stream";
import { createRequire } from "node:module";
// M4 lives outside the agent package; resolve its dependencies from that package.
const { parse } = createRequire(path.resolve(__dirname, "../../agent/package.json"))("smol-toml") as {
  parse(text: string): Record<string, unknown>;
};
import { CodexExecutor, type CodexExecutorDeps } from "../../agent/src/codex/codex-executor.js";
import { createCodexProviderExecutionAuthority } from "../../agent/src/codex/select.js";
import { launchCodexRoot, CODEX_BIN } from "../../agent/src/codex/launcher.js";
import { makeDirectCodexSpawn } from "./direct-codex-spawn.js";
import { nullLogger } from "../../agent/test/helpers.js";
import { createServer } from "node:http";
import { resolveCodexBin } from "./provision.js";
import { recordEvidence } from "./evidence.js";
import { PROVIDER_ROOT_TITLES } from "./titles-provider.js";
import { P_LAYER_SKIP } from "./p-platform.js";



// Linux executes here; macOS must execute this P proof in the existing Linux runner.
// Missing or invalid pinned runtime is a provisioning failure, never a skip.
for (const scenario of [
  { redirect: undefined, crossPort: false, title: PROVIDER_ROOT_TITLES.direct },
  { redirect: 307, crossPort: false, title: PROVIDER_ROOT_TITLES.same307 },
  { redirect: 308, crossPort: false, title: PROVIDER_ROOT_TITLES.same308 },
  { redirect: 307, crossPort: true, title: PROVIDER_ROOT_TITLES.cross307 },
  { redirect: 308, crossPort: true, title: PROVIDER_ROOT_TITLES.cross308 },
] as const) {
  const { redirect, crossPort, title } = scenario;
  test(title, { skip: P_LAYER_SKIP }, async (t) => {
    recordEvidence(title, "fail");
    const resolved = resolveCodexBin();
    const scratch = path.resolve(__dirname, "../../.uzi/scratch");
    mkdirSync(scratch, { recursive: true });
    const parent = await fs.mkdtemp(path.join(scratch, "provider-production-root-"));
    t.after(() => fs.rm(parent, { recursive: true, force: true }));
    const priorLoopback = process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    process.env.UZI_PROVIDER_ALLOW_LOOPBACK = "true";
    t.after(() => { if (priorLoopback === undefined) delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
      else process.env.UZI_PROVIDER_ALLOW_LOOPBACK = priorLoopback; });
    const credential = ["production-root", "managed-fixture-key"].join("-");
    type Receipt = { method: string | undefined; url: string | undefined; authorization: string | undefined; body: string };
    const receiverLog: Receipt[] = [];
    const originLog: Receipt[] = [];
    let redirectEnabled = false;
    // Same deterministic SSE vocabulary as M4, kept local to the agent TS project.
    // This listener's exact bearer and recorded requests prove the responder is ours.
    const requests: Record<string, unknown>[] = [];
    const requestCap = 4 * 1024 * 1024;
    const server = createServer((request, response) => {
      const receipt: Receipt = { method: request.method, url: request.url,
        authorization: request.headers.authorization, body: "" };
      const isOrigin = redirectEnabled && !crossPort && request.url === "/v1/responses";
      (isOrigin ? originLog : receiverLog).push(receipt);
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
        receipt.body = Buffer.concat(chunks).toString("utf8");
        if (isOrigin) {
          response.writeHead(redirect!, { location: "/accepted/responses" }).end();
          return;
        }
        const acceptedPath = redirectEnabled && !crossPort ? "/accepted/responses" : "/v1/responses";
        if (request.url !== acceptedPath || request.method !== "POST" ||
            request.headers.authorization !== `Bearer ${credential}`) {
          response.writeHead(401, { connection: "close" }).end();
          return;
        }
        let parsed: Record<string, unknown>;
        try { parsed = JSON.parse(receipt.body); }
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
    const oversizedInit = { method: "POST",
      headers: { authorization: `Bearer ${credential}` },
      body: new ReadableStream<Uint8Array>({ start(controller) {
        for (let i = 0; i <= requestCap / 65536; i++) controller.enqueue(new Uint8Array(65536));
        controller.close();
      } }), duplex: "half" as const };
    const oversized = await fetch(`${fake.baseUrl}/responses`, oversizedInit);
    assert.equal(oversized.status, 413);
    await oversized.arrayBuffer();
    assert.equal(requests.length, 0, "refused input never reaches the responder");
    // Independent receiver controls: neither missing nor incorrect credentials may
    // advance the SSE responder; a discovery path must not accept requests either.
    const wrongAuth = await fetch(`${fake.baseUrl}/responses`, { method: "POST",
      headers: { authorization: "Bearer wrong-receiver-key" }, body: "{}" });
    assert.equal(wrongAuth.status, 401);
    await wrongAuth.arrayBuffer();
    const models = await fetch(`${fake.baseUrl}/models`);
    assert.equal(models.status, 401);
    await models.arrayBuffer();
    assert.equal(requests.length, 0, "negative receiver controls cannot produce Responses frames");
    assert.deepEqual(receiverLog.map(r => [r.method, r.url, r.authorization]), [
      ["POST", "/v1/responses", undefined],
      ["POST", "/v1/responses", `Bearer ${credential}`], // oversized control
      ["POST", "/v1/responses", "Bearer wrong-receiver-key"],
      ["GET", "/v1/models", undefined],
    ], "controls demonstrably reached this receiver");
    receiverLog.length = 0;
    let baseUrl = fake.baseUrl;
    redirectEnabled = redirect !== undefined;
    if (crossPort) {
      const origin = createServer((request, response) => {
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
          originLog.push({ method: request.method, url: request.url,
            authorization: request.headers.authorization, body: Buffer.concat(chunks).toString("utf8") });
          if (request.method !== "POST" || request.url !== "/v1/responses") {
            response.writeHead(404).end();
            return;
          }
          response.writeHead(redirect, { location: `${fake.baseUrl}/responses` }).end();
        });
      });
      await new Promise<void>((resolve, reject) => {
        origin.once("error", reject);
        origin.listen(0, "127.0.0.1", resolve);
      });
      t.after(() => new Promise<void>(resolve => origin.close(() => resolve())));
      const originAddress = origin.address();
      assert.ok(originAddress && typeof originAddress !== "string");
      baseUrl = `http://127.0.0.1:${originAddress.port}/v1`;
    }
    const input = { provider_id: "focused-root", revision: 7, engine: "codex" as const,
      protocol: "openai_responses" as const, base_url: baseUrl, auth_style: "bearer" as const,
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
    const run = executor.run({ runId: "focused-provider-root", issueIid: 42, issueTitle: "Fake issue",
      issueDescription: "Complete the approved focused proof", worktreePath: workspace, branch: "agent/issue-42",
      planApproved: true, approvedPlan: "Signal completion", agents: [], emit: m => { emitted.push(m); } });
    if (crossPort) {
      await assert.rejects(run, /httpConnectionFailed; http 401/);
      assert.equal(requests.length, 0, "unauthorized redirect cannot advance Responses");
      assert.equal(receiverLog.length, 1, "no retry or discovery at rejected receiver");
      assert.equal(originLog.length, 1);
      assert.equal(originLog[0]!.authorization, `Bearer ${credential}`);
      assert.equal(receiverLog[0]!.authorization, undefined, "pin strips bearer across ports");
      assert.equal(receiverLog[0]!.method, "POST");
      assert.equal(receiverLog[0]!.url, "/v1/responses");
      assert.equal(receiverLog[0]!.body, originLog[0]!.body, "307/308 still forwards the raw body");
      assert.equal(JSON.parse(receiverLog[0]!.body).model, "gpt-6-astra");
    } else {
      await run;
    }
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
    assert.equal(selected.base_url, baseUrl);
    assert.equal(selected.requires_openai_auth, true);
    if (crossPort) {
      recordEvidence(title, "pass");
      return;
    }
    assert.ok(fake.requests.length >= 2, "authenticated real HTTP request and callback continuation");
    for (const request of fake.requests) assert.equal(request.model, "gpt-6-astra");
    assert.match(JSON.stringify(fake.requests.slice(1)), /root-done/, "callback result returns to our HTTP listener");
    assert.ok(methods.some(m => m.method === "turn/start"));
    assert.ok(emitted.length > 0);
    assert.equal(receiverLog.length, requests.length, "no discovery or extra receiver traffic");
    for (const receipt of receiverLog) {
      assert.equal(receipt.method, "POST");
      assert.equal(receipt.url, redirect === undefined ? "/v1/responses" : "/accepted/responses",
        "only accepted Responses path carries the key at receiver");
      assert.equal(receipt.authorization, `Bearer ${credential}`);
      assert.equal(JSON.parse(receipt.body).model, "gpt-6-astra");
    }
    if (redirect !== undefined) {
      // Characterization of the installed pin, not an assertion of confinement:
      // an accepted owner's same-origin redirect forwards auth/body.
      assert.deepEqual(receiverLog, originLog.map(r => ({ ...r, url: "/accepted/responses" })),
        "redirect preserves method, raw body and bearer");
    } else {
      assert.deepEqual(originLog, [], "direct production control needs no redirect");
    }
    recordEvidence(title, "pass");
  });
}
