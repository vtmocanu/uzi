import { spawnSync } from "node:child_process";
import { makeTextRedactor } from "../src/redact.js";

/** Maintainer-only gate, evaluated before any runtime, filesystem credential access or Docker call. */
export function liveArgs(argv: string[], env: NodeJS.ProcessEnv): {
  credential: string; variable: string; modelA: string; modelB: string; effortA: "high"; effortB: "medium";
} {
  if (env.UZI_PHASE_MODEL_LIVE !== "1") throw new Error("refusing live probe: set UZI_PHASE_MODEL_LIVE=1");
  const values = new Map<string, string>();
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i], value = argv[i + 1];
    if (!key || !["--credential-env", "--model-a", "--model-b", "--effort-a", "--effort-b"].includes(key) || !value || values.has(key))
      throw new Error("invalid or duplicate probe argument");
    values.set(key, value);
  }
  const variable = values.get("--credential-env");
  if (!variable || !/^[A-Z][A-Z0-9_]*$/.test(variable) || !env[variable]) throw new Error("refusing live probe: named credential variable is absent");
  const modelA = values.get("--model-a"), modelB = values.get("--model-b");
  if (!modelA || !modelB || ![modelA, modelB].every(v => /^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$/.test(v)) || modelA === modelB)
    throw new Error("supply distinct explicit --model-a and --model-b");
  if (values.get("--effort-a") !== "high" || values.get("--effort-b") !== "medium") throw new Error("probe matrix requires --effort-a high --effort-b medium");
  return { credential: env[variable]!, variable, modelA, modelB, effortA: "high", effortB: "medium" };
}

/** Exact-secret scrub plus identity/token-shape scrubbing; errors otherwise retain their wording. */
export function probeError(error: unknown, credential?: string): string {
  return makeTextRedactor([credential])(error instanceof Error ? error.message : String(error))
    .replace(/\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b/gi, "[email]")
    .replace(/\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b/gi, "[id]")
    .replace(/\/(?:Users|home)\/[^/\s]+/g, "/home/[user]")
    .replace(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, "[jwt]")
    .replace(/((?:access_token|refresh_token|id_token|authorization|api_key)["'\s:=]+)[^\s,"'}]+/gi, "$1[redacted]")
    .slice(0, 4_096);
}

/** The live entry re-runs case 9 with a DUMMY-credential child, never with its live environment. */
export function offlineProof(file: string, prefix: string): unknown {
  const run = spawnSync(process.execPath, ["--import", "tsx", "--test", "--test-timeout=120000", file], {
    cwd: process.cwd(), env: { PATH: process.env.PATH, HOME: process.env.TMPDIR ?? "/tmp", TMPDIR: process.env.TMPDIR,
      GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", CODEX_M4_SRC: process.env.CODEX_M4_SRC },
    encoding: "utf8", timeout: 120_000, maxBuffer: 128 * 1024,
  });
  if (run.status !== 0) throw new Error("credential-free characterization preflight failed; run its test directly for diagnostics");
  const line = run.stdout.split("\n").find(v => v.startsWith(prefix));
  if (!line) throw new Error("characterization preflight emitted no evidence");
  return JSON.parse(line.slice(prefix.length)) as unknown;
}
