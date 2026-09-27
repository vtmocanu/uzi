import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// issue #1783 (R4) — every place the worker crosses into a runner/runner-cmd uid is listed here
// with its disposition toward the run-quiescence reaper. The reaper attributes runner-uid
// processes by env: an AGENT process carries the attempt marker (only the CLI spawn), and every
// WORKER-initiated spawn carries the worker mark (UZI_WORKER_SPAWN) so a reap never kills the
// worker's own op. A new, unlisted `runnerCommand(` / `runnerSpawn(` / `commandRootCommand(` site
// (or a bare reference that is later called) fails this test until someone decides which side of
// that line it is on and records it below. A listed site that disappears fails it too.

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
const NAMES = ["runnerCommand", "runnerSpawn", "commandRootCommand"] as const;

/** `<file>#<name>()` for a call, `<file>#<name>&` for a bare reference → expected count + why. */
const ALLOWLIST: Record<string, { count: number; disposition: string }> = {
  "sdk-spawn.ts#runnerSpawn()": { count: 1, disposition: "the agent CLI: carries the attempt marker (buildSdkEnv), never the worker mark" },
  "runner-uid.ts#runnerCommand()": { count: 1, disposition: "inside runnerSpawn: the caller's env decides (see sdk-spawn.ts)" },
  "executor.ts#runnerCommand()": { count: 1, disposition: "stub executor git: workerSpawnEnv; runs inside the stub run(), awaited before it returns" },
  "git.ts#runnerCommand()": { count: 1, disposition: "runGitAsRunner: workerSpawnEnv" },
  "js-deps.ts#runnerCommand()": { count: 1, disposition: "JS deps install: worker-marked in execInstall's spawn" },
  "provision.ts#runnerCommand()": { count: 2, disposition: "devbox/nix run + PATH probe: worker-marked in the default run functions (pre-turn)" },
  "provision.ts#commandRootCommand()": { count: 1, disposition: "PATH probe as runner-cmd (uid 10003, never scanned): worker-marked anyway" },
  "self-improve.ts#runnerCommand()": { count: 1, disposition: "finalize-time check: workerSpawnEnv" },
  "tick-spawner.ts#runnerCommand()": { count: 3, disposition: "tick child + kill/probe helpers: workerSpawnEnv" },
  "rmtree.ts#runnerCommand&": { count: 2, disposition: "purge helper passes: workerSpawnEnv on the minimal env" },
  "rmtree.ts#commandRootCommand&": { count: 1, disposition: "purge helper pass as runner-cmd: workerSpawnEnv" },
  "run-quiescence.ts#runnerCommand()": { count: 1, disposition: "the quiescence helper: workerSpawnEnv, never an attempt marker" },
  "codex/launcher.ts#runnerCommand()": { count: 1, disposition: "Codex root supervisor: workerSpawnEnv(replacedEnv)" },
  "codex/launcher.ts#commandRootCommand()": { count: 4, disposition: "Codex command/effect supervisors, standalone modes, kill: workerSpawnEnv" },
  "codex/launcher.ts#runnerCommand&": { count: 2, disposition: "owned-tree create/remove wrap: workerSpawnEnv on the inert env" },
  "codex/launcher.ts#commandRootCommand&": { count: 2, disposition: "owned-tree create/remove wrap: workerSpawnEnv on the inert env" },
};

function listTs(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return listTs(p);
    return e.isFile() && e.name.endsWith(".ts") ? [p] : [];
  });
}

/** Drop imports, comment lines and the three definitions, keeping code. */
function codeOf(text: string): string {
  return text
    .replace(/^import\s[\s\S]*?\sfrom\s+"[^"]+";/gm, "")
    .split("\n")
    .filter((l) => !/^\s*(\/\/|\/\*|\*)/.test(l))
    .map((l) => l.replace(/\s\/\/\s.*$/, ""))
    .filter((l) => !/export function (runnerCommand|runnerSpawn|commandRootCommand)\(/.test(l))
    .join("\n");
}

function found(): Record<string, number> {
  const out: Record<string, number> = {};
  for (const file of listTs(SRC)) {
    const rel = path.relative(SRC, file).split(path.sep).join("/");
    const code = codeOf(fs.readFileSync(file, "utf8"));
    for (const name of NAMES) {
      for (const m of code.matchAll(new RegExp(`\\b${name}\\b(\\s*\\()?`, "g"))) {
        const key = `${rel}#${name}${m[1] ? "()" : "&"}`;
        out[key] = (out[key] ?? 0) + 1;
      }
    }
  }
  return out;
}

describe("runner-uid spawn sites (issue #1783 R4)", () => {
  it("every site is listed with its disposition, and every listed site still exists", () => {
    const actual = found();
    const expected = Object.fromEntries(Object.entries(ALLOWLIST).map(([k, v]) => [k, v.count]));
    assert.deepEqual(actual, expected);
    for (const v of Object.values(ALLOWLIST)) assert.ok(v.disposition.length > 0);
  });

  it("the source scan actually sees calls (not vacuous)", () => {
    assert.ok((found()["tick-spawner.ts#runnerCommand()"] ?? 0) >= 1);
  });
});
