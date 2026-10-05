// Deliberately outside test/*.test.ts. Only the root confinement target selects this proof.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createInterface } from "node:readline";
import {
  CODEX_BIN, SUPERVISOR_BIN, PROVIDER_CHILD_ARGV, launchCodexRoot, launchCodexEffectRoot,
  type LauncherDeps, type CodexLaunchSpec,
} from "../src/codex/launcher.js";
import { createCodexTransport } from "../src/codex/transport.js";
import { createCodexAppServerAuth } from "../src/codex/appserver-auth.js";
import { FileopHelperClient } from "../src/codex/fileop-client.js";
import { commandRootCommand, runnerCommand, RUNNER_UID } from "../src/runner-uid.js";

const required = process.env.UZI_CROSS_CHECK_CONFINEMENT_REQUIRED === "1";
const helper = process.env.UZI_CROSS_CHECK_SANDBOX;
const supervisor = process.env.UZI_CROSS_CHECK_SUPERVISOR;
const fileop = process.env.UZI_CROSS_CHECK_FILEOP;
const scratch = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../.uzi/scratch");
const probeUrl = new URL("./cross-check-confinement-probe.mjs", import.meta.url).href;
const { filesystemProbe, assertIsolation } = await import(probeUrl);

function prerequisites(identity: "provider" | "command" = "provider"): void {
  if (!required || process.platform !== "linux" || !helper || !supervisor || !fileop
    || ![helper, supervisor, fileop, CODEX_BIN].every((p) => existsSync(p))
    || process.env.UZI_UID_SPLIT !== "1") {
    throw new Error("NOT RUN: explicit root target, fresh helpers, packaged Codex and real uid split required");
  }
  const boundary = (identity === "command" ? commandRootCommand : runnerCommand)("/bin/true", []);
  const available = spawnSync(boundary.command, boundary.args, { encoding: "utf8", timeout: 5000, env: { PATH: "/usr/bin:/bin", LANG: "C" } });
  if (available.status !== 0) throw new Error("NOT RUN: real " + identity + " uid boundary unavailable: " + available.stderr.trim());
}

function fixture() {
  mkdirSync(scratch, { recursive: true });
  const base = mkdtempSync(path.join(scratch, "packaged-confinement-"));
  // Both real identities must traverse the shared synthetic fixture parent.
  chmodSync(base, 0o777);
  const checkout = path.join(base, "checkout");
  const sibling = path.join(base, "sibling");
  for (const p of [checkout, sibling]) { mkdirSync(p, { mode: 0o777 }); chmodSync(p, 0o777); }
  writeFileSync(path.join(checkout, "read.txt"), "checkout");
  writeFileSync(path.join(sibling, "secret.txt"), "synthetic sibling");
  chmodSync(path.join(sibling, "secret.txt"), 0o666);
  symlinkSync(path.join(sibling, "secret.txt"), path.join(checkout, "escape"));
  return { base, checkout, sibling };
}

const deps = (): LauncherDeps => ({
  helperBinsForTest: { supervisor: supervisor!, crossCheckSandbox: helper! },
});

function providerSpec(checkout: string, state: string): CodexLaunchSpec {
  return {
    kind: "provider", codexBin: CODEX_BIN, supervisorBin: SUPERVISOR_BIN,
    childArgv: PROVIDER_CHILD_ARGV, cwd: checkout, ownedDataRoot: state,
    provider: { name: "openai", baseUrl: "https://api.openai.com/v1", envKey: "CODEX_PROVIDER_KEY", credentialValue: "" },
    model: "gpt-5-codex", useAppServerAuth: true, authMode: "api_key", crossCheckReadOnly: true, codeModeHost: true,
  };
}

test("packaged fileop enforces RO independently of provider/broker admission", { timeout: 30000 }, async () => {
  prerequisites("command");
  const { base, checkout } = fixture();
  const state = path.join(base, "fileop-private");
  const executable = path.join(state, "fileop");
  let handle: Awaited<ReturnType<typeof launchCodexEffectRoot>> | undefined;
  try {
    const provision = commandRootCommand("/bin/sh", ["-ceu",
      'mkdir -m 700 "$1"; chmod 700 "$1"; cp "$2" "$1/fileop"; chmod 700 "$1/fileop"',
      "sh", state, fileop!]);
    const provisioned = spawnSync(provision.command, provision.args, { encoding: "utf8", timeout: 5000, env: { PATH: "/usr/bin:/bin", LANG: "C" } });
    assert.equal(provisioned.status, 0, "NOT RUN: command uid provisioning unavailable: " + provisioned.stderr);
    handle = await launchCodexEffectRoot({
      identity: "command", supervisorBin: SUPERVISOR_BIN, command: helper!,
      args: ["--cross-check", "--root", checkout, "--state", state, "--cwd", checkout, "--", executable, "--root", checkout],
      cwd: checkout, env: { PATH: "/usr/bin:/bin", HOME: state, TMPDIR: state, LANG: "C" },
    }, deps());
    const client = new FileopHelperClient({ inbound: handle.transport.stdout!, outbound: handle.transport.stdin!, requestTimeoutMs: 5000 });
    const read = await client.op({ op: "read", path: "read.txt" });
    assert.equal(read.ok, true, JSON.stringify(read));
    assert.equal(Buffer.from(read.data!, "base64").toString(), "checkout");
    const write = await client.op({ op: "write", path: "write.txt", data: Buffer.from("probe").toString("base64") });
    assert.equal(write.ok, false);
    assert.equal(write.code, "E_PERM", "RO enforced on an in-root write, independently of traversal denial");
    for (const p of ["../sibling/secret.txt", "escape"]) {
      const response = await client.op({ op: "read", path: p });
      assert.equal(response.ok, false);
      assert.ok(["E_ESCAPE", "E_SYMLINK"].includes(response.code!));
    }
    console.log("PASS: FILEOP actual Read, in-root write denial, separate traversal/symlink checks");
  } catch (e) { console.error("NOT RUN/FAIL: packaged fileop proof", e); throw e; }
  finally {
    if (handle) assert.equal((await handle.dispose(5000)).clean, true);
    const cleanup = commandRootCommand("/bin/rm", ["-rf", "--", state]);
    const cleaned = spawnSync(cleanup.command, cleanup.args, { encoding: "utf8", timeout: 5000, env: { PATH: "/usr/bin:/bin", LANG: "C" } });
    if (existsSync(state)) assert.equal(cleaned.status, 0, "NOT RUN: command fixture cleanup unavailable");
    rmSync(base, { recursive: true, force: true });
  }
});

test("provider direct filesystem enforcement under real launcher posture", { timeout: 30000 }, async () => {
  prerequisites();
  const { base, checkout, sibling } = fixture();
  let handle: Awaited<ReturnType<typeof launchCodexRoot>> | undefined;
  try {
    const state = path.join(base, "provider-direct");
    handle = await launchCodexRoot(providerSpec(checkout, state), {
      ...deps(),
      // Trusted fixture replaces only the pinned child, retaining real setpriv,
      // provisioning, supervisor evidence, confinement and disposal.
      spawnSupervisor(command, args, options) {
        const index = args.indexOf(CODEX_BIN);
        assert.ok(index >= 0);
        const actual = [...args.slice(0, index), process.execPath, "--eval", filesystemProbe, checkout, state, sibling];
        return spawn(command, actual, { ...options, stdio: [...options.stdio] });
      },
    });
    const line = await new Promise<string>((resolve, reject) => {
      const lines = createInterface({ input: handle!.transport.stdout! });
      const timer = setTimeout(() => { lines.close(); reject(new Error("NOT RUN: direct provider probe produced no result")); }, 10000);
      lines.once("line", (value) => { clearTimeout(timer); lines.close(); resolve(value); });
    });
    assertIsolation(JSON.parse(line));
    assert.equal(handle.started.uid, RUNNER_UID);
    console.log("PASS: PROVIDER direct filesystem enforcement (real uid/custody/supervisor/helper)");
  } catch (e) { console.error("NOT RUN/FAIL: provider direct proof", e); throw e; }
  finally {
    if (handle) assert.equal((await handle.dispose(5000)).clean, true);
    rmSync(base, { recursive: true, force: true });
  }
});

test("packaged Codex authenticates and creates a session with confined production config", { timeout: 45000 }, async () => {
  prerequisites();
  const { base, checkout } = fixture();
  let handle: Awaited<ReturnType<typeof launchCodexRoot>> | undefined;
  let transport: ReturnType<typeof createCodexTransport> | undefined;
  const auth = createCodexAppServerAuth({ mode: "api_key", apiKey: ["synthetic", "confinement", "credential"].join("-") });
  try {
    const state = path.join(base, "provider-login");
    handle = await launchCodexRoot(providerSpec(checkout, state), deps());
    transport = createCodexTransport({ inbound: handle.transport.stdout!, outbound: handle.transport.stdin! });
    await auth.authenticate(transport, AbortSignal.timeout(15000));
    const config = await transport.request<{ config: Record<string, unknown> }>("config/read", { includeLayers: false }, { signal: AbortSignal.timeout(5000) });
    assert.equal(config.config.project_doc_max_bytes, 0);
    assert.equal((config.config.features as Record<string, unknown>).shell_tool, false);
    const result = await transport.request<{ thread: { id: string } }>("thread/start", {
      model: "gpt-5-codex", cwd: checkout, approvalPolicy: "never", sandbox: "danger-full-access", ephemeral: true,
    }, { signal: AbortSignal.timeout(5000) });
    assert.ok(result.thread.id);
    // Session creation only; check auth/config/session custody using the real runner uid helper. No model turn or external call.
    const custodyCommand = runnerCommand(process.execPath, ["--eval",
      `const fs=require("node:fs"); const root=process.argv[1]; const st=p=>fs.statSync(root+p);
      console.log(JSON.stringify({root:st(""),config:st("/codex/config.toml"),auth:st("/codex/auth.json"),sessions:st("/codex/sessions")}));`,
      state,
    ]);
    const custody = spawnSync(custodyCommand.command, custodyCommand.args, { encoding: "utf8", timeout: 5000, env: { PATH: "/usr/bin:/bin", LANG: "C" } });
    assert.equal(custody.status, 0, "NOT RUN: real runner custody inspection failed: " + custody.stderr);
    const observed = JSON.parse(custody.stdout) as Record<string, { uid: number; mode: number }>;
    for (const key of ["root", "config", "auth", "sessions"]) assert.equal(observed[key]!.uid, RUNNER_UID);
    assert.equal(observed.root!.mode & 0o7777, 0o710);
    assert.equal(observed.config!.mode & 0o7777, 0o600);
    assert.equal(observed.auth!.mode & 0o7777, 0o600);
    assert.equal(observed.sessions!.mode & 0o7777, 0o2750);
    console.log("PASS: PACKAGED Codex initialize/login/session creation/config and credential custody");
  } catch (e) { console.error("NOT RUN/FAIL: packaged Codex proof", e); throw e; }
  finally {
    auth.closeAdmissionAndCancel();
    await transport?.close();
    await auth.drainInterceptedRequests();
    if (handle) assert.equal((await handle.dispose(5000)).clean, true);
    rmSync(base, { recursive: true, force: true });
  }
});
