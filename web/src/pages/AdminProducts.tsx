// Admin → Products (PRD #1907 M4): the registry of external products users may mint
// /api/v1 tokens for, and the product-credential inventory beneath each one.
//
// IA: one card per product, because every admin decision here is per product (turn
// it off, delete it, kill one of its tokens) and the token rows only mean something
// next to the product they unlock. Soft-deleted products (D9) stay listed, last, as the
// audit trail; they have no controls, since a deleted product cannot change.
//
// Product names/descriptions (admin-written) and token names/owner emails
// (user-written) are untrusted text: React text nodes only, never HTML.

import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, type AdminProductToken, type Product } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail, maskIp } from "../lib/demoMask";
import {
  PRODUCT_DESCRIPTION_MAX_BYTES,
  PRODUCT_NAME_MAX_BYTES,
  productTextError,
  trimProductText,
} from "../lib/productText";
import {
  Alert,
  Badge,
  Button,
  Card,
  EmptyState,
  Field,
  Input,
  ListSkeleton,
  SectionTitle,
  Toggle,
} from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import {
  isProductTokenActive,
  ProductTokenBadges,
  productTokenExpiryText,
} from "../components/ProductTokens";
import { PackageIcon } from "../components/icons";

const plural = (n: number, noun: string) => `${n} ${noun}${n === 1 ? "" : "s"}`;

export function AdminProducts() {
  const { data, loading, error: loadError, reload } = useAsyncData<{
    products: Product[];
    tokens: AdminProductToken[];
    truncated: boolean;
  }>(
    async () => {
      const [{ products }, { tokens, truncated }] = await Promise.all([
        api.adminListProducts(),
        api.adminListProductTokens(),
      ]);
      return { products, tokens, truncated: truncated === true };
    },
    [],
    { fallback: "Failed to load products" },
  );
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // When the server cut the inventory, how many tokens it did list (the notices name
  // this number rather than a hard-coded cap that could drift from the server's).
  const truncatedAt = data?.truncated ? data.tokens.length : null;
  // The first load failed: show only the error, never a "No products registered"
  // empty state the page cannot know to be true.
  const loadFailed = data === null && loadError !== "";

  // Live products first, soft-deleted ones last (the audit trail), server order within.
  const products = [...(data?.products ?? [])].sort(
    (a, b) => Number(a.deleted_at !== null) - Number(b.deleted_at !== null),
  );
  const tokensByProduct = new Map<string, AdminProductToken[]>();
  for (const t of data?.tokens ?? []) {
    const list = tokensByProduct.get(t.product_id) ?? [];
    list.push(t);
    tokensByProduct.set(t.product_id, list);
  }

  // Wraps every write: clears the banners, runs it, reloads, and surfaces the
  // server's message (a 409 on a duplicate name or a deleted product) on failure.
  const run = async (fn: () => Promise<string | void>, fallback: string): Promise<boolean> => {
    setError("");
    setNotice("");
    try {
      const msg = await fn();
      if (msg) setNotice(msg);
      await reload();
      return true;
    } catch (err) {
      setError(errorMessage(err, fallback));
      return false;
    }
  };

  return (
    <AdminShell description="External products your users can connect to uzi’s /api/v1. Disabling or deleting a product stops every token for it on its next request.">
      {(error || loadError) && <Alert message={error || loadError} />}
      {notice && <Alert tone="success" message={notice} />}

      <CreateProduct
        onCreate={(name, description) =>
          run(async () => {
            const { product } = await api.adminCreateProduct(name, description);
            return `Registered “${product.name}”. Users can now mint tokens for it.`;
          }, "Failed to register product")
        }
      />

      {truncatedAt !== null && (
        <p className="rounded-lg border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
          Showing the first {truncatedAt} tokens, active first; older tokens are not listed.
        </p>
      )}

      {loading ? (
        <ListSkeleton rows={3} />
      ) : loadFailed ? null : products.length === 0 ? (
        <EmptyState
          icon={<PackageIcon />}
          title="No products registered"
          description="Register a product above. Users then mint a token for it under Settings → Access."
        />
      ) : (
        <div className="space-y-4">
          {products.map((p) => (
            <ProductCard
              key={p.id}
              product={p}
              tokens={tokensByProduct.get(p.id) ?? []}
              truncatedAt={truncatedAt}
              onToggle={(enabled) =>
                run(async () => {
                  await api.adminUpdateProduct(p.id, { enabled });
                }, "Failed to update product")
              }
              onDelete={() =>
                run(async () => {
                  const res = await api.adminDeleteProduct(p.id);
                  return `Deleted “${res.product.name}”. Stopped ${plural(res.stopped_token_count, "active token")}.`;
                }, "Failed to delete product")
              }
              onRevoke={(t) =>
                run(async () => {
                  await api.adminRevokeProductToken(t.id);
                  return `Revoked “${t.name}” (${t.token_prefix}…).`;
                }, "Failed to revoke token")
              }
            />
          ))}
        </div>
      )}
    </AdminShell>
  );
}

function CreateProduct({ onCreate }: { onCreate: (name: string, description: string) => Promise<boolean> }) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [busy, setBusy] = useState(false);
  // Inline mirrors of the server's gate (byte caps, no control characters), so a value
  // the server is certain to refuse never leaves the form.
  const nameError = productTextError("Name", name, PRODUCT_NAME_MAX_BYTES);
  const descriptionError = productTextError("Description", description, PRODUCT_DESCRIPTION_MAX_BYTES);
  const canSubmit = !busy && trimProductText(name) !== "" && nameError === null && descriptionError === null;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!canSubmit) return;
    setBusy(true);
    const ok = await onCreate(trimProductText(name), trimProductText(description));
    setBusy(false);
    if (ok) {
      setName("");
      setDescription("");
    }
  };

  return (
    <Card>
      <form onSubmit={submit} className="space-y-3" aria-label="Register a product">
        <SectionTitle>Register a product</SectionTitle>
        <Field label="Name" htmlFor="product-name">
          <Input
            id="product-name"
            placeholder="e.g. Helpdesk assistant"
            value={name}
            aria-invalid={nameError !== null || undefined}
            aria-describedby={nameError ? "product-name-error" : undefined}
            onChange={(e) => setName(e.target.value)}
          />
          {nameError && (
            <p id="product-name-error" className="text-xs text-warn">
              {nameError}
            </p>
          )}
        </Field>
        {/* One line on purpose: the server refuses any newline in a description (it is
            printed in listings, where a line break could forge a row). */}
        <Field label="Description (shown to users when they mint a token)" htmlFor="product-description">
          <Input
            id="product-description"
            placeholder="What it does with uzi, so users know what they are connecting."
            value={description}
            aria-invalid={descriptionError !== null || undefined}
            aria-describedby={descriptionError ? "product-description-error" : undefined}
            onChange={(e) => setDescription(e.target.value)}
          />
          {descriptionError && (
            <p id="product-description-error" className="text-xs text-warn">
              {descriptionError}
            </p>
          )}
        </Field>
        <Button type="submit" disabled={!canSubmit}>
          {busy ? "Registering…" : "Register product"}
        </Button>
      </form>
    </Card>
  );
}

// Only the deleted state gets a badge: a live product's state is carried by its
// enable switch and that switch's visible label, so a badge would say it twice.
function DeletedBadge({ deletedAt }: { deletedAt: string }) {
  return (
    <Badge tone="danger" dot>
      Deleted {new Date(deletedAt).toLocaleDateString()}
    </Badge>
  );
}

function ProductCard({
  product,
  tokens,
  truncatedAt,
  onToggle,
  onDelete,
  onRevoke,
}: {
  product: Product;
  tokens: AdminProductToken[];
  // Non-null when the inventory was cut server-side (the number listed), so an empty
  // `tokens` does not mean none exist.
  truncatedAt: number | null;
  onToggle: (enabled: boolean) => Promise<boolean>;
  onDelete: () => Promise<boolean>;
  onRevoke: (t: AdminProductToken) => Promise<boolean>;
}) {
  const deleted = product.deleted_at !== null;
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const confirmRef = useRef<HTMLDivElement>(null);
  // Focus the warning, not the destructive button, when the confirm arms (CliTokens).
  useEffect(() => {
    if (confirming) confirmRef.current?.focus();
  }, [confirming]);

  const act = async (fn: () => Promise<boolean>) => {
    setBusy(true);
    await fn();
    setBusy(false);
  };

  // What the delete will stop: an enabled product's active tokens. A disabled
  // product's tokens are already refused, so deleting it stops none (the server's
  // stopped_token_count says the same).
  const stops = product.enabled ? product.active_token_count : 0;
  const warningId = `delete-warning-${product.id}`;
  const headingId = `product-${product.id}`;

  return (
    <section aria-labelledby={headingId}>
      <Card className={deleted ? "space-y-4 opacity-75" : "space-y-4"}>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0 space-y-1">
            <div className="flex flex-wrap items-center gap-2">
              <h3 id={headingId} className="truncate text-base font-semibold text-fg">
                {product.name}
              </h3>
              {product.deleted_at !== null && <DeletedBadge deletedAt={product.deleted_at} />}
              <span className="text-xs text-muted">
                {plural(product.active_token_count, "active token")}
              </span>
            </div>
            {product.description ? (
              <p className="max-w-prose text-sm text-muted">{product.description}</p>
            ) : (
              <p className="text-sm text-faint">No description.</p>
            )}
          </div>
          {!deleted && !confirming && (
            <div className="flex items-center gap-3">
              <label className="flex items-center gap-2 text-sm text-muted">
                <Toggle
                  checked={product.enabled}
                  disabled={busy}
                  // Keep keyboard focus on the switch while its own request is in flight
                  // (a native disabled button is blurred), as ScheduleListRow does.
                  focusableWhenDisabled
                  label={`${product.enabled ? "Disable" : "Enable"} ${product.name}`}
                  onChange={(next) => act(() => onToggle(next))}
                />
                <span aria-hidden="true">{product.enabled ? "Enabled" : "Disabled"}</span>
              </label>
              <Button variant="danger" size="sm" disabled={busy} onClick={() => setConfirming(true)}>
                Delete
              </Button>
            </div>
          )}
        </div>

        {confirming && (
          <div
            ref={confirmRef}
            tabIndex={-1}
            role="group"
            aria-label={`Confirm deleting ${product.name}`}
            aria-describedby={warningId}
            onKeyDown={(e) => {
              if (e.key === "Escape") setConfirming(false);
            }}
            className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 outline-hidden"
          >
            <p id={warningId} className="text-xs text-warn">
              Delete “{product.name}”? This stops {plural(stops, "active token")}
              {product.enabled
                ? " on their next request."
                : ": it is disabled, so its tokens are already refused."}{" "}
              It can never be re-enabled; the product and its token history stay listed.
            </p>
            <div className="flex items-center gap-1.5">
              <Button
                variant="danger"
                size="sm"
                disabled={busy}
                onClick={() =>
                  act(async () => {
                    const ok = await onDelete();
                    if (ok) setConfirming(false);
                    return ok;
                  })
                }
              >
                Delete product
              </Button>
              <Button variant="ghost" size="sm" onClick={() => setConfirming(false)}>
                Cancel
              </Button>
            </div>
          </div>
        )}

        {tokens.length === 0 ? (
          <p className="text-sm text-faint">
            {truncatedAt !== null
              ? `None of this product’s tokens are among the first ${truncatedAt} listed; its tokens may be beyond the list.`
              : "No tokens minted for this product."}
          </p>
        ) : (
          <ProductTokenTable tokens={tokens} onRevoke={onRevoke} />
        )}
      </Card>
    </section>
  );
}

function ProductTokenTable({
  tokens,
  onRevoke,
}: {
  tokens: AdminProductToken[];
  onRevoke: (t: AdminProductToken) => Promise<boolean>;
}) {
  const demo = useDemoMode();
  const [busyId, setBusyId] = useState<string | null>(null);
  return (
    <div className="overflow-x-auto rounded-lg border border-edge">
      <table className="w-full text-left text-sm">
        <caption className="sr-only">Tokens for this product</caption>
        <thead className="border-b border-edge text-muted">
          <tr>
            <th className="px-3 py-2 font-medium">Owner</th>
            <th className="px-3 py-2 font-medium">Token</th>
            <th className="px-3 py-2 font-medium">Access</th>
            <th className="px-3 py-2 font-medium">Last used</th>
            <th className="px-3 py-2 font-medium">Expiry</th>
            <th className="px-3 py-2 text-right font-medium">Action</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-edge">
          {tokens.map((t) => {
            const active = isProductTokenActive(t);
            return (
              <tr key={t.id} className={active ? "" : "opacity-60"}>
                <td className="px-3 py-2 text-fg">{maskEmail(t.owner_email, demo) || "—"}</td>
                <td className="px-3 py-2">
                  <span className="block font-medium text-fg">{t.name}</span>
                  <code className="font-mono text-xs text-muted">{t.token_prefix}…</code>
                </td>
                <td className="px-3 py-2">
                  <div className="flex flex-wrap gap-1">
                    <ProductTokenBadges token={t} />
                  </div>
                </td>
                <td className="px-3 py-2 text-xs text-muted">
                  {t.last_used_at ? new Date(t.last_used_at).toLocaleString() : "never used"}
                  {t.last_used_ip && (
                    <span className="block text-faint">from {maskIp(t.last_used_ip, demo)}</span>
                  )}
                </td>
                <td className="px-3 py-2 text-xs text-muted">{productTokenExpiryText(t)}</td>
                <td className="px-3 py-2 text-right">
                  {active && (
                    <Button
                      variant="danger"
                      size="sm"
                      disabled={busyId === t.id}
                      aria-label={`Revoke ${t.name}`}
                      onClick={async () => {
                        setBusyId(t.id);
                        await onRevoke(t);
                        setBusyId(null);
                      }}
                    >
                      Revoke
                    </Button>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
