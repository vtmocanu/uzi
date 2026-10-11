import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { installHarness, git } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
installHarness();

for (const fault of ["short EOF", "valid prefix invalid trailing", "replacement"] as const) {
  it(`owed metadata report proof: ${fault}`, async t => {
    const f = await reportRetirementFixture(t);
    const dir = path.join(f.bare, "uzi-owed");
    const name = (await fs.readdir(dir)).find(n => n.startsWith("context-"))!;
    assert.ok(name);
    const target = path.join(dir, name);
    const original = await fs.readFile(target);
    if (fault === "valid prefix invalid trailing") await fs.appendFile(target, "!");
    const open = fs.open.bind(fs);
    let reads = 0, eof = false;
    t.mock.method(fs, "open", async (...args: Parameters<typeof open>) => {
      const file = await open(...args);
      if (String(args[0]) === target) {
        const read = file.read.bind(file);
        file.read = (async (buffer: Buffer, offset: number, length: number, position: number | null) => {
          reads++;
          const result = await read(buffer, offset, Math.min(length, original.length), position);
          if (!result.bytesRead) eof = true;
          if (fault === "replacement" && reads === 1) {
            const replacement = target + ".replacement";
            await fs.writeFile(replacement, original, { mode: 0o600 });
            await fs.rename(replacement, target);
          }
          return result;
        }) as typeof file.read;
      }
      return file;
    });
    const proof = git.withReportProofBudget({ signal: new AbortController().signal, deadline: Date.now() + 5000 },
      () => git.enumerateOwedCandidates(f.bare, f.claim.run_id));
    if (fault === "short EOF") assert.ok((await proof).length);
    else await assert.rejects(proof);
    assert.ok(reads >= 2, "proof reached full file reader");
    assert.equal(eof, true, "JSON parsing follows actual EOF");
    await fs.writeFile(target, original, { mode: 0o600 });
    await f.assertPending();
  });
}

for (const fault of ["physical directory overflow", "file overflow", "invalid trailing"] as const) {
  it(`recovery journal ${fault} retains report`, async t => {
    const f = await reportRetirementFixture(t);
    const dir = path.join(git.recoveryRoot, f.claim.run_id);
    const target = path.join(dir, f.archive.captureId + ".json");
    const original = await fs.readFile(target);
    if (fault === "physical directory overflow") {
      for (let i = 0; i < 257; i++) await fs.writeFile(path.join(dir, "entry-" + i + ".tmp"), "");
    } else await fs.appendFile(target, fault === "file overflow" ? " ".repeat(1024 * 1024 + 1) : "!");
    await f.replay();
    assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, f.generation), true);
    assert.equal(f.calls.final, 0);
    if (fault !== "physical directory overflow") await fs.writeFile(target, original);
    await f.assertPending();
  });
}

it("owed cumulative 32 MiB budget refuses individually admissible files in one scan", async t => {
  const f = await reportRetirementFixture(t);
  const dir = path.join(f.bare, "uzi-owed");
  const name = (await fs.readdir(dir)).find(n => n.startsWith("candidate-"))!;
  assert.ok(name);
  const candidate = JSON.parse(await fs.readFile(path.join(dir, name), "utf8"));
  const created: string[] = [];
  // 513 individually valid 64 KiB files fit the entry cap but exceed the aggregate cap.
  for (let i = 0; i < 513; i++) {
    const sha = i.toString(16).padStart(40, "0");
    const bytes = Buffer.from(JSON.stringify({ ...candidate, sha }));
    const file = path.join(dir, `candidate-${candidate.runId}-${sha}-${candidate.context.slice(8, -5)}.json`);
    await fs.writeFile(file, Buffer.concat([bytes, Buffer.alloc(65536 - bytes.length, 32)]), { mode: 0o600 });
    created.push(file);
  }
  await assert.rejects(git.withReportProofBudget({
    signal: new AbortController().signal, deadline: Date.now() + 10000,
  }, () => git.enumerateOwedCandidates(f.bare, f.claim.run_id)), /owed byte budget/);
  for (const file of created) await fs.unlink(file);
  await f.assertPending();
});

it("owed streaming physical directory cap refuses before metadata parsing", async t => {
  const f = await reportRetirementFixture(t);
  const dir = path.join(f.bare, "uzi-owed");
  for (let i = 0; i < 1025; i++) await fs.writeFile(path.join(dir, ".tmp-" + String(i).padStart(36, "0")), "");
  await assert.rejects(git.withReportProofBudget({
    signal: new AbortController().signal, deadline: Date.now() + 5000,
  }, () => git.enumerateOwedCandidates(f.bare, f.claim.run_id)), /owed entry budget/);
  await f.assertPending();
});
