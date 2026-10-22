#!/usr/bin/env node
// Execute the workflow's actual startup shell with fake Docker/sleep, no daemon.
import assert from "node:assert/strict";
import { readFileSync, writeFileSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
const root = fileURLToPath(new URL("../", import.meta.url));
const workflow = readFileSync(path.join(root, ".github/workflows/ci.yml"), "utf8");
const job = workflow.match(/^  test-api-store-it:\n([\s\S]*?)(?=^  [a-zA-Z0-9_-]+:)/m)?.[1];
assert.ok(job, "LiveDB job must exist");
assert.doesNotMatch(job, /^    services:/m, "services pull before fallback steps can run");
const image = job.match(/^      UZI_CI_PG_IMAGE: (\S+)$/m)?.[1];
assert.ok(image, "pin must be present");
const block = job.match(/      - name: Start pinned Postgres[^\n]*\n        run: \|\n((?:          [^\n]*\n)+)/)?.[1];
assert.ok(block, "database start block must exist");
const script = block.replace(/^          /gm, "");
const temp = mkdtempSync(path.join(tmpdir(), "ci-postgres-test-"));
try {
  mkdirSync(path.join(temp, "bin"));
  writeFileSync(path.join(temp, "bin/docker"), `#!/bin/sh
printf '%s\\n' "$*" >> "$PG_TEST_LOG"
case "$1 $2" in
  'pull mirror.gcr.io/'*) exit 1 ;;
  'pull docker.io/'*) echo 'pull progress'; exit "$PG_TEST_HUB_RC" ;;
  'run -d') echo 'test-container-id'; exit 0 ;;
  'exec ci-pg') exit "$PG_TEST_READY_RC" ;;
  'logs ci-pg') echo 'postgres startup diagnostics'; exit 0 ;;
  *) echo "unexpected docker: $*" >&2; exit 99 ;;
esac
`, { mode: 0o755 });
  writeFileSync(path.join(temp, "bin/sleep"), `#!/bin/sh
printf 'sleep %s\\n' "$*" >> "$PG_TEST_LOG"
`, { mode: 0o755 });
  for (const scenario of ["ready", "unready", "pull-failed"]) {
    const ready = scenario === "ready";
    const log = path.join(temp, `${scenario}.log`);
    const result = spawnSync("bash", ["-euo", "pipefail", "-c", script], {
      cwd: root, encoding: "utf8", timeout: 10000,
      env: { ...process.env, PATH: `${temp}/bin:${process.env.PATH}`,
        UZI_CI_PG_IMAGE: image, PG_TEST_LOG: log, PG_TEST_READY_RC: ready ? "0" : "1",
        PG_TEST_HUB_RC: scenario === "pull-failed" ? "1" : "0" },
    });
    assert.ifError(result.error);
    assert.equal(result.status, ready ? 0 : 1, result.stderr);
    const calls = readFileSync(log, "utf8").trim().split("\n");
    const hubRef = image.replace("mirror.gcr.io/", "docker.io/");
    if (scenario === "pull-failed") {
      assert.deepEqual(calls, [
        `pull ${image}`, "sleep 5", `pull ${image}`, "sleep 15", `pull ${image}`,
        `pull ${hubRef}`, "sleep 5", `pull ${hubRef}`, "sleep 15", `pull ${hubRef}`,
      ], "pull exhaustion must prevent database startup");
      continue;
    }
    assert.deepEqual(calls.slice(0, 6), [
      `pull ${image}`, "sleep 5", `pull ${image}`, "sleep 15", `pull ${image}`, `pull ${hubRef}`,
    ]);
    assert.equal(calls[6], `run -d --pull=never --name ci-pg -p 127.0.0.1:5432:5432 -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi -e POSTGRES_PASSWORD=uzi-ci ${hubRef}`);
    assert.equal(calls.filter((s) => s.startsWith("exec ci-pg pg_isready ")).length, ready ? 1 : 10);
    assert.equal(calls.filter((s) => s === "sleep 5").length, ready ? 1 : 11);
    assert.equal(calls.includes("logs ci-pg"), !ready);
    if (!ready) assert.match(result.stdout, /postgres startup diagnostics/);
  }
} finally {
  rmSync(temp, { recursive: true, force: true });
}
console.log("CI Postgres startup contract: PASS (ready, unready and pull exhaustion)");
