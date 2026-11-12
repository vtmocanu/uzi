// Issue #4201 m2d: a source census of every place the agent reads the Anthropic run token. After an
// in-place credential switch every consumer must see the NEW token and stamp its frames with the NEW
// epoch ordinal, so a consumer may only take the token from the flight's CredentialHolder (or a context
// view the helper rebinds), never from a copy of `claim.secrets.anthropic_oauth_token` made at claim time.
//
// The census fails when a source file reads the token and is neither (a) a claim-scoped runner that owns
// a whole separate claim (chat, judge, review, cross-check, isolated, job: none of them can switch in
// place, they never outlive their own claim), nor (b) one of the explicitly listed seed/consume sites of
// the run flight below. A new read in runner.ts or sdk-executor.ts therefore reddens this test and must
// be routed through the holder before its pattern is added.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
const TOKEN_READ = /anthropic_oauth_token|\boauthToken\b/;

/** Claim-scoped runners: each owns its own claim and credential for its whole life. */
const CLAIM_SCOPED = new Set([
  "chat-executor.ts", "chat-runner.ts", "claude-cross-check.ts", "cross-check-runner.ts", "code-cross-check-replay.ts",
  "isolated-executor.ts", "isolated-runner.ts", "job-runner.ts", "judge-runner.ts", "review-runner.ts",
  // Pure types, comments and redaction-secret lists (no model call is made with the value).
  "protocol.ts", "executor.ts", "sdk-env.ts", "model-pass.ts", "diagram-diagnostic.ts",
  // The delivery editor pass reads its token from the claim VIEW the runner builds from the holder at the
  // publication's start (runner.ts `descClaim`).
  "summary-runner.ts",
]);

/** The run flight's own reads. Each pattern is one reviewed site; the label says why it is safe. */
const FLIGHT_SITES: Record<string, Array<{ label: string; pattern: RegExp }>> = {
  "runner.ts": [
    { label: "seed: run-scoped secret registration at claim start", pattern: /runScopedSecrets\.push\(claim\.secrets\.anthropic_oauth_token\)|^\s*if \(claim\.secrets\.anthropic_oauth_token\)$/ },
    { label: "seed: the flight SecretSet", pattern: /^\s*claim\.secrets\.anthropic_oauth_token,$/ },
    { label: "seed: the CredentialHolder", pattern: /claim\.secrets\.anthropic_oauth_token\s*$|new CredentialHolder\(\{ token: claim\.secrets\.anthropic_oauth_token/ },
    { label: "presence flag only (no value used)", pattern: /claude: !claim\.secrets\.codex && !!claim\.secrets\.anthropic_oauth_token/ },
    { label: "seed: the context's initial token (rebound by the helper)", pattern: /^\s*oauthToken: claim\.secrets\.anthropic_oauth_token,$/ },
    { label: "rebind: the helper points the context at the holder", pattern: /ctx\.oauthToken = flight\.credential!\.current\(\)\.token/ },
    { label: "the editor pass's claim view built from the holder", pattern: /anthropic_oauth_token: descCredential\.token/ },
  ],
  "sdk-executor.ts": [
    { label: "type", pattern: /^\s*oauthToken: string;$/ },
    { label: "plan summary: credentialFor snapshot", pattern: /this\.credentialFor\(ctx, ctx\.oauthToken \?\? ""\)/ },
    { label: "drive rebind", pattern: /drive\.oauthToken = token/ },
    { label: "phaseSetup reads the (rebound) context at drive start", pattern: /const oauthToken = ctx\.oauthToken\?\.trim\(\)|if \(!oauthToken\)|buildSdkEnv\(oauthToken,/ },
    { label: "drive carrier fields", pattern: /^\s*oauthToken,$|!!oauthToken &&$/ },
    { label: "intent summary: credentialFor snapshot", pattern: /this\.credentialFor\(ctx, oauthToken\)/ },
    { label: "rebindCredential", pattern: /ctx\.oauthToken|credential: ctx\.oauthToken/ },
  ],
  "now-summary.ts": [],
};

function codeLines(file: string): Array<{ n: number; text: string }> {
  return fs.readFileSync(path.join(SRC, file), "utf8").split("\n")
    .map((text, i) => ({ n: i + 1, text }))
    // Comment lines name the token without reading it.
    .filter(({ text }) => !/^\s*(\/\/|\*|\/\*)/.test(text));
}

describe("issue #4201 m2d: no consumer reads the run token outside the credential holder", () => {
  const files = fs.readdirSync(SRC).filter((f) => f.endsWith(".ts"));

  it("every source file that reads the token is claim-scoped or an allowlisted flight site", () => {
    const offenders: string[] = [];
    for (const file of files) {
      if (CLAIM_SCOPED.has(file)) continue;
      const hits = codeLines(file).filter(({ text }) => TOKEN_READ.test(text));
      if (hits.length === 0) continue;
      const sites = FLIGHT_SITES[file];
      if (!sites) {
        offenders.push(`${file}: ${hits.length} read(s), not allowlisted (first at line ${hits[0]!.n})`);
        continue;
      }
      for (const { n, text } of hits) {
        if (!sites.some((s) => s.pattern.test(text))) offenders.push(`${file}:${n}: ${text.trim()}`);
      }
    }
    assert.deepEqual(offenders.slice(0, 5), [], `first unlisted token reads (${offenders.length} total)`);
  });

  it("the flight consumers snapshot the holder at call start and stamp the epoch", () => {
    const nowSummary = fs.readFileSync(path.join(SRC, "now-summary.ts"), "utf8");
    assert.match(nowSummary, /const cred = this\.deps\.credential\?\.\(\)/);
    assert.match(nowSummary, /\.\.\.\(cred \? \{ token: cred\.token \} : \{\}\)/);
    assert.match(nowSummary, /epochStamp\(call\.epoch\)/);
    const sdk = fs.readFileSync(path.join(SRC, "sdk-executor.ts"), "utf8");
    assert.equal((sdk.match(/credentialEpochOrdinal: cred\.ordinal/g) ?? []).length, 2, "intent and plan summary frames");
    const runner = fs.readFileSync(path.join(SRC, "runner.ts"), "utf8");
    assert.match(runner, /credential: \(\) => flight\.credential!\.current\(\)/);
    assert.match(runner, /credentialEpochOrdinal: descCredential\.ordinal/);
  });

  it("the census patterns each still match a real line (a stale pattern must be deleted)", () => {
    for (const [file, sites] of Object.entries(FLIGHT_SITES)) {
      const lines = codeLines(file).map((l) => l.text);
      for (const site of sites) {
        assert.ok(lines.some((l) => site.pattern.test(l)), `${file}: stale census pattern "${site.label}"`);
      }
    }
  });
});
