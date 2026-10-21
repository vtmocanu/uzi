// Helper for runner-shared-real.test.mts: drives the production retention delete
// (GitCache.deleteRetainedArtifact, the path operator discard and the attempt sweeps share) as
// whatever uid this process was started as. Under UZI_UID_SPLIT=1 the delete is the real
// setpriv-to-runner `rm -rf`; without it the in-process fs.rm of the single-uid posture. Prints
// one JSON line: the result, the process-wide undeletable counter before and after, and the
// retention log lines.
import path from "node:path";
import { GitCache, retainedUndeletableCount } from "../../agent/src/git.js";
import { recordingLogger, testGitCacheOptions } from "../../agent/test/helpers.js";

const spec = JSON.parse(process.argv[2]!) as { dataDir: string; parent: string; target: string; guard: "allow" | "veto" };
const rec = recordingLogger();
const git = new GitCache(spec.dataDir, rec.logger, undefined, testGitCacheOptions());
const seam = git as unknown as {
  deleteRetainedArtifact(parent: string, target: string, isExpected: (name: string) => boolean,
    opts?: { beforeDelete?: () => Promise<boolean>; canDelete?: () => boolean }): Promise<boolean>;
};
const before = retainedUndeletableCount();
// The writer guards the real callers pass: a journal/liveness recheck and the synchronous
// destruction-boundary veto. "veto" models a guard that sees a live writer.
const guards = spec.guard === "veto"
  ? { beforeDelete: async () => true, canDelete: () => false }
  : { beforeDelete: async () => true, canDelete: () => true };
const deleted = await seam.deleteRetainedArtifact(spec.parent, spec.target, (name) => name === path.basename(spec.target), guards);
console.log(JSON.stringify({
  uid: process.getuid?.(),
  split: process.env.UZI_UID_SPLIT === "1",
  deleted,
  before,
  after: retainedUndeletableCount(),
  lines: rec.lines.filter((line) => String((line as { msg?: string }).msg).startsWith("runner clone retention")),
}));
