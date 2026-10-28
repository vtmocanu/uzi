// The harness owns this otherwise-unused, per-container tmpfs directory.
// Parallel Node test children share the same repository-relative scratch link.
import { lstatSync, readFileSync, readlinkSync, symlinkSync } from "node:fs";
import assert from "node:assert/strict";
// Inspect the baked paths, independent of which tracked sources the lane mounts.
const directory = "/app/templates";
const directoryStat = lstatSync(directory);
assert.ok(directoryStat.isDirectory() && !directoryStat.isSymbolicLink(), directory);
assert.equal(directoryStat.uid, 0, directory + " uid");
assert.equal(directoryStat.gid, 0, directory + " gid");
assert.equal(directoryStat.mode & 0o7777, 0o755, directory + " mode");

const helpers = [
  "/app/templates/pod-epoch-init.cjs",
  "/usr/local/lib/uzi/pod-epoch-init.cjs",
];
const contents = helpers.map((file) => {
  const stat = lstatSync(file);
  assert.ok(stat.isFile() && !stat.isSymbolicLink(), file);
  assert.equal(stat.uid, 0, file + " uid");
  assert.equal(stat.gid, 0, file + " gid");
  assert.equal(stat.mode & 0o7777, 0o644, file + " mode");
  return readFileSync(file);
});
assert.ok(contents[0].equals(contents[1]), "baked epoch helpers must match");

// Exercise the real baked wrapper's relative CJS import without opening an epoch.
const { createPodEpoch } = await import("/app/src/pod-epoch-lock.ts");
assert.equal(typeof createPodEpoch, "function", "baked createPodEpoch export");

const link = "/app/uzi-worker-uid-proof/.uzi";
const target = "/proof/.uzi";
try {
  symlinkSync(target, link);
} catch (error) {
  if (error.code !== "EEXIST" || readlinkSync(link) !== target) throw error;
}
