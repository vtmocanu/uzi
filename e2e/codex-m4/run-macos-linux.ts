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
  // The per-invocation host evidence file (unique-per-run, set by `task test:codex-m4`). The
  // container appends its codex/P evidence to exactly this file (its directory is the .evidence
  // mount, its basename the container CODEX_M4_EVIDENCE), so the native U leg + run-completeness
  // read the SAME file this leg wrote. HARD-FAIL loudly when it is unset rather than silently
  // falling back to a shared current.jsonl two concurrent runs would clobber (matches the runner's
  // never-a-silent-fallback prerequisite philosophy).
  const evidenceHostFile = process.env.CODEX_M4_EVIDENCE;
  if (evidenceHostFile === undefined || evidenceHostFile.trim().length === 0) {
    throw new Error(
      "macOS Linux-container runner requires CODEX_M4_EVIDENCE (the per-invocation host evidence file); none was set",
    );
  }
  executeMacosLinuxRun({
    whichDocker,
    nodeArch: process.arch,
    etcCodexPresent: existsSync("/etc/codex"),
    agentDir,
    e2eDir,
    evidenceHostFile,
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
