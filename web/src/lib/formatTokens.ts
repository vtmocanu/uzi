// One shared token formatter (PRD #40 Decision 10) so every usage figure — run
// view strip, per-phase/per-agent tables, finish lines, runs list, dashboard —
// reads identically across the app. Adaptive scale: bare integer under 1k, "k"
// with one decimal under 1M, "M" with two decimals above. Always pair with a
// `tabular-nums font-mono` element so the digits line up column to column.
//
//   999      → "999"
//   48_200   → "48.2k"
//   188_000  → "188.0k"
//   1_280_000→ "1.28M"
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n < 0) n = 0;
  n = Math.round(n);
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

// formatCost renders a USD cost the way the mock does: "$1.87". A zero cost with
// nonzero tokens is a subscription-auth run the SDK prices at $0 (Decision 8) —
// callers render "—" for that rather than a misleading "$0.00", so this only ever
// formats a genuinely present cost.
export function formatCost(usd: number): string {
  if (!Number.isFinite(usd) || usd < 0) usd = 0;
  return `$${usd.toFixed(2)}`;
}
