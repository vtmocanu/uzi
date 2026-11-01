import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { acquireWorkerLifetime, AuthorityRefusal, type AuthorityTransaction, withAuthorityTransaction } from "../src/authority-lock.js";
import { type AuthorityStore, leaseEpoch, openAuthorityStore, type SourceUseLease } from "../src/completion-authority.js";
import {
  buildClaudeSourceCore, buildCodexSourceCore, type CompletionSourceV2, parseCompletionSourceV2,
  parseSourceFence, parseSourceFenceJson, serializeSourceFence, sourceCoreDigest, sourceFenceOf, verifyCertifiedSource,
  verifyPublishedSource,
} from "../src/completion-source.js";
import { ExecutorLifetime } from "../src/codex/executor-lifetime.js";
import { type CompletionSource, validCompletionSource } from "../src/recovery.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");
const SHA = "a".repeat(40);
const CAP = ["11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"];

interface Fixture {
  root: string; store: AuthorityStore; lease: SourceUseLease; tx<T>(fn: (t: AuthorityTransaction) => Promise<T>): Promise<T>;
  clone: string; base: CompletionSource;
  /** A codex-admitted slot: its lease, the lifetime that sealed it and the terminal seal. */
  codexLease: SourceUseLease; codexBase: CompletionSource; lifetime: ExecutorLifetime; seal: unknown;
  /** Make the sealed facade's own check fail from now on: the seal is revoked after it was issued. */
  revoke(): void;
}
async function fixture(t: TestContext, token = TOKEN): Promise<Fixture> {
  const root = makeTmp(t);
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  const run = <T>(fn: (t2: AuthorityTransaction) => Promise<T>): Promise<T> => withAuthorityTransaction(lifetime, { timeoutMs: 5000 }, fn);
  const store = await run((t2) => openAuthorityStore(t2, { workerToken: token }));
  const clone = path.join(root, "runner", "repo-a");
  const codexClone = path.join(root, "runner", "repo-x");
  fs.mkdirSync(clone, { recursive: true });
  fs.mkdirSync(codexClone, { recursive: true });
  const lease = await run((t2) => store.admitSourceUse(t2, clone));
  const executor = new ExecutorLifetime();
  const executorId = executor.executorIncarnation;
  const codexLease = await run((t2) => store.admitSourceUse(t2, codexClone, { executor: executorId }));
  const identity = {};
  executor.inventory(identity, () => true);
  let facadeOk = true;
  executor.bindFacade(identity, {}, () => facadeOk);
  const sealed = await executor.seal(codexLease, async () => undefined, () => true);
  assert.equal(sealed.kind, "sealed");
  const context = {
    runId: "run-1", branch: "agent/issue-1", kind: "issue" as const, barePath: path.join(root, "bare"), generation: 2,
    defaultIdentity: { ref: "refs/heads/main", sha: SHA },
  };
  const base: CompletionSource = {
    context, clonePath: clone, attemptId: "20260101T000000Z-g2-0123456789abcdef", expectedHead: SHA,
    roots: [{ sha: "b".repeat(40), contexts: [context] }],
  };
  return {
    root, store, lease, tx: run, clone, base, codexLease, codexBase: { ...base, clonePath: codexClone }, lifetime: executor,
    seal: sealed.kind === "sealed" ? sealed.seal : undefined, revoke: () => { facadeOk = false; },
  };
}

const expectation = (f: Fixture): { dataDir: string; storageId: string; layout: string } =>
  ({ dataDir: f.root, storageId: f.store.storageId, layout: f.store.layout });
const codex = (f: Fixture): CompletionSourceV2 => buildCodexSourceCore({
  store: f.store, base: f.codexBase, lease: f.codexLease, seal: f.seal,
});
const codexSeal = (f: Fixture): { seal: unknown } => ({ seal: f.seal });
const claude = (f: Fixture): CompletionSourceV2 => buildClaudeSourceCore({ store: f.store, base: f.base, lease: f.lease });
const clone = <T>(v: T): T => structuredClone(v);

test("builders produce valid sorted cores that round-trip through the strict parser", async (t) => {
  const f = await fixture(t);
  const source = codex(f);
  assert.equal(source.version, 2);
  assert.equal(source.relativeSource, "runner/repo-x");
  assert.equal(source.registryFacadePairs?.length, 1);
  assert.equal(source.workerIncarnation, f.store.workerIncarnation);
  assert.deepEqual(parseCompletionSourceV2(JSON.parse(JSON.stringify(source)), expectation(f)), source);
  const c = claude(f);
  assert.equal(c.harness, "claude");
  assert.equal(c.relativeSource, "runner/repo-a");
  for (const key of ["executorIncarnation", "registryFacadePairs"]) assert.equal(key in c, false);
  assert.deepEqual(parseCompletionSourceV2(clone(c), expectation(f)), c);
});

test("the codex builder takes only an opaque seal bound to this lease that the executor still accepts", async (t) => {
  const f = await fixture(t);
  const build = (over: Record<string, unknown>): CompletionSourceV2 => buildCodexSourceCore({
    store: f.store, base: f.codexBase, lease: f.codexLease, seal: f.seal, ...over,
  });
  const refused = (over: Record<string, unknown>, re: RegExp): void => assert.throws(() => build(over), re);
  const material = { sourceUse: f.codexLease, executorIncarnation: "exec-x", incarnations: [{ registry: "r", facade: "f" }] };
  refused({ seal: material }, /not a terminal seal/);
  refused({ seal: Object.freeze({}) }, /not a terminal seal/);
  refused({ seal: undefined }, /not a terminal seal/);
  refused({ lease: f.lease }, /another source use/);
  // A caller-supplied acceptance callback is ignored: only the seal's own lifetime answers.
  refused({ accepts: () => true, seal: Object.freeze({}) }, /not a terminal seal/);
  assert.equal(build({}).harness, "codex");
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
    "harness", "workerIncarnation", "sourceUseEpoch", "executorIncarnation", "registryFacadePairs"]) {
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
  refused((o) => { o.clonePath = `${f.root}/runner/../runner/repo-x`; }, "unnormalized clone");
  refused((o) => { o.clonePath = path.join(f.root, "runner", "other"); }, "clone inconsistent with relative");
  refused((o) => { o.relativeSource = "runner/../runner/repo-x"; }, "unnormalized relative");
  refused((o) => { o.relativeSource = "/runner/repo-x"; }, "absolute relative");
  refused((o) => { o.relativeSource = ".uzi-authority/slots"; o.clonePath = path.join(f.root, ".uzi-authority/slots"); }, "authority storage");
  refused((o) => { o.relativeSource = "x".repeat(5000); }, "oversized relative");
  refused((o) => { o.relativeSource = "../x"; o.clonePath = path.join(path.dirname(f.root), "x"); }, "relative with a leading .. outside the data root");
  refused((o) => { o.relativeSource = "../../x"; o.clonePath = path.join(path.dirname(path.dirname(f.root)), "x"); }, "relative escaping two levels");
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
  refused((o) => { o.registryFacadePairs = [["b", "x"], ["a", "y"]]; }, "unsorted registries");
  refused((o) => { o.registryFacadePairs = [["a", "x"], ["a", "y"]]; }, "duplicate registries");
  refused((o) => { o.registryFacadePairs = [["a", "x"], ["b", "x"]]; }, "duplicate facades");
  refused((o) => { o.registryFacadePairs = [["a"]]; }, "short pair");
  refused((o) => { o.registryFacadePairs = [["a", "x", "z"]]; }, "long pair");
  refused((o) => { o.registryFacadePairs = [["a", "bad id"]]; }, "pair format");
  refused((o) => { o.registryFacadePairs = "reg-a"; }, "pairs type");
  refused((o) => { o.registryFacadePairs = Array.from({ length: 257 }, (_, i) => [String(i).padStart(4, "0"), `f${i}`]); }, "too many");
  assert.throws(() => parseCompletionSourceV2([], expectation(f)), /malformed/);
  assert.throws(() => parseCompletionSourceV2(Object.create({ version: 2 }), expectation(f)), /malformed/);
  assert.throws(() => parseCompletionSourceV2(good, { ...expectation(f), dataDir: path.join(f.root, "elsewhere") }), /malformed/);
});

test("the V1 core validator and the V2 decoder agree on every core mutation", async (t) => {
  const f = await fixture(t);
  const good = clone(claude(f)) as unknown as Record<string, any>;
  const v1Of = (o: Record<string, any>): unknown => {
    const out: Record<string, unknown> = {};
    for (const key of ["context", "clonePath", "expectedHead", "roots", "attemptId"]) if (o[key] !== undefined) out[key] = o[key];
    return out;
  };
  assert.equal(validCompletionSource(v1Of(good)), true);
  const mutations: Array<(o: Record<string, any>) => void> = [
    (o) => { o.context.extra = 1; }, (o) => { o.context.legacy = true; }, (o) => { o.context.origin = "historical"; },
    (o) => { o.context.kind = "chat"; }, (o) => { o.context.generation = 0; }, (o) => { o.context.generation = 1.5; },
    (o) => { o.context.branch = ""; }, (o) => { o.context.barePath = "bare"; }, (o) => { o.context.defaultIdentity.ref = "heads/main"; },
    (o) => { o.context.defaultIdentity.sha = "x"; }, (o) => { o.clonePath = "rel"; }, (o) => { o.clonePath = `${f.root}//runner/repo-a`; },
    (o) => { o.expectedHead = "g".repeat(40); }, (o) => { o.attemptId = "20260101T000000Z-g1-0123456789abcdef"; },
    (o) => { o.roots[0].contexts[0].generation = 9; }, (o) => { o.roots[0].contexts[0].branch = "other"; },
    (o) => { o.roots[0].contexts[0].kind = "mr_rework"; }, (o) => { o.roots[0].contexts = []; }, (o) => { o.roots[0].sha = "1"; },
    (o) => { o.roots = [null]; }, (o) => { o.roots = [1]; }, (o) => { o.context = null; }, (o) => { o.context = "x"; },
  ];
  for (const mutate of mutations) {
    const copy = clone(good);
    mutate(copy);
    if (!validCompletionSource(v1Of(copy))) {
      assert.throws(() => parseCompletionSourceV2(copy, expectation(f)), /malformed completion source/, JSON.stringify(copy.context));
    }
  }
});

test("codex requires the incarnations and claude must not carry them", async (t) => {
  const f = await fixture(t);
  const withExtras = { ...clone(claude(f)), executorIncarnation: "e", registryFacadePairs: [] };
  assert.throws(() => parseCompletionSourceV2(withExtras, expectation(f)), /malformed/);
  for (const key of ["executorIncarnation", "registryFacadePairs"]) {
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

test("the core digest is canonical, domain separated, excludes the certificate and covers every incarnation", async (t) => {
  const f = await fixture(t);
  const source = codex(f);
  const digest = sourceCoreDigest(source);
  assert.match(digest, /^[0-9a-f]{64}$/);
  assert.equal(sourceCoreDigest(codex(f)), digest);
  const reordered = JSON.parse(JSON.stringify(Object.fromEntries(Object.entries(source).reverse()))) as CompletionSourceV2;
  assert.equal(sourceCoreDigest(reordered), digest, "key order is irrelevant");
  assert.equal(sourceCoreDigest({ ...source, certificate: "f".repeat(64) }), digest);

  const [registry, facade] = (source.registryFacadePairs as [string, string][])[0] as [string, string];
  const withPairs = (pairs: [string, string][]): string => sourceCoreDigest({ ...source, registryFacadePairs: pairs });
  assert.notEqual(withPairs([[`${registry}x`, facade]]), digest, "changing only the registry incarnation changes the digest");
  assert.notEqual(withPairs([[registry, `${facade}x`]]), digest, "changing only the facade incarnation changes the digest");
  assert.notEqual(withPairs([[facade, registry]]), digest, "registry and facade ids are not interchangeable");
  const pairsOf = (...pairs: [string, string][]): string => withPairs(pairs);
  assert.notEqual(pairsOf(["reg-a", "fac-a"], ["reg-b", "fac-b"]), pairsOf(["reg-a", "fac-b"], ["reg-b", "fac-a"]),
    "the pairing, not just the two sets, is covered");
  assert.notEqual(sourceCoreDigest({ ...source, executorIncarnation: `${source.executorIncarnation}x` }), digest);
  assert.notEqual(sourceCoreDigest({ ...source, executorIncarnation: registry, registryFacadePairs: [["exec-x", facade]] }),
    sourceCoreDigest({ ...source, executorIncarnation: "exec-x", registryFacadePairs: [[registry, facade]] }));
  assert.notEqual(sourceCoreDigest({ ...source, expectedHead: "c".repeat(40) }), digest);
  assert.notEqual(sourceCoreDigest({ ...source, sourceUseEpoch: randomUUID() }), digest);
  assert.notEqual(sourceCoreDigest({ ...source, workerIncarnation: randomUUID() }), digest);
  assert.notEqual(sourceCoreDigest(claude(f)), digest);
});

test("the digest and the fence refuse what JSON would drop or conflate", async (t) => {
  const f = await fixture(t);
  const source = claude(f);
  for (const bad of [
    { ...source, attemptId: undefined }, { ...source, extra: 1 }, { ...source, expectedHead: undefined },
    { ...source, certificate: undefined }, { ...source, context: { ...source.context, generation: Number.NaN } },
    { ...source, context: { ...source.context, generation: Infinity } }, Object.create(source) as CompletionSourceV2,
    { ...source, roots: [{ ...source.roots[0], contexts: Object.assign([], { 0: undefined }) }] },
  ] as CompletionSourceV2[]) {
    assert.throws(() => sourceCoreDigest(bad), /malformed completion source/);
    assert.throws(() => sourceFenceOf(bad), /malformed completion source/);
  }
  const { attemptId: _attempt, ...absent } = source;
  assert.equal(sourceCoreDigest(absent as CompletionSourceV2) === sourceCoreDigest(source), false, "absent and present attemptId differ");
});

test("only the exact live lease can certify, and a certificate verifies only under the store key", async (t) => {
  const f = await fixture(t);
  const certified = await f.tx(async (t2) => f.store.certifySource(t2, f.codexLease, codex(f), codexSeal(f)));
  assert.match(certified.certificate ?? "", /^[0-9a-f]{64}$/);
  const verified = await f.tx(async (t2) => verifyCertifiedSource(t2, f.store, JSON.parse(JSON.stringify(certified))));
  assert.deepEqual(verified, certified);

  const tampers: Array<(o: Record<string, any>) => void> = [
    (o) => { o.expectedHead = "c".repeat(40); },
    (o) => { o.sourceUseEpoch = randomUUID(); },
    (o) => { o.registryFacadePairs = [["fac-z", "reg-z"]]; },
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

  // Another store key never verifies, even for the same digest.
  const other = await fixture(t, TOKEN + "-other");
  const foreign = await other.tx(async (t2) => other.store.certifySource(t2, other.codexLease, codex(other), codexSeal(other)));
  assert.equal(
    await f.tx(async (t2) => f.store.verifySourceCertificate(t2, sourceCoreDigest(certified), foreign.certificate)), false,
  );
  const transplanted = { ...foreign, storageId: f.store.storageId, relativeSource: certified.relativeSource, clonePath: certified.clonePath };
  await assert.rejects(f.tx(async (t2) => verifyCertifiedSource(t2, f.store, transplanted)), /malformed|certificate does not verify/);
});

test("certification refuses a source that is not the live lease's: slot, harness, epoch, worker, executor, state", async (t) => {
  const f = await fixture(t);
  const certify = (lease: SourceUseLease, source: unknown): Promise<CompletionSourceV2> =>
    f.tx(async (t2) => f.store.certifySource(t2, lease, source as CompletionSourceV2, lease === f.codexLease ? codexSeal(f) : undefined));
  await assert.rejects(certify(f.lease, codex(f)), /another source-use epoch/);
  await assert.rejects(certify(f.codexLease, claude(f)), /another source-use epoch/);
  // Right epoch, right slot, right worker, wrong harness: only the harness check can refuse it.
  await assert.rejects(certify(f.lease, { ...codex(f), sourceUseEpoch: leaseEpoch(f.lease), clonePath: f.clone, relativeSource: "runner/repo-a" }),
    /harness does not match/);
  await assert.rejects(certify(f.lease, { ...claude(f), clonePath: path.join(f.root, "runner", "repo-x"), relativeSource: "runner/repo-x" }),
    /another slot/);
  await assert.rejects(certify(f.lease, { ...claude(f), sourceUseEpoch: randomUUID() }), /another source-use epoch/);
  await assert.rejects(certify(f.lease, { ...claude(f), workerIncarnation: randomUUID() }), /another worker incarnation/);
  await assert.rejects(certify(f.codexLease, { ...codex(f), executorIncarnation: "exec-other" }), /executor does not match/);

  // A claude source for a slot that was ever admitted as codex is refused, even under a fresh claude lease.
  const reused = await f.tx((t2) => f.store.admitSourceUse(t2, f.codexBase.clonePath));
  const claudeOnCodexPath = buildClaudeSourceCore({ store: f.store, base: f.codexBase, lease: reused });
  await assert.rejects(certify(reused, claudeOnCodexPath), /admitted as codex/);
  // The superseded codex lease is stale.
  await assert.rejects(certify(f.codexLease, codex(f)), /stale/);

  const fresh = await certify(f.lease, claude(f));
  await f.tx((t2) => f.store.transition(t2, f.lease, "ineligible"));
  await assert.rejects(certify(f.lease, fresh), /not active/);
});

test("a codex source is certified only against the actual terminal seal, never a hand-built core", async (t) => {
  const f = await fixture(t);
  const certify = (source: unknown, seal?: { seal: unknown }, lease = f.codexLease): Promise<CompletionSourceV2> =>
    f.tx((t2) => f.store.certifySource(t2, lease, source as CompletionSourceV2, seal));
  const genuine = codex(f);
  // The slot owner's executor id with invented registry/facade incarnations: parses, binds, and used to certify.
  const handBuilt = { ...genuine, registryFacadePairs: [["reg-invented", "fac-invented"]] };
  await assert.rejects(certify(genuine), /needs its terminal seal/, "no seal at all");
  await assert.rejects(certify(handBuilt), /needs its terminal seal/);
  await assert.rejects(certify(handBuilt, codexSeal(f)), /does not match the terminal seal/, "invented pairs");
  await assert.rejects(certify({ ...genuine, registryFacadePairs: [] }, codexSeal(f)), /does not match the terminal seal/, "dropped pairs");

  const other = await fixture(t);
  await assert.rejects(certify(genuine, codexSeal(other)), /another source use/, "a seal of another lease");
  await assert.rejects(certify(genuine, { seal: {} }), /not a terminal seal/, "a look-alike seal");
  await assert.rejects(certify(claude(f), codexSeal(f), f.lease), /takes no terminal seal/, "a claude lease takes no seal");

  const certified = await certify(genuine, codexSeal(f));
  assert.match(certified.certificate ?? "", /^[0-9a-f]{64}$/);
});

test("a seal revoked after it was issued is never accepted, whatever acceptance callback the caller supplies", async (t) => {
  const f = await fixture(t);
  const genuine = codex(f);
  f.revoke();
  assert.equal(f.lifetime.accepts(f.seal, f.codexLease), false, "precondition: the lifetime itself no longer accepts the seal");
  assert.throws(() => codex(f), /not accepted/, "the builder");
  // The old API let a caller pass accepts: () => true; the extra field is now simply ignored.
  const callerOverride = { seal: f.seal, accepts: () => true } as { seal: unknown };
  assert.throws(() => buildCodexSourceCore({ store: f.store, base: f.codexBase, lease: f.codexLease, ...callerOverride }), /not accepted/);
  await assert.rejects(f.tx((t2) => f.store.certifySource(t2, f.codexLease, genuine, callerOverride)), /not accepted/, "certification");
});

test("verifyPublishedSource needs the certificate AND the slot's current publication for the same epoch and digest", async (t) => {
  const f = await fixture(t);
  const certified = await f.tx((t2) => f.store.certifySource(t2, f.lease, claude(f)));
  await assert.rejects(f.tx((t2) => verifyPublishedSource(t2, f.store, certified)), /not the published source/, "active slot publishes nothing");
  const rival = await f.tx((t2) => f.store.certifySource(t2, f.lease, { ...claude(f), expectedHead: "c".repeat(40) }));
  await f.tx((t2) => f.store.transition(t2, f.lease, "claude_ready", { source: certified, captureIds: CAP }));
  const published = await f.tx((t2) => verifyPublishedSource(t2, f.store, JSON.parse(JSON.stringify(certified))));
  assert.deepEqual(published.source, certified);
  assert.equal(published.publication.state, "claude_ready");
  assert.deepEqual(published.publication.cohort, { runId: "run-1", generation: 2, captureIds: CAP });
  await assert.rejects(f.tx((t2) => verifyPublishedSource(t2, f.store, rival)), /not the published source/, "valid certificate, other digest");
  // issue #3645: the refusal is typed so a handoff classifies it invalid, never uncertain.
  await assert.rejects(f.tx((t2) => verifyPublishedSource(t2, f.store, rival)),
    (err: unknown) => err instanceof AuthorityRefusal && err.kind === "source_mismatch");
  await assert.rejects(f.tx((t2) => verifyPublishedSource(t2, f.store, { ...certified, certificate: "0".repeat(64) })), /certificate does not verify/);
  await f.tx((t2) => f.store.transition(t2, f.lease, "ineligible"));
  assert.equal((await f.tx((t2) => f.store.readPublication(t2, f.clone)))?.state, "ineligible", "the publication outlives the state");
  await assert.rejects(f.tx((t2) => verifyPublishedSource(t2, f.store, certified)), /not the published source/, "an ineligible slot no longer publishes it");
  await f.tx((t2) => f.store.admitSourceUse(t2, f.clone));
  await assert.rejects(f.tx((t2) => verifyPublishedSource(t2, f.store, certified)), /not the published source/, "a new epoch supersedes it");

  const sealedCodex = await f.tx((t2) => f.store.certifySource(t2, f.codexLease, codex(f), codexSeal(f)));
  await f.tx((t2) => f.store.transition(t2, f.codexLease, "sealed", { source: sealedCodex, captureIds: CAP }));
  assert.equal((await f.tx((t2) => verifyPublishedSource(t2, f.store, sealedCodex))).publication.state, "sealed");
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
