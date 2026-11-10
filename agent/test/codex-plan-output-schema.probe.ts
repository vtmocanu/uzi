// Manual, synthetic-only capture. Importing this module never launches a child.
// Main owns execution: from agent/, node --import tsx test/codex-plan-output-schema.probe.ts capture [out path]
import assert from "node:assert/strict";
import { spawn, spawnSync, type ChildProcessWithoutNullStreams } from "node:child_process";
import { createServer } from "node:http";
import { access, mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { constants } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { PassThrough } from "node:stream";
import { CODEX_BIN, type CodexRootHandle } from "../src/codex/launcher.js";
import { buildCodexLoopbackTestConfigToml, CODEX_M3B_LOOPBACK_PROVIDER_NAME, assertNoUnexpectedSystemConfig } from "../src/codex/config.js";
import { PLAN_VERDICT_SCHEMA } from "../src/codex/cross-check.js";
import { ANCHOR, VERDICT, CAPTURE_LIMIT, frameSink, runChecker, assertFixture, type Event, type SchemaFixture } from "./codex-plan-output-schema-helper.js";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const SCRATCH = path.join(ROOT, ".uzi/scratch");
// CODEX_BIN is the fixed, versioned launcher path, updated by codex-pins:sync.
const PROBE_VERSION = path.basename(path.dirname(path.dirname(CODEX_BIN)));
const DEFAULT_OUT = path.join(ROOT, `agent/test/fixtures/codex-plan-output-schema-${PROBE_VERSION}.json`);
const TIMEOUT = 60_000;

/** Direct roots intentionally omit supervisor/identity isolation, per the M1 owner exception.
 * Each child has an owned process group, including its code-mode host. Disposal has two
 * signal attempts and a five-second bound; one root failure does not block sibling disposal.
 */
function directRoot(child: ChildProcessWithoutNullStreams): CodexRootHandle {
 let closing = false;
 let exited = false;
 let disposal: Promise<{ clean: boolean }> | undefined;
 let failure: Error | undefined;
 let fail!: (e: Error) => void;
 const whenFailed = new Promise<Error>(resolve => { fail = resolve; });
 const exit = new Promise<void>(resolve => {
  child.once("exit", () => { exited = true; if (!closing) { failure = new Error("probe child exited early"); fail(failure); } resolve(); });
  child.once("error", error => { failure = error; fail(error); resolve(); });
 });
 child.stderr.resume(); // Never retain stderr, auth material or diagnostics.
 const signalGroup = (signal: NodeJS.Signals): void => {
  if (!child.pid) return;
  try { process.kill(-child.pid, signal); } catch (error) {
   if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error;
  }
 };
 return {
  transport: child, whenFailed, get failed() { return failure; },
  dispose: () => disposal ??= (async () => {
   closing = true;
   signalGroup("SIGTERM");
   let timer: ReturnType<typeof setTimeout> | undefined;
   await Promise.race([exit, new Promise<void>(resolve => { timer = setTimeout(resolve, 1500); })]);
   clearTimeout(timer);
   // Signal the owned group even if the primary exited: code-mode host is a sibling.
   signalGroup("SIGKILL");
   await Promise.race([exit, new Promise<void>(resolve => { timer = setTimeout(resolve, 1500); })]);
   clearTimeout(timer);
   // At most 20 checks over one second; only this launch-owned group is probed.
   let groupGone = child.pid === undefined;
   for (let attempt = 0; !groupGone && attempt < 20; attempt++) {
    try { process.kill(-child.pid!, 0); } catch (error) {
     if ((error as NodeJS.ErrnoException).code === "ESRCH") groupGone = true;
     else throw error;
    }
    if (!groupGone) await new Promise<void>(resolve => setTimeout(resolve, 50));
   }
   return { clean: exited && groupGone };
  })(),
 } as unknown as CodexRootHandle;
}

function sse(items: Record<string, unknown>[], index: number): string {
 const frames = [
  { type: "response.created", response: { id: "probe-response-" + index } },
  ...items.map(item => ({ type: "response.output_item.done", item })),
  { type: "response.completed", response: { id: "probe-response-" + index,
   usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0, input_tokens_details: null, output_tokens_details: null } } },
 ];
 return frames.map(f => "event: " + f.type + "\ndata: " + JSON.stringify(f) + "\n\n").join("");
}

/** Returns a fixture only after the real checker and provider-visible Read proof succeed. */
export async function captureSchemaFixture(out = DEFAULT_OUT,
 /** Test-only setup failure injection; never supplied by model or capture CLI input. */
 beforeSetupForTest?: (base: string) => Promise<void>): Promise<void> {
 await mkdir(SCRATCH, { recursive: true });
 const base = await mkdtemp(path.join(SCRATCH, "plan-schema-"));
 const checkout = path.join(base, "checkout");
 const home = path.join(base, "home");
 const codexHome = path.join(home, ".codex");
 const tmp = path.join(base, "tmp");
 const credential = "synthetic-schema-provider";
 const events: Event[] = [];
 const requests: Record<string, any>[] = [];
 const errors: string[] = [];
 const roots: CodexRootHandle[] = [];
 let retained = 0;
 let active = false;
 let terminal = false;
 const retain = (event: Event): void => {
  retained += Buffer.byteLength(JSON.stringify(event));
  assert.ok(retained <= CAPTURE_LIMIT && events.length < 10000, "capture cap");
  events.push(event);
 };
 const server = createServer((req, res) => {
  if (req.method !== "POST" || req.url !== "/v1/responses" || req.headers.authorization !== "Bearer " + credential) {
   req.resume(); res.writeHead(400).end(); return;
  }
  let raw = "";
  req.setEncoding("utf8");
  req.on("data", (chunk: string) => {
   raw += chunk;
   if (Buffer.byteLength(raw) > CAPTURE_LIMIT) req.destroy(new Error("provider body cap"));
  });
  req.on("error", () => { errors.push("provider request interrupted"); });
  req.on("end", () => {
   try {
    assert.ok(requests.length < 2, "exactly two Responses requests");
    const body = JSON.parse(raw) as Record<string, any>;
    retained += Buffer.byteLength(raw);
    assert.ok(retained <= CAPTURE_LIMIT, "retained bytes cap");
    requests.push(body);
    let items: Record<string, unknown>[];
    if (requests.length === 1) {
     // Literal inert script; never execute screened or user-provided command text.
     items = [{ type: "custom_tool_call", call_id: "probe-read", name: "exec",
      input: 'text(await tools.uzi_read({path:"anchor.ts"}));' }];
    } else {
     const outputs = (body.input as Record<string, unknown>[]).filter(i => i.type === "custom_tool_call_output" && i.call_id === "probe-read");
     assert.ok(outputs.some(i => JSON.stringify(i.output).includes(ANCHOR.trim())), "provider must see actual Read result");
     items = [{ type: "message", role: "assistant", id: "probe-final", phase: "final_answer",
      content: [{ type: "output_text", text: VERDICT }] }];
    }
    res.writeHead(200, { "content-type": "text/event-stream" }).end(sse(items, requests.length));
   } catch (error) { errors.push(String(error)); res.writeHead(500).end(); }
  });
 });
 server.requestTimeout = TIMEOUT;
 server.headersTimeout = TIMEOUT;
 try {
  await beforeSetupForTest?.(base);
  // Sequential setup leaves no pending filesystem work racing failure cleanup.
  await mkdir(checkout);
  await mkdir(codexHome, { recursive: true });
  await mkdir(tmp);
  await writeFile(path.join(checkout, "anchor.ts"), ANCHOR);
  assertNoUnexpectedSystemConfig();
  const version = spawnSync(CODEX_BIN, ["--version"], { encoding: "utf8", timeout: 5000, maxBuffer: 4096 });
  assert.equal(version.status, 0);
  assert.equal(version.stdout.trim(), `codex-cli ${PROBE_VERSION}`);
  await access(path.join(path.dirname(CODEX_BIN), "codex-code-mode-host"), constants.X_OK);
  await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  const baseUrl = "http://127.0.0.1:" + address.port + "/v1";
  await writeFile(path.join(codexHome, "config.toml"), buildCodexLoopbackTestConfigToml({
   model: "gpt-6-astra", projectPath: checkout, authMode: "api_key", codeModeHost: true,
  }, baseUrl));
  const env = { HOME: home, CODEX_HOME: codexHome, TMPDIR: tmp, PATH: "/usr/local/bin:/usr/bin:/bin", LANG: "C.UTF-8" };
  const result = await runChecker({
   prepareHome: async () => {},
   launchFileop: async () => {
    const root = directRoot(spawn("/usr/local/bin/uzi-codex-fileop", ["--root", checkout], { cwd: checkout, env, detached: true, stdio: "pipe" }));
    roots.push(root); return root;
   },
   launchProvider: async () => {
    const child = spawn(CODEX_BIN, ["app-server"], { cwd: checkout, env, detached: true, stdio: "pipe" });
    const root = directRoot(child); roots.push(root);
    const inbound = new PassThrough();
    const authIds = new Set<number | string>();
    const callbackIds = new Set<number | string>();
    const recordedMethods = new Set(["thread/started", "thread/settings/updated", "thread/status/changed",
     "turn/started", "item/started", "item/completed", "item/agentMessage/delta", "item/tool/call",
     "thread/tokenUsage/updated", "turn/completed", "error"]);
    const sink = frameSink(frame => {
     if (frame.method === "account/login/start" || frame.method === "initialize") authIds.add(frame.id!);
     if (frame.method === "thread/start") active = true;
     const adapted = structuredClone(frame);
     if (frame.method === "thread/start") adapted.params!.modelProvider = CODEX_M3B_LOOPBACK_PROVIDER_NAME;
     if (frame.method === "turn/start") adapted.params!.outputSchema = PLAN_VERDICT_SCHEMA;
     if (active && (["thread/start", "turn/start"].includes(frame.method ?? "")
      || (!frame.method && frame.id !== undefined && callbackIds.has(frame.id)))) retain({ direction: "out", frame,
      ...(["thread/start", "turn/start"].includes(frame.method ?? "") ? { adapted } : {}) });
     child.stdin.write(JSON.stringify(adapted) + "\n");
    });
    sink.on("error", error => inbound.destroy(error));
    const observer = frameSink(frame => {
     // Auth RPCs/notifications never enter the retained tape.
     if (active && !terminal && !authIds.has(frame.id!)
      && (frame.method ? recordedMethods.has(frame.method) : frame.id !== undefined)) {
      if (frame.method === "item/tool/call" && frame.id !== undefined) callbackIds.add(frame.id);
      retain({ direction: "in", frame });
      if (frame.method === "turn/completed") terminal = true;
     }
     inbound.write(JSON.stringify(frame) + "\n");
    });
    observer.on("error", error => inbound.destroy(error));
    child.stdout.pipe(observer);
    child.stdout.on("end", () => inbound.end());
    return { ...root, transport: { stdin: sink, stdout: inbound, stderr: null } };
   },
  }, checkout, home, credential, AbortSignal.timeout(TIMEOUT));
  assert.equal(result.text, VERDICT); assert.equal(result.cleanup, true); assert.deepEqual(errors, []);
  // Preserve identities/topology with stable placeholders, including inside nested text.
  const identities = new Map<string, string>();
  const normalize = (text: string): string => text
   .replaceAll(base, "/fixture")
   .replaceAll(baseUrl, "http://127.0.0.1:12345/v1")
   .replace(/[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}|(?<![a-f0-9])[a-f0-9]{32}(?![a-f0-9])/g, value => {
    if (!identities.has(value)) identities.set(value, "identity-" + (identities.size + 1));
    return identities.get(value)!;
   });
  const fixture: SchemaFixture = JSON.parse(normalize(JSON.stringify({
   version: 1,
   provenance: {
    codexVersion: version.stdout.trim(), capturedAt: new Date().toISOString(),
    command: `cd agent && node --import tsx test/codex-plan-output-schema.probe.ts capture test/fixtures/codex-plan-output-schema-${PROBE_VERSION}.json`,
    substitutions: ["owned scratch root -> /fixture", "loopback endpoint -> 127.0.0.1:12345", "UUID and fence nonce -> stable identity-N"],
    limitations: ["Scripted Responses provider; no live-provider acceptance", "Direct spawn under current identity; no OS identity or confinement proof",
     "Auth RPC traffic and stderr excluded; real auth handshake executed",
     "Allowlisted checker frames only; provider projection retains model, text.format, tool_choice and custom tool outputs, omitting stock instructions/tools", "Probe adapter only: thread/start modelProvider and turn/start outputSchema; production wiring is M2"],
   }, events, providerRequests: requests.map(request => ({ model: request.model, text: request.text,
    tool_choice: request.tool_choice, input: (request.input as Record<string, unknown>[])
     .filter(item => item.type === "custom_tool_call_output") })), verdict: result.text,
  })));
  assertFixture(fixture, PROBE_VERSION);
  assert.ok(!JSON.stringify(fixture).includes(credential), "no dummy credential in fixture");
  await writeFile(path.resolve(out), JSON.stringify(fixture, null, 2) + "\n", { flag: "wx" });
 } finally {
  // Idempotent cleanup for setup failures too. Try every owned root even if a sibling fails.
  const cleanup = await Promise.allSettled(roots.map(root => root.dispose(5000)));
  server.closeAllConnections();
  await new Promise<void>(resolve => server.close(() => resolve()));
  const failed = cleanup.some(r => r.status === "rejected" || !r.value.clean);
  if (!failed) await rm(base, { recursive: true, force: true });
  assert.equal(failed, false, "owned process cleanup must be confirmed");
 }
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
 assert.equal(process.argv[2], "capture", "usage: probe.ts capture [out path]");
 await captureSchemaFixture(process.argv[3]);
}
