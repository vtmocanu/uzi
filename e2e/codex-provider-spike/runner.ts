import { codeModeExecStep, nestedWorkerCallScript, scriptedStepsResponder } from "../codex-m4/fake-provider.js";
import { randomBytes } from "node:crypto";
import { readFileSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { request, type IncomingMessage } from "node:http";
import { FakeProvider, Budget, SAFE_HEADER_NAMES, type ServerOptions } from "./fake-provider.js";
import { runProtocolTurn, loadProtocolModules, type ProtocolTurnOptions } from "./driver.js";
import { probeCodexRuntime } from "../../agent/src/codex/codex-runtime-probe.js";
import { parseLock, resolveArch } from "../codex-m4/provision.js";

export const names = ["baseline", "wrong-key", "guard-removed", "env-key", "env-key-control", "alternate-header", "unsupported-wire", "responses-control",
  "receiver-control", "307-same", "308-same", "307-port", "307-port-stall", "308-port", "307-host", "308-host",
  "tls-no-ca", "tls-ssl", "tls-codex", "tls-combined", "tls-untrusted", "tls-stall", "callback", "callback-disabled", "tool-grant-denied", "phase-denied", "advice", "cancel-stream", "cancel-control", "resume", "restart-rebind", "restart-closed-a", "rebind-observer-control", "same-process-override", "unknown-model", "destination-control"] as const;
export type Name = typeof names[number];
export function validateName(name: string): asserts name is Name {
  if (!(names as readonly string[]).includes(name)) throw new Error("invalid-case");
}
export type Row = { name: Name; outcome: "observed-supported" | "observed-failure" | "unverified"; path: "production-launcher" | "fixture-config" | "fixture-control";
  streamed: boolean; credentialReceived: boolean; responsePath: boolean; header: boolean; body: boolean; caSSL: boolean; caCodex: boolean; credentialEnv: boolean;
  config: Name; requestPaths: string[]; headerNames: string[]; elapsedMs: number; actualSSL: boolean; actualCodex: boolean; actualCredentialEnv: boolean; keyInConfig: boolean;
  callbackAttempted?: boolean; receiverControlHeader?: boolean; receiverControlBody?: boolean; observerControlPassed?: boolean; callbackSeen?: boolean; effect?: boolean; denied?: boolean; roundtrip?: boolean; deadlineObserved?: boolean; nativeSuppressed?: boolean; nativeApplyPatch?: boolean; unknownInventory?: boolean; modelContextWindowOwn?: boolean; modelContextWindowNullableInteger?: boolean; fallbackWarningObserved?: boolean; untrusted?: boolean; nativeConfigDisabled?: boolean; poisonStripped?: boolean; auxiliaryConfigDisabled?: boolean; destinationMatched?: boolean; zeroRequestRetries?: boolean; zeroStreamRetries?: boolean; websocketsDisabled?: boolean; cancelled?: boolean; resumed?: boolean;
  safetyOutcome?: "observed-supported" | "observed-failure"; cleanupFailed?: boolean;
  held: boolean; reason: "completed" | "rejected" | "forwarded" | "config-or-runtime-failure" | "deadline" | "cross-host-bind" | "config-rejected" | "tls-refused" | "protocol-failure" | "harness-failure" | "native-authority-advertised" | "auxiliary-network-unverified" };
export class Evidence {
  readonly budget = new Budget(1024 * 1024, 64);
  readonly rows: Row[] = [];
  add(row: Row): void {
    validateName(row.name);
    if (!names.includes(row.config) || !["observed-supported", "observed-failure", "unverified"].includes(row.outcome)
      || !["production-launcher", "fixture-config", "fixture-control"].includes(row.path)
      || !["completed", "rejected", "forwarded", "config-or-runtime-failure", "deadline", "cross-host-bind", "config-rejected", "tls-refused", "protocol-failure", "harness-failure", "native-authority-advertised", "auxiliary-network-unverified"].includes(row.reason)
      || !Number.isSafeInteger(row.elapsedMs) || row.elapsedMs < 0
      || row.requestPaths.some(p => !["/v1/responses", "/v1/models", "/target"].includes(p))
      || row.headerNames.length > 64 || row.headerNames.some(h => !SAFE_HEADER_NAMES.has(h)))
      throw new Error("invalid-evidence");
    for (const [key, value] of Object.entries(row)) {
      if (!["name", "config", "outcome", "safetyOutcome", "path", "reason", "requestPaths", "headerNames", "elapsedMs"].includes(key) && typeof value !== "boolean")
        throw new Error("invalid-evidence");
    }
    if (row.safetyOutcome !== undefined && !["observed-supported", "observed-failure"].includes(row.safetyOutcome)) throw new Error("invalid-evidence");
    const bytes = Buffer.byteLength(JSON.stringify(row));
    if (bytes > 65536) throw new Error("evidence-limit");
    this.budget.take(bytes); this.rows.push(row);
  }
}
async function main(): Promise<void> {
  const globalAbort = new AbortController();
  const owned = new Set<FakeProvider>();
  const globalTimer = setTimeout(() => {
    globalAbort.abort(new Error("deadline"));
    for (const server of owned) void server.close();
  }, 300000);
  try {
  const lockText = readFileSync(path.resolve(__dirname, "../../agent/codex/codex-package.lock"), "utf8");
  const lock = parseLock(lockText);
  const probe = await probeCodexRuntime({ prefix: "/opt/uzi-codex", expectedVersion: lock.version,
    expectedLockDigest: lock.sha256[resolveArch(process.arch)], env: {} });
  if (!probe.capable) throw new Error("prerequisite");
  const mods = await loadProtocolModules();
  const scratch = path.resolve(__dirname, "../../.uzi/scratch");
  mkdirSync(scratch, { recursive: true });
  const base = mkdtempSync(path.join(scratch, "provider-spike-"));
  const evidence = new Evidence();
  const credential = randomBytes(24).toString("hex");
  const wrong = randomBytes(24).toString("hex");
  let broken = false;
  try {
    // Literal executable and fixed arguments only; generated keys remain in scratch.
    const openssl = (args: string[]) => {
      const result = runOpenSSL(args, base);
      if (result.status !== 0) throw new Error("tls-fixture");
    };
    for (const name of ["trusted", "untrusted"]) {
      openssl(["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", name + ".key", "-out", name + ".crt", "-days", "1", "-subj", "/CN=spike-ca"]);
    }
    openssl(["req", "-newkey", "rsa:2048", "-nodes", "-keyout", "server.key", "-out", "server.csr", "-subj", "/CN=127.0.0.1",
      "-addext", "subjectAltName=IP:127.0.0.1"]);
    openssl(["x509", "-req", "-in", "server.csr", "-CA", "trusted.crt", "-CAkey", "trusted.key", "-CAcreateserial",
      "-out", "server.crt", "-days", "1", "-copy_extensions", "copy"]);
    const tls = { key: readFileSync(path.join(base, "server.key")), cert: readFileSync(path.join(base, "server.crt")) };
    const selected = process.argv[2];
    if (selected !== undefined) validateName(selected);
    for (const name of selected ? [selected] : names) {
      globalAbort.signal.throwIfAborted();
      const caseStarted = Date.now();
      const caseAbort = new AbortController();
      const abortCase = () => caseAbort.abort(new Error("deadline"));
      globalAbort.signal.addEventListener("abort", abortCase, { once: true });
      const caseTimer = setTimeout(() => {
        abortCase();
        for (const server of owned) void server.close();
      }, 30000);
      let receiver: FakeProvider | undefined;
      let provider: FakeProvider | undefined;
      let measuredProvider: FakeProvider | undefined;
      const resumeProbe = { started: false };
      const fixture = ["env-key", "env-key-control", "alternate-header", "unsupported-wire", "responses-control"].includes(name) || name.startsWith("tls-");
      const row: Row = { name, outcome: "observed-failure", path: ["receiver-control", "destination-control"].includes(name) ? "fixture-control" : fixture || name === "same-process-override" ? "fixture-config" : "production-launcher",
        streamed: false, credentialReceived: false, responsePath: false, header: false, body: false, caSSL: false, caCodex: false, credentialEnv: false,
        config: name, requestPaths: [], headerNames: [], elapsedMs: 0, actualSSL: false, actualCodex: false, actualCredentialEnv: false, keyInConfig: name === "alternate-header",
        held: ["env-key", "env-key-control", "alternate-header"].includes(name), reason: "rejected" };
      let diagnosis: "config-rejected" | "tls-refused" | undefined;
      try {
        const m2 = ["callback", "callback-disabled", "tool-grant-denied", "phase-denied", "advice", "cancel-stream", "cancel-control", "resume", "restart-rebind", "restart-closed-a", "rebind-observer-control", "same-process-override"].includes(name);
        let effects = 0;
        const options: ServerOptions = { credential, guard: name !== "guard-removed", alternate: name === "alternate-header" };
        if (m2 && !["cancel-stream", "cancel-control", "resume", "restart-rebind", "restart-closed-a", "rebind-observer-control", "same-process-override"].includes(name)) {
          options.respond = scriptedStepsResponder([
            codeModeExecStep("spike-exec", nestedWorkerCallScript(name === "phase-denied" ? "uzi_apply_patch" : "uzi_bash",
              name === "phase-denied" ? { file_path: "synthetic", content: "synthetic" } : { command: "echo synthetic" })),
            { kind: "finish" }]);
        }
        if (name === "cancel-stream") {
          options.holdStream = true;
          options.onStream = () => caseAbort.abort(new Error("stream-cancel"));
        }
        if (name.startsWith("tls-")) { options.tls = tls; options.stallConnect = name === "tls-stall"; }
        if (name.endsWith("-same")) options.sameOrigin = name.startsWith("307") ? 307 : 308;
        if (name.endsWith("-port") || name === "307-port-stall" || name.endsWith("-host") || name === "receiver-control") {
          try { receiver = await FakeProvider.start({ credential, stall: name === "307-port-stall", host: name.endsWith("-host") ? "127.0.0.2" : "127.0.0.1" }); }
          catch { if (name.endsWith("-host")) { row.outcome = "unverified"; row.reason = "cross-host-bind"; continue; } throw new Error("bind"); }
          owned.add(receiver!);
          const host = name.endsWith("-host") ? "127.0.0.2" : "127.0.0.1";
          options.redirect = { status: name.startsWith("308") ? 308 : 307, target: `http://${host}:${receiver.port}/target` };
        }
        if (receiver && name !== "receiver-control") {
          const stalled = receiver.options.stall;
          receiver.options.stall = false;
          try {
          await new Promise<void>((resolve, reject) => {
            const req = request(options.redirect!.target, { method: "POST", headers: { authorization: `Bearer ${credential}` }, timeout: 3000 },
              res => settleReceiverResponse(res, resolve, reject));
            req.once("timeout", () => req.destroy(new Error("timeout"))); req.once("error", reject); req.end("synthetic");
          });
          } finally { receiver.options.stall = stalled; }
          row.receiverControlHeader = receiver!.header; row.receiverControlBody = receiver!.body;
          if (!row.receiverControlHeader || !row.receiverControlBody) throw new Error("receiver-control-failure");
          // Preserve the separately recorded positive control, not its target observations.
          receiver!.resetObservations();
        }
        if (name === "receiver-control") {
          await new Promise<void>((resolve, reject) => {
            const req = request(`http://127.0.0.1:${receiver!.port}/target`, { method: "POST", headers: { authorization: `Bearer ${credential}` }, timeout: 3000 },
              res => settleReceiverResponse(res, resolve, reject));
            req.once("timeout", () => req.destroy(new Error("timeout"))); req.once("error", reject); req.end("synthetic");
          });
          row.header = receiver!.header; row.body = receiver!.body;
          row.outcome = row.header && row.body ? "observed-supported" : "observed-failure"; row.reason = "completed";
          if (row.outcome !== "observed-supported") broken = true;
          continue;
        }
        provider = await FakeProvider.start(options); owned.add(provider);
        measuredProvider = provider;
        if (name === "destination-control") {
          let rejected = false;
          try {
            await runProtocolTurn(mods, { provider, credential, grants: { role: "coder", phase: "implement", allowedTools: new Set(), allowedSkills: new Set(), isRoot: true },
              spawnCommand: async () => { throw new Error("unexpected-command"); }, fileop: { op: async () => { throw new Error("unexpected-fileop"); } },
              fixture: { env: {}, table: table => table.replace(provider!.baseUrl, `http://127.0.0.1:${provider!.port === 65535 ? 65534 : provider!.port + 1}/v1`) },
              configObservation: config => { Object.assign(row, config); }, signal: caseAbort.signal });
          } catch (error) { if (error instanceof Error && error.message === "config-policy") rejected = true; else throw error; }
          if (!rejected || row.destinationMatched !== false || provider.requests.length !== 0) throw new Error("destination-control-failure");
          row.outcome = "observed-supported"; row.reason = "auxiliary-network-unverified";
          continue;
        }
        let fixtureOptions: ProtocolTurnOptions["fixture"];
        if (fixture) {
          const env: NodeJS.ProcessEnv = {};
          if (name === "env-key" || name === "env-key-control") env.SPIKE_KEY = name === "env-key" ? credential : wrong;
          if (["tls-ssl", "tls-combined", "tls-untrusted", "tls-stall"].includes(name)) env.SSL_CERT_FILE = path.join(base, name === "tls-untrusted" ? "untrusted.crt" : "trusted.crt");
          if (["tls-codex", "tls-combined"].includes(name)) env.CODEX_CA_CERTIFICATE = path.join(base, "trusted.crt");
          fixtureOptions = { env, table: table => {
            if (name.startsWith("tls-")) table = table.replace("http://", "https://");
            if (name === "unsupported-wire" || name === "responses-control") table = wireFixture(table, name);
            if (name === "env-key" || name === "env-key-control") table = table.replace("requires_openai_auth = true", 'requires_openai_auth = false\nenv_key = "SPIKE_KEY"');
            if (name === "alternate-header") table = table.replace("requires_openai_auth = true", `requires_openai_auth = false\nhttp_headers = { "x-spike-key" = "${credential}" }`);
            return table;
          } };
        }
        const turnOptions: ProtocolTurnOptions = { provider, credential: ["wrong-key", "guard-removed", "env-key", "env-key-control"].includes(name) ? wrong : credential,
          onResumeProbe: () => { resumeProbe.started = true; measuredProvider?.resetObservations(); },
          ...(name === "unknown-model" ? { model: "spike-unknown-model", metadataObservation: (value: { modelContextWindowOwn: boolean; modelContextWindowNullableInteger: boolean }) => Object.assign(row, value), warningObservation: () => { row.fallbackWarningObserved = true; } } : {}),
          adversarialAdvertisement: name === "tool-grant-denied", codeModeHost: m2 ? !["callback-disabled", "advice"].includes(name) : false, allowCallbacks: m2,
          lifecycle: name === "advice" ? "advice" : ["resume", "same-process-override"].includes(name) ? "resume" : "start",
          diagnostic: code => { diagnosis = code; }, signal: caseAbort.signal, fixture: fixtureOptions, turnDeadlineMs: 8000,
          launcherEnv: { UZI_UID_SPLIT: "1", SSL_CERT_FILE: "synthetic", CODEX_CA_CERTIFICATE: "synthetic", OPENAI_API_KEY: "synthetic-poison", OPENAI_BASE_URL: "http://127.0.0.1:1/v1", HTTPS_PROXY: "http://127.0.0.1:1", NODE_OPTIONS: "--synthetic-poison" },
          configObservation: config => { Object.assign(row, config); },
          captureActual: env => { row.actualSSL = env.SSL_CERT_FILE !== undefined; row.actualCodex = env.CODEX_CA_CERTIFICATE !== undefined; row.actualCredentialEnv = Object.values(env).includes(credential); },
          capture: env => { row.caSSL = env.SSL_CERT_FILE !== undefined; row.caCodex = env.CODEX_CA_CERTIFICATE !== undefined;
            row.credentialEnv = Object.values(env).includes(credential); row.poisonStripped = ["OPENAI_API_KEY", "OPENAI_BASE_URL", "HTTPS_PROXY", "NODE_OPTIONS"].every(k => env[k] === undefined); if (!row.poisonStripped) throw new Error("env-policy"); },
          // Reviewer allocation lacks Bash even though the adversarial schema advertises it.
          grants: { role: name === "tool-grant-denied" ? "reviewer" : "coder", phase: name === "phase-denied" ? "plan" : "implement", allowedTools: new Set(m2 && name !== "tool-grant-denied" ? ["Bash", "apply_patch"] : []), allowedSkills: new Set(), isRoot: true },
          spawnCommand: async () => { if (!m2) throw new Error("unexpected-command"); effects++; return { code: 0, stdout: "synthetic", stderr: "" }; }, fileop: { op: async () => { effects++; return { ok: true }; } } };
        if (name === "unknown-model") row.fallbackWarningObserved = false;
        let result;
        if (name === "same-process-override") {
          receiver = await FakeProvider.start({ credential }); owned.add(receiver);
          Object.assign(turnOptions, { resumeOverride: receiver });
        }
        if (["restart-rebind", "restart-closed-a"].includes(name)) {
          const seed = path.join(base, "sessions-" + name);
          const prime = await runProtocolTurn(mods, { ...turnOptions, session: { save: seed } });
          if (prime.turnStatus !== "completed" || !provider.streamed) throw new Error("prime-failure");
          const closedA = provider;
          await closedA.close();
          receiver = await FakeProvider.start({ credential }); owned.add(receiver);
          const control = await runProtocolTurn(mods, { ...turnOptions, provider: receiver });
          if (control.turnStatus !== "completed" || !receiver.responsePath) throw new Error("B observer failure");
          row.observerControlPassed = true;
          receiver.resetObservations();
          const target = name === "restart-rebind" ? receiver : closedA;
          measuredProvider = target;
          result = await runProtocolTurn(mods, { ...turnOptions, provider: target, session: { seed, resumeId: prime.threadId } });
          row.resumed = result.threadId === prime.threadId && result.turnStatus === "completed";
          row.effect = receiver.requests.length > 0;
          row.streamed = receiver.streamed && row.resumed;
          if (name === "restart-rebind" ? !row.streamed || !row.effect : row.resumed || row.effect) broken = true;
        } else result = await runProtocolTurn(mods, turnOptions);
        if (m2) {
          row.callbackAttempted = result.callbackAttempted; row.callbackSeen = result.callbacks.length > 0; if (!name.startsWith("restart-")) row.effect = effects > 0;
          row.denied = result.callbacks.some(c => !c.result.ok);
          row.roundtrip = measuredProvider.callbackOutput && (name !== "callback" || measuredProvider.callbackResultMatched);
          row.cancelled = name === "cancel-stream" && caseAbort.signal.aborted && result.turnStatus === undefined;
          if (!name.startsWith("restart-")) row.resumed = name === "resume" && resumeProbe.started && provider.requests.length > 0 && result.turnStatus === "completed";
          const valid = name === "callback" ? row.callbackSeen && row.effect && row.roundtrip && !row.denied
            : ["tool-grant-denied", "phase-denied"].includes(name) ? row.callbackSeen && row.denied && !row.effect && row.roundtrip
            : ["callback-disabled", "advice"].includes(name) ? !row.callbackSeen && !row.effect && (name !== "advice" || !row.callbackAttempted)
            : name === "cancel-stream" ? row.cancelled && provider.streamed
            : name === "resume" ? row.resumed : result.turnStatus === "completed" && !caseAbort.signal.aborted;
          if (!valid) broken = true;
        }
        if (name === "same-process-override" && (result.turnStatus === "completed" || receiver?.responsePath)) broken = true;
        row.streamed = name === "restart-rebind" ? !!receiver?.streamed && result.turnStatus === "completed" : name === "restart-closed-a" ? false : provider.streamed && result.turnStatus === "completed";
        row.outcome = row.cancelled || row.streamed ? "observed-supported" : "observed-failure";
        row.reason = row.cancelled || row.streamed ? "completed" : ["wrong-key", "env-key-control"].includes(name) && !provider.accepted && provider.responsePath ? "rejected" : name.startsWith("tls-") && !provider.responsePath && diagnosis === "tls-refused" ? "tls-refused" : "protocol-failure";
        if (name === "unknown-model" && (!row.modelContextWindowOwn || !row.modelContextWindowNullableInteger || !row.fallbackWarningObserved || !row.streamed)) broken = true;
        if (provider.errors.length || provider.oversized || receiver?.errors.length || receiver?.oversized) broken = true;
      } catch (error) {
        if (error instanceof AggregateError) { row.cleanupFailed = true; broken = true; }
        row.reason = diagnosis ?? (caseAbort.signal.aborted || error instanceof Error && /deadline|timed out|timeout/i.test(error.message) ? "deadline" : "harness-failure");
        row.deadlineObserved = row.reason === "deadline";
        if (row.reason === "harness-failure") broken = true;
        if (name === "restart-rebind") broken = true;
        if (name === "same-process-override" && resumeDeadlineObserved(resumeProbe.started, row.reason, receiver?.requests.length)) { row.resumed = false; row.effect = false; }
        if (name === "destination-control" && (row.outcome !== "observed-supported" || row.destinationMatched !== false || row.actualSSL || row.actualCodex || row.actualCredentialEnv)) broken = true;
      if (name === "restart-rebind" && (!row.resumed || !row.effect || !row.streamed)) broken = true;
      if (name === "restart-closed-a" && row.observerControlPassed && resumeDeadlineObserved(resumeProbe.started, row.reason, receiver?.requests.length)) {
          row.resumed = false; row.effect = false;
        }
      }
      finally {
        await Promise.all([provider?.close(), receiver?.close()]);
        if (provider) owned.delete(provider); if (receiver) owned.delete(receiver);
        clearTimeout(caseTimer); globalAbort.signal.removeEventListener("abort", abortCase);
        Object.assign(row, inventoryEvidence(measuredProvider));
        if (provider?.errors.length || provider?.oversized || receiver?.errors.length || receiver?.oversized) broken = true;
        row.credentialReceived = measuredProvider?.credentialReceived ?? false;
        row.responsePath = measuredProvider?.responsePath ?? false;
        row.header = (receiver ?? provider)?.header ?? false;
        row.body = (receiver ?? provider)?.body ?? false;
        if (name !== "receiver-control" && (row.header || row.body)) {
          row.outcome = "observed-failure"; row.reason = "forwarded";
        }
        row.elapsedMs = Date.now() - caseStarted;
        row.requestPaths = [...new Set([...(measuredProvider?.paths ?? []), ...(receiver?.paths ?? [])])];
        row.headerNames = [...new Set([...(measuredProvider?.headerNames ?? []), ...(receiver?.headerNames ?? [])])];
        evidence.add(row);
      }
      if (["baseline", "guard-removed", "responses-control", "restart-rebind", "rebind-observer-control"].includes(name) && !row.streamed) broken = true;
      if (["baseline", "guard-removed", "responses-control", "restart-rebind", "rebind-observer-control"].includes(name) && row.outcome !== "observed-supported") broken = true;
      if (["callback", "callback-disabled", "tool-grant-denied", "phase-denied", "advice", "cancel-stream", "cancel-control", "resume"].includes(name) && row.outcome !== "observed-supported") broken = true;
      if (name === "destination-control" && (row.outcome !== "observed-supported" || row.destinationMatched !== false || row.actualSSL || row.actualCodex || row.actualCredentialEnv)) broken = true;
      if (name === "restart-rebind" && (!row.resumed || !row.effect || !row.streamed)) broken = true;
      if (name === "restart-closed-a" && (row.deadlineObserved !== true || row.resumed !== false || row.effect !== false)) broken = true;
      if (name === "same-process-override" && (row.deadlineObserved !== true || row.resumed !== false || row.effect !== false || receiver?.responsePath)) broken = true;
      if (name === "307-port-stall" && (!row.body || row.streamed || row.reason !== "forwarded" || row.outcome !== "observed-failure")) broken = true;
      if ((name.endsWith("-host") || name.endsWith("-port") || name === "307-port-stall") && (!row.receiverControlHeader || !row.receiverControlBody)) broken = true;
      if (name === "env-key" && (!row.streamed || !row.credentialReceived || !row.actualCredentialEnv)) broken = true;
      if (["wrong-key", "env-key-control"].includes(name) && (row.reason !== "rejected" || !row.responsePath || row.credentialReceived)) broken = true;
      if (name === "env-key-control" && (row.streamed || row.outcome !== "observed-failure")) broken = true;
      if (name === "guard-removed" && row.credentialReceived) broken = true;
      if (name === "unsupported-wire" && (row.reason !== "config-rejected" || row.requestPaths.length !== 0)) broken = true;
    }
    console.log(JSON.stringify({ schema: 2, version: lock.version, sourceCommit: lockText.match(/^CODEX_SOURCE_COMMIT=(.+)$/m)?.[1],
      source: "image-baked", posture: "protocol-only", status: broken ? "harness-broken" : "characterized",
      metadataKey: evidence.rows.some(r => r.name === "unknown-model" && r.modelContextWindowOwn && r.modelContextWindowNullableInteger) ? "model_context_window" : null,
      auxiliaryDestinations: "unverified", rows: evidence.rows }));
    if (broken) process.exitCode = 1;
  } finally { rmSync(base, { recursive: true, force: true }); }
  } finally { clearTimeout(globalTimer); await Promise.all([...owned].map(server => server.close())); }
}
if (require.main === module) void main().catch(() => { console.log(JSON.stringify({ status: "harness-broken", reason: "prerequisite-or-budget" })); process.exitCode = 1; });

export function runOpenSSL(args: string[], cwd: string) {
  return spawnSync("/opt/uzi-toolchain/bin/openssl", args, { cwd, env: { PATH: "/usr/bin:/bin" }, timeout: 5000, maxBuffer: 65536 });
}

export function inventoryEvidence(provider: FakeProvider | undefined): Partial<Row> {
  if (!provider?.inventoryObserved) return {};
  return { nativeSuppressed: !provider.nativeTools, nativeApplyPatch: provider.nativeApplyPatch,
    unknownInventory: provider.unknownInventory, ...(provider.nativeTools ? { safetyOutcome: "observed-failure" as const } : {}) };
}

export function resumeDeadlineObserved(started: boolean, reason: Row["reason"], requests: number | undefined): boolean {
  return started && reason === "deadline" && requests === 0;
}

export function settleReceiverResponse(res: IncomingMessage, resolve: () => void, reject: (error: Error) => void): void {
  res.once("end", resolve);
  res.once("error", reject);
  res.once("close", () => { if (!res.complete) reject(new Error("receiver-response-incomplete")); });
  res.resume();
}

export function wireFixture(table: string, name: "unsupported-wire" | "responses-control"): string {
  if (!table.includes('wire_api = "responses"')) throw new Error("fixture-config");
  const unsupported = table.replace('wire_api = "responses"', 'wire_api = "unsupported"');
  return name === "responses-control" ? unsupported.replace('wire_api = "unsupported"', 'wire_api = "responses"') : unsupported;
}
