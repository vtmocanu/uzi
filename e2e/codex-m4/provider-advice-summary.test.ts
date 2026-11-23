// PRD #4323: the provider-bound advice pass and summary pass through the PRODUCTION advice factory
// (makeProductionCodexAdviceHarnessFactory -> makeProductionLaunchAdviceRoot -> CodexAdviceHarness
// -> launchCodexRoot -> app-server auth -> real transport) against the pinned app-server and an
// authenticated local HTTP Responses fake.
// Only the OS-bound primitives are simulated, as in bound-provider-root.test.ts: the supervisor spawn,
// runner-tree provisioning and the shared-directory owner. This is NOT a production UID split, kernel
// drainage, or session-staging proof.
import assert from "node:assert/strict";
import { test } from "node:test";
import { promises as fs, mkdirSync, writeFileSync, rmSync } from "node:fs";
import path from "node:path";
import { createServer } from "node:http";
import { PassThrough } from "node:stream";
import { createRequire } from "node:module";
// M4 lives outside the agent package; resolve its dependencies from that package.
const { parse } = createRequire(path.resolve(__dirname, "../../agent/package.json"))("smol-toml") as {
  parse(text: string): Record<string, unknown>;
};
import { makeProductionCodexAdviceHarnessFactory } from "../../agent/src/codex/codex-executor.js";
import { launchCodexRoot } from "../../agent/src/codex/launcher.js";
import { selectCodexExecutionAuthority } from "../../agent/src/codex/select.js";
import { runReadOnlyModelPass } from "../../agent/src/model-pass.js";
import { SummaryRunner } from "../../agent/src/summary-runner.js";
import type { WorkerClient } from "../../agent/src/client.js";
import { recordingLogger } from "../../agent/test/helpers.js";
import { makeDirectCodexSpawn } from "./direct-codex-spawn.js";
import { resolveCodexBin } from "./provision.js";
import { recordEvidence } from "./evidence.js";
import { PROVIDER_ADVICE_SUMMARY_TITLE } from "./titles-provider.js";
import { P_LAYER_SKIP } from "./p-platform.js";

// Linux executes here; macOS must execute this P proof in the existing Linux runner.
// Missing or invalid pinned runtime is a provisioning failure, never a skip.
test(PROVIDER_ADVICE_SUMMARY_TITLE, { skip: P_LAYER_SKIP }, async (t) => {
  recordEvidence(PROVIDER_ADVICE_SUMMARY_TITLE, "fail");
  const resolved = resolveCodexBin();
  const scratch = path.resolve(__dirname, "../../.uzi/scratch");
  mkdirSync(scratch, { recursive: true });
  const parent = await fs.mkdtemp(path.join(scratch, "provider-advice-summary-"));
  t.after(() => fs.rm(parent, { recursive: true, force: true }));
  const priorLoopback = process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
  process.env.UZI_PROVIDER_ALLOW_LOOPBACK = "true";
  t.after(() => { if (priorLoopback === undefined) delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    else process.env.UZI_PROVIDER_ALLOW_LOOPBACK = priorLoopback; });

  // The key carries "/" so its JSON "\/" and percent-encoded forms differ from the raw one.
  const credential = ["advice-summary", "gw/fixture", "key-7c1e"].join("-");
  const escaped = credential.replaceAll("/", "\\/");
  const percent = encodeURIComponent(credential);
  const echoed = `head RAW:${credential} ESC:${escaped} PCT:${percent} tail`;

  type Receipt = { method: string | undefined; url: string | undefined; headers: Record<string, string | string[] | undefined>; body: string };
  const receipts: Receipt[] = [];
  const requests: Record<string, unknown>[] = [];
  let nextText = "";
  const server = createServer((request, response) => {
    const chunks: Buffer[] = [];
    request.on("error", () => {});
    request.on("data", (chunk: Buffer) => { chunks.push(chunk); });
    request.on("end", () => {
      const receipt: Receipt = { method: request.method, url: request.url, headers: { ...request.headers },
        body: Buffer.concat(chunks).toString("utf8") };
      receipts.push(receipt);
      if (request.url !== "/v1/responses" || request.method !== "POST" ||
          request.headers.authorization !== `Bearer ${credential}`) {
        response.writeHead(401, { connection: "close" }).end();
        return;
      }
      let parsed: Record<string, unknown>;
      try { parsed = JSON.parse(receipt.body); } catch { response.writeHead(400).end(); return; }
      requests.push(parsed);
      const id = `advice-summary-${requests.length}`;
      const frames = [
        { type: "response.created", response: { id } },
        { type: "response.output_item.done", item: { type: "message", role: "assistant",
          content: [{ type: "output_text", text: nextText }] } },
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
  const baseUrl = `http://127.0.0.1:${address.port}/v1`;
  const unauthorized = await fetch(`${baseUrl}/responses`, { method: "POST", body: "{}" });
  assert.equal(unauthorized.status, 401);
  await unauthorized.arrayBuffer();
  assert.equal(requests.length, 0, "an unauthenticated control never reaches the responder");
  receipts.length = 0;

  // The claim's raw provider block; the frozen binding is minted from it by the production selector.
  const rawProvider = { provider_id: "advice-summary", revision: 3, engine: "codex" as const,
    protocol: "openai_responses" as const, base_url: baseUrl, auth_style: "bearer" as const, credential,
    model_map: {
      default: { model: "gateway/default-model", context_window: 8192 },
      "gpt-6-sol": { model: "gateway/sol-model", context_window: 32768 },
      "gpt-6-luna": { model: "gateway/luna-model", context_window: 16384 },
    } };
  const secrets = { provider: rawProvider };
  const selection = selectCodexExecutionAuthority(secrets);
  assert.equal(selection.kind, "codex");
  const binding = selection.binding;
  assert.equal(binding.authMode, "provider");

  // Any use of the worker client is a vendor release/refresh/admission call: there must be none.
  const clientCalls: string[] = [];
  const client = new Proxy({}, { get: (_target, prop) => { clientCalls.push(String(prop)); return undefined; } }) as unknown as WorkerClient;

  const configs: Record<string, unknown>[] = [];
  const methods: { method?: string; params?: Record<string, unknown> }[] = [];
  const specs: { authMode?: string; providerBinding?: unknown; model?: string }[] = [];
  let launches = 0, removals = 0, disposals = 0;
  const streams: PassThrough[] = [];
  t.after(() => { for (const stream of streams) stream.destroy(); });
  const launchCodexRootForTest: typeof launchCodexRoot = async (spec, launcherDeps) => {
    launches++;
    specs.push({ authMode: spec.authMode, providerBinding: spec.providerBinding, model: spec.model });
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
        assert.equal(JSON.stringify(options.env).includes(credential), false, "the key never rides the launcher env");
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
  };

  const home = path.join(parent, "home");
  await fs.mkdir(home);
  const { logger: log, lines } = recordingLogger();
  // The factory both production runners are given (main.ts). Only the root launcher's OS-bound seams are
  // swapped (adviceRootTestSeams); no launchRoot is passed, so the factory builds its own root and derives
  // the auth mode from the binding. main.ts also passes codexExtraCa; this row omits it.
  const factory = makeProductionCodexAdviceHarnessFactory(client, log, home, {
    adviceRootTestSeams: { launchCodexRootForTest, sharedDirOwner: { uid: process.getuid!(), gid: process.getgid!() } },
  });
  const refuseClaude = (): never => { throw new Error("the Claude query path must not run on a Codex provider pass"); };

  // (a) The advice pass: the exact option shape review-runner and judge-runner hand runReadOnlyModelPass.
  const advice = (prompt: string) => runReadOnlyModelPass({
    model: "gpt-6-sol", systemPrompt: "Review the change.", prompt, homeRoot: home, homePrefix: "uzi-review-",
    label: "review", timeoutMs: 60_000, queryFn: refuseClaude as never, denyReason: "read-only", log,
    codex: { runId: "advice-summary-run", claimGeneration: 1, binding, buildHarness: factory },
  });
  nextText = echoed;
  const adviceEcho = await advice("review the diff A");
  assert.match(adviceEcho, /^head .* tail$/, "the pass output keeps its surrounding text");
  for (const form of [credential, escaped, percent]) assert.equal(adviceEcho.includes(form), false, "advice output carries no key form");
  nextText = "head clean advice tail";
  assert.equal(await advice("review the diff B"), "head clean advice tail", "a clean control is returned unchanged");
  assert.equal(requests.length, 2);

  // (b) The summary pass: SummaryRunner's Codex branch with the same production factory.
  const summaries = new SummaryRunner(log, { codexAdviceHarnessFactory: factory, modelTimeoutMs: 60_000, homeRoot: home });
  const planInput = { claim: { run_id: "advice-summary-run", claim_generation: 1, secrets }, token: "", model: "gpt-6-luna",
    issueTitle: "Fixture issue", issueBody: "Fixture body", prdText: null, planMd: "1. Do the thing" };
  nextText = JSON.stringify({ summary: echoed, deltas: [{ kind: "added", text: echoed }] });
  const planEcho = await summaries.generatePlanSummary(planInput);
  assert.ok(planEcho, "the summary pass produced a plan summary");
  const planText = JSON.stringify(planEcho);
  for (const form of [credential, escaped, percent, JSON.stringify(escaped).slice(1, -1)]) {
    assert.equal(planText.includes(form), false, "summary output carries no key form");
  }
  assert.match(planEcho.summary, /^head .* tail$/);
  nextText = JSON.stringify({ summary: "head clean summary tail", deltas: [] });
  assert.deepEqual(await summaries.generatePlanSummary(planInput), { summary: "head clean summary tail", deltas: [] },
    "a clean control is returned unchanged");
  assert.equal(requests.length, 4);

  // Every model request reached the fake with the bound key only, for the mapped gateway model.
  assert.deepEqual(requests.map(r => r.model),
    ["gateway/sol-model", "gateway/sol-model", "gateway/luna-model", "gateway/luna-model"]);
  assert.equal(receipts.length, 4, "no discovery or extra receiver traffic");
  for (const receipt of receipts) {
    assert.equal(receipt.method, "POST");
    assert.equal(receipt.url, "/v1/responses");
    assert.equal(receipt.headers.authorization, `Bearer ${credential}`);
    for (const name of ["x-api-key", "chatgpt-account-id", "openai-organization", "openai-project", "cookie"]) {
      assert.equal(receipt.headers[name], undefined, `no other credential header (${name})`);
    }
  }

  // The generated config: mapped model and the slot's model_context_window, on the root and per thread.
  assert.equal(launches, 4);
  assert.equal(configs.length, 4);
  assert.deepEqual(configs.map(c => [c.model, c.model_context_window]), [
    ["gateway/sol-model", 32768], ["gateway/sol-model", 32768],
    ["gateway/luna-model", 16384], ["gateway/luna-model", 16384],
  ]);
  assert.equal(specs.length, 4);
  for (const spec of specs) {
    // The auth mode here is the factory's own derivation from the binding (binding.authMode).
    assert.equal(spec.authMode, "provider", "advice roots launch under the provider auth mode, never a vendor one");
    assert.deepEqual(spec.providerBinding, (binding as { providerBinding: unknown }).providerBinding);
  }
  const threads = methods.filter(m => m.method === "thread/start");
  assert.equal(threads.length, 4);
  assert.deepEqual(threads.map(m => [m.params?.model, (m.params?.config as Record<string, unknown>).model_context_window]), [
    ["gateway/sol-model", 32768], ["gateway/sol-model", 32768],
    ["gateway/luna-model", 16384], ["gateway/luna-model", 16384],
  ]);
  const logins = methods.filter(m => m.method === "account/login/start");
  assert.equal(logins.length, 4, "one app-server login per pass");
  for (const login of logins) assert.deepEqual(login.params, { type: "apiKey", apiKey: credential });

  // Zero vendor operations: no client member was ever read, so no release/refresh/admission call.
  assert.deepEqual(clientCalls, []);
  // Root lifecycle: every pass disposed its own root cleanly, and no log line carries any key form.
  assert.equal(disposals, 4);
  assert.equal(removals, 4);
  const logText = JSON.stringify(lines);
  for (const form of [credential, escaped, percent]) assert.equal(logText.includes(form), false, "no log line carries a key form");
  recordEvidence(PROVIDER_ADVICE_SUMMARY_TITLE, "pass");
});
