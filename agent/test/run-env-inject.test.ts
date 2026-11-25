import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { buildSdkEnv, REASON_RUN_ENV_NEEDS_SPLIT, sdkRunEnvInjected, type SdkCredential } from "../src/sdk-env.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { buildCommandEnv, commandRunEnvInjected, withCommandGitTrust } from "../src/codex/codex-executor.js";
import { foldRunEnv, runEnvDropText, runEnvInjectedSet } from "../src/run-env-inject.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";
import { makeTextRedactor } from "../src/redact.js";
import { RUN_ENV_DENIED_NAMES, RUN_ENV_DENIED_PREFIXES } from "../src/run-env-names.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";

// PRD 4416 M2: injection of user variables into the lead lane's env builders, and the forms
// registered for redaction. No token-shaped literal lives here: every value is an obvious dummy.

const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const GATEWAY_KEY = ["gw", "key", "do-not-scan", "5555", "abcdef"].join("-");

function v(name: string, value = `dummy-value-${name.toLowerCase()}`, secret = false): ClaimRunEnvVar {
  return { var_id: UUID, scope: "repo", rev: 1, name, value, secret, description: "" };
}

// Every denied fixture entry: all exact names, a name under every prefix, and a proxy suffix name.
const DENIED: string[] = [
  ...RUN_ENV_DENIED_NAMES.map((r) => r.value),
  ...RUN_ENV_DENIED_PREFIXES.map((r) => `${r.value}TESTVAR`),
  "HTTPS_PROXY",
  "GIT_CONFIG_KEY_0",
  "GIT_CONFIG_VALUE_0",
  "GH_TOKEN",
  "GITLAB_TOKEN",
  "BUN_BE_BUN",
  "PATH",
];
const ALLOWED = [
  "AWS_REGION", "GOOGLE_CLOUD_PROJECT", "NODE_ENV",
  "NPM_TOKEN", "NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", "GRADLE_OPTS", "MAVEN_OPTS",
  "ORG_GRADLE_PROJECT_X", "TESTCONTAINERS_X", "DOCKER_AUTH_CONFIG",
];
const hostile = [...DENIED.map((n) => v(n)), ...ALLOWED.map((n) => v(n))];

function collector(): { drops: Array<[string, string]>; onDrop: (n: string, r: string) => void } {
  const drops: Array<[string, string]> = [];
  return { drops, onDrop: (n, r) => drops.push([n, r]) };
}

function assertOnlyAllowedAdded(
  baseline: Record<string, string | undefined>,
  withVars: Record<string, string | undefined>,
  allowed: readonly string[],
): void {
  for (const k of Object.keys(baseline)) {
    assert.ok(Object.hasOwn(withVars, k), `${k} vanished`);
    assert.equal(withVars[k], baseline[k], `${k} changed`);
  }
  const added = Object.keys(withVars).filter((k) => !Object.hasOwn(baseline, k)).sort();
  assert.deepEqual(added, [...allowed].sort());
  for (const k of allowed) assert.equal(withVars[k], `dummy-value-${k.toLowerCase()}`);
  for (const name of DENIED) {
    if (!Object.hasOwn(baseline, name)) assert.ok(!Object.hasOwn(withVars, name), `${name} injected`);
  }
}

// These tests model the protected two-uid lead lane, the only posture on which buildSdkEnv delivers
// run variables (sdk-env.ts assertRunEnvDeliverable). The single-uid refusal is pinned below and in
// sdk-env.test.ts.
let savedSplit: string | undefined;
before(() => { savedSplit = process.env.UZI_UID_SPLIT; process.env.UZI_UID_SPLIT = "1"; });
after(() => {
  if (savedSplit === undefined) delete process.env.UZI_UID_SPLIT;
  else process.env.UZI_UID_SPLIT = savedSplit;
});

describe("buildSdkEnv on a single-uid worker", () => {
  it("refuses run variables and still builds an env without them", () => {
    const cred: SdkCredential = { kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" };
    delete process.env.UZI_UID_SPLIT;
    try {
      assert.throws(() => buildSdkEnv(cred, "/h", {}, undefined, undefined, undefined, undefined, { vars: [v("NPM_TOKEN")] }),
        (err: unknown) => err instanceof TrustedExecutionRefusal && err.message === REASON_RUN_ENV_NEEDS_SPLIT);
      assert.ok(!Object.hasOwn(buildSdkEnv(cred, "/h", {}, undefined, undefined, undefined, undefined, { vars: [] }), "NPM_TOKEN"));
    } finally { process.env.UZI_UID_SPLIT = "1"; }
  });
});

describe("buildSdkEnv with run variables", () => {
  const creds: Array<[string, SdkCredential]> = [
    ["oauth", { kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" }],
    ["provider", {
      kind: "provider", baseUrl: "https://gw.example.test/anthropic", authStyle: "bearer", key: GATEWAY_KEY,
      modelMap: { opus: { model: "corp-opus" }, sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku", context_window: 200000 } },
    }],
  ];
  const attempt = { marker: "run:att", clonePath: "/data/runner/x", cloneKey: "x/y" };

  for (const [label, cred] of creds) {
    it(`${label}: the hostile catalogue changes nothing but the allowed names`, () => {
      const base = buildSdkEnv(cred, "/data/home", { NIX_SSL_CERT_FILE: "/nix/ca.pem" }, "unix:///run/dind.sock", attempt, "/data/gotmp", "/data/runtmp");
      const { drops, onDrop } = collector();
      const env = buildSdkEnv(cred, "/data/home", { NIX_SSL_CERT_FILE: "/nix/ca.pem" }, "unix:///run/dind.sock", attempt, "/data/gotmp", "/data/runtmp", { vars: hostile, onDrop });
      assertOnlyAllowedAdded(base, env, ALLOWED);
      // Every denied name was reported with a rule, once, and no value reached the report.
      assert.deepEqual(drops.map(([n]) => n).sort(), [...DENIED].sort());
      for (const [, rule] of drops) assert.ok(rule.length > 0 && !rule.includes("dummy-value"));
    });
  }

  it("no variables leaves the env identical", () => {
    const cred = creds[0]![1];
    assert.deepEqual(buildSdkEnv(cred, "/h", {}, undefined, undefined, undefined, undefined, { vars: [] }), buildSdkEnv(cred, "/h"));
  });

  it("a tool-profile key of the same name wins over the user variable", () => {
    const { drops, onDrop } = collector();
    const env = buildSdkEnv(creds[0]![1], "/h", { NPM_TOKEN: "from-tool-profile" }, undefined, undefined, undefined, undefined,
      { vars: [v("NPM_TOKEN"), v("PIP_INDEX_URL")], onDrop });
    assert.equal(env.NPM_TOKEN, "from-tool-profile");
    assert.equal(env.PIP_INDEX_URL, "dummy-value-pip_index_url");
    assert.deepEqual(drops.map(([n]) => n), ["NPM_TOKEN"]);
  });

  it("a smuggled PATH or BUN_BE_BUN is dropped and the worker PATH is intact", () => {
    const base = buildSdkEnv(creds[0]![1], "/h");
    const env = buildSdkEnv(creds[0]![1], "/h", {}, undefined, undefined, undefined, undefined, { vars: [v("PATH", "/evil"), v("BUN_BE_BUN", "1")] });
    assert.equal(env.PATH, base.PATH);
    assert.ok(!Object.hasOwn(env, "BUN_BE_BUN"));
  });
});

describe("buildCommandEnv with run variables", () => {
  const tool = { PATH: "/nix/bin", NIX_SSL_CERT_FILE: "/nix/ca.pem" };

  it("the hostile catalogue changes nothing but the allowed names, git config included", () => {
    const base = buildCommandEnv("/private/tmp", tool, "unix:///run/dind.sock", true);
    const { drops, onDrop } = collector();
    const env = buildCommandEnv("/private/tmp", tool, "unix:///run/dind.sock", true, { vars: hostile, onDrop });
    assertOnlyAllowedAdded(base, env, ALLOWED);
    assert.deepEqual(drops.map(([n]) => n).sort(), [...DENIED].sort());
    // The worker's git trust config is built over it unchanged.
    const trusted = withCommandGitTrust(env, "/checkout");
    const trustedBase = withCommandGitTrust(base, "/checkout");
    for (const k of Object.keys(trustedBase).filter((key) => key.startsWith("GIT_"))) assert.equal(trusted[k], trustedBase[k]);
    assert.deepEqual(Object.keys(trusted).filter((key) => key.startsWith("GIT_")).sort(), Object.keys(trustedBase).filter((key) => key.startsWith("GIT_")).sort());
  });

  it("a tool-profile key of the same name wins over the user variable", () => {
    const env = buildCommandEnv("/private/tmp", { NPM_TOKEN: "from-tool-profile" }, undefined, false, { vars: [v("NPM_TOKEN")] });
    assert.equal(env.NPM_TOKEN, "from-tool-profile");
  });
});

describe("foldRunEnv protected predicate", () => {
  it("drops an otherwise allowed name the builder protects, and reports it", () => {
    const env: Record<string, string | undefined> = {};
    const { drops, onDrop } = collector();
    foldRunEnv(env, { vars: [v("MY_FLAG"), v("OTHER_FLAG")], onDrop }, (k) => k === "MY_FLAG");
    assert.ok(!Object.hasOwn(env, "MY_FLAG"));
    assert.equal(env.OTHER_FLAG, "dummy-value-other_flag");
    assert.deepEqual(drops, [["MY_FLAG", "protected worker key"]]);
  });
});

describe("runEnvInjectedSet", () => {
  const names = (vars: readonly ClaimRunEnvVar[]): string[] => vars.map((x) => x.name);

  it("names the same variables and drops, in the same order, as foldRunEnv over the same env", () => {
    const pre: Record<string, string | undefined> = { PRESET_KEY: "x", UNSET_KEY: undefined, PINNED_KEY: "p" };
    const vars = [...hostile, v("PRESET_KEY"), v("UNSET_KEY"), v("PINNED_KEY"), v("TWICE_NAMED", "first"), v("TWICE_NAMED", "second"), v("LAST_ONE")];
    const isProtected = (k: string): boolean => k === "PINNED_KEY";
    const env = { ...pre };
    const { drops, onDrop } = collector();
    const folded = foldRunEnv(env, { vars, onDrop }, isProtected);
    const set = runEnvInjectedSet(vars, Object.keys(pre), isProtected);
    assert.deepEqual(set.injected, folded);
    assert.deepEqual(set.dropped.map((d) => [d.name, d.rule]), drops);
    assert.deepEqual(Object.keys(env).filter((k) => !Object.hasOwn(pre, k)).sort(), names(set.injected).sort());
    assert.deepEqual(set.injected.filter((x) => x.name === "TWICE_NAMED").map((x) => x.value), ["first"], "the first of two same-named variables wins");
    assert.ok(!set.injected.some((x) => ["PRESET_KEY", "UNSET_KEY", "PINNED_KEY"].includes(x.name)));
  });

  it("foldRunEnv applies exactly a precomputed set and nothing else", () => {
    const vars = [v("FIRST_ONE"), v("SECOND_ONE")];
    const target: Record<string, string | undefined> = {};
    const folded = foldRunEnv(target, { vars, set: { injected: [vars[0]!], dropped: [{ name: "SECOND_ONE", rule: "test rule" }] } }, () => false);
    assert.deepEqual(names(folded), ["FIRST_ONE"]);
    assert.deepEqual(Object.keys(target), ["FIRST_ONE"]);
  });

  it("foldRunEnv throws on a precomputed set that holds a protected, invalid or already-set name, leaving env unchanged", () => {
    const secretValue = "dummy-secret-run-value-000000";
    const bad = (name: string): ClaimRunEnvVar => v(name, secretValue);
    for (const name of ["PINNED_KEY", "PATH", "lower-case", "PRESET_KEY"]) {
      const target: Record<string, string | undefined> = { PRESET_KEY: "x" };
      const vars = [v("FIRST_ONE"), bad(name)];
      assert.throws(
        () => foldRunEnv(target, { vars, set: { injected: vars, dropped: [] } }, (k) => k === "PINNED_KEY"),
        (e: Error) => e.message.includes(name) && !e.message.includes(secretValue),
        name,
      );
      assert.deepEqual(target, { PRESET_KEY: "x" }, `${name}: env unchanged`);
    }
    const dup = [v("DUP_ONE"), v("DUP_ONE")];
    const dupTarget: Record<string, string | undefined> = { PRESET_KEY: "x" };
    assert.throws(
      () => foldRunEnv(dupTarget, { vars: dup, set: { injected: dup, dropped: [] } }, () => false),
      (e: Error) => e.message.includes("DUP_ONE"),
      "duplicate name",
    );
    assert.deepEqual(dupTarget, { PRESET_KEY: "x" }, "duplicate name: env unchanged");
  });

  it("is pure: it reads no env and leaves its inputs unchanged", () => {
    const vars = [v("A_ONE"), v("PATH")];
    const keys = ["X_KEY"];
    const before = JSON.stringify([vars, keys]);
    runEnvInjectedSet(vars, keys, () => false);
    assert.equal(JSON.stringify([vars, keys]), before);
  });

  it("buildSdkEnv injects exactly sdkRunEnvInjected's list, tool-profile shadows included", () => {
    const cred: SdkCredential = { kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" };
    const tool = { NPM_TOKEN: "from-tool-profile", NIX_SSL_CERT_FILE: "/nix/ca.pem" };
    const vars = [...hostile, v("NPM_TOKEN"), v("REGION")];
    const base = buildSdkEnv(cred, "/h", tool);
    const set = sdkRunEnvInjected(vars, cred, "/h", tool);
    const injected = set.injected;
    const { drops, onDrop } = collector();
    const env = buildSdkEnv(cred, "/h", tool, undefined, undefined, undefined, undefined, { vars, set, onDrop });
    assert.deepEqual(Object.keys(env).filter((k) => !Object.hasOwn(base, k)).sort(), names(injected).sort());
    assert.ok(names(injected).includes("REGION"), "the set is not empty");
    assert.ok(!names(injected).includes("NPM_TOKEN"));
    assert.equal(env.NPM_TOKEN, "from-tool-profile");
    assert.deepEqual(drops, set.dropped.map((d) => [d.name, d.rule]), "the builder reports the set's drops");
    assert.deepEqual(sdkRunEnvInjected([], cred, "/h", tool).injected, []);
    const computed = buildSdkEnv(cred, "/h", tool, undefined, undefined, undefined, undefined, { vars });
    assert.deepEqual(env, computed, "folding the precomputed set equals folding over the env");
  });

  it("buildCommandEnv injects exactly commandRunEnvInjected's list, tool-profile shadows included", () => {
    const tool = { PATH: "/nix/bin", NPM_TOKEN: "from-tool-profile" };
    const vars = [...hostile, v("NPM_TOKEN"), v("REGION")];
    const base = buildCommandEnv("/private/tmp", tool);
    const set = commandRunEnvInjected(vars, "/private/tmp", tool);
    const injected = set.injected;
    const { drops, onDrop } = collector();
    const env = buildCommandEnv("/private/tmp", tool, undefined, false, { vars, set, onDrop });
    assert.deepEqual(Object.keys(env).filter((k) => !Object.hasOwn(base, k)).sort(), names(injected).sort());
    assert.ok(names(injected).includes("REGION"), "the set is not empty");
    assert.ok(!names(injected).includes("NPM_TOKEN"));
    assert.equal(env.NPM_TOKEN, "from-tool-profile");
    assert.deepEqual(drops, set.dropped.map((d) => [d.name, d.rule]), "the builder reports the set's drops");
    assert.deepEqual(env, buildCommandEnv("/private/tmp", tool, undefined, false, { vars }), "folding the precomputed set equals folding over the env");
  });
});

describe("dropped-name event text", () => {
  it("sanitizes the name and never carries a value", () => {
    const text = runEnvDropText("BAD\u001b[31mNAME\u0000", "must match the name shape");
    assert.ok(!/\p{Cc}/u.test(text));
    assert.ok(text.includes("must match the name shape"));
  });
});

describe("only the lead lane receives run variables", () => {
  const srcDir = new URL("../src/", import.meta.url).pathname;
  const walk = (dir: string): string[] =>
    fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(path.join(dir, e.name)) : e.name.endsWith(".ts") ? [path.join(dir, e.name)] : []));
  // The files that may mention the run env at all: the seam, the claim path, the lead-lane builders and executors.
  const ALLOWED_FILES = new Set([
    "client.ts", "executor.ts", "protocol.ts", "runner.ts", "sdk-env.ts", "sdk-executor.ts", "worker.ts",
    "run-env-claim.ts", "run-env-forms.ts", "run-env-inject.ts", "run-env-lane.ts", "run-env-names.ts", "run-env-prompt.ts", "run-env-seen.ts",
    "prompt.ts", "agents.ts", path.join("codex", "render.ts"),
    path.join("codex", "codex-executor.ts"),
  ]);

  // Scan-needle consumers (PRD 4416 M2 exact-value scan): they search publishes for the registered forms of the
  // secret values and never build an env. Allowed by name; the test below pins that none of them injects.
  // cross-check-runner.ts registers a cross-check child's run_env_redact forms (the lead's secret versions) for redaction only.
  const NEEDLE_CONSUMERS = ["cross-check-runner.ts", "frozen-held.ts", "recovery.ts", "run-env-scan.ts", "terminal-preservation-lane.ts"];

  // runEnvProbe (env-probe.ts) is the unrelated environment probe, so match the variable names only.
  const RUN_ENV_MENTION = /\brunEnv\b|run_env|RunEnv(Claim|Var|Injection|Redact)|ClaimRunEnv|run-env-/;

  it("no other lane or launcher mentions run variables", () => {
    const offenders = walk(srcDir)
      .map((f) => path.relative(srcDir, f))
      .filter((rel) => !ALLOWED_FILES.has(rel) && !NEEDLE_CONSUMERS.includes(rel))
      .filter((rel) => RUN_ENV_MENTION.test(fs.readFileSync(path.join(srcDir, rel), "utf8")));
    assert.deepEqual(offenders, []);
  });

  it("the scan-needle consumers never hand run variables to an env builder", () => {
    for (const rel of NEEDLE_CONSUMERS) {
      const text = fs.readFileSync(path.join(srcDir, rel), "utf8");
      assert.doesNotMatch(text, /\b(buildSdkEnv|buildCommandEnv|foldRunEnv|RunEnvInjection)\b|\brunEnv\s*[:,)]/, rel);
    }
  });

  it("the Codex app-server launch env and the chat, isolated and advice builders take no run variables", () => {
    for (const rel of ["codex/launcher.ts", "chat-executor.ts", "isolated-executor.ts", "claude-advice-harness.ts", "claude-cross-check.ts", "job-runner.ts"]) {
      assert.doesNotMatch(fs.readFileSync(path.join(srcDir, rel), "utf8"), RUN_ENV_MENTION, rel);
    }
    // And their SDK env is the one with no injection.
    const cred: SdkCredential = { kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" };
    assert.ok(!Object.hasOwn(buildSdkEnv(cred, "/h"), "NPM_TOKEN"));
  });
});

describe("registered forms of secret variables", () => {
  const redactor = (runEnv: ClaimRunEnvVar[], redact: ClaimRunEnvRedact[] = []) => makeTextRedactor(runEnvClaimForms(runEnv, redact).redaction);
  const SECRET = "s3cr3t-pa55word-xyz";

  it("hard-coded base64 alignments of a known value", () => {
    const forms = runEnvClaimForms([v("X", "abcdefgh", true)], []).redaction;
    assert.ok(forms.includes("abcdefgh"));
    for (const f of ["YWJjZGVmZ2", "FiY2RlZmdo", "hYmNkZWZna"]) assert.ok(forms.includes(f), f);
  });

  it("redacts a docker auth entry base64(user:password) at every alignment", () => {
    const redact = redactor([v("DOCKER_AUTH_CONFIG", SECRET, true)]);
    for (let n = 0; n < 6; n++) {
      const b64 = Buffer.from("p".repeat(n) + SECRET).toString("base64");
      const text = JSON.stringify({ auths: { "registry.example.test": { auth: b64 } } });
      const out = redact(text);
      assert.ok(text.includes(b64.slice(9, -9)));
      assert.ok(!out.includes(b64.slice(9, -9)), `alignment ${n}`);
      assert.ok(out.includes("***REDACTED***"));
    }
  });

  it("redacts an HTTP Basic header containing the secret", () => {
    const header = `Authorization: Basic ${Buffer.from(`admin:${SECRET}`).toString("base64")}`;
    const out = redactor([v("SVC_PASSWORD", SECRET, true)])(header);
    assert.notEqual(out, header);
    assert.ok(out.includes("***REDACTED***"));
  });

  it("redacts the JSON-escaped form of a value with quotes and backslashes", () => {
    const value = 'pa"ss\\word-12345';
    const text = JSON.stringify({ token: value });
    assert.ok(!text.includes(value));
    const out = redactor([v("SVC_PASSWORD", value, true)])(text);
    assert.ok(out.includes("***REDACTED***"));
    assert.ok(!out.includes("pa\\\"ss"));
  });

  it("does not register a plain value", () => {
    assert.deepEqual(runEnvClaimForms([v("PLAIN_SETTING", "plain-value-visible", false)], []).redaction, []);
    const text = "the value is plain-value-visible";
    assert.equal(redactor([v("PLAIN_SETTING", "plain-value-visible", false)])(text), text);
  });

  it("registers older secret versions from run_env_redact", () => {
    const old: ClaimRunEnvRedact = { var_id: UUID, scope: "repo", rev: 1, name: "SVC_PASSWORD", value: "retired-secret-value-77" };
    const out = redactor([], [old])("seen retired-secret-value-77 here");
    assert.ok(!out.includes("retired-secret-value-77"));
  });

  it("drops forms shorter than 8 characters and deduplicates", () => {
    for (const f of runEnvClaimForms([v("A_SECRET", "12345678", true), v("B_SECRET", "12345678", true)], []).redaction) assert.ok(f.length >= 8);
    const forms = runEnvClaimForms([v("A_SECRET", "12345678", true), v("B_SECRET", "12345678", true)], []).redaction;
    assert.equal(new Set(forms).size, forms.length);
  });
});

describe("the executors pass the run variables to the env builders", () => {
  // A deleted runEnv argument at the spawn site would leave the built env without the variable. The
  // executor sources are scanned for the call shape, and the builders are exercised with the same
  // injection object the executors build from RunContext.runEnv.
  const srcDir = new URL("../src/", import.meta.url).pathname;
  const lead = [v("DB_PASSWORD", "dummy-secret-run-value-000000", true), v("REGION")];

  it("buildSdkEnv and buildCommandEnv carry a lead-lane run's variables", () => {
    const sdk = buildSdkEnv({ kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" }, "/h", {}, undefined, undefined, undefined, undefined, { vars: lead });
    assert.equal(sdk.DB_PASSWORD, "dummy-secret-run-value-000000");
    assert.equal(sdk.REGION, "dummy-value-region");
    const cmd = buildCommandEnv("/private/tmp", {}, undefined, false, { vars: lead });
    assert.equal(cmd.DB_PASSWORD, "dummy-secret-run-value-000000");
  });

  it("the spawn sites of both executors pass ctx.runEnv and the precomputed set (call-site pin)", () => {
    const sdkSrc = fs.readFileSync(path.join(srcDir, "sdk-executor.ts"), "utf8");
    assert.match(sdkSrc, /buildSdkEnv\(credential,[^;]*ctx\.runEnv && runEnvSet\s*\?\s*\{ vars: ctx\.runEnv, set: runEnvSet/s);
    const codexSrc = fs.readFileSync(path.join(srcDir, "codex", "codex-executor.ts"), "utf8");
    assert.match(codexSrc, /buildCommandEnv\([\s\S]{0,900}?ctx\.runEnv && runEnvSet\s*\?\s*\{ vars: ctx\.runEnv, set: runEnvSet/s);
  });
});
