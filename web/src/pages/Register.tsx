import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { ApiError } from "../lib/api";
import { scorePassword } from "../lib/passwordStrength";
import { Alert, Button, Card, Field, Input } from "../components/ui";

const MIN_PASSWORD = 12;

export function Register() {
  const { register } = useAuth();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const strength = scorePassword(password);
  const tooShort = password.length > 0 && password.length < MIN_PASSWORD;

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (password.length < MIN_PASSWORD) {
      setError(`Password must be at least ${MIN_PASSWORD} characters`);
      return;
    }
    setSubmitting(true);
    try {
      await register(email, password, displayName);
      navigate("/dashboard");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Something went wrong");
    } finally {
      setSubmitting(false);
    }
  };

  // Weak→strong ramp mapped onto the status tokens: danger (very weak/weak),
  // warn (fair), ok (good/strong). The bar width already encodes the 5 levels.
  const barColors = ["bg-danger", "bg-danger", "bg-warn", "bg-ok", "bg-ok"];

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
