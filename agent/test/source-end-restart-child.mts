// Fresh-process authority setup, following completion-authority.test.ts's setupInChild.
// This helper models durable worker state only; it does not simulate a server commit or ACK.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { acquireWorkerLifetime, withAuthorityTransaction, type AuthorityTransaction } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type AuthorityFsOps } from "../src/completion-authority.js";
import { testLockOptions } from "./authority-test-support.js";

const [root, token, mode] = process.argv.slice(2);
assert.ok(root && token);
assert.ok(mode === "declined" || mode === "active-ahead" || mode === "ended-ahead");
const lifetime = await acquireWorkerLifetime(root, testLockOptions());
let failHead = false;
const ops: AuthorityFsOps = {
  syncFile: h => h.sync(), syncDir: h => h.sync(),
  rename: async (a, b) => {
    if (failHead && path.basename(path.dirname(b)) === "heads") throw new Error("injected head failure");
    fs.renameSync(a, b);
  },
};
const rt = new AuthorityRuntime({ lifetime, workerToken: token, timeoutMs: 5000 });
const tx = <T,>(fn: (t: AuthorityTransaction) => Promise<T>) =>
  withAuthorityTransaction(lifetime, { timeoutMs: 5000 }, fn);
const store = await tx(t => openAuthorityStore(t, { workerToken: token, ops }));
const sources = [path.join(root, "runner", "first"), path.join(root, "runner", "second")];
for (const source of sources) fs.mkdirSync(source, { recursive: true });
// Keep injected writes in distinct head shards so one source's later head write
// cannot complete its sibling's crash window. At most 256 candidate paths.
const firstKey = await tx(t => store.slotKey(t, sources[0]!));
for (let attempt = 0; attempt < 256; attempt++) {
  const secondKey = await tx(t => store.slotKey(t, sources[1]!));
  if (secondKey.slice(0, 2) !== firstKey.slice(0, 2)) break;
  assert.ok(attempt < 255, "could not allocate distinct source head shards");
  sources[1] = path.join(root, "runner", "second-" + attempt);
  fs.mkdirSync(sources[1], { recursive: true });
}
if (mode === "declined") {
  const sink = new Set<string>();
  for (const source of sources) await rt.recordAdmissions(sink, () => rt.bindSource(source));
  rt.retireSink(sink);
  assert.equal(await rt.awaitSinkQuiescent(), true);
  rt.markSourceEndDeclined(sources, sink);
  for (const source of sources) assert.equal(await rt.sourceEndDeclined(source, new Set()), true);
} else {
  // Both failures use the production atomic-write injection seam: slot rename succeeds,
  // head rename fails. No authenticated JSON is fabricated or rolled back.
  for (const source of sources) {
    failHead = mode === "active-ahead";
    if (failHead) {
      await assert.rejects(tx(t => store.admitSourceUse(t, source)), /injected head failure/);
    } else {
      const lease = await tx(t => store.admitSourceUse(t, source));
      failHead = true;
      await assert.rejects(tx(t => store.transition(t, lease, "ineligible")), /injected head failure/);
    }
    failHead = false;
  }
}
const slots = [];
for (const source of sources) {
  const key = await tx(t => store.slotKey(t, source));
  const slot = JSON.parse(fs.readFileSync(path.join(root, ".uzi-authority", "slots", key + ".json"), "utf8"));
  slots.push({ source, slot });
}
process.stdout.write(JSON.stringify({ pid: process.pid, sources, slots }) + "\n");
