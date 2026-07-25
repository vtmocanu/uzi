// One shared token formatter (PRD #40 Decision 10) so every usage figure — run
// view strip, per-phase/per-agent tables, finish lines, runs list, dashboard —
// reads identically across the app. Adaptive scale: bare integer under 1k, "k"
// with one decimal under 1M, then "M"/"B"/"T" with two decimals. Always pair with
// a `tabular-nums font-mono` element so the digits line up column to column.
//
//   999          → "999"
//   48_200       → "48.2k"
//   188_000      → "188.0k"
//   1_280_000    → "1.28M"
//   5_400_000_000→ "5.40B"
//   2.3e12       → "2.30T"
//
// The M scale used to be the top of the ladder, so a run in the billions rendered
// as "5400.00M" (issue #77) — technically correct, unreadable in a column, and
// increasingly common as runs get longer. B and T continue the same two-decimal
// convention rather than introducing a third.
//
// NOTE on an adjacent wart, left ALONE deliberately: each tier's upper boundary
// can still round up past its own unit — 999_999 renders "1000.0k", not "1.00M",
// because the tier is chosen from the raw value before `toFixed` rounds it. Same
// defect class one rung down, but fixing it changes what a handful of existing
// values render as, which is a behaviour change nobody asked for in #77's scope.
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n < 0) n = 0;
  n = Math.round(n);
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  if (n < 1_000_000_000_000) return `${(n / 1_000_000_000).toFixed(2)}B`;
  return `${(n / 1_000_000_000_000).toFixed(2)}T`;
}

// formatCost renders a USD cost the way the mock does: "$1.87". A zero cost with
// nonzero tokens is a subscription-auth run the SDK prices at $0 (Decision 8) —
// callers render "—" for that rather than a misleading "$0.00", so this only ever
// formats a genuinely present cost.
export function formatCost(usd: number): string {
  if (!Number.isFinite(usd) || usd < 0) usd = 0;
  return `$${usd.toFixed(2)}`;
}
