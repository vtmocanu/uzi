// The app shell: a persistent grouped sidebar + content pane, adapted from
// multica's dashboard shell (packages/views/layout/app-sidebar.tsx +
// dashboard-layout.tsx). Three tiers the old single-row topbar could not
// express: product identity up top, grouped destinations (Work / Factory /
// Settings / Admin) in the middle — with enabled repos' boards as real nav
// children instead of a <select> — and the signed-in user pinned to the footer.
// Active-state logic keeps a parent lit on child routes (multica's isNavActive).

import { Link, useLocation, useNavigate } from "react-router-dom";
import { useEffect, useState, type ReactNode } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, MOCK_MODE, type Repo } from "../lib/api";
import { cx } from "./ui";
import {
  ActivityIcon,
  BoardIcon,
  BookIcon,
  BotIcon,
  BranchIcon,
  FactoryIcon,
  GearIcon,
  HomeIcon,
  LogOutIcon,
  MenuIcon,
  ServerIcon,
  UsersIcon,
  XIcon,
} from "./icons";

function isNavActive(pathname: string, href: string): boolean {
  return pathname === href || pathname.startsWith(href + "/");
}

function NavItem({
  to,
  icon,
  label,
  exactOnly = false,
  indent = false,
  onNavigate,
}: {
  to: string;
  icon?: ReactNode;
  label: string;
  exactOnly?: boolean;
  indent?: boolean;
  onNavigate?: () => void;
}) {
  const { pathname } = useLocation();
  const active = exactOnly ? pathname === to : isNavActive(pathname, to);
  return (
    <Link
      to={to}
      onClick={onNavigate}
      aria-current={active ? "page" : undefined}
      className={cx(
        "group flex items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-sm transition-colors",
        indent && "ml-4",
        active ? "bg-raised font-medium text-fg" : "text-muted hover:bg-raised/60 hover:text-fg",
      )}
    >
      {icon && (
        <span className={cx("text-base", active ? "text-brand" : "text-faint group-hover:text-muted")}>
          {icon}
        </span>
      )}
      <span className="truncate">{label}</span>
    </Link>
  );
}

function NavGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="space-y-0.5">
      <p className="px-2.5 pb-1 pt-4 text-[11px] font-semibold uppercase tracking-wider text-faint/80">
        {label}
      </p>
      {children}
    </div>
  );
}

function SidebarContent({ onNavigate }: { onNavigate?: () => void }) {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [repos, setRepos] = useState<Repo[]>([]);

  // Boards in the nav mirror the user's enabled repos; refetched on navigation
  // so enabling/disabling a repo shows up without a reload.
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
    onNavigate?.();
    await logout();
    navigate("/login");
  };

  return (
    <div className="flex h-full flex-col">
      <Link
        to="/dashboard"
        onClick={onNavigate}
        className="flex items-center gap-2.5 border-b border-edge px-4 py-4"
      >
        <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-brand/15 text-lg text-brand">
          <FactoryIcon />
        </span>
        <span className="min-w-0">
          <span className="block text-sm font-semibold leading-tight tracking-tight">uzi</span>
          <span className="block truncate text-[11px] leading-tight text-faint">
            uzinele întunecate
          </span>
        </span>
        {MOCK_MODE && (
          <span
            title="This build runs entirely in your browser on demo data — no backend."
            className="ml-auto rounded-md border border-brand/40 bg-brand/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-brand"
          >
            demo
          </span>
        )}
      </Link>

      <nav className="flex-1 space-y-1 overflow-y-auto px-2 pb-4">
        <div className="space-y-0.5 pt-3">
          <NavItem to="/dashboard" icon={<HomeIcon />} label="Overview" onNavigate={onNavigate} />
        </div>

        <NavGroup label="Work">
          <NavItem to="/repos" icon={<BoardIcon />} label="Boards" exactOnly onNavigate={onNavigate} />
          {repos.map((r) => (
            <NavItem
              key={r.id}
              to={`/repos/${r.id}/board`}
              label={r.path_with_namespace}
              indent
              onNavigate={onNavigate}
            />
          ))}
          <NavItem to="/runs" icon={<ActivityIcon />} label="Runs" onNavigate={onNavigate} />
        </NavGroup>

        <NavGroup label="Factory">
          <NavItem to="/agents" icon={<BotIcon />} label="Agents" onNavigate={onNavigate} />
          <NavItem to="/settings/workers" icon={<ServerIcon />} label="Workers" onNavigate={onNavigate} />
        </NavGroup>

        <NavGroup label="Configure">
          <NavItem to="/settings" icon={<GearIcon />} label="Settings" exactOnly onNavigate={onNavigate} />
          <NavItem to="/settings/forge" icon={<BranchIcon />} label="Forge" onNavigate={onNavigate} />
        </NavGroup>

        {user?.is_admin && (
          <NavGroup label="Admin">
            <NavItem to="/admin/users" icon={<UsersIcon />} label="Users" onNavigate={onNavigate} />
          </NavGroup>
        )}

        <NavGroup label="Help">
          <NavItem to="/docs" icon={<BookIcon />} label="Docs" onNavigate={onNavigate} />
        </NavGroup>
      </nav>

      {user && (
        <div className="flex items-center gap-2 border-t border-edge px-3 py-3">
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-raised text-xs font-semibold text-muted">
            {(user.display_name?.[0] ?? user.email[0] ?? "?").toUpperCase()}
          </span>
          <span className="min-w-0 flex-1">
            <span className="block truncate text-xs font-medium text-fg">
              {user.display_name ?? user.email}
            </span>
            <span className="block truncate text-[11px] text-faint">
              {user.is_admin ? "Administrator" : "User"}
            </span>
          </span>
          <button
            onClick={handleLogout}
            title="Log out"
            className="rounded-md p-1.5 text-faint transition-colors hover:bg-raised hover:text-fg"
          >
            <LogOutIcon />
          </button>
        </div>
      )}
    </div>
  );
}

// Public (signed-out) chrome: a minimal top bar; the sidebar only exists for
// signed-in sessions.
function PublicShell({ children }: { children: ReactNode }) {
  return (
    <div className="min-h-screen">
      <header className="border-b border-edge">
        <div className="mx-auto flex max-w-5xl items-center justify-between px-4 py-3">
          <Link to="/" className="flex items-center gap-2 text-sm font-semibold tracking-tight">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-brand/15 text-brand">
              <FactoryIcon />
            </span>
            uzi <span className="font-normal text-faint">· uzinele întunecate</span>
          </Link>
          <nav className="flex items-center gap-4 text-sm">
            <Link to="/docs" className="text-muted hover:text-fg">
              Docs
            </Link>
            <Link to="/login" className="text-muted hover:text-fg">
              Log in
            </Link>
            <Link
              to="/register"
              className="rounded-lg bg-brand px-3 py-1.5 text-sm font-medium text-on-brand hover:bg-brand-hover"
            >
              Register
            </Link>
          </nav>
        </div>
      </header>
      <main className="mx-auto max-w-5xl px-4 py-10">{children}</main>
    </div>
  );
}

export function AppShell({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const location = useLocation();
  const [mobileOpen, setMobileOpen] = useState(false);

  if (!user) return <PublicShell>{children}</PublicShell>;

  // The board is the one screen that wants every pixel of width; text-heavy
  // pages read better on a measure.
  const fullBleed = /^\/repos\/[^/]+\/board/.test(location.pathname);

  return (
    <div className="min-h-screen lg:pl-60">
      {/* Desktop sidebar */}
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-60 border-r border-edge bg-surface lg:block">
        <SidebarContent />
      </aside>

      {/* Mobile top bar + sheet */}
      <div className="sticky top-0 z-20 flex items-center gap-3 border-b border-edge bg-surface px-4 py-3 lg:hidden">
        <button
          onClick={() => setMobileOpen(true)}
          aria-label="Open navigation"
          className="rounded-md p-1 text-muted hover:text-fg"
        >
          <MenuIcon />
        </button>
        <Link to="/dashboard" className="text-sm font-semibold tracking-tight">
          uzi <span className="font-normal text-faint">· dark factory</span>
        </Link>
      </div>
      {mobileOpen && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div
            className="absolute inset-0 bg-black/60"
            onClick={() => setMobileOpen(false)}
            aria-hidden="true"
          />
          <div className="absolute inset-y-0 left-0 w-64 border-r border-edge bg-surface shadow-2xl">
            <button
              onClick={() => setMobileOpen(false)}
              aria-label="Close navigation"
              className="absolute right-3 top-4 z-10 rounded-md p-1 text-muted hover:text-fg"
            >
              <XIcon />
            </button>
            <SidebarContent onNavigate={() => setMobileOpen(false)} />
          </div>
        </div>
      )}

      <main className="min-w-0 px-4 py-6 sm:px-6 lg:py-8">
        <div className={cx(!fullBleed && "mx-auto w-full max-w-5xl")}>{children}</div>
      </main>
    </div>
  );
}
