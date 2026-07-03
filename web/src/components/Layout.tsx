import { Link, useNavigate } from "react-router-dom";
import type { ReactNode } from "react";
import { useAuth } from "../auth/AuthContext";
import { Button } from "./ui";

export function Layout({ children }: { children: ReactNode }) {
  const { user, logout } = useAuth();
  const navigate = useNavigate();

  const handleLogout = async () => {
    await logout();
    navigate("/login");
  };

  return (
    <div className="min-h-screen">
      <header className="border-b border-slate-800">
        <div className="mx-auto flex max-w-5xl items-center justify-between px-4 py-3">
          <Link to={user ? "/dashboard" : "/"} className="text-lg font-semibold tracking-tight">
            uzi<span className="text-indigo-400"> · dark factory</span>
          </Link>
          <nav className="flex items-center gap-3 text-sm">
            {user ? (
              <>
                <Link to="/dashboard" className="text-slate-300 hover:text-white">
                  Dashboard
                </Link>
                {user.is_admin && (
                  <Link to="/admin/users" className="text-slate-300 hover:text-white">
                    Users
                  </Link>
                )}
                <Link to="/settings" className="text-slate-300 hover:text-white">
                  Settings
                </Link>
                <span className="text-slate-500">{user.email}</span>
                <Button variant="ghost" onClick={handleLogout}>
                  Log out
                </Button>
              </>
            ) : (
              <>
                <Link to="/login" className="text-slate-300 hover:text-white">
                  Log in
                </Link>
                <Link to="/register" className="text-slate-300 hover:text-white">
                  Register
                </Link>
              </>
            )}
          </nav>
        </div>
      </header>
      <main className="mx-auto max-w-5xl px-4 py-10">{children}</main>
    </div>
  );
}
