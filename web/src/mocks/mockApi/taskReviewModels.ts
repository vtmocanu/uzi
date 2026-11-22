import type { ModelSettingCell } from "../../lib/apiTypes";

// The mock's task-review model cells (PRD #1570), shaped like the server's
// task_review_models: claude then codex. The Claude built-in is "" (the SDK default),
// the Codex built-in is gpt-6-sol; a stored value resolves as the user's default.
const BUILTIN = { claude: "", codex: "gpt-6-sol" } as const;

export function mockTaskReviewModelCells(
  claude: string | null | undefined,
  codex: string | null | undefined,
): ModelSettingCell[] {
  return (["claude", "codex"] as const).map((harness) => {
    const stored = (harness === "claude" ? claude : codex)?.trim() || null;
    return {
      harness,
      model: stored,
      default_model: BUILTIN[harness],
      default_source: "builtin",
      resolved_model: stored ?? BUILTIN[harness],
      source: stored ? "user_default" : "builtin",
    };
  });
}
