import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, type AuthConfig } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { emailDomainAllowed } from "../lib/emailDomain";
import { useDemoMode } from "../lib/demoMode";
import { maskDomains } from "../lib/demoMask";
import { scorePassword } from "../lib/passwordStrength";
import { Alert, Button, Card, Field, Input, Skeleton } from "../components/ui";

const MIN_PASSWORD = 12;

export function Register() {
  const { register } = useAuth();
  const demo = useDemoMode();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [config, setConfig] = useState<AuthConfig | null>(null);

  // Load the registration policy so the form can hide itself when registration is
  // off, or hint the allowed domains. On a fetch failure we fall open (permissive
  // defaults): the server stays authoritative, so a transient config blip must not
  // wall off a legitimate signup.
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

  const strength = scorePassword(password);
  const tooShort = password.length > 0 && password.length < MIN_PASSWORD;
  const domains = config?.allowed_email_domains ?? [];
  const domainRestricted = domains.length > 0;

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (password.length < MIN_PASSWORD) {
      setError(`Password must be at least ${MIN_PASSWORD} characters`);
      return;
    }
    // Client-side pre-validation of the domain allowlist. The server enforces the
    // same rule authoritatively; this just avoids a round-trip and a late 403.
    if (domainRestricted && !emailDomainAllowed(email, domains)) {
      setError(`Registration is restricted to: ${maskDomains(domains.join(", "), demo)}`);
      return;
    }
    setSubmitting(true);
    try {
      await register(email, password, displayName);
      navigate("/dashboard");
    } catch (err) {
      setError(errorMessage(err, "Something went wrong"));
    } finally {
      setSubmitting(false);
    }
  };

  // Weak→strong ramp mapped onto the status tokens: danger (very weak/weak),
  // warn (fair), ok (good/strong). The bar width already encodes the 5 levels.
  const barColors = ["bg-danger", "bg-danger", "bg-warn", "bg-ok", "bg-ok"];

  // Still loading the policy: keep the card shape stable rather than flashing a
  // form we might immediately replace with the disabled notice.
  if (config === null) {
    return (
      <div className="mx-auto max-w-md">
        <Card>
          <Skeleton className="h-8 w-2/3" />
          <div className="mt-6 space-y-4">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        </Card>
      </div>
    );
  }

  // SSO-only mode: no password accounts can be created here (the server 403s), so
  // point people at single sign-on instead (PRD #45). Checked before the generic
  // registration-disabled notice because it is the more specific reason.
  if (config.password_login_enabled === false) {
    return (
      <div className="mx-auto max-w-md">
        <Card>
          <h1 className="text-2xl font-semibold">Sign-up is via single sign-on</h1>
          <p className="mt-4 text-sm text-muted">
            This instance uses single sign-on — your account is created automatically the first
            time you sign in with your identity provider. There is no password to set here.
          </p>
          <p className="mt-4 text-sm text-muted">
            <Link to="/login" className="text-brand hover:underline">
              Go to log in
            </Link>
          </p>
        </Card>
      </div>
    );
  }

  if (!config.registration_enabled) {
    return (
      <div className="mx-auto max-w-md">
        <Card>
          <h1 className="text-2xl font-semibold">Registration is disabled</h1>
          <p className="mt-4 text-sm text-muted">
            New accounts are not being accepted right now. If you already have an account,
            you can still log in.
          </p>
          <p className="mt-4 text-sm text-muted">
            <Link to="/login" className="text-brand hover:underline">
              Go to log in
            </Link>
          </p>
        </Card>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-md">
      <Card>
        <h1 className="text-2xl font-semibold">Create your account</h1>
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
            {domainRestricted && (
              <p className="mt-1 text-xs text-muted">
                Only {maskDomains(domains.join(", "), demo)} addresses may register.
              </p>
            )}
          </Field>
          <Field label="Display name (optional)">
            <Input
              type="text"
              autoComplete="name"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
            />
          </Field>
          <Field label="Password">
            <Input
              type="password"
              autoComplete="new-password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </Field>
          {password.length > 0 && (
            <div className="space-y-1">
              <div className="h-1.5 w-full overflow-hidden rounded bg-raised">
                <div
                  className={`h-full transition-all ${barColors[strength.score]}`}
                  style={{ width: `${((strength.score + 1) / 5) * 100}%` }}
                />
              </div>
              <p className={`text-xs ${tooShort ? "text-danger" : "text-muted"}`}>
                {tooShort ? `At least ${MIN_PASSWORD} characters` : `Strength: ${strength.label}`}
              </p>
            </div>
          )}
          <Button type="submit" className="w-full" disabled={submitting || tooShort}>
            {submitting ? "Creating…" : "Create account"}
          </Button>
        </form>
        <p className="mt-4 text-sm text-muted">
          Already have an account?{" "}
          <Link to="/login" className="text-brand hover:underline">
            Log in
          </Link>
        </p>
      </Card>
    </div>
  );
}
