import { it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { copyFileSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readlinkSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { EXPLICIT_PIN_SITES, planPinSync, readCodexLock, scannedPinFiles, syncCodexPins } from "./codex-pins.js";

// #1565: `task codex-pins:sync` rewrites every registered Codex pin from the lock, and the
// coordination and parity tests stay the fail-closed check of the result.
const root = fileURLToPath(new URL("../../", import.meta.url));
const LOCK = "agent/codex/codex-package.lock";
const realLock = readFileSync(join(root, LOCK), "utf8");
const current = readCodexLock(realLock);
const NEXT = { version: "9.9.9", amd64: "a".repeat(64), arm64: "b".repeat(64) };
// Never write a pin prefix next to a literal version in this file: the live-pin scan would
// count it as a stale pin, and the sync would rewrite it.
const INSTALL_ROOT = "/opt/uzi-codex/";

function bumped(lock: string): string {
  return lock.split(current.version).join(NEXT.version)
    .replace(current.digests.amd64, NEXT.amd64).replace(current.digests.arm64, NEXT.arm64);
}
function git(cwd: string, ...args: string[]): string {
  const result = spawnSync("git", ["-c", "core.fsmonitor=false", ...args], { cwd, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, `git ${args.join(" ")}: ${result.stderr}`);
  return result.stdout;
}
function tempRepo(): string {
  const dir = mkdtempSync(join(tmpdir(), "codex-pins-"));
  git(dir, "init", "-q");
  return dir;
}
function put(dir: string, file: string, text: string): void {
  mkdirSync(dirname(join(dir, file)), { recursive: true });
  writeFileSync(join(dir, file), text);
}
// A nested `node --test` must not inherit the parent runner's child-reporting context.
function childEnv(): NodeJS.ProcessEnv {
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT;
  return env;
}

it("the tree is in sync with its lock: the sync plans no change", () => {
  assert.deepEqual([...planPinSync(root, current).keys()], []);
});

it("a bumped lock fails the coordination and parity checks until the sync runs, then passes them", (t) => {
  // Phase timings go to the test diagnostics: CI time is review evidence (#1565).
  let mark = Date.now();
  const phase = (name: string): void => { t.diagnostic(`${name}: ${Date.now() - mark} ms`); mark = Date.now(); };
  // Copy what the checks read into a scratch repository: agent/ and e2e/codex-m4/ (the
  // imported sources and the AST-checked M4 files) plus every file holding a live pin, from
  // the working tree (tracked plus untracked, non-ignored files). The scan in the copy then
  // sees exactly the real pin files.
  const dir = tempRepo();
  try {
    const pinFiles = new Set([LOCK, ...scannedPinFiles(root), ...EXPLICIT_PIN_SITES.map((site) => site.file)]);
    const listed = git(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "agent", "e2e/codex-m4")
      .split("\0").filter(Boolean);
    for (const file of new Set([...listed, ...pinFiles])) {
      let stat;
      try { stat = lstatSync(join(root, file)); } catch { continue; } // tracked but deleted locally
      mkdirSync(dirname(join(dir, file)), { recursive: true });
      if (stat.isSymbolicLink()) symlinkSync(readlinkSync(join(root, file)), join(dir, file));
      else copyFileSync(join(root, file), join(dir, file));
    }
    git(dir, "add", "-A");
    symlinkSync(join(root, "agent/node_modules"), join(dir, "agent/node_modules"));
    put(dir, LOCK, bumped(realLock));
    phase("copy");
    const checks = (): ReturnType<typeof spawnSync> => spawnSync(process.execPath, [
      "--import", "tsx", "--import", "./test/setup/hermetic-proc.ts", "--test", "--test-timeout=120000",
      "test/codex-runtime-coordination.test.ts", "test/codex-pin-parity.test.ts",
    ], { cwd: join(dir, "agent"), env: childEnv(), encoding: "utf8", maxBuffer: 16 * 1024 * 1024 });

    const before = checks();
    phase("checks before sync");
    assert.notEqual(before.status, 0, "a lock-only bump must fail the checks");
    assert.match(String(before.stdout), /live Codex pins must match CODEX_VERSION=9\.9\.9/);

    const recordedTape = join(dir, "agent/test/fixtures/codex-plan-output-schema-0.164.0.json");
    const recordedBefore = readFileSync(recordedTape);
    const originals = new Map<string, string>();
    for (const file of new Set([...scannedPinFiles(dir), ...EXPLICIT_PIN_SITES.map((site) => site.file)])) {
      originals.set(file, readFileSync(join(dir, file), "utf8"));
    }
    const cli = spawnSync(process.execPath, ["--import", "tsx", "test/codex-pins-sync.cli.ts"],
      { cwd: join(dir, "agent"), env: childEnv(), encoding: "utf8" });
    assert.equal(cli.status, 0, `sync failed: ${cli.stderr}`);
    assert.match(cli.stdout, /codex-pins:sync rewrote \d+ file\(s\)/);
    phase("sync");
    assert.ok(readFileSync(recordedTape).equals(recordedBefore),
      "pin sync must preserve recorded evidence byte-for-byte, not relabel its runtime");

    // Only pin tokens changed: each changed line is its original with the version and the
    // digests substituted, and no file outside the registered sites changed.
    const changed = git(dir, "diff", "--name-only").split("\n").filter(Boolean).sort();
    assert.deepEqual(changed, [...new Set([LOCK, ...originals.keys()])].filter((file) =>
      file === LOCK || readFileSync(join(dir, file), "utf8") !== originals.get(file)).sort());
    for (const [file, before] of originals) {
      const oldLines = before.split("\n");
      const newLines = readFileSync(join(dir, file), "utf8").split("\n");
      assert.equal(newLines.length, oldLines.length, `${file}: line count changed`);
      oldLines.forEach((line, index) => {
        if (line === newLines[index]) return;
        const expected = line.split(current.version).join(NEXT.version)
          .replace(current.digests.amd64, NEXT.amd64).replace(current.digests.arm64, NEXT.arm64);
        assert.equal(newLines[index], expected, `${file}:${index + 1} changed beyond its pin`);
      });
    }

    const after = checks();
    phase("checks after sync");
    assert.equal(after.status, 0, `the synced tree must pass the checks:\n${String(after.stdout).slice(-4000)}`);
    assert.deepEqual([...planPinSync(dir, readCodexLock(bumped(realLock))).keys()], [], "a second sync changes nothing");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// A small tree holding every explicit site plus one scanned pin, for the fail-closed cases.
function smallTree(edit?: (files: Record<string, string>) => void): string {
  const files: Record<string, string> = { [LOCK]: realLock, "docs/pin.md": `binary at ${INSTALL_ROOT}${current.version}/bin/codex\n` };
  for (const site of EXPLICIT_PIN_SITES) files[site.file] = readFileSync(join(root, site.file), "utf8");
  edit?.(files);
  const dir = tempRepo();
  for (const [file, text] of Object.entries(files)) put(dir, file, text);
  git(dir, "add", "-A");
  return dir;
}
function snapshot(dir: string): string[] {
  return git(dir, "ls-files").split("\n").filter(Boolean).map((file) => `${file}\0${readFileSync(join(dir, file), "utf8")}`);
}
function refuses(name: string, pattern: RegExp, edit: (files: Record<string, string>) => void): void {
  it(`refuses, writing nothing, when ${name}`, () => {
    const dir = smallTree(edit);
    try {
      const before = snapshot(dir);
      assert.throws(() => syncCodexPins(dir), pattern);
      assert.deepEqual(snapshot(dir), before);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
}

refuses("an explicit site has no match", /token-resume\.test\.ts: expected 1 match\(es\).*found 0/, (files) => {
  files["e2e/codex-m4/token-resume.test.ts"] = files["e2e/codex-m4/token-resume.test.ts"]!.replace(/assertBinaryVersion\(bin, "[^"]+"\);/, "");
  files[LOCK] = bumped(realLock);
});
refuses("an explicit site matches twice", /codex-runtime-probe\.ts: expected 1 match\(es\).*found 2/, (files) => {
  const probe = files["agent/src/codex/codex-runtime-probe.ts"]!;
  files["agent/src/codex/codex-runtime-probe.ts"] = probe + probe.slice(probe.indexOf("\n  version: "), probe.indexOf("\n", probe.indexOf("\n  version: ") + 1) + 1);
  files[LOCK] = bumped(realLock);
});
refuses("the lock's per-arch tag disagrees", /CODEX_TAG_arm64 disagrees with CODEX_TAG/, (files) => {
  files[LOCK] = bumped(realLock).replace(/^CODEX_TAG_arm64=.*$/m, "CODEX_TAG_arm64=rust-v1.0.0");
});

it("syncs the small tree and is idempotent", () => {
  const dir = smallTree((files) => { files[LOCK] = bumped(realLock); });
  try {
    const written = syncCodexPins(dir).sort();
    assert.deepEqual(written, ["agent/src/codex/codex-runtime-probe.ts", "docs/pin.md", "e2e/codex-m4/large-resume.test.ts", "e2e/codex-m4/token-resume.test.ts"]);
    assert.equal(readFileSync(join(dir, "docs/pin.md"), "utf8"), `binary at ${INSTALL_ROOT}${NEXT.version}/bin/codex\n`);
    assert.deepEqual(syncCodexPins(dir), []);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

it("never rewrites a recorded adoption assertion, even malformed text containing a live pin", () => {
  const file = "agent/codex/codex-package.adopted";
  // Even an invalid assertion is not sync's to repair or re-certify.
  for (const suffix of ["", `binary at ${INSTALL_ROOT}${current.version}/bin/codex\n`]) {
    const assertion = `lock-sha256=${"a".repeat(64)}\nruntime-tag=rust-v${current.version}\n${suffix}`;
    const dir = smallTree((files) => { files[LOCK] = bumped(realLock); files[file] = assertion; });
    try {
      assert.ok(!scannedPinFiles(dir).includes(file), "adoption is excluded from live-pin discovery");
      assert.ok(!syncCodexPins(dir).includes(file));
      assert.equal(readFileSync(join(dir, file), "utf8"), assertion, "sync does not certify adoption");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

it("a synchronized runtime bump still fails the release adoption guard until hand-recorded", () => {
  const file = "agent/codex/codex-package.adopted";
  const assertion = `lock-sha256=${createHash("sha256").update(realLock).digest("hex")}\nruntime-tag=rust-v${current.version}\n`;
  const cutter = readFileSync(join(root, ".agents/skills/uzi-release/scripts/release-cut.sh"), "utf8");
  // Exercise the real planner guard without the unrelated release ceremony.
  const start = cutter.indexOf("codex_adoption_record_matches() {");
  const end = cutter.indexOf("\n}\n", start);
  assert.ok(start >= 0 && end > start);
  const guard = `${cutter.slice(start, end + 3)}\ncodex_adoption_record_matches HEAD\n`;
  const dir = smallTree((files) => { files[file] = assertion; });
  const commit = () => { git(dir, "add", "-A"); git(dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "fixture"); };
  const check = () => spawnSync("bash", ["-s"], { cwd: dir, input: guard, encoding: "utf8" });
  try {
    commit();
    assert.equal(check().status, 0, "the original committed assertion authorizes its lock");
    writeFileSync(join(dir, LOCK), bumped(realLock));
    syncCodexPins(dir);
    assert.equal(readFileSync(join(dir, file), "utf8"), assertion);
    commit();
    assert.equal(check().status, 1, "pin sync cannot authorize the bumped runtime for nightly release");
    const nextLock = readFileSync(join(dir, LOCK));
    writeFileSync(join(dir, file), `lock-sha256=${createHash("sha256").update(nextLock).digest("hex")}\nruntime-tag=rust-v${NEXT.version}\n`);
    commit();
    assert.equal(check().status, 0, "the explicit adoption update authorizes the new lock");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
