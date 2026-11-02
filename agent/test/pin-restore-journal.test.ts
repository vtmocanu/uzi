import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { test } from "node:test";
import {
  parsePinRestoreJson, parsePinRestoreLink, parseRetainedPinEnvelope,
  serializeRetainedPinEnvelope, validateRetainedPinActive, PinRestoreDecodeBudget,
  type PinJournalIdentity, type PinRestoreLink, type RetainedPinEnvelope,
} from "../src/pin-restore-journal.js";

const epoch = (g: number) => `00000000-0000-4000-8000-${String(g).padStart(12, "0")}`;
const attempt = (g: number) => `20261101T120000Z-g${g}-0123456789abcdef`;
const digest = (raw: string) => createHash("sha256").update(raw).digest("hex");
const identity = (g: number): PinJournalIdentity => ({
  runId: "run-1", clonePath: `/runner/repo/key.attempt-${attempt(g)}`, attemptId: attempt(g), sourceGeneration: g,
});
// This seam deliberately retains unrelated memoryPark/fence fields in the raw source.
// Their semantic validation belongs to git.ts's callback, not this pure utility.
const parseJournal = (raw: string) => parsePinRestoreJson(raw, 64 * 1024 * 1024) as PinJournalIdentity;
const entry = (rawJournal: string, generation: number) => ({
  journalDigest: digest(rawJournal), rawJournal, generation, sourceEpoch: epoch(generation),
});
function link(raw: string, generation: number): PinRestoreLink {
  return {
    version: 1, intentId: `intent-${generation}`, predecessorJournalDigest: digest(raw),
    predecessorGeneration: generation, predecessorEpoch: epoch(generation),
    successor: {
      runId: "run-1", claimGeneration: generation + 1, workerId: "worker.example",
      registerNonce: "register:nonce", authorityIncarnation: epoch(10), executorIncarnation: "exec-123",
      sourceEpoch: epoch(generation + 1), attemptId: attempt(generation + 1),
      clonePath: identity(generation + 1).clonePath,
    },
    restore: {
      captureId: "capture-1", authenticatedRecordDigest: "a".repeat(64),
      predecessorGeneration: generation, bareDir: "host+repo.git",
      pinRef: `refs/uzi-recovery-pin/run-1/${generation}`, tip: "b".repeat(40),
      contentSource: "e".repeat(40), expectedHead: "c".repeat(40), expectedTree: "d".repeat(40),
    },
  };
}
function envelope(): RetainedPinEnvelope {
  const raw = JSON.stringify(identity(1));
  return { version: 2, runId: "run-1", branch: "uzi/issue-1", key: "issue-1",
    predecessors: [entry(raw, 1)], intent: { version: 1, phase: "prepared", link: link(raw, 1) } };
}
const parse = (e: RetainedPinEnvelope, budget?: PinRestoreDecodeBudget) =>
  parseRetainedPinEnvelope(JSON.stringify(e), parseJournal, budget);
function next(e: RetainedPinEnvelope): string {
  return JSON.stringify({ ...identity(e.intent.link.successor.claimGeneration), pinRestore: e.intent.link });
}
function replaceRaw(e: RetainedPinEnvelope, raw: string): void {
  const old = e.predecessors[0]!;
  e.predecessors[0] = entry(raw, old.generation);
  e.intent.link = link(raw, old.generation);
}
function sequential(): RetainedPinEnvelope {
  const e = envelope();
  const raw = next(e);
  e.predecessors.push(entry(raw, 2));
  e.intent.link = link(raw, 2);
  return e;
}

test("canonical fresh link and envelope preserve exact journal whitespace and opaque fence bytes", () => {
  const e = envelope();
  const raw = '  { "runId":"run-1", "clonePath":"' + identity(1).clonePath + '", "attemptId":"' +
    attempt(1) + '", "sourceGeneration":1, "memoryPark":{"opaque":"\\u001b"}, ' +
    '"completionSourceFence":{"seq":7,"opaque":"\\n"}} \n';
  replaceRaw(e, raw);
  const parsed = parse(e);
  assert.deepEqual(parsed, e);
  assert.notEqual(parsed, e);
  assert.notEqual(parsed.intent.link, e.intent.link);
  assert.equal(parsed.predecessors[0]!.rawJournal, raw);
  assert.equal(parsed.predecessors[0]!.journalDigest, digest(raw));
  const reordered = Object.fromEntries(Object.entries(e.intent.link).reverse());
  assert.equal(JSON.stringify(parsePinRestoreLink(reordered)), JSON.stringify(e.intent.link));
  assert.equal(serializeRetainedPinEnvelope(parsed, parseJournal), JSON.stringify(e));
  const bounded = structuredClone(e.intent.link);
  bounded.successor.workerId = "w".repeat(128);
  bounded.successor.registerNonce = "n".repeat(256);
  assert.doesNotThrow(() => parsePinRestoreLink(bounded));
  const nullable = structuredClone(e.intent.link);
  nullable.successor.executorIncarnation = null;
  assert.equal(parsePinRestoreLink(nullable).successor.executorIncarnation, null);
});

test("strict JSON refuses decoded duplicate aliases at every nesting level and bounds depth", () => {
  for (const raw of [
    '{"a":1,"a":2}', '{"a":1,"\\u0061":2}',
    '{"x":[{"a":1,"\\u0061":2}]}', '{"x":{"y":{"q":1,"q":2}}}',
    '{"x":[{"a":1,"a":2}],"a":3}', '{"a":1,}', '[1,]', '"unterminated',
  ]) assert.throws(() => parsePinRestoreJson(raw, 65536), raw);
  assert.deepEqual(parsePinRestoreJson('{"x":[{"a":1},{"a":2}],"a":3}', 65536),
    { x: [{ a: 1 }, { a: 2 }], a: 3 });
  assert.doesNotThrow(() => parsePinRestoreJson("[".repeat(32) + "0" + "]".repeat(32), 100));
  assert.throws(() => parsePinRestoreJson("[".repeat(33) + "0" + "]".repeat(33), 100));
  assert.throws(() => parsePinRestoreJson('"' + String.fromCharCode(0xd800) + '"', 100));
  assert.throws(() => parsePinRestoreJson('"é"', 3));
  assert.equal(parsePinRestoreJson('"é"', 4), "é");
  const legacy = JSON.stringify({ data: "x".repeat(100000) });
  assert.doesNotThrow(() => parsePinRestoreJson(legacy, 64 * 1024 * 1024));
  assert.throws(() => parsePinRestoreJson(legacy, 16384));
});

test("root key observer exposes value positions before duplicate failure without decoding the value", () => {
  const raw = '{"\\u0076ersion" \t:\r\n2e0,"version":2,"intent":';
  const seen: Array<[string, number]> = [];
  assert.throws(() => parsePinRestoreJson(raw, 65536, (key, keyEnd) => {
    seen.push([key, keyEnd]);
  }));
  assert.deepEqual(seen, [["version", raw.indexOf(" \t:")], ["version", raw.indexOf('":2') + 1]]);
  assert.throws(() => parsePinRestoreJson(raw, 1, () => assert.fail("size check must precede observation")));
  const sentinel = new Error("observer");
  assert.throws(() => parsePinRestoreJson('{"version":2}', 65536, () => { throw sentinel; }),
    error => error === sentinel);
});

test("unknown fields, omitted fields and unsupported versions fail throughout the persisted shape", () => {
  const locations = [
    (e: RetainedPinEnvelope) => e,
    (e: RetainedPinEnvelope) => e.intent,
    (e: RetainedPinEnvelope) => e.intent.link,
    (e: RetainedPinEnvelope) => e.intent.link.successor,
    (e: RetainedPinEnvelope) => e.intent.link.restore,
    (e: RetainedPinEnvelope) => e.predecessors[0]!,
  ];
  for (const location of locations) {
    const e = envelope();
    Object.assign(location(e), { unknown: true });
    assert.throws(() => parse(e));
    const missing = envelope();
    const object = location(missing) as unknown as Record<string, unknown>;
    delete object[Object.keys(object)[0]!];
    assert.throws(() => parse(missing));
  }
  for (const location of locations.slice(0, 3)) {
    const e = envelope();
    Object.assign(location(e), { version: 99 });
    assert.throws(() => parse(e));
  }
  const e = envelope();
  Object.assign(e.intent, { phase: "complete" });
  assert.throws(() => parse(e));
});

test("identities, hashes, paths, generation and pin ref are constrained", () => {
  const changes: ((l: PinRestoreLink) => void)[] = [
    l => { l.successor.runId = "wrong/run"; },
    l => { l.intentId = "a".repeat(161); },
    l => { l.restore.captureId = ""; },
    l => { l.successor.workerId = "ansi" + String.fromCharCode(27); },
    l => { l.successor.registerNonce = "bidi\u202e"; },
    l => { l.successor.workerId = "line\u2028"; },
    l => { l.successor.registerNonce = String.fromCharCode(0xdfff); },
    l => { l.successor.workerId = "x".repeat(129); },
    l => { l.successor.registerNonce = "x".repeat(257); },
    l => { l.successor.clonePath = "/runner/repo/key"; },
    l => { l.successor.runId += "\n"; },
    l => { l.intentId += "\n"; },
    l => { l.predecessorJournalDigest += "\n"; },
    l => { l.predecessorEpoch += "\n"; },
    l => { l.successor.sourceEpoch += "\n"; },
    l => { l.successor.attemptId += "\n"; },
    l => { l.restore.tip += "\n"; },
    l => { l.restore.contentSource += "\n"; },
    l => { l.restore.contentSource = "pin"; },
    l => { l.successor.executorIncarnation = "exec/" ; },
    l => { l.successor.authorityIncarnation = "not-a-uuid"; },
    l => { l.predecessorEpoch = "not-a-uuid"; },
    l => { l.successor.clonePath = "/runner/../clone"; },
    l => { l.successor.clonePath = "relative"; },
    l => { l.successor.clonePath = "/é" + "é".repeat(2048); },
    l => { l.successor.attemptId = attempt(9); },
    l => { l.successor.attemptId = "20261101T120000Z-gx-0123456789abcdef"; },
    l => { l.successor.claimGeneration = 1; },
    l => { l.predecessorGeneration = 1.5; },
    l => { l.restore.predecessorGeneration = 2; },
    l => { l.restore.bareDir = ".."; },
    l => { l.restore.bareDir = "../repo.git"; },
    l => { l.restore.pinRef += "/extra"; },
    l => { l.restore.tip = "A".repeat(40); },
    l => { l.restore.authenticatedRecordDigest = "A".repeat(64); },
    l => { l.restore.expectedTree = "a".repeat(39); },
    l => { l.restore.contentSource = "bad\nsource"; },
  ];
  for (const change of changes) {
    const e = envelope();
    change(e.intent.link);
    assert.throws(() => parse(e));
  }
  for (const change of [
    (e: RetainedPinEnvelope) => { e.branch = "bad\nbranch"; },
    (e: RetainedPinEnvelope) => { e.branch = "b".repeat(257); },
    (e: RetainedPinEnvelope) => { e.key = "../issue"; },
    (e: RetainedPinEnvelope) => { e.key = "k".repeat(256); },
    (e: RetainedPinEnvelope) => { e.intent.link.successor.runId = "other"; },
    (e: RetainedPinEnvelope) => { e.predecessors[0]!.sourceEpoch = epoch(9); },
    (e: RetainedPinEnvelope) => { e.predecessors[0]!.journalDigest = "f".repeat(64); },
    (e: RetainedPinEnvelope) => { e.predecessors[0]!.generation = 2; },
  ]) {
    const e = envelope(); change(e); assert.throws(() => parse(e));
  }
});

test("raw journal identities require explicit positive generation and matching run and attempt", () => {
  for (const patch of [
    { runId: "other" }, { sourceGeneration: undefined }, { sourceGeneration: 0 },
    { sourceGeneration: 2 }, { sourceGeneration: Number.MAX_SAFE_INTEGER + 1 },
    { attemptId: attempt(2) }, { clonePath: "/runner/repo/key" },
    { clonePath: identity(2).clonePath }, { sourceGeneration: -1 }, { sourceGeneration: 1.5 },
    { runId: "run-1\n" }, { attemptId: attempt(1) + "\n" },
  ]) {
    const e = envelope();
    replaceRaw(e, JSON.stringify({ ...identity(1), ...patch }));
    assert.throws(() => parse(e));
  }
  const e = envelope();
  replaceRaw(e, JSON.stringify({ ...identity(1), clonePath: "/runner/repo/key", attemptId: undefined }));
  assert.doesNotThrow(() => parse(e)); // legacy canonical predecessor
});

test("two sequential handoffs retain flat exact raw history with earlier link references", () => {
  const e = sequential();
  const first = e.predecessors[0]!.rawJournal;
  const second = e.predecessors[1]!.rawJournal;
  assert.deepEqual(parse(e), e);
  assert.equal(validateRetainedPinActive(e, second, parseJournal), "prepared-old");
  assert.equal(validateRetainedPinActive(e, next(e), parseJournal), "prepared-new");
  assert.equal(parseJournal(second).pinRestore!.predecessorJournalDigest, digest(first));
  assert.equal(Object.hasOwn(JSON.parse(second), "predecessors"), false);
  assert.equal(serializeRetainedPinEnvelope(e, parseJournal).includes('"predecessors":[{'), true);
  const nested = envelope();
  replaceRaw(nested, JSON.stringify(envelope()));
  assert.throws(() => parse(nested));
});

test("cycles, forward references, wrong epochs and reused paths or attempts fail closed", () => {
  const changes: ((e: RetainedPinEnvelope) => void)[] = [
    e => { e.predecessors.reverse(); },
    e => {
      const raw = JSON.stringify(identity(2));
      e.predecessors[1] = entry(raw, 2); e.intent.link = link(raw, 2);
    },
    e => { e.predecessors[1]!.sourceEpoch = epoch(99); },
    e => { e.intent.link.predecessorJournalDigest = e.predecessors[0]!.journalDigest; },
    e => { e.intent.link.successor.clonePath = identity(1).clonePath; },
    e => {
      const old = e.predecessors[1]!;
      const j = parseJournal(old.rawJournal);
      j.clonePath = identity(1).clonePath;
      const raw = JSON.stringify(j);
      e.predecessors[1] = entry(raw, 2); e.intent.link = link(raw, 2);
    },
    e => {
      const j = parseJournal(e.predecessors[1]!.rawJournal);
      j.pinRestore!.predecessorJournalDigest = "f".repeat(64);
      const raw = JSON.stringify(j);
      e.predecessors[1] = entry(raw, 2); e.intent.link = link(raw, 2);
    },
    e => {
      const j = parseJournal(e.predecessors[1]!.rawJournal);
      j.pinRestore!.predecessorEpoch = epoch(99);
      const raw = JSON.stringify(j);
      e.predecessors[1] = entry(raw, 2); e.intent.link = link(raw, 2);
    },
    e => { e.predecessors.push(e.predecessors[0]!); },
  ];
  for (const change of changes) {
    const e = sequential(); change(e); assert.throws(() => parse(e));
  }
  const e = envelope();
  const duplicate = { ...identity(1), clonePath: "/different", arbitrary: true };
  e.predecessors.push(entry(JSON.stringify(duplicate), 1));
  e.intent.link = link(e.predecessors[1]!.rawJournal, 1);
  assert.throws(() => parse(e)); // repeated attempt with distinct path and digest
});

test("former links may reference any earlier entry but history generations strictly increase", () => {
  const e = sequential();
  const earlier = link(e.predecessors[0]!.rawJournal, 1);
  earlier.successor = { ...e.intent.link.successor };
  const raw = JSON.stringify({ ...identity(3), pinRestore: earlier });
  e.predecessors.push(entry(raw, 3));
  e.intent.link = link(raw, 3);
  assert.deepEqual(parse(e), e);
  // Each former link remains internally valid; only the append order is invalid.
  [e.predecessors[1], e.predecessors[2]] = [e.predecessors[2]!, e.predecessors[1]!];
  e.intent.link = link(e.predecessors.at(-1)!.rawJournal, 2);
  assert.throws(() => parse(e));
});

test("prepared, ready and absent active relations require exact old bytes or exact new link", () => {
  const e = envelope();
  const old = e.predecessors[0]!.rawJournal;
  assert.equal(validateRetainedPinActive(e, old, parseJournal), "prepared-old");
  assert.equal(validateRetainedPinActive(e, next(e), parseJournal), "prepared-new");
  assert.equal(validateRetainedPinActive(e, undefined, parseJournal), "absent");
  assert.throws(() => validateRetainedPinActive(e, " " + old, parseJournal));
  for (const patch of [{ runId: "other" }, { clonePath: "/elsewhere" },
    { attemptId: attempt(3) }, { sourceGeneration: 3 }, { pinRestore: undefined }]) {
    assert.throws(() => validateRetainedPinActive(e,
      JSON.stringify({ ...identity(2), pinRestore: e.intent.link, ...patch }), parseJournal));
  }
  const altered = structuredClone(e.intent.link);
  altered.restore.expectedHead = "e".repeat(40);
  assert.throws(() => validateRetainedPinActive(e,
    JSON.stringify({ ...identity(2), pinRestore: altered }), parseJournal));
  const reordered = Object.fromEntries(Object.entries(e.intent.link).reverse());
  assert.equal(validateRetainedPinActive(e,
    JSON.stringify({ ...identity(2), pinRestore: reordered }), parseJournal), "prepared-new");
  e.intent.phase = "ready";
  assert.equal(validateRetainedPinActive(e, next(e), parseJournal), "ready-new");
  assert.equal(validateRetainedPinActive(e, undefined, parseJournal), "absent");
  assert.throws(() => validateRetainedPinActive(e, old, parseJournal));
  assert.throws(() => validateRetainedPinActive(e, JSON.stringify(identity(5)), parseJournal));
});

test("UTF8 per-entry, sum, entry count and escaped whole-envelope bounds are conjunctive", () => {
  const e = envelope();
  const base = JSON.stringify(identity(1));
  replaceRaw(e, base + " ".repeat(16384 - Buffer.byteLength(base)));
  assert.doesNotThrow(() => parse(e));
  replaceRaw(e, e.predecessors[0]!.rawJournal + " ");
  assert.throws(() => parse(e));
  replaceRaw(e, JSON.stringify({ ...identity(1), data: "é".repeat(8200) }));
  assert.throws(() => parse(e));
  const count = envelope();
  for (let g = 2; g <= 9; g++) {
    const raw = next(count);
    count.predecessors.push(entry(raw, g));
    count.intent.link = link(raw, g);
  }
  assert.throws(() => parse(count));
  count.predecessors.pop();
  count.intent.link = link(count.predecessors.at(-1)!.rawJournal, 8);
  assert.doesNotThrow(() => parse(count));
  count.predecessors = [];
  assert.throws(() => parse(count));
  for (const padding of [" ", "\t"]) {
    const size = envelope();
    const first = JSON.stringify(identity(1));
    replaceRaw(size, first + padding.repeat(16384 - Buffer.byteLength(first)));
    const second = next(size);
    const raw = second + padding.repeat(16384 - Buffer.byteLength(second));
    size.predecessors.push(entry(raw, 2));
    size.intent.link = link(raw, 2);
    if (padding === " ") {
      assert.doesNotThrow(() => parse(size)); // exactly 32768 nested raw bytes
      assert.equal(validateRetainedPinActive(size, raw, parseJournal), "prepared-old");
      size.predecessors.push(entry(next(size), 3));
      size.intent.link = link(size.predecessors[2]!.rawJournal, 3);
      assert.throws(() => parse(size));
    } else {
      assert.ok(Buffer.byteLength(JSON.stringify(size)) > 65536);
      assert.throws(() => parse(size)); // escaping counts, even though raw sum fits
    }
  }
});

test("active validation bounds combined decoded history and new active before parsing active", () => {
  const e = envelope();
  const first = e.predecessors[0]!.rawJournal;
  replaceRaw(e, first + " ".repeat(10000 - Buffer.byteLength(first)));
  const second = next(e);
  const old = second + " ".repeat(10000 - Buffer.byteLength(second));
  e.predecessors.push(entry(old, 2));
  e.intent.link = link(old, 2);
  const active = next(e);
  const atLimit = active + " ".repeat(12768 - Buffer.byteLength(active));
  const budget = new PinRestoreDecodeBudget();
  assert.equal(validateRetainedPinActive(e, atLimit, parseJournal, budget), "prepared-new");
  assert.equal(budget.decodedBytes, 32768);
  let calls = 0;
  const before = structuredClone(e);
  const rejectedBudget = new PinRestoreDecodeBudget();
  assert.throws(() => validateRetainedPinActive(e, atLimit + " ", raw => {
    calls++; return parseJournal(raw);
  }, rejectedBudget));
  assert.equal(calls, 2); // predecessors decoded; active not parsed or charged
  assert.equal(rejectedBudget.decodedBytes, 20000);
  assert.deepEqual(e, before); // history retained, never truncated to fit
  const oldBudget = new PinRestoreDecodeBudget();
  assert.equal(validateRetainedPinActive(e, old, parseJournal, oldBudget), "prepared-old");
  assert.equal(oldBudget.decodedBytes, 20000); // byte-equal old is not reparsed
});

test("shared operation budget charges repeated actual decodes before identity deduplication", () => {
  const e = envelope();
  const budget = new PinRestoreDecodeBudget();
  const bytes = Buffer.byteLength(e.predecessors[0]!.rawJournal);
  parse(e, budget);
  parse(e, budget);
  assert.equal(budget.decodedBytes, bytes * 2);
  const duplicate = envelope();
  duplicate.predecessors.push(duplicate.predecessors[0]!);
  assert.throws(() => parse(duplicate, budget));
  assert.equal(budget.decodedBytes, bytes * 4);
  budget.decodedBytes = 1048576 - bytes;
  assert.doesNotThrow(() => parse(e, budget));
  assert.throws(() => parse(e, budget));
  assert.doesNotThrow(() => parse(e)); // fresh default operation
  const activeBudget = new PinRestoreDecodeBudget();
  validateRetainedPinActive(e, next(e), parseJournal, activeBudget);
  assert.equal(activeBudget.decodedBytes, bytes + Buffer.byteLength(next(e)));
});

test("nested journal duplicate aliases fail before callback and callback errors propagate", () => {
  const e = envelope();
  replaceRaw(e, e.predecessors[0]!.rawJournal.replace('"runId":', '"\\u0072unId":"other","runId":'));
  let calls = 0;
  assert.throws(() => parseRetainedPinEnvelope(JSON.stringify(e), raw => { calls++; return parseJournal(raw); }));
  assert.equal(calls, 0);
  const sentinel = new Error("callback rejected fence");
  assert.throws(() => parseRetainedPinEnvelope(JSON.stringify(envelope()), () => { throw sentinel; }),
    error => error === sentinel);
});
