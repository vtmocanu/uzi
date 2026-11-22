import type { EffortSettingCell, Harness, ModelSettingCell } from "../../lib/apiTypes";
import { ApiError } from "../../lib/apiError";
import { CODEX_ROLE_MODELS } from "../../lib/codexModels";

// The mock's task-review model cells (PRD #1570), shaped like the server's
// task_review_models: claude then codex. The Claude built-in is "" (the SDK default),
// the Codex built-in is gpt-6-sol; a valid stored value resolves as the user's default.
// A stored value the lane no longer accepts (a Codex id in the Claude lane, a non-Codex
// id in the Codex lane) is ignored and reports the built-in, like the server.
const BUILTIN = { claude: "", codex: "gpt-6-sol" } as const;

export function mockTaskReviewModelCells(
  claude: string | null | undefined,
  codex: string | null | undefined,
): ModelSettingCell[] {
  return (["claude", "codex"] as const).map((harness) => {
    const raw = (harness === "claude" ? claude : codex)?.trim() || null;
    const valid = raw !== null && (harness === "codex" ? CODEX_ROLE_MODELS.includes(raw) : !CODEX_ROLE_MODELS.includes(raw));
    return {
      harness,
      model: raw,
      default_model: BUILTIN[harness],
      default_source: "builtin",
      resolved_model: valid ? raw : BUILTIN[harness],
      source: valid ? "user_default" : "builtin",
    };
  });
}

const LEVELS = ["low", "medium", "high", "xhigh", "max"];

// The task-review effort validator, mirroring the server's PUT rule: blank/null clears
// (inherit the worker effort), otherwise one of the five levels or "runtime".
export function mockTaskReviewEffort(raw: string | null | undefined, field: string): string | null {
  const trimmed = raw?.trim() ?? "";
  if (trimmed === "") return null;
  if (trimmed !== "runtime" && !LEVELS.includes(trimmed)) {
    throw new ApiError(400, `${field}: effort must be one of low, medium, high, xhigh, max or runtime`);
  }
  return trimmed;
}

// The task-review effort cells (PRD #1570 M2), claude then codex. A stored review value wins;
// otherwise the review inherits the harness's worker effort cell, so default_effort/default_source
// describe what a reset selects ("runtime" when the worker effort sends no level).
export function mockTaskReviewEffortCells(
  stored: { claude: string | null | undefined; codex: string | null | undefined },
  worker: { claude: EffortSettingCell; codex: EffortSettingCell },
): EffortSettingCell[] {
  return (["claude", "codex"] as Harness[]).map((harness) => {
    const w = worker[harness as "claude" | "codex"];
    const raw = (stored[harness as "claude" | "codex"] ?? "").trim();
    const base = {
      harness,
      effort: raw === "" ? null : raw,
      default_effort: w.mode === "runtime_default" ? "runtime" : (w.resolved_effort ?? "medium"),
      default_source: w.source,
    };
    if (raw === "runtime") return { ...base, resolved_effort: null, source: "user_default", mode: "runtime_default" as const };
    if (LEVELS.includes(raw)) return { ...base, resolved_effort: raw, source: "user_default", mode: "explicit" as const };
    return { ...base, resolved_effort: w.resolved_effort, source: raw === "" ? w.source : "builtin", mode: w.mode };
  });
}
