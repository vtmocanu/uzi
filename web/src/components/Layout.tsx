import { Link, useLocation, useNavigate } from "react-router-dom";
import { useEffect, useState, type ReactNode } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, type Repo } from "../lib/api";
import { Button } from "./ui";

export function Layout({ children }: { children: ReactNode }) {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [repos, setRepos] = useState<Repo[]>([]);

  // Keep the sidebar picker in sync with the user's enabled repos. Re-fetched
  // on navigation so enabling/disabling a repo is reflected without a reload.
  useEffect(() => {
    if (!user) {
      setRepos([]);
      return;
    }
    api
      .listRepos()
      .then(({ repos }) => setRepos(repos))
      .catch(() => setRepos([]));
  }, [user, location.pathname]);

  const handleLogout = async () => {
    await logout();
    navigate("/login");
  };

  const currentRepoId = location.pathname.match(/^\/repos\/([^/]+)\/board/)?.[1] ?? "";

  return (
    <div className="min-h-screen">
      <header className="border-b border-slate-800">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center justify-between gap-3 px-4 py-3">
          <Link to={user ? "/dashboard" : "/"} className="text-lg font-semibold tracking-tight">
            uzi<span className="text-indigo-400"> · dark factory</span>
          </Link>
          <nav className="flex items-center gap-3 text-sm">
            {user ? (
              <>
                <Link to="/dashboard" className="text-slate-300 hover:text-white">
                  Dashboard
                </Link>
                <Link to="/agents" className="text-slate-300 hover:text-white">
                  Agents
                </Link>
                <Link to="/repos" className="text-slate-300 hover:text-white">
                  Repos
                </Link>
                {repos.length > 0 && (
                  <select
                    value={currentRepoId}
                    onChange={(e) => {
                      if (e.target.value) navigate(`/repos/${e.target.value}/board`);
                    }}
                    className="rounded-md border border-slate-700 bg-slate-900 px-2 py-1 text-xs text-slate-200 outline-none focus:border-indigo-400"
                    aria-label="Open a board"
                  >
                    <option value="">Boards…</option>
                    {repos.map((r) => (
                      <option key={r.id} value={r.id}>
                        {r.path_with_namespace}
                      </option>
                    ))}
                  </select>
                )}
                <Link to="/settings/forge" className="text-slate-300 hover:text-white">
                  Forge
                </Link>
                {user.is_admin && (
                  <Link to="/admin/users" className="text-slate-300 hover:text-white">
                    Users
                  </Link>
                )}
                <Link to="/settings" className="text-slate-300 hover:text-white">
                  Settings
                </Link>
                <span className="hidden text-slate-500 sm:inline">{user.email}</span>
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
      <main className="mx-auto max-w-6xl px-4 py-10">{children}</main>
    </div>
  );
}
