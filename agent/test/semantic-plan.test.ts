import assert from "node:assert/strict";
import { test } from "node:test";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import {
  captureSemanticSources, parseSemanticCandidate, parseSemanticEnrollment,
  parseSemanticProposal, semanticSourceCaptureSchema,
} from "../src/semantic-plan.js";
import { findSemanticPrdPaths, findValidPrdCore } from "../src/prd-link.js";
import type { BarePrdReader } from "../src/prd-link.js";

// Supplementary cases use the same DTO shape; the shared fixture below is
// recorded from the standalone Go API DTO marshal test.
const sha = "a".repeat(40);
const digest = "b".repeat(64);
const sourceBytes = Buffer.from("require é\u0000\r\n");
const sourceHash = createHash("sha256").update(sourceBytes).digest("hex");
const criterion = () => ({
  milestone_id: "M1", kind: "code", text: "Preserve source bytes",
  done_when: "The captured bytes are identical",
  citations: [{ source_id: "issue:1230", hash: sourceHash, start: 0, end: sourceBytes.length }],
  evidence_requirements: [{ description: "Focused test", path: "agent/test/semantic-plan.test.ts" }],
});
const proposal = () => ({ criteria: [criterion()] });
const candidate = () => ({
  candidate_identity_version: "semantic_v1", revision: 1, digest,
  sources: [{
    id: "issue:1230", kind: "issue", hash: sourceHash,
    provenance: "example/repo:issue:1230", completeness: "complete",
  }],
  plan_hash: digest, source_outcome: "complete",
  criteria: [{ id: "12345678-1234-4234-8234-123456789abc", ...criterion() }],
});
function input(issue = "prds/1230-a.md") {
  return {
    barePath: "/worker/cache/repo.git", repo: "example/repo", headSha: sha,
    issue: { id: "issue:1230", bytes: Buffer.from(issue) },
    planBytes: Buffer.from("Plan"), timeoutMs: 1000,
  };
}
function reader(bytes: Uint8Array = sourceBytes) {
  const calls: { path: string; args: readonly string[]; opts: { maxBytes: number; timeoutMs?: number } }[] = [];
  const git: BarePrdReader = {
    async readBare(path, args, opts) {
      calls.push({ path, args, opts });
      return { text: Buffer.from(bytes).toString("utf8"), bytes, truncated: false };
    },
  };
  return { git, calls };
}

test("server-recorded semantic DTO fixture round trips through the agent schema", () => {
  const raw = readFileSync(new URL("../../fixtures/semantic-plan/candidate.json", import.meta.url), "utf8");
  const decoded = parseSemanticCandidate(raw);
  assert.deepEqual(decoded, JSON.parse(raw));
  assert.deepEqual(parseSemanticCandidate(JSON.stringify(decoded)), JSON.parse(raw));
});

test("API-shaped candidate and proposal round trip without renaming or losing fields", () => {
  const wire = candidate();
  assert.deepEqual(parseSemanticCandidate(JSON.stringify(wire)), wire);
  assert.deepEqual(JSON.parse(JSON.stringify(parseSemanticCandidate(wire))), wire);
  assert.deepEqual(parseSemanticProposal(JSON.stringify(proposal())), proposal());
  const withCoverage = {
    ...wire, coverage: { outcome: "uncertain", candidate_digest: digest, reviews: 1, invocations: 2, tokens: 3, seconds: 4 },
  };
  assert.deepEqual(parseSemanticCandidate(JSON.stringify(withCoverage)), withCoverage);
  assert.equal(parseSemanticCandidate({ ...wire, coverage: null }).coverage, null);
});

test("semantic candidate preserves optional plan fields digest and historical absence", () => {
  const historical = parseSemanticCandidate(candidate());
  assert.equal(Object.hasOwn(historical, "plan_fields_digest"), false);
  const current = { ...candidate(), plan_fields_digest: digest };
  assert.deepEqual(parseSemanticCandidate(current), current);
  for (const invalid of [null, "", "f".repeat(63), 42])
    assert.throws(() => parseSemanticCandidate({ ...candidate(), plan_fields_digest: invalid }));
});

test("mandatory escaped proposals retain all fields through string and Uint8Array input", () => {
  const fill = (prefix: string, length: number) => prefix + "\u0001".repeat(length - prefix.length);
  const bytes = Buffer.from("requirement");
  const id = '"'.repeat(512);
  const hash = createHash("sha256").update(bytes).digest("hex");
  const source = { id, kind: "issue", provenance: "server:run-issue", completeness: "complete", bytes: bytes.toString("base64") };
  const sources = [source];
  const milestoneIds = Array.from({ length: 8 }, (_, i) => "m" + i);
  const expected = { criteria: Array.from({ length: 256 }, (_, i) => ({
    milestone_id: milestoneIds[Math.floor(i / 32)], kind: "maintainer",
    text: fill(String(i).padStart(3, "0"), 2048), done_when: fill("", 2048),
    citations: Array.from({ length: 8 }, (_, j) => ({ source_id: id, hash, start: 0, end: j + 1 })),
    evidence_requirements: Array.from({ length: 8 }, (_, j) => ({ description: fill(String(j), 512), path: id })),
    maintainer: { reason: fill("", 2048),
      conditions: Array.from({ length: 8 }, (_, j) => fill(String(j), 512)),
      verification_steps: Array.from({ length: 8 }, (_, j) => fill(String(j), 512)) },
  })) };
  const raw = JSON.stringify(expected);
  assert.ok(Buffer.byteLength(raw) > 16 * 1024 * 1024);
  for (const asBytes of [false, true]) {
    const parsed = parseSemanticProposal(asBytes ? Buffer.from(raw) : raw, { sources, milestoneIds });
    assert.deepEqual(parsed, expected);
    assert.equal(new Set(parsed.criteria.map(c => c.text)).size, 256);
    assert.equal(parsed.criteria.filter(c => c.milestone_id === "m7").length, 32);
  }
  assert.throws(() => parseSemanticProposal({ criteria: [...expected.criteria, expected.criteria[0]] }));
  assert.throws(() => parseSemanticProposal({ criteria: expected.criteria.map(c => ({ ...c, milestone_id: "m0" })) }));
  assert.deepEqual(Buffer.from(source.bytes, "base64"), bytes);
});

test("proposal wire ceiling is exactly 48 MiB and metadata keeps its smaller ceiling", () => {
  const raw = JSON.stringify(proposal());
  const atLimit = raw + " ".repeat(48 * 1024 * 1024 - Buffer.byteLength(raw));
  for (const asBytes of [false, true]) {
    assert.deepEqual(parseSemanticProposal(asBytes ? Buffer.from(atLimit) : atLimit), proposal());
    assert.throws(() => parseSemanticProposal(asBytes ? Buffer.from(atLimit + " ") : atLimit + " "));
  }
  const enrollment = JSON.stringify({ profile: "semantic", policy: {
    version: 1, model: "model", max_reviews: 3, max_invocations: 2, max_tokens: 120000, max_seconds: 900,
  } });
  assert.throws(() => parseSemanticEnrollment(enrollment + " ".repeat(16 * 1024 * 1024)));
});

test("enrollment policy placeholder mirrors all API field names", () => {
  const enrollment = {
    profile: "semantic", policy: { version: 1, model: "model", max_reviews: 3, max_invocations: 6, max_tokens: 120000, max_seconds: 900 },
  };
  assert.deepEqual(parseSemanticEnrollment(JSON.stringify(enrollment)), enrollment);
  assert.throws(() => parseSemanticEnrollment({ ...enrollment, enabled: true }));
  assert.throws(() => parseSemanticEnrollment({ ...enrollment, policy: { ...enrollment.policy, max_calls: 1 } }));
});

test("proposals reject chosen IDs, unknown fields, unknown kinds and maintainer misuse", () => {
  for (const patch of [
    { id: "chosen" }, { kind: "security" }, { doneWhen: "alias" },
    { maintainer: { reason: "Owner", conditions: ["Condition"], verification_steps: ["Step"] } },
  ]) {
    assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), ...patch }] }));
  }
  assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), evidence_requirements: [{ description: "Proof", extra: true }] }] }));
  const maintainer = { ...criterion(), kind: "maintainer", maintainer: { reason: "Needs owner", conditions: ["Cluster access"], verification_steps: ["Observe deployment"] } };
  assert.deepEqual(parseSemanticProposal({ criteria: [maintainer] }).criteria[0], maintainer);
  assert.throws(() => parseSemanticProposal({ criteria: [{ ...maintainer, maintainer: null }] }));
  for (const key of ["conditions", "verification_steps"] as const) {
    for (const list of [[], Array(9).fill("x"), ["é".repeat(257)]]) {
      assert.throws(() => parseSemanticProposal({ criteria: [{ ...maintainer, maintainer: { ...maintainer.maintainer, [key]: list } }] }));
    }
  }
});

test("strict wire rejects duplicates, escaped duplicate keys, aliases, trailing JSON and invalid UTF-8", () => {
  const wire = JSON.stringify(proposal());
  for (const bad of [
    wire.replace('"text":', '"text":"first","text":'),
    wire.replace('"text":', '"te\\u0078t":"first","text":'),
    wire.replace('"text":', '"Text":'),
    wire + "{}",
    Buffer.from([0xff]),
    '{"criteria":[], "criteria":[]}',
  ]) assert.throws(() => parseSemanticProposal(bad));
  assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), text: "\ud800" }] }));
});

test("text/done_when limits are UTF-8 bytes, not JS character counts", () => {
  for (const key of ["text", "done_when"]) {
    assert.doesNotThrow(() => parseSemanticProposal({ criteria: [{ ...criterion(), [key]: "é".repeat(1024) }] }));
    assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), [key]: "é".repeat(1025) }] }));
    assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), [key]: " \n " }] }));
  }
  assert.doesNotThrow(() => parseSemanticProposal({ criteria: [{ ...criterion(), text: "exact\u0000source" }] }));
  assert.doesNotThrow(() => parseSemanticProposal({ criteria: [{ ...criterion(), text: "\ufeff" }] }));
  assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), text: "\u0085" }] }));
});

test("criterion, evidence and citation cardinality bounds", () => {
  assert.throws(() => parseSemanticProposal({ criteria: [] }));
  assert.doesNotThrow(() => parseSemanticProposal({ criteria: Array.from({ length: 256 }, (_, i) => ({ ...criterion(), milestone_id: "M" + Math.floor(i / 32) })) }));
  assert.throws(() => parseSemanticProposal({ criteria: Array.from({ length: 257 }, (_, i) => ({ ...criterion(), milestone_id: "M" + i })) }));
  assert.throws(() => parseSemanticProposal({ criteria: Array(33).fill(criterion()) }));
  for (const key of ["citations", "evidence_requirements"] as const) {
    for (const count of [0, 9]) assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), [key]: Array(count).fill(criterion()[key][0]) }] }));
  }
  for (const patch of [{ description: "é".repeat(257) }, { description: "x", path: "é".repeat(257) }, { description: "x", path: "x\u202ey" }]) {
    assert.throws(() => parseSemanticProposal({ criteria: [{ ...criterion(), evidence_requirements: [patch] }] }));
  }
});

test("canonical IDs, source identities, and available citation context are validated", () => {
  const wire = candidate();
  for (const id of ["chosen", "12345678-1234-1234-8234-123456789abc"]) {
    assert.throws(() => parseSemanticCandidate({ ...wire, criteria: [{ ...wire.criteria[0], id }] }));
  }
  assert.throws(() => parseSemanticCandidate({ ...wire, criteria: [...wire.criteria, ...wire.criteria] }));
  assert.throws(() => parseSemanticCandidate({ ...wire, sources: [...wire.sources, ...wire.sources] }));
  assert.throws(() => parseSemanticCandidate({ ...wire, candidate_identity_version: "structural" }));
  for (const patch of [{ source_id: "foreign" }, { hash: "c".repeat(64) }, { start: -1 }, { start: 3, end: 3 }, { start: 0.5 }]) {
    const c = { ...wire.criteria[0], citations: [{ ...criterion().citations[0], ...patch }] };
    assert.throws(() => parseSemanticCandidate({ ...wire, criteria: [c] }));
  }
  // Wire-only proposal parsing has no trusted milestone/snapshot context. The
  // API compiler owns foreign milestones, semantic duplicates and byte offsets.
  assert.doesNotThrow(() => parseSemanticProposal({ criteria: [{ ...criterion(), milestone_id: "foreign" }] }));
  assert.doesNotThrow(() => parseSemanticProposal({ criteria: [criterion(), criterion()] }));
});

test("capture DTO is Go []byte base64, preserves NUL, and rejects malformed/unsafe writes", () => {
  const capture = { id: "issue:1230", kind: "issue", provenance: "example/repo:issue", completeness: "complete", bytes: sourceBytes.toString("base64") };
  const parsed = semanticSourceCaptureSchema.parse(JSON.parse(JSON.stringify(capture)));
  assert.deepEqual(Buffer.from(parsed.bytes ?? "", "base64"), sourceBytes);
  assert.equal(semanticSourceCaptureSchema.parse({ ...capture, completeness: "unknown", bytes: null }).bytes, null);
  for (const patch of [
    { bytes: "%%%" }, { bytes: "/w==" }, { id: "bad\u0000id" },
    { provenance: "bad\u202eref" }, { kind: "plan" }, { completeness: "truncated" }, { hash: sourceHash },
  ]) assert.equal(semanticSourceCaptureSchema.safeParse({ ...capture, ...patch }).success, false);
});

test("all linked PRDs are captured through pinned bare show, preserving exact bytes", async () => {
  const { git, calls } = reader();
  const result = await captureSemanticSources(git, input("prds/1230-a.md and [second](prds/done/1231-b.md) prds/1230-a.md"));
  assert.equal(result.source_outcome, "complete");
  assert.equal(result.sources.length, 3);
  assert.deepEqual(calls.map(c => c.args), [["show", sha + ":prds/1230-a.md"], ["show", sha + ":prds/done/1231-b.md"]]);
  assert.ok(calls.every(c => c.path === "/worker/cache/repo.git" && c.opts.maxBytes === 256 * 1024 && c.opts.timeoutMs! > 0 && c.opts.timeoutMs! <= 1000));
  for (const source of result.sources.slice(1)) assert.deepEqual(Buffer.from(source.bytes, "base64"), sourceBytes);
  assert.match(result.sources[1]!.provenance, new RegExp("example/repo:" + sha + ":prds/"));
  assert.equal("hash" in result.sources[1]!, false);
});

test("optional trusted context checks complete snapshot hashes, byte offsets and milestone membership", () => {
  const source = {
    id: "issue:1230", kind: "issue", provenance: "example/repo:issue", completeness: "complete",
    bytes: sourceBytes.toString("base64"),
  };
  const context = { sources: [source], milestoneIds: ["M1"] };
  assert.deepEqual(parseSemanticCandidate(candidate(), context), candidate());
  assert.deepEqual(parseSemanticProposal(proposal(), context), proposal());
  for (const patch of [
    { end: sourceBytes.length + 1 }, { start: 9 }, // offset 9 splits é (bytes 8..10)
    { source_id: "foreign" }, { hash: "c".repeat(64) },
  ]) {
    assert.throws(() => parseSemanticProposal({
      criteria: [{ ...criterion(), citations: [{ ...criterion().citations[0], ...patch }] }],
    }, context));
  }
  assert.throws(() => parseSemanticProposal(proposal(), { ...context, milestoneIds: ["M2"] }));
  assert.throws(() => parseSemanticProposal(proposal(), { sources: [{ ...source, completeness: "unknown" }] }));
  assert.throws(() => parseSemanticProposal(proposal(), { sources: [source, source] }));
});

test("capture supports eight linked documents and rejects unsafe identity/ref/plan without writes", async () => {
  const { git, calls } = reader();
  const eight = await captureSemanticSources(git, input(Array.from({ length: 8 }, (_, i) => "prds/" + i + "-a.md").join(" ")));
  assert.equal(eight.sources.length, 9);
  assert.equal(eight.source_outcome, "complete");
  assert.equal(calls.length, 8);
  const url = await captureSemanticSources(git, input("[PRD](https://example.com/repo/blob/main/prds/1230-a.md#details)"));
  assert.equal(url.source_outcome, "complete");
  const punctuated = await captureSemanticSources(git, input("Read prds/1230-a.md."));
  assert.equal(punctuated.source_outcome, "complete");
  for (const patch of [
    { repo: "bad\u202erepo" }, { issue: { id: "bad\u0000id", bytes: Buffer.from("prds/1230-a.md") } },
    { planBytes: Buffer.from([0xc3]) },
  ]) {
    const result = await captureSemanticSources(git, { ...input(), ...patch });
    assert.equal(result.source_outcome, "unknown");
    assert.ok(result.sources.every(s => s.bytes === ""));
  }
});

test("legacy preferred-PRD behavior stays unchanged while semantic discovery retains unsafe candidates", () => {
  const description = "prds/1-a.md prds/2-b.md prds/../bad.md";
  assert.equal(findValidPrdCore(description, 2), "prds/2-b.md");
  assert.deepEqual(findSemanticPrdPaths(description), ["prds/1-a.md", "prds/2-b.md", "prds/../bad.md"]);
});

test("bad ref, unsafe paths, unreadable, missing, truncated and invalid UTF-8 are unknown with no prefix", async () => {
  for (const headSha of ["main", "a".repeat(7), "-x", sha + "\n"]) {
    const { git, calls } = reader();
    const result = await captureSemanticSources(git, { ...input(), headSha });
    assert.equal(result.source_outcome, "unknown");
    assert.ok(result.sources.every(s => s.bytes === "" && s.completeness === "unknown"));
    assert.equal(calls.length, 0);
  }
  for (const link of ["prds/../bad.md", "/prds/a.md", "../prds/a.md", "prds/.git/bad.md", "prds/a.md/evil", "prds/a\u202e.md", "prds/a\\bad.md"]) {
    const { git, calls } = reader();
    const result = await captureSemanticSources(git, input(link));
    assert.equal(result.source_outcome, "unknown", link);
    assert.equal(result.sources[1]!.bytes, "");
    assert.equal(calls.length, 0);
  }
  const failures: BarePrdReader[] = [
    { async readBare() { throw new Error("missing/unreadable"); } },
    { async readBare() { return { text: "prefix", bytes: Buffer.from("prefix"), truncated: true }; } },
    { async readBare() { return { text: "prefix", bytes: Buffer.from([0x70, 0xc3]), truncated: false }; } },
    { async readBare() { return { text: "prefix", truncated: false }; } },
  ];
  for (const git of failures) {
    const result = await captureSemanticSources(git, input());
    assert.equal(result.source_outcome, "unknown");
    assert.equal(result.sources[1]!.completeness, "unknown");
    assert.equal(result.sources[1]!.bytes, "");
  }
});

test("one failed PRD does not suppress other required sources; cancellation starts no reads", async () => {
  let reads = 0;
  const git: BarePrdReader = { async readBare() {
    if (++reads === 1) throw new Error("unreadable");
    return { text: "ok", bytes: Buffer.from("ok"), truncated: false };
  } };
  const result = await captureSemanticSources(git, input("prds/1-a.md prds/2-b.md"));
  assert.equal(reads, 2);
  assert.deepEqual(result.sources.map(s => s.completeness), ["complete", "unknown", "complete"]);
  assert.equal(result.source_outcome, "unknown");
  const aborted = new AbortController();
  aborted.abort();
  const cancelled = await captureSemanticSources(git, { ...input(), signal: aborted.signal });
  assert.equal(reads, 2);
  assert.equal(cancelled.source_outcome, "unknown");
  for (const timeoutMs of [0, -1, Infinity, NaN]) {
    assert.equal((await captureSemanticSources(git, { ...input(), timeoutMs })).source_outcome, "unknown");
  }
});

test("snapshot size limits are inclusive and overflow never captures a passing prefix", async () => {
  const { git } = reader(Buffer.alloc(256 * 1024, 0x61));
  const boundary = await captureSemanticSources(git, { ...input("prds/1230-a.md".padEnd(64 * 1024, " ")), planBytes: Buffer.alloc(256 * 1024, 0x61) });
  assert.equal(boundary.source_outcome, "complete");
  const oversized = await captureSemanticSources(reader(Buffer.alloc(256 * 1024 + 1, 0x61)).git, input());
  assert.equal(oversized.sources[1]!.bytes, "");
  for (const bytes of [undefined, Buffer.alloc(64 * 1024 + 1), Buffer.from([0xff])]) {
    const result = await captureSemanticSources(git, { ...input(), issue: { id: "issue:1230", bytes } });
    assert.equal(result.source_outcome, "unknown");
    assert.equal(result.sources[0]!.bytes, "");
  }
  const issueLimit = await captureSemanticSources(git, input("x".repeat(64 * 1024)));
  assert.equal(issueLimit.source_outcome, "complete");
  const tooMany = await captureSemanticSources(git, input(Array.from({ length: 9 }, (_, i) => "prds/" + i + "-a.md").join(" ")));
  assert.equal(tooMany.sources.length, 9);
  assert.equal(tooMany.source_outcome, "unknown");
  assert.ok(tooMany.sources.every(s => s.completeness === "unknown" && s.bytes === ""));
  const totalOverflow = await captureSemanticSources(git, { ...input("prds/1-a.md prds/2-b.md"), planBytes: Buffer.alloc(256 * 1024, 0x61) });
  assert.equal(totalOverflow.source_outcome, "unknown");
  assert.equal(totalOverflow.sources[2]!.bytes, "");
  const planOverflow = await captureSemanticSources(git, { ...input(), planBytes: Buffer.alloc(256 * 1024 + 1) });
  assert.ok(planOverflow.sources.every(s => s.bytes === "" && s.completeness === "unknown"));
});

test("shared server wire rejects missing offsets and unpaired escapes, retaining valid Unicode", () => {
  const raw = readFileSync(new URL("../../fixtures/semantic-plan/candidate.json", import.meta.url), "utf8");
  const fixture = JSON.parse(readFileSync(new URL("../../fixtures/semantic-plan/invalid-proposals.json", import.meta.url), "utf8")) as {
    cases: { name: string; from: string; to: string; valid: boolean }[];
  };
  for (const c of fixture.cases) {
    assert.equal(raw.split(c.from).length, 2, c.name);
    const wire = raw.replace(c.from, c.to);
    if (c.valid) assert.deepEqual(parseSemanticCandidate(wire), JSON.parse(wire), c.name);
    else assert.throws(() => parseSemanticCandidate(wire), c.name);
  }
});
test("canonical candidates reject incomplete cited sources without trusted context", () => {
  const wire = candidate();
  assert.throws(() => parseSemanticCandidate({
    ...wire, source_outcome: "unknown",
    sources: wire.sources.map(s => ({ ...s, completeness: "unknown" })),
  }));
});
