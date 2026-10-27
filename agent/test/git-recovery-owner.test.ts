import assert from "node:assert/strict";
import { test } from "node:test";
import { ownerFixture } from "./recovery-owner-fixture.js";

test("raw zero baseline permits one owner resume with a newer generation; fresh generation and derived rises do not", async () => {
  const f = ownerFixture();
  await f.reserve();
  await f.block();
  const before = f.read();
  for (const [gen, grant, derived] of [[2, 0, 9], [2, undefined, 9], [1, 1, 8], [2, -1, 8], [2, 0.5, 8]]) {
    await assert.rejects(f.reserve(gen, grant, derived), /blocked/);
    assert.deepEqual(f.read(), before);
  }
  const renewed = await f.reserve(2, 1, 8);
  assert.equal(renewed.attempts, 1);
  assert.equal(renewed.claimGeneration, 2);
  assert.equal(JSON.parse(f.values.get(f.ownerKey)![0]!).consumed, 1);
  await f.block();
  await assert.rejects(f.reserve(3, 1, 9), /blocked/);
  assert.equal((await f.reserve(3, 2, 9)).attempts, 1);
});

for (const control of ["missing", "malformed", "duplicate", "baseline", "bare", "key", "branch", "runId", "source", "episode"] as const) {
  test(`blocked adoption refuses ${control} owner evidence without reconstructing a baseline`, async () => {
    const f = ownerFixture();
    await f.reserve();
    await f.block();
    const raw = f.values.get(f.ownerKey)![0]!;
    const record = JSON.parse(raw);
    if (control === "missing") f.values.delete(f.ownerKey);
    else if (control === "malformed") f.values.set(f.ownerKey, ["{"]);
    else if (control === "duplicate") f.values.set(f.ownerKey, [raw, raw]);
    else {
      if (control === "baseline") delete record.baseline;
      if (control === "bare") record.bare = "other-bare";
      if (control === "key") record.key = "other-key";
      if (control === "branch") record.branch = "other-branch";
      if (control === "runId") record.runId = "other-run";
      if (control === "source") record.source.clonePath += "-other";
      if (control === "episode") record.after.episode++;
      f.values.set(f.ownerKey, [JSON.stringify(record)]);
    }
    const before = f.read();
    await assert.rejects(f.reserve(2, 1, 8));
    assert.deepEqual(f.read(), before);
    assert.equal(await f.recreate().canRenewRecoveryAdoption(f.bare, f.branch, f.key, before, 2, 1).catch(() => false), false);
  });
}

for (const point of ["pending-write-before", "pending-write-after", "pending-read", "journal-write-before",
  "journal-write-after", "journal-read", "commit-write-before", "commit-write-after", "commit-read",
  "pending-read-after", "journal-read-after", "commit-read-after"] as const) {
  test(`owner charge interruption at ${point} never returns work or replays the grant`, async () => {
    const f = ownerFixture();
    await f.reserve();
    await f.block();
    let lastWrite = "";
    f.fault((args, phase) => {
      const ownerWrite = args.at(-2) === f.ownerKey;
      const journalWrite = args.at(-2) === f.journalKey;
      const state = ownerWrite ? JSON.parse(args.at(-1)!).state : undefined;
      const label = ownerWrite ? (state === "pending" ? "pending" : "commit") : journalWrite ? "journal" : "";
      if (args.includes("--list") && ((phase === "before" && point === lastWrite + "-read") ||
          (phase === "after" && point === lastWrite + "-read-after"))) throw new Error("injected " + point);
      if (label && point === label + "-write-" + phase) throw new Error("injected " + point);
      if (label && phase === "after") lastWrite = label;
    });
    await assert.rejects(f.reserve(2, 1, 8), /injected/);
    f.fault();
    const raw = f.values.get(f.ownerKey)![0]!;
    const record = JSON.parse(raw);
    if (point === "pending-write-before") {
      assert.equal(record.consumed, 0, "nothing landed; the original grant remains available");
      const fresh = await f.reserve(3, 1, 9);
      assert.equal(fresh.claimGeneration, 3);
      assert.equal(fresh.attempts, 1);
      assert.equal(JSON.parse(f.values.get(f.ownerKey)![0]!).after.claimGeneration, 3);
      return;
    }
    assert.equal(record.consumed, 1);
    if (record.state === "pending") await assert.rejects(f.reserve(2, 1, 8), /pending/);
    else await f.block();
    await assert.rejects(f.reserve(3, 1, 9));
    if (["pending-write-after", "pending-read", "pending-read-after", "journal-write-before"].includes(point)) {
      await assert.rejects(f.reserve(3, 2, 9), /pending/);
      assert.equal(f.read().recovery?.claimGeneration, 1);
    } else {
      const renewed = await f.reserve(3, 2, 9);
      assert.equal(renewed.claimGeneration, 3);
      assert.equal(renewed.attempts, 1);
      const landed = JSON.parse(f.values.get(f.ownerKey)![0]!);
      assert.equal(landed.state, "committed");
      assert.equal(landed.consumed, 2);
      assert.equal(landed.after.claimGeneration, 3);
      assert.equal(landed.after.attempts, 1);
    }
  });
}

test("adoption renewal does not require a derived scalar rise", async () => {
  const f = ownerFixture();
  await f.reserve(); await f.block();
  const renewed = await f.reserve(2, 1, 7);
  assert.equal(renewed.episode, 7);
  assert.equal(renewed.attempts, 1);
});

test("changed journal source, root and reservation refuse resolution and renewal", async () => {
  for (const change of ["source", "root", "episode", "generation", "attempts", "clock"]) {
    const f = ownerFixture();
    const reservation = await f.reserve(); await f.block();
    const j = f.read();
    if (change === "source") {
      j.recovery!.source = { ...j.recovery!.source, clonePath: j.clonePath + "-other" };
      j.retainedSources = [j.recovery!.source];
    }
    if (change === "root") { j.clonePath += "-other"; j.retainedSources = [f.source]; }
    if (change === "episode") j.recovery!.episode!++;
    if (change === "generation") j.recovery!.claimGeneration!++;
    if (change === "attempts") j.recovery!.attempts++;
    if (change === "clock") { j.recovery!.startedAt++; j.recovery!.deadline++; }
    f.write(j);
    await assert.rejects(f.recreate().recoveryReservationSnapshot(f.bare, f.branch, f.key, f.source, reservation));
    await assert.rejects(f.reserve(3, 1, 8));
  }
});

test("bootstrap requires a positive claim fence and never occurs on an existing blocker", async () => {
  const f = ownerFixture();
  await f.recreate().reserveRecoveryIteration(f.bare, f.branch, f.key, f.source, 1, undefined, 7, 0);
  assert.equal(f.values.has(f.ownerKey), false);
  await f.block();
  await assert.rejects(f.reserve(2, 1, 8));
  assert.equal(f.values.has(f.ownerKey), false);
});

test("external record rejects duplicate JSON keys and bounded record overflow", async () => {
  const f = ownerFixture();
  await f.reserve(); await f.block();
  const raw = f.values.get(f.ownerKey)![0]!;
  for (const value of ['{"version":1,' + raw.slice(1), raw + " ".repeat(32_768)]) {
    f.values.set(f.ownerKey, [value]);
    await assert.rejects(f.reserve(2, 1, 8));
  }
});

test("a journal missing its charged claim fence cannot renew through a zero-generation record", async () => {
  const f = ownerFixture();
  await f.reserve(); await f.block();
  const record = JSON.parse(f.values.get(f.ownerKey)![0]!);
  record.after.claimGeneration = 0;
  f.values.set(f.ownerKey, [JSON.stringify(record)]);
  const journal = f.read(); delete journal.recovery!.claimGeneration; f.write(journal);
  await assert.rejects(f.reserve(2, 1, 8));
  assert.equal(await f.recreate().canRenewRecoveryAdoption(f.bare, f.branch, f.key, journal, 2, 1).catch(() => false), false);
});

test("owner fixture works without a TMPDIR override", () => {
  const previous = process.env.TMPDIR;
  delete process.env.TMPDIR;
  try { assert.doesNotThrow(() => ownerFixture()); }
  finally {
    if (previous === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = previous;
  }
});
