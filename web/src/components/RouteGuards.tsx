import { Navigate } from "react-router-dom";
import type { ReactNode } from "react";
import { useAuth } from "../auth/AuthContext";

function Loading() {
  return (
    <div className="flex min-h-screen items-center justify-center text-faint">Loading…</div>
  );
}

// ProtectedRoute gates a route to authenticated users.
export function ProtectedRoute({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth();
  if (loading) return <Loading />;
  if (!user) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

// AdminRoute gates a route to admins (implies authenticated).
export function AdminRoute({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth();
  if (loading) return <Loading />;
  if (!user) return <Navigate to="/login" replace />;
  if (!user.is_admin) return <Navigate to="/dashboard" replace />;
  return <>{children}</>;
}

// GuestRoute is the inverse of ProtectedRoute: the landing/login/register pages
// are for signed-out visitors, so a signed-in user is bounced to the dashboard
// instead of seeing a public page rendered inside the authenticated shell.
export function GuestRoute({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth();
  if (loading) return <Loading />;
  if (user) return <Navigate to="/dashboard" replace />;
  return <>{children}</>;
}
