// PRD #1287 D7 point 3 — the maintainer/macOS entry that actually RUNS the strict Codex M4 P suite
// inside the pinned Linux container. It wires the real docker/child_process seams into
// executeMacosLinuxRun. Missing Docker / unsupported architecture / unexpected /etc/codex fail
// LOUDLY (assertPrerequisites throws → non-zero exit). Invoked by `task test:codex-m4:macos` and the
// darwin leg of `task test:codex-m4`; never imported (the orchestrator + plan builder are the
// unit-tested surface).
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import path from "node:path";

import { executeMacosLinuxRun } from "./macos-linux-runner.js";

function whichDocker(): string | undefined {
  const r = spawnSync("docker", ["--version"], { stdio: "ignore" });
  return r.status === 0 ? "docker" : undefined;
}

// The Taskfile runs this with `dir: agent`, so cwd is the agent package root.
const agentDir = process.cwd();
const e2eDir = path.resolve(process.cwd(), "../e2e");

try {
  executeMacosLinuxRun({
    whichDocker,
    nodeArch: process.arch,
    etcCodexPresent: existsSync("/etc/codex"),
    agentDir,
    e2eDir,
    runStage: (argv) => {
      const r = spawnSync(argv[0], argv.slice(1), { stdio: "inherit" });
      if (r.error) throw r.error;
      return r.status ?? 1;
    },
    log: (m) => console.error(m),
  });
} catch (e) {
  console.error(`[codex-m4 macos] FAILED: ${e instanceof Error ? e.message : String(e)}`);
  process.exit(1);
}
