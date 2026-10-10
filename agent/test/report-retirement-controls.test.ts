import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { git, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";

installHarness();

it("older generation archive cannot retire generation 2's cancelled report", async t => {
  const f = await reportRetirementFixture(t, { archiveGeneration: 1 });
  assert.equal(f.archive.generation, 1);
  await f.replay();
  assert.equal(f.calls.sends, 1);
  await f.assertPending();
});

it("unresolved terminal or archive status retains the unchanged covered source", async t => {
  const f = await reportRetirementFixture(t);
  for (const mode of ["unknown", "throw"] as const) {
    f.ownership.mode = mode;
    try {
      await assert.doesNotReject(async () => { await f.replay(); await f.assertPending(); },
        `ownership is ${mode}`);
    } finally { f.ownership.mode = "healthy"; }
  }
  f.ownership.generation = 3;
  try {
    await assert.doesNotReject(async () => { await f.replay(); await f.assertPending(); },
      "ownership is a newer generation");
  } finally { f.ownership.generation = 2; }
  for (const mode of ["throw", "expired", "unbound", "checksum"] as const) {
    f.capture.mode = mode;
    try {
      await assert.doesNotReject(async () => { await f.replay(); await f.assertPending(); },
        `capture status is ${mode}`);
    } finally { f.capture.mode = "healthy"; }
  }
});

it("actual live SDK flight and queued duplicate retain an already uploaded report", async t => {
  const f = await reportRetirementFixture(t, { liveFlight: true });
  const live = f.liveFlight;
  const queued = f.r.execute(f.claim);
  t.after(async () => { live.release(); await queued; });
  assert.equal(f.r.isExecuting(f.claim.run_id), true, "real execution owns the tail");
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(live.executions(), 1, "queued execute has not entered the SDK");
  await f.replay();
  await f.assertPending();
  live.release();
  await Promise.all([live.flight, queued]);
  assert.equal(f.r.isExecuting(f.claim.run_id), false, "every execute tail settled");
});

it("new committed source outside the frozen archive retains the report", async t => {
  const f = await reportRetirementFixture(t);
  const newer = f.commit("uncovered.txt");
  assert.notEqual(newer, f.head);
  assert.equal(await git.ancestry(f.clone, newer, f.archive.sourceSha), "divergent");
  await f.replay();
  await f.assertPending();
  assert.equal(fs.readFileSync(path.join(f.clone, "uncovered.txt"), "utf8"), "uncovered.txt\n");
});

for (const mode of ["staged", "unstaged", "untracked"] as const) {
  it(`${mode} source change after capture without a HEAD change retains the report`, async t => {
    const f = await reportRetirementFixture(t);
    const name = mode === "untracked" ? "uncaptured.txt" : "unpublished.txt";
    fs.writeFileSync(path.join(f.clone, name), "late uncaptured source\n");
    if (mode === "staged") f.cmd(f.clone, ["add", name]);
    assert.equal(f.cmd(f.clone, ["rev-parse", "HEAD"]), f.head);
    assert.notEqual(f.cmd(f.clone, ["status", "--porcelain"]), "");
    await f.replay();
    await f.assertPending();
    assert.equal(fs.readFileSync(path.join(f.clone, name), "utf8"), "late uncaptured source\n");
  });
}

it("captured WIP marker fixture has a clean source whose tree is in the actual archive", async t => {
  const f = await reportRetirementFixture(t, { capturedWip: true });
  assert.match(f.cmd(f.clone, ["log", "-1", "--format=%s"]), /^wip\(park\):/);
  assert.equal(f.cmd(f.clone, ["status", "--porcelain"]), "");
  assert.equal(f.cmd(f.bare, ["show", f.archive.sourceSha + ":marker.txt"]), "captured WIP");
  await f.assertCustody();
});

it("reset-soft captured WIP content must remain pending despite archive ancestry", async t => {
  const f = await reportRetirementFixture(t, { capturedWip: true });
  f.cmd(f.clone, ["reset", "--soft", "HEAD^"]);
  assert.notEqual(f.cmd(f.clone, ["rev-parse", "HEAD"]), f.head);
  assert.match(f.cmd(f.clone, ["status", "--porcelain"]), /marker.txt/);
  await f.replay();
  await f.assertPending();
  assert.equal(fs.readFileSync(path.join(f.clone, "marker.txt"), "utf8"), "captured WIP\n");
});

it("unknown physical source inventory cannot authorize report retirement", async t => {
  const f = await reportRetirementFixture(t);
  t.mock.method(git, "readInventoryCloneHeads", async () => ({ kind: "unknown", cause: "attribution_unreadable" }));
  await f.replay();
  await f.assertPending();
});

it("a parseable coverage journal with an invalid MAC retains the report despite available server bytes", async t => {
  const f = await reportRetirementFixture(t, { invalidJournal: true });
  await f.replay();
  await f.assertPending();
});

it("late dirty source hook remains available across the proof's asynchronous read seam", async t => {
  const f = await reportRetirementFixture(t);
  const read = git.credentialFreeCancelCleanHead.bind(git);
  // M2 must reach and revalidate this seam. Base retains before it needs local archive authority.
  t.mock.method(git, "credentialFreeCancelCleanHead", async (clone: string, bare: string, head: string) => {
    const clean = await read(clone, bare, head);
    if (clone === f.clone) fs.writeFileSync(path.join(clone, "late-hook.txt"), "late source mutation\n");
    return clean;
  });
  await f.replay();
  await f.assertPending();
});
