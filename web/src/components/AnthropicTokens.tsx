// Settings → Anthropic tokens: the token LIST (PRD #104 M2/M6). A user may hold
// several named credentials, exactly one of which is the default that unbound
// workers and the judge lane spend.
//
// The value is never displayed, never returned by the API, and never re-shown
// after saving — rotation is a re-paste, which is why "Replace value" is a form
// and not an edit-in-place field.

import { useState, type FormEvent } from "react";
import { api, ApiError, type SecretMeta } from "../lib/api";
import { isVaultLocked } from "../lib/api";
import { Badge, Button, Card, Field, Input, SectionTitle, Skeleton } from "./ui";

const DOC_URL =
  "https://gitlab.example.com/vtmocanu/uzi/-/blob/main/docs/anthropic-token.md";

// vaultLockedMessage is the shared copy for a 409 vault_locked: the global handler
// has already refreshed the session, so the unlock banner is showing above.
const VAULT_LOCKED = "Your vault is locked — unlock it with the banner above, then save again.";

function errText(err: unknown, fallback: string): string {
  if (isVaultLocked(err)) return VAULT_LOCKED;
  return err instanceof ApiError ? err.message : fallback;
}

// TokenRow is one stored credential: its label, the default badge, when it was
// last updated, and the per-row actions. Rename and set-default are inline; the
// value rotation opens the shared form below (a value is pasted, never edited).
function TokenRow({
  secret,
  busy,
  soleToken,
  onChanged,
  onError,
  onNotice,
}: {
  secret: SecretMeta;
  busy: boolean;
  soleToken: boolean;
  onChanged: () => Promise<void>;
  onError: (m: string) => void;
  onNotice: (m: string) => void;
}) {
  const [renaming, setRenaming] = useState(false);
  const [label, setLabel] = useState(secret.label);
  const [rowBusy, setRowBusy] = useState(false);
  const disabled = busy || rowBusy;

  const run = async (fn: () => Promise<unknown>, ok: string, fallback: string) => {
    onError("");
    onNotice("");
    setRowBusy(true);
    try {
      await fn();
      onNotice(ok);
      await onChanged();
    } catch (err) {
      onError(errText(err, fallback));
    } finally {
      setRowBusy(false);
    }
  };

  const rename = async (e: FormEvent) => {
    e.preventDefault();
    const next = label.trim();
    if (next === "" || next === secret.label) {
      setRenaming(false);
      return;
    }
    await run(
      () => api.patchAnthropicToken(secret.id, { label: next }),
      `Renamed to “${next}”.`,
      "Failed to rename the token",
    );
    setRenaming(false);
  };

  return (
    <div className="rounded-lg border border-edge bg-raised/60 px-4 py-3" data-testid={`token-${secret.id}`}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        {renaming ? (
          <form onSubmit={rename} className="flex items-center gap-2">
            <Input
              autoFocus
              value={label}
              aria-label={`Rename ${secret.label}`}
              onChange={(e) => setLabel(e.target.value)}
              className="h-8 w-48"
            />
            <Button type="submit" size="sm" disabled={disabled}>
              Save
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => {
                setLabel(secret.label);
                setRenaming(false);
              }}
            >
              Cancel
            </Button>
          </form>
        ) : (
          <div className="flex min-w-0 items-center gap-2">
            <span className="truncate font-medium text-fg">{secret.label}</span>
            {secret.is_default && <Badge tone="ok">default</Badge>}
          </div>
        )}
        {!renaming && (
          <div className="flex items-center gap-2">
            <Button variant="ghost" size="sm" disabled={disabled} onClick={() => setRenaming(true)}>
              Rename
            </Button>
            {!secret.is_default && (
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled}
                onClick={() =>
                  run(
                    () => api.patchAnthropicToken(secret.id, { default: true }),
                    `“${secret.label}” is now your default token.`,
                    "Failed to set the default token",
                  )
                }
              >
                Make default
              </Button>
            )}
            <Button
              variant="danger"
              size="sm"
              // The default cannot be deleted while other tokens exist (D6) —
              // disable rather than let the server 409, and say why on hover.
              disabled={disabled || (secret.is_default && !soleToken)}
              title={
                secret.is_default && !soleToken
                  ? "Make another token the default first — every account needs one default while any token exists."
                  : undefined
              }
              onClick={() => {
                // Deleting a bound token silently returns its workers to the
                // default (D5), so the confirmation must SAY so rather than let a
                // user discover it from a meter.
                const warning = secret.is_default
                  ? `Delete “${secret.label}”? This is your last token — uzi will no longer be connected to your Anthropic account.`
                  : `Delete “${secret.label}”? Any worker or judge setting bound to it falls back to your default token.`;
                if (!window.confirm(warning)) return;
                void run(
                  () => api.deleteAnthropicTokenById(secret.id),
                  `Deleted “${secret.label}”.`,
                  "Failed to delete the token",
                );
              }}
            >
              Delete
            </Button>
          </div>
        )}
      </div>
      <div className="mt-1 text-xs text-faint">
        updated {new Date(secret.updated_at).toLocaleString()}
      </div>
    </div>
  );
}

// AnthropicTokens is the whole card: the list, the add form, and the rotate form.
// `secrets` and `reload` are owned by Settings so the rate-limit card and this one
// refresh together after a change.
export function AnthropicTokens({
  secrets,
  loading,
  busy,
  reload,
  onError,
  onNotice,
  error,
}: {
  secrets: SecretMeta[];
  loading: boolean;
  busy: boolean;
  reload: () => Promise<void>;
  onError: (m: string) => void;
  onNotice: (m: string) => void;
  error: string;
}) {
  const [token, setToken] = useState("");
  const [label, setLabel] = useState("");
  const [addBusy, setAddBusy] = useState(false);
  const [rotateFor, setRotateFor] = useState("");
  const [rotateValue, setRotateValue] = useState("");

  const first = secrets.length === 0;

  const add = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    setAddBusy(true);
    try {
      // A user's FIRST token is forced default server-side whatever we send, so
      // the form does not offer the choice until there is something to choose
      // between.
      await api.createAnthropicToken(token, label.trim() || "default", false);
      setToken("");
      setLabel("");
      onNotice("Token saved. It is sealed with your login password and validated on the first agent run.");
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to save token"));
    } finally {
      setAddBusy(false);
    }
  };

  const rotate = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    setAddBusy(true);
    try {
      await api.patchAnthropicToken(rotateFor, { token: rotateValue });
      setRotateValue("");
      setRotateFor("");
      onNotice("Token value replaced. The new value is used on the next agent run.");
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to replace the token value"));
    } finally {
      setAddBusy(false);
    }
  };

  const anyBusy = busy || addBusy;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Anthropic tokens</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          uzi runs your agents with your own Anthropic credentials. Paste an OAuth token from{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">claude setup-token</code> or a
          Console API key. Give each one a name so you can point individual workers at it. The{" "}
          <strong className="text-fg">default</strong> is what every unbound worker spends.{" "}
          <a href={DOC_URL} target="_blank" rel="noreferrer" className="text-brand hover:text-brand-hover">
            How to obtain a token
          </a>
          .
        </p>
      </div>

      {loading ? (
        <Skeleton className="h-16 w-full" />
      ) : first ? (
        <div className="rounded-lg border border-edge bg-raised/60 px-4 py-3 text-sm">
          <Badge tone="neutral">Not set</Badge>
        </div>
      ) : (
        <div className="space-y-2">
          {secrets.map((s) => (
            <TokenRow
              key={s.id}
              secret={s}
              busy={anyBusy}
              soleToken={secrets.length === 1}
              onChanged={reload}
              onError={onError}
              onNotice={onNotice}
            />
          ))}
        </div>
      )}

      {/* Rotating a value is a separate form because the value is pasted, never
          edited: the API never returns it, so there is nothing to pre-fill. */}
      {!loading && !first && (
        <div className="space-y-3">
          <Field label="Replace a token's value">
            <select
              aria-label="Token to replace"
              className="h-9 w-full rounded-md border border-edge bg-surface px-2 text-sm text-fg"
              value={rotateFor}
              onChange={(e) => setRotateFor(e.target.value)}
            >
              <option value="">Select a token…</option>
              {secrets.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.label}
                  {s.is_default ? " (default)" : ""}
                </option>
              ))}
            </select>
          </Field>
          {rotateFor !== "" && (
            <form onSubmit={rotate} className="space-y-3">
              <Field label="New value">
                <Input
                  type="password"
                  autoComplete="off"
                  placeholder="Paste the replacement token"
                  value={rotateValue}
                  onChange={(e) => setRotateValue(e.target.value)}
                />
              </Field>
              <Button type="submit" disabled={anyBusy || rotateValue.trim() === ""}>
                Replace value
              </Button>
            </form>
          )}
        </div>
      )}

      <form onSubmit={add} className="space-y-3 border-t border-edge pt-5">
        <Field label={first ? "Token" : "Add another token"}>
          <Input
            type="password"
            autoComplete="off"
            placeholder="Paste your Anthropic token"
            value={token}
            onChange={(e) => setToken(e.target.value)}
          />
        </Field>
        {!first && (
          <Field label="Name">
            <Input
              placeholder="console-key"
              value={label}
              aria-label="Token name"
              onChange={(e) => setLabel(e.target.value)}
            />
          </Field>
        )}
        <p className="text-xs text-faint">
          Encrypted with your login password. If you forget your password this token cannot be
          recovered and must be re-entered.
        </p>
        <Button type="submit" disabled={anyBusy || token.trim() === "" || (!first && label.trim() === "")}>
          {first ? "Save token" : "Add token"}
        </Button>
        {error !== "" && <span className="sr-only">{error}</span>}
      </form>
    </Card>
  );
}
