// PRD #1156 M3a — Control C: isolation controls specific to the m3 credential-free
// posture. The m2 setsid/double-fork/non-SIGCHLD reaping, child-descriptor closure +
// /proc forgery, and env-canary/home-perm controls already run under the profile posture
// in `controls.sh`; this file adds the m3-specific ones:
//
//   C1  replacement-env canaries read off the REAL Codex app-server child's /proc/environ
//       (not a stub): exactly the allowlist + provider credential, nothing from host/worker.
//   C2  no auth/session/credential material is baked into the shipped Codex image.
//   C3  a MUTATION control: the same allowlist assertion REJECTS a leaked/mutated env,
//       proving C1 is not vacuous (fails the intended assertion after compile/typecheck).
//   C4  malicious repo instructions/config cannot enter the PRODUCTION launch construction
//       (the config builder pins an untrusted project, folds no repo config, and rejects an
//       injection-shaped provider identifier).
//
// No hook-trust bypass, no live credentials.

import assert from "node:assert/strict";
import { lstatSync } from "node:fs";
import { before, test } from "node:test";

import type { CodexLaunchSpec, CodexRootHandle } from "../../agent/src/codex/launcher.js";
import { FakeProvider, dummyCredential, trivialResponder } from "./fake-provider.js";
import {
  loadPackagedConfig,
  loadPackagedLauncher,
  type ConfigModule,
  type LauncherModule,
} from "./packaged-modules.js";
import { asRunnerSync } from "./supervisor-driver.js";

let buildCodexConfigToml: ConfigModule["buildCodexConfigToml"];
let launchCodexRoot: LauncherModule["launchCodexRoot"];
before(async () => {
  [{ buildCodexConfigToml }, { launchCodexRoot }] = await Promise.all([
    loadPackagedConfig(),
    loadPackagedLauncher(),
  ]);
});

const SUPERVISOR_BIN = process.env.M3A_SUPERVISOR_BIN ?? "/usr/local/bin/uzi-codex-supervisor";
const CODEX_BIN = process.env.M3A_CODEX_BIN ?? "/opt/uzi-codex/0.153.2/bin/codex";
const CODEX_PREFIX = process.env.M3A_CODEX_PREFIX ?? "/opt/uzi-codex";
const DATA_BASE = process.env.M3A_DATA_BASE ?? "/data/runner";
const ENV_KEY = "FAKE_PROVIDER_API_KEY";

/** The EXACT replacement-env allowlist a provider root delivers (mirrors the launcher's
 *  `buildReplacedEnv`). The provider credential var is the twelfth allowed key. */
const BASE_ALLOWLIST = [
  "HOME", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
  "XDG_STATE_HOME", "TMPDIR", "PATH", "SHELL", "LANG", "TERM",
] as const;

/** Parse a NUL- or newline-separated /proc/<pid>/environ dump into a key→value map. */
function parseEnviron(raw: string): Map<string, string> {
  const map = new Map<string, string>();
  for (const entry of raw.split(/[\0\n]/)) {
    if (entry.length === 0) continue;
    const eq = entry.indexOf("=");
    if (eq < 0) continue;
    map.set(entry.slice(0, eq), entry.slice(eq + 1));
  }
  return map;
}

/** The intended replacement-env assertion, factored PURE so both the live control (C1)
 *  and the mutation control (C3) drive the identical logic. Throws on ANY violation:
 *  a missing/extra key, an empty allowed value, a wrong credential, a HOME/CODEX_HOME
 *  outside the owned trees, or any host/worker leak. */
export function assertReplacementEnv(
  env: Map<string, string>,
  opts: { credentialKey: string; credentialValue: string; home: string; codexHome: string },
): void {
  const expected = new Set<string>([...BASE_ALLOWLIST, opts.credentialKey]);
  const keys = [...env.keys()];
  // Named + wildcard host/worker leak families that must ALL be absent. Checked FIRST so a
  // recognized leak yields the specific "leaked into child" verdict (the exact-allowlist
  // check below would otherwise flag it only as a generic unexpected key).
  const named = ["M3A_ENV_CANARY", "UZI_WORKER_TOKEN", "UZI_WORKER_TOKEN_FILE", "UZI_UID_SPLIT", "UZI_RUNNER_PATH", "UZI_RUNNER_TMPDIR", "NODE_OPTIONS"];
  const leaked = named.filter((k) => env.has(k));
  for (const k of keys) {
    if (/^ANTHROPIC/.test(k) || /_PAT$/.test(k) || /(^|_)(GH|GITHUB|GITLAB|GITEA|FORGEJO)_TOKEN$/.test(k)) leaked.push(k);
  }
  if (leaked.length > 0) throw new Error(`host/worker env leaked into child: ${leaked.join(", ")}`);
  // Exact allowlist: no missing key, no unexpected extra.
  const missing = [...expected].filter((k) => !env.has(k));
  const extra = keys.filter((k) => !expected.has(k));
  if (missing.length > 0) throw new Error(`child env missing allowlist keys: ${missing.join(", ")}`);
  if (extra.length > 0) throw new Error(`child env carries unexpected keys: ${extra.join(", ")}`);
  for (const k of BASE_ALLOWLIST) {
    if ((env.get(k) ?? "").length === 0) throw new Error(`child env key ${k} is empty`);
  }
  if (env.get(opts.credentialKey) !== opts.credentialValue) throw new Error("provider credential mismatch in child env");
  if (env.get("HOME") !== opts.home) throw new Error(`child HOME=${String(env.get("HOME"))} is not the fresh owned tree`);
  if (env.get("CODEX_HOME") !== opts.codexHome) throw new Error(`child CODEX_HOME=${String(env.get("CODEX_HOME"))} is not the fresh owned tree`);
}

interface Launched { handle: CodexRootHandle; provider: FakeProvider; base: string; home: string; codexHome: string; credential: string }

/** Launch one stock provider root (real launcher + config) and return its handle. */
async function launchStockRoot(model = "gpt-6-astra"): Promise<Launched> {
  const credential = dummyCredential();
  const base = `${DATA_BASE}/m3a-C-${process.pid}-${Math.random().toString(16).slice(2, 8)}`;
  const ownedDataRoot = `${base}/run`;
  const cwd = `${base}/cwd`;
  const mk = asRunnerSync("/bin/sh", ["-c", 'umask 022; mkdir -p "$1" "$2"', "sh", base, cwd]);
  assert.equal(mk.status, 0, `base/cwd mkdir: ${mk.stderr}`);
  const provider = await FakeProvider.start({ credential, respond: trivialResponder() });
  const spec: CodexLaunchSpec = {
    ownedDataRoot,
    provider: { name: "m3aprovC", baseUrl: provider.baseUrl, envKey: ENV_KEY, credentialValue: credential },
    model, codexBin: CODEX_BIN, supervisorBin: SUPERVISOR_BIN, kind: "provider", childArgv: ["app-server"], cwd,
  };
  const handle = await launchCodexRoot(spec);
  return { handle, provider, base, home: `${ownedDataRoot}/home`, codexHome: `${ownedDataRoot}/codex`, credential };
}

// ── C1: replacement-env canaries on the REAL Codex app-server child ─────────────────
test("C1: real Codex child sees ONLY the replacement-env allowlist + credential, no host/worker leak", async (t) => {
  // A pointed canary in the TEST env: the launcher REPLACES env (never merges), so it must
  // not reach the child. The worker env already carries UZI_UID_SPLIT/UZI_RUNNER_PATH etc.
  process.env.M3A_ENV_CANARY = "leak-canary-must-not-reach-child";
  const launched = await launchStockRoot();
  t.after(async () => {
    try { if (!launched.handle.failed) await launched.handle.dispose(); } catch { /* asserted elsewhere */ }
    await launched.provider.close();
    asRunnerSync("/bin/rm", ["-rf", launched.base]);
    delete process.env.M3A_ENV_CANARY;
  });

  const childPid = launched.handle.started.childPid;
  const snapshot = await launched.handle.snapshot();
  assert.ok(snapshot.processes.some((process) => process.pid === childPid), `started child ${childPid} must still be owned by the supervisor`);
  // Read the REAL codex child's environ as the runner uid (owner of the dumpable child).
  // `started` follows ForkExec, so /proc can briefly expose an empty environ while exec is
  // settling. Poll for positive non-empty evidence; a vanished child or persistent empty
  // result fails rather than being interpreted as an empty allowlist.
  let read = asRunnerSync("/bin/false", []);
  for (let attempt = 0; attempt < 80; attempt += 1) {
    read = asRunnerSync("/bin/sh", ["-c", 'tr "\\000" "\\n" < "$1"', "sh", `/proc/${childPid}/environ`]);
    if (read.status === 0 && read.stdout.length > 0) break;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  assert.equal(read.status, 0, `read /proc/${childPid}/environ as runner: ${read.stderr}`);
  assert.notEqual(read.stdout.length, 0, `read /proc/${childPid}/environ returned no positive evidence`);
  const env = parseEnviron(read.stdout);
  // The intended assertion must PASS for the real child.
  assertReplacementEnv(env, { credentialKey: ENV_KEY, credentialValue: launched.credential, home: launched.home, codexHome: launched.codexHome });
  t.diagnostic(JSON.stringify({ childPid, childEnvKeys: [...env.keys()].sort() }));
});

// ── C5: confinement posture — fresh trees on the owned writable mount, uid 10002 0700 ─
test("C5: fresh HOME/CODEX_HOME/XDG/TMPDIR land on the owned writable mount, uid 10002 mode 0700", async (t) => {
  const launched = await launchStockRoot();
  t.after(async () => {
    try { if (!launched.handle.failed) await launched.handle.dispose(); } catch { /* asserted elsewhere */ }
    await launched.provider.close();
    asRunnerSync("/bin/rm", ["-rf", launched.base]);
  });
  const ownedDataRoot = `${launched.base}/run`;
  // The owned writable mount is /data (a writable tmpfs under the read-only root fs).
  assert.ok(launched.base.startsWith("/data/"), `owned root is on the writable /data mount (${launched.base})`);
  let setgidSeen = false;
  for (const name of ["home", "codex", "xdg-config", "xdg-cache", "xdg-data", "xdg-state", "tmp"]) {
    const tree = `${ownedDataRoot}/${name}`;
    const s = asRunnerSync("/bin/stat", ["-c", "%u %a", tree]);
    assert.equal(s.status, 0, `stat ${tree}: ${s.stderr}`);
    const [uid, mode] = s.stdout.trim().split(/\s+/);
    assert.equal(uid, "10002", `${name} owned by runner uid 10002`);
    // The trees carry an inherited setgid bit (raw 2700) from the setgid /data/runner
    // parent; access stays owner-only 0700 (group/other get nothing). Assert the low bits.
    if (mode.length === 4) setgidSeen = true;
    assert.equal(mode.slice(-3), "700", `${name} owner-only 0700 access (raw ${mode})`);
  }
  const cfg = asRunnerSync("/bin/stat", ["-c", "%u %a", `${ownedDataRoot}/codex/config.toml`]);
  assert.equal(cfg.status, 0, `stat config.toml: ${cfg.stderr}`);
  assert.deepEqual(cfg.stdout.trim().split(/\s+/), ["10002", "600"], "config.toml uid 10002 mode 0600");
  t.diagnostic(JSON.stringify({ ownedDataRoot, setgidInherited: setgidSeen }));
});

// ── C2: no auth/session/credential material baked into the image ────────────────────
test("C2: no auth/session/credential material is baked into the shipped Codex image", () => {
  // /etc/codex must be absent (a fresh HOME does not neutralize system config; the launcher
  // refuses when it exists — here we assert the shipped state is genuinely clean).
  assert.throws(() => lstatSync("/etc/codex"), (e: unknown) => (e as NodeJS.ErrnoException).code === "ENOENT",
    "the shipped image installs no /etc/codex");
  // Scan the Codex install for any credential/session/token/key residue.
  const find = asRunnerSync("/usr/bin/find", [CODEX_PREFIX, "-type", "f"]);
  assert.equal(find.status, 0, `find ${CODEX_PREFIX}: ${find.stderr}`);
  const files = find.stdout.split("\n").map((l) => l.trim()).filter((l) => l.length > 0);
  assert.ok(files.length > 0, "the Codex install has files (find worked)");
  const suspicious = /(^|\/)(auth\.json|\.codexauth|session|sessions|credentials?|token|id_rsa|id_ecdsa|.*\.pem)($|[./])/i;
  const hits = files.filter((f) => suspicious.test(f));
  assert.deepEqual(hits, [], `no baked auth/session/credential files under ${CODEX_PREFIX}`);
  // The install root carries only the pinned package layout (no dotfile config/state).
  const dots = files.filter((f) => f.split("/").some((seg) => seg.startsWith(".") && seg.length > 1));
  assert.deepEqual(dots, [], `no dotfile config/state baked into ${CODEX_PREFIX}`);
});

// ── C3: MUTATION control — the C1 assertion REJECTS a leaked/mutated env ─────────────
test("C3 (mutation control): the replacement-env assertion fails a leaked/mutated child env", () => {
  const credential = dummyCredential();
  const good = new Map<string, string>([
    ...BASE_ALLOWLIST.map((k) => [k, k === "PATH" ? "/usr/bin" : `val-${k}`] as [string, string]),
    [ENV_KEY, credential],
  ]);
  good.set("HOME", "/data/runner/x/home");
  good.set("CODEX_HOME", "/data/runner/x/codex");
  const args = { credentialKey: ENV_KEY, credentialValue: credential, home: "/data/runner/x/home", codexHome: "/data/runner/x/codex" };
  // The unmutated env passes (the assertion is real, not always-throwing).
  assertReplacementEnv(good, args);
  // Each mutation must be REJECTED — proving the assertion is non-vacuous.
  const leaked = new Map(good); leaked.set("UZI_WORKER_TOKEN", "sk-worker");
  assert.throws(() => assertReplacementEnv(leaked, args), /leaked into child/);
  const canary = new Map(good); canary.set("M3A_ENV_CANARY", "leak");
  assert.throws(() => assertReplacementEnv(canary, args), /leaked into child/);
  const wrongCred = new Map(good); wrongCred.set(ENV_KEY, "sk-other");
  assert.throws(() => assertReplacementEnv(wrongCred, args), /credential mismatch/);
  const missing = new Map(good); missing.delete("CODEX_HOME");
  assert.throws(() => assertReplacementEnv(missing, args), /missing allowlist keys/);
  const anthropic = new Map(good); anthropic.set("ANTHROPIC_API_KEY", "sk-ant");
  assert.throws(() => assertReplacementEnv(anthropic, args), /leaked into child/);
});

// ── C4: malicious repo instructions/config cannot enter the launch construction ─────
test("C4: the production config builder pins an untrusted project, folds no repo config, and rejects injection", () => {
  const cwd = "/data/runner/repo-under-test";
  const toml = buildCodexConfigToml({
    model: "gpt-6-astra",
    provider: { name: "m3aprov", baseUrl: "http://127.0.0.1:1/v1", envKey: ENV_KEY, wireApi: "responses" },
    projectPath: cwd,
  });
  // The canonical project is EXPLICITLY untrusted with docs disabled (no repo AGENTS.md is
  // read). The project path is a QUOTED TOML table key so any path is represented safely.
  assert.ok(toml.includes(`[projects."${cwd}"]`), "canonical project table present (quoted key)");
  assert.match(toml, /trust_level = "untrusted"/);
  assert.match(toml, /project_doc_max_bytes = 0/);
  // Native authority is pinned OFF and the code-mode host is DISABLED in the stock config.
  for (const off of ["code_mode_host = false", "code_mode = false", "hooks = false", "unified_exec = false", "multi_agent = false", "multi_agent_v2 = false"]) {
    assert.ok(toml.includes(off), `stock config pins ${off}`);
  }
  assert.doesNotMatch(toml, /bypass_hook_trust/, "no hook-trust bypass in the production config");
  // A repo-controlled value shaped for TOML injection through the provider identifier is REJECTED.
  assert.throws(() => buildCodexConfigToml({
    model: "gpt-6-astra",
    provider: { name: 'evil"]\ninjected = true\n[x', baseUrl: "http://127.0.0.1:1/v1", envKey: ENV_KEY, wireApi: "responses" },
    projectPath: cwd,
  }), /provider name must match/);
  // A malicious project path is SAFELY quoted (no unescaped injection into the table header).
  const evilCwd = 'x"]\nmalicious = true\n[y';
  const t2 = buildCodexConfigToml({ model: "m", provider: { name: "p", baseUrl: "http://127.0.0.1:1/v1", envKey: ENV_KEY, wireApi: "responses" }, projectPath: evilCwd });
  assert.doesNotMatch(t2, /^malicious = true$/m, "a malicious project path cannot inject a top-level key");
});
