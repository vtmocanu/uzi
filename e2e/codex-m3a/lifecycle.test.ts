// PRD #1156 M3a — Control A: real code-mode-host lifecycle, credential-free, NO
// hook-trust bypass.
//
// Reproduces the BEHAVIOUR of the frozen `e2e/codex-m0/supervisor.test.mjs` `running()`
// (model, code-mode config, m0_hold/m0_mark dynamic tools, the custom `exec` cell body,
// the delayed-marker positive control, and revoke/settle/interrupt/dispose reaping BOTH
// the app-server and the differently-grouped code-mode host) with the PRODUCTION Go
// supervisor + real Codex + a localhost fake Responses provider — WITHOUT importing that
// fixture's `Probe`/`SupervisorProbe` and WITHOUT `bypass_hook_trust`/live credentials.
//
// The m3a spike proved the real `codex-code-mode-host` launches and runs a code-mode cell
// under this isolated config with no hook-trust bypass and no live creds; these tests are
// its permanent regression coverage across both intended models, active AND yielded.

import assert from "node:assert/strict";
import { appendFile, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { message, callOutput, poll } from "../codex-m0/harness.mjs";
import { FakeProvider, dummyCredential, type ResponseItem, type ResponsesBody } from "./fake-provider.js";
import {
  SupervisorRoot,
  codeModeHostConfigToml,
  type DynamicReply,
  type SnapshotEvidence,
} from "./supervisor-driver.js";

const SUPERVISOR_BIN = process.env.M3A_SUPERVISOR_BIN ?? "/usr/local/bin/uzi-codex-supervisor";
const CODEX_BIN = process.env.M3A_CODEX_BIN ?? "/opt/uzi-codex/0.153.2/bin/codex";
const DATA_ROOT = process.env.M3A_DATA_ROOT ?? "/data/runner/m3a";
const RUNNER_UID = Number(process.env.M3A_RUNNER_UID ?? "10002");
const ENV_KEY = "FAKE_PROVIDER_API_KEY";
const delayMs = 500;
const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

const success = (text: string): DynamicReply => ({ result: { success: true, contentItems: [{ type: "inputText", text }] } });
const failure = (text: string): DynamicReply => ({ result: { success: false, contentItems: [{ type: "inputText", text }] } });
const dynamicTools = ["hold", "mark"].map((name) => ({
  name: `m0_${name}`, description: `m3a harmless ${name} fixture`,
  inputSchema: { type: "object", properties: {}, additionalProperties: false },
}));

/** The replacement env allowlist for a provider root (mirrors the launcher's own). */
function replacedEnv(root: string, credential: string): Record<string, string> {
  return {
    HOME: `${root}/home`, CODEX_HOME: `${root}/codex`,
    XDG_CONFIG_HOME: `${root}/xdg-config`, XDG_CACHE_HOME: `${root}/xdg-cache`,
    XDG_DATA_HOME: `${root}/xdg-data`, XDG_STATE_HOME: `${root}/xdg-state`,
    TMPDIR: `${root}/tmp`,
    PATH: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin",
    SHELL: "/bin/sh", LANG: "C", TERM: "dumb",
    [ENV_KEY]: credential,
  };
}

interface Running {
  readonly root: SupervisorRoot;
  readonly provider: FakeProvider;
  readonly app: { pid: number; pgid: number };
  readonly host: { pid: number; pgid: number };
  readonly rootId: string;
  readonly turnId: string;
  readonly markerPath: string;
  admitted: boolean;
  markCalls(): number;
  releaseCallback(): void;
  releaseResponse(): void;
  callbackSettled: Promise<void>;
}

async function markerContent(markerPath: string): Promise<string | null> {
  try { return await readFile(markerPath, "utf8"); }
  catch (error) { if ((error as NodeJS.ErrnoException).code === "ENOENT") return null; throw error; }
}

/** Launch a real code-mode-host root running one cell that awaits m0_hold, sleeps, then
 *  m0_mark. Returns once the cell is inside the held callback and the host is confirmed. */
async function running(t: test.TestContext, model: string, yielded: boolean): Promise<Running> {
  const credential = dummyCredential();
  const rootDir = `${DATA_ROOT}/A-${process.pid}-${Math.random().toString(16).slice(2, 8)}`;
  const cwd = `${rootDir}/cwd`;
  const markerDir = await mkdtemp(path.join(tmpdir(), "m3a-marker-"));
  const markerPath = path.join(markerDir, "dynamic-marker");

  let holdStarted = false;
  let settle!: () => void;
  const callbackSettled = new Promise<void>((resolve) => { settle = resolve; });
  const gate = Promise.withResolvers<void>();
  const responseGate = Promise.withResolvers<void>();
  const st = { admitted: true, markCalls: 0 };

  const provider = await FakeProvider.start({
    credential,
    respond: async (body: ResponsesBody): Promise<ResponseItem[]> => {
      assert.equal(body.model, model);
      if (callOutput(body, "cell")) {
        if (yielded) await responseGate.promise;
        return [message() as ResponseItem];
      }
      return [{
        type: "custom_tool_call", name: "exec", call_id: "cell",
        input: `${yielded ? '// @exec: {"yield_time_ms": 1}\n' : ""}await tools.m0_hold({});\n`
          + `await new Promise((resolve) => setTimeout(resolve, ${delayMs}));\ntext(await tools.m0_mark({}));`,
      }];
    },
  });

  const sup = await SupervisorRoot.start({
    supervisorBin: SUPERVISOR_BIN, codexBin: CODEX_BIN, childArgv: ["app-server"],
    ownedDataRoot: rootDir, cwd, expectUid: RUNNER_UID,
    configToml: codeModeHostConfigToml({ model, baseUrl: provider.baseUrl, envKey: ENV_KEY, projectPath: cwd }),
    env: replacedEnv(rootDir, credential),
    dynamicTool: async (request): Promise<DynamicReply> => {
      if (request.params.tool === "m0_hold") {
        holdStarted = true;
        await gate.promise;
        // Settlement is a worker-owned completion; revoke wins before release.
        const reply = st.admitted ? success("m3a hold released") : failure("m3a revoked");
        settle();
        return reply;
      }
      assert.equal(request.params.tool, "m0_mark");
      if (!st.admitted) return failure("m3a revoked");
      st.markCalls += 1;
      await appendFile(markerPath, "m3a delayed dynamic marker\n");
      return success("m3a marker recorded");
    },
  });

  t.after(async () => {
    await sup.close();
    await provider.close();
    await rm(markerDir, { recursive: true, force: true });
    assert.deepEqual(sup.errors, [], "driver transport/handler errors");
    assert.deepEqual(provider.errors, [], "provider errors");
  });

  await sup.initialize();
  const rootId = await sup.threadStart({ model, environments: [], dynamicTools, approvalPolicy: "never" });
  const turnId = await sup.turnStart(rootId);
  // Time-based poll (not the change-event `until`): `holdStarted` flips inside the async
  // dynamic-tool callback with no further RPC traffic in the active case, so a purely
  // event-driven wait would miss it.
  await poll(() => holdStarted, "actual cell entered worker callback");

  assert.equal(sup.started.subreaper, true, "supervisor established subreaper before fork");
  const snap: SnapshotEvidence = await sup.snapshot();
  const app = snap.processes.find((p) => p.pid === sup.started.childPid);
  const host = snap.processes.find((p) => p.comm.includes("code-mode"));
  assert.ok(app, `real app-server exists below supervisor (${JSON.stringify(snap.processes)})`);
  assert.ok(host, `real remote code-mode host exists below supervisor (${JSON.stringify(snap.processes)})`);
  assert.notEqual(app.pgid, host.pgid, "code host escapes the app-server process group");
  t.diagnostic(JSON.stringify({ event: "actual ownership", model, yielded, started: sup.started, app, host }));

  return {
    root: sup, provider, app, host, rootId, turnId, markerPath,
    get admitted() { return st.admitted; },
    set admitted(v: boolean) { st.admitted = v; },
    markCalls: () => st.markCalls,
    releaseCallback: () => gate.resolve(),
    releaseResponse: () => responseGate.resolve(),
    callbackSettled,
  };
}

for (const model of ["gpt-6-astra", "gpt-5.6-sol"]) {
  for (const yielded of [false, true]) {
    const label = yielded ? "yielded" : "active";

    test(`${model} ${label}: real code-mode host + uninterrupted delayed dynamic positive control`, async (t) => {
      const r = await running(t, model, yielded);
      r.releaseCallback();
      const marker = await poll(async () => (await markerContent(r.markerPath)) !== null, "positive delayed dynamic marker");
      void marker;
      assert.equal(await markerContent(r.markerPath), "m3a delayed dynamic marker\n");
      assert.equal(r.markCalls(), 1);
      r.releaseResponse();
      const disp = await r.root.dispose();
      assert.equal(disp.state, "drained");
      assert.equal(disp.authority, "ECHILD+__WALL");
      assert.ok(disp.reaped?.includes(r.app.pid), "app-server reaped by own supervisor");
      assert.ok(disp.reaped?.includes(r.host.pid), "code-mode host reaped by own supervisor");
      t.diagnostic(JSON.stringify({ positiveMarker: true, disposal: disp }));
    });

    test(`${model} ${label}: revoke+settle+interrupt+dispose reaps app+host and suppresses late marker`, async (t) => {
      const r = await running(t, model, yielded);
      r.admitted = false;
      r.releaseCallback();
      await r.callbackSettled;
      await r.root.turnInterrupt(r.rootId, r.turnId);
      const disp = await r.root.dispose();
      assert.equal(disp.state, "drained");
      assert.equal(disp.authority, "ECHILD+__WALL");
      assert.ok(disp.reaped?.includes(r.app.pid), "app-server reaped by own supervisor");
      assert.ok(disp.reaped?.includes(r.host.pid), "escaped host adopted and reaped by own supervisor");
      await sleep(delayMs + 200);
      assert.equal(await markerContent(r.markerPath), null, "late marker absent beyond the positive-control delay");
      assert.equal(r.markCalls(), 0, "late dynamic callback itself absent, not merely rejected by revoked admission");
      t.diagnostic(JSON.stringify({ markerAbsentAfterMs: delayMs + 200, lateCalls: 0, disposal: disp }));
    });
  }
}
