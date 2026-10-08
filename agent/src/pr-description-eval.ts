// Opt-in offline pipeline proof. Model output and helper fields remain internal.
import fs from "node:fs/promises";
import path from "node:path";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { WorkerClient, type ClientOptions } from "./client.js";
import { WORKER_API_PREFIX, type RawPrDescriptionFields } from "./protocol.js";
import { assembleGeneratedFields } from "./pr-description-publisher.js";
import { buildDeliveryPrompt, DELIVERY_SYSTEM_PROMPT, parseDeliverySummary } from "./summary-runner.js";
import { capBody, renderBody, renderRegion, type RegionInput } from "./pr-description.js";
import type { DeliveryContext } from "./pr-description-context.js";
import type { PrSummaryClaim } from "./signals.js";
import type { Logger } from "./log.js";

export const evalLog: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return evalLog; },
};
export interface EditorFixture {
  id: string;
  context: DeliveryContext;
  code: { unavailable: boolean; added: number; deleted: number };
  lead: PrSummaryClaim;
  region: RegionInput;
  completion: string;
  bodyPrefix: string;
}
export interface EvalOptions {
  harness: "claude" | "codex";
  model: string;
  promptMode: "baseline" | "revised";
}
export interface PipelineResult {
  emitted: boolean;
  parsed: boolean;
  would_publish: boolean;
  error: string | null;
}
const ids = ["fallbackchain", "components", "trivial", "truncation", "hostile"] as const;
export async function loadEditorFixtures(checkout: string): Promise<EditorFixture[]> {
  return Promise.all(ids.map(async (id) => {
    const fixture = JSON.parse(
      await fs.readFile(path.join(checkout, "fixtures/pr-description-editor", id + ".json"), "utf8"),
    ) as EditorFixture;
    if (fixture.id !== id) throw new Error("fixture_integrity");
    return fixture;
  }));
}
export async function editorRequest(checkout: string, fixture: EditorFixture, opts: EvalOptions) {
  const dir = path.join(checkout, "fixtures/pr-description-editor");
  const baseline = await fs.readFile(path.join(dir, "baseline-system-prompt.txt"), "utf8");
  const hash = (await fs.readFile(path.join(dir, "baseline.sha256"), "utf8")).trim();
  if (hash !== "2891a46208957ff7f2f706de2c6d6730e697d99775b0aed52ebffd4a75d7bdfc" ||
      createHash("sha256").update(baseline).digest("hex") !== hash) throw new Error("baseline_integrity");
  return {
    systemPrompt: opts.promptMode === "baseline" ? baseline : DELIVERY_SYSTEM_PROMPT,
    prompt: buildDeliveryPrompt(fixture.context),
    model: opts.model,
  };
}

/** One bounded helper process, no retries. Failure blocks this fixture, not sibling fixtures.
 * Replacement environment contains tool/cache coordinates only, never model or worker auth.
 */
export async function sanitizeEditorFields(checkout: string, fields: RawPrDescriptionFields) {
  const input = JSON.stringify({ fields });
  if (Buffer.byteLength(input) > 4 * 1024 * 1024) throw new Error("helper_failed");
  const scratch = path.join(checkout, ".uzi/scratch");
  const env: NodeJS.ProcessEnv = { HOME: scratch, GOFLAGS: "-buildvcs=false" };
  for (const key of ["PATH", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE"] as const) {
    if (process.env[key]) env[key] = process.env[key];
  }
  const stdout = await new Promise<string>((resolve, reject) => {
    let inputError = false;
    const child = execFile("go", ["run", "./cmd/pr-description-eval-sanitize"], {
      cwd: path.join(checkout, "api"), env, timeout: 120_000, maxBuffer: 128 * 1024,
    }, (error, output) => error || inputError ? reject(new Error("helper_failed")) : resolve(output));
    // Observe write failure internally, but settle only after execFile has reaped the child.
    child.stdin?.on("error", () => { inputError = true; });
    child.stdin?.end(input);
  });
  try {
    const value: unknown = JSON.parse(stdout);
    if (!value || typeof value !== "object" || !("fields" in value) ||
        !("diagram_rejected" in value) || typeof value.diagram_rejected !== "boolean") throw new Error();
    return { fields: value.fields, diagram_rejected: value.diagram_rejected };
  } catch { throw new Error("helper_failed"); }
}
const origin = "http://pr-description-eval.invalid";
const runId = "offline-fixture";
const marker = "offline-eval-no-authority";

/** Only the exact local stage operation is accepted. The normal client decoder mints the brand. */
export function editorStageTransport(fields: unknown): NonNullable<ClientOptions["fetch"]> {
  return async (url, init) => {
    if (url !== origin + WORKER_API_PREFIX + "/runs/" + runId + "/pr-description/stage" ||
        init?.method !== "POST" || new Headers(init.headers).get("Authorization") !== "Bearer " + marker) {
      throw new Error("transport_refused");
    }
    const req = JSON.parse(String(init.body));
    if (req.source !== "generated" || req.claim_generation !== 1 ||
        req.base_sha !== "b".repeat(40) || req.head_sha !== "a".repeat(40) ||
        req.target_branch !== "main" || req.size !== null || !req.fields) throw new Error("transport_refused");
    return new Response(JSON.stringify({ version: {
      id: "offline-version", run_id: runId, claim_generation: 1, mr_iid: null,
      fields, size: null, base_sha: req.base_sha, head_sha: req.head_sha,
      target_branch: req.target_branch, source: "generated", rendered_region_sha256: null,
      region_has_diagram: null, state: "pending", created_at: "2026-10-08T00:00:00Z", published_at: null,
    } }), { status: 200 });
  };
}
export async function evaluateEditorResponse(checkout: string, fixture: EditorFixture, response: string): Promise<PipelineResult> {
  let emitted = false;
  let summary;
  try { summary = parseDeliverySummary(response, (_raw, present) => { emitted = present; }); }
  catch { return { emitted, parsed: false, would_publish: false, error: "invalid_summary" }; }
  const parsed = !!summary?.diagram;
  if (!summary) return { emitted, parsed, would_publish: false, error: "invalid_summary" };
  const zeroCode = !fixture.code.unavailable && fixture.code.added + fixture.code.deleted === 0;
  const fields = assembleGeneratedFields(summary, fixture.lead, zeroCode);
  try {
    const sanitized = await sanitizeEditorFields(checkout, fields);
    const client = new WorkerClient(origin, marker, "offline-eval", evalLog, { fetch: editorStageTransport(sanitized.fields) });
    const { version } = await client.stagePrDescription(runId, {
      claim_generation: 1, source: "generated", fields, size: null,
      base_sha: "b".repeat(40), head_sha: "a".repeat(40), target_branch: "main",
    });
    const input = { ...fixture.region, source: version.source };
    const region = renderRegion(input, version.fields);
    const less = renderRegion(input, version.fields, true).text;
    const deterministic = renderRegion(fixture.region).text;
    const selected = capBody((r) => fixture.bodyPrefix + renderBody(r, fixture.completion), region.text, deterministic, less);
    const would_publish = !!version.fields.diagram && region.withFields && !region.diagramRemoval &&
      !selected.capped && !selected.diagramless && selected.body !== undefined;
    const error = zeroCode && parsed ? "zero_code" : sanitized.diagram_rejected ? "api_rejected" :
      region.diagramRemoval?.reason ?? (selected.removal ? "body_cap" :
        emitted && !parsed ? "parser_rejected" : !emitted ? "editor_omission" : null);
    return { emitted, parsed, would_publish, error };
  } catch { return { emitted, parsed, would_publish: false, error: "pipeline_failed" }; }
}
