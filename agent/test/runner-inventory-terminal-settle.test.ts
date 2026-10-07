import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();
function command(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();
}
function commit(dir: string, name: string): string {
  fs.writeFileSync(path.join(dir, name), name + "\n");
  command(dir, ["add", name]);
  command(dir, ["-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit", "-m", name]);
  return command(dir, ["rev-parse", "HEAD"]);
}

// Guarded server fake: one open exact-generation hold, a reservable/uploadable capture, and a
// final release that closes the hold.
function guardedServer(claim: ReturnType<typeof gitlabClaim>, status: () => string) {
  const feature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
  client.getRunOwnership = async () => ({
    status: status(), claim_generation: claim.claim_generation, inventory_guarded: true,
  });
  const serverId = "00000000-0000-4000-8000-000000001945";
  let manifest: { checksum: string; byte_size: number } | undefined;
  let finals = 0, open = true;
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
    hold_id: "inventory-hold", generation: claim.claim_generation!, inventory_guarded: true,
    has_available_capture: false,
  }] : [] });
  client.reserveRecoveryCapture = async () => ({ capture_id: serverId, state: "preparing" });
  client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
    for await (const chunk of stream) assert.ok(chunk);
    manifest = m;
    return { capture_id: serverId, state: "available", manifest_bound: true };
  };
  client.getRecoveryCaptureStatus = async () => ({
    capture_id: serverId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
    checksum: manifest?.checksum, byte_size: manifest?.byte_size,
    expires_at: new Date(Date.now() + 60_000).toISOString(),
  });
  client.releaseRecoveryCustody = async () => {
    finals++;
    open = false;
    return { run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1 };
  };
  return { finals: () => finals };
}

// The pre-report terminal snapshot proves the clone quiescent and uploads the archive. The
// post-report terminal drive must reuse that proof: it runs while execute() still holds the run's
// execution lane, so without it the freeze is refused, the clone is marked preserved and the
// finalize cleanup (quiescence, settlement, clone retirement) is skipped for every finished run.
it(`guarded completed run whose inventory was released retires its clone`, async () => {
  const claim = gitlabClaim(1962, { claim_generation: 29, inventory_guarded: true });
  const { gitlab } = fakeGitlab();
  guardedServer(claim, () => api.states.some(s => s.body.status === "completed") ? "completed" : "running");
  let clone = "";
  const r = runner({ run: async ctx => {
    clone = ctx.worktreePath;
    commit(clone, "released.txt");
    return { branch: ctx.branch };
  } }, gitlab, "journal-key");
  await r.execute(claim);
  assert.equal(fs.existsSync(clone), false, "the released run's clone is retired");
});
