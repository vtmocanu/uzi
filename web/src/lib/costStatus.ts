// Truthful cost-status rendering (PRD #1429 M4b / D7). Every usage reader must branch
// on the CLOSED per-run cost_status (or, for an aggregate, the subscription/unreported
// run counts) — never re-derive the same fact from cost_usd. A $0 metered total is a
// REAL reading (a tiny or cache-only call can genuinely price at $0.00), so "cost_usd
// <= 0 therefore subscription" was always a guess, and it is the guess this module
// retires. Both halves live here so RunUsage.tsx, UsageCards.tsx and RunsList.tsx
// share ONE mapping rather than three copies that can drift.
//
// cost_status is a Harness/CostStatus-shaped union that a NEWER server can extend
// without this build knowing (see the comment on CostStatus in apiTypes.ts) — so every
// function below treats anything other than the two known non-metered values
// ("subscription") as the SAME safe "unavailable" bucket: the empty string (a
// pre-M1 run whose status was never backfilled), "unreported" itself, and any future
// enum value alike. None of them may ever render as a bare "$0" or "—" standing in for
// zero — that ambiguity (does "—" mean "no data" or "we chose not to say") is exactly
// the bug D7 exists to close.
import type { CostStatus, Harness } from "./apiTypes";
import { formatCost } from "./formatTokens";

export type CostDisplayKind = "metered" | "subscription" | "unavailable";

export interface CostDisplay {
  kind: CostDisplayKind;
  /** formatCost(cost_usd) — set ONLY when kind === "metered". Never read cost_usd
   *  directly in a renderer; go through this so an "unavailable"/"subscription" run's
   *  real (often nonzero-looking on the wire) cost_usd can never leak into view. */
  dollars?: string;
}

/** Fold a per-run (or per-window) cost_status + its cost_usd into the one display
 *  decision every renderer switches on. `status` is typed as the closed wire union
 *  plus "" (the documented pre-M1/unset value) — a hostile value from a newer server
 *  still narrows to "unavailable" at runtime even though it is not a member of the
 *  compile-time type (the union is a compile-time hint, not a runtime guarantee; see
 *  CostStatus in apiTypes.ts). */
export function costDisplay(status: CostStatus | "", costUsd: number): CostDisplay {
  if (status === "metered") return { kind: "metered", dollars: formatCost(costUsd) };
  if (status === "subscription") return { kind: "subscription" };
  return { kind: "unavailable" };
}

/** The Cost stat tile's headline VALUE. Deliberately never "—" for a non-metered run:
 *  "—" already meant "no data" elsewhere in this file (e.g. the Model column's "no
 *  model recorded"), and reusing it here would recreate the exact ambiguity this
 *  milestone retires. "Subscription"/"Unavailable" are unambiguous words instead. */
export function costHeadline(d: CostDisplay): string {
  if (d.kind === "metered") return d.dollars as string;
  if (d.kind === "subscription") return "Subscription";
  return "Unavailable";
}

/** The Cost stat tile's one-line sub-label. `harness` is the closed runs.harness
 *  union (untrusted forward-compat text per Harness's own doc comment) — branched on
 *  by value only, never interpolated, so an unrecognised future harness safely falls
 *  through to the generic "your token" rather than rendering raw. */
export function costSubLabel(d: CostDisplay, harness?: Harness | string | null): string {
  if (d.kind === "metered") {
    if (harness === "codex") return "your OpenAI credential";
    if (harness === "claude") return "your Anthropic token";
    return "your token";
  }
  if (d.kind === "subscription") return "subscription usage · no metered cost";
  return "tokens only · cost unavailable";
}

/** A dense table cell's cost text — compact, and still never a bare dollar figure or
 *  "—" for a non-metered run. "sub"/"n/a" are short enough not to break a numeric
 *  column's width while remaining distinct words a reader can look up, unlike a dash. */
export function costCellText(d: CostDisplay): string {
  if (d.kind === "metered") return d.dollars as string;
  if (d.kind === "subscription") return "sub";
  return "n/a";
}

// ── Aggregate disclosure (mixed metered/subscription/unreported totals) ───────────
//
// An aggregate (SelfUsage / AdminUsage / AdminUsageUser) has no single cost_status —
// its cost_usd is the SUM across every folded run, so it is by construction the
// METERED SUBSET's dollar total (a subscription or unreported run contributes $0 to
// that stored numeric). Showing that dollar figure alone, with no signal that other
// runs exist and are NOT counted in it, is the aggregate shape of the same bug: a
// reader has no way to tell "the whole cost was $12" from "$12 is only the metered
// slice of a bigger, partly-unknown total". aggregateDisclosure turns the window's
// subscription/unreported run counts into that disclosure.
export interface AggregateDisclosure {
  /** True when one or more subscription/unreported runs are folded into this
   *  window's token totals but excluded from its dollar figure. */
  incomplete: boolean;
  /** Disclosure text, e.g. "2 subscription + 1 unreported run excluded from the $
   *  total". Exactly "" (byte-identical) when `incomplete` is false, so a fully-
   *  metered window renders no disclosure noise at all. */
  text: string;
}

export function aggregateDisclosure(subscriptionCount: number, unreportedCount: number): AggregateDisclosure {
  const sub = Math.max(0, subscriptionCount);
  const unrep = Math.max(0, unreportedCount);
  if (sub <= 0 && unrep <= 0) return { incomplete: false, text: "" };
  const parts: string[] = [];
  if (sub > 0) parts.push(`${sub} subscription`);
  if (unrep > 0) parts.push(`${unrep} unreported`);
  const n = sub + unrep;
  return { incomplete: true, text: `${parts.join(" + ")} run${n === 1 ? "" : "s"} excluded from the $ total` };
}
