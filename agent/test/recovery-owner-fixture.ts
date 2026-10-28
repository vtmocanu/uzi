import path from "node:path";
import os from "node:os";
import { GitCache, type RecoveryJournalEntry } from "../src/git.js";
import { recoveryProgress, sameRecoverySource, type RecoverySource } from "../src/recovery-progress.js";
import { nullLogger } from "./helpers.js";

// Persistent config IO seam; reservation, strict parsers, charging and blocking are production.
export function ownerFixture() {
  const bare = path.join(os.tmpdir(), "owner-bare");
  const source = { runId: "owner-run", clonePath: path.join(os.tmpdir(), "owner-source") };
  const branch = "agent/issue-3527", key = "issue-3527";
  const journalKey = `uzi-recovery.${branch}.clone`;
  const ownerKey = `uzi-recoveryowner.${source.runId}.record`;
  const values = new Map<string, string[]>([[journalKey, [JSON.stringify(source)]]]);
  let fault: ((args: string[], phase: "before" | "after") => void) | undefined;
  const read = (): RecoveryJournalEntry => JSON.parse(values.get(journalKey)![0]!);
  function recreate() {
    const cache = new GitCache(path.join(os.tmpdir(), "owner-cache"), nullLogger());
    Object.assign(cache, {
      withLock: async (_bare: string, action: () => Promise<unknown>) => action(),
      assertRecoveryBare: async () => {},
      recoveryOwnerBareIdentity: async () => JSON.stringify([bare, "1", "2"]),
      checkedRecovery: async (_bare: string, _branch: string, _key: string, expected: RecoverySource) => {
        const j = read();
        if (!sameRecoverySource(j, expected)) throw new Error("recovery identity changed");
        if (j.recovery) recoveryProgress(j.recovery);
        return j;
      },
      runGit: async (_cwd: string, args: string[]) => {
        fault?.(args, "before");
        let result = "";
        if (args.includes("--list")) {
          result = [...values].flatMap(([k, entries]) => entries.map(v => k + "\n" + v)).join("\0") + "\0";
        } else if (args[0] === "config") {
          values.set(args.at(-2)!, [args.at(-1)!]);
        } else throw new Error("unexpected Git operation: " + args.join(" "));
        fault?.(args, "after");
        return result;
      },
    });
    return cache;
  }
  let ownershipProofs = 0;
  const proveOwnership = async () => { ownershipProofs++; };
  const reserve = (generation = 1, grant: number | undefined = 0, episode = 7, cache = recreate()) =>
    cache.reserveRecoveryIteration(bare, branch, key, read(), 1000, generation, episode, grant, proveOwnership);
  return { bare, branch, key, source, journalKey, ownerKey, values, read, recreate, reserve, proveOwnership, ownershipProofs: () => ownershipProofs,
    write: (j: RecoveryJournalEntry) => values.set(journalKey, [JSON.stringify(j)]),
    fault: (f?: typeof fault) => { fault = f; },
    block: async (cache = recreate()) => {
      const j = read();
      await cache.blockRecoveryEpisode(bare, branch, key, j, "adoption_failed", j.recovery);
    } };
}
