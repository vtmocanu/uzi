// /reset — redeem an admin-issued account reset link (PRD #3989).
//
// The link is `/reset#<token>`. The token lives in the URL FRAGMENT so it never
// reaches an access log or a Referer (D9). This page reads it once, immediately
// strips the fragment from the address bar with history.replaceState, and from then
// on sends it only in POST bodies (previewReset / redeemReset), never a URL.
//
// Public route: the holder has no working credential, so nothing here reads the
// session. Redemption does not sign anyone in (D11); the done state points at /login.

import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type ResetLinkKind, type ResetLinkPreview } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Button, Card, Field, Input, Skeleton } from "../components/ui";

const MIN_PASSWORD = 12;

// One message for every invalid cause (unknown, used, expired, superseded, missing):
// the server answers one byte-identical 404, so the page must not guess which.
const INVALID_LINK_MESSAGE = "This link is invalid or has expired. Ask an administrator for a new one.";

const REDEEM_UNCONFIRMED =
  "The reset could not be confirmed; try signing in with the new password, or ask for a new link";

// The fetcher throws this when the page was opened with no fragment at all.
const MISSING_TOKEN = Symbol("missing-token");

// readFragmentToken returns the token in the current URL fragment ("" when absent).
function readFragmentToken(): string {
  return window.location.hash.replace(/^#/, "").trim();
}

// clearFragment drops the fragment from the address bar without a navigation,
// keeping the path, the query and the router's history state.
function clearFragment(): void {
  if (!window.location.hash) return;
  const { pathname, search } = window.location;
  window.history.replaceState(window.history.state, "", pathname + search);
}

// handoffToken carries the token from a mounted page to its replacement when the
// page REMOUNTS after the fragment is gone: AppShell swaps its public shell for the
// signed-in one once the session probe resolves, which unmounts this page and mounts
// a fresh one. The new instance renders before the old one's cleanup runs, so it can
// read the token here; the cleanup then clears it, and the new instance's effect sets
// it again. Memory only: nothing persists past a reload or an unmount with no successor.
let handoffToken = "";

// Whether this instance allows password sign-in, from the public auth config (the same
// GET /api/auth/config the login page reads). "unknown" while it loads or when the fetch
// fails: the page then uses wording that claims neither mode. A server that omits the
// field allows password sign-in, as on the login page.
type PasswordLogin = "on" | "off" | "unknown";

function usePasswordLogin(): PasswordLogin {
  const [mode, setMode] = useState<PasswordLogin>("unknown");
  useEffect(() => {
    let live = true;
    api
      .authConfig()
      .then((c) => live && setMode(c.password_login_enabled === false ? "off" : "on"))
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, []);
  return mode;
}

export function ResetAccount() {
  const passwordLogin = usePasswordLogin();
  // Read once, BEFORE the effect below clears the fragment. A useState initializer
  // also survives React's dev double-mount, which keeps the instance's state.
  const [token] = useState(() => readFragmentToken() || handoffToken);
  useEffect(() => {
    handoffToken = token;
    clearFragment();
    return () => {
      if (handoffToken === token) handoffToken = "";
    };
  }, [token]);

  // The link turned out invalid at redeem time (used or expired since preview).
  const [invalidAtRedeem, setInvalidAtRedeem] = useState(false);
  const [done, setDone] = useState<ResetLinkKind | null>(null);

  const {
    data: preview,
    loading,
    error: loadError,
    reload,
  } = useAsyncData<ResetLinkPreview>(
    async () => {
      if (!token) throw MISSING_TOKEN;
      return api.previewReset(token);
    },
    [token],
    {
      mapError: (err) =>
        err === MISSING_TOKEN || (err instanceof ApiError && err.status === 404)
          ? INVALID_LINK_MESSAGE
          : errorMessage(err, "Could not check this link. Check your connection and try again."),
    },
  );

  const invalid = invalidAtRedeem || loadError === INVALID_LINK_MESSAGE;

  return (
    <div className="mx-auto max-w-xl">
      <Card className="space-y-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Reset your account</h1>
          {!done && !invalid && preview && (
            <p className="mt-1 text-sm text-muted">
              An administrator issued this link for you. It works once, until{" "}
              {new Date(preview.expires_at).toLocaleString()}.
            </p>
          )}
        </div>

        {loading ? (
          <div className="space-y-3" aria-busy="true">
            <Skeleton className="h-5 w-2/3" />
            <Skeleton className="h-24 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : invalid ? (
          <InvalidLink />
        ) : loadError ? (
          <div className="space-y-3">
            <Alert message={loadError} />
            <Button variant="secondary" onClick={() => void reload()}>
              Try again
            </Button>
          </div>
        ) : done ? (
          <ResetDone kind={done} passwordLogin={passwordLogin} />
        ) : (
          preview && (
            <ResetForm
              token={token}
              kind={preview.kind}
              passwordLogin={passwordLogin}
              onDone={() => setDone(preview.kind)}
              onInvalid={() => setInvalidAtRedeem(true)}
            />
          )
        )}
      </Card>
    </div>
  );
}

function InvalidLink() {
  return (
    <div className="space-y-4">
      <Alert message={INVALID_LINK_MESSAGE} />
      <p className="text-sm text-muted">
        Reset links work once and expire after an hour. A newer link for the same account also cancels this one.
      </p>
      <Link to="/login" className="text-sm text-brand hover:underline">
        Go to sign in
      </Link>
    </div>
  );
}

// ResetLedger is the page's one deliberate element: the reset's boundary, drawn as two
// columns. What goes is ruled in danger; what stays is quiet. A user deciding whether to
// press the button needs exactly this split, so it is information, not decoration.
function ResetLedger() {
  return (
    <div className="grid gap-3 sm:grid-cols-2">
      <section aria-labelledby="reset-deleted" className="rounded-lg border border-danger/40 bg-danger/5 p-4">
        <h2 id="reset-deleted" className="text-sm font-semibold text-danger">
          Deleted permanently
        </h2>
        <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-fg">
          <li>Model credentials: Anthropic and OpenAI tokens</li>
          <li>Codex logins</li>
          <li>Everything in your vault</li>
        </ul>
      </section>
      <section aria-labelledby="reset-kept" className="rounded-lg border border-edge bg-raised/40 p-4">
        <h2 id="reset-kept" className="text-sm font-semibold text-muted">
          Kept as it is
        </h2>
        <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-muted">
          <li>Repos, boards and schedules</li>
          <li>Forge connections</li>
          <li>Run history and settings</li>
          <li>CLI tokens</li>
        </ul>
      </section>
    </div>
  );
}

function ResetForm({
  token,
  kind,
  passwordLogin,
  onDone,
  onInvalid,
}: {
  token: string;
  kind: ResetLinkKind;
  passwordLogin: PasswordLogin;
  onDone: () => void;
  onInvalid: () => void;
}) {
  const ids = useId();
  const passwordId = `${ids}-password`;
  const confirmId = `${ids}-confirm`;
  const ackId = `${ids}-ack`;
  const hintId = `${ids}-hint`;
  const needsPassword = kind === "password";

  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (!acknowledged) return;
    if (needsPassword) {
      if (password.length < MIN_PASSWORD) {
        setError(`Use at least ${MIN_PASSWORD} characters for the new password.`);
        return;
      }
      if (password !== confirm) {
        setError("The two passwords do not match.");
        return;
      }
    }
    setBusy(true);
    try {
      await api.redeemReset(token, needsPassword ? password : undefined);
      onDone();
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        onInvalid();
        return;
      }
      setError(
        err instanceof ApiError && err.status === 503
          ? err.message || REDEEM_UNCONFIRMED
          : errorMessage(err, "The reset failed. Check your connection and try again."),
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-5" noValidate>
      <ResetLedger />

      <ul className="space-y-1.5 text-sm text-muted">
        <li>
          Every browser session ends. A CLI login you already approved but have not finished is refused; one still
          waiting for approval stays pending, but your old sessions can no longer approve it.
        </li>
        <li>Runs pinned to a deleted credential use your default credential once you add a new one.</li>
        <li>Queued Codex runs will fail. Start new ones after you add a Codex login again.</li>
        {!needsPassword && (
          <li>
            Your account has no password, so this link asks for none. Your next single sign-on sign-in asks you to set a
            new vault passphrase.
          </li>
        )}
      </ul>

      {needsPassword && (
        <div className="space-y-4">
          <Field label="New password" htmlFor={passwordId}>
            <Input
              id={passwordId}
              type="password"
              autoComplete="new-password"
              aria-describedby={hintId}
              value={password}
              disabled={busy}
              onChange={(e) => setPassword(e.target.value)}
            />
          </Field>
          <Field label="Confirm new password" htmlFor={confirmId}>
            <Input
              id={confirmId}
              type="password"
              autoComplete="new-password"
              value={confirm}
              disabled={busy}
              onChange={(e) => setConfirm(e.target.value)}
            />
          </Field>
          <div id={hintId} className="space-y-1 text-xs text-faint">
            <p>At least {MIN_PASSWORD} characters.</p>
            {passwordLogin === "off" && (
              <p>
                This instance signs in with single sign-on only, so the web app cannot create your fresh vault yet. uzi
                creates it at your first password sign-in, once an administrator turns password sign-in on. Until then
                you can still sign in with single sign-on, but you cannot store new model credentials.
              </p>
            )}
            {passwordLogin === "unknown" && (
              <p>
                If your instance signs in with single sign-on only, the web app cannot create your fresh vault until an
                administrator turns password sign-in on; uzi creates it at that first password sign-in. Until then you
                can sign in with single sign-on, but you cannot store new model credentials.
              </p>
            )}
          </div>
        </div>
      )}

      <label htmlFor={ackId} className="flex items-start gap-2.5 text-sm text-fg">
        <input
          id={ackId}
          type="checkbox"
          className="mt-0.5 h-4 w-4 shrink-0 accent-brand"
          checked={acknowledged}
          disabled={busy}
          onChange={(e) => setAcknowledged(e.target.checked)}
        />
        I understand my credentials will be deleted
      </label>

      {error && <Alert message={error} />}

      <Button type="submit" variant="dangerSolid" className="w-full" disabled={busy || !acknowledged}>
        {busy ? "Resetting…" : needsPassword ? "Reset and set new password" : "Reset"}
      </Button>
    </form>
  );
}

function doneMessage(kind: ResetLinkKind, passwordLogin: PasswordLogin): string {
  if (kind !== "password") {
    return "Your vault is reset. Sign in with single sign-on; uzi then asks you to set a new vault passphrase. Once it is set, add your model credentials again in Settings.";
  }
  switch (passwordLogin) {
    case "on":
      return "Your account is reset. Sign in with your new password; uzi creates a fresh vault when you do.";
    case "off":
      return "Your account is reset. This instance uses single sign-on only, so the web app cannot create your fresh vault yet; uzi creates it at your first password sign-in, once an administrator turns password sign-in on. Until then you can sign in with single sign-on, but you cannot store new model credentials.";
    default:
      // Config still loading or unavailable: name both paths, claim neither.
      return "Your account is reset. If your instance allows password sign-in, sign in with your new password; uzi creates your fresh vault then. If it uses single sign-on only, the web app cannot create your fresh vault until an administrator turns password sign-in on. Until then you can sign in with single sign-on, but you cannot store new model credentials.";
  }
}

function ResetDone({ kind, passwordLogin }: { kind: ResetLinkKind; passwordLogin: PasswordLogin }) {
  const sso = kind !== "password" || passwordLogin === "off";
  // Move focus to the outcome so a keyboard or screen-reader user lands on it once the
  // form it replaced is gone.
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current?.focus();
  }, []);
  return (
    <div ref={ref} tabIndex={-1} className="space-y-4 outline-hidden">
      <Alert tone="success" message={doneMessage(kind, passwordLogin)} />
      {kind === "password" && passwordLogin !== "off" && (
        <p className="text-sm text-muted">Add your model credentials again in Settings once you are signed in.</p>
      )}
      <Link
        to="/login"
        className="inline-flex h-9 select-none items-center justify-center rounded-lg bg-brand px-4 text-sm font-medium text-on-brand hover:bg-brand-hover"
      >
        {sso ? "Sign in with SSO" : "Sign in"}
      </Link>
    </div>
  );
}
