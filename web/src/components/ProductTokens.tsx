// Settings → Access: product tokens (PRD #1907 M5). A uzp_ token a user mints for an
// admin-registered external product; it acts as that user on /api/v1 only, with the
// scopes picked here. Same lifecycle and idiom as CliTokens: mint (show-once), list
// with the forensic surface (prefix / last used / last IP / expiry), single revoke.
// The panic button stays the ONE "Revoke all" in CliTokens, which revokes these too
// (D8); AccessSettings bumps `reloadKey` after it so this list reflects the change.
//
// Every product name, description and token name is untrusted text and is rendered
// as a React text node only (escaped), never as HTML.

import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import {
  api,
  type MintableProduct,
  type ProductToken,
  type ProductTokenExpiry,
  type ProductTokenScope,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { useDemoMode } from "../lib/demoMode";
import { maskIp } from "../lib/demoMask";
import { PRODUCT_NAME_MAX_BYTES, productTextError } from "../lib/productText";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Select } from "./ui";
import { PackageIcon } from "./icons";

// The scope vocabulary in the user's words (D5). Order is the display order. /api/v1
// has only /whoami today; the job endpoints arrive with PRD #1908, so each hint says
// what the permission WILL allow rather than promising a capability that is not there.
const SCOPES: { value: ProductTokenScope; label: string; hint: string }[] = [
  { value: "jobs:read", label: "Read jobs", hint: "Will allow reading job status once job endpoints ship." },
  { value: "jobs:run", label: "Run jobs", hint: "Will allow starting jobs as you once job endpoints ship." },
];
const SCOPE_LABEL = Object.fromEntries(SCOPES.map((s) => [s.value, s.label])) as Record<
  ProductTokenScope,
  string
>;

// The per-user list cap (the server's; `truncated` reports the cut).
const USER_LIST_CAP = 200;

// D10: 90 days is the default; "never" is allowed for unattended products but is
// never preselected.
const EXPIRIES: { value: ProductTokenExpiry; label: string }[] = [
  { value: "30d", label: "30 days" },
  { value: "90d", label: "90 days" },
  { value: "1y", label: "1 year" },
  { value: "never", label: "Never" },
];

// A token is active when it is neither revoked nor past its expiry (the server's
// is_active, D15). expires_at null means it never expires.
function isExpired(t: ProductToken, now = Date.now()): boolean {
  return t.expires_at !== null && Date.parse(t.expires_at) <= now;
}
export function isProductTokenActive(t: ProductToken): boolean {
  return !t.revoked && !isExpired(t);
}

export function ProductTokens({
  reloadKey = 0,
  onActiveCountChange,
}: {
  // Bumped by the parent after Revoke all (which also revokes product tokens), so the
  // list refetches without owning that button here.
  reloadKey?: number;
  // Reports how many tokens are active, so the shared Revoke all can count and offer
  // itself even when the user holds no CLI token. null means "unknown" (the list
  // failed to load, or was cut while every listed row is active): the parent then
  // keeps Revoke all available and words its confirm without a product count.
  onActiveCountChange?: (count: number | null) => void;
}) {
  const { data, loading, error: loadError, reload } = useAsyncData<{
    tokens: ProductToken[];
    truncated: boolean;
    products: MintableProduct[];
  }>(
    async () => {
      const [{ tokens, truncated }, { products }] = await Promise.all([
        api.listProductTokens(),
        api.listMintableProducts(),
      ]);
      return { tokens, truncated: truncated === true, products };
    },
    [reloadKey],
    { fallback: "Failed to load product tokens" },
  );
  const tokens = data?.tokens ?? [];
  const truncated = data?.truncated ?? false;
  const products = data?.products ?? [];
  // The first load failed: there is no list to speak about, so only the error shows
  // (an empty state here would claim "no products" / "no tokens" it cannot know).
  const loadFailed = data === null && loadError !== "";
  const [error, setError] = useState("");

  const [productId, setProductId] = useState("");
  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<ProductTokenScope[]>(["jobs:read"]);
  const [expiry, setExpiry] = useState<ProductTokenExpiry>("90d");
  const [busy, setBusy] = useState(false);

  // With exactly one product there is nothing to choose; preselect it. With several,
  // the user picks deliberately (a token for the wrong product is a real mistake).
  const chosenProductId =
    productId && products.some((p) => p.id === productId)
      ? productId
      : products.length === 1
        ? products[0].id
        : "";
  const chosenProduct = products.find((p) => p.id === chosenProductId);

  // Show-once plaintext of the just-minted token (only its hash is stored).
  const [minted, setMinted] = useState<{ token: string; row: ProductToken } | null>(null);
  const [copied, setCopied] = useState(false);
  const mintedRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (minted) mintedRef.current?.focus();
  }, [minted]);

  // A parent-driven reload means Revoke all just ran, which revoked the token this panel
  // shows: drop the panel with it, so a dead secret is not left on screen to be copied.
  // Adjusted during render (React's prop-change pattern), not in an effect.
  const [seenReloadKey, setSeenReloadKey] = useState(reloadKey);
  if (reloadKey !== seenReloadKey) {
    setSeenReloadKey(reloadKey);
    setMinted(null);
  }

  // Rows come active first, so the count is exact unless the cut may have dropped
  // active rows too (every listed row active), or the list could not be read.
  const activeCount = tokens.filter(isProductTokenActive).length;
  const countKnown =
    loadError === "" && data !== null && !(truncated && activeCount === tokens.length);
  useEffect(() => {
    if (loading && data === null && loadError === "") return; // nothing to report yet
    onActiveCountChange?.(countKnown ? activeCount : null);
  }, [loading, data, loadError, countKnown, activeCount, onActiveCountChange]);

  const ids = useId();
  const productHintId = `${ids}-product-hint`;
  const expiryHintId = `${ids}-expiry-hint`;

  const toggleScope = (s: ProductTokenScope, on: boolean) =>
    setScopes((prev) => (on ? [...new Set([...prev, s])] : prev.filter((x) => x !== s)));

  const nameError = productTextError("Name", name, PRODUCT_NAME_MAX_BYTES);
  const nameErrorId = `${ids}-name-error`;
  const canCreate =
    !busy && chosenProductId !== "" && name.trim() !== "" && nameError === null && scopes.length > 0;

  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (!canCreate) return;
    setError("");
    setBusy(true);
    try {
      const { token, product_token } = await api.createProductToken({
        product_id: chosenProductId,
        name: name.trim(),
        // Stable order on the wire whatever order the boxes were ticked in.
        scopes: SCOPES.map((s) => s.value).filter((s) => scopes.includes(s)),
        expiry,
      });
      setMinted({ token, row: product_token });
      setCopied(false);
      setName("");
      await reload();
    } catch (err) {
      // The 409 cap ("you already have 10 active tokens for this product; …") and the
      // disabled-product 409 carry the actionable wording; show it verbatim.
      setError(errorMessage(err, "Failed to create product token"));
    } finally {
      setBusy(false);
    }
  };

  const copy = async () => {
    if (!minted) return;
    try {
      await navigator.clipboard.writeText(minted.token);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the token stays visible.
    }
  };

  const revoke = async (id: string) => {
    setError("");
    try {
      await api.revokeProductToken(id);
      // Revoking the token whose secret is still on screen retires that panel too.
      setMinted((m) => (m?.row.id === id ? null : m));
      await reload();
    } catch (err) {
      setError(errorMessage(err, "Failed to revoke token"));
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Product tokens</SectionTitle>
        <p className="mt-2 max-w-prose text-sm text-muted">
          Let an external product call uzi’s{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">/api/v1</code> as you. Each token
          belongs to one product and carries only the permissions you pick; it cannot reach the
          rest of uzi. A password change or sign-out does <strong className="text-fg">not</strong>{" "}
          revoke it.
        </p>
      </div>

      {(error || loadError) && <Alert message={error || loadError} />}

      {minted && (
        <div
          ref={mintedRef}
          tabIndex={-1}
          role="status"
          className="space-y-3 rounded-xl border border-ok/40 bg-surface p-5 outline-hidden"
        >
          <SectionTitle className="text-ok">
            Token “{minted.row.name}” created for {minted.row.product_name}
          </SectionTitle>
          <p className="text-sm text-muted">
            Copy it into {minted.row.product_name} now. It is shown{" "}
            <strong className="text-fg">once and never again</strong> (only its hash is stored), so
            you won’t see this value again.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-lg border border-edge bg-ink console px-3 py-2 font-mono text-sm text-ok">
              {minted.token}
            </code>
            <Button variant="secondary" onClick={copy}>
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <div>
            <Button variant="ghost" onClick={() => setMinted(null)}>
              Done
            </Button>
          </div>
        </div>
      )}

      {loading || loadFailed ? null : products.length === 0 ? (
        <EmptyState
          icon={<PackageIcon />}
          title="No products to connect yet"
          description="A product appears here once an admin registers and enables it under Admin → Products. Ask your admin if you expected one."
        />
      ) : (
        <form onSubmit={create} className="space-y-4" aria-label="Create a product token">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Field label="Product" htmlFor="product-token-product">
                <Select
                  id="product-token-product"
                  value={chosenProductId}
                  aria-describedby={chosenProduct?.description ? productHintId : undefined}
                  onChange={(e) => setProductId(e.target.value)}
                >
                  {products.length > 1 && (
                    <option value="" disabled>
                      Choose a product
                    </option>
                  )}
                  {products.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </Select>
              </Field>
              {chosenProduct?.description && (
                <p id={productHintId} className="text-xs text-faint">
                  {chosenProduct.description}
                </p>
              )}
            </div>
            <Field label="Name" htmlFor="product-token-name">
              <Input
                id="product-token-name"
                placeholder="e.g. production, staging"
                value={name}
                aria-invalid={nameError !== null || undefined}
                aria-describedby={nameError ? nameErrorId : undefined}
                onChange={(e) => setName(e.target.value)}
              />
              {nameError && (
                <p id={nameErrorId} className="text-xs text-warn">
                  {nameError}
                </p>
              )}
            </Field>
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <fieldset className="space-y-1.5">
              <legend className="mb-1.5 text-sm font-medium text-muted">Permissions</legend>
              {SCOPES.map((s) => (
                <label key={s.value} className="flex items-start gap-2 text-sm text-fg">
                  <input
                    type="checkbox"
                    className="mt-0.5 h-4 w-4 accent-brand"
                    checked={scopes.includes(s.value)}
                    onChange={(e) => toggleScope(s.value, e.target.checked)}
                  />
                  <span>
                    {s.label}
                    <span className="block text-xs text-faint">{s.hint}</span>
                  </span>
                </label>
              ))}
              {scopes.length === 0 && (
                <p className="text-xs text-warn">Pick at least one permission.</p>
              )}
            </fieldset>
            <div className="space-y-1">
              <Field label="Expires after" htmlFor="product-token-expiry">
                <Select
                  id="product-token-expiry"
                  value={expiry}
                  aria-describedby={expiry === "never" ? expiryHintId : undefined}
                  onChange={(e) => setExpiry(e.target.value as ProductTokenExpiry)}
                >
                  {EXPIRIES.map((x) => (
                    <option key={x.value} value={x.value}>
                      {x.label}
                    </option>
                  ))}
                </Select>
              </Field>
              {expiry === "never" && (
                <p id={expiryHintId} className="text-xs text-warn">
                  It stays valid until you or an admin revoke it, the product is disabled or
                  deleted, or your account is deactivated.
                </p>
              )}
            </div>
          </div>

          <Button type="submit" disabled={!canCreate}>
            {busy ? "Creating…" : "Create product token"}
          </Button>
        </form>
      )}

      {!loadFailed && (
        <div className="space-y-3">
          <SectionTitle>Your product tokens</SectionTitle>
          {truncated && (
            <p className="rounded-lg border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
              Showing your first {USER_LIST_CAP} product tokens, active first; older tokens are not
              listed.
            </p>
          )}
          {loading ? (
            <div className="space-y-2">
              <div className="h-14 animate-pulse rounded-lg bg-raised" />
              <div className="h-14 animate-pulse rounded-lg bg-raised" />
            </div>
          ) : tokens.length === 0 ? (
            <p className="text-sm text-faint">You have no product tokens.</p>
          ) : (
            <ul className="space-y-2">
              {tokens.map((t) => (
                <ProductTokenRow
                  key={t.id}
                  token={t}
                  // The mint picker lists exactly the enabled, live products, so an
                  // active token whose product is missing from it is being refused.
                  productUnavailable={!products.some((p) => p.id === t.product_id)}
                  onRevoke={() => revoke(t.id)}
                />
              ))}
            </ul>
          )}
        </div>
      )}
    </Card>
  );
}

// The shared "which scopes / what state / when does it end" vocabulary for a product
// token row, reused by the admin inventory (AdminProducts) so both surfaces say it the
// same way.
export function ProductTokenBadges({ token }: { token: ProductToken }) {
  const expired = !token.revoked && isExpired(token);
  return (
    <>
      {token.scopes.map((s) => (
        <Badge key={s} tone={s === "jobs:run" ? "info" : "neutral"}>
          {SCOPE_LABEL[s] ?? s}
        </Badge>
      ))}
      {token.revoked && <Badge tone="danger">revoked</Badge>}
      {expired && <Badge tone="warning">expired</Badge>}
    </>
  );
}

export function productTokenExpiryText(token: ProductToken): string {
  if (token.expires_at === null) return "never expires";
  const when = new Date(token.expires_at).toLocaleDateString();
  return isExpired(token) ? `expired ${when}` : `expires ${when}`;
}

function ProductTokenRow({
  token,
  productUnavailable,
  onRevoke,
}: {
  token: ProductToken;
  productUnavailable: boolean;
  onRevoke: () => void;
}) {
  const demo = useDemoMode();
  const active = isProductTokenActive(token);
  return (
    <li
      className={
        "flex flex-col gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm" +
        (active ? "" : " opacity-60")
      }
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <code className="font-mono text-fg">{token.token_prefix}…</code>
            <span className="truncate font-medium text-fg">{token.name}</span>
            <span className="text-muted">for {token.product_name}</span>
            <ProductTokenBadges token={token} />
            {active && productUnavailable && (
              <Badge tone="warning" title="An admin disabled or deleted this product, so uzi refuses this token.">
                product unavailable
              </Badge>
            )}
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-faint">
            <span>created {new Date(token.created_at).toLocaleDateString()}</span>
            <span>
              ·{" "}
              {token.last_used_at
                ? `last used ${new Date(token.last_used_at).toLocaleString()}`
                : "never used"}
            </span>
            <span>
              · {token.last_used_ip ? `from ${maskIp(token.last_used_ip, demo)}` : "no IP recorded"}
            </span>
            <span>· {productTokenExpiryText(token)}</span>
          </div>
        </div>
        {active && (
          <Button
            variant="danger"
            size="sm"
            onClick={onRevoke}
            aria-label={`Revoke ${token.name}`}
          >
            Revoke
          </Button>
        )}
      </div>
    </li>
  );
}
