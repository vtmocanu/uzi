import { test } from "node:test";
import assert from "node:assert/strict";
import {
  parseAdjudications, parseReplayRecords, sanitizedAggregate, scoreReplays,
  CREDENTIAL_MODE, READER_IMPLEMENTATION, type Adjudication, type ExpectedDefect, type GroupAggregate, type ReplayRecord, type ScoreCorpus,
} from "../src/code-cross-check-replay.js";
import type { CodeFinding } from "../src/code-cross-check-contract.js";

const expected = (over: Partial<ExpectedDefect> & { id: string }): ExpectedDefect => ({
  paths: ["src/a.ts"], line_range: null, use_line_range: false, match: [], detectability: "read_only",
  rationale: "r", in_diff: true, signature: { path: "src/a.ts", literal: "x", present: true, witness: null }, ...over,
});
const corpus: ScoreCorpus = {
  corpus_version: "v-test",
  brief_sha256: { codex: "c".repeat(64), claude: "d".repeat(64) },
  cases: [
    { id: "d-a", kind: "defect", input_provenance: "reconstructed", expected: [
      expected({ id: "D1", line_range: [10, 20], use_line_range: true, match: ["off-by-one"] }),
      expected({ id: "D2", paths: ["src/b.ts", "src/c.ts"], match: ["Race"] }),
      expected({ id: "D3", detectability: "execution_only", paths: ["src/e.ts"], match: ["never"] }),
    ] },
    { id: "d-b", kind: "defect", input_provenance: "recorded", expected: [expected({ id: "D1", line_range: [1, 2], use_line_range: true })] },
    { id: "c-a", kind: "control", input_provenance: "reconstructed", expected: [] },
  ],
};
const finding = (id: string, over: Partial<CodeFinding> = {}): CodeFinding => ({
  id, severity: "major", path: "src/a.ts", line: 1, title: "t-" + id, detail: "d-" + id, ...over,
});
const rec = (over: Partial<ReplayRecord> & { case_id: string }): ReplayRecord => ({
  kind: over.case_id.startsWith("c-") ? "control" : "defect",
  input_provenance: over.case_id === "d-b" ? "recorded" : "reconstructed", family: "claude", repeat: 1,
  corpus_version: "v-test", checker_version: { uzi_head: "h", agent_version: "v", corpus_version: "v-test" },
  brief_sha256: "d".repeat(64), model: "m", effort: "low", reader_implementation: "reader", credential_mode: "oauth_token",
  outcome: "completed", latency_ms: 100, cost_usd: 1, findings: [], ...over,
});
const only = (records: ReplayRecord[], adj?: Adjudication[]): GroupAggregate => scoreReplays(corpus, records, adj).groups[0]!;

test("score rejects a foreign explicit replay profile even with matching brief hashes", () => {
  assert.throws(() => scoreReplays({ ...corpus, profile: "revised" }, [rec({ case_id: "d-a", profile: "baseline" })]),
    { message: "records_invalid" });
});
const defectScore = (g: GroupAggregate, case_id: string, id: string) => g.defects.find((d) => d.case_id === case_id && d.id === id)!;

test("a finding matches by path plus line, or path plus keyword, and not otherwise", () => {
  const g = only([
    rec({ case_id: "d-a", repeat: 1, findings: [finding("F1", { line: 15 })] }),
    rec({ case_id: "d-a", repeat: 2, findings: [finding("F1", { line: 30, title: "An OFF-BY-ONE here" })] }),
    rec({ case_id: "d-a", repeat: 3, findings: [finding("F1", { line: 15, path: "src/zzz.ts" })] }),
    rec({ case_id: "d-a", repeat: 4, findings: [finding("F1", { line: 30, title: "unrelated", detail: "nothing" })] }),
  ]);
  assert.deepEqual(defectScore(g, "d-a", "D1"), { case_id: "d-a", id: "D1", attempted: 4, completed: 4, matched: 2 });
});

test("a path-set defect without a line range matches by keyword on any listed path", () => {
  const g = only([
    rec({ case_id: "d-a", repeat: 1, findings: [finding("F1", { path: "src/c.ts", line: 999, detail: "a data race" })] }),
    rec({ case_id: "d-a", repeat: 2, findings: [finding("F1", { path: "src/a.ts", line: 999, detail: "a data race" })] }),
  ]);
  assert.equal(defectScore(g, "d-a", "D2").matched, 1);
});

test("scoring applies inline keyword patterns with word-level context (not the committed corpus patterns)", () => {
  const tight: ScoreCorpus = {
    ...corpus,
    cases: [{ id: "d-t", kind: "defect", input_provenance: "reconstructed", expected: [expected({
      id: "D3", paths: ["src/client.ts"], line_range: [100, 110], use_line_range: false,
      match: ["reader\\.cancel[\\s\\S]*(never|hang|settl|await|block)", "(await|awaited)[\\s\\S]*cancel[\\s\\S]*(never|hang|settl|block)", "cancel(lation)?\\b[\\s\\S]*(never|hang|settl)", "(never|hang|settl)[\\s\\S]*cancel"],
    })] }],
  };
  const score = (f: CodeFinding) => scoreReplays(tight, [rec({ case_id: "d-t", findings: [f] })]).groups[0]!.defects[0]!.matched;
  const base = { path: "src/client.ts", line: 500 };
  assert.equal(score(finding("F1", { ...base, title: "readBoundedText could use a comment", detail: "naming" })), 0);
  assert.equal(score(finding("F1", { ...base, title: "reader.cancel is fine", detail: "cleanup" })), 0);
  assert.equal(score(finding("F1", { ...base, title: "Awaited reader.cancel() can hang", detail: "x" })), 1);
  assert.equal(score(finding("F1", { ...base, title: "Overflow error lost", detail: "a source whose cancellation never settles" })), 1);
});

test("unmatched defect-case findings and every control finding are unadjudicated, never false positives", () => {
  const agg = scoreReplays(corpus, [
    rec({ case_id: "d-a", findings: [finding("F1", { line: 15 }), finding("F2", { path: "src/other.ts" })] }),
    rec({ case_id: "c-a", findings: [finding("F1"), finding("F2")] }),
  ]);
  const g = agg.groups[0]!;
  assert.equal(g.unmatched_findings, 1);
  assert.equal(g.control_findings, 2);
  assert.equal(g.unadjudicated_findings, 3);
  assert.ok(!("verified_false_positives" in g));
  const keys: string[] = [];
  JSON.stringify(JSON.parse(sanitizedAggregate(agg)), (k, v: unknown) => { keys.push(k); return v; });
  assert.deepEqual(keys.filter((k) => /false_positive|confirmed/.test(k)), []);
  assert.match(agg.scoring, /proxy/);
});

test("adjudications count only false_positive verdicts and reject unknown findings", () => {
  const records = [
    rec({ case_id: "d-a", findings: [finding("F1", { path: "src/o.ts" }), finding("F2", { path: "src/o.ts" }), finding("F3", { path: "src/o.ts" })] }),
    rec({ case_id: "c-a", findings: [finding("F1")] }),
  ];
  const adj: Adjudication[] = [
    { record_key: "d-a/claude/1", finding_id: "F1", verdict: "false_positive" },
    { record_key: "d-a/claude/1", finding_id: "F2", verdict: "valid" },
    { record_key: "c-a/claude/1", finding_id: "F1", verdict: "duplicate" },
  ];
  const g = only(records, adj);
  assert.equal(g.verified_false_positives, 1);
  assert.equal(g.adjudicated_findings, 3);
  assert.equal(g.unadjudicated_findings, 1);
  assert.throws(() => only(records, [{ record_key: "d-a/claude/1", finding_id: "NOPE", verdict: "valid" }]), { message: "adjudication_unknown_finding" });
  assert.throws(() => only(records, [{ record_key: "d-a/claude/9", finding_id: "F1", verdict: "valid" }]), { message: "adjudication_unknown_finding" });
  assert.throws(() => only(records, [adj[0]!, adj[0]!]), { message: "adjudication_duplicate" });
  assert.throws(() => parseAdjudications('[{"record_key":"a","finding_id":"b","verdict":"maybe"}]'), { message: "adjudication_invalid" });
  assert.deepEqual(parseAdjudications(JSON.stringify(adj)), adj);
});

test("execution-only defects are excluded and counted, not scored", () => {
  const g = only([rec({ case_id: "d-a", findings: [finding("F1", { path: "src/e.ts", title: "never terminates" })] })]);
  assert.equal(g.execution_only_excluded, 1);
  assert.ok(!g.defects.some((d) => d.id === "D3"));
  // The finding still matches the excluded defect, so it is not counted as unmatched.
  assert.equal(g.unmatched_findings, 0);
});

test("an incomplete replay is a miss for the attempted rate only", () => {
  const g = only([
    rec({ case_id: "d-b", repeat: 1, findings: [finding("F1", { line: 1 })] }),
    rec({ case_id: "d-b", repeat: 2, outcome: "model_timeout", findings: [] }),
  ]);
  assert.equal(g.replays, 2);
  assert.equal(g.completed, 1);
  assert.equal(g.incomplete_rate, 0.5);
  assert.equal(g.match_rate_completed, 1);
  assert.equal(g.match_rate_attempted, 0.5);
  assert.deepEqual(g.outcomes.model_timeout, 1);
  assert.equal(only([rec({ case_id: "c-a" })]).match_rate_completed, null);
});

test("unknown cost is counted apart from zero; latency percentiles ignore missing auth", () => {
  const g = only([
    rec({ case_id: "c-a", repeat: 1, cost_usd: 0, latency_ms: 10 }),
    rec({ case_id: "c-a", repeat: 2, cost_usd: 2, latency_ms: 20 }),
    rec({ case_id: "c-a", repeat: 3, cost_usd: 4, latency_ms: 30 }),
    rec({ case_id: "c-a", repeat: 4, cost_usd: null, latency_ms: 1000 }),
    rec({ case_id: "c-a", repeat: 5, outcome: "missing_auth", cost_usd: null, latency_ms: 0 }),
  ]);
  assert.equal(g.cost_usd_sum, 6);
  assert.equal(g.cost_usd_median, 2);
  assert.equal(g.cost_unknown, 1);
  assert.equal(g.latency_ms_median, 25);
  assert.equal(g.latency_ms_p90, 1000);
  assert.equal(g.latency_ms_max, 1000);
  const none = only([rec({ case_id: "c-a", cost_usd: null })]);
  assert.equal(none.cost_usd_median, null);
  assert.equal(none.cost_usd_sum, 0);
});

test("provenance and family split into separate groups with their pins", () => {
  const agg = scoreReplays(corpus, [
    rec({ case_id: "d-a" }),
    rec({ case_id: "d-b" }),
    rec({ case_id: "d-a", family: "codex", brief_sha256: "c".repeat(64), credential_mode: "api_key", model: "m2", repeat: 2 }),
    rec({ case_id: "d-a", family: "codex", brief_sha256: "c".repeat(64), model: "m3", repeat: 3 }),
  ]);
  assert.equal(agg.groups.length, 3);
  const codex = agg.groups.find((g) => g.family === "codex")!;
  assert.deepEqual(codex.model, ["m2", "m3"]);
  assert.deepEqual(codex.credential_mode, ["api_key", "oauth_token"]);
  assert.deepEqual(agg.brief_sha256, corpus.brief_sha256);
});

test("the sanitized aggregate carries no finding, issue or credential text", () => {
  const secret = ["tok-", "SECRET-VALUE"].join("");
  const agg = scoreReplays(corpus, [rec({
    case_id: "d-a", findings: [finding("F1", { path: "src/PRIVATE-PATH.ts", title: "PRIVATE-TITLE", detail: "PRIVATE-DETAIL " + secret })],
  })]);
  const text = sanitizedAggregate(agg);
  for (const leak of ["PRIVATE-PATH", "PRIVATE-TITLE", "PRIVATE-DETAIL", secret]) assert.ok(!text.includes(leak));
  assert.equal(JSON.parse(text).groups[0].unmatched_findings, 1);
});

test("record parsing is strict", () => {
  const good = (over: Partial<ReplayRecord> = {}): string => JSON.stringify(rec({
    case_id: "d-a", findings: [finding("F1")],
    reader_implementation: READER_IMPLEMENTATION.claude, credential_mode: CREDENTIAL_MODE.claude, ...over,
  }));
  const line = good();
  assert.equal(parseReplayRecords(line + "\n" + good({ repeat: 2 }) + "\n").length, 2);
  const credentialEnv = { UZI_API_TOKEN: "zzsecretvalue9", SHORT_KEY: "abc" };
  assert.throws(() => parseReplayRecords(good({ model: "m-zzsecretvalue9-x" }), credentialEnv), { message: "records_invalid" });
  assert.equal(parseReplayRecords(good({ model: "m-zzsecretvalue9-x" })).length, 1);
  assert.equal(parseReplayRecords(good({ model: "m-abc-x" }), credentialEnv).length, 1);
  assert.throws(() => parseReplayRecords(line + "\n" + line + "\n"), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(good({ model: "safe\u202eevil" })), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(good({ model: "sk-abcdefghijklmnop" })), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(good({ effort: "extreme" as ReplayRecord["effort"] })), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(good({ reader_implementation: "other" })), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(good({ credential_mode: CREDENTIAL_MODE.codex })), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(good({ family: "codex" })), { message: "records_invalid" });
  assert.equal(parseReplayRecords(good({ latency_ms: null }))[0]!.latency_ms, null);
  assert.throws(() => parseReplayRecords(""), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(line.replace('"completed"', '"done"')), { message: "records_invalid" });
  assert.throws(() => parseReplayRecords(line.slice(0, -1) + ',"extra":1}'), { message: "records_invalid" });
  assert.throws(() => scoreReplays(corpus, [rec({ case_id: "nope" })]), { message: "score_unknown_case" });
});

test("scoring refuses records that disagree with the loaded corpus", () => {
  const ok = rec({ case_id: "d-a" });
  assert.doesNotThrow(() => scoreReplays(corpus, [ok]));
  const bad = (over: Partial<ReplayRecord>): void => {
    assert.throws(() => scoreReplays(corpus, [rec({ case_id: "d-a", ...over })]), { message: "records_invalid" }, JSON.stringify(Object.keys(over)));
  };
  bad({ corpus_version: "other" });
  bad({ checker_version: { uzi_head: "h", agent_version: "v", corpus_version: "other" } });
  bad({ brief_sha256: "e".repeat(64) });
  bad({ brief_sha256: "d".repeat(64), family: "codex" });
  bad({ kind: "control" });
  bad({ input_provenance: "recorded" });
  bad({ outcome: "model_timeout", findings: [finding("F1")] });
  assert.throws(() => scoreReplays(corpus, [ok, { ...ok }]), { message: "records_invalid" });
  // Groups follow the case's provenance.
  assert.equal(scoreReplays(corpus, [rec({ case_id: "d-b" })]).groups[0]!.input_provenance, "recorded");
});

test("a null latency is skipped by the percentiles", () => {
  const g = only([
    rec({ case_id: "c-a", repeat: 1, latency_ms: 10 }),
    rec({ case_id: "c-a", repeat: 2, outcome: "interrupted", latency_ms: null }),
  ]);
  assert.equal(g.latency_ms_median, 10);
  assert.equal(g.latency_ms_max, 10);
});
