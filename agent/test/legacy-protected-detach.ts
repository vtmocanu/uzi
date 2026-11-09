import type { GitCache, RecoveryJournalEntry } from "../src/git.js";

/**
 * Test-only reproduction of the LEGACY (pre-#4058) `GitCache.detachTerminalRetained` output.
 *
 * Production `detachTerminalRetained` now refuses a journal carrying an active remediation record
 * (RemediationRecoveryRetainedError). Installs upgraded from earlier versions can still hold protected
 * `uzi-retained.<run>.journal` descriptors that carry remediation records, written by the old detach, so the
 * readers of that state must stay tested. This helper performs the same config writes the old function did, in
 * the same order, through GitCache's private seams (typed cast, test code only):
 * attempt-mode marker, protected descriptor `JSON.stringify({version:1, branch, key, journal})`, `reclaimed`
 * attempt-ledger appends, then clearing the active journal key. The residue-quarantine guards of the old
 * function are omitted: they only refuse, they never write, and these fixtures run without a quarantine.
 */
export async function legacyProtectedDetach(
  cache: GitCache, barePath: string, branch: string, key: string, expectedIn: RecoveryJournalEntry,
): Promise<void> {
  const c = cache as any;
  let expected: RecoveryJournalEntry = expectedIn;
  await c.withLock(barePath, async () => {
    // The old function normalised `expected` through the private parseRecoveryJournal (not exported). Fixture
    // callers pass the journal read back from this same config key, so the parsed active capture is that value.
    const parsed = await c.readRecoveryCapture(barePath, branch);
    if (parsed && parsed.runId === expectedIn.runId) expected = parsed;
    await c.refuseMarkedPaths(barePath, c.recoverySources(expected).map((x: { clonePath: string }) => x.clonePath), expected.runId);
    const records: Array<{ branch: string; key: string; journal: RecoveryJournalEntry }> = await c.protectedRecoveries(barePath);
    const prior = records.find(r => r.journal.runId === expected.runId);
    const value = JSON.stringify({ version: 1, branch, key, journal: expected });
    if (prior && JSON.stringify({ version: 1, ...prior }) !== value) throw new Error("conflicting protected recovery descriptor");
    const current = await c.readRecoveryCapture(barePath, branch);
    if (prior) await c.checkedRecovery(barePath, branch, key, expected, true, prior.journal);
    if (prior && (!current || current.runId !== expected.runId)) return;
    if (!current || JSON.stringify(current) !== JSON.stringify(expected)) throw new Error("terminal recovery snapshot changed");
    await c.checkedRecovery(barePath, branch, key, expected, true);
    if (!await c.readRecoveryAttemptMode(barePath, key)) await c.runGit(barePath, ["config", "--local", `uzi-attemptmode.${key}.enabled`, "true"]);
    if (!await c.readRecoveryAttemptMode(barePath, key)) throw new Error("attempt mode persistence failed");
    if (!prior) await c.runGit(barePath, ["config", "--local", `uzi-retained.${expected.runId}.journal`, value]);
    const readback = (await c.protectedRecoveries(barePath)).find((r: { journal: RecoveryJournalEntry }) => r.journal.runId === expected.runId);
    if (!readback || JSON.stringify({ version: 1, ...readback }) !== value) throw new Error("protected recovery readback mismatch");
    await c.checkedRecovery(barePath, branch, key, expected, true, readback.journal);
    for (const source of c.recoverySources(expected) as Array<{ attemptId?: string; runId: string; clonePath: string }>) {
      if (source.attemptId) await c.appendAttemptLedger(barePath, branch, {
        attemptId: source.attemptId, runId: source.runId, clonePath: source.clonePath, state: "reclaimed",
      });
    }
    const final = await c.readRecoveryCapture(barePath, branch);
    if (!final || JSON.stringify(final) !== JSON.stringify(expected)) throw new Error("terminal recovery snapshot changed");
    await c.checkedRecovery(barePath, branch, key, expected, true);
    await c.runGit(barePath, ["config", "--local", `uzi-recovery.${branch}.clone`, ""]);
  });
}
