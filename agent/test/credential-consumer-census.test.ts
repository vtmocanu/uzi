// Issue #4201 m2d: a source census of every place the agent reads the Anthropic run token. After an
// in-place credential switch every consumer must see the NEW token and stamp its frames with the NEW
// epoch ordinal, so a consumer may only take the token from the flight's CredentialHolder (or a context
// view the helper rebinds), never from a copy of `claim.secrets.anthropic_oauth_token` made at claim time.
//
// The census fails when a source file (any depth under src/) holds a token-reading line that is not an
// exact, counted, reviewed line below. The run flight's files and the type/redaction files are pinned to the
// exact trimmed text of each line and how many times it may occur; the claim-scoped runners (chat, judge,
// review, cross-check, isolated, job: none of them can switch in place, they never outlive their own claim)
// are pinned to a per-file count of reading lines. A new read anywhere, or a stray copy of an allowed line,
// therefore reddens this test and must be routed through the holder before it is added here.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
const TOKEN_READ = /anthropic_oauth_token|\boauthToken\b/;

/** Claim-scoped runners: each owns its own claim and credential for its whole life. The number is the exact
 *  count of token-reading code lines the file may hold. */
const CLAIM_SCOPED_BUDGET: Record<string, number> = {
  "chat-executor.ts": 4, "chat-runner.ts": 5, "claude-cross-check.ts": 4, "cross-check-runner.ts": 2,
  "code-cross-check-replay.ts": 1, "isolated-executor.ts": 2, "isolated-runner.ts": 3, "job-runner.ts": 1,
  "judge-runner.ts": 1, "review-runner.ts": 1,
};

interface Site { label: string; line: string; count: number }

/** Reviewed sites in files that are not claim-scoped, each pinned to the exact trimmed line and its count. */
const EXACT_SITES: Record<string, Site[]> = {
  "runner.ts": [
    { label: "seed: run-scoped secret registration at claim start", line: "if (claim.secrets.anthropic_oauth_token)", count: 1 },
    { label: "seed: run-scoped secret registration at claim start", line: "runScopedSecrets.push(claim.secrets.anthropic_oauth_token);", count: 1 },
    { label: "seed: the flight SecretSet", line: "claim.secrets.anthropic_oauth_token,", count: 1 },
    { label: "seed: the CredentialHolder", line: "credential: claim.secrets.anthropic_oauth_token", count: 1 },
    { label: "seed: the CredentialHolder", line: "? new CredentialHolder({ token: claim.secrets.anthropic_oauth_token, ordinal: 0 }) : undefined,", count: 1 },
    { label: "presence flag only (no value used)", line: "claude: !claim.secrets.codex && !!claim.secrets.anthropic_oauth_token,", count: 1 },
    { label: "seed: the context's initial token (rebound by the helper)", line: "oauthToken: claim.secrets.anthropic_oauth_token,", count: 1 },
    { label: "rebind: the helper points the context at the holder", line: "ctx.oauthToken = flight.credential!.current().token;", count: 1 },
    { label: "the editor pass's claim view built from the holder", line: "? { ...claim, secrets: { ...claim.secrets, anthropic_oauth_token: descCredential.token } }", count: 1 },
  ],
  "sdk-executor.ts": [
    { label: "type", line: "oauthToken: string;", count: 1 },
    { label: "plan summary: credentialFor snapshot", line: 'const cred = this.credentialFor(ctx, ctx.oauthToken ?? "");', count: 1 },
    { label: "drive rebind", line: "drive.state.rebindDrive = (token) => { drive.oauthToken = token; };", count: 1 },
    { label: "phaseSetup reads the (rebound) context at drive start", line: "const oauthToken = ctx.oauthToken?.trim();", count: 1 },
    { label: "phaseSetup reads the (rebound) context at drive start", line: "if (!oauthToken) {", count: 1 },
    { label: "phaseSetup reads the (rebound) context at drive start", line: "const env = buildSdkEnv(oauthToken, this.homeDir, toolEnv, this.dockerHost, ctx.runAttempt, ctx.managedGoWorkDir);", count: 1 },
    { label: "drive carrier fields", line: "oauthToken,", count: 2 },
    { label: "drive carrier fields", line: "!!oauthToken &&", count: 1 },
    { label: "intent summary: credentialFor snapshot", line: "const cred = this.credentialFor(ctx, oauthToken);", count: 1 },
    { label: "rebindCredential", line: "const token = (ctx.credential?.().token ?? ctx.oauthToken)?.trim();", count: 1 },
    { label: "rebindCredential", line: "ctx.oauthToken = token;", count: 1 },
  ],
  // The delivery editor pass reads its token from the claim VIEW the runner builds from the holder at the
  // publication's start (runner.ts `descClaim`).
  "summary-runner.ts": [
    { label: "claim view type", line: "readonly secrets: { readonly anthropic_oauth_token?: string; readonly codex?: unknown };", count: 1 },
    { label: "the editor pass's token, from the holder-built claim view", line: "const token = claim.secrets.anthropic_oauth_token?.trim();", count: 1 },
  ],
  // Pure types and the redaction-secret list (no model call is made with the value).
  "executor.ts": [{ label: "type", line: "oauthToken?: string;", count: 1 }],
  "sdk-env.ts": [
    { label: "env builder parameter", line: "oauthToken: string,", count: 1 },
    { label: "env builder value", line: "CLAUDE_CODE_OAUTH_TOKEN: oauthToken,", count: 1 },
  ],
  "protocol.ts": [
    { label: "type", line: "anthropic_oauth_token?: string;", count: 1 },
    { label: "type", line: "anthropic_oauth_token: string;", count: 1 },
  ],
  "diagram-diagnostic.ts": [
    { label: "claim type", line: "secrets: { anthropic_oauth_token?: string; codex?: unknown };", count: 1 },
    { label: "redaction list", line: "const secrets = [claim.secrets.anthropic_oauth_token, ...extraSecrets];", count: 1 },
  ],
  "now-summary.ts": [],
};

/** Every .ts file under `dir`, recursively, keyed by its path relative to `dir`. */
function readSources(dir: string, rel = ""): Map<string, string> {
  const out = new Map<string, string>();
  for (const entry of fs.readdirSync(path.join(dir, rel), { withFileTypes: true })) {
    const next = path.join(rel, entry.name);
    if (entry.isDirectory()) for (const [k, v] of readSources(dir, next)) out.set(k, v);
    else if (entry.name.endsWith(".ts")) out.set(next, fs.readFileSync(path.join(dir, next), "utf8"));
  }
  return out;
}

/** The token-reading code lines of one source (comment lines name the token without reading it). */
function tokenLines(source: string): Array<{ n: number; text: string }> {
  return source.split("\n")
    .map((text, i) => ({ n: i + 1, text }))
    .filter(({ text }) => !/^\s*(\/\/|\*|\/\*)/.test(text) && TOKEN_READ.test(text));
}

/** Why each file's token reads are not exactly the reviewed ones (empty when the census holds). */
function censusOffenders(sources: Map<string, string>): string[] {
  const offenders: string[] = [];
  for (const [file, source] of sources) {
    const hits = tokenLines(source);
    if (hits.length === 0) continue;
    const budget = CLAIM_SCOPED_BUDGET[file];
    if (budget !== undefined) {
      if (hits.length !== budget) offenders.push(`${file}: ${hits.length} read(s), the reviewed claim-scoped budget is ${budget} (first at line ${hits[0]!.n})`);
      continue;
    }
    const sites = EXACT_SITES[file];
    if (!sites) {
      offenders.push(`${file}: ${hits.length} read(s), not allowlisted (first at line ${hits[0]!.n})`);
      continue;
    }
    const remaining = new Map(sites.map((s) => [s.line, s.count]));
    for (const { n, text } of hits) {
      const left = remaining.get(text.trim());
      if (left === undefined) offenders.push(`${file}:${n}: unreviewed read: ${text.trim()}`);
      else if (left <= 0) offenders.push(`${file}:${n}: more copies of a reviewed line than allowed: ${text.trim()}`);
      else remaining.set(text.trim(), left - 1);
    }
  }
  return offenders;
}

describe("issue #4201 m2d: no consumer reads the run token outside the credential holder", () => {
  const sources = readSources(SRC);

  it("every token-reading line in src/ (any depth) is an exact reviewed line or inside a claim-scoped budget", () => {
    const offenders = censusOffenders(sources);
    assert.deepEqual(offenders.slice(0, 5), [], `first unlisted token reads (${offenders.length} total)`);
  });

  it("self-test: a stray read in runner.ts, a duplicated reviewed line, or a read in a nested directory all fail the census", () => {
    const runner = sources.get("runner.ts")!;
    assert.deepEqual(censusOffenders(new Map(sources).set("runner.ts", `${runner}\nvoid claim.secrets.anthropic_oauth_token\n`)).length, 1,
      "a stray `void claim.secrets.anthropic_oauth_token` is caught");
    assert.equal(censusOffenders(new Map(sources).set("runner.ts", `${runner}\n    const x = claim.secrets.anthropic_oauth_token\n`)).length, 1,
      "a line merely ending with the read is caught");
    assert.equal(censusOffenders(new Map(sources).set("runner.ts", `${runner}\nctx.oauthToken = flight.credential!.current().token;\n`)).length, 1,
      "a second copy of a reviewed line is caught");
    assert.equal(censusOffenders(new Map(sources).set("sdk-executor.ts", `${sources.get("sdk-executor.ts")!}\nif (!oauthToken) return;\n`)).length, 1,
      "a pattern that used to match anywhere in sdk-executor.ts is caught");
    assert.equal(censusOffenders(new Map(sources).set("summary-runner.ts", `${sources.get("summary-runner.ts")!}\nconst t = claim.secrets.anthropic_oauth_token;\n`)).length, 1,
      "a file that used to be allowlisted wholesale is caught");
    assert.equal(censusOffenders(new Map(sources).set(path.join("codex", "nested.ts"), "const t = claim.secrets.anthropic_oauth_token;\n")).length, 1,
      "a read in a subdirectory is caught");
    assert.equal(censusOffenders(new Map(sources).set("model-pass.ts", "const t = opts.oauthToken;\n")).length, 1,
      "a read in a file that used to be allowlisted but holds none is caught");
  });

  it("readSources recurses into subdirectories", () => {
    const tmp = fs.mkdtempSync(path.join(fs.realpathSync(process.env.TMPDIR ?? "/tmp"), "census-"));
    try {
      fs.mkdirSync(path.join(tmp, "a", "b"), { recursive: true });
      fs.writeFileSync(path.join(tmp, "top.ts"), "export {}\n");
      fs.writeFileSync(path.join(tmp, "a", "b", "deep.ts"), "export {}\n");
      assert.deepEqual([...readSources(tmp).keys()].sort(), [path.join("a", "b", "deep.ts"), "top.ts"]);
    } finally {
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });

  it("the flight consumers snapshot the holder at call start and stamp the epoch", () => {
    const nowSummary = sources.get("now-summary.ts")!;
    assert.match(nowSummary, /const cred = this\.deps\.credential\?\.\(\)/);
    assert.match(nowSummary, /\.\.\.\(cred \? \{ token: cred\.token \} : \{\}\)/);
    assert.match(nowSummary, /epochStamp\(call\.epoch\)/);
    const sdk = sources.get("sdk-executor.ts")!;
    assert.equal((sdk.match(/credentialEpochOrdinal: cred\.ordinal/g) ?? []).length, 2, "intent and plan summary frames");
    const runner = sources.get("runner.ts")!;
    assert.match(runner, /credential: \(\) => flight\.credential!\.current\(\)/);
    assert.match(runner, /credentialEpochOrdinal: descCredential\.ordinal/);
  });

  it("the reviewed lines each still exist as often as listed (a stale entry must be deleted)", () => {
    for (const [file, sites] of Object.entries(EXACT_SITES)) {
      const lines = tokenLines(sources.get(file) ?? "").map((l) => l.text.trim());
      for (const site of sites) {
        assert.equal(lines.filter((l) => l === site.line).length, site.count, `${file}: stale or miscounted census entry "${site.label}": ${site.line}`);
      }
    }
  });
});
