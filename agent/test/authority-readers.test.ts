import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { installHarness, fakeGitlab, runnerWith } from "./runner-harness.js";
import { JOIN, readRecords, recordDir, rig, slotOf, snapshotFiles, type PrivateRunner } from "./completion-retirement-fixture.js";

installHarness();

// issue #3404 M2 U5 - the readers of source state. Nothing that enumerates, accounts for or deletes under the
// data dir may reach `<dataDir>/.uzi-authority`, and no observational reader may turn a V2 record into release
// authority. The sweep itself (every reader checked, with its location) is in the U5 report; these tests pin the
// two properties that can be checked mechanically.

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../src");

function sources(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap(entry => entry.isDirectory()
    ? sources(path.join(dir, entry.name)) : entry.name.endsWith(".ts") ? [path.join(dir, entry.name)] : []);
}

describe("the authority store is never enumerated, reclaimed or deleted by a data-root sweep", () => {
  it("no source file lists or removes the data root itself (every enumerator works on a named subdirectory)", () => {
    // A call whose FIRST argument is the data root (config.dataDir / opts.dataDir / this.dataDir / dataDir).
    const call = /\b(?:readdir|readdirSync|opendir|opendirSync|rm|rmSync|rmdir|rmtree|rmTreePinned|rmTeardownTree|glob|globSync|unlink|unlinkSync)\(\s*(?:path\.resolve\(\s*)?(?:this\.|opts\.|options\.|config\.|ctx\.|deps\.)*dataDir\b/;
    const offenders: string[] = [];
    for (const file of sources(SRC)) {
      fs.readFileSync(file, "utf8").split("\n").forEach((line, index) => {
        if (call.test(line) && !line.trimStart().startsWith("//") && !line.trimStart().startsWith("*")) {
          offenders.push(`${path.relative(SRC, file)}:${index + 1}: ${line.trim()}`);
        }
      });
    }
    assert.deepEqual(offenders, []);
  });
});

describe("observational readers never confer release authority", () => {
  it("a coordinator with no authority scope cannot verify a V2 source and retains the published cohort", async t => {
    const r = await rig(t, { codex: false, cohort: 2 });
    await r.end();
    await r.receipt();
    assert.equal(readRecords(r).length, 2);
    const files = snapshotFiles(r);
    const noAuthority = runnerWith(() => ({ executor: r.executor }), fakeGitlab().gitlab, JOIN) as unknown as PrivateRunner;
    const snapshot = (await noAuthority.recovery.inspect(r.runId)).filter(x => x.completionReceipt);
    assert.equal(snapshot.length, 1, "inspect decodes the V2 record");
    await noAuthority.recovery.resumePending(undefined, snapshot);
    assert.ok(fs.existsSync(path.join(r.clone, ".git")), "the clone stays");
    assert.deepEqual(snapshotFiles(r), files);
    assert.equal((await slotOf(r))?.state, "claude_ready", "the publication is untouched");
    assert.ok(fs.existsSync(recordDir(r)));
  });
});
