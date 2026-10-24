import { test, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash } from "node:crypto";
import { EventEmitter } from "node:events";
import {
  assertSnapshotTraversable, BRIEF_TOOLS, buildReplayClaim, createReplayRunDir, snapshotTraversalGap, type AncestorFacts, codexReleaseSeam, loadReplayCorpus, materializeCase, REPLAY_DIFF_ARGS, replayGit, replayIdentityEnv, runReplays,
  verifyCasePins,
  type MaterializedCase, type ReplayCase, type ReplayCorpus, type ReplayRecord, type RunReplaysDeps,
} from "../src/code-cross-check-replay.js";
import type { ClaudeCrossCheck } from "../src/claude-cross-check.js";
import { defaultReplayCliDeps, runReplayCli } from "../src/code-cross-check-replay-cli.js";
import { codeCrossCheckBrief, crossCheckPrompt } from "../src/codex/cross-check.js";
import { decodeCrossCheckClaim } from "../src/code-cross-check-contract.js";
import { CrossCheckCheckerUnavailableError } from "../src/codex/model-rejection.js";
import {
  buildTempCorpus, FIXTURE_BODY, FIXTURE_PLAN, FIXTURE_TITLE, type TempCorpus,
} from "./code-cross-check-replay-fixture.js";

const tc: TempCorpus = await buildTempCorpus();
after(() => tc.cleanup());
const scratch = path.join(tc.root, "scratch");
const sha256 = (s: string | Uint8Array): string => createHash("sha256").update(s).digest("hex");
const load = (): Promise<ReplayCorpus> => loadReplayCorpus(tc.checkout, { manifestSha256: tc.manifestSha256 });
const corpus = await load();
const defect = corpus.cases.find((c) => c.id === tc.defectId)!;
const findingsText = JSON.stringify({ findings: [{ id: "F1", severity: "major", path: "src/client.ts", line: 4, title: "UNIQUE-TITLE-XYZ", detail: "UNIQUE-DETAIL-XYZ" }] });
const env = { CLAUDE_CODE_OAUTH_TOKEN: ["fixture-", "oauth-no-authority"].join(""), OPENAI_API_KEY: ["fixture-", "openai-no-authority"].join("") };

type Run = Parameters<ClaudeCrossCheck["run"]>;
type Fake = (...args: Run) => Promise<string>;
const checker = (fn: Fake) => {
  const calls: Run[] = [];
  return { calls, run: async (...args: Run) => { calls.push(args); return fn(...args); } };
};
const completes = (text = findingsText): Fake => async (_c, _d, _h, _s, _p, confirm) => { confirm?.(); return text; };
const waitForAbort: Fake = (_c, _d, _h, signal) => new Promise((_resolve, reject) => {
  signal.addEventListener("abort", () => { reject(signal.reason); }, { once: true });
});

async function replay(over: Partial<RunReplaysDeps> & { checkers?: RunReplaysDeps["checkers"] } = {}): Promise<ReplayRecord[]> {
  const out: ReplayRecord[] = [];
  const records = await runReplays({
    checkout: tc.checkout, corpus: { ...corpus, cases: [defect] }, families: ["claude"], model: "haiku", effort: "low",
    repeats: 1, scratchDir: scratch, env, checkerVersion: { uzi_head: "a".repeat(40), agent_version: "0.0.0-test" },
    writeRecord: async (r) => { out.push(r); }, ...over,
  });
  assert.deepEqual(out, records);
  return records;
}

test("the claim passes the real decoder and prompt builder with a single-family secret block", () => {
  const inputs = defect.inputs;
  const opts = { model: "haiku", effort: "low" as const, timeoutMs: 1000, repeat: 2 };
  const claude = buildReplayClaim(defect, inputs, "claude", opts, { claudeToken: "tok-claude", openaiKey: "key-openai" });
  const codex = buildReplayClaim(defect, inputs, "codex", opts, { claudeToken: "tok-claude", openaiKey: "key-openai" });
  for (const claim of [claude, codex]) {
    assert.doesNotThrow(() => decodeCrossCheckClaim(claim.cross_check));
    const prompt = crossCheckPrompt(claim);
    for (const text of [FIXTURE_PLAN, FIXTURE_TITLE, FIXTURE_BODY, inputs.diff]) assert.ok(prompt.includes(text));
    assert.deepEqual((claim.cross_check as { milestones: unknown[] }).milestones, []);
    assert.equal(claim.cross_check?.stage, "code");
  }
  assert.equal(claude.run_id, `replay-${defect.id}-claude-2`);
  assert.equal(claude.secrets.anthropic_oauth_token, "tok-claude");
  assert.equal(claude.secrets.codex, undefined);
  assert.deepEqual(Object.keys(codex.secrets.codex!).sort(), ["access_token", "auth_mode", "capability"]);
  assert.equal(codex.secrets.codex!.access_token, "key-openai");
  assert.equal(codex.secrets.anthropic_oauth_token, undefined);
});

test("records carry every pinned field", async () => {
  const claudeOk = checker(completes());
  const codexOk = checker(completes());
  const records = await replay({ families: ["claude", "codex"], checkers: { claude: claudeOk, codex: codexOk } });
  assert.equal(records.length, 2);
  const [claude, codex] = records as [ReplayRecord, ReplayRecord];
  assert.equal(claude.credential_mode, "oauth_token");
  assert.equal(codex.credential_mode, "api_key");
  assert.equal(claude.reader_implementation, "claude sdk Read+Grep+Glob");
  assert.equal(codex.reader_implementation, "codex fileop Read+Search");
  assert.equal(claude.brief_sha256, sha256(codeCrossCheckBrief(BRIEF_TOOLS.claude)));
  assert.equal(codex.brief_sha256, sha256(codeCrossCheckBrief("Read and Search")));
  for (const r of records) {
    assert.deepEqual(r.checker_version, { uzi_head: "a".repeat(40), agent_version: "0.0.0-test", corpus_version: "test-1" });
    assert.equal(r.model, "haiku");
    assert.equal(r.effort, "low");
    assert.equal(r.outcome, "completed");
    assert.equal(r.input_provenance, "reconstructed");
    assert.equal(r.findings[0]?.id, "F1");
  }
  // The checker sees an isolated one-commit snapshot, and its scratch is gone afterwards.
  assert.equal(claudeOk.calls.length, 1);
  assert.deepEqual(await fs.readdir(scratch), []);
});

test("codex seam releases only API keys and never refreshes", async () => {
  const seam = codexReleaseSeam("key-1");
  assert.deepEqual(await seam.releaseCodex("r", { capability: "c" }, { authMode: "api_key" }), { auth_mode: "api_key", access_token: "key-1" });
  await assert.rejects(seam.releaseCodex("r", { capability: "c" }, { authMode: "subscription", chatgptAccountId: "a", minimumGeneration: 1 }));
  await assert.rejects(seam.refreshCodex("r", { capability: "c", operation_id: "o", observed_generation: 1 },
    { authMode: "subscription", chatgptAccountId: "a" }), /replay_refresh_unsupported/);
});

test("N repeats across both families run serially, one record per replay", async () => {
  let inFlight = 0;
  let max = 0;
  const seen: string[] = [];
  const serial: Fake = async (claim, _d, _h, _s, _p, confirm) => {
    inFlight++;
    max = Math.max(max, inFlight);
    seen.push(claim.run_id);
    await new Promise((resolve) => setTimeout(resolve, 5));
    inFlight--;
    confirm?.();
    return '{"findings":[]}';
  };
  const records = await replay({
    corpus, families: ["claude", "codex"], repeats: 3,
    checkers: { claude: checker(serial), codex: checker(serial) },
  });
  assert.equal(records.length, 2 * 2 * 3);
  assert.equal(max, 1);
  assert.deepEqual(seen.slice(0, 6), [1, 2, 3].map((n) => `replay-${tc.defectId}-claude-${n}`)
    .concat([1, 2, 3].map((n) => `replay-${tc.defectId}-codex-${n}`)));
  assert.equal(records.filter((r) => r.case_id === tc.controlId).length, 6);
});

test("outcomes are classified like the production runner and never record error text", async () => {
  const outcome = async (fn: Fake, over: Partial<RunReplaysDeps> = {}) =>
    (await replay({ checkers: { claude: checker(fn) }, ...over }))[0]!;
  assert.equal((await outcome(completes())).outcome, "completed");
  assert.equal((await outcome(waitForAbort, { timeoutMs: 20 })).outcome, "model_timeout");
  assert.equal((await outcome(completes("not json"))).outcome, "malformed");
  const boom = await outcome(async () => { throw new Error("SECRET-boom-text"); });
  assert.equal(boom.outcome, "model_error");
  assert.ok(!JSON.stringify(boom).includes("SECRET-boom-text"));
  assert.equal((await outcome(async () => { throw new CrossCheckCheckerUnavailableError(); })).outcome, "checker_unavailable");
  assert.equal((await outcome(async () => findingsText)).outcome, "confinement_failed");
  assert.equal((await outcome(async () => { throw new Error("cross-check confinement refused: x"); })).outcome, "confinement_failed");

  const outer = new AbortController();
  setTimeout(() => { outer.abort(); }, 30);
  assert.equal((await outcome(waitForAbort, { signal: outer.signal })).outcome, "interrupted");
  const pre = checker(completes());
  assert.equal((await replay({ checkers: { claude: pre }, signal: outer.signal }))[0]!.outcome, "interrupted");
  assert.equal(pre.calls.length, 0);

  const missing = checker(completes());
  const rec = (await replay({ checkers: { claude: missing }, env: {} }))[0]!;
  assert.equal(rec.outcome, "missing_auth");
  assert.equal(missing.calls.length, 0);
  assert.deepEqual(rec.findings, []);
});

test("cost is a number only when the family reports one", async () => {
  const withUsage = (payload: Record<string, unknown> | null): Fake => async (_c, _d, _h, _s, post, confirm) => {
    if (payload) await post(payload);
    confirm?.();
    return '{"findings":[]}';
  };
  const cost = async (family: "claude" | "codex", payload: Record<string, unknown> | null) =>
    (await replay({ families: [family], checkers: { [family]: checker(withUsage(payload)) } }))[0]!.cost_usd;
  assert.equal(await cost("claude", { event: "result", total_cost_usd: 0.5 }), 0.5);
  assert.equal(await cost("claude", { event: "result" }), null);
  assert.equal(await cost("claude", null), null);
  const entry = (status: "metered" | "unreported", usd?: number) => ({
    inputTokens: 1, outputTokens: 1, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, reasoningOutputTokens: 0,
    costStatus: status, ...(usd === undefined ? {} : { costUSD: usd }),
  });
  assert.equal(await cost("codex", { event: "result", modelUsage: { a: entry("metered", 0.25), b: entry("metered", 0.5) } }), 0.75);
  assert.equal(await cost("codex", { event: "result", modelUsage: { a: entry("metered", 0.25), b: entry("unreported") } }), null);
  assert.equal(await cost("codex", { event: "result", modelUsage: {} }), null);
  assert.equal(await cost("codex", { event: "result" }), null);
  assert.equal(await cost("codex", { event: "result", modelUsage: null }), null);
});

test("latency times only the checker call and is null when no checker ran", async () => {
  const slow = async (): Promise<MaterializedCase> => {
    await new Promise((resolve) => setTimeout(resolve, 200));
    return { snapshot: scratch, cleanup: async () => { await new Promise((resolve) => setTimeout(resolve, 200)); } };
  };
  const rec = (await replay({ materialize: slow, checkers: { claude: checker(completes()) } }))[0]!;
  assert.equal(rec.outcome, "completed");
  assert.ok(rec.latency_ms !== null && rec.latency_ms < 150, String(rec.latency_ms));
  assert.equal((await replay({ env: {} }))[0]!.latency_ms, null);
  const outer = new AbortController();
  outer.abort();
  assert.equal((await replay({ signal: outer.signal }))[0]!.latency_ms, null);
});

const quietSignals = new EventEmitter();
async function cli(args: string[], over: Partial<Parameters<typeof runReplayCli>[1]> = {}) {
  const lines: string[] = [];
  const errLines: string[] = [];
  const code = await runReplayCli(args, {
    checkout: tc.checkout, env, write: (l) => { lines.push(l); }, writeErr: (l) => { errLines.push(l); },
    signals: quietSignals, manifestSha256: tc.manifestSha256,
    uziHead: async () => "b".repeat(40), ...over,
  });
  return { code, lines, errLines };
}
const replayArgs = (extra: string[] = []) =>
  ["replay", "--family", "claude", "--model", "haiku", "--effort", "low", "--repeats", "1", ...extra];

test("replay refuses without explicit authorization before loading or constructing anything", async () => {
  const guard = checker(completes());
  const r = await cli(replayArgs(), { checkers: { claude: guard }, manifestSha256: "f".repeat(64) });
  assert.equal(r.code, 1);
  assert.deepEqual(r.lines, ['{"error":"authorization_required"}']);
  assert.equal(guard.calls.length, 0);
});

test("invalid arguments are never echoed and credential-bearing models are refused", async () => {
  const weird = "--WEIRD-arg-should-not-echo";
  const r = await cli(replayArgs([weird, "--authorize-real-model-calls"]));
  assert.deepEqual(r.lines, ['{"error":"invalid_cli"}']);
  assert.ok(!r.lines.join("").includes("WEIRD"));
  const secret = "zzsecretvalue9";
  const guard = checker(completes());
  const leak = await cli(["replay", "--family", "claude", "--model", `m-${secret}-x`, "--effort", "low", "--repeats", "1",
    "--authorize-real-model-calls"], { env: { OPENAI_API_KEY: secret }, checkers: { claude: guard } });
  assert.deepEqual(leak.lines, ['{"error":"invalid_cli"}']);
  assert.equal(guard.calls.length, 0);
  for (const bad of [["--repeats", "21"], ["--repeats", "0"], ["--effort", "extreme"], ["--family", "other"]]) {
    const args = replayArgs(["--authorize-real-model-calls"]);
    const at = args.indexOf(bad[0]!);
    args[at + 1] = bad[1]!;
    assert.deepEqual((await cli(args)).lines, ['{"error":"invalid_cli"}']);
  }
  assert.deepEqual((await cli(replayArgs(["--authorize-real-model-calls", "--authorize-real-model-calls"]))).lines, ['{"error":"invalid_cli"}']);
});

test("raw records go to an owner-only file; stdout carries ids, counts and pins only", async () => {
  const r = await cli(replayArgs(["--authorize-real-model-calls"]), { checkers: { claude: checker(completes()) } });
  assert.equal(r.code, 0);
  assert.equal(r.errLines.length, 1);
  const { records_file } = JSON.parse(r.errLines[0]!) as { records_file: string };
  assert.ok(path.isAbsolute(records_file));
  assert.equal(r.lines.length, 1);
  assert.ok(!r.lines.join("\n").includes("records_file"));
  const file = records_file;
  assert.equal((await fs.stat(file)).mode & 0o777, 0o600);
  const raw = await fs.readFile(file, "utf8");
  assert.ok(raw.includes("UNIQUE-TITLE-XYZ"));
  const stdout = r.lines.join("\n");
  for (const text of ["UNIQUE-TITLE-XYZ", "UNIQUE-DETAIL-XYZ", FIXTURE_TITLE, FIXTURE_BODY, env.CLAUDE_CODE_OAUTH_TOKEN, tc.root]) {
    assert.ok(!stdout.includes(text), "stdout must not carry " + text);
  }
  const aggregate = JSON.parse(r.lines[0]!) as { groups: { replays: number }[] };
  assert.equal(aggregate.groups.reduce((n, g) => n + g.replays, 0), 2);

  // The offline scorer reads the same file and agrees.
  const scored = await cli(["score", file]);
  assert.equal(scored.code, 0);
  assert.equal(scored.lines[0], r.lines[0]);
  // A path relative to the checkout root also works, whatever the cwd is.
  const relative = await cli(["score", path.relative(tc.checkout, file)]);
  assert.equal(relative.code, 0);
  assert.equal(relative.lines[0], r.lines[0]);
  const bad = await cli(["score", file, "--adjudications", path.join(tc.root, "missing.json")]);
  assert.equal(bad.code, 1);
  assert.deepEqual(bad.lines, ['{"error":"adjudication_invalid"}']);
  const noRecords = await cli(["score", path.join(tc.root, "missing.jsonl")]);
  assert.deepEqual(noRecords.lines, ['{"error":"records_invalid"}']);
  // A recorded model that embeds an env credential value is refused at score time, with the CLI's env.
  const leakedFile = path.join(tc.root, "leaked-model.jsonl");
  await fs.writeFile(leakedFile, raw.replaceAll('"model":"haiku"', '"model":"m-zzsecretvalue9-x"'));
  assert.equal((await cli(["score", leakedFile])).code, 0);
  const leaked = await cli(["score", leakedFile], { env: { ...env, OPENAI_API_KEY: "zzsecretvalue9" } });
  assert.equal(leaked.code, 1);
  assert.deepEqual(leaked.lines, ['{"error":"records_invalid"}']);
});

test("a mid-run failure still reports the partial records file", async () => {
  const r = await cli(replayArgs(["--authorize-real-model-calls"]), {
    materialize: async () => { throw new Error("disk exploded"); },
    checkers: { claude: checker(completes()) },
  });
  assert.equal(r.code, 1);
  assert.deepEqual(r.lines, ['{"error":"startup_failed"}']);
  assert.equal(r.errLines.length, 1);
  assert.ok(path.isAbsolute((JSON.parse(r.errLines[0]!) as { records_file: string }).records_file));
});

test("default wiring sends stdout lines to stdout and the locator channel to stderr", () => {
  const out: string[] = [];
  const err: string[] = [];
  const realOut = process.stdout.write;
  const realErr = process.stderr.write;
  const capture = (sink: string[]) => (chunk: string | Uint8Array): boolean => { sink.push(String(chunk)); return true; };
  process.stdout.write = capture(out) as typeof process.stdout.write;
  process.stderr.write = capture(err) as typeof process.stderr.write;
  try {
    const deps = defaultReplayCliDeps(tc.checkout);
    deps.write("to-stdout");
    deps.writeErr("to-stderr");
  } finally {
    process.stdout.write = realOut;
    process.stderr.write = realErr;
  }
  assert.deepEqual(out, ["to-stdout\n"]);
  assert.deepEqual(err, ["to-stderr\n"]);
});

test("verify materializes every case; a wrong manifest pin fails with a fixed code", async () => {
  assert.deepEqual((await cli(["verify"])).lines, ['{"verify":"ok","cases":2}']);
  const wrong = await cli(["verify"], { manifestSha256: "f".repeat(64) });
  assert.equal(wrong.code, 1);
  assert.deepEqual(wrong.lines, ['{"error":"corpus_integrity"}']);
});

async function tamper(file: string, run: () => Promise<void>): Promise<void> {
  const original = await fs.readFile(file);
  const flipped = Buffer.from(original);
  const at = flipped.length >> 1;
  flipped[at] = (flipped[at] ?? 0) ^ 1;
  await fs.writeFile(file, flipped);
  try { await run(); } finally { await fs.writeFile(file, original); }
}

test("any tampered corpus byte is refused with corpus_integrity", async () => {
  const d = tc.corpusDir;
  for (const rel of ["manifest.json", "pins.json", `cases/${tc.defectId}/case.json`, `cases/${tc.defectId}/plan.md`,
    `cases/${tc.defectId}/issue.md`, `cases/${tc.defectId}/prefix.patch`, `cases/${tc.defectId}/removed.patch`,
    `cases/${tc.defectId}/witness/client.blob`]) {
    await tamper(path.join(d, rel), async () => { await assert.rejects(load(), { message: "corpus_integrity" }, rel); });
  }
  await load();
});

async function reseal(mutate: (files: { pins: Record<string, any>; caseJson: Record<string, any> }) => Promise<void> | void): Promise<{ sha: string; restore(): Promise<void> }> {
  const m = path.join(tc.corpusDir, "manifest.json");
  const p = path.join(tc.corpusDir, "pins.json");
  const c = path.join(tc.corpusDir, `cases/${tc.defectId}/case.json`);
  const saved = await Promise.all([m, p, c].map((f) => fs.readFile(f)));
  const pins = JSON.parse(saved[1]!.toString());
  const caseJson = JSON.parse(saved[2]!.toString());
  await mutate({ pins, caseJson });
  const manifest = JSON.parse(saved[0]!.toString());
  const pinsText = JSON.stringify(pins);
  const caseText = JSON.stringify(caseJson);
  await fs.writeFile(p, pinsText);
  await fs.writeFile(c, caseText);
  manifest.pins_sha256 = sha256(pinsText);
  manifest.cases.find((e: { id: string }) => e.id === tc.defectId).case_sha256 = sha256(caseText);
  const manifestText = JSON.stringify(manifest);
  await fs.writeFile(m, manifestText);
  return { sha: sha256(manifestText), restore: async () => { await Promise.all([m, p, c].map((f, i) => fs.writeFile(f, saved[i]!))); } };
}

test("a consistently re-pinned corpus still fails on brief drift, oversize diff, symlinks and provenance lies", async () => {
  const drift = await reseal(({ pins }) => { pins.brief_sha256.claude = "0".repeat(64); });
  try { await assert.rejects(loadReplayCorpus(tc.checkout, { manifestSha256: drift.sha }), { message: "brief_drift" }); } finally { await drift.restore(); }

  const patch = path.join(tc.corpusDir, `cases/${tc.defectId}/prefix.patch`);
  const oldPatch = await fs.readFile(patch);
  const big = Buffer.alloc(1024 * 1024, 0x20);
  await fs.writeFile(patch, big);
  const oversize = await reseal(({ caseJson }) => { caseJson.artifacts["prefix.patch"] = sha256(big); });
  try { await assert.rejects(loadReplayCorpus(tc.checkout, { manifestSha256: oversize.sha }), { message: "diff_oversize" }); }
  finally { await oversize.restore(); await fs.writeFile(patch, oldPatch); }

  const lie = await reseal(({ caseJson }) => { caseJson.input_provenance = "recorded"; });
  try { await assert.rejects(loadReplayCorpus(tc.checkout, { manifestSha256: lie.sha }), { message: "corpus_integrity" }); } finally { await lie.restore(); }
  const milestones = await reseal(({ caseJson }) => { caseJson.milestones = []; });
  try { await assert.rejects(loadReplayCorpus(tc.checkout, { manifestSha256: milestones.sha }), { message: "corpus_integrity" }); } finally { await milestones.restore(); }

  const plan = path.join(tc.corpusDir, `cases/${tc.defectId}/plan.md`);
  const savedPlan = await fs.readFile(plan);
  const target = path.join(tc.root, "plan-copy.md");
  await fs.writeFile(target, savedPlan);
  await fs.rm(plan);
  await fs.symlink(target, plan);
  try { await assert.rejects(load(), { message: "corpus_integrity" }); } finally { await fs.rm(plan); await fs.writeFile(plan, savedPlan); }
  await load();
});

const git = async (cwd: string, args: string[], env?: Record<string, string>): Promise<string> =>
  (await replayGit(args, { cwd, env })).toString("utf8").trim();

test("materialize exports the pre-fix tree as a single parentless commit and cleans up", async () => {
  const m = await materializeCase(tc.checkout, defect, scratch);
  try {
    const client = await fs.readFile(path.join(m.snapshot, "src/client.ts"), "utf8");
    assert.ok(client.includes("const limit = 10;"));
    assert.ok(!client.includes("items.length"));
    assert.equal(await git(m.snapshot, ["rev-list", "--count", "HEAD"]), "1");
    assert.equal((await git(m.snapshot, ["rev-list", "--parents", "-n1", "HEAD"])).split(" ").length, 1);
    assert.equal(await git(m.snapshot, ["rev-parse", "--show-toplevel"]), await fs.realpath(m.snapshot));
  } finally { await m.cleanup(); }
  assert.deepEqual(await fs.readdir(scratch), []);
  const control = await materializeCase(tc.checkout, corpus.cases.find((c) => c.id === tc.controlId)!, scratch);
  try { assert.equal(await fs.readFile(path.join(control.snapshot, "src/other.ts"), "utf8"), "export const other = 2;\n"); } finally { await control.cleanup(); }
});

test("materialize writes exact blobs: tracked-but-ignored and export-subst files, tree equals prefix_tree", async () => {
  const m = await materializeCase(tc.checkout, defect, scratch);
  try {
    assert.equal(await fs.readFile(path.join(m.snapshot, "notes/ignored.log"), "utf8"), "tracked but ignored\n");
    assert.equal(await git(m.snapshot, ["ls-files", "notes/ignored.log"]), "notes/ignored.log");
    assert.equal(await fs.readFile(path.join(m.snapshot, "src/stamp.txt"), "utf8"), "$Format:%H$\n");
    assert.equal(await git(m.snapshot, ["rev-parse", "HEAD^{tree}"]), defect.prefix_tree);
    assert.equal(await git(m.snapshot, ["status", "--porcelain", "--ignored"]), "");
  } finally { await m.cleanup(); }
});

test("verifyCasePins reads exact prefix-tree blobs without a snapshot", async () => {
  const v = await verifyCasePins(tc.checkout, defect, scratch);
  try {
    assert.equal((await v.readFile("src/stamp.txt")).toString("utf8"), "$Format:%H$\n");
    assert.ok((await v.readFile("src/client.ts")).toString("utf8").includes("const limit = 10;"));
    assert.deepEqual((await fs.readdir(scratch)).filter((n) => n.startsWith("replay-snap-")), []);
    await assert.rejects(v.readFile("src/missing.ts"));
  } finally { await v.cleanup(); }
  assert.deepEqual(await fs.readdir(scratch), []);
});

test("each pin check fails on its own", async () => {
  const control = corpus.cases.find((c) => c.id === tc.controlId)!;
  // removed.patch must reach the squash tree: pin the defect to another commit's squash.
  await assert.rejects(verifyCasePins(tc.checkout, { ...defect, squash_sha: control.squash_sha }, scratch), { message: "corpus_integrity" });
  // The commit id must reproduce: an absent id and an existing commit with the same tree and parent but another identity.
  await assert.rejects(verifyCasePins(tc.checkout, { ...defect, prefix_commit: "4".repeat(40) }, scratch), { message: "corpus_integrity" });
  const lookalike = await git(tc.checkout, ["commit-tree", defect.prefix_tree, "-p", defect.base_sha, "-m", "another message"], { ...replayIdentityEnv(), GIT_COMMITTER_DATE: "2026-02-02T00:00:00Z" });
  await assert.rejects(verifyCasePins(tc.checkout, { ...defect, prefix_commit: lookalike }, scratch), { message: "corpus_integrity" });
  // Same tree, different patch bytes (longer abbreviations): only the byte comparison catches it.
  const longer = await replayGit(["diff", ...REPLAY_DIFF_ARGS.filter((a) => !a.startsWith("--abbrev")), "--abbrev=12", defect.base_sha, defect.prefix_commit],
    { cwd: tc.checkout });
  assert.ok(!longer.equals(defect.prefixPatch));
  await assert.rejects(verifyCasePins(tc.checkout, { ...defect, prefixPatch: longer }, scratch), { message: "corpus_integrity" });
  await (await verifyCasePins(tc.checkout, defect, scratch)).cleanup();
  assert.deepEqual(await fs.readdir(scratch), []);
});

test("materialize refuses a snapshot whose tree differs from prefix_tree (a gitlink cannot be exported)", async () => {
  const index = path.join(tc.root, "gitlink-index");
  const env = { GIT_INDEX_FILE: index };
  await replayGit(["read-tree", defect.base_sha], { cwd: tc.checkout, env });
  await replayGit(["update-index", "--add", "--cacheinfo", `160000,${"5".repeat(40)},vendor/sub`], { cwd: tc.checkout, env });
  const tree = await git(tc.checkout, ["write-tree"], env);
  const ident = replayIdentityEnv();
  const prefixCommit = await git(tc.checkout, ["commit-tree", tree, "-p", defect.base_sha, "-m", "code cross-check replay prefix c-gitlink"], ident);
  const squash = await git(tc.checkout, ["commit-tree", tree, "-p", defect.base_sha, "-m", "gitlink squash"], ident);
  const prefixPatch = await replayGit(["diff", ...REPLAY_DIFF_ARGS, defect.base_sha, prefixCommit], { cwd: tc.checkout });
  const gitlink: ReplayCase = {
    ...corpus.cases.find((c) => c.id === tc.controlId)!, id: "c-gitlink", squash_sha: squash, base_sha: defect.base_sha,
    prefix_tree: tree, prefix_commit: prefixCommit, prefixPatch, removedPatch: Buffer.alloc(0),
  };
  // Every pin holds, so verification passes and only the snapshot tree check can refuse.
  await (await verifyCasePins(tc.checkout, gitlink, scratch)).cleanup();
  await assert.rejects(materializeCase(tc.checkout, gitlink, scratch), { message: "corpus_integrity" });
  assert.deepEqual(await fs.readdir(scratch), []);
});

test("materialize reports a missing base object and an unpinned tree, leaving no scratch behind", async () => {
  const missing: ReplayCase = { ...defect, base_sha: "1".repeat(40) };
  await assert.rejects(materializeCase(tc.checkout, missing, scratch), { message: "corpus_objects_missing" });
  const wrongTree: ReplayCase = { ...defect, prefix_tree: "2".repeat(40) };
  await assert.rejects(materializeCase(tc.checkout, wrongTree, scratch), { message: "corpus_integrity" });
  const wrongWitness: ReplayCase = { ...defect, witnesses: [{ ...defect.witnesses[0]!, blob_id: "3".repeat(40) }] };
  await assert.rejects(materializeCase(tc.checkout, wrongWitness, scratch), { message: "corpus_integrity" });
  assert.deepEqual(await fs.readdir(scratch), []);
});

// ---------------------------------------------------------------------------------------------
// uid-split sharing, injected with a group this process belongs to (the real 10002 needs the image)

const groups = process.getgroups?.() ?? [];
const splitGid = groups.find((g) => g !== process.getgid?.()) ?? process.getgid!();
const split = { split: true, gid: splitGid };
const single = { split: false, gid: splitGid };
const modeOf = async (p: string): Promise<number> => (await fs.lstat(p)).mode & 0o7777;

async function walkAll(dir: string): Promise<string[]> {
  const out: string[] = [];
  for (const e of await fs.readdir(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    out.push(p);
    if (e.isDirectory()) out.push(...await walkAll(p));
  }
  return out;
}

test("the split run directory is search-only for the runner group; single-uid stays 0700", async () => {
  const root = await fs.mkdtemp(path.join(tc.root, "rundir-"));
  const shared = await createReplayRunDir(root, split);
  const plain = await createReplayRunDir(root, single);
  assert.equal(await modeOf(shared), 0o2710);
  assert.equal((await fs.lstat(shared)).gid, splitGid);
  assert.equal(await modeOf(plain), 0o700);
});

test("the split snapshot is group-readable, never group-writable, and single-uid keeps its modes", async () => {
  const run = await createReplayRunDir(await fs.mkdtemp(path.join(tc.root, "snap-")), split);
  const m = await materializeCase(tc.checkout, defect, run, undefined, split);
  try {
    assert.equal(await modeOf(m.snapshot), 0o2750);
    const entries = await walkAll(m.snapshot);
    assert.ok(entries.some((p) => p.includes(`${path.sep}.git${path.sep}objects${path.sep}`)));
    for (const p of entries) {
      const st = await fs.lstat(p);
      assert.equal(st.gid, splitGid, p);
      if (st.isDirectory()) assert.equal(st.mode & 0o7777, 0o2750, p);
      else {
        assert.ok(st.isFile());
        assert.equal(st.mode & 0o040, 0o040, `group read: ${p}`);
        assert.equal(st.mode & 0o020, 0, `no group write: ${p}`);
        assert.equal(st.mode & 0o007, 0, `no other access: ${p}`);
      }
    }
  } finally { await m.cleanup(); }
  const plainRun = await createReplayRunDir(await fs.mkdtemp(path.join(tc.root, "snap-")), single);
  const p = await materializeCase(tc.checkout, defect, plainRun, undefined, single);
  try {
    assert.equal(await modeOf(p.snapshot), 0o700);
    assert.equal((await fs.lstat(p.snapshot)).gid, process.getgid!());
  } finally { await p.cleanup(); }
});

test("under umask 002 the split snapshot still ends with no group write bit anywhere, root included", async () => {
  const run = await createReplayRunDir(await fs.mkdtemp(path.join(tc.root, "umask-")), split);
  const previous = process.umask(0o002);
  let m;
  try { m = await materializeCase(tc.checkout, defect, run, undefined, split); } finally { process.umask(previous); }
  try {
    assert.equal(await modeOf(m.snapshot), 0o2750);
    for (const p of await walkAll(m.snapshot)) assert.equal((await fs.lstat(p)).mode & 0o022, 0, p);
  } finally { await m.cleanup(); }
});

test("the snapshot walk never follows a tracked symlink to a file outside the snapshot", async () => {
  const outside = await fs.mkdtemp(path.join(tc.root, "outside-"));
  const secret = path.join(outside, "secret.txt");
  await fs.writeFile(secret, "private\n", { mode: 0o600 });
  await fs.chmod(secret, 0o600);
  const before = await fs.stat(secret);
  const linked = await buildTempCorpus({ symlinks: { "link-out": secret } });
  try {
    const lc = await loadReplayCorpus(linked.checkout, { manifestSha256: linked.manifestSha256 });
    const c = lc.cases.find((x) => x.id === linked.defectId)!;
    const run = await createReplayRunDir(await fs.mkdtemp(path.join(tc.root, "link-")), split);
    const m = await materializeCase(linked.checkout, c, run, undefined, split);
    try {
      assert.equal(await fs.readlink(path.join(m.snapshot, "link-out")), secret);
      const after = await fs.stat(secret);
      assert.equal(after.mode & 0o7777, 0o600);
      assert.equal(after.gid, before.gid);
      assert.equal(await modeOf(path.join(m.snapshot, "src/client.ts")) & 0o040, 0o040);
    } finally { await m.cleanup(); }
  } finally { await linked.cleanup(); }
});

test("runReplays creates the per-case HOME as runner-group 2770 under the split and 0700 otherwise", async () => {
  const seen: { mode: number; gid: number }[] = [];
  const probe = checker(async (_c, _d, home, _s, _p, confirm) => {
    const st = await fs.lstat(home);
    seen.push({ mode: st.mode & 0o7777, gid: st.gid });
    confirm?.();
    return findingsText;
  });
  for (const layout of [split, single]) {
    const dir = await createReplayRunDir(await fs.mkdtemp(path.join(tc.root, "home-")), layout);
    const records = await replay({ scratchDir: dir, layout, checkers: { claude: probe } });
    assert.equal(records[0]!.outcome, "completed");
    assert.deepEqual(await fs.readdir(dir), []);
  }
  assert.deepEqual(seen, [{ mode: 0o2770, gid: splitGid }, { mode: 0o700, gid: process.getgid!() }]);
});

test("traversal preflight decision: owner class alone applies to the owner, then group, then other", () => {
  const dir = (uid: number, gid: number, mode: number): AncestorFacts[] => [{ path: "/x", uid, gid, mode }];
  const gap = (a: AncestorFacts[]): string | null => snapshotTraversalGap(a);
  assert.equal(gap(dir(0, 0, 0o755)), null, "world-searchable");
  assert.equal(gap(dir(10001, 10001, 0o701)), null, "other search covers both uids");
  assert.equal(gap(dir(10001, 10002, 0o710)), null, "group runner covers the command uid and the runner owner-less group member");
  assert.equal(gap(dir(10001, 10001, 0o700)), "/x", "worker-private ancestor");
  assert.equal(gap(dir(10001, 10001, 0o750)), "/x", "an unrelated group grants neither");
  assert.equal(gap(dir(10001, 10002, 0o701)), "/x", "group members use the group class, not other");
  assert.equal(gap(dir(10003, 10002, 0o070)), "/x", "the owner uid is denied by its own bits even if the group could search");
  assert.equal(gap(dir(10002, 10002, 0o710)), null, "owner runner via owner bits, command uid via group");
  assert.equal(gap(dir(10002, 10002, 0o070)), "/x", "owner runner has no owner search");
  assert.equal(gap([{ path: "/a", uid: 0, gid: 0, mode: 0o755 }, { path: "/b", uid: 10001, gid: 10001, mode: 0o700 }, { path: "/c", uid: 10001, gid: 10001, mode: 0o700 }]), "/b", "first offender");
  assert.equal(snapshotTraversalGap(dir(10001, 10001, 0o700), [{ uid: 10001, groups: [10001] }]), null);
});

test("the traversal preflight refuses a symlinked ancestor and a private ancestor, and passes an open chain", async () => {
  const base = await fs.mkdtemp(path.join(tc.root, "pre-"));
  const real = path.join(base, "real");
  await fs.mkdir(path.join(real, "run"), { recursive: true });
  await fs.symlink(real, path.join(base, "link"));
  const me = [{ uid: process.getuid!(), groups: process.getgroups!() }];
  await assertSnapshotTraversable(path.join(real, "run"), me);
  await assert.rejects(assertSnapshotTraversable(path.join(base, "link", "run"), me), { message: "snapshot_untraversable" });
  // tc.root is a 0700 mkdtemp: the real reader uids (10003 and 10002) cannot all search it.
  await assert.rejects(assertSnapshotTraversable(path.join(real, "run")), { message: "snapshot_untraversable" });
  await assert.rejects(assertSnapshotTraversable(path.join(base, "missing", "run"), me), { message: "snapshot_untraversable" });
});

test("a split replay start fails snapshot_untraversable before any case, record file or checker call", async () => {
  const root = await fs.mkdtemp(path.join(tc.root, "cli-split-"));
  const guard = checker(completes());
  const r = await cli(replayArgs(["--authorize-real-model-calls"]), { scratchRoot: root, layout: split, checkers: { claude: guard } });
  assert.equal(r.code, 1);
  assert.deepEqual(r.lines, ['{"error":"snapshot_untraversable"}']);
  assert.deepEqual(r.errLines, []);
  assert.equal(guard.calls.length, 0);
  assert.deepEqual(await fs.readdir(root), []);
});
