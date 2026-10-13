import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";

for (const fault of ["oversized replacement", "nonregular", "trailing", "cancel", "deadline", "replacement", "short valid"] as const) {
  it(`finalize report bounded reader: ${fault}`, async t => {
    const root = await fs.mkdtemp(path.join(os.tmpdir(), "finalize-reader-"));
    t.after(() => fs.rm(root, { recursive: true, force: true }));
    const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 1024 * 1024,
      maxBytes: 16 * 1024 * 1024, retentionMs: 86400000 });
    await outbox.init();
    const run = "00000000-0000-4000-8000-000000002652";
    await outbox.journalFinalize(run, 2);
    const identity = outbox.finalizeRecordIdentity(run, 2)!;
    assert.ok(identity);
    const target = path.join(root, run, "finalize-2.json");
    const original = await fs.readFile(target);
    if (fault === "oversized replacement") {
      await fs.writeFile(target + ".new", Buffer.alloc(8 * 1024 * 1024, 32));
      await fs.rename(target + ".new", target);
    }
    if (fault === "nonregular") { await fs.unlink(target); await fs.mkdir(target); }
    if (fault === "trailing") await fs.appendFile(target, "!");
    const abort = new AbortController();
    const budget = { signal: abort.signal, deadline: Date.now() + 5000 };
    const open = fs.open.bind(fs);
    let reached = 0, reads = 0, readFiles = 0;
    t.mock.method(fs, "open", async (...args: Parameters<typeof open>) => {
      const file = await open(...args);
      if (String(args[0]) === target) {
        reached++;
        const readFile = file.readFile.bind(file);
        file.readFile = ((...a: Parameters<typeof readFile>) => {
          readFiles++; return readFile(...a);
        }) as typeof file.readFile;
        const read = file.read.bind(file);
        file.read = (async (buffer: Buffer, offset: number, length: number, position: number | null) => {
          reads++;
          const result = await read(buffer, offset, Math.min(length, 17), position);
          if (reads === 1) {
            if (fault === "cancel") abort.abort();
            if (fault === "deadline") budget.deadline = Date.now() - 1;
            if (fault === "replacement") {
              await fs.writeFile(target + ".new", original);
              await fs.rename(target + ".new", target);
            }
          }
          return result;
        }) as typeof file.read;
      }
      return file;
    });
    await outbox.retireFinalizeIfEligible(run, 2, () => true, abort.signal, identity, true, budget);
    assert.equal(reached, 1);
    assert.equal(readFiles, 0, "report reader never uses uncapped readFile");
    if (fault === "short valid") {
      assert.ok(reads > 2);
      assert.equal(outbox.finalizeRecordIdentity(run, 2), undefined);
      await assert.rejects(fs.stat(target), { code: "ENOENT" });
    } else {
      assert.equal(outbox.finalizeRecordIdentity(run, 2), identity);
      await fs.lstat(target);
      if (fault === "oversized replacement" || fault === "nonregular") assert.equal(reads, 0);
      else assert.ok(reads > 0);
    }
  });
}
