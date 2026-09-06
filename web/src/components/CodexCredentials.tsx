// Settings → OpenAI / Codex credentials (PRD #1147 M3). The Codex sibling of the
// Anthropic token card, deliberately SIMPLER: two kinds share one card and one
// default, and none of the Anthropic-only machinery applies here.
//
// Two kinds live side by side and share a SINGLE default across both:
//   - codex_auth      — a Codex login, born "staging"; a resolver later moves it to
//                        "linked" or "failed".
//   - openai_api_key  — a static OpenAI Console key, born "static".
//
// The value is never displayed, never returned by the API, and never re-shown after
// saving — rotation is a re-paste (a "Replace value" form, not an edit-in-place).
//
// What this card deliberately does NOT carry (no Codex analog — see PRD #1147):
// the auto-selection pool toggle, the sidebar-rail checkbox, the rate-limit chip
// machinery, and the judge-lane binding. A Codex-only credential set also must NOT
// make a run startable — that gate stays Anthropic-only (lib/hasToken.ts).

import { useEffect, useState, type FormEvent } from "react";
import { api, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { isVaultLocked } from "../lib/api";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Badge, Button, Card, Field, Input, SectionTitle, Skeleton } from "./ui";
import type { BadgeTone } from "./ui";

// vaultLockedMessage is the shared copy for a 409 vault_locked: the global handler
// has already refreshed the session, so the unlock banner is showing above.
const VAULT_LOCKED =
  "Your vault is locked — unlock it with the banner above, then save again.";

// D6's reason, in one place: rendered as a tooltip AND as the screen-reader
// description the disabled-looking Delete points at, so the two cannot drift.
const D6_HINT =
  "Make another credential the default first — every account needs one default while any credential exists.";

function errText(err: unknown, fallback: string): string {
  if (isVaultLocked(err)) return VAULT_LOCKED;
  return errorMessage(err, fallback);
}

// The two Codex kinds. Their API triplets are picked per-row by the row's kind, so
// rename/rotate/set-default/delete hit the correct route without a second lookup.
type CodexKind = "codex_auth" | "openai_api_key";

function kindLabel(kind: string): string {
  return kind === "openai_api_key" ? "OpenAI API key" : "Codex login";
}

// apiForKind picks the create/patch/delete wrappers for a kind. There is no shared
// route: codex_auth and openai_api_key each have their own three endpoints.
function apiForKind(kind: string) {
  return kind === "openai_api_key"
    ? {
        create: api.createOpenAIApiKey,
        patch: api.patchOpenAIApiKey,
        del: api.deleteOpenAIApiKeyById,
      }
    : {
        create: api.createCodexAuth,
        patch: api.patchCodexAuth,
        del: api.deleteCodexAuthById,
      };
}

// statusBadge maps a row's stateless codex_status to a badge tone + label, read
// STRAIGHT off secret.codex_status (never a second fetch). Absent → no badge (an
// anthropic_token would omit the field; a codex row always carries one).
function statusBadge(status?: string): { tone: BadgeTone; label: string } | null {
  switch (status) {
    case "staging":
      return { tone: "info", label: "staging" };
    case "linked":
      return { tone: "ok", label: "linked" };
    case "failed":
      return { tone: "danger", label: "failed" };
    case "static":
      return { tone: "neutral", label: "static" };
    default:
      return null;
  }
}

// codexDeleteWarning mirrors the Anthropic D5 delete confirmation, minus the
// auto-eligible pool clause (no Codex pool) and the worker/judge naming (this card
// carries no such bindings). The default is reachable here only as the LAST
// credential (D6 blocks deleting a default while others exist), so that branch is
// the disconnect-my-account case.
export function codexDeleteWarning(label: string, isDefault: boolean): string {
  if (isDefault) {
    return `Delete “${label}”? This is your last Codex credential — uzi will no longer be connected to your OpenAI account.`;
  }
  return `Delete “${label}”? Removing it changes nothing else — your default Codex credential is unaffected.`;
}

// CredentialRow is one stored Codex credential: its label, the kind, the stateless
// status badge, the default badge, and the per-row actions. Rename and set-default
// are inline; value rotation opens the shared form below (a value is pasted, never
// edited).
function CredentialRow({
  secret,
  busy,
  soleCredential,
  onChanged,
  onError,
  onNotice,
}: {
  secret: SecretMeta;
  busy: boolean;
  soleCredential: boolean;
  onChanged: () => Promise<void>;
  onError: (m: string) => void;
  onNotice: (m: string) => void;
}) {
  const [renaming, setRenaming] = useState(false);
  const [label, setLabel] = useState(secret.label);
  const [rowBusy, setRowBusy] = useState(false);
  const disabled = busy || rowBusy;
  const blockedByD6 = secret.is_default && !soleCredential;
  const d6HintId = `codex-d6-${secret.id}`;
  const kindApi = apiForKind(secret.kind);
  const badge = statusBadge(secret.codex_status);

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
      () => kindApi.patch(secret.id, { label: next }),
      `Renamed to “${next}”.`,
      "Failed to rename the credential",
    );
    setRenaming(false);
  };

  return (
    <div
      className="rounded-lg border border-edge bg-raised/60 px-4 py-3"
      data-testid={`codex-${secret.id}`}
    >
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
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            {/* The label is user-authored and reaches this renderer without
                necessarily having passed the server validator (rows written before
                it landed are never re-validated); sanitizeLabel neutralizes a bidi
                override that React escaping does not touch. */}
            <span className="truncate font-medium text-fg">
              {sanitizeLabel(secret.label)}
            </span>
            <Badge tone="neutral">{kindLabel(secret.kind)}</Badge>
            {badge && <Badge tone={badge.tone}>{badge.label}</Badge>}
            {secret.is_default && <Badge tone="ok">default</Badge>}
          </div>
        )}
        {!renaming && (
          <div className="flex items-center gap-2">
            <Button
              variant="ghost"
              size="sm"
              disabled={disabled}
              onClick={() => setRenaming(true)}
            >
              Rename
            </Button>
            {!secret.is_default && (
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled}
                onClick={() =>
                  run(
                    () => kindApi.patch(secret.id, { default: true }),
                    `“${secret.label}” is now your default Codex credential.`,
                    "Failed to set the default credential",
                  )
                }
              >
                Make default
              </Button>
            )}
            <Button
              variant="danger"
              size="sm"
              // The default cannot be deleted while other credentials exist (D6). It
              // is aria-disabled rather than `disabled` so a keyboard / screen-reader
              // user still reaches it and hears why (aria-describedby), and the click
              // is refused here rather than by a server 409 that says only "it
              // failed", not "promote another first".
              disabled={disabled}
              aria-disabled={blockedByD6 || undefined}
              aria-describedby={blockedByD6 ? d6HintId : undefined}
              title={blockedByD6 ? D6_HINT : undefined}
              className={blockedByD6 ? "cursor-not-allowed opacity-50" : undefined}
              onClick={() => {
                if (blockedByD6) return;
                if (
                  !window.confirm(
                    codexDeleteWarning(sanitizeLabel(secret.label), secret.is_default),
                  )
                )
                  return;
                void run(
                  () => kindApi.del(secret.id),
                  `Deleted “${secret.label}”.`,
                  "Failed to delete the credential",
                );
              }}
            >
              Delete
            </Button>
            {blockedByD6 && (
              <span id={d6HintId} className="sr-only">
                {D6_HINT}
              </span>
            )}
          </div>
        )}
      </div>
      <div className="mt-1 text-xs text-faint">
        updated {new Date(secret.updated_at).toLocaleString()}
      </div>
    </div>
  );
}

// AddCredentialForm is one "add a credential of kind K" form. Two are rendered (a
// Codex login and an OpenAI API key). When the user holds NO codex credential yet
// the Name field collapses (first-credential force-default UX, like the Anthropic
// card): the server force-defaults it and labels it "default", so a Name field
// would offer a choice that does not exist.
function AddCredentialForm({
  kind,
  first,
  busy,
  onError,
  onNotice,
  reload,
}: {
  kind: CodexKind;
  first: boolean;
  busy: boolean;
  onError: (m: string) => void;
  onNotice: (m: string) => void;
  reload: () => Promise<void>;
}) {
  const [token, setToken] = useState("");
  const [label, setLabel] = useState("");
  const [addBusy, setAddBusy] = useState(false);

  // The Name field UNMOUNTS when the card collapses to first-credential mode, so a
  // label typed and never submitted would silently name the next credential through
  // a field the user cannot see. Clearing both fields on the transition is the fix.
  useEffect(() => {
    setToken("");
    setLabel("");
  }, [first]);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    setAddBusy(true);
    try {
      // A user's FIRST codex credential is forced default server-side whatever we
      // send, and the Name field does not exist in that mode, so send the literal
      // "default" rather than reading a field that is not on screen.
      await apiForKind(kind).create(token, first ? "default" : label.trim(), false);
      setToken("");
      setLabel("");
      onNotice(
        "Credential saved. It is sealed with your login password and validated on the first agent run.",
      );
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to save credential"));
    } finally {
      setAddBusy(false);
    }
  };

  const anyBusy = busy || addBusy;
  const noun = kindLabel(kind);

  return (
    <form onSubmit={add} className="space-y-3 border-t border-edge pt-5">
      <Field label={first ? noun : `Add a ${noun}`}>
        <Input
          type="password"
          autoComplete="off"
          placeholder={
            kind === "openai_api_key"
              ? "Paste your OpenAI API key"
              : "Paste your Codex login token"
          }
          value={token}
          onChange={(e) => setToken(e.target.value)}
        />
      </Field>
      {!first && (
        <Field label="Name">
          <Input
            placeholder={kind === "openai_api_key" ? "console-key" : "codex-login"}
            value={label}
            aria-label={`${noun} name`}
            onChange={(e) => setLabel(e.target.value)}
          />
        </Field>
      )}
      <Button
        type="submit"
        disabled={anyBusy || token.trim() === "" || (!first && label.trim() === "")}
      >
        {first ? `Save ${noun}` : `Add ${noun}`}
      </Button>
    </form>
  );
}

// CodexCredentials is the whole card: the list, the two add forms, and the rotate
// form. `secrets` and `reload` are owned by Settings so this card and the Anthropic
// one refresh from the one fetch.
export function CodexCredentials({
  secrets,
  loading,
  busy,
  reload,
  onError,
  onNotice,
}: {
  secrets: SecretMeta[];
  loading: boolean;
  busy: boolean;
  reload: () => Promise<void>;
  onError: (m: string) => void;
  onNotice: (m: string) => void;
}) {
  const [rotateFor, setRotateFor] = useState("");
  const [rotateValue, setRotateValue] = useState("");
  const [rotateBusy, setRotateBusy] = useState(false);

  const first = secrets.length === 0;
  const anyBusy = busy || rotateBusy;

  const rotate = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    const row = secrets.find((s) => s.id === rotateFor);
    if (!row) return;
    setRotateBusy(true);
    try {
      await apiForKind(row.kind).patch(rotateFor, { token: rotateValue });
      setRotateValue("");
      setRotateFor("");
      onNotice("Credential value replaced. The new value is used on the next agent run.");
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to replace the credential value"));
    } finally {
      setRotateBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>OpenAI / Codex credentials</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Run your agents on OpenAI Codex instead of, or alongside, Anthropic. Paste a
          Codex login token or an OpenAI API key, and give each one a name. A single{" "}
          <strong className="text-fg">default</strong> is shared across both kinds. A
          Codex credential on its own does not make a run startable — an Anthropic token
          is still required to start a run.
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
            <CredentialRow
              key={s.id}
              secret={s}
              busy={anyBusy}
              soleCredential={secrets.length === 1}
              onChanged={reload}
              onError={onError}
              onNotice={onNotice}
            />
          ))}
        </div>
      )}

      {/* Rotating a value is a separate form because the value is pasted, never
          edited: the API never returns it, so there is nothing to pre-fill. The
          route is chosen from the selected row's kind. */}
      {!loading && !first && (
        <div className="space-y-3">
          <Field label="Replace a credential's value">
            <select
              aria-label="Credential to replace"
              className="h-9 w-full rounded-md border border-edge bg-surface px-2 text-sm text-fg"
              value={rotateFor}
              onChange={(e) => setRotateFor(e.target.value)}
            >
              <option value="">Select a credential…</option>
              {secrets.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.label} — {kindLabel(s.kind)}
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
                  placeholder="Paste the replacement credential"
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

      {!loading && (
        <>
          <AddCredentialForm
            kind="codex_auth"
            first={first}
            busy={anyBusy}
            onError={onError}
            onNotice={onNotice}
            reload={reload}
          />
          <AddCredentialForm
            kind="openai_api_key"
            first={first}
            busy={anyBusy}
            onError={onError}
            onNotice={onNotice}
            reload={reload}
          />
          <p className="text-xs text-faint">
            Encrypted with your login password. If you forget your password these
            credentials cannot be recovered and must be re-entered.
          </p>
        </>
      )}
    </Card>
  );
}
