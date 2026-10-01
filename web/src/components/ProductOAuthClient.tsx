// Admin → Products → one product's OAuth client registration (PRD #1910 M1, D2). A product is
// an OAuth client when it has at least one redirect URI, a scope list and a client secret; the
// panel is where an admin sets the first two and rotates the third.
//
// IA mirrors ProductEgressProfiles: a disclosure at the foot of the product card, collapsed by
// default. Everything it shows comes with the product row, so there is nothing to load on open.
// A deleted product cannot change, so the panel is read-only for it.
//
// The client secret is shown ONCE, in the rotate response (only its sha256 is stored): the panel
// keeps the plaintext in component state until the admin dismisses it, as ProductTokens does for
// a minted token. Redirect URIs are admin-written but still rendered as React text only.

import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, type Product, type ProductOAuthClient, type ProductTokenScope } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { MAX_REDIRECT_URIS, parseRedirectUriLines, redirectUrisError } from "../lib/oauthClient";
import { Alert, Badge, Button, Field, Textarea } from "./ui";

const SCOPES: { value: ProductTokenScope; label: string; hint: string }[] = [
  { value: "jobs:read", label: "Read jobs", hint: "Read job status and results." },
  { value: "jobs:run", label: "Run jobs", hint: "Start and cancel jobs." },
];

const NOT_A_CLIENT: ProductOAuthClient = {
  redirect_uris: [],
  scopes: [],
  has_secret: false,
  secret_prefix: "",
  rotated_at: null,
  is_client: false,
};

// What is still missing for the product to become a client, in the order an admin fixes it.
function missingParts(c: ProductOAuthClient): string[] {
  const out: string[] = [];
  if (c.redirect_uris.length === 0) out.push("a redirect URI");
  if (c.scopes.length === 0) out.push("a scope");
  if (!c.has_secret) out.push("a client secret");
  return out;
}

export function ProductOAuthPanel({
  product,
  onChanged,
}: {
  product: Product;
  // Called after a write so the page reloads the product list.
  onChanged: () => Promise<unknown> | void;
}) {
  const deleted = product.deleted_at !== null;
  const [open, setOpen] = useState(false);
  // oauth_client is absent from an api predating PRD #1910 (rollout skew): treat as not a client.
  const client = product.oauth_client ?? NOT_A_CLIENT;
  const missing = missingParts(client);

  // The edit form's draft; null means "show what is stored".
  const storedUris = client.redirect_uris.join("\n");
  const [draftUris, setDraftUris] = useState<string | null>(null);
  const [draftScopes, setDraftScopes] = useState<ProductTokenScope[] | null>(null);
  const uriText = draftUris ?? storedUris;
  const scopes = draftScopes ?? client.scopes;
  const uris = parseRedirectUriLines(uriText);
  const uriError = redirectUrisError(uris);
  const scopeError = uris.length > 0 && scopes.length === 0 ? "Pick at least one scope." : null;
  const dirty = draftUris !== null || draftScopes !== null;
  const canSave = dirty && uriError === null && scopeError === null;

  const [saving, setSaving] = useState(false);
  const [rotating, setRotating] = useState(false);
  const [confirmRotate, setConfirmRotate] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const secretRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (secret) secretRef.current?.focus();
  }, [secret]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!canSave || saving) return;
    setSaving(true);
    setError("");
    setNotice("");
    try {
      // Both lists are written together; clearing both stops the product being a client.
      await api.adminSetProductOAuth(product.id, uris, uris.length === 0 ? [] : scopes);
      setDraftUris(null);
      setDraftScopes(null);
      setNotice(uris.length === 0 ? "OAuth client registration cleared." : "OAuth client settings saved.");
      await onChanged();
    } catch (err) {
      setError(errorMessage(err, "Failed to save the OAuth client settings"));
    } finally {
      setSaving(false);
    }
  };

  const rotate = async () => {
    setRotating(true);
    setError("");
    setNotice("");
    try {
      const res = await api.adminRotateProductClientSecret(product.id);
      setSecret(res.client_secret);
      setCopied(false);
      setConfirmRotate(false);
      await onChanged();
    } catch (err) {
      setError(errorMessage(err, "Failed to rotate the client secret"));
    } finally {
      setRotating(false);
    }
  };

  const copy = async () => {
    if (!secret) return;
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the secret stays visible.
    }
  };

  const toggleScope = (s: ProductTokenScope, on: boolean) => {
    const base = draftScopes ?? client.scopes;
    setDraftScopes(SCOPES.map((x) => x.value).filter((v) => (v === s ? on : base.includes(v))));
  };

  return (
    <details
      className="group/oauth border-t border-edge pt-3"
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-2 gap-y-1 text-sm font-medium text-fg marker:content-none">
        <span
          aria-hidden="true"
          className="inline-block text-faint group-open/oauth:rotate-90 motion-safe:transition-transform"
        >
          ▸
        </span>
        OAuth client
        {client.is_client ? (
          <Badge tone="ok" dot>
            Client
          </Badge>
        ) : (
          <span className="text-xs font-normal text-muted">Not a client</span>
        )}
        <span className="basis-full pl-4 text-xs font-normal text-faint">
          Lets this product send users to uzi to approve access, instead of pasting a token.
        </span>
      </summary>

      {/* Rendered only while open, like the sibling panels: a collapsed card stays light. */}
      {open && (
        <div className="mt-3 space-y-3 pl-4">
          {error && <Alert message={error} />}
          {notice && <Alert tone="success" message={notice} />}

          {secret && (
            <div
              ref={secretRef}
              tabIndex={-1}
              role="status"
              className="space-y-3 rounded-xl border border-ok/40 bg-surface p-4 outline-hidden"
            >
              <p className="text-sm font-medium text-ok">New client secret for {product.name}</p>
              <p className="text-sm text-muted">
                Copy it into {product.name} now. It is shown{" "}
                <strong className="text-fg">once and never again</strong> (only its hash is stored). The
                previous secret no longer works.
              </p>
              <div className="flex items-center gap-2">
                <code className="flex-1 overflow-x-auto rounded-lg border border-edge bg-ink console px-3 py-2 font-mono text-sm text-ok">
                  {secret}
                </code>
                <Button variant="secondary" onClick={copy}>
                  {copied ? "Copied" : "Copy"}
                </Button>
              </div>
              <Button
                variant="ghost"
                onClick={() => {
                  setSecret(null);
                  setCopied(false);
                }}
              >
                Done
              </Button>
            </div>
          )}

          <p className="text-sm text-muted">
            {client.is_client
              ? "This product is an OAuth client: users can approve it from the product."
              : `Not an OAuth client yet: it still needs ${missing.join(", ")}.`}
          </p>

          <dl className="space-y-2 text-sm">
            <div>
              <dt className="text-muted">Redirect URIs</dt>
              <dd>
                {client.redirect_uris.length === 0 ? (
                  <span className="text-faint">None.</span>
                ) : (
                  <ul className="space-y-0.5">
                    {client.redirect_uris.map((u) => (
                      <li key={u}>
                        <code className="break-all font-mono text-xs text-fg">{u}</code>
                      </li>
                    ))}
                  </ul>
                )}
              </dd>
            </div>
            <div>
              <dt className="text-muted">Scopes</dt>
              <dd className="text-fg">
                {client.scopes.length === 0 ? (
                  <span className="text-faint">None.</span>
                ) : (
                  client.scopes
                    .map((s) => SCOPES.find((x) => x.value === s)?.label ?? s)
                    .join(", ")
                )}
              </dd>
            </div>
            <div>
              <dt className="text-muted">Client secret</dt>
              <dd className="text-fg">
                {client.has_secret ? (
                  <>
                    <code className="font-mono text-xs">{client.secret_prefix}…</code>
                    {client.rotated_at && (
                      <span className="text-xs text-muted">
                        {" "}
                        rotated {new Date(client.rotated_at).toLocaleString()}
                      </span>
                    )}
                  </>
                ) : (
                  <span className="text-faint">None yet.</span>
                )}
              </dd>
            </div>
          </dl>

          {deleted ? (
            <p className="text-xs text-faint">A deleted product cannot change.</p>
          ) : (
            <>
              <form onSubmit={save} className="space-y-3" aria-label={`OAuth client settings for ${product.name}`}>
                <Field label="Redirect URIs (one per line)" htmlFor={`oauth-uris-${product.id}`}>
                  <Textarea
                    id={`oauth-uris-${product.id}`}
                    rows={3}
                    spellCheck={false}
                    placeholder="https://app.example.com/uzi/callback"
                    value={uriText}
                    aria-invalid={uriError !== null || undefined}
                    aria-describedby={`oauth-uris-hint-${product.id}`}
                    onChange={(e) => setDraftUris(e.target.value)}
                  />
                  <p
                    id={`oauth-uris-hint-${product.id}`}
                    className={uriError ? "text-xs text-warn" : "text-xs text-faint"}
                  >
                    {uriError ??
                      `https only (http only for 127.0.0.1 and [::1] with a port). No fragments. Up to ${MAX_REDIRECT_URIS}; exact match.`}
                  </p>
                </Field>
                <fieldset className="space-y-1.5" disabled={saving}>
                  <legend className="mb-1.5 text-sm font-medium text-muted">Scopes it may request</legend>
                  {SCOPES.map((s) => (
                    <label key={s.value} className="flex items-start gap-2 text-sm text-fg">
                      <input
                        type="checkbox"
                        className="mt-0.5 h-4 w-4 accent-brand"
                        checked={scopes.includes(s.value)}
                        onChange={(e) => toggleScope(s.value, e.target.checked)}
                      />
                      <span>
                        {s.label} <span className="text-xs text-faint">{s.hint}</span>
                      </span>
                    </label>
                  ))}
                  {scopeError && <p className="text-xs text-warn">{scopeError}</p>}
                </fieldset>
                <div className="flex items-center gap-2">
                  <Button type="submit" size="sm" disabled={!canSave || saving}>
                    {saving ? "Saving…" : "Save OAuth settings"}
                  </Button>
                  {dirty && (
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      onClick={() => {
                        setDraftUris(null);
                        setDraftScopes(null);
                      }}
                    >
                      Discard changes
                    </Button>
                  )}
                </div>
                <p className="text-xs text-faint">Clear every URI and scope to stop this product being a client.</p>
              </form>

              <div className="space-y-2 border-t border-edge pt-3">
                {confirmRotate ? (
                  <div
                    role="group"
                    aria-label={`Confirm rotating the client secret for ${product.name}`}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2"
                  >
                    <p className="text-xs text-warn">
                      Rotate the secret for “{product.name}”? The current secret stops working at once; the
                      product needs the new one.
                    </p>
                    <div className="flex items-center gap-1.5">
                      <Button variant="danger" size="sm" disabled={rotating} onClick={() => void rotate()}>
                        {rotating ? "Rotating…" : "Rotate secret"}
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => setConfirmRotate(false)}>
                        Cancel
                      </Button>
                    </div>
                  </div>
                ) : (
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={rotating}
                    onClick={() => (client.has_secret ? setConfirmRotate(true) : void rotate())}
                  >
                    {client.has_secret ? "Rotate secret" : "Create client secret"}
                  </Button>
                )}
              </div>
            </>
          )}
        </div>
      )}
    </details>
  );
}
