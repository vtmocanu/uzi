// /connect — the consent page of uzi's OAuth authorization server (PRD #1910 M2, D3 and D4).
// A product sends the user's browser to /api/oauth/authorize, which validates the request,
// binds it to this browser with a cookie and redirects here with ?request=<id>. The user signs
// in if needed (the return path survives both a password and an OIDC login), sees WHO is asking
// and WHAT it may do, and approves or denies.
//
// Three rules this page keeps on purpose:
//   1. The product's name and description are admin-written strings, rendered as React text
//      nodes only: never Markdown, never dangerouslySetInnerHTML, never a link.
//   2. The page navigates to exactly one URL: the redirect_url the server returns from approve
//      or deny, built from the product's registered redirect URI. Nothing in this page's own
//      query string (a redirect_uri, a next, a state) is ever read or navigated to.
//   3. Approve and Deny are plain buttons, not a form submit: the web tier's CSP form-action
//      'self' would block a form navigation to the product's origin, and a script-driven
//      window.location.assign is not subject to it.

import { useEffect, useState } from "react";
import { Navigate, useLocation, useSearchParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { ServerUnreachable } from "../components/RouteGuards";
import { api, ApiError, type OAuthAuthorizeRequest } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { clearPendingReturn, setPendingReturn } from "../lib/pendingReturn";
import { scopeText } from "../lib/oauthClient";
import { Alert, Button, Card, Skeleton } from "../components/ui";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";

// The fixed disclosure of D4, shown on every consent exactly as written.
const AUTOMATION_NOTICE =
  "This product may run jobs automatically on your behalf, as you, using your model credential configured in uzi.";

// A fetched request that is not pending cannot be decided. Map each status to the line shown
// instead of the buttons.
const TERMINAL_STATUS_MESSAGE: Record<Exclude<OAuthAuthorizeRequest["status"], "pending">, string> = {
  approved: "This request was already approved. Return to the product you came from.",
  redeemed: "This request was already completed. Return to the product you came from.",
  denied: "This request was denied. No access was given. Start again from the product if that was a mistake.",
  superseded: "This request was replaced by a newer one. Return to the product and start again.",
};

const EXPIRED_OR_FOREIGN =
  "This request has expired, was already used, or was started in a different browser. Start the connection again from the product.";

const MISSING_REQUEST = Symbol("missing-request");

export function Connect() {
  const demo = useDemoMode();
  const { user, loading: authLoading, serverUnreachable } = useAuth();
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const requestId = searchParams.get("request");

  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState("");

  // FIRST, on every load: drop any stored pending-return entry, whatever brought the user here
  // (a password login returned through ?next=, an OIDC login through AppShell). The entry is
  // for crossing a login only; leaving it would let AppShell navigate here a second time.
  useEffect(() => {
    clearPendingReturn();
  }, []);

  // Signed out: remember this page for a login that cannot carry ?next= (OIDC lands on "/").
  // An unreachable server is not "signed out": the visitor may be signed in, so neither
  // redirect nor remember the return path until the session probe answers.
  const unreachable = !authLoading && !user && serverUnreachable;
  const signedOut = !authLoading && !user && !serverUnreachable;
  useEffect(() => {
    if (signedOut && requestId) {
      setPendingReturn(`/connect?request=${encodeURIComponent(requestId)}`);
    }
  }, [signedOut, requestId]);

  const {
    data: meta,
    loading,
    error: loadError,
  } = useAsyncData<OAuthAuthorizeRequest>(
    async () => {
      if (!requestId) throw MISSING_REQUEST;
      return api.getOAuthRequest(requestId);
    },
    [requestId],
    {
      enabled: !authLoading && !!user,
      mapError: (err) =>
        err === MISSING_REQUEST
          ? "This link is missing its request. Start the connection again from the product."
          : err instanceof ApiError && err.status === 404
            ? EXPIRED_OR_FOREIGN
            : errorMessage(err, "Failed to load the request."),
    },
  );

  if (unreachable) return <ServerUnreachable />;
  if (signedOut) {
    const next = encodeURIComponent(location.pathname + location.search);
    return <Navigate to={`/login?next=${next}`} replace />;
  }

  // decide runs approve or deny and navigates to the server-built URL it returns.
  const decide = async (action: "approve" | "deny") => {
    if (!requestId) return;
    setFormError("");
    setBusy(true);
    try {
      const resp =
        action === "approve"
          ? await api.approveOAuthRequest(requestId)
          : await api.denyOAuthRequest(requestId);
      window.location.assign(resp.redirect_url);
      // Stay busy: the browser is leaving. Re-enabling the buttons here would invite a second
      // click on a request that is already decided.
    } catch (err) {
      setBusy(false);
      setFormError(decideErrorMessage(err, action));
    }
  };

  const status = meta?.status;
  const terminal = status && status !== "pending" ? TERMINAL_STATUS_MESSAGE[status] : null;

  return (
    <div className="mx-auto max-w-md">
      <Card className="space-y-5">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Connect a product to uzi</h1>
          <p className="mt-1 text-sm text-muted">
            A product is asking for access to your uzi account. Check who is asking and what it may
            do before you approve.
          </p>
        </div>

        {authLoading || loading ? (
          <div className="space-y-3">
            <Skeleton className="h-5 w-2/3" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : loadError ? (
          <Alert message={loadError} />
        ) : terminal ? (
          <Alert tone="info" message={terminal} />
        ) : (
          meta && (
            <>
              {/* Every value below is untrusted display text: rendered as plain text nodes,
                  never as a link, Markdown or HTML. */}
              <dl className="divide-y divide-edge rounded-lg border border-edge bg-raised/40 text-sm">
                <div className="flex justify-between gap-3 px-3 py-2">
                  <dt className="text-muted">Product</dt>
                  <dd className="min-w-0 break-words text-right font-medium text-fg">{meta.product_name}</dd>
                </div>
                {meta.product_description !== "" && (
                  <div className="flex justify-between gap-3 px-3 py-2">
                    <dt className="text-muted">About</dt>
                    <dd className="min-w-0 break-words text-right text-fg">{meta.product_description}</dd>
                  </div>
                )}
                <div className="flex justify-between gap-3 px-3 py-2">
                  <dt className="text-muted">Returns you to</dt>
                  <dd className="min-w-0 break-all text-right text-fg">{meta.redirect_host}</dd>
                </div>
                <div className="flex justify-between gap-3 px-3 py-2">
                  <dt className="text-muted">Signed in as</dt>
                  <dd className="min-w-0 truncate text-fg">{maskEmail(user?.email, demo)}</dd>
                </div>
                <div className="flex justify-between gap-3 px-3 py-2">
                  <dt className="text-muted">Expires</dt>
                  <dd className="text-fg">{new Date(meta.expires_at).toLocaleTimeString()}</dd>
                </div>
              </dl>

              <div>
                <h2 className="text-sm font-medium text-fg">This product will be able to</h2>
                <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-fg">
                  {meta.scopes.map((scope) => (
                    <li key={scope}>{scopeText(scope)}</li>
                  ))}
                </ul>
              </div>

              <p className="rounded-lg border border-edge-strong bg-raised/60 px-3 py-2 text-sm text-fg">
                {AUTOMATION_NOTICE}
              </p>

              {formError && <Alert message={formError} />}

              <div className="flex items-center gap-2">
                <Button type="button" disabled={busy} onClick={() => void decide("approve")}>
                  {busy ? "Working…" : "Approve"}
                </Button>
                <Button type="button" variant="secondary" disabled={busy} onClick={() => void decide("deny")}>
                  Deny
                </Button>
              </div>
              <p className="text-xs text-faint">
                You can disconnect this product at any time from your uzi settings.
              </p>
            </>
          )
        )}
      </Card>
    </div>
  );
}

// decideErrorMessage maps the approve / deny status codes to one clear line. 404 is the
// server's single answer for an unknown, expired or other-browser request.
function decideErrorMessage(err: unknown, action: "approve" | "deny"): string {
  if (err instanceof ApiError) {
    switch (err.status) {
      case 404:
        return EXPIRED_OR_FOREIGN;
      case 409:
        return "This request was already decided, or the product can no longer be connected. Start again from the product.";
      default:
        return err.message;
    }
  }
  return action === "approve" ? "Failed to approve the request." : "Failed to deny the request.";
}
