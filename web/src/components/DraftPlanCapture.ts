import type { RunMessage } from "../lib/api";

const DRAFT_LABEL = "draft, unapproved, possibly incomplete";

// M1's harness reducer writes this status payload; ActivityFeed reads it to select
// the latest card and RunEventRow reads it to render. No persisted record is changed.
export function parseDraftPlanCapture(msg: RunMessage): {
  label: typeof DRAFT_LABEL;
  plan_md: string;
  truncated: boolean;
} | undefined {
  if (msg.kind !== "status" || msg.agent !== "lead" || msg.agent_instance != null) return;
  const payload = msg.payload;
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return;
  const rec = payload as Record<string, unknown>;
  if (
    rec["event"] !== "draft_plan_capture" ||
    rec["version"] !== 1 ||
    rec["label"] !== DRAFT_LABEL ||
    typeof rec["plan_md"] !== "string" ||
    typeof rec["truncated"] !== "boolean"
  ) return;
  return { label: DRAFT_LABEL, plan_md: rec["plan_md"], truncated: rec["truncated"] };
}
