import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

const earlyFile = "000-unpinned-early.test.ts.mjs";
const survivingFiles = [
  "config.test.ts.mjs",
  "runner-held-publication-fallbacks.test.ts.mjs",
  "runner-midturn-checkpoint.test.ts.mjs",
  "runner-retained-episode.test.ts.mjs",
  "scheduler.test.ts.mjs",
  "unit-utils.test.ts.mjs",
  "worker.test.ts.mjs",
];

// M2's adapter seam: replace native sharding with selected fixture filenames here.
// The execution and membership oracle below must still observe real child tests.
function shardInvocation(files: readonly string[], shard: number): string[] {
  return [`--test-shard=${shard}/2`, ...files.toSorted()];
}

function writeFixture(directory: string, filename: string): void {
  writeFileSync(
    path.join(directory, filename),
    `import test from "node:test";\ntest(${JSON.stringify(`fixture:${filename}`)}, () => {});\n`,
  );
}

function observeMembership(directory: string, files: readonly string[]): Map<string, number> {
  const membership = new Map<string, number>();
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT; // Each child must start an independent node:test runner.
  // Exactly two sequential children per observation; a failed child blocks the
  // observation. Each child has a 30s process bound as well as Node's test cap.
  for (const shard of [1, 2]) {
    const args = [
      "--test",
      "--test-reporter=tap",
      "--test-timeout=120000",
      ...shardInvocation(files, shard),
    ];
    const child = spawnSync(process.execPath, args, {
      cwd: directory,
      env,
      encoding: "utf8",
      timeout: 30_000,
      maxBuffer: 1024 * 1024,
    });
    const diagnostic = `shard ${shard}: ${child.stdout}\n${child.stderr}`;
    assert.ifError(child.error);
    assert.equal(child.signal, null, diagnostic);
    assert.equal(child.status, 0, diagnostic);
    assert.match(child.stdout, /^# fail 0$/m, diagnostic);
    assert.match(child.stdout, /^# cancelled 0$/m, diagnostic);

    const observed = [...child.stdout.matchAll(/^ok \d+ - fixture:(.+)$/gm)]
      .map((match) => match[1]!);
    assert.ok(observed.length > 0, `shard ${shard} must execute fixtures`);
    for (const filename of observed) {
      assert.ok(files.includes(filename), `unexpected fixture: ${filename}`);
      assert.ok(!membership.has(filename), `shards must be disjoint: ${filename}`);
      membership.set(filename, shard);
    }
  }
  assert.deepEqual(
    [...membership.keys()].sort(),
    [...files].sort(),
    "shard union must cover the entire fixture inventory",
  );
  return membership;
}

// Separate top-level subtests ensure deletion is exercised even when addition fails.
for (const mutation of ["add", "delete"] as const) {
  test(`unpinned early-file ${mutation} preserves every surviving file membership`, () => {
    const directory = mkdtempSync(path.join(tmpdir(), "uzi-unit-test-shards-"));
    try {
      const beforeFiles = mutation === "add"
        ? [...survivingFiles]
        : [earlyFile, ...survivingFiles];
      for (const filename of beforeFiles) writeFixture(directory, filename);
      const before = observeMembership(directory, beforeFiles);

      if (mutation === "add") writeFixture(directory, earlyFile);
      else unlinkSync(path.join(directory, earlyFile));
      const afterFiles = mutation === "add"
        ? [earlyFile, ...survivingFiles]
        : [...survivingFiles];
      const after = observeMembership(directory, afterFiles);

      assert.deepEqual(
        survivingFiles.map((filename) => [filename, after.get(filename)]),
        survivingFiles.map((filename) => [filename, before.get(filename)]),
        `unpinned early-file ${mutation} must preserve every surviving file membership`,
      );
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });
}
