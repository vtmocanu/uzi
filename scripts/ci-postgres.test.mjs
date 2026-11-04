#!/usr/bin/env node
// Execute the workflow's actual Postgres pull shell with fake Docker/sleep, no daemon,
// and pin the single-LiveDB-execution wiring of ci.yml's two required contexts.
import assert from "node:assert/strict";
import { readFileSync, writeFileSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
const root = fileURLToPath(new URL("../", import.meta.url));
const workflow = readFileSync(path.join(root, ".github/workflows/ci.yml"), "utf8");
const jobBody = (name) => workflow.match(new RegExp(`^  ${name}:\\n([\\s\\S]*?)(?=^  [a-zA-Z0-9_-]+:|^  # -{10})`, "m"))?.[1];

// The producer owns the one execution: pinned digest, pull step, and the canonical task.
const job = jobBody("test-codex-refresh-lostreply-e2e");
assert.ok(job, "single LiveDB producer job must exist");
assert.doesNotMatch(job, /^    services:/m, "services pull before fallback steps can run");
const image = job.match(/^      UZI_CI_PG_IMAGE: (\S+)$/m)?.[1];
assert.ok(image, "pin must be present");
assert.match(image, /^mirror\.gcr\.io\/library\/postgres:[^@\s]+@sha256:[0-9a-f]{64}$/, "producer must use the pinned digest");
assert.doesNotMatch(job, /docker-pull-fallback\.sh postgres:/, "no floating pre-pull may replace the pinned digest");
assert.match(job, /^    timeout-minutes: 75$/m, "headroom for one full sweep");
assert.match(job, /^        run: task test:codex-refresh-lostreply-e2e$/m, "producer runs the canonical recipe once");
const code = workflow.split("\n").filter((l) => !/^\s*#/.test(l)).join("\n");
assert.equal((code.match(/task test:codex-refresh-lostreply-e2e/g) ?? []).length, 1, "recipe appears once in ci.yml");
assert.equal((code.match(/run-store-it\.sh|-run 'LiveDB\$'|livedb\.log|livedb-skip-guard/g) ?? []).length, 0,
  "no job may carry its own LiveDB sweep; the recipe owns it");
assert.match(job, /^          UZI_STORE_IT_PG_IMAGE: \$\{\{ steps\.pg\.outputs\.ref \}\}$/m, "database image comes from the pulled digest ref");
const block = job.match(/      - name: Pull the pinned Postgres[^\n]*\n        id: pg\n        run: \|\n((?:          [^\n]*\n)+)/)?.[1];
assert.ok(block, "pinned pull block must exist");
const script = block.replace(/^          /gm, "");

// The aggregation job runs nothing but the producer-result check.
const agg = jobBody("test-api-store-it");
assert.ok(agg, "required context test-api-store-it must exist");
assert.match(agg, /^    needs: \[changes, test-codex-refresh-lostreply-e2e\]$/m, "aggregator needs the producer");
assert.doesNotMatch(agg, /^      UZI_CI_PG_IMAGE:|go test|docker /m, "aggregator must not run its own sweep or database");
const aggRun = agg.match(/        run: \|\n((?:          [^\n]*\n)+)/)?.[1]?.replace(/^          /gm, "");
assert.ok(aggRun, "aggregator check block must exist");
assert.match(agg, /PRODUCER_RESULT: \$\{\{ needs\['test-codex-refresh-lostreply-e2e'\]\.result \}\}/);
for (const [result, want] of [["success", 0], ["failure", 1], ["cancelled", 1], ["skipped", 1], ["", 1]]) {
  const r = spawnSync("bash", ["-e", "-c", aggRun], { encoding: "utf8", env: { ...process.env, PRODUCER_RESULT: result } });
  assert.equal(r.status, want, `aggregator with producer result ${JSON.stringify(result)}`);
}

const temp = mkdtempSync(path.join(tmpdir(), "ci-postgres-test-"));
try {
  mkdirSync(path.join(temp, "bin"));
  writeFileSync(path.join(temp, "bin/docker"), `#!/bin/sh
printf '%s\\n' "$*" >> "$PG_TEST_LOG"
case "$1 $2" in
  'pull mirror.gcr.io/'*) exit 1 ;;
  'pull docker.io/'*) echo 'pull progress'; exit "$PG_TEST_HUB_RC" ;;
  *) echo "unexpected docker: $*" >&2; exit 99 ;;
esac
`, { mode: 0o755 });
  writeFileSync(path.join(temp, "bin/sleep"), `#!/bin/sh
printf 'sleep %s\\n' "$*" >> "$PG_TEST_LOG"
`, { mode: 0o755 });
  for (const scenario of ["hub-fallback", "pull-failed"]) {
    const log = path.join(temp, `${scenario}.log`);
    const out = path.join(temp, `${scenario}.out`);
    writeFileSync(out, "");
    const result = spawnSync("bash", ["-euo", "pipefail", "-c", script], {
      cwd: root, encoding: "utf8", timeout: 10000,
      env: { ...process.env, PATH: `${temp}/bin:${process.env.PATH}`,
        UZI_CI_PG_IMAGE: image, PG_TEST_LOG: log, GITHUB_OUTPUT: out,
        PG_TEST_HUB_RC: scenario === "pull-failed" ? "1" : "0" },
    });
    assert.ifError(result.error);
    const calls = readFileSync(log, "utf8").trim().split("\n");
    const hubRef = image.replace("mirror.gcr.io/", "docker.io/");
    if (scenario === "pull-failed") {
      assert.equal(result.status, 1, result.stderr);
      assert.deepEqual(calls, [
        `pull ${image}`, "sleep 5", `pull ${image}`, "sleep 15", `pull ${image}`,
        `pull ${hubRef}`, "sleep 5", `pull ${hubRef}`, "sleep 15", `pull ${hubRef}`,
      ], "pull exhaustion must prevent the sweep from starting");
      assert.equal(readFileSync(out, "utf8"), "", "no image ref may be published after pull exhaustion");
      continue;
    }
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(calls, [
      `pull ${image}`, "sleep 5", `pull ${image}`, "sleep 15", `pull ${image}`, `pull ${hubRef}`,
    ]);
    assert.equal(readFileSync(out, "utf8"), `ref=${hubRef}\n`, "the digest ref actually pulled feeds run-store-it.sh");
  }
} finally {
  rmSync(temp, { recursive: true, force: true });
}
console.log("CI Postgres contract: PASS (single execution, pinned digest, hub fallback, pull exhaustion, aggregation)");
