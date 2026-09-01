import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, MOCK_MODE, type AuthConfig } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Alert, Button, Card, Field, Input, Skeleton } from "../components/ui";

// Enumerated OIDC callback error codes (PRD #45, Decision 9). The callback only
// ever redirects with one of these known codes; the SPA switches on them and never
// renders the raw value, and an unknown code falls back to a generic message.
const OIDC_ERROR_MESSAGES: Record<string, string> = {
  oidc_state: "Your sign-in session expired or was invalid. Please try signing in again.",
  oidc_exchange: "We couldn't complete sign-in with your identity provider. Please try again.",
  oidc_forbidden: "Your account isn't permitted to sign in here. Contact your administrator.",
  oidc_deactivated: "Your account has been deactivated. Contact your administrator.",
  oidc_error: "Something went wrong during sign-in. Please try again.",
};

function oidcErrorMessage(code: string | null): string | null {
  if (!code) return null;
  return OIDC_ERROR_MESSAGES[code] ?? "Sign-in failed. Please try again.";
}

// safeNextPath returns a same-origin internal path to return to after login, or
// "/dashboard" for anything unsafe. It admits only a rooted path ("/…") that is
// NOT protocol-relative ("//host") and contains NO backslash: this feeds
// react-router navigate() today (client-side, so "/\evil.com" is inert), but
// window.location normalizes "\"→"/", so the day a refactor points it at
// window.location.assign, "/\evil.com" would become a live open redirect. Reject
// the backslash here so that refactor can't silently reopen the hole.
//
// INVARIANT (per-call-site, not global): this guard covers ONLY the returnTo
// sink below (navigate(returnTo)). It is not a global open-redirect guard. Every
// OTHER navigate() of a value derived from the URL or the server must protect its
// own dynamic path segment — the sinks that interpolate a server id wrap it in
// encodeURIComponent(...) so a future non-UUID id (slug, name, forge identifier)
// cannot inject a "/", "//", or "\" path segment. A new navigate() sink is NOT
// covered by anything here; encode its dynamic segment at the call site.
export function safeNextPath(next: string | null): string {
  if (next && next.startsWith("/") && !next.startsWith("//") && !next.includes("\\")) {
    return next;
  }
  return "/dashboard";
}

// ssoButtonClass matches the secondary Button variant, but as an anchor: the SSO
// button is a full-page navigation to the API redirect endpoint (a server 302 to the
// IdP), NOT a fetch, so it must be a real link.
const ssoButtonClass =
  "inline-flex w-full select-none items-center justify-center rounded-lg border border-edge bg-raised px-4 py-2 text-sm font-medium text-fg transition-colors hover:border-edge-strong hover:bg-raised/70";

export function Login() {
  const { login } = useAuth();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [config, setConfig] = useState<AuthConfig | null>(null);

  // Load the auth policy so the page can show the SSO button and hide the password
  // form in SSO-only mode. Fall open (password on / OIDC off) on a fetch failure —
  // the server stays authoritative, so a transient blip must not wall off login.
  useEffect(() => {
    let live = true;
    api
      .authConfig()
      .then((c) => live && setConfig(c))
      .catch(() => live && setConfig({ registration_enabled: true, allowed_email_domains: [] }));
    return () => {
      live = false;
    };
  }, []);

  const oidcError = oidcErrorMessage(searchParams.get("error"));

  // Where to land after login. Defaults to /dashboard; a ?next= (set by the CLI
  // consent page so it is returned to after logging in) overrides it, but only for
  // a same-origin internal path — see safeNextPath (rejects absolute,
  // protocol-relative, and backslash vectors) so it can never become an open
  // redirect.
  const returnTo = safeNextPath(searchParams.get("next"));

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setSubmitting(true);
    try {
      await login(email, password);
      navigate(returnTo);
    } catch (err) {
      setError(errorMessage(err, "Something went wrong"));
    } finally {
      setSubmitting(false);
    }
  };

  // Keep the card shape stable while the policy loads rather than flashing a form we
  // might immediately hide.
  if (config === null) {
    return (
      <div className="mx-auto max-w-md">
        <Card>
          <Skeleton className="h-8 w-1/3" />
          <div className="mt-6 space-y-4">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        </Card>
      </div>
    );
  }

  // Only false disables the password form (older server / absent field reads as on).
  const passwordEnabled = config.password_login_enabled !== false;
  const oidcEnabled = config.oidc_enabled === true;
  const providerName = config.oidc_provider_name || "SSO";

  return (
    <div className="mx-auto max-w-md">
      <Card>
        <h1 className="text-2xl font-semibold">Log in</h1>
        {MOCK_MODE && passwordEnabled && (
          <p className="mt-2 text-sm text-brand">
            Demo mode — any email and password sign you in as the admin.
          </p>
        )}
        {oidcError && (
          <div className="mt-4">
            <Alert message={oidcError} />
          </div>
        )}

        {passwordEnabled && (
          <form className="mt-6 space-y-4" onSubmit={onSubmit}>
            {error && <Alert message={error} />}
            <Field label="Email">
              <Input
                type="email"
                autoComplete="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </Field>
            <Field label="Password">
              <Input
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </Field>
            <Button type="submit" className="w-full" disabled={submitting}>
              {submitting ? "Logging in…" : "Log in"}
            </Button>
          </form>
        )}

        {oidcEnabled && (
          <div className="mt-6">
            {passwordEnabled && (
              <div className="mb-4 flex items-center gap-3 text-xs text-muted">
                <span className="h-px flex-1 bg-edge" />
                or
                <span className="h-px flex-1 bg-edge" />
              </div>
            )}
            {!passwordEnabled && (
              <p className="mb-4 text-sm text-muted">
                This instance uses single sign-on. Continue with your identity provider to log in.
              </p>
            )}
            <a href="/api/auth/oidc/login" className={ssoButtonClass}>
              Sign in with {providerName}
            </a>
          </div>
        )}

        {passwordEnabled && (
          <p className="mt-4 text-sm text-muted">
            No account?{" "}
            <Link to="/register" className="text-brand hover:underline">
              Register
            </Link>
          </p>
        )}
      </Card>
    </div>
  );
}
