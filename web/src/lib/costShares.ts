// Largest-remainder (Hamilton) rounding: integer percentages that sum to exactly
// 100. Floor each row's raw share, then award the leftover point(s) to the largest
// fractional remainders. Deterministic tie-break (the leftover often ties): larger
// raw total first, then row order — so per-row output is pinnable. Integer-exact
// (remainder == total*100 % factoryTotal) so ties compare exactly. Guards a
// non-positive factory total to all zeros; forcing 100 is correct only because the
// rows partition the factory total (AdminUsagePerUser and AdminUsageTotals sum the
// same non-chat runs).
function costShares(totals: number[], factoryTotal: number): number[] {
  if (factoryTotal <= 0) return totals.map(() => 0);
  const scaled = totals.map((t) => t * 100);
  const floors = scaled.map((s) => Math.floor(s / factoryTotal));
  const remainders = scaled.map((s, i) => s - floors[i] * factoryTotal); // exact s % factoryTotal
  const leftover = 100 - floors.reduce((a, b) => a + b, 0);
  const order = totals
    .map((_, i) => i)
    .sort((a, b) => {
      if (remainders[b] !== remainders[a]) return remainders[b] - remainders[a]; // largest remainder first
      if (totals[b] !== totals[a]) return totals[b] - totals[a]; // then larger raw total
      return a - b; // then row order
    });
  const shares = floors.slice();
  for (let k = 0; k < leftover && k < shares.length; k++) shares[order[k]] += 1;
  return shares;
}

// Each row's displayed cost share (whole percents summing to 100) from the rows' recorded
// dollar costs and the factory's. Rank uses unrounded cost; shares use integer microdollars.
// undefined when the factory recorded no cost, so callers show "—" instead of a fabricated 0%.
// Shared by the current Overview's per-user table and the preview's By user table.
export function costShareRows(rowCosts: number[], factoryCost: number): number[] | undefined {
  const factoryMicroUSD = Math.round(factoryCost * 1e6);
  if (factoryMicroUSD <= 0) return undefined;
  return costShares(rowCosts.map((c) => Math.round(c * 1e6)), factoryMicroUSD);
}
