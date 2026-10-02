import { Navigate } from "react-router-dom";
import type { ReactNode } from "react";
import { useAuth } from "../auth/AuthContext";

function Loading() {
  return (
    <div className="flex min-h-screen items-center justify-center text-faint">Loading…</div>
  );
}

// ServerUnreachable replaces a redirect while the session probe could not reach
// the API (e.g. a 503 during a DB outage): the visitor may well be signed in, so
// bouncing them to /login would be wrong. AuthProvider keeps retrying on a timer.
export function ServerUnreachable() {
  const { retry } = useAuth();
  return (
    <div className="flex min-h-screen flex-col items-center justify-center gap-3 text-center">
      <div role="alert" className="flex flex-col gap-3">
        <h1 className="text-lg font-medium text-fg">Can't reach the server</h1>
        <p className="text-sm text-faint">Retrying automatically…</p>
      </div>
      <button
        type="button"
        onClick={() => void retry()}
        className="rounded-lg border border-edge px-3 py-1.5 text-sm text-muted hover:bg-raised/60"
      >
        Retry now
      </button>
    </div>
  );
}

// ProtectedRoute gates a route to authenticated users.
export function ProtectedRoute({ children }: { children: ReactNode }) {
  const { user, loading, serverUnreachable } = useAuth();
  if (loading) return <Loading />;
  if (!user && serverUnreachable) return <ServerUnreachable />;
  if (!user) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

// AdminRoute gates a route to admins (implies authenticated).
export function AdminRoute({ children }: { children: ReactNode }) {
  const { user, loading, serverUnreachable } = useAuth();
  if (loading) return <Loading />;
  if (!user && serverUnreachable) return <ServerUnreachable />;
  if (!user) return <Navigate to="/login" replace />;
  if (!user.is_admin) return <Navigate to="/dashboard" replace />;
  return <>{children}</>;
}

// GuestRoute is the inverse of ProtectedRoute: the landing/login/register pages
// are for signed-out visitors, so a signed-in user is bounced to the dashboard
// instead of seeing a public page rendered inside the authenticated shell.
export function GuestRoute({ children }: { children: ReactNode }) {
  const { user, loading, serverUnreachable } = useAuth();
  if (loading) return <Loading />;
  if (!user && serverUnreachable) return <ServerUnreachable />;
  if (user) return <Navigate to="/dashboard" replace />;
  return <>{children}</>;
}
