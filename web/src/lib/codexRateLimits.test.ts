import { describe, it, expect } from "vitest";
import {
  codexAccountLabel,
  codexAccountWorstPct,
  codexResetEpoch,
  codexStatusBadge,
  codexWindowForecast,
  formatCodexWindowLabel,
  hasCodexReading,
  isCodexAccountShownInSidebar,
  sortCodexAdminRows,
} from "./codexRateLimits";
import { paceForecast } from "./rateLimits";
import type {
  CodexAccountRateLimit,
  CodexAdminRateLimitRow,
  CodexRateLimitBucket,
  CodexRateLimitStatus,
  CodexRateLimitWindow,
} from "./api";

function win(
  usedPct: number | null,
  windowSecs: number | null,
  resetAt: number | null,
  resetAfter: number | null = null,
): CodexRateLimitWindow {
  return {
    used_percent: usedPct,
    limit_window_seconds: windowSecs,
    reset_after_seconds: resetAfter,
    reset_at: resetAt,
  };
}

function bucket(
  id: string,
  primary: CodexRateLimitWindow | null,
  secondary: CodexRateLimitWindow | null = null,
  extra: Partial<CodexRateLimitBucket> = {},
): CodexRateLimitBucket {
  return { id, allowed: true, limit_reached: false, primary, secondary, ...extra };
}

function acct(
  account_id: string,
  status: CodexRateLimitStatus,
  buckets: CodexRateLimitBucket[],
  over: Partial<CodexAccountRateLimit> = {},
): CodexAccountRateLimit {
  return { account_id, aliases: [account_id], is_default: false, status, buckets, ...over };
}

describe("formatCodexWindowLabel", () => {
  it("derives the chip from the reported window length", () => {
    expect(formatCodexWindowLabel(18000)).toBe("5h");
    expect(formatCodexWindowLabel(604800)).toBe("7d");
    // The nonstandard 3-hour bucket: proof the label is computed, not a fixed 5h/7d.
    expect(formatCodexWindowLabel(10800)).toBe("3h");
    expect(formatCodexWindowLabel(300)).toBe("5m");
  });

  it("falls back to 'window' for a missing or impossible length", () => {
    expect(formatCodexWindowLabel(null)).toBe("window");
    expect(formatCodexWindowLabel(0)).toBe("window");
    expect(formatCodexWindowLabel(-1)).toBe("window");
  });

  it("renders an odd length in seconds rather than lying about a unit", () => {
    expect(formatCodexWindowLabel(10_000)).toBe("10000s");
  });
});

describe("codexResetEpoch", () => {
  const nowMs = 10_000_000_000; // nowSec = 10_000_000
  it("prefers the absolute reset_at", () => {
    expect(codexResetEpoch(win(50, 18000, 10_009_899, 123), nowMs)).toBe(10_009_899);
  });
  it("derives from reset_after_seconds when reset_at is absent", () => {
    expect(codexResetEpoch(win(50, 18000, null, 900), nowMs)).toBe(10_000_900);
  });
  it("is null when neither is reported (no horizon)", () => {
    expect(codexResetEpoch(win(50, 18000, null, null), nowMs)).toBeNull();
  });
});

describe("codexWindowForecast — anchors to the REPORTED window duration, never 5h/7d", () => {
  const nowMs = 10_000_000_000; // nowSec = 10_000_000
  // A 3-hour window (10800s) with ~901s elapsed and a 10% reading. Projected over the
  // REPORTED 10800s ⇒ ≈120% ⇒ "over" (a ghost + marker). Projected over a hardcoded
  // 18000s (5h) from the SAME reset ⇒ ≈22% ⇒ "safe". So the band itself flips on which
  // duration is used — this is the non-vacuous proof there is no 5h/7d hardcode.
  const resetAt = 10_000_000 + 9899; // elapsed against 10800 = 10800 - 9899 = 901
  const w3h = win(10, 10800, resetAt, 9899);

  it("bands the 3-hour window 'over' from its own reported length", () => {
    const f = codexWindowForecast(false, w3h, nowMs);
    expect(f.state).toBe("over");
    expect(f.projectedPct).toBe(120);
  });

  it("would be 'safe' if the length were the hardcoded 5h — proving the duration is read", () => {
    // The counterfactual: same reading + reset, but projected over 18000s.
    const wrong = paceForecast(10, resetAt, 18000, nowMs);
    expect(wrong.state).toBe("safe");
  });

  it("suppresses the forecast for a partial window (no percentage)", () => {
    expect(codexWindowForecast(false, win(null, 10800, resetAt), nowMs).state).toBe("safe");
  });

  it("suppresses the forecast when the window length is missing (impossible timing)", () => {
    expect(codexWindowForecast(false, win(10, null, resetAt), nowMs).state).toBe("safe");
    expect(codexWindowForecast(false, win(10, 0, resetAt), nowMs).state).toBe("safe");
  });

  it("suppresses the forecast when there is no reset horizon", () => {
    expect(codexWindowForecast(false, win(10, 10800, null, null), nowMs).state).toBe("safe");
  });

  it("suppresses the forecast on a stale reading", () => {
    expect(codexWindowForecast(true, w3h, nowMs).state).toBe("safe");
  });
});

describe("codexAccountLabel", () => {
  it("collapses duplicate aliases to one label", () => {
    expect(codexAccountLabel({ aliases: ["team-codex", "team-codex"] })).toBe("team-codex");
    expect(codexAccountLabel({ aliases: ["a", "b", "a"] })).toBe("a, b");
  });
  it("falls back to a neutral noun when there is no alias", () => {
    expect(codexAccountLabel({ aliases: [] })).toBe("Codex account");
    expect(codexAccountLabel({ aliases: ["  "] })).toBe("Codex account");
  });
});

describe("isCodexAccountShownInSidebar", () => {
  it("always shows the default account, whatever the chosen set", () => {
    expect(isCodexAccountShownInSidebar({ account_id: "a", is_default: true }, [])).toBe(true);
  });
  it("shows a non-default account only when its id is chosen", () => {
    expect(isCodexAccountShownInSidebar({ account_id: "a", is_default: false }, [])).toBe(false);
    expect(isCodexAccountShownInSidebar({ account_id: "a", is_default: false }, ["a"])).toBe(true);
    expect(isCodexAccountShownInSidebar({ account_id: "a", is_default: false }, ["b"])).toBe(false);
  });
});

describe("hasCodexReading / codexStatusBadge", () => {
  it("has a reading only for fresh and stale", () => {
    expect(hasCodexReading("fresh")).toBe(true);
    expect(hasCodexReading("stale")).toBe(true);
    for (const s of ["pending", "no_reading", "vault_locked", "credential_action_required", "polling_disabled", "no_subscription"] as const) {
      expect(hasCodexReading(s)).toBe(false);
    }
  });
  it("maps each status to a tone and label", () => {
    expect(codexStatusBadge("fresh")).toMatchObject({ tone: "ok", label: "Live" });
    expect(codexStatusBadge("credential_action_required")).toMatchObject({ tone: "danger", label: "Action required" });
    expect(codexStatusBadge("no_reading")).toMatchObject({ tone: "warning", label: "No reading yet" });
    expect(codexStatusBadge("stale").label).toBe("Stale");
    expect(codexStatusBadge("pending").label).toBe("Pending");
    expect(codexStatusBadge("vault_locked").label).toBe("🔒 Vault locked");
    expect(codexStatusBadge("polling_disabled").label).toBe("Polling off");
  });
});

describe("codexAccountWorstPct", () => {
  const nowFuture = Math.floor(Date.now() / 1000) + 5000;
  it("is the max used_percent across all windows on a reading account", () => {
    const a = acct("a", "fresh", [
      bucket("b1", win(30, 18000, nowFuture), win(72, 604800, nowFuture)),
      bucket("b2", win(55, 10800, nowFuture), null),
    ]);
    expect(codexAccountWorstPct(a)).toBe(72);
  });
  it("is null when the account has no numeric reading", () => {
    expect(codexAccountWorstPct(acct("a", "pending", []))).toBeNull();
    expect(codexAccountWorstPct(acct("a", "fresh", [bucket("b", win(null, 18000, nowFuture))]))).toBeNull();
  });
});

describe("sortCodexAdminRows", () => {
  const nowFuture = Math.floor(Date.now() / 1000) + 5000;
  const user = (
    id: string,
    accounts: CodexAccountRateLimit[],
    over: Partial<CodexAdminRateLimitRow> = {},
  ): CodexAdminRateLimitRow => ({ id, email: `${id}@x`, name: id, vault_locked: false, accounts, ...over });

  it("orders danger → warn → ok → stale → action → no-reading, then by pct desc", () => {
    const danger = user("danger", [acct("d", "fresh", [bucket("b", win(96, 18000, nowFuture))])]);
    const warn = user("warn", [acct("w", "fresh", [bucket("b", win(60, 18000, nowFuture))])]);
    const okLow = user("oklow", [acct("o", "fresh", [bucket("b", win(12, 18000, nowFuture))])]);
    const stale = user("stale", [acct("s", "stale", [bucket("b", win(80, 18000, nowFuture))], { stale: true })]);
    const action = user("action", [acct("a", "credential_action_required", [])]);
    const pending = user("pending", [acct("p", "pending", [])]);
    const sorted = sortCodexAdminRows([pending, okLow, action, danger, stale, warn]);
    expect(sorted.map((u) => u.id)).toEqual(["danger", "warn", "oklow", "stale", "action", "pending"]);
  });

  it("ranks a user by their MOST urgent account, not a sum (no double-count)", () => {
    // Two accounts: one exhausted, one idle. The user ranks with the exhausted one.
    const hot = user("hot", [
      acct("h1", "fresh", [bucket("b", win(97, 18000, nowFuture))]),
      acct("h2", "fresh", [bucket("b", win(3, 18000, nowFuture))]),
    ]);
    const mid = user("mid", [acct("m", "fresh", [bucket("b", win(50, 18000, nowFuture))])]);
    expect(sortCodexAdminRows([mid, hot]).map((u) => u.id)).toEqual(["hot", "mid"]);
  });
});
