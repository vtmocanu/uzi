// App-wide forge-token notice (#4817). Mounted once in AppBanners for every signed-in
// user: a forge connection whose token has expired, is about to, or could not be verified
// silently stops repositories syncing and runs pushing, so say so on any page.
//
// It reads GET /forge/connections in the background (on mount and on window focus, no
// interval) through api.listConnectionsBackground, whose 401 never signs the user out. It
// takes userId instead of calling useAuth so the account it belongs to is explicit: the
// effect is keyed on it, and a response from a previous account is dropped. All strings are
// rendered as React text, never HTML.

import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../lib/api";
import type { ForgeConnection } from "../lib/api";
import { bannerFor } from "../lib/forgeTokenBanner";
import { Button } from "./ui";

const STORAGE_PREFIX = "uzi:forge-token-banner:dismissed:";

function readDismissed(userId: string): Set<string> {
  try {
    const raw = window.sessionStorage.getItem(STORAGE_PREFIX + userId);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return new Set(Array.isArray(parsed) ? parsed.filter((s): s is string => typeof s === "string") : []);
  } catch {
    return new Set();
  }
}

function addDismissed(userId: string, signature: string): void {
  try {
    const next = readDismissed(userId).add(signature);
    window.sessionStorage.setItem(STORAGE_PREFIX + userId, JSON.stringify([...next]));
  } catch {
    // Storage unavailable: the dismissal simply does not persist.
  }
}

function formatDate(iso: string | undefined): string {
  const t = Date.parse(iso ?? "");
  if (!Number.isFinite(t)) return iso ?? "";
  return new Date(t).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

export function ForgeTokenBanner({ userId }: { userId: string }) {
  // The data is stamped with the account it was read for, so a render for another account
  // never shows it, even before this effect has run for the new account.
  const [data, setData] = useState<{ userId: string; connections: ForgeConnection[] } | null>(null);
  const [, setDismissTick] = useState(0);
  const seqRef = useRef(0);

  useEffect(() => {
    let live = true;
    // Release the previous account's data. The guarantee that it never renders for another
    // account is the render-time userId check below; this reset is defence in depth.
    setData(null);
    const load = () => {
      const seq = ++seqRef.current;
      Promise.resolve()
        .then(() => api.listConnectionsBackground())
        .then((r) => {
          if (!live || seq !== seqRef.current) return;
          if (!Array.isArray(r?.connections)) return;
          setData({ userId, connections: r.connections });
        })
        .catch(() => {
          // Keep the last good data; a failed background read is not an error to show.
        });
    };
    load();
    window.addEventListener("focus", load);
    return () => {
      window.removeEventListener("focus", load);
      live = false;
    };
  }, [userId]);

  const dismiss = useCallback(
    (signature: string) => {
      addDismissed(userId, signature);
      setDismissTick((n) => n + 1);
    },
    [userId],
  );

  if (data === null || data.userId !== userId) return null;
  const banner = bannerFor(data.connections, Date.now());
  if (banner === null) return null;

  const { cls, affected } = banner;
  const signature = `${cls}|${affected.map((a) => `${a.id}:${a.expiresAt ?? ""}`).sort().join(",")}`;
  if (cls !== "expired" && readDismissed(userId).has(signature)) return null;

  const tone =
    cls === "expired"
      ? "border-danger/40 bg-danger/10"
      : cls === "expiring"
        ? "border-warn/40 bg-warn/10"
        : "border-edge bg-surface";
  const label =
    cls === "expired"
      ? "Forge token expired"
      : cls === "expiring"
        ? "Forge token expiring"
        : "Forge connection unverified";

  return (
    <div
      role={cls === "expired" ? "alert" : "status"}
      aria-label={label}
      className={`mb-6 rounded-lg border px-4 py-3 ${tone}`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 space-y-2 text-sm">
          {cls === "expired" &&
            affected.map((a) => (
              <p key={a.id} className="text-fg">
                {`Your ${a.forge} connection's token has expired or is inactive. Repositories using it stop syncing and runs cannot push. Update the token in Settings → Forge.`}{" "}
                <span className="break-all text-muted">{a.host}</span>
              </p>
            ))}
          {cls === "expiring" &&
            affected.map((a) => (
              <p key={a.id} className="text-fg">
                {`Your ${a.forge} connection's token expires on ${formatDate(a.expiresAt)}. Update it in Settings → Forge before then.`}{" "}
                <span className="break-all text-muted">{a.host}</span>
              </p>
            ))}
          {cls === "unverified" && (
            <>
              <p className="text-fg">uzi could not verify this forge connection. Check it in Settings → Forge.</p>
              <ul className="text-muted">
                {affected.map((a) => (
                  <li key={a.id} className="break-all">{`${a.forge} · ${a.host}`}</li>
                ))}
              </ul>
            </>
          )}
          <Link to="/settings/forge" className="inline-block text-info hover:underline">
            Open Settings → Forge
          </Link>
        </div>
        {cls !== "expired" && (
          <Button variant="secondary" size="sm" onClick={() => dismiss(signature)}>
            Dismiss
          </Button>
        )}
      </div>
    </div>
  );
}
