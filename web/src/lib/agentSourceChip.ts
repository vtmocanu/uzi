// The Overview Agents chip counts configured templates directly. The Agents page's
// allocation model is separate: missing allocations cannot hide a configured agent.

import type { AgentSourceView, AgentTemplate } from "./api";
import { templateOrigin } from "./agentTemplates";
import { maskHost } from "./demoMask";
import { formatAgo } from "./rateLimits";
import { stripUnsafeChars } from "./safeText";

export type AgentSourceGroupKey = "source" | "shipped" | "admin" | "yours";

export interface AgentSourceChipGroup {
  key: AgentSourceGroupKey;
  label: string;
  agents: { name: string; detail: string | null }[];
}

export interface AgentSourceChipModel {
  total: number;
  groups: AgentSourceChipGroup[];
  suffix: { text: string; tone: "danger" | "warn" | "muted" } | null;
  sourceMeta: string | null;
  sourceAppliedSha: string | null;
  sourceNotice: string | null;
  reviewInSettings: boolean;
}

const GROUPS: { key: AgentSourceGroupKey; label: string }[] = [
  { key: "source", label: "From your agent source" },
  { key: "shipped", label: "Shipped with uzi" },
  { key: "admin", label: "Added by an admin" },
  { key: "yours", label: "Yours" },
];

function groupOf(template: AgentTemplate): AgentSourceGroupKey {
  if (template.scope === "user") return "yours";
  if (templateOrigin(template) === "synced") return "source";
  if (template.scope === "builtin") return "shipped";
  return "admin";
}

export function agentSourceChipModel(
  templates: AgentTemplate[],
  opts: { viewerId: string; isAdmin: boolean; source: AgentSourceView | null; demo: boolean; now: number },
): AgentSourceChipModel {
  const population = templates.filter((t) => t.scope !== "user" || t.user_id === opts.viewerId);
  const groups = GROUPS.flatMap(({ key, label }) => {
    const agents = population.filter((t) => groupOf(t) === key).map((t) => ({
      name: stripUnsafeChars(t.name) || "Unnamed agent",
      detail: key === "shipped" && templateOrigin(t) === "admin" ? "edited by an admin" : null,
    }));
    return agents.length ? [{ key, label, agents }] : [];
  });

  const source = opts.isAdmin && opts.source?.config.enabled ? opts.source : null;
  let suffix: AgentSourceChipModel["suffix"] = null;
  let sourceNotice: string | null = null;
  if (source?.status.last_sync_status === "error") {
    suffix = { text: "sync failed", tone: "danger" };
    sourceNotice = "The last agent-source sync failed.";
  } else if (source?.staged?.pending) {
    const changed = source.staged.counts.changed;
    suffix = { text: changed > 0 ? `${changed} ${changed === 1 ? "change" : "changes"} to review` : "review pending", tone: "warn" };
    sourceNotice = changed > 0
      ? `A fetched snapshot is waiting for review: ${changed} ${changed === 1 ? "change" : "changes"}.`
      : "A fetched snapshot is waiting for review.";
  } else if (source?.status.update_available) {
    suffix = { text: "update available", tone: "warn" };
    sourceNotice = "A newer agent-source version is available.";
  } else if (source?.status.last_sync_status === "ok") {
    suffix = { text: "synced", tone: "muted" };
  }

  let sourceMeta: string | null = null;
  let sourceAppliedSha: string | null = null;
  if (source) {
    const url = stripUnsafeChars(maskHost(source.config.url, opts.demo));
    const ref = opts.demo ? "demo-ref" : stripUnsafeChars(source.config.ref);
    const target = [url, ref].filter(Boolean).join("@");
    const stamp = source.status.last_sync_at;
    const age = stamp && Number.isFinite(Date.parse(stamp)) ? `synced ${formatAgo(stamp, opts.now)}` : "";
    // This names the commit in force, never the latest fetched or unapplied commit.
    sourceAppliedSha = stripUnsafeChars(source.status.last_applied_sha ?? "").slice(0, 7) || null;
    sourceMeta = [target, age].filter(Boolean).join(", ") || null;
  }

  return {
    total: population.length,
    groups,
    suffix,
    sourceMeta,
    sourceAppliedSha,
    sourceNotice,
    reviewInSettings: suffix?.tone === "danger" || suffix?.tone === "warn",
  };
}
