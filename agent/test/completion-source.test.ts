import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { acquireWorkerLifetime, type AuthorityTransaction, withAuthorityTransaction } from "../src/authority-lock.js";
import { type AuthorityStore, openAuthorityStore, type SourceUseLease } from "../src/completion-authority.js";
import {
  buildClaudeSourceCore, buildCodexSourceCore, certifySourceCore, type CompletionSourceV2, parseCompletionSourceV2,
  parseSourceFence, parseSourceFenceJson, serializeSourceFence, sourceCoreDigest, sourceFenceOf, verifyCertifiedSource,
} from "../src/completion-source.js";
import type { ExecutorSealMaterial } from "../src/codex/executor-lifetime.js";
import type { CompletionSource } from "../src/recovery.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const SHA = "a".repeat(40);

interface Fixture {
  root: string; store: AuthorityStore; lease: SourceUseLease; tx<T>(fn: (t: AuthorityTransaction) => Promise<T>): Promise<T>;
  clone: string; base: CompletionSource;
}
async function fixture(t: TestContext, token = TOKEN): Promise<Fixture> {
  const root = makeTmp(t);
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  const run = <T>(fn: (t2: AuthorityTransaction) => Promise<T>): Promise<T> => withAuthorityTransaction(lifetime, { timeoutMs: 5000 }, fn);
  const store = await run((t2) => openAuthorityStore(t2, { workerToken: token }));
  const clone = path.join(root, "runner", "repo-a");
  fs.mkdirSync(clone, { recursive: true });
  const lease = await run((t2) => store.admitSourceUse(t2, clone));
  const context = {
    runId: "run-1", branch: "agent/issue-1", kind: "issue" as const, barePath: path.join(root, "bare"), generation: 2,
    defaultIdentity: { ref: "refs/heads/main", sha: SHA },
  };
  const base: CompletionSource = {
    context, clonePath: clone, attemptId: "20260101T000000Z-g2-0123456789abcdef", expectedHead: SHA,
    roots: [{ sha: "b".repeat(40), contexts: [context] }],
  };
  return { root, store, lease, tx: run, clone, base };
}

const material: ExecutorSealMaterial = {
  sourceUse: {}, executorIncarnation: "exec-" + randomUUID(),
  incarnations: [{ registry: "reg-b", facade: "fac-b" }, { registry: "reg-a", facade: "fac-a" }],
};
const expectation = (f: Fixture): { dataDir: string; storageId: string; layout: string } =>
  ({ dataDir: f.root, storageId: f.store.storageId, layout: f.store.layout });
const codex = (f: Fixture, m: ExecutorSealMaterial = material): CompletionSourceV2 =>
  buildCodexSourceCore({ store: f.store, base: f.base, lease: f.lease, material: m });
const claude = (f: Fixture): CompletionSourceV2 => buildClaudeSourceCore({ store: f.store, base: f.base, lease: f.lease });
const clone = <T>(v: T): T => structuredClone(v);

test("builders produce valid sorted cores that round-trip through the strict parser", async (t) => {
  const f = await fixture(t);
  const source = codex(f);
  assert.equal(source.version, 2);
  assert.equal(source.relativeSource, "runner/repo-a");
  assert.deepEqual(source.registryIncarnations, ["reg-a", "reg-b"]);
  assert.deepEqual(source.facadeIncarnations, ["fac-a", "fac-b"]);
  assert.equal(source.executorIncarnation, material.executorIncarnation);
  assert.equal(source.workerIncarnation, f.store.workerIncarnation);
  assert.deepEqual(parseCompletionSourceV2(JSON.parse(JSON.stringify(source)), expectation(f)), source);
  const c = claude(f);
  assert.equal(c.harness, "claude");
  for (const key of ["executorIncarnation", "registryIncarnations", "facadeIncarnations"]) assert.equal(key in c, false);
  assert.deepEqual(parseCompletionSourceV2(clone(c), expectation(f)), c);
});

test("the validator rejects every malformed or unknown field", async (t) => {
  const f = await fixture(t);
  const good = codex(f) as unknown as Record<string, unknown>;
  const refused = (mutate: (o: Record<string, any>) => void, why: string): void => {
    const copy = clone(good) as Record<string, any>;
    mutate(copy);
    assert.throws(() => parseCompletionSourceV2(copy, expectation(f)), /malformed completion source/, why);
  };
  refused((o) => { o.extra = 1; }, "unknown top-level key");
  for (const key of ["version", "context", "clonePath", "expectedHead", "roots", "storageId", "layout", "relativeSource",
    "harness", "workerIncarnation", "sourceUseEpoch", "executorIncarnation", "registryIncarnations", "facadeIncarnations"]) {
    refused((o) => { delete o[key]; }, `missing ${key}`);
    refused((o) => { o[key] = undefined; }, `undefined ${key}`);
  }
  refused((o) => { o.version = 1; }, "version");
  refused((o) => { o.harness = "other"; }, "harness");
  refused((o) => { o.expectedHead = "A".repeat(40); }, "head case");
  refused((o) => { o.expectedHead = "a".repeat(39); }, "head length");
  refused((o) => { o.storageId = randomUUID(); }, "other storage");
  refused((o) => { o.storageId = "x"; }, "storage format");
  refused((o) => { o.layout = "uzi-authority-layout/2"; }, "layout");
  refused((o) => { o.workerIncarnation = "not-a-uuid"; }, "worker incarnation");
  refused((o) => { o.sourceUseEpoch = "not-a-uuid"; }, "epoch");
  refused((o) => { o.certificate = "abc"; }, "certificate format");
  refused((o) => { o.attemptId = "20260101T000000Z-g9-0123456789abcdef"; }, "attempt generation");
  refused((o) => { o.attemptId = 7; }, "attempt type");
  refused((o) => { o.clonePath = "relative/path"; }, "relative clone");
  refused((o) => { o.clonePath = `${f.root}/runner/../runner/repo-a`; }, "unnormalized clone");
  refused((o) => { o.clonePath = path.join(f.root, "runner", "other"); }, "clone inconsistent with relative");
  refused((o) => { o.relativeSource = "runner/../runner/repo-a"; }, "unnormalized relative");
  refused((o) => { o.relativeSource = "/runner/repo-a"; }, "absolute relative");
  refused((o) => { o.relativeSource = ".uzi-authority/slots"; o.clonePath = path.join(f.root, ".uzi-authority/slots"); }, "authority storage");
  refused((o) => { o.relativeSource = "x".repeat(5000); }, "oversized relative");
  refused((o) => { o.context.extra = 1; }, "context extra");
  refused((o) => { o.context.kind = "chat"; }, "context kind");
  refused((o) => { o.context.generation = 0; }, "context generation");
  refused((o) => { o.context.defaultIdentity.ref = "heads/main"; }, "default ref");
  refused((o) => { o.context.legacy = true; }, "legacy context");
  refused((o) => { o.roots[0].sha = "z".repeat(40); }, "root sha");
  refused((o) => { o.roots[0].extra = 1; }, "root extra");
  refused((o) => { o.roots[0].contexts = []; }, "root contexts empty");
  refused((o) => { o.roots[0].contexts[0].runId = "other"; }, "root context foreign run");
  refused((o) => { o.roots = {}; }, "roots type");
  refused((o) => { o.executorIncarnation = "bad id"; }, "executor format");
  refused((o) => { o.registryIncarnations = ["b", "a"]; }, "unsorted registries");
  refused((o) => { o.facadeIncarnations = ["a", "a"]; }, "duplicate facades");
  refused((o) => { o.facadeIncarnations = "fac-a"; }, "facade type");
  refused((o) => { o.registryIncarnations = Array.from({ length: 257 }, (_, i) => String(i).padStart(4, "0")); }, "too many");
  assert.throws(() => parseCompletionSourceV2([], expectation(f)), /malformed/);
  assert.throws(() => parseCompletionSourceV2(Object.create({ version: 2 }), expectation(f)), /malformed/);
  assert.throws(() => parseCompletionSourceV2(good, { ...expectation(f), dataDir: path.join(f.root, "elsewhere") }), /malformed/);
});

test("codex requires the incarnations and claude must not carry them", async (t) => {
  const f = await fixture(t);
  const withExtras = { ...clone(claude(f)), executorIncarnation: "e", registryIncarnations: [], facadeIncarnations: [] };
  assert.throws(() => parseCompletionSourceV2(withExtras, expectation(f)), /malformed/);
  for (const key of ["executorIncarnation", "registryIncarnations", "facadeIncarnations"]) {
    assert.throws(() => parseCompletionSourceV2({ ...clone(claude(f)), [key]: key === "executorIncarnation" ? "e" : [] }, expectation(f)),
      /malformed/, key);
    const missing = clone(codex(f)) as unknown as Record<string, unknown>;
    delete missing[key];
    assert.throws(() => parseCompletionSourceV2(missing, expectation(f)), /malformed/, key);
  }
  const noIncarnations = clone(claude(f)) as unknown as Record<string, unknown>;
  noIncarnations.harness = "codex";
  assert.throws(() => parseCompletionSourceV2(noIncarnations, expectation(f)), /malformed/);
});

test("the core digest is canonical, domain separated, excludes the certificate and covers every swapped incarnation", async (t) => {
  const f = await fixture(t);
  const source = codex(f);
  const digest = sourceCoreDigest(source);
  assert.match(digest, /^[0-9a-f]{64}$/);
  assert.equal(sourceCoreDigest(codex(f)), digest);
  const reordered = JSON.parse(JSON.stringify(Object.fromEntries(Object.entries(source).reverse()))) as CompletionSourceV2;
  assert.equal(sourceCoreDigest(reordered), digest, "key order is irrelevant");
  assert.equal(sourceCoreDigest({ ...source, certificate: "f".repeat(64) }), digest);

  const swapped = codex(f, { ...material, incarnations: [{ registry: "fac-a", facade: "reg-a" }, { registry: "fac-b", facade: "reg-b" }] });
  assert.notEqual(sourceCoreDigest(swapped), digest, "registry and facade ids are not interchangeable");
  const otherExecutor = codex(f, { ...material, executorIncarnation: material.executorIncarnation + "x" });
  assert.notEqual(sourceCoreDigest(otherExecutor), digest);
  const executorAsRegistry = codex(f, { ...material, executorIncarnation: "reg-a", incarnations: [{ registry: "exec-x", facade: "fac-a" }] });
  const registryAsExecutor = codex(f, { ...material, executorIncarnation: "exec-x", incarnations: [{ registry: "reg-a", facade: "fac-a" }] });
  assert.notEqual(sourceCoreDigest(executorAsRegistry), sourceCoreDigest(registryAsExecutor));
  assert.notEqual(sourceCoreDigest({ ...source, expectedHead: "c".repeat(40) }), digest);
  assert.notEqual(sourceCoreDigest({ ...source, sourceUseEpoch: randomUUID() }), digest);
  assert.notEqual(sourceCoreDigest({ ...source, workerIncarnation: randomUUID() }), digest);
  assert.notEqual(sourceCoreDigest(claude(f)), digest);
});

test("certificates verify, and tamper or another store key fails", async (t) => {
  const f = await fixture(t);
  const certified = await f.tx(async (t2) => certifySourceCore(t2, f.store, codex(f)));
  assert.match(certified.certificate ?? "", /^[0-9a-f]{64}$/);
  const verified = await f.tx(async (t2) => verifyCertifiedSource(t2, f.store, JSON.parse(JSON.stringify(certified))));
  assert.deepEqual(verified, certified);

  const tampers: Array<(o: Record<string, any>) => void> = [
    (o) => { o.expectedHead = "c".repeat(40); },
    (o) => { o.sourceUseEpoch = randomUUID(); },
    (o) => { o.facadeIncarnations = ["fac-z"]; },
    (o) => { o.executorIncarnation = "exec-other"; },
    (o) => { o.certificate = (o.certificate as string).replace(/^./, (c: string) => (c === "0" ? "1" : "0")); },
  ];
  for (const tamper of tampers) {
    const copy = clone(certified) as unknown as Record<string, any>;
    tamper(copy);
    await assert.rejects(f.tx(async (t2) => verifyCertifiedSource(t2, f.store, copy)), /certificate does not verify/);
  }
  const bare = clone(certified) as unknown as Record<string, unknown>;
  delete bare.certificate;
  await assert.rejects(f.tx(async (t2) => verifyCertifiedSource(t2, f.store, bare)), /certificate does not verify/);

  // The same layout under a different worker token derives a different key.
  const other = await fixture(t, TOKEN + "-other");
  const foreign = await other.tx(async (t2) => certifySourceCore(t2, other.store, codex(other)));
  const transplanted = { ...foreign, storageId: f.store.storageId, relativeSource: certified.relativeSource, clonePath: certified.clonePath };
  await assert.rejects(f.tx(async (t2) => verifyCertifiedSource(t2, f.store, transplanted)), /malformed|certificate does not verify/);
  assert.equal(
    await f.tx(async (t2) => f.store.verifySourceCertificate(t2, sourceCoreDigest(certified), foreign.certificate)), false,
    "a certificate minted under another store key never verifies, even for the same digest",
  );
  const sameDigest = await other.tx(async (t2) => other.store.certifySource(t2, sourceCoreDigest(certified)));
  assert.equal(await f.tx(async (t2) => f.store.verifySourceCertificate(t2, sourceCoreDigest(certified), sameDigest)), false);
});

test("the fence is exactly three fields and parses strictly", async (t) => {
  const f = await fixture(t);
  const source = codex(f);
  const fence = sourceFenceOf(source);
  assert.deepEqual(fence, { version: 1, sourceUseEpoch: source.sourceUseEpoch, sourceCoreDigest: sourceCoreDigest(source) });
  const text = serializeSourceFence(fence);
  assert.equal(text, `{"version":1,"sourceUseEpoch":"${fence.sourceUseEpoch}","sourceCoreDigest":"${fence.sourceCoreDigest}"}`);
  assert.deepEqual(parseSourceFenceJson(text), fence);
  const reversed = { sourceCoreDigest: fence.sourceCoreDigest, sourceUseEpoch: fence.sourceUseEpoch, version: 1 as const };
  assert.equal(serializeSourceFence(reversed), text, "serialization order is fixed whatever the input order");
  assert.throws(() => parseSourceFenceJson(JSON.stringify(reversed)), /malformed/, "a reordered spelling is not canonical");
  assert.throws(() => parseSourceFenceJson(text.replace(",", ", ")), /malformed/);
  assert.throws(() => parseSourceFenceJson("{"), /malformed/);
  assert.throws(() => parseSourceFence({ ...fence, extra: 1 }), /malformed/);
  assert.throws(() => parseSourceFence({ ...fence, version: 2 }), /malformed/);
  assert.throws(() => parseSourceFence({ version: 1, sourceUseEpoch: fence.sourceUseEpoch }), /malformed/);
  assert.throws(() => parseSourceFence({ ...fence, sourceCoreDigest: "A".repeat(64) }), /malformed/);
  assert.throws(() => parseSourceFence({ ...fence, sourceUseEpoch: "x" }), /malformed/);
  assert.throws(() => parseSourceFence(null), /malformed/);
});
