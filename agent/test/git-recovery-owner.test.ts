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
    const refusal = {
      missing: { message: /^recovery episode is blocked; no new owner grant$/ },
      malformed: SyntaxError,
      duplicate: { message: /^duplicate or missing recovery owner record$/ },
      baseline: { message: /^invalid recovery owner record$/ },
      bare: { message: /^recovery owner binding changed$/ },
      key: { message: /^recovery owner binding changed$/ },
      branch: { message: /^recovery owner binding changed$/ },
      runId: { message: /^invalid recovery owner record$/ },
      source: { message: /^recovery owner reservation changed$/ },
      episode: { message: /^recovery owner reservation changed$/ },
    }[control];
    await assert.rejects(f.reserve(2, 1, 8), refusal);
    assert.deepEqual(f.read(), before);
    const eligible = f.recreate().canRenewRecoveryAdoption(f.bare, f.branch, f.key, before, 2, 1);
    if (control === "missing" || control === "source" || control === "episode") assert.equal(await eligible, false);
    else await assert.rejects(eligible, refusal);
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
    const before = f.read();
    const isBefore = ["pending-write-after", "pending-read", "pending-read-after", "journal-write-before"].includes(point);
    if (record.state === "pending") {
      await assert.rejects(f.reserve(2, 1, 8), /pending/);
      assert.deepEqual(f.read(), before, "settlement changes accounting only");
    }
    if (isBefore) {
      await assert.rejects(f.reserve(2, 2, 9), /pending/);
      await assert.rejects(f.reserve(3, 1, 9), /blocked/);
      assert.equal(await f.recreate().canRenewRecoveryAdoption(f.bare, f.branch, f.key, before, 3, 2), true);
    }
    const renewed = await f.reserve(3, isBefore ? 2 : 1, 9);
    assert.equal(renewed.claimGeneration, 3);
    assert.equal(renewed.attempts, 1);
    const landed = JSON.parse(f.values.get(f.ownerKey)![0]!);
    assert.equal(landed.state, "committed");
    assert.equal(landed.consumed, isBefore ? 2 : 1);
    assert.equal(landed.after.claimGeneration, 3);
    await f.block();
    await assert.rejects(f.reserve(4, landed.consumed, 10), /blocked/);
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
    await assert.rejects(f.recreate().recoveryReservationSnapshot(f.bare, f.branch, f.key, f.source, reservation),
      change === "source" || change === "root" ? { message: /^recovery source identity mismatch$/ } : { message: /^recovery reservation changed$/ });
    await assert.rejects(f.reserve(3, 1, 8), { message: /^recovery owner reservation changed$/ });
  }
});

test("bootstrap requires a positive claim fence and never reconstructs blocked adoption", async () => {
  const f = ownerFixture();
  await f.recreate().reserveRecoveryIteration(f.bare, f.branch, f.key, f.source, 1, undefined, 7, 0);
  assert.equal(f.values.has(f.ownerKey), false);
  await f.block();
  await assert.rejects(f.reserve(2, 1, 8), { message: /^recovery episode is blocked; no new owner grant$/ });
  assert.equal(f.values.has(f.ownerKey), false);
});

test("external record rejects duplicate JSON keys and bounded record overflow", async () => {
  const f = ownerFixture();
  await f.reserve(); await f.block();
  const raw = f.values.get(f.ownerKey)![0]!;
  for (const value of ['{"version":1,' + raw.slice(1), raw + " ".repeat(32_768)]) {
    f.values.set(f.ownerKey, [value]);
    await assert.rejects(f.reserve(2, 1, 8), { message: /^invalid recovery owner record$/ });
  }
});

test("a journal missing its charged claim fence cannot renew through a zero-generation record", async () => {
  const f = ownerFixture();
  await f.reserve(); await f.block();
  const record = JSON.parse(f.values.get(f.ownerKey)![0]!);
  record.after.claimGeneration = 0;
  f.values.set(f.ownerKey, [JSON.stringify(record)]);
  const journal = f.read(); delete journal.recovery!.claimGeneration; f.write(journal);
  await assert.rejects(f.reserve(2, 1, 8), { message: /^invalid recovery owner record$/ });
  await assert.rejects(f.recreate().canRenewRecoveryAdoption(f.bare, f.branch, f.key, journal, 2, 1), { message: /^invalid recovery owner record$/ });
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

async function interrupted(kind: "first" | "budget" | "adoption", side: "before" | "after") {
  const f = ownerFixture();
  if (kind !== "first") {
    await f.reserve();
    if (kind === "budget") {
      await f.reserve(); await f.reserve();
      await assert.rejects(f.reserve(), /budget/);
    } else await f.block();
  }
  const original = f.read();
  f.fault((args, phase) => {
    if (args.at(-2) === f.journalKey && phase === side) throw new Error("interrupted journal");
  });
  await assert.rejects(f.reserve(2, 1, 8), /interrupted/);
  f.fault();
  return { f, original };
}

for (const kind of ["first", "budget", "adoption"] as const) {
  test(`pending AFTER ${kind} settles accounting without inventing a blocker`, async () => {
    const { f } = await interrupted(kind, "after");
    const raw = f.values.get(f.journalKey)![0];
    const owner = JSON.parse(f.values.get(f.ownerKey)![0]!);
    await assert.rejects(f.reserve(3, 1, 8), { name: "Error", message: /pending/ });
    assert.equal(f.values.get(f.journalKey)![0], raw);
    assert.deepEqual(JSON.parse(f.values.get(f.ownerKey)![0]!), { ...owner, state: "committed" });
    const charged = await f.reserve(3, 1, 8);
    assert.equal(charged.attempts, 2);
    assert.equal(charged.claimGeneration, 3);
    assert.equal(JSON.parse(f.values.get(f.ownerKey)![0]!).consumed, 1);
    await f.block();
    await assert.rejects(f.reserve(4, 1, 99), /blocked/);
  });

  test(`pending BEFORE ${kind} requires fresh generation and current ownership for a new charge`, async () => {
    const { f, original } = await interrupted(kind, "before");
    const pending = f.values.get(f.ownerKey)![0];
    await assert.rejects(f.reserve(2, 2, 99), /pending/);
    await assert.rejects(f.recreate().reserveRecoveryIteration(f.bare, f.branch, f.key, f.read(), 1, 3, 9, 2), /pending/);
    await assert.rejects(f.recreate().reserveRecoveryIteration(f.bare, f.branch, f.key, f.read(), 1, 3, 9, 2,
      async () => { throw new Error("not current owner"); }), /not current owner/);
    assert.deepEqual(f.read(), original);
    assert.equal(f.values.get(f.ownerKey)![0], pending);
    if (kind === "adoption") {
      await assert.rejects(f.reserve(3, 1, 99), /blocked/);
      assert.equal(await f.recreate().canRenewRecoveryAdoption(f.bare, f.branch, f.key, f.read(), 2, 2), false);
    }
    const charged = await f.reserve(3, kind === "adoption" ? 2 : 1, 9);
    assert.equal(charged.attempts, 1);
    assert.equal(charged.claimGeneration, 3);
    assert.ok(f.ownershipProofs() > 0);
    const owner = JSON.parse(f.values.get(f.ownerKey)![0]!);
    assert.equal(owner.baseline, kind === "first" ? 1 : 0);
    assert.equal(owner.consumed, kind === "adoption" ? 2 : 1);
    assert.equal(owner.state, "committed");
  });
}

for (const change of ["source", "root", "reservation", "bare", "null-with-progress", "zero-generation"] as const) {
  test(`pending BEFORE refuses ${change} evidence`, async () => {
    const { f } = await interrupted("adoption", "before");
    const record = JSON.parse(f.values.get(f.ownerKey)![0]!);
    if (change === "source") record.source.clonePath += "-other";
    if (change === "root") record.root.clonePath += "-other";
    if (change === "reservation") record.before.attempts++;
    if (change === "bare") record.bare += "-other";
    if (change === "null-with-progress") record.before = null;
    if (change === "zero-generation") {
      record.before.claimGeneration = 0;
      const journal = f.read(); delete journal.recovery!.claimGeneration; f.write(journal);
    }
    f.values.set(f.ownerKey, [JSON.stringify(record)]);
    const before = f.read();
    await assert.rejects(f.reserve(3, 2, 9), change === "bare" ? { message: /^recovery owner binding changed$/ }
      : change === "zero-generation" ? { message: /^recovery episode is blocked; no new owner grant$/ } : { message: /^recovery owner reservation changed$/ });
    const eligible = f.recreate().canRenewRecoveryAdoption(f.bare, f.branch, f.key, before, 3, 2);
    if (change === "bare") await assert.rejects(eligible, { message: /^recovery owner binding changed$/ });
    else assert.equal(await eligible, false);
    assert.deepEqual(f.read(), before);
  });
}

test("pending BEFORE snapshot only reads an existing blocker", async () => {
  const { f, original } = await interrupted("adoption", "before");
  assert.equal(JSON.stringify(await f.recreate().recoveryReservationSnapshot(f.bare, f.branch, f.key, original, original.recovery!)), JSON.stringify(original));
  const journal = f.read(); journal.recovery!.stage = "capturing"; delete journal.recovery!.blocker; f.write(journal);
  await assert.rejects(f.recreate().recoveryReservationSnapshot(f.bare, f.branch, f.key, journal, journal.recovery!), { message: /^recovery owner reservation changed$/ });
});

for (const operation of ["replacement", "settlement"] as const) {
  for (const point of ["pending-write-before", "pending-write-after", "pending-read", "pending-read-after",
    "journal-write-before", "journal-write-after", "journal-read", "journal-read-after",
    "commit-write-before", "commit-write-after", "commit-read", "commit-read-after"] as const) {
    if (operation === "settlement" && !point.startsWith("commit")) continue;
    test(`${operation} interruption ${point} never returns an interrupted charge`, async () => {
      const { f } = await interrupted("adoption", operation === "replacement" ? "before" : "after");
      const original = f.values.get(f.journalKey)![0];
      let lastWrite = "";
      f.fault((args, phase) => {
        const ownerWrite = args.at(-2) === f.ownerKey;
        const label = ownerWrite ? (JSON.parse(args.at(-1)!).state === "pending" ? "pending" : "commit") :
          args.at(-2) === f.journalKey ? "journal" : "";
        if (label && point === label + "-write-" + phase) throw new Error("injected " + point);
        if (args.includes("--list") && point === lastWrite + "-read" + (phase === "after" ? "-after" : ""))
          throw new Error("injected " + point);
        if (label && phase === "after") lastWrite = label;
      });
      await assert.rejects(f.reserve(3, 2, 9), /injected/);
      f.fault();
      const record = JSON.parse(f.values.get(f.ownerKey)![0]!);
      const journal = f.read();
      if (operation === "settlement") assert.equal(f.values.get(f.journalKey)![0], original);
      if (record.state === "pending" && (record.after.claimGeneration === 3 || operation === "settlement")) {
        await assert.rejects(f.reserve(3, 2, 9), /pending/);
        assert.deepEqual(f.read(), journal);
      }
      // A completed replacement consumes grant 2; an interrupted BEFORE requires grant 3.
      const stillBefore = f.read().recovery?.stage === "blocked";
      const next = await f.reserve(4, stillBefore ? 3 : record.consumed, operation === "settlement" ? 8 : 9);
      assert.equal(next.claimGeneration, 4);
      assert.equal(next.attempts, stillBefore ? 1 : 2);
      const landed = JSON.parse(f.values.get(f.ownerKey)![0]!);
      assert.equal(landed.after.claimGeneration, 4);
      assert.equal(landed.state, "committed");
    });
  }
}

test("pending BEFORE ordinary legacy generation zero stays distinct from null and can charge normally", async () => {
  const { f } = await interrupted("adoption", "before");
  const record = JSON.parse(f.values.get(f.ownerKey)![0]!);
  const journal = f.read();
  record.before.claimGeneration = 0;
  delete journal.recovery!.claimGeneration;
  journal.recovery!.stage = "capturing"; delete journal.recovery!.blocker;
  f.values.set(f.ownerKey, [JSON.stringify(record)]); f.write(journal);
  const next = await f.reserve(3, 1, 7);
  assert.equal(next.attempts, 2);
  assert.equal(next.claimGeneration, 3);
});

test("pending BEFORE budget needs derived renewal; ordinary charge needs no raw increase", async () => {
  const { f } = await interrupted("budget", "before");
  const raw = f.values.get(f.journalKey)![0];
  await assert.rejects(f.reserve(3, 2, 7), /blocked/);
  assert.equal(f.values.get(f.journalKey)![0], raw);
  assert.equal((await f.reserve(3, 1, 8)).attempts, 1);
  const g = ownerFixture(); await g.reserve();
  g.fault((args, phase) => { if (args.at(-2) === g.journalKey && phase === "before") throw new Error("interrupted"); });
  await assert.rejects(g.reserve(2, 0, 7)); g.fault();
  assert.equal((await g.reserve(3, 0, 7)).attempts, 2);
});

for (const baseline of [0, 3]) {
  test(`legacy budget renewal bootstraps raw owner baseline ${baseline} before adoption can fail`, async () => {
    const f = ownerFixture();
    const legacy = f.recreate();
    const reserveLegacy = () => legacy.reserveRecoveryIteration(f.bare, f.branch, f.key, f.read(), 1000, 1, 7);
    for (let i = 0; i < 3; i++) await reserveLegacy();
    await assert.rejects(reserveLegacy(), /budget/);
    assert.equal(f.read().recovery!.blocker, "budget_exhausted");
    assert.equal(f.values.has(f.ownerKey), false);
    const renewed = await f.reserve(2, baseline, 8);
    assert.equal(renewed.attempts, 1);
    assert.ok(f.values.has(f.ownerKey), "eligible legacy budget renewal must persist its owner baseline before work");
    const owner = JSON.parse(f.values.get(f.ownerKey)![0]!);
    assert.equal(owner.baseline, baseline);
    assert.equal(owner.consumed, baseline, "bootstrap snapshots the existing grant without inventing another");
    assert.equal(owner.state, "committed");
    await f.block();
    await assert.rejects(f.reserve(3, baseline, 99), /blocked/);
    assert.equal((await f.reserve(3, baseline + 1, 9)).attempts, 1);
    assert.equal(JSON.parse(f.values.get(f.ownerKey)![0]!).consumed, baseline + 1);
    await f.block();
    await assert.rejects(f.reserve(4, baseline + 1, 100), /blocked/);
  });
}
