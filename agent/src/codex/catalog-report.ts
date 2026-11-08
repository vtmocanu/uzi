// Issue #3251: the per-claim reporter that records the Codex catalog's default reasoning effort for
// the claim's model (POST /api/worker/runs/{id}/codex-catalog-default).
//
// Best-effort and one-shot: at most one POST per claim, no retry, and every refusal or transport
// error ends in one bounded log line. It never throws into a turn, so a failed, slow or refused
// POST cannot change what the run does (the probe's pending-request residual is documented in
// codex-harness.ts runCatalogProbe). Modelled on LeadConfigReporter (lead-config-report.ts).

import type { LeadConfigPostOutcome } from "../client.js";
import type { Logger } from "../log.js";
import type { CodexCatalogDefaultBody } from "../protocol.js";
import type { CodexCatalogEntry } from "./catalog-probe.js";
import { CODEX_PROBE_EXPECTATION } from "./codex-runtime-probe.js";

/** The efforts the server stores (the Codex wire subset of the shared effort vocabulary). */
const STORED_EFFORTS: ReadonlySet<string> = new Set([
  "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra",
]);

const CATALOG_CONTEXT_RE = /^[0-9a-f]{64}$/;
const POST_TIMEOUT_MS = 5000;

export interface CodexCatalogReportClient {
  recordCodexCatalogDefault(runId: string, body: CodexCatalogDefaultBody, timeoutMs?: number): Promise<LeadConfigPostOutcome>;
}

export interface CodexCatalogReporterOptions {
  client: CodexCatalogReportClient;
  runId: string;
  claimGeneration: number;
  /** The claim's `codex_catalog_context`, echoed verbatim. */
  catalogContext: string;
  logger: Logger;
  /** Defaults to the pinned runtime's version. */
  runtimeVersion?: string;
}

/** Whether a claim should get a reporter: a well-formed context and a fenced claim generation. */
export function codexCatalogReportActive(context: string | undefined, claimGeneration: number | undefined): context is string {
  return context !== undefined && CATALOG_CONTEXT_RE.test(context) &&
    Number.isSafeInteger(claimGeneration) && (claimGeneration ?? 0) > 0;
}

/** Exactly one per claim generation, constructed by the runner and carried as
 *  `RunContext.codexCatalogReport`. */
export class CodexCatalogReporter {
  private readonly client: CodexCatalogReportClient;
  private readonly runId: string;
  private readonly claimGeneration: number;
  private readonly catalogContext: string;
  private readonly log: Logger;
  private readonly runtimeVersion: string;
  private attempted = false;

  constructor(opts: CodexCatalogReporterOptions) {
    this.client = opts.client;
    this.runId = opts.runId;
    this.claimGeneration = opts.claimGeneration;
    this.catalogContext = opts.catalogContext;
    this.log = opts.logger;
    this.runtimeVersion = opts.runtimeVersion ?? CODEX_PROBE_EXPECTATION.version;
  }

  /** Report the entry's default effort for `model`. The first entry whose effort maps into the
   *  stored set spends the claim's one attempt; an entry that does not map (absent, `persistent`,
   *  custom) spends nothing. Fire-and-forget: the post continues in the background. Never throws. */
  report(entry: CodexCatalogEntry | undefined, model: string): void {
    try {
      if (this.attempted) return;
      const effort = entry?.defaultEffort;
      if (effort === undefined || !STORED_EFFORTS.has(effort)) return;
      this.attempted = true;
      void this.post({
        claim_generation: this.claimGeneration,
        catalog_context: this.catalogContext,
        model,
        runtime_version: this.runtimeVersion,
        default_effort: effort,
      });
    } catch (err) {
      this.log.warn("codex catalog default: report failed", { run_id: this.runId, error: errText(err) });
    }
  }

  private async post(body: CodexCatalogDefaultBody): Promise<void> {
    try {
      const outcome = await this.client.recordCodexCatalogDefault(this.runId, body, POST_TIMEOUT_MS);
      if (outcome.kind === "http") {
        this.log.warn("codex catalog default: report not accepted", { run_id: this.runId, status: outcome.status });
      } else if (outcome.kind === "transport") {
        this.log.warn("codex catalog default: report not delivered", { run_id: this.runId });
      }
    } catch (err) {
      this.log.warn("codex catalog default: report failed", { run_id: this.runId, error: errText(err) });
    }
  }
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
