import { describe, expect, it } from "vitest";

import type { ForgeConnection } from "./api";
import { bannerFor, classifyConnection } from "./forgeTokenBanner";

const NOW = Date.parse("2026-06-15T12:00:00Z");
const DAY = 24 * 3600 * 1000;

function conn(
  id: string,
  opts: {
    status?: string;
    expiresAt?: string;
    violations?: string[];
    forge?: string;
    baseUrl?: string;
    noToken?: boolean;
  } = {},
): ForgeConnection {
  return {
    id,
    forge_type: opts.forge ?? "github",
    base_url: "baseUrl" in opts ? opts.baseUrl : `https://${id}.example.com`,
    privilege_report: {
      status: opts.status ?? "ok",
      checked_at: "2026-06-15T00:00:00Z",
      repos: [],
      token: opts.noToken
        ? undefined
        : {
            scopes: [],
            active: true,
            expires_at: opts.expiresAt,
            violations: opts.violations ?? [],
            warnings: [],
          },
    },
  } as unknown as ForgeConnection;
}

const at = (offsetMs: number) => new Date(NOW + offsetMs).toISOString();

describe("classifyConnection", () => {
  it("classifies a past expires_at as expired", () => {
    expect(classifyConnection(conn("a", { expiresAt: at(-DAY) }), NOW)).toBe("expired");
  });
  it.each(["token has expired", "token is not active"])("classifies violation %s as expired", (v) => {
    expect(classifyConnection(conn("a", { violations: [v], status: "violations" }), NOW)).toBe("expired");
  });
  it("classifies expiry in 3 days as expiring", () => {
    expect(classifyConnection(conn("a", { expiresAt: at(3 * DAY) }), NOW)).toBe("expiring");
  });
  it("ignores expiry 30 days out", () => {
    expect(classifyConnection(conn("a", { expiresAt: at(30 * DAY) }), NOW)).toBeNull();
  });
  it("ignores a scope violation with no expiry", () => {
    expect(classifyConnection(conn("a", { status: "violations", violations: ["token has write scope"] }), NOW)).toBeNull();
  });
  it("an error report is unverified, even with an inactive token or past expiry", () => {
    expect(classifyConnection(conn("a", { status: "error" }), NOW)).toBe("unverified");
    expect(classifyConnection(conn("a", { status: "error", expiresAt: at(-DAY) }), NOW)).toBe("unverified");
    const c = conn("a", { status: "error" });
    c.privilege_report!.token.active = false;
    expect(classifyConnection(c, NOW)).toBe("unverified");
  });
  it("returns null for missing data and never throws", () => {
    expect(classifyConnection({ ...conn("a"), privilege_report: null }, NOW)).toBeNull();
    expect(classifyConnection({} as ForgeConnection, NOW)).toBeNull();
    expect(classifyConnection(conn("a", { noToken: true }), NOW)).toBeNull();
    expect(classifyConnection(null, NOW)).toBeNull();
  });
  it("ignores a malformed expires_at but still honours a violation", () => {
    expect(classifyConnection(conn("a", { expiresAt: "not-a-date" }), NOW)).toBeNull();
    expect(classifyConnection(conn("a", { expiresAt: "not-a-date", violations: ["token has expired"] }), NOW)).toBe("expired");
  });
  it("a stale warnings status whose expiry is now past is expired", () => {
    expect(classifyConnection(conn("a", { status: "warnings", expiresAt: at(-1) }), NOW)).toBe("expired");
  });
  it("applies the boundaries strictly", () => {
    expect(classifyConnection(conn("a", { expiresAt: at(0) }), NOW)).toBe("expired");
    expect(classifyConnection(conn("a", { expiresAt: at(1) }), NOW)).toBe("expiring");
    expect(classifyConnection(conn("a", { expiresAt: at(14 * DAY - 1) }), NOW)).toBe("expiring");
    expect(classifyConnection(conn("a", { expiresAt: at(14 * DAY) }), NOW)).toBeNull();
  });
});

describe("bannerFor", () => {
  it("returns null with no connections or nothing affected", () => {
    expect(bannerFor([], NOW)).toBeNull();
    expect(bannerFor([conn("a")], NOW)).toBeNull();
  });
  it("highest class wins and lists only its connections", () => {
    const b = bannerFor(
      [
        conn("exp", { expiresAt: at(-DAY) }),
        conn("soon", { expiresAt: at(2 * DAY) }),
        conn("err", { status: "error" }),
        conn("exp2", { violations: ["token is not active"] }),
      ],
      NOW,
    );
    expect(b?.cls).toBe("expired");
    expect(b?.affected.map((a) => a.id)).toEqual(["exp", "exp2"]);
  });
  it("expiring beats unverified", () => {
    const b = bannerFor([conn("err", { status: "error" }), conn("soon", { expiresAt: at(2 * DAY) })], NOW);
    expect(b?.cls).toBe("expiring");
    expect(b?.affected.map((a) => a.id)).toEqual(["soon"]);
  });
  it("each expiring connection carries its own date, forge and host", () => {
    const d1 = at(2 * DAY);
    const d2 = at(5 * DAY);
    const b = bannerFor(
      [conn("a", { expiresAt: d1 }), conn("b", { expiresAt: d2, forge: "gitlab" })],
      NOW,
    );
    expect(b?.affected).toEqual([
      { id: "a", forge: "GitHub", host: "a.example.com", expiresAt: d1 },
      { id: "b", forge: "GitLab", host: "b.example.com", expiresAt: d2 },
    ]);
  });
  it("does not throw on an undefined or malformed base_url", () => {
    const b = bannerFor([conn("a", { expiresAt: at(-DAY), baseUrl: undefined }), conn("b", { expiresAt: at(-DAY), baseUrl: "not a url" })], NOW);
    expect(b?.affected.map((a) => a.host)).toEqual(["", "not a url"]);
  });
});
