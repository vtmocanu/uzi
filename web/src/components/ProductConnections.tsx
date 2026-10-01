// Admin → Products → one product's OAuth connections (PRD #1910 M5): who has connected the
// product through "Connect with uzi", whatever the state of their access tokens, with a revoke.
// The product's manual tokens stay in the inventory below the cards; a connection's hourly
// access tokens are not listed there (D5), so this panel is where an admin sees and ends them.
//
// IA mirrors ProductEgressProfiles: a disclosure at the foot of the product card, collapsed by
// default and loaded on first open. A deleted product still lists whatever grants it had (the
// audit trail) and the revoke still works, since the route is by connection id.
//
// The connecting users' emails and the product's data are untrusted text: React text nodes only.

import { useEffect, useRef, useState } from "react";
import { api, ApiError, type AdminOAuthConnection, type Product } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { scopeText } from "../lib/oauthClient";
import { Badge, Button, Spinner } from "./ui";

export function ProductConnectionsPanel({
  product,
  onChanged,
}: {
  product: Product;
  // Called after a revoke so the page can reload what depends on the connection count (the card
  // header and the delete confirmation read product.live_connection_count).
  onChanged?: () => Promise<unknown> | void;
}) {
  const [open, setOpen] = useState(false);
  const { data, loading, error: loadError, reload } = useAsyncData(
    () => api.adminListProductConnections(product.id),
    [product.id],
    { enabled: open, fallback: "Failed to load this product’s connections" },
  );
  const rows = data?.connections ?? null;

  return (
    <details
      className="group/connections border-t border-edge pt-3"
      onToggle={(e) => {
        setOpen((e.currentTarget as HTMLDetailsElement).open);
      }}
    >
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-2 gap-y-1 text-sm font-medium text-fg marker:content-none">
        <span
          aria-hidden="true"
          className="inline-block text-faint group-open/connections:rotate-90 motion-safe:transition-transform"
        >
          ▸
        </span>
        Connections
        {rows && (
          <span className="text-xs font-normal text-muted">
            {rows.length === 0 ? "none" : `${rows.length}${data?.truncated ? "+" : ""} connected`}
          </span>
        )}
        <span className="basis-full pl-4 text-xs font-normal text-faint">
          The users who connected this product through “Connect with uzi”.
        </span>
      </summary>

      {open && (
        <div className="mt-3 space-y-3 pl-4">
          {loadError && (
            <p role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
              {loadError}
            </p>
          )}
          {loading && !rows ? (
            <p className="flex items-center gap-2 text-sm text-muted">
              <Spinner /> Loading connections…
            </p>
          ) : rows ? (
            <ConnectionsBody
              rows={rows}
              truncated={data?.truncated === true}
              onRevoked={async () => {
                await reload();
                await onChanged?.();
              }}
            />
          ) : null}
        </div>
      )}
    </details>
  );
}

function ConnectionsBody({
  rows,
  truncated,
  onRevoked,
}: {
  rows: AdminOAuthConnection[];
  truncated: boolean;
  onRevoked: () => Promise<void>;
}) {
  const demo = useDemoMode();
  const [confirmingId, setConfirmingId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const confirmRef = useRef<HTMLDivElement>(null);
  // Focus the warning, not the destructive button, when the confirm arms (CliTokens).
  useEffect(() => {
    if (confirmingId) confirmRef.current?.focus();
  }, [confirmingId]);

  const revoke = async (c: AdminOAuthConnection) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await api.adminRevokeOAuthConnection(c.id);
      setNotice(`Revoked the connection of ${maskEmail(c.owner_email, demo)}. The product has to ask them again.`);
    } catch (err) {
      // A 404 means it is already gone (revoked elsewhere): the refetch below drops the stale row.
      if (!(err instanceof ApiError && err.status === 404)) {
        setError(errorMessage(err, "Failed to revoke the connection"));
        setBusy(false);
        return;
      }
    }
    setConfirmingId(null);
    await onRevoked();
    setBusy(false);
  };

  return (
    <>
      <p className="text-xs text-muted">
        Revoking a connection stops its access tokens and refresh token on their next use and
        cancels the jobs they created. The user can connect again.
      </p>
      {truncated && (
        <p className="rounded-lg border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
          Showing the {rows.length} most recent connections; older ones are not listed.
        </p>
      )}
      {rows.length === 0 ? (
        <p className="text-sm text-faint">No one has connected this product.</p>
      ) : (
        <ul className="space-y-1.5">
          {rows.map((c) => {
            const who = maskEmail(c.owner_email, demo) || "—";
            return (
              <li key={c.id} className="space-y-2 rounded-lg border border-edge bg-surface px-3 py-2">
                <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
                  <span className="min-w-0">
                    <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                      <span className="text-sm font-medium break-all text-fg">{who}</span>
                      {c.scopes.map((s) => (
                        <Badge key={s} tone={s === "jobs:run" ? "info" : "neutral"}>
                          {scopeText(s)}
                        </Badge>
                      ))}
                    </span>
                    <span className="block text-xs text-faint">
                      connected {new Date(c.connected_at).toLocaleDateString()} ·{" "}
                      {c.last_used_at ? `last used ${new Date(c.last_used_at).toLocaleString()}` : "never used"}
                    </span>
                  </span>
                  {confirmingId !== c.id && (
                    <Button
                      type="button"
                      variant="danger"
                      size="sm"
                      disabled={busy}
                      aria-label={`Revoke the connection of ${who}`}
                      onClick={() => setConfirmingId(c.id)}
                    >
                      Revoke
                    </Button>
                  )}
                </div>
                {confirmingId === c.id && (
                  <div
                    ref={confirmRef}
                    tabIndex={-1}
                    role="group"
                    aria-label={`Confirm revoking the connection of ${who}`}
                    onKeyDown={(e) => {
                      if (e.key === "Escape") setConfirmingId(null);
                    }}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 outline-hidden"
                  >
                    <p className="text-xs text-warn">
                      Revoke the connection of {who}? The product loses access on its next request and
                      the jobs it created for them are cancelled.
                    </p>
                    <div className="flex items-center gap-1.5">
                      <Button type="button" variant="danger" size="sm" disabled={busy} onClick={() => revoke(c)}>
                        Revoke connection
                      </Button>
                      <Button type="button" variant="ghost" size="sm" onClick={() => setConfirmingId(null)}>
                        Cancel
                      </Button>
                    </div>
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {error && (
        <p role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      <p role="status" className={notice ? "text-sm text-ok" : "sr-only"}>
        {notice}
      </p>
    </>
  );
}
