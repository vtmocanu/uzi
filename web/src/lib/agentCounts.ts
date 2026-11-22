// Agent counts for the Agents page summary line (#4335), plus the shadowing
// helpers the page shares with it.
//
// Population rule: the agents "you have" are every builtin and global template
// plus your OWN private ("Mine") templates. Other users' private templates are
// dropped even for an admin, and an allocation with no matching template is
// dropped. The lead is counted like any other agent (no special case).
//
// This deliberately differs from the retired old Overview's "Agent templates" tile, which counted every
// template the caller could see (for an admin, other users' private ones too).
// Here `total` is the population above and `inRuns` is the subset that actually
// reaches a run (effective and not shadowed by a same-named builtin/global).

import type { AgentTemplate, AgentTemplateScope, TemplateAllocation } from "./api";
import { TEMPLATE_SCOPE_LABEL } from "./agentTemplates";

interface ScopedName {
  scope: AgentTemplateScope;
  name: string;
}

/** Names carried by builtin/global rows: they take precedence over user rows. */
export function sharedTemplateNames(rows: ScopedName[]): Set<string> {
  return new Set(rows.filter((r) => r.scope !== "user").map((r) => r.name));
}

/** A user row whose name collides with a shared one is dropped from runs. */
export function isShadowedTemplate(row: ScopedName, sharedNames: Set<string>): boolean {
  return row.scope === "user" && sharedNames.has(row.name);
}

export interface AgentCountScope {
  scope: AgentTemplateScope;
  label: string;
  total: number;
  inRuns: number;
}

export interface AgentCountModel {
  total: number;
  inRuns: number;
  byScope: AgentCountScope[];
  /** Present only for admins. */
  globalDefaults?: number;
}

const SCOPE_ORDER: AgentTemplateScope[] = ["builtin", "global", "user"];

export function agentCountModel(
  allocations: TemplateAllocation[],
  templates: AgentTemplate[],
  opts: { isAdmin: boolean; viewerId: string },
): AgentCountModel {
  const byId = new Map(templates.map((t) => [t.id, t]));
  const population: { scope: AgentTemplateScope; name: string; alloc: TemplateAllocation }[] = [];
  for (const alloc of allocations) {
    const t = byId.get(alloc.id);
    if (!t) continue;
    if (t.scope === "user" && t.user_id !== opts.viewerId) continue;
    population.push({ scope: t.scope, name: t.name, alloc });
  }

  // Shared names come from every template, not only allocation-backed rows, so a builtin or
  // global that lacks an allocation row (two separate reads) still shadows exactly as the
  // Agents page badge does.
  const shared = sharedTemplateNames(templates);
  const stats = new Map<AgentTemplateScope, { total: number; inRuns: number }>();
  let inRuns = 0;
  for (const row of population) {
    const s = stats.get(row.scope) ?? { total: 0, inRuns: 0 };
    s.total += 1;
    if (row.alloc.effective && !isShadowedTemplate(row, shared)) {
      s.inRuns += 1;
      inRuns += 1;
    }
    stats.set(row.scope, s);
  }

  const byScope: AgentCountScope[] = SCOPE_ORDER.flatMap((scope) => {
    const s = stats.get(scope);
    return s && s.total > 0 ? [{ scope, label: TEMPLATE_SCOPE_LABEL[scope], ...s }] : [];
  });

  const model: AgentCountModel = { total: population.length, inRuns, byScope };
  if (opts.isAdmin) {
    model.globalDefaults = population.filter((r) => r.alloc.global_default).length;
  }
  return model;
}
