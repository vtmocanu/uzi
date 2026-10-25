import assert from "node:assert/strict";
import type { TestContext } from "node:test";
import fs from "node:fs/promises";
import path from "node:path";
import type { GitCache } from "../src/git.js";
import { maintenanceDeltaFixture } from "./maintenance-delta-fixture.js";

type Request = Parameters<GitCache["importMaintenanceTip"]>[0];
export const snapshotLinux = { skip: process.platform !== "linux" };

/** Encoded walk observation is not an authenticated/imported tip. The actual
 * importer runs under its original slot/lock/pins and stops at the test seam. */
export async function snapshotPortFixture(t: TestContext) {
  const f = await maintenanceDeltaFixture(t);
  const requestFor = (overrides: Partial<Request> = {}): Request => {
    const request = { ...f.request, ...overrides };
    return { ...request, family: overrides.family ?? [request.source] };
  };
  const importTip = (overrides: Partial<Request> = {}) =>
    f.inScope(() => f.cache.importMaintenanceTip(requestFor(overrides)));
  const walk = async (overrides: Partial<Request> = {}, completedDirectory?: string) => {
    assert.equal(f.cache.afterPhase, undefined, "use importTip for a mutation after a completed walk");
    const completed = new Error("snapshot_port_walk_complete");
    let observed = false, directoryClosed = false;
    const previousPrepare = f.hooks.prepare;
    if (completedDirectory) {
      const opendir = fs.opendir.bind(fs);
      t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
        const stream = await opendir(...args);
        if (f.physical(args[0]) === completedDirectory) {
          const close = stream.close.bind(stream);
          t.mock.method(stream, "close", async () => { await close(); directoryClosed = true; });
        }
        return stream;
      });
      // The next prepare runs in the parent loop, after the child's EOF, close
      // and unchanged descriptor check. Later info controls may derive longer paths.
      f.hooks.prepare = async () => {
        await previousPrepare?.();
        if (directoryClosed && f.cache.phase === "walk") { observed = true; throw completed; }
      };
    }
    f.cache.afterPhase = async phase => {
      if (phase === "walk") {
        assert.equal(completedDirectory, undefined, "requested directory must stop the walk before trailing controls");
        observed = true; throw completed;
      }
    };
    try {
      await importTip(overrides);
      assert.fail("walk observation must not return an accepted tip");
    } catch (error) {
      if (error !== completed) throw error;
      assert.equal(observed, true);
    } finally { f.cache.afterPhase = undefined; f.hooks.prepare = previousPrepare; }
    assert.equal(f.requests.length, 0, "encoded walk launches no Git");
    assert.equal(f.pairs(), 0, "encoded walk obtains no remote authority");
    assert.equal(f.joined(), f.requests.length);
  };
  const originalRefs = await f.refs();
  const assertRetained = async () => {
    await f.retained();
    assert.equal(await f.refs(), originalRefs, "no ref effects");
    assert.equal(f.joined(), f.requests.length, "every actual child joined");
  };
  return { ...f, requestFor, importTip, walk, assertRetained };
}
export type SnapshotPortFixture = Awaited<ReturnType<typeof snapshotPortFixture>>;

export function portLongPath(root: string, targetBytes: number): string {
  assert.ok(Buffer.byteLength(root) < targetBytes);
  while (targetBytes - Buffer.byteLength(root) - 1 > 200) root += "/" + "p".repeat(200);
  return root + "/" + "q".repeat(targetBytes - Buffer.byteLength(root) - 1);
}

export async function portEmptyObjects(clone: string): Promise<void> {
  await fs.mkdir(path.join(clone, ".git", "objects", "pack"), { recursive: true, mode: 0o700 });
}
