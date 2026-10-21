import assert from "node:assert/strict";
import fsSync from "node:fs";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { after } from "node:test";
import { restoreTreeWritability, type TeardownTestDeps } from "../src/rmtree.js";

/** Portable lifecycle fixture only: production never falls back to path removal. */
export const portableTeardownTestDeps: TeardownTestDeps | undefined =
  process.platform === "linux" ? undefined : {
    removeTreePinned: async (parent, name) => {
      const target = path.join(parent, name);
      await restoreTreeWritability(target);
      await fs.rm(target, { recursive: true, force: true });
      return "removed";
    },
  };

/** A file-owned parent for model-pass HOMEs (`homeRoot`), removed after the file's tests (#1912).
 *  Production HOME cleanup is descriptor-pinned and refuses off Linux, so on macOS every pass a test
 *  runs with the default `os.tmpdir()` root leaves a `uzi-<label>-*` HOME the tmpdir leak guard
 *  reports. On Linux the root must already be empty: production cleanup still owns those HOMEs, and
 *  this fixture never hides a HOME it failed to remove. Call it at module scope. */
export function testOwnedPassHomeRoot(): string {
  const root = fsSync.mkdtempSync(path.join(fsSync.realpathSync(os.tmpdir()), "uzi-test-pass-homes-"));
  after(async () => {
    const left = await fs.readdir(root);
    try {
      if (process.platform === "linux") assert.deepEqual(left, [], "model-pass HOME cleanup left entries on Linux");
    } finally {
      if (process.platform !== "linux") await restoreTreeWritability(root);
      await fs.rm(root, { recursive: true, force: true });
    }
  });
  return root;
}
