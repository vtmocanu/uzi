import { selectCodexBinding } from "./codex/select.js";
import type { Logger } from "./log.js";
import { isValidModel } from "./models.js";
import { makeTextRedactor } from "./redact.js";
import { CODEX_PR_DESCRIPTION_MODEL } from "./codex/pr-description-model.js";

interface Claim {
  run_id: string;
  summary_model?: string | null;
  secrets: { anthropic_oauth_token?: string; codex?: unknown };
}

export function diagramIdentity(claim: Claim, extraSecrets: string[] = []): { harness: string; editor_model: string } {
  let codex: boolean;
  let invalid = false;
  try {
    codex = selectCodexBinding(claim.secrets).kind === "codex";
  } catch {
    codex = true;
    invalid = true;
  }
  const selected = claim.summary_model?.trim() ?? "";
  const model = codex ? CODEX_PR_DESCRIPTION_MODEL : isValidModel(selected) ? selected : "haiku";
  const secrets = [claim.secrets.anthropic_oauth_token, ...extraSecrets];
  if (claim.secrets.codex && typeof claim.secrets.codex === "object") {
    secrets.push(...Object.values(claim.secrets.codex).filter((v): v is string => typeof v === "string"));
  }
  const safe = Buffer.byteLength(model, "utf8") <= 100 && !/[\s\p{Cc}\p{Cf}\uFFFD]/u.test(model) &&
    makeTextRedactor(secrets)(model) === model && !secrets.some((secret) => secret && model.includes(secret)) && !/(?:glpat-|github_pat_|gh[pousr]_|sk-|xox[baprs]-)/i.test(model);
  return { harness: codex ? "codex" : "claude", editor_model: !invalid && safe && model ? model : "unknown" };
}

export type DiagramParserReason = "absent" | "valid" | "shape" | "kind" | "entries" | "title" | "node_shape" | "key" | "node_label" | "edge_shape" | "endpoints" | "edge_label";
export type DiagramReason = Exclude<DiagramParserReason, "absent" | "valid"> | "editor_omission" | "deadline" | "invalid_codex" | "missing_factory" | "missing_credential" | "pass_failed" | "unparseable" | "missing_summary" | "zero_code" | "mermaid_bytes" | "backtick" | "region_bytes" | "diagramless" | "sizeonly";

export function diagramEvent(
  log: Pick<Logger, "info">,
  claim: Claim,
  stage: "editor" | "agent_parser" | "zero_code" | "renderer" | "region_cap" | "body_cap",
  outcome: "omitted" | "dropped",
  reason: DiagramReason,
  correlation: { claim_generation?: number; version_id?: string } = {},
  extraSecrets: string[] = [],
): void {
  log.info("PR description diagram", {
    run_id: claim.run_id, stage, outcome, reason, ...diagramIdentity(claim, extraSecrets), ...correlation,
  });
}
