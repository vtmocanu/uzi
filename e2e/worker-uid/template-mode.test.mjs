// Copy this file and both Dockerfiles with their repository-relative layout into
// a disposable image inheriting the verified worker image. Run as root with a
// command-scoped container TMPDIR and the image's native PATH (including Nix).
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { lstatSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { isAbsolute, join } from "node:path";
import test from "node:test";

function run(command, args) {
  // Fixed argv only; each command has one attempt and a five-second deadline.
  execFileSync(command, args, { timeout: 5000, stdio: "pipe" });
}

function withFixture(check) {
  assert.equal(process.getuid(), 0, "run inside the supplemental root image build");
  const tmpdir = process.env.TMPDIR;
  assert.ok(tmpdir && isAbsolute(tmpdir), "set a command-scoped container TMPDIR");
  const directory = mkdtempSync(join(tmpdir, "template-mode-"));
  try {
    run("chmod", ["2755", directory]);
    assert.equal(lstatSync(directory).mode & 0o7777, 0o2755, "fixture starts setgid");
    check(directory);
  } finally {
    // Only this invocation's mkdtemp directory; no retries or sibling cleanup.
    rmSync(directory, { recursive: true, force: true });
  }
}

function normalize(directory, clearSetgid) {
  run("chown", ["root:root", directory]);
  run("chmod", ["0755", directory]);
  if (clearSetgid) run("chmod", ["g-s", directory]);
}

function assertLayout(directory, mode, label) {
  const stat = lstatSync(directory);
  assert.ok(stat.isDirectory() && !stat.isSymbolicLink(), label);
  assert.equal(stat.uid, 0, label + " uid");
  assert.equal(stat.gid, 0, label + " gid");
  assert.equal(stat.mode & 0o7777, mode, label + " mode");
}

test("old 0755 normalization preserves inherited setgid with image PATH chmod", { timeout: 30000 }, () => {
  withFixture((directory) => {
    normalize(directory, false);
    assertLayout(directory, 0o2755, "old normalization reproduces the blocker");
  });
});

for (const template of ["base", "jvm"]) {
  test(`${template} Dockerfile normalization clears inherited setgid`, { timeout: 30000 }, () => {
    const source = readFileSync(new URL(`../../agent/templates/${template}/Dockerfile`, import.meta.url), "utf8");
    const sequences = source.split("\n").filter((line) =>
      /^\s*RUN\b/.test(line) && line.includes("/app/templates"));
    assert.equal(sequences.length, 1, template + " must have exactly one normalization RUN");
    // Permit the old form so removing g-s fails on the resulting mode, rather
    // than a source-presence assertion. Never execute extracted source as code.
    const sequence = /^RUN chown root:root \/app\/templates && chmod 0755 \/app\/templates( && chmod g-s \/app\/templates)?$/.exec(sequences[0]);
    assert.ok(sequence, template + " has an unrecognized normalization sequence");
    withFixture((directory) => {
      normalize(directory, sequence[1] !== undefined);
      assertLayout(directory, 0o755, template + " candidate normalization");
    });
  });
}
