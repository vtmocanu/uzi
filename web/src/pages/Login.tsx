import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { ApiError, MOCK_MODE } from "../lib/api";
import { Alert, Button, Card, Field, Input } from "../components/ui";

export function Login() {
  const { login } = useAuth();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setSubmitting(true);
    try {
      await login(email, password);
      navigate("/dashboard");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Something went wrong");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="mx-auto max-w-md">
      <Card>
        <h1 className="text-2xl font-semibold">Log in</h1>
        {MOCK_MODE && (
          <p className="mt-2 text-sm text-brand">
            Demo mode — any email and password sign you in as the admin.
          </p>
        )}
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
        <p className="mt-4 text-sm text-muted">
          No account?{" "}
          <Link to="/register" className="text-brand hover:underline">
            Register
          </Link>
        </p>
      </Card>
    </div>
  );
}
