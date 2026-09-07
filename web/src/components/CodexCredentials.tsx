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
import { DocLink } from "./DocLink";
import { DOC_CODEX_CREDENTIALS } from "../lib/doclinks";
// Every "dark" sentence on this card comes from ONE flag, so the milestone that
// enables Codex routing flips it in a single place (issue #1174 item 6).
import { CODEX_DARK_COPY } from "./codexCredentialsCopy";

// vaultLockedMessage is the shared copy for a 409 vault_locked: the global handler
// has already refreshed the session, so the unlock banner is showing above.
const VAULT_LOCKED =
  "Your vault is locked; unlock it with the banner above, then save again.";

// D6's reason, in one place: rendered as a tooltip AND as the screen-reader
// description the disabled-looking Delete points at, so the two cannot drift.
const D6_HINT =
  "Make another credential the default first; every account needs one default while any credential exists.";

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

// statusBadge maps a row's stateless codex_status to a badge tone + label + hint,
// read STRAIGHT off secret.codex_status (never a second fetch). Absent → no badge (an
// anthropic_token would omit the field; a codex row always carries one). The hint is
// rendered both as a title tooltip AND as an sr-only description (mirroring the
// Anthropic chip): it conveys what the status means and that, while the card is dark,
// nothing here is actionable yet — Codex is not used for runs.
function statusBadge(
  status?: string,
): { tone: BadgeTone; label: string; hint: string } | null {
  switch (status) {
    case "staging":
      // The one place a `staging` login says WHY it is not yet linked: this build
      // does not auto-verify (routed through the flag so retirement is one edit).
      return {
        tone: "info",
        label: "staging",
        hint:
          "Saved and encrypted; identity not yet verified. " +
          CODEX_DARK_COPY.stagingNotAutoVerified,
      };
    case "linked":
      return { tone: "ok", label: "linked", hint: "Provider identity verified." };
    case "failed":
      // Deliberately GENERIC — no provider or secret detail leaks into a diagnosis
      // string that is rendered as a tooltip and read aloud by a screen reader.
      return {
        tone: "danger",
        label: "failed",
        hint: "Verification failed. Replace the value or re-add the login.",
      };
    case "static":
      return {
        tone: "neutral",
        label: "static",
        hint: "API key stored; provider-account linking does not apply.",
      };
    default:
      return null;
  }
}

// codexShapeError is a PURE, IN-MEMORY pre-check for a codex_auth paste (issue
// #1174 item 2 / AC 2). It exists to catch the two shapes a user actually pastes
// by mistake — a raw token, and the WHOLE ~/.codex/auth.json file — and answer with
// a specific, secret-free hint BEFORE any request leaves the browser. It returns a
// message string, NEVER the pasted value, and logs/persists nothing; the server
// validator stays the real backstop. Exported because the card and its test both
// import it. NB: only a codex_auth value is JSON — an OpenAI API key is a raw
// string, so the caller must NOT run this check on the openai_api_key path.
export function codexShapeError(raw: string): string | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return "This field expects a JSON object with access_token and refresh_token, not a raw token. See “How to get this” below.";
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return "This field expects a JSON object with access_token and refresh_token. See “How to get this” below.";
  }
  const obj = parsed as Record<string, unknown>;
  const top = obj.access_token;
  if (typeof top === "string" && top.trim() !== "") return null;
  const nested = obj.tokens;
  if (
    typeof nested === "object" &&
    nested !== null &&
    typeof (nested as Record<string, unknown>).access_token === "string" &&
    ((nested as Record<string, unknown>).access_token as string).trim() !== ""
  ) {
    return "This looks like the whole ~/.codex/auth.json file. Uzi needs a flat object with just access_token and refresh_token; use the copy command in “How to get this”.";
  }
  return "This JSON has no access_token. See “How to get this” below.";
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
            {badge && (
              <>
                {/* The status word alone states a DIAGNOSIS but not its meaning or
                    that nothing is actionable yet while dark; the hint carries both,
                    as a title AND an sr-only description that the badge points at —
                    the same idiom AnthropicTokens uses for its auto chip. */}
                <Badge
                  tone={badge.tone}
                  title={badge.hint}
                  aria-describedby={`codex-status-${secret.id}`}
                >
                  {badge.label}
                </Badge>
                <span id={`codex-status-${secret.id}`} className="sr-only">
                  {badge.hint}
                </span>
              </>
            )}
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
  // The wrong-shape pre-check message (issue #1174 item 2 / AC 2) lives in LOCAL
  // state and renders INLINE beneath this field, deliberately NOT on the shared
  // onError banner: a paste-shape mistake is local to this field, and the shared
  // banner is a surface where a value-adjacent message must never land. Cleared on
  // every keystroke so a corrected paste is not shadowed by a stale message.
  const [shapeError, setShapeError] = useState<string | null>(null);
  // Only a Codex login is JSON; an OpenAI API key is a raw string. This gate decides
  // both the inline help and whether the JSON shape check runs at all.
  const isCodexLogin = kind === "codex_auth";

  // The Name field UNMOUNTS when the card collapses to first-credential mode, so a
  // label typed and never submitted would silently name the next credential through
  // a field the user cannot see. Clearing both fields on the transition is the fix;
  // the shape message is cleared too so it cannot outlive the value that caused it.
  useEffect(() => {
    setToken("");
    setLabel("");
    setShapeError(null);
  }, [first]);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    // Wrong-shape pre-check, for a codex_auth value ONLY. Runs in memory, echoes
    // nothing, and on a bad shape shows the message inline and SENDS NOTHING — the
    // server validator remains the backstop. An OpenAI API key skips this entirely.
    if (isCodexLogin) {
      const shape = codexShapeError(token);
      if (shape !== null) {
        setShapeError(shape);
        return;
      }
    }
    setShapeError(null);
    setAddBusy(true);
    try {
      // A user's FIRST codex credential is forced default server-side whatever we
      // send, and the Name field does not exist in that mode, so send the literal
      // "default" rather than reading a field that is not on screen.
      await apiForKind(kind).create(token, first ? "default" : label.trim(), false);
      setToken("");
      setLabel("");
      // Name the resulting lifecycle state (issue #1174 item 5): a login is born
      // `staging` (not verified), a key is born `static`. The dark clause comes
      // from the one flag so it retires everywhere at once.
      onNotice(
        isCodexLogin
          ? "Saved and encrypted. Status: staging (not verified). " +
              CODEX_DARK_COPY.notUsedForRuns
          : "Saved and encrypted. Status: static. " + CODEX_DARK_COPY.notUsedForRuns,
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
      <Field label={first ? noun : `Add ${noun}`}>
        <Input
          type="password"
          autoComplete="off"
          placeholder={
            kind === "openai_api_key"
              ? "Paste your OpenAI API key"
              : "Paste your Codex login JSON"
          }
          value={token}
          // A keystroke means the value changed, so a stale shape message (which
          // described the OLD value) must go — never persisted, never re-checked.
          onChange={(e) => {
            setToken(e.target.value);
            if (shapeError) setShapeError(null);
          }}
        />
      </Field>
      {isCodexLogin && (
        // Codex-login help lives OUTSIDE the <Field> (which is a wrapping <label>)
        // so the DocLink and the disclosure are not swallowed into the input's
        // accessible name. The whole block is codex_auth-only: the OpenAI key field
        // stays a bare raw-string paste.
        <div className="space-y-2 text-xs">
          {/* What this field IS, so a user does not paste an API key here: it
              imports an EXISTING Codex CLI / ChatGPT-subscription login — a JSON
              object with access_token (+ refresh_token for renewal). */}
          <p className="text-muted">
            This imports an existing Codex CLI / ChatGPT-subscription login — a JSON
            object with{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">access_token</code>{" "}
            (and{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">refresh_token</code>{" "}
            for renewal). It is <strong className="text-fg">not</strong> an OpenAI
            API key.
          </p>
          {shapeError && (
            // The pre-check verdict, inline and value-free. Its text ends with
            // See "How to get this" below. — pointing at the disclosure just below.
            <p role="alert" className="text-danger">
              {shapeError}
            </p>
          )}
          {/* Native <details>/<summary>: keyboard, touch and screen-reader reachable
              with no JS. Summarizes the copy recipe; the full guide is the DocLink. */}
          <details className="group">
            <summary className="cursor-pointer list-none text-muted marker:content-none">
              <span aria-hidden="true" className="text-faint group-open:hidden">
                ▸{" "}
              </span>
              <span aria-hidden="true" className="hidden text-faint group-open:inline">
                ▾{" "}
              </span>
              How to get this
            </summary>
            <div className="mt-2 space-y-2 text-muted">
              <p>
                First sign in with{" "}
                <code className="rounded bg-raised px-1 py-0.5 text-fg">codex login</code>
                , then confirm it with{" "}
                <code className="rounded bg-raised px-1 py-0.5 text-fg">
                  codex login status
                </code>
                .
              </p>
              <p>Then copy JUST the two fields uzi needs, never the whole file:</p>
              <p>
                <code className="block overflow-x-auto rounded bg-raised px-2 py-1 text-fg">
                  {
                    "jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' ~/.codex/auth.json | pbcopy"
                  }
                </code>
              </p>
              <p>
                On Linux swap{" "}
                <code className="rounded bg-raised px-1 py-0.5 text-fg">pbcopy</code>{" "}
                for{" "}
                <code className="rounded bg-raised px-1 py-0.5 text-fg">
                  xclip -selection clipboard
                </code>{" "}
                or <code className="rounded bg-raised px-1 py-0.5 text-fg">wl-copy</code>
                .
              </p>
              <p className="text-faint">
                This is a renewable credential — do not commit it, log it, paste it
                into issues, or otherwise share it.{" "}
                <DocLink slug={DOC_CODEX_CREDENTIALS}>Full guide</DocLink>.
              </p>
            </div>
          </details>
        </div>
      )}
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
  // Same in-memory, value-free wrong-shape guard as the add form, for a codex_auth
  // rotation (issue #1174 item 2 / AC 2). Inline, never the shared banner.
  const [rotateShapeError, setRotateShapeError] = useState<string | null>(null);

  const first = secrets.length === 0;
  const anyBusy = busy || rotateBusy;

  const rotate = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    const row = secrets.find((s) => s.id === rotateFor);
    if (!row) return;
    // A codex_auth rotation is JSON and gets the same pre-check as the add form; an
    // OpenAI key is a raw string and skips it. On a bad shape: show inline, send
    // nothing. The check runs in memory and never carries the pasted value.
    if (row.kind === "codex_auth") {
      const shape = codexShapeError(rotateValue);
      if (shape !== null) {
        setRotateShapeError(shape);
        return;
      }
    }
    setRotateShapeError(null);
    setRotateBusy(true);
    try {
      await apiForKind(row.kind).patch(rotateFor, { token: rotateValue });
      setRotateValue("");
      setRotateFor("");
      onNotice("Credential value replaced and re-sealed.");
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
          Store your OpenAI Codex logins and API keys. {CODEX_DARK_COPY.notUsedForRuns}{" "}
          Paste a Codex login or an OpenAI API key, and give each one a name. A single{" "}
          <strong className="text-fg">default</strong> is shared across both kinds.
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

      {/* A visible, keyboard/touch/screen-reader-reachable legend for the four
          status badges (issue #1174 item 3). The per-badge title + sr-only text
          answers "what does THIS badge mean" in place; this answers "what can a
          badge mean" in one surface a keyboard or touch user can actually open. It
          rides the same native <details>/<summary> idiom as RunUsage's breakdown,
          so <details> conveys expanded state and the ▸/▾ is decorative only. */}
      {!loading && !first && (
        <details className="group">
          <summary className="cursor-pointer list-none text-xs text-muted marker:content-none">
            <span aria-hidden="true" className="text-faint group-open:hidden">
              ▸{" "}
            </span>
            <span aria-hidden="true" className="hidden text-faint group-open:inline">
              ▾{" "}
            </span>
            What do these statuses mean?
          </summary>
          <dl className="mt-2 space-y-2 text-xs text-muted">
            <div>
              <dt className="font-medium text-fg">staging</dt>
              <dd>
                Saved and encrypted, but the provider identity has not been verified.{" "}
                {CODEX_DARK_COPY.stagingNotAutoVerified}
              </dd>
            </div>
            <div>
              <dt className="font-medium text-fg">linked</dt>
              <dd>The provider identity has been verified.</dd>
            </div>
            <div>
              <dt className="font-medium text-fg">failed</dt>
              <dd>
                Verification failed. Replace the value or re-add the login. No
                provider or secret details are shown.
              </dd>
            </div>
            <div>
              <dt className="font-medium text-fg">static</dt>
              <dd>
                An API key is stored. Provider-account linking does not apply to API
                keys.
              </dd>
            </div>
          </dl>
          <p className="mt-2 text-xs text-faint">{CODEX_DARK_COPY.notUsedForRuns}</p>
        </details>
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
              // Switching the target row invalidates any shape message the previous
              // row's paste produced.
              onChange={(e) => {
                setRotateFor(e.target.value);
                setRotateShapeError(null);
              }}
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
                  onChange={(e) => {
                    setRotateValue(e.target.value);
                    if (rotateShapeError) setRotateShapeError(null);
                  }}
                />
              </Field>
              {rotateShapeError && (
                // A codex_auth rotation with the wrong shape: inline, value-free,
                // and pointing at the guide (only ever set for a codex_auth row).
                <p role="alert" className="text-xs text-danger">
                  {rotateShapeError}{" "}
                  <DocLink slug={DOC_CODEX_CREDENTIALS}>How to get this</DocLink>.
                </p>
              )}
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
