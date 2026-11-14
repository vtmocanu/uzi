#!/usr/bin/env node
// Execute the workflow's actual Postgres pull shell with fake Docker/sleep, no daemon,
// and pin the two-producer LiveDB wiring (#4232) behind the required test-api-store-it.
import assert from "node:assert/strict";
import { readFileSync, writeFileSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
const root = fileURLToPath(new URL("../", import.meta.url));
const workflow = readFileSync(path.join(root, ".github/workflows/ci.yml"), "utf8");
const jobBody = (name) => workflow.match(new RegExp(`^  ${name}:\\n([\\s\\S]*?)(?=^  [a-zA-Z0-9_-]+:|^  # -{10})`, "m"))?.[1];

// One workflow-level pin feeds both producers; no job carries its own.
assert.equal((workflow.match(/^\s*UZI_CI_PG_IMAGE:/gm) ?? []).length, 1, "exactly one Postgres pin in ci.yml");
const image = workflow.match(/^  UZI_CI_PG_IMAGE: (\S+)$/m)?.[1];
assert.ok(image, "workflow-level pin must be present");
assert.match(image, /^mirror\.gcr\.io\/library\/postgres:[^@\s]+@sha256:[0-9a-f]{64}$/, "producers use the pinned digest");
const code = workflow.split("\n").filter((l) => !/^\s*#/.test(l)).join("\n");
assert.equal((code.match(/run-store-it\.sh|-run 'LiveDB\$'|livedb\.log|livedb-skip-guard/g) ?? []).length, 0,
  "no job may carry its own LiveDB sweep; the recipes own it");

// Each producer owns its share of the one sweep: pull step, pulled ref, canonical task once.
const producers = [
  { name: "test-livedb-workersvc", task: "test:livedb-workersvc", timeout: 60, group: null },
  { name: "test-codex-refresh-lostreply-e2e", task: "test:codex-refresh-lostreply-e2e", timeout: 75, group: "rest" },
];
const blocks = [];
for (const p of producers) {
  const job = jobBody(p.name);
  assert.ok(job, `LiveDB producer ${p.name} must exist`);
  assert.doesNotMatch(job, /^    services:/m, "services pull before fallback steps can run");
  assert.doesNotMatch(job, /docker-pull-fallback\.sh postgres:/, "no floating pre-pull may replace the pinned digest");
  assert.match(job, new RegExp(`^    timeout-minutes: ${p.timeout}$`, "m"), `${p.name} timeout headroom`);
  assert.match(job, new RegExp(`^        run: task ${p.task}$`, "m"), `${p.name} runs its canonical recipe`);
  assert.equal((code.match(new RegExp(`task ${p.task}\\b`, "g")) ?? []).length, 1, `${p.task} appears once in ci.yml`);
  assert.match(job, /^          UZI_STORE_IT_PG_IMAGE: \$\{\{ steps\.pg\.outputs\.ref \}\}$/m, "database image comes from the pulled digest ref");
  const groups = job.match(/^          UZI_STORE_IT_GROUP: (\S+)$/gm) ?? [];
  assert.deepEqual(groups, p.group ? [`          UZI_STORE_IT_GROUP: ${p.group}`] : [], `${p.name} sweep group`);
  const block = job.match(/      - name: Pull the pinned Postgres[^\n]*\n        id: pg\n        run: \|\n((?:          [^\n]*\n)+)/)?.[1];
  assert.ok(block, `${p.name} pinned pull block must exist`);
  blocks.push(block);
}
assert.equal(blocks[0], blocks[1], "both producers pull Postgres identically");
const script = blocks[0].replace(/^          /gm, "");
const workersvcTask = readFileSync(path.join(root, "Taskfile.yml"), "utf8").match(/^  test:livedb-workersvc:\n((?:    [^\n]*\n|\n)+?)(?=^  \S)/m)?.[1];
assert.match(workersvcTask ?? "", /UZI_STORE_IT_GROUP=workersvc TMPDIR=\S+ \.\/e2e\/run-store-it\.sh/, "workersvc recipe selects its group");

// The aggregation job runs nothing but the two producer-result checks.
const agg = jobBody("test-api-store-it");
assert.ok(agg, "required context test-api-store-it must exist");
assert.match(agg, /^    needs: \[changes, test-livedb-workersvc, test-codex-refresh-lostreply-e2e\]$/m, "aggregator needs both producers");
assert.doesNotMatch(agg, /^      UZI_CI_PG_IMAGE:|go test|docker /m, "aggregator must not run its own sweep or database");
const aggRun = agg.match(/        run: \|\n((?:          [^\n]*\n)+)/)?.[1]?.replace(/^          /gm, "");
assert.ok(aggRun, "aggregator check block must exist");
assert.match(agg, /WORKERSVC_RESULT: \$\{\{ needs\['test-livedb-workersvc'\]\.result \}\}/);
assert.match(agg, /REST_RESULT: \$\{\{ needs\['test-codex-refresh-lostreply-e2e'\]\.result \}\}/);
const results = ["success", "failure", "cancelled", "skipped", ""];
for (const a of results) {
  for (const b of results) {
    const r = spawnSync("bash", ["-e", "-c", aggRun], { encoding: "utf8", env: { ...process.env, WORKERSVC_RESULT: a, REST_RESULT: b } });
    assert.equal(r.status, a === "success" && b === "success" ? 0 : 1, `aggregator with producer results ${JSON.stringify([a, b])}`);
  }
}

const temp = mkdtempSync(path.join(tmpdir(), "ci-postgres-test-"));
try {
  mkdirSync(path.join(temp, "bin"));
  // Fake Docker with a local-image table: pulls from the mirror miss, Docker Hub hits
  // and records the pulled ref; `image inspect` only hits a ref that was tagged locally
  // (a digest ref deliberately misses, the case that made run-store-it.sh pull raw).
  writeFileSync(path.join(temp, "bin/docker"), `#!/bin/sh
printf '%s\\n' "$*" >> "$PG_TEST_LOG"
case "$1 $2" in
  'pull mirror.gcr.io/'*) exit 1 ;;
  'pull docker.io/'*) echo 'pull progress'; [ "$PG_TEST_HUB_RC" = 0 ] && echo "$2" >> "$PG_TEST_PULLED"; exit "$PG_TEST_HUB_RC" ;;
  'tag '*) grep -Fxq "$2" "$PG_TEST_PULLED" || exit 1; echo "$3" >> "$PG_TEST_LOCAL"; exit 0 ;;
  'image inspect') grep -Fxq "$3" "$PG_TEST_LOCAL" || exit 1; exit 0 ;;
  'run -d') echo 'test-container-id'; exit 0 ;;
  'exec '*|'rm '*) exit 0 ;;
  *) echo "unexpected docker: $*" >&2; exit 99 ;;
esac
`, { mode: 0o755 });
  writeFileSync(path.join(temp, "bin/go"), "#!/bin/sh\necho 'fake go: sweep not run'\nexit 1\n", { mode: 0o755 });
  writeFileSync(path.join(temp, "bin/sleep"), `#!/bin/sh
printf 'sleep %s\\n' "$*" >> "$PG_TEST_LOG"
`, { mode: 0o755 });
  for (const scenario of ["hub-fallback", "pull-failed"]) {
    const log = path.join(temp, `${scenario}.log`);
    const out = path.join(temp, `${scenario}.out`);
    writeFileSync(out, "");
    writeFileSync(path.join(temp, `${scenario}.pulled`), "");
    writeFileSync(path.join(temp, `${scenario}.local`), "");
    const result = spawnSync("bash", ["-euo", "pipefail", "-c", script], {
      cwd: root, encoding: "utf8", timeout: 10000,
      env: { ...process.env, PATH: `${temp}/bin:${process.env.PATH}`,
        UZI_CI_PG_IMAGE: image, PG_TEST_LOG: log, GITHUB_OUTPUT: out,
        PG_TEST_PULLED: path.join(temp, `${scenario}.pulled`), PG_TEST_LOCAL: path.join(temp, `${scenario}.local`),
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
      `tag ${hubRef} uzi-ci-pg:pinned`, "image inspect uzi-ci-pg:pinned",
    ]);
    const ref = readFileSync(out, "utf8").trim().replace(/^ref=/, "");
    assert.equal(ref, "uzi-ci-pg:pinned", "step publishes the local handle of the pinned digest image");
    assert.ok(calls.includes(`tag ${hubRef} uzi-ci-pg:pinned`), "handle is tagged from the digest actually pulled");
    // End to end through the real run-store-it.sh image check: inspect hits the local
    // handle, so no registry pull happens after the fallback step, and the database
    // starts from that handle. The fake go fails the sweep, which ends the script there.
    const before = calls.length;
    const sweep = spawnSync("bash", [path.join(root, "e2e/run-store-it.sh")], {
      cwd: root, encoding: "utf8", timeout: 20000,
      env: { ...process.env, PATH: `${temp}/bin:${process.env.PATH}`, TMPDIR: temp,
        UZI_STORE_IT_PG_IMAGE: ref, PG_TEST_LOG: log, PG_TEST_PULLED: path.join(temp, `${scenario}.pulled`),
        PG_TEST_LOCAL: path.join(temp, `${scenario}.local`), PG_TEST_HUB_RC: "0" },
    });
    assert.ifError(sweep.error);
    assert.notEqual(sweep.status, 0, "fake go must stop the script at the sweep");
    const after = readFileSync(log, "utf8").trim().split("\n").slice(before);
    assert.equal(after.filter((c) => c.startsWith("pull ")).length, 0, "no second registry pull");
    assert.ok(after.some((c) => c.startsWith("run -d ") && c.endsWith(" uzi-ci-pg:pinned")), "database starts from the pinned handle");
    // Control: the raw digest ref misses inspect, which is why the handle exists.
    assert.equal(spawnSync("docker", ["image", "inspect", hubRef], { env: { ...process.env, PATH: `${temp}/bin:${process.env.PATH}`, PG_TEST_LOG: log, PG_TEST_LOCAL: path.join(temp, `${scenario}.local`) } }).status, 1);
  }
} finally {
  rmSync(temp, { recursive: true, force: true });
}
console.log("CI Postgres contract: PASS (two producers, one pinned digest, hub fallback, pull exhaustion, aggregation)");
