// Admin → Products → one product's allowed site lists (PRD #1976 M2). A product's tokens
// may only name these lists (egress profiles, managed under Site lists) when they create a
// job; the panel is where an admin adds or removes one.
//
// IA mirrors ProductSkills: a disclosure at the foot of the product card, collapsed by
// default and loaded on first open. A deleted product still lists its lists (the audit
// trail) but the api refuses a write, so the panel is read-only for it.

import { useId, useState } from "react";
import { api, type Product, type ProductEgressProfile } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Button, Select, Spinner, cx } from "./ui";

export function ProductEgressProfilesPanel({ product }: { product: Product }) {
  const deleted = product.deleted_at !== null;
  const [open, setOpen] = useState(false);
  const { data, loading, error: loadError } = useAsyncData(
    () => api.adminListProductEgressProfiles(product.id),
    [product.id],
    { enabled: open, fallback: "Failed to load this product’s site lists" },
  );
  // The admin's whole set of lists, only needed to offer an add; a deleted product offers none.
  const { data: all, error: allError } = useAsyncData(
    () => api.adminListEgressProfiles(),
    [product.id],
    { enabled: open && !deleted, fallback: "Failed to load the site lists" },
  );
  // A write's own response is the freshest view, but only over the load it superseded.
  const [fresh, setFresh] = useState<{ rows: ProductEgressProfile[]; over: typeof data } | null>(null);
  const rows = fresh && fresh.over === data ? fresh.rows : (data?.egress_profiles ?? null);

  return (
    <details
      className="group/egress border-t border-edge pt-3"
      onToggle={(e) => {
        const isOpen = (e.currentTarget as HTMLDetailsElement).open;
        setOpen(isOpen);
        if (!isOpen) setFresh(null);
      }}
    >
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-2 gap-y-1 text-sm font-medium text-fg marker:content-none">
        <span
          aria-hidden="true"
          className="inline-block text-faint group-open/egress:rotate-90 motion-safe:transition-transform"
        >
          ▸
        </span>
        Site lists
        {rows && (
          <span className="text-xs font-normal text-muted">
            {rows.length === 0 ? "none allowed" : `${rows.length} allowed`}
          </span>
        )}
        <span className="basis-full pl-4 text-xs font-normal text-faint">
          The site lists this product’s tokens may name when they create a job.
        </span>
      </summary>

      {open && (
        <div className="mt-3 space-y-3 pl-4">
          {loadError && !rows && (
            <p role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
              {loadError}
            </p>
          )}
          {loading && !rows ? (
            <p className="flex items-center gap-2 text-sm text-muted">
              <Spinner /> Loading site lists…
            </p>
          ) : rows ? (
            <ListsBody
              product={product}
              deleted={deleted}
              rows={rows}
              available={all?.egress_profiles.map((p) => p.name) ?? []}
              availableError={allError}
              onRows={(next) => setFresh({ rows: next, over: data })}
            />
          ) : null}
        </div>
      )}
    </details>
  );
}

function ListsBody({
  product,
  deleted,
  rows,
  available,
  availableError,
  onRows,
}: {
  product: Product;
  deleted: boolean;
  rows: ProductEgressProfile[];
  available: string[];
  availableError: string;
  onRows: (rows: ProductEgressProfile[]) => void;
}) {
  const id = useId();
  const [choice, setChoice] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const allowed = new Set(rows.map((r) => r.name));
  const addable = available.filter((n) => !allowed.has(n));
  // A selection another action made unavailable falls back to the empty choice.
  const selected = addable.includes(choice) ? choice : "";

  const add = async () => {
    if (selected === "" || busy) return;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const next = await api.adminAllowProductEgressProfile(product.id, selected);
      onRows(next.egress_profiles);
      setNotice(`Allowed ${selected}. Tokens of this product can name it on job create.`);
      setChoice("");
    } catch (err) {
      setError(errorMessage(err, "Failed to allow the site list"));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (name: string) => {
    if (busy) return;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await api.adminDisallowProductEgressProfile(product.id, name);
      onRows(rows.filter((r) => r.name !== name));
      setNotice(`Removed ${name}. Jobs created from now on can no longer name it.`);
    } catch (err) {
      setError(errorMessage(err, "Failed to remove the site list"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <p className="text-xs text-muted">
        A job created with this product’s token may only name these lists. Removing a list
        affects jobs created afterwards; jobs already created keep the lists they were created with.
      </p>

      {rows.length === 0 ? (
        <p className="text-sm text-faint">No site lists allowed. Jobs from this product’s tokens cannot name one.</p>
      ) : (
        <ul className="space-y-1.5">
          {rows.map((r) => (
            <li
              key={r.name}
              className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 rounded-lg border border-edge bg-surface px-3 py-2"
            >
              <span className="min-w-0">
                <span className="font-mono text-sm font-medium break-all text-fg">{r.name}</span>
                <span className="block text-xs text-muted">{r.description || "No description."}</span>
              </span>
              {!deleted && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  disabled={busy}
                  aria-label={`Remove ${r.name}`}
                  onClick={() => remove(r.name)}
                >
                  Remove
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}

      {deleted ? (
        <p className="text-xs text-faint">This product is deleted, so its site lists can no longer change.</p>
      ) : (
        <div className="space-y-1.5">
          <label htmlFor={`${id}-add`} className="block text-xs font-medium text-muted">
            Allow another site list
          </label>
          <div className="flex flex-wrap items-center gap-2">
            <Select
              id={`${id}-add`}
              value={selected}
              disabled={busy || addable.length === 0}
              className="max-w-xs"
              onChange={(e) => setChoice(e.target.value)}
            >
              <option value="">{addable.length === 0 ? "No lists left to allow" : "Choose a site list"}</option>
              {addable.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </Select>
            <Button type="button" variant="secondary" size="sm" disabled={selected === "" || busy} onClick={add}>
              {busy ? "Saving…" : "Allow"}
            </Button>
          </div>
          {availableError && <p className="text-xs text-danger">{availableError}</p>}
        </div>
      )}

      {error && (
        <p role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      <p role="status" className={cx("text-sm text-ok", !notice && "sr-only")}>
        {notice}
      </p>
    </>
  );
}
