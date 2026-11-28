// Pure classifier for the forge-token banner (#4817). A forge connection's stored
// privilege report says whether its token has expired, is about to, or could not be
// verified; bannerFor picks the single most severe class to show.

import type { ForgeConnection } from "./api";
import { forgePlatform } from "./forgeNoun";

type TokenClass = "expired" | "expiring" | "unverified";

interface AffectedConnection {
  id: string;
  forge: string;
  host: string;
  expiresAt?: string;
}

interface BannerState {
  cls: TokenClass;
  affected: AffectedConnection[];
}

// Matches the checker's warnWindow (api/internal/privcheck/checker.go): strict "before".
const WARN_WINDOW_MS = 14 * 24 * 3600 * 1000;
// The checker's own violation strings for an expired or inactive token.
const EXPIRED_VIOLATIONS = ["token has expired", "token is not active"];

const SEVERITY: Record<TokenClass, number> = { expired: 3, expiring: 2, unverified: 1 };

function expiryOf(conn: ForgeConnection | null | undefined): string | undefined {
  const raw = conn?.privilege_report?.token?.expires_at;
  return typeof raw === "string" ? raw : undefined;
}

export function classifyConnection(
  conn: ForgeConnection | null | undefined,
  nowMs: number,
): TokenClass | null {
  const report = conn?.privilege_report;
  if (!report || typeof report !== "object") return null;
  if (report.status === "error") return "unverified";
  const token = report.token;
  if (!token || typeof token !== "object") return null;

  const t = Date.parse(expiryOf(conn) ?? "");
  if (Number.isFinite(t) && t <= nowMs) return "expired";
  const violations: unknown = token.violations;
  if (Array.isArray(violations) && violations.some((v) => EXPIRED_VIOLATIONS.includes(v as string))) {
    return "expired";
  }
  if (Number.isFinite(t) && t > nowMs && t < nowMs + WARN_WINDOW_MS) return "expiring";
  return null;
}

function hostOf(baseUrl: unknown): string {
  const raw = typeof baseUrl === "string" ? baseUrl : "";
  try {
    return new URL(raw).host || raw;
  } catch {
    return raw;
  }
}

export function bannerFor(
  connections: readonly ForgeConnection[],
  nowMs: number,
): BannerState | null {
  let best: TokenClass | null = null;
  const classes: Array<[ForgeConnection, TokenClass]> = [];
  for (const conn of connections) {
    const cls = classifyConnection(conn, nowMs);
    if (cls === null) continue;
    classes.push([conn, cls]);
    if (best === null || SEVERITY[cls] > SEVERITY[best]) best = cls;
  }
  if (best === null) return null;
  const winner = best;
  const affected = classes
    .filter(([, cls]) => cls === winner)
    .map(([conn]) => ({
      id: conn.id,
      forge: forgePlatform(conn.forge_type),
      host: hostOf(conn.base_url),
      expiresAt: winner === "expiring" ? expiryOf(conn) : undefined,
    }));
  return { cls: winner, affected };
}
