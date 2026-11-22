import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it } from "node:test";
import { parseSync } from "oxc-parser";

// M2 textual architecture census, re-enumerated at e7ffe477e3cfbcb6ec3f84cb5546f5c1ceb45983.
// Pins are reviewed source lines, NOT generated at test time. No per-file budgets.
// Parser use excludes comments/string lookalikes/types and handles optional chains. This is
// Literal computed members and direct provider destructuring are included. Dynamic
// reflection, arbitrary aliases, dependencies and sources outside src are outside this
// textual census; this is not arbitrary JS dataflow analysis.
// Native opts.provider sites are explicitly distinguished from claim binding carriers.
// Guard pins establish reviewed holds, not supported Codex-provider execution or dominance
// under arbitrary code movement. Behavioral lane tests remain necessary.
const SRC = fileURLToPath(new URL("../src/", import.meta.url));
type Site = { file: string; line: string; count: number; category: string; rationale: string };
const SITES: Site[] = [
  {"file": "chat-executor.ts", "line": "if (!oauthToken && !ctx.provider) throw new Error(REASON_NO_TOKEN);", "count": 1, "category": "engine-handling", "rationale": "Claude credential accessor refuses non-Claude before model resolution; accounting follows that accessor."},
  {"file": "chat-executor.ts", "line": "const credential: SdkCredential = ctx.provider ? sdkCredentialFromBinding(ctx.provider) : { kind: \"oauth\", token: oauthToken! };", "count": 1, "category": "engine-handling", "rationale": "Claude credential accessor refuses non-Claude before model resolution; accounting follows that accessor."},
  {"file": "chat-executor.ts", "line": "model: ctx.provider ? resolveProviderModel(ctx.model, ctx.provider.model_map) : ctx.model,", "count": 1, "category": "engine-handling", "rationale": "Claude credential accessor refuses non-Claude before model resolution; accounting follows that accessor."},
  {"file": "chat-executor.ts", "line": "for (const raw of mapSdkMessage(msg)) ctx.emit(ctx.provider ? stripProviderCost(raw) : raw);", "count": 1, "category": "engine-handling", "rationale": "Claude credential accessor refuses non-Claude before model resolution; accounting follows that accessor."},
  {"file": "chat-executor.ts", "line": "if (ctx.provider && typeof status === \"number\" && isGatewayLimitStatus(status)) {", "count": 1, "category": "engine-handling", "rationale": "Claude credential accessor refuses non-Claude before model resolution; accounting follows that accessor."},
  {"file": "chat-runner.ts", "line": "const providerKeys = claim.secrets.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : [];", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "chat-runner.ts", "line": "...(claim.secrets.provider ? { provider: claim.secrets.provider } : {}),", "count": 1, "category": "engine-handling", "rationale": "Forwarded ChatContext reaches chat-executor sdkCredentialFromBinding refusal."},
  {"file": "claude-advice-harness.ts", "line": "const providerModel = credential.kind === \"provider\" ? resolveProviderModel(request.model, credential.modelMap) : undefined;", "count": 1, "category": "engine-handling", "rationale": "SdkCredential provider variant is Claude SDK configuration; resolveProviderModel is a Claude map resolver."},
  {"file": "claude-cross-check.ts", "line": "if (claim.secrets.provider) throw new Error(PROVIDER_LANE_UNSUPPORTED);", "count": 1, "category": "lane-refusal", "rationale": "Provider presence refuses this unsupported lane regardless of engine."},
  {"file": "client.ts", "line": "claim.secrets.provider = provider;", "count": 2, "category": "admission-write", "rationale": "Decoded binding written only after requireProviderCapability engine hold."},
  {"file": "codex/codex-advice-harness.ts", "line": "this.provider = opts.provider;", "count": 1, "category": "native-config", "rationale": "opts is native CodexProviderConfig, not ClaimProviderBinding; no claim-provider execution permission."},
  {"file": "codex/codex-executor.ts", "line": "? this.opts.provider", "count": 1, "category": "native-config", "rationale": "opts is native CodexProviderConfig, not ClaimProviderBinding; no claim-provider execution permission."},
  {"file": "codex/codex-executor.ts", "line": "...this.opts.provider,", "count": 1, "category": "native-config", "rationale": "opts is native CodexProviderConfig, not ClaimProviderBinding; no claim-provider execution permission."},
  {"file": "codex/codex-executor.ts", "line": "providerDefaultModel: this.opts.provider.model,", "count": 1, "category": "native-config", "rationale": "opts is native CodexProviderConfig, not ClaimProviderBinding; no claim-provider execution permission."},
  {"file": "codex/codex-harness.ts", "line": "this.provider = opts.provider;", "count": 1, "category": "native-config", "rationale": "opts is native CodexProviderConfig, not ClaimProviderBinding; no claim-provider execution permission."},
  {"file": "cross-check-runner.ts", "line": "const secrets = [claim.secrets.forge_pat, claim.secrets.anthropic_oauth_token, ...(claim.secrets.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : []), claim.secrets.codex?.capability];", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "cross-check-runner.ts", "line": "if (claim.secrets.provider) throw new Error(PROVIDER_LANE_UNSUPPORTED);", "count": 1, "category": "lane-refusal", "rationale": "Provider presence refuses this unsupported lane regardless of engine."},
  {"file": "diagram-diagnostic.ts", "line": "const secrets = [claim.secrets.anthropic_oauth_token, ...(claim.secrets.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : []), ...extraSecrets];", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "isolated-runner.ts", "line": "if (secrets?.provider) return PROVIDER_LANE_UNSUPPORTED;", "count": 1, "category": "lane-refusal", "rationale": "Provider presence refuses this unsupported lane regardless of engine."},
  {"file": "isolated-runner.ts", "line": "const runSecrets = [token, ...(claim.secrets?.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : []), grant?.credential ?? \"\"].filter((s) => s.length > 0);", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "job-runner.ts", "line": "const providerKeys = claim.secrets?.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : [];", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "job-runner.ts", "line": "if (claim.secrets?.provider) {", "count": 1, "category": "lane-refusal", "rationale": "Provider presence refuses this unsupported lane regardless of engine."},
  {"file": "judge-runner.ts", "line": "} else if (claim.secrets?.provider) {", "count": 1, "category": "lane-refusal", "rationale": "Provider presence refuses this unsupported lane regardless of engine."},
  {"file": "judge-runner.ts", "line": "const providerKeys = providerKeyForms(claim.secrets.provider.credential);", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "main.ts", "line": "if (addr.provider && (addr.provider.engine !== \"claude\" || codex !== undefined)) {", "count": 1, "category": "engine-handling", "rationale": "buildRunExecutor explicitly refuses non-Claude/mixed credentials before native or stub selection."},
  {"file": "model-pass.ts", "line": "if (opts.provider && (opts.provider.engine !== \"claude\" || opts.codex !== undefined)) throw new ProviderLaneUnsupportedError();", "count": 1, "category": "engine-handling", "rationale": "Early explicit engine/mixed-credential refusal precedes scope and SDK credential construction."},
  {"file": "model-pass.ts", "line": "if (!token && !opts.provider) {", "count": 1, "category": "engine-handling", "rationale": "Early explicit engine/mixed-credential refusal precedes scope and SDK credential construction."},
  {"file": "model-pass.ts", "line": "const scope = opts.provider ? providerScope(opts.provider.credential, opts.log, opts.label) : undefined;", "count": 1, "category": "engine-handling", "rationale": "Early explicit engine/mixed-credential refusal precedes scope and SDK credential construction."},
  {"file": "model-pass.ts", "line": "credential: opts.provider ? sdkCredentialFromBinding(opts.provider) : { kind: \"oauth\", token: token! },", "count": 1, "category": "engine-handling", "rationale": "Early explicit engine/mixed-credential refusal precedes scope and SDK credential construction."},
  {"file": "provider-binding.ts", "line": "const raw = secrets.provider;", "count": 1, "category": "engine-handling", "rationale": "Decoder validates wire engine/protocol; sdkCredentialFromBinding independently refuses non-Claude."},
  {"file": "provider-binding.ts", "line": "if (def.model !== undefined && def.model !== \"\" && def.model !== \"inherit\") resolveProviderModel(def.model, modelMap);", "count": 1, "category": "engine-handling", "rationale": "Decoder validates wire engine/protocol; sdkCredentialFromBinding independently refuses non-Claude."},
  {"file": "review-runner.ts", "line": "const credential = claim.secrets?.provider?.credential;", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "review-runner.ts", "line": "const providerKeys = claim.secrets?.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : [];", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "review-runner.ts", "line": "if (claim.secrets.provider && (claim.secrets.provider.engine !== \"claude\" || claim.secrets.codex !== undefined)) {", "count": 1, "category": "engine-handling", "rationale": "ReviewRunner.execute refuses non-Claude/mixed claims before executeClaim; forwarded review reaches guarded model-pass."},
  {"file": "review-runner.ts", "line": "if (selection.kind === \"claude\" && !claim.secrets?.provider && !hasProviderCostForEveryModel(snapshot)) {", "count": 1, "category": "engine-handling", "rationale": "ReviewRunner.execute refuses non-Claude/mixed claims before executeClaim; forwarded review reaches guarded model-pass."},
  {"file": "review-runner.ts", "line": "if (!token && !claim.secrets?.provider) {", "count": 1, "category": "engine-handling", "rationale": "ReviewRunner.execute refuses non-Claude/mixed claims before executeClaim; forwarded review reaches guarded model-pass."},
  {"file": "review-runner.ts", "line": "review = await this.review(claim, diff, token ?? \"\", onUsage, claim.secrets?.provider);", "count": 1, "category": "engine-handling", "rationale": "ReviewRunner.execute refuses non-Claude/mixed claims before executeClaim; forwarded review reaches guarded model-pass."},
  {"file": "review-runner.ts", "line": "const providerCredential = claim.secrets?.provider?.credential;", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "runner.ts", "line": "...(addr?.provider ? providerKeyForms(addr.provider.credential) : []),", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "runner.ts", "line": "const providerForms = claim.secrets.provider ? providerKeyForms(claim.secrets.provider.credential) : [];", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "runner.ts", "line": "claim.secrets.provider ? { provider: claim.secrets.provider } : undefined);", "count": 1, "category": "engine-handling", "rationale": "Factory forwarding reaches buildRunExecutor hold; Now eligibility explicitly requires Claude and passes reach guarded model-pass."},
  {"file": "runner.ts", "line": "...(claim.secrets.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : []),", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "runner.ts", "line": "providerKeyForms: claim.secrets.provider?.credential ? providerKeyForms(claim.secrets.provider.credential) : [],", "count": 1, "category": "neutral-secrecy", "rationale": "Engine-independent redaction/upload key material, including refused keys; no engine filter."},
  {"file": "runner.ts", "line": "claude: !claim.secrets.codex && (!!claim.secrets.anthropic_oauth_token || claim.secrets.provider?.engine === \"claude\"),", "count": 1, "category": "engine-handling", "rationale": "Factory forwarding reaches buildRunExecutor hold; Now eligibility explicitly requires Claude and passes reach guarded model-pass."},
  {"file": "runner.ts", "line": "...(claim.secrets.provider ? { provider: claim.secrets.provider } : {}),", "count": 2, "category": "engine-handling", "rationale": "Factory forwarding reaches buildRunExecutor hold; Now eligibility explicitly requires Claude and passes reach guarded model-pass."},
  {"file": "runner.ts", "line": "const built = this.makeClaimExecutor(flight.runId, this.claimWithCodexAuthority(claim, flight).secrets.codex, { generation, ...(claim.secrets.provider ? { provider: claim.secrets.provider } : {}) });", "count": 1, "category": "engine-handling", "rationale": "Factory forwarding reaches buildRunExecutor hold; Now eligibility explicitly requires Claude and passes reach guarded model-pass."},
  {"file": "runner.ts", "line": "flight.executor = publicationOnlyExecutor(claim, this.makeClaimExecutor(claim.run_id, this.claimWithCodexAuthority(claim, flight).secrets.codex, claim.secrets.provider ? { provider: claim.secrets.provider } : undefined).executor);", "count": 1, "category": "engine-handling", "rationale": "Factory forwarding reaches buildRunExecutor hold; Now eligibility explicitly requires Claude and passes reach guarded model-pass."},
  {"file": "sdk-executor.ts", "line": "if (!isRunSummaryKind(ctx.kind) || (!token && !ctx.provider) || !this.client || !prdInputP ||", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "token, ...(ctx.provider ? { provider: ctx.provider } : {}), model: ctx.summaryModel ?? \"\", issueTitle: ctx.issueTitle,", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "if (!oauthToken && !ctx.provider) {", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "const credential: SdkCredential = ctx.provider ? sdkCredentialFromBinding(ctx.provider) : { kind: \"oauth\", token: oauthToken! };", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "const spawnModel = ctx.provider ? resolveProviderModel(leadModel, ctx.provider.model_map) : leadModel;", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "if (ctx.provider) assertAgentModelsMapped(planAgents, ctx.provider.model_map);", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "(!!oauthToken || !!ctx.provider) &&", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "...(ctx.provider ? { provider: ctx.provider } : {}),", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "if (ctx.provider) assertAgentModelsMapped(selectedSubagents, ctx.provider.model_map);", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "if (ctx.provider) throw new Error(REASON_PROVIDER_SWITCH_REFUSED);", "count": 2, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "const em = ctx.provider ? stripProviderCost(raw) : raw;", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "sdk-executor.ts", "line": "if (ctx.provider && isGatewayLimitStatus(terminal.apiErrorStatus)) {", "count": 1, "category": "engine-handling", "rationale": "Claude setup uses sdkCredentialFromBinding; summaries reach guarded model-pass; switching is refused."},
  {"file": "summary-runner.ts", "line": "if (claim.secrets.provider && (claim.secrets.provider.engine !== \"claude\" || claim.secrets.codex !== undefined)) {", "count": 1, "category": "engine-handling", "rationale": "deliveryHarness and runModel explicitly refuse non-Claude or mixed native credentials before forwarding."},
  {"file": "summary-runner.ts", "line": "if (claim.secrets.provider) return { provider: claim.secrets.provider, model: isValidModel(model) ? model : DEFAULT_DELIVERY_SUMMARY_MODEL };", "count": 1, "category": "engine-handling", "rationale": "deliveryHarness and runModel explicitly refuse non-Claude or mixed native credentials before forwarding."},
  {"file": "summary-runner.ts", "line": "if (input.provider && (input.provider.engine !== \"claude\" || input.claim?.secrets.codex !== undefined)) {", "count": 1, "category": "engine-handling", "rationale": "deliveryHarness and runModel explicitly refuse non-Claude or mixed native credentials before forwarding."},
  {"file": "summary-runner.ts", "line": "harness = { token: input.token, ...(input.provider ? { provider: input.provider } : {}), model: input.model };", "count": 1, "category": "engine-handling", "rationale": "deliveryHarness and runModel explicitly refuse non-Claude or mixed native credentials before forwarding."},
];

/** File-relative recursive walk; reject symlinks instead of following them outside src. */
function readSources(dir: string, rel = ""): Map<string, string> {
  assert.ok(!fs.lstatSync(path.join(dir, rel)).isSymbolicLink(), "source directory symlink refused");
  const out = new Map<string, string>();
  for (const entry of fs.readdirSync(path.join(dir, rel), { withFileTypes: true })) {
    const next = path.join(rel, entry.name);
    assert.ok(!entry.isSymbolicLink(), `source symlink refused: ${next}`);
    if (entry.isDirectory()) for (const [file, source] of readSources(dir, next)) out.set(file, source);
    else if (entry.isFile() && entry.name.endsWith(".ts")) out.set(next.split(path.sep).join("/"), fs.readFileSync(path.join(dir, next), "utf8"));
  }
  return out;
}

// One site per physical line, even if it contains multiple references. Multiplicity counts
// copies of that exact line, as in credential-consumer-census.test.ts.
type Evidence = { lines: string[]; starts: Set<number> };
const evidenceCache = new Map<string, Evidence>();
function evidence(source: string): Evidence {
  const cached = evidenceCache.get(source);
  if (cached) return cached;
  const starts = new Set<number>();
  const parsed = parseSync("census.ts", source, { lang: "ts" });
  assert.equal(parsed.errors.length, 0, "census input must parse as TypeScript");
  const hits = new Set<number>();
  const record = (value: unknown): Record<string, unknown> =>
    value !== null && typeof value === "object" ? value as Record<string, unknown> : {};
  function visit(value: unknown): void {
    if (Array.isArray(value)) { for (const child of value) visit(child); return; }
    const node = record(value);
    const object = record(node.object);
    const memberName = (member: Record<string, unknown>): unknown =>
      member.computed === true ? record(member.property).value : record(member.property).name;
    const receiver = object.type === "Identifier" ? object.name : memberName(object);
    const carrier = node.type === "MemberExpression" && memberName(node) === "provider" &&
      ["secrets", "opts", "ctx", "addr", "input"].includes(String(receiver));
    const callee = record(node.callee);
    const accessor = node.type === "CallExpression" && callee.type === "Identifier" &&
      ["sdkCredentialFromBinding", "resolveProviderModel", "assertAgentModelsMapped"].includes(String(callee.name));
    const binding = record(node.id);
    const init = record(node.init);
    const destructured = node.type === "VariableDeclarator" && binding.type === "ObjectPattern" &&
      init.type === "MemberExpression" && memberName(init) === "secrets" &&
      Array.isArray(binding.properties) && binding.properties.some(value => {
        const prop = record(value);
        const key = record(prop.key);
        return (prop.computed === true ? key.value : key.name ?? key.value) === "provider";
      });
    if (typeof node.start === "number") {
      if (carrier || accessor || destructured) hits.add(source.lastIndexOf("\n", node.start - 1) + 1);
      // Explicit syntax families exclude Literal, TemplateElement and type-node starts.
      // A Set dedupes admission CallExpression / ExpressionStatement shared offsets.
      if (String(node.type).endsWith("Statement") || node.type === "VariableDeclaration" ||
          node.type === "CallExpression" || node.type === "SpreadElement" || node.type === "Property") {
        starts.add(node.start);
      }
    }
    for (const child of Object.values(node)) if (child !== null && typeof child === "object") visit(child);
  }
  visit(parsed.program);
  const lines = [...hits].sort((a, b) => a - b).map(start => {
    const end = source.indexOf("\n", start);
    return source.slice(start, end < 0 ? undefined : end).trim();
  });
  const result = { lines, starts };
  evidenceCache.set(source, result); // Exact text key; changed source parses afresh.
  return result;
}
function readLines(source: string): string[] { return evidence(source).lines; }

function holdCount(source: string, block: string): number {
  const parts = block.split("\n");
  let count = 0;
  for (const start of evidence(source).starts) {
    if (!source.startsWith(parts[0]!, start)) continue;
    // Normalize just the required lines; partial-call pins remain source prefixes.
    const prefix = source.slice(start).split("\n", parts.length)
      .map(line => line.trim()).join("\n");
    if (prefix.startsWith(block)) count++;
  }
  return count;
}

// Separately pin the mechanisms on which carrier classifications rely, including guard
// bodies and admission calls (a condition alone would not establish refusal).
const HOLDS: Record<string, string[]> = {
  "client.ts": ['this.requireProviderCapability(provider.engine);', 'if (engine === "codex") throw new ProviderLaneUnsupportedError();'],
  "provider-binding.ts": ['if (b.engine !== "claude") throw new ProviderLaneUnsupportedError();', 'if (engine !== "claude" && engine !== "codex") throw new ProviderBindingError("unknown engine");', 'if (protocol !== (engine === "claude" ? "anthropic_messages" : "openai_responses")) throw new ProviderBindingError("unknown protocol");'],
  "main.ts": ['return { executor: new FailClosedExecutor("provider_lane_unsupported") };'],
  "summary-runner.ts": ['throw new ProviderLaneUnsupportedError();'],
  "review-runner.ts": ['await safeReportFailed(this.client, this.log, "review", claim.run_id,\nPROVIDER_LANE_UNSUPPORTED, undefined, claim.claim_generation, this.terminalDeps);\nreturn;'],
  "job-runner.ts": ['await this.fail(runId, generation, PROVIDER_LANE_UNSUPPORTED);\nreturn;'],
  "judge-runner.ts": ['await safeReportFailed(this.client, this.log, "judge", judgeRunId, PROVIDER_LANE_UNSUPPORTED, undefined,'],
  "now-summary.ts": ['...this.deps.pass,'],
  "claude-advice-harness.ts": ['const tracker = new AdviceUsageTracker(credential.kind === "provider");'],
  "sdk-env.ts": ['ANTHROPIC_BASE_URL: credential.baseUrl,', 'ANTHROPIC_API_KEY: credential.authStyle === "x_api_key" ? credential.key : undefined,', 'ANTHROPIC_AUTH_TOKEN: credential.authStyle === "bearer" ? credential.key : undefined,'], 
};

function offenders(sources: Map<string, string>): string[] {
  const errors: string[] = [];
  // Bound storage AND assertion output, while still scanning every file and pin.
  const report = (message: string): void => { if (errors.length < 5) errors.push(message); };
  const remaining = new Map(SITES.map(s => [`${s.file}\n${s.line}`, s.count]));
  for (const [file, source] of sources) for (const line of readLines(source)) {
    const key = `${file}\n${line}`;
    const left = remaining.get(key);
    if (left === undefined || left <= 0) report(`${file}: new/duplicate read: ${line.slice(0, 240)}`);
    else remaining.set(key, left - 1);
  }
  for (const [key, count] of remaining) if (count !== 0) report(`stale pin (${count} missing): ${key.slice(0, 240)}`);
  for (const [file, blocks] of Object.entries(HOLDS)) {
    const text = sources.get(file) ?? "";
    for (const block of blocks) {
      const expected = file === "client.ts" && block.startsWith("this.require") ? 2 : file === "summary-runner.ts" ? 2 : 1;
      if (holdCount(text, block) !== expected) report(`${file}: missing/duplicated hold: ${block.slice(0, 200)}`);
    }
  }
  return errors;
}

describe("M2 provider consumer architectural census (textual scope)", () => {
  const sources = readSources(SRC);
  it("every named read/write has an exact live pin and handling rationale; holds remain", () => {
    assert.deepEqual(offenders(sources), []);
    assert.equal(new Set(SITES.map(s => `${s.file}\n${s.line}`)).size, SITES.length);
    for (const site of SITES) assert.ok(site.category && site.rationale);
  });
  it("self-test: new nested reads and duplicate allowed reads fail", () => {
    assert.ok(offenders(new Map(sources).set("nested/new.ts", "void claim.secrets?.provider;" )).length);
    const site = SITES.find(s => s.file === "chat-runner.ts")!;
    assert.ok(offenders(new Map(sources).set(site.file, `${sources.get(site.file)}\n${site.line}`)).length);
  });
  it("self-test: deleted guard body and presence-means-Claude assumption fail", () => {
    const file = "model-pass.ts";
    const original = sources.get(file)!;
    const guard = SITES.find(s => s.file === file && s.line.includes('engine !== "claude"'))!.line;
    assert.ok(original.includes(guard));
    assert.ok(offenders(new Map(sources).set(file, original.replace(guard, ""))).length);
    assert.ok(offenders(new Map(sources).set(file, original.replace('opts.provider.engine !== "claude"', "false"))).length);
    const summary = sources.get("summary-runner.ts")!;
    assert.ok(offenders(new Map(sources).set("summary-runner.ts", summary.replace("throw new ProviderLaneUnsupportedError();", "return null;"))).length);
    assert.ok(offenders(new Map(sources).set("provider-binding.ts", sources.get("provider-binding.ts")!.replace('if (b.engine !== "claude") throw new ProviderLaneUnsupportedError();', ""))).length);
  });
  it("self-test: commented summary refusals cannot satisfy holds", () => {
    const file = "summary-runner.ts";
    const original = sources.get(file)!;
    const guard = "throw new ProviderLaneUnsupportedError();";
    assert.equal(original.split(guard).length - 1, 2);
    assert.ok(offenders(new Map(sources).set(file, original.replaceAll(guard, `// ${guard}`))).length);
  });
  it("self-test: comments, strings and template text cannot replace guard evidence", () => {
    const file = "summary-runner.ts";
    const original = sources.get(file)!;
    const guard = "throw new ProviderLaneUnsupportedError();";
    for (const replacement of [`/* ${guard} */`, JSON.stringify(guard) + ";", "`" + guard + "`;"]) {
      const changed = original.replaceAll(guard, replacement);
      assert.equal(holdCount(changed, guard), 0);
      assert.ok(offenders(new Map(sources).set(file, changed)).length);
    }
    assert.equal(holdCount(guard, guard), 1);
    const call = "this.requireProviderCapability(provider.engine);";
    assert.equal(holdCount(call, call), 1, "shared AST starts count once");
    assert.equal(holdCount("`" + call + "`;", call), 0);
  });
  it("self-test: exact-source cache reuses facts but never changed-source evidence", () => {
    const before = "void ctx.provider;";
    const after = "void ctx.disabled;";
    assert.equal(before.length, after.length);
    const facts = evidence(before);
    assert.equal(evidence(before), facts);
    assert.deepEqual(readLines(after), []);
    assert.notEqual(evidence(after), facts);
    assert.deepEqual(readLines(before), [before]);
    const guard = "throw new ProviderLaneUnsupportedError();";
    assert.equal(holdCount(guard, guard), 1);
    assert.equal(holdCount("// " + guard, guard), 0);
    assert.equal(holdCount(guard, guard), 1);
  });
  it("self-test: literal computed and destructured provider reads require new pins", () => {
    for (const source of [
      'void claim.secrets["provider"];', "void ctx['provider'];",
      "const {provider} = claim.secrets;", "const {provider: renamed} = claim.secrets;",
      'const {["provider"]: renamed} = claim["secrets"];',
    ]) {
      assert.deepEqual(readLines(source), [source]);
      assert.ok(offenders(new Map(sources).set("nested/new.ts", source)).length);
    }
  });
  it("self-test: removal/stale pins fail, including a whole missing source", () => {
    const site = SITES.find(s => s.file === "chat-runner.ts")!;
    assert.ok(offenders(new Map(sources).set(site.file, sources.get(site.file)!.replace(site.line, ""))).length);
    const missing = new Map(sources);
    missing.delete(site.file);
    assert.ok(offenders(missing).length);
  });
  it("classifies syntax rather than comment, string, or type lookalikes", () => {
    assert.deepEqual(readLines('// claim.secrets.provider\nconst text = "opts.provider";\ntype T = { provider: string };'), []);
    assert.deepEqual(readLines('void ctx?.provider;\nvoid input.provider;'), ['void ctx?.provider;', 'void input.provider;']);
  });
});
