import { randomBytes } from "node:crypto";
import { readFileSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { request } from "node:http";
import { FakeProvider, Budget, SAFE_HEADER_NAMES, type ServerOptions } from "./fake-provider.js";
import { runProtocolTurn, loadProtocolModules, type ProtocolTurnOptions } from "./driver.js";
import { probeCodexRuntime } from "../../agent/src/codex/codex-runtime-probe.js";
import { parseLock, resolveArch } from "../codex-m4/provision.js";

export const names = ["baseline", "wrong-key", "guard-removed", "env-key", "alternate-header", "unsupported-wire", "responses-control",
  "receiver-control", "307-same", "308-same", "307-port", "307-port-stall", "308-port", "307-host", "308-host",
  "tls-no-ca", "tls-ssl", "tls-codex", "tls-combined", "tls-untrusted", "tls-stall"] as const;
export type Name = typeof names[number];
export function validateName(name: string): asserts name is Name {
  if (!(names as readonly string[]).includes(name)) throw new Error("invalid-case");
}
export type Row = { name: Name; outcome: "observed-supported" | "observed-failure" | "unverified"; path: "production-launcher" | "fixture-config" | "fixture-control";
  streamed: boolean; credentialReceived: boolean; responsePath: boolean; header: boolean; body: boolean; caSSL: boolean; caCodex: boolean; credentialEnv: boolean;
  config: Name; requestPaths: string[]; headerNames: string[]; elapsedMs: number; actualSSL: boolean; actualCodex: boolean; actualCredentialEnv: boolean; keyInConfig: boolean;
  held: boolean; reason: "completed" | "rejected" | "forwarded" | "config-or-runtime-failure" | "deadline" | "cross-host-bind" | "config-rejected" | "tls-refused" | "protocol-failure" | "harness-failure" };
export class Evidence {
  readonly budget = new Budget(1024 * 1024, 64);
  readonly rows: Row[] = [];
  add(row: Row): void {
    validateName(row.name);
    if (!names.includes(row.config) || !["observed-supported", "observed-failure", "unverified"].includes(row.outcome)
      || !["production-launcher", "fixture-config", "fixture-control"].includes(row.path)
      || !["completed", "rejected", "forwarded", "config-or-runtime-failure", "deadline", "cross-host-bind", "config-rejected", "tls-refused", "protocol-failure", "harness-failure"].includes(row.reason)
      || !Number.isSafeInteger(row.elapsedMs) || row.elapsedMs < 0
      || row.requestPaths.some(p => !["/v1/responses", "/target"].includes(p))
      || row.headerNames.length > 64 || row.headerNames.some(h => !SAFE_HEADER_NAMES.has(h)))
      throw new Error("invalid-evidence");
    for (const [key, value] of Object.entries(row)) {
      if (!["name", "config", "outcome", "path", "reason", "requestPaths", "headerNames", "elapsedMs"].includes(key) && typeof value !== "boolean")
        throw new Error("invalid-evidence");
    }
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
    for (const name of names) {
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
      const fixture = ["env-key", "alternate-header", "unsupported-wire", "responses-control"].includes(name) || name.startsWith("tls-");
      const row: Row = { name, outcome: "observed-failure", path: name === "receiver-control" ? "fixture-control" : fixture ? "fixture-config" : "production-launcher",
        streamed: false, credentialReceived: false, responsePath: false, header: false, body: false, caSSL: false, caCodex: false, credentialEnv: false,
        config: name, requestPaths: [], headerNames: [], elapsedMs: 0, actualSSL: false, actualCodex: false, actualCredentialEnv: false, keyInConfig: name === "alternate-header",
        held: name === "env-key" || name === "alternate-header", reason: "rejected" };
      let diagnosis: "config-rejected" | "tls-refused" | undefined;
      try {
        const options: ServerOptions = { credential, guard: name !== "guard-removed", alternate: name === "alternate-header" };
        if (name.startsWith("tls-")) { options.tls = tls; options.stallConnect = name === "tls-stall"; }
        if (name.endsWith("-same")) options.sameOrigin = name.startsWith("307") ? 307 : 308;
        if (name.endsWith("-port") || name === "307-port-stall" || name.endsWith("-host") || name === "receiver-control") {
          try { receiver = await FakeProvider.start({ credential, stall: name === "307-port-stall", host: name.endsWith("-host") ? "127.0.0.2" : "127.0.0.1" }); }
          catch { if (name.endsWith("-host")) { row.outcome = "unverified"; row.reason = "cross-host-bind"; continue; } throw new Error("bind"); }
          owned.add(receiver!);
          const host = name.endsWith("-host") ? "127.0.0.2" : "127.0.0.1";
          options.redirect = { status: name.startsWith("308") ? 308 : 307, target: `http://${host}:${receiver.port}/target` };
        }
        if (name === "receiver-control") {
          await new Promise<void>((resolve, reject) => {
            const req = request(`http://127.0.0.1:${receiver!.port}/target`, { method: "POST", headers: { authorization: `Bearer ${credential}` }, timeout: 3000 },
              res => { res.resume(); res.once("end", resolve); });
            req.once("timeout", () => req.destroy(new Error("timeout"))); req.once("error", reject); req.end("synthetic");
          });
          row.header = receiver!.header; row.body = receiver!.body;
          row.outcome = row.header && row.body ? "observed-supported" : "observed-failure"; row.reason = "completed";
          if (row.outcome !== "observed-supported") broken = true;
          continue;
        }
        provider = await FakeProvider.start(options); owned.add(provider);
        let fixtureOptions: ProtocolTurnOptions["fixture"];
        if (fixture) {
          const env: NodeJS.ProcessEnv = {};
          if (name === "env-key") env.SPIKE_KEY = credential;
          if (["tls-ssl", "tls-combined", "tls-untrusted", "tls-stall"].includes(name)) env.SSL_CERT_FILE = path.join(base, name === "tls-untrusted" ? "untrusted.crt" : "trusted.crt");
          if (["tls-codex", "tls-combined"].includes(name)) env.CODEX_CA_CERTIFICATE = path.join(base, "trusted.crt");
          fixtureOptions = { env, table: table => {
            if (name.startsWith("tls-")) table = table.replace("http://", "https://");
            if (name === "unsupported-wire" || name === "responses-control") table = wireFixture(table, name);
            if (name === "env-key") table = table.replace("requires_openai_auth = true", 'requires_openai_auth = false\nenv_key = "SPIKE_KEY"');
            if (name === "alternate-header") table = table.replace("requires_openai_auth = true", `requires_openai_auth = false\nhttp_headers = { "x-spike-key" = "${credential}" }`);
            return table;
          } };
        }
        const result = await runProtocolTurn(mods, { provider, credential: ["wrong-key", "guard-removed"].includes(name) ? wrong : credential,
          diagnostic: code => { diagnosis = code; }, signal: caseAbort.signal, fixture: fixtureOptions, turnDeadlineMs: 8000,
          launcherEnv: { UZI_UID_SPLIT: "1", SSL_CERT_FILE: "synthetic", CODEX_CA_CERTIFICATE: "synthetic" },
          captureActual: env => { row.actualSSL = env.SSL_CERT_FILE !== undefined; row.actualCodex = env.CODEX_CA_CERTIFICATE !== undefined; row.actualCredentialEnv = Object.values(env).includes(credential); },
          capture: env => { row.caSSL = env.SSL_CERT_FILE !== undefined; row.caCodex = env.CODEX_CA_CERTIFICATE !== undefined;
            row.credentialEnv = Object.values(env).includes(credential); },
          grants: { role: "coder", phase: "implement", allowedTools: new Set(), allowedSkills: new Set(), isRoot: true },
          spawnCommand: async () => { throw new Error("unexpected-command"); }, fileop: { op: async () => ({ ok: false }) } });
        row.streamed = provider.streamed && result.turnStatus === "completed";
        row.outcome = row.streamed ? "observed-supported" : "observed-failure";
        row.reason = row.streamed ? "completed" : name === "wrong-key" && !provider.accepted && provider.responsePath ? "rejected" : name.startsWith("tls-") && !provider.responsePath ? "tls-refused" : "protocol-failure";
      } catch (error) {
        row.reason = diagnosis ?? (caseAbort.signal.aborted || error instanceof Error && /deadline|timed out|timeout/i.test(error.message) ? "deadline" : "harness-failure");
        if (row.reason === "harness-failure") broken = true;
      }
      finally {
        await Promise.all([provider?.close(), receiver?.close()]);
        if (provider) owned.delete(provider); if (receiver) owned.delete(receiver);
        clearTimeout(caseTimer); globalAbort.signal.removeEventListener("abort", abortCase);
        row.credentialReceived = provider?.credentialReceived ?? false;
        row.responsePath = provider?.responsePath ?? false;
        row.header = (receiver ?? provider)?.header ?? false;
        row.body = (receiver ?? provider)?.body ?? false;
        if (name !== "receiver-control" && (row.header || row.body)) {
          row.outcome = "observed-failure"; row.reason = "forwarded";
        }
        row.elapsedMs = Date.now() - caseStarted;
        row.requestPaths = [...new Set([...(provider?.paths ?? []), ...(receiver?.paths ?? [])])];
        row.headerNames = [...new Set([...(provider?.headerNames ?? []), ...(receiver?.headerNames ?? [])])];
        evidence.add(row);
      }
      if (["baseline", "guard-removed", "responses-control"].includes(name) && row.outcome !== "observed-supported") broken = true;
      if (name === "307-port-stall" && (!row.body || row.streamed || row.reason !== "forwarded" || row.outcome !== "observed-failure")) broken = true;
      if (name === "wrong-key" && (row.reason !== "rejected" || !row.responsePath || row.credentialReceived)) broken = true;
      if (name === "guard-removed" && row.credentialReceived) broken = true;
      if (name === "unsupported-wire" && (row.reason !== "config-rejected" || row.requestPaths.length !== 0)) broken = true;
    }
    console.log(JSON.stringify({ schema: 1, version: lock.version, sourceCommit: lockText.match(/^CODEX_SOURCE_COMMIT=(.+)$/m)?.[1],
      source: "image-baked", posture: "protocol-only", status: broken ? "harness-broken" : "characterized",
      auxiliaryDestinations: "unverified", rows: evidence.rows }));
    if (broken) process.exitCode = 1;
  } finally { rmSync(base, { recursive: true, force: true }); }
  } finally { clearTimeout(globalTimer); await Promise.all([...owned].map(server => server.close())); }
}
if (require.main === module) void main().catch(() => { console.log(JSON.stringify({ status: "harness-broken", reason: "prerequisite-or-budget" })); process.exitCode = 1; });

export function runOpenSSL(args: string[], cwd: string) {
  return spawnSync("/opt/uzi-toolchain/bin/openssl", args, { cwd, env: { PATH: "/usr/bin:/bin" }, timeout: 5000, maxBuffer: 65536 });
}

export function wireFixture(table: string, name: "unsupported-wire" | "responses-control"): string {
  if (!table.includes('wire_api = "responses"')) throw new Error("fixture-config");
  const unsupported = table.replace('wire_api = "responses"', 'wire_api = "unsupported"');
  return name === "responses-control" ? unsupported.replace('wire_api = "unsupported"', 'wire_api = "responses"') : unsupported;
}
