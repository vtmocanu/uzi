// /cli-auth — the browser-brokered `uzi login` consent screen (PRD #64 M5/M6).
// The CLI opens this page with ?request=<id>. The human must be logged in (that
// login is the whole point — it happens here, on the way to the request read),
// then TYPE the user_code shown in THEIR terminal, pick a scope, and Approve or
// Deny. Typing the code (never pre-filling it) is the anti-async-phishing
// property: the server withholds the code precisely so this page cannot auto-fill
// it and the human must confirm the tab belongs to their own invocation.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Navigate, useLocation, useSearchParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  ApiError,
  type CliAuthRequestMeta,
  type CliAuthStatus,
  type CliTokenScope,
} from "../lib/api";
import { Alert, Button, Card, Field, Input, Skeleton } from "../components/ui";
import { ScopePicker } from "../components/ScopePicker";

// A non-pending fetched request is terminal — there is nothing to approve. Map
// each status to the line the page shows instead of the consent form.
const TERMINAL_STATUS_MESSAGE: Record<Exclude<CliAuthStatus, "pending">, string> = {
  approved: "This login was already approved. Return to your terminal — it should be signed in.",
  denied: "This login was denied. If that wasn’t you, no token was issued. Start a new one from your terminal.",
  consumed: "This login was already completed. Return to your terminal — it should be signed in.",
  expired: "This login request has expired. Run uzi login again in your terminal to start a new one.",
};

export function CliAuth() {
  const { user, loading: authLoading } = useAuth();
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const requestId = searchParams.get("request");

  const [meta, setMeta] = useState<CliAuthRequestMeta | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");

  const [code, setCode] = useState("");
  const [scope, setScope] = useState<CliTokenScope>("user");
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState("");
  // The terminal outcome after an approve/deny action, or the fetched terminal
  // status — either way the consent form is replaced by a done message.
  const [outcome, setOutcome] = useState<{ tone: "success" | "info"; message: string } | null>(null);

  const isAdmin = user?.is_admin ?? false;

  const load = useCallback(async () => {
    if (!requestId) {
      setLoadError("This link is missing its login request. Run uzi login again in your terminal.");
      setLoading(false);
      return;
    }
    try {
      const m = await api.getCliAuthRequest(requestId);
      setMeta(m);
      if (m.status !== "pending") {
        setOutcome({ tone: "info", message: TERMINAL_STATUS_MESSAGE[m.status] });
      }
    } catch (err) {
      setLoadError(
        err instanceof ApiError && err.status === 404
          ? "This login request was not found. It may have already been used or expired."
          : err instanceof ApiError
            ? err.message
            : "Failed to load the login request.",
      );
    } finally {
      setLoading(false);
    }
  }, [requestId]);

  // Load only once the auth state is known and the user is signed in — otherwise
  // the Navigate below sends them to log in first (the read is cookie-only).
  useEffect(() => {
    if (authLoading || !user) return;
    load();
  }, [authLoading, user, load]);

  // Not signed in → send to login, preserving the full URL so the consent page is
  // returned to after authenticating (password OR OIDC). The request id rides the
  // query, so nothing is lost.
  if (!authLoading && !user) {
    const next = encodeURIComponent(location.pathname + location.search);
    return <Navigate to={`/login?next=${next}`} replace />;
  }

  const approve = async (e: FormEvent) => {
    e.preventDefault();
    if (!requestId) return;
    setFormError("");
    setBusy(true);
    try {
      const chosen: CliTokenScope = isAdmin ? scope : "user";
      await api.approveCliAuth(requestId, code, chosen);
      setOutcome({
        tone: "success",
        message: "Approved. Return to your terminal — the uzi CLI is now signed in.",
      });
    } catch (err) {
      setFormError(approveErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const deny = async () => {
    if (!requestId) return;
    setFormError("");
    setBusy(true);
    try {
      await api.denyCliAuth(requestId);
      setOutcome({ tone: "info", message: "Denied. No token was issued." });
    } catch (err) {
      setFormError(
        err instanceof ApiError && err.status === 409
          ? "This request is no longer pending, so there is nothing to deny."
          : err instanceof ApiError
            ? err.message
            : "Failed to deny the request.",
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto max-w-md">
      <Card className="space-y-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Approve CLI login</h1>
          <p className="mt-1 text-sm text-muted">
            A device is asking to sign in to uzi as you from the command line.
          </p>
        </div>

        {authLoading || loading ? (
          <div className="space-y-3">
            <Skeleton className="h-5 w-2/3" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : loadError ? (
          <Alert message={loadError} />
        ) : outcome ? (
          <Alert tone={outcome.tone} message={outcome.message} />
        ) : (
          meta && (
            <>
              {/* client_desc is the requesting host/OS the CLI self-reported —
                  untrusted display text, so it is rendered as plain text, never a
                  link. */}
              <dl className="divide-y divide-edge rounded-lg border border-edge bg-raised/40 text-sm">
                <div className="flex justify-between gap-3 px-3 py-2">
                  <dt className="text-muted">Requesting device</dt>
                  {/* title so the full value is readable when truncated on a
                      narrow viewport (plain text — still safe, never a link). */}
                  <dd className="min-w-0 truncate text-fg" title={meta.client_desc}>
                    {meta.client_desc}
                  </dd>
                </div>
                <div className="flex justify-between gap-3 px-3 py-2">
                  <dt className="text-muted">Signed in as</dt>
                  <dd className="min-w-0 truncate text-fg">{user?.email}</dd>
                </div>
                <div className="flex justify-between gap-3 px-3 py-2">
                  <dt className="text-muted">Expires</dt>
                  <dd className="text-fg">{new Date(meta.expires_at).toLocaleTimeString()}</dd>
                </div>
              </dl>

              <form onSubmit={approve} className="space-y-4">
                {formError && <Alert message={formError} />}
                <Field label="Enter the code shown in your terminal" htmlFor="cli-auth-code">
                  {/* NOT pre-filled and NOT fetched from the server — the human
                      types it from their own terminal. That is the anti-phishing
                      confirmation the whole flow rests on. */}
                  <Input
                    id="cli-auth-code"
                    placeholder="XXXX-XXXX"
                    autoComplete="off"
                    autoFocus
                    value={code}
                    onChange={(e) => setCode(e.target.value)}
                  />
                </Field>

                {/* Scope picker: admin-only, exactly like the static mint (same
                    shared component owns the gate). A non-admin can only grant a
                    'user'-scoped login. */}
                <ScopePicker admin={isAdmin} value={scope} onChange={setScope} id="cli-auth-scope" />

                <p className="text-xs text-faint">
                  Only approve this if you just ran{" "}
                  <code className="rounded bg-raised px-1 py-0.5 text-fg">uzi login</code> yourself and
                  the code above matches the one in that terminal.
                </p>

                <div className="flex items-center gap-2">
                  <Button type="submit" disabled={busy || code.trim() === ""}>
                    {busy ? "Approving…" : "Approve"}
                  </Button>
                  <Button type="button" variant="secondary" disabled={busy} onClick={deny}>
                    Deny
                  </Button>
                </div>
              </form>
            </>
          )
        )}
      </Card>
    </div>
  );
}

// approveErrorMessage maps the approve endpoint's status codes to a clear line.
// 400 code-mismatch is recoverable (the form stays up to retry); the rest are
// terminal states the CLI-side flow reflects.
function approveErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.status) {
      case 400:
        return "The code you entered does not match the one in your terminal. Check it and try again.";
      case 403:
        return "Your account is not an administrator, so it can’t grant admin read access. Choose the User scope instead.";
      case 404:
        return "This login request was not found. It may have already been used or expired.";
      case 409:
        return "This request is no longer pending — it may have already been approved or denied.";
      case 410:
        return "This login request has expired. Run uzi login again in your terminal to start a new one.";
      default:
        return err.message;
    }
  }
  return "Failed to approve the request.";
}
