// Settings → Access → Connected products (PRD #1910 M5). The products the user approved through
// "Connect with uzi": one row per LIVE connection (grant), whatever the state of its access
// tokens. A product renews its hourly access tokens itself, so a connection whose tokens all
// expired is still live and still has to be revocable here; listing by token would hide it.
//
// Revoking ends the connection: the product's access stops on its next request, its refresh
// token stops working and jobs it created are cancelled. The product has to ask again. The panic
// button stays the ONE "Revoke all" in CliTokens, which revokes connections too (D6); this
// component reports its count up so that confirm can name it, and AccessSettings bumps
// `reloadKey` after it so this list refetches.
//
// Every product name is untrusted text and is rendered as a React text node only (escaped),
// never as HTML.

import { useEffect, useRef, useState } from "react";
import { api, ApiError, type OAuthConnection } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { scopeText } from "../lib/oauthClient";
import { Alert, Badge, Button, Card, EmptyState, SectionTitle } from "./ui";
import { PackageIcon } from "./icons";

export function OAuthConnections({
  reloadKey = 0,
  onCountChange,
}: {
  // Bumped by the parent after Revoke all (which also revokes connections), so the list refetches
  // without owning that button here.
  reloadKey?: number;
  // Reports how many live connections there are, so the shared Revoke all can count and offer
  // itself even when the user holds no token. null means "unknown" (the list failed to load): the
  // parent then keeps Revoke all available and words its confirm without a number.
  onCountChange?: (count: number | null) => void;
}) {
  const { data, loading, error: loadError, reload } = useAsyncData<OAuthConnection[]>(
    async () => (await api.listOAuthConnections()).connections,
    [reloadKey],
    { fallback: "Failed to load connected products" },
  );
  const connections = data ?? [];
  // The first load failed: there is no list to speak about, so only the error shows (an empty
  // state here would claim "no connected products" it cannot know).
  const loadFailed = data === null && loadError !== "";
  const [error, setError] = useState("");
  const [confirmingId, setConfirmingId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const confirmRef = useRef<HTMLDivElement>(null);
  // Focus the warning, not the destructive button, when the confirm arms (CliTokens).
  useEffect(() => {
    if (confirmingId) confirmRef.current?.focus();
  }, [confirmingId]);

  useEffect(() => {
    if (loading && data === null && loadError === "") return; // nothing to report yet
    onCountChange?.(data !== null && loadError === "" ? data.length : null);
  }, [loading, data, loadError, onCountChange]);

  const revoke = async (id: string) => {
    setError("");
    setBusy(true);
    try {
      await api.revokeOAuthConnection(id);
    } catch (err) {
      // A 404 means it is already gone (revoked elsewhere): the row is stale, so the refetch
      // below drops it. Anything else is a real failure to report.
      if (!(err instanceof ApiError && err.status === 404)) {
        setError(errorMessage(err, "Failed to revoke the connection"));
        setBusy(false);
        return;
      }
    }
    setConfirmingId(null);
    await reload();
    setBusy(false);
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Connected products</SectionTitle>
        <p className="mt-2 max-w-prose text-sm text-muted">
          Products you let act as you by approving their request to connect. Each one holds only the
          permissions you approved. Revoke a connection to cut the product off: it loses access on
          its next request, its running jobs are cancelled, and it must ask you to connect again.
          A password change or sign-out does <strong className="text-fg">not</strong> disconnect a
          product.
        </p>
      </div>

      {(error || loadError) && <Alert message={error || loadError} />}

      {loading ? (
        <div className="space-y-2">
          <div className="h-14 animate-pulse rounded-lg bg-raised" />
          <div className="h-14 animate-pulse rounded-lg bg-raised" />
        </div>
      ) : loadFailed ? null : connections.length === 0 ? (
        <EmptyState
          icon={<PackageIcon />}
          title="No connected products"
          description="When a product asks to connect to your uzi account and you approve it, it shows up here."
        />
      ) : (
        <ul className="space-y-2">
          {connections.map((c) => (
            <li
              key={c.id}
              className="flex flex-col gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm"
            >
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span className="truncate font-medium text-fg">{c.product_name}</span>
                    {c.scopes.map((s) => (
                      <Badge key={s} tone={s === "jobs:run" ? "info" : "neutral"}>
                        {scopeText(s)}
                      </Badge>
                    ))}
                  </div>
                  <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-faint">
                    <span>connected {new Date(c.connected_at).toLocaleDateString()}</span>
                    <span>
                      ·{" "}
                      {c.last_used_at
                        ? `last used ${new Date(c.last_used_at).toLocaleString()}`
                        : "never used"}
                    </span>
                  </div>
                </div>
                {confirmingId !== c.id && (
                  <Button
                    variant="danger"
                    size="sm"
                    disabled={busy}
                    onClick={() => setConfirmingId(c.id)}
                    aria-label={`Revoke ${c.product_name}`}
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
                  aria-label={`Confirm revoking ${c.product_name}`}
                  onKeyDown={(e) => {
                    if (e.key === "Escape") setConfirmingId(null);
                  }}
                  className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 outline-hidden"
                >
                  <p className="text-xs text-warn">
                    Disconnect “{c.product_name}”? It stops working on its next request and its
                    running jobs are cancelled. It has to ask you to connect again.
                  </p>
                  <div className="flex items-center gap-1.5">
                    <Button variant="danger" size="sm" disabled={busy} onClick={() => revoke(c.id)}>
                      Revoke connection
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => setConfirmingId(null)}>
                      Cancel
                    </Button>
                  </div>
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
