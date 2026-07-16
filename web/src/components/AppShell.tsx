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
import { prefs } from "../lib/prefs";
import { cx } from "./ui";
import { VaultBadge, VaultLockedBanner } from "./VaultControls";
import { RateLimitAnnouncer, SidebarRateLimits } from "./RateLimitMeters";
import { onNotificationsChanged } from "../lib/notifications";
import {
  ActivityIcon,
  BellIcon,
  BoardIcon,
  BookIcon,
  BotIcon,
  ChatIcon,
  ChevronRightIcon,
  FactoryIcon,
  GaugeIcon,
  GearIcon,
  PackageIcon,
  GitIcon,
  GitLabIcon,
  HomeIcon,
  LogOutIcon,
  MenuIcon,
  ServerIcon,
  SkillIcon,
  UsersIcon,
  XIcon,
} from "./icons";

// localStorage key for the desktop sidebar's collapsed state (per browser).
const SIDEBAR_COLLAPSED_KEY = "uzi.sidebar.collapsed";

function isNavActive(pathname: string, href: string): boolean {
  return pathname === href || pathname.startsWith(href + "/");
}

// forgeIcon picks a board entry's glyph from its connection's forge type
// (Decision 2): GitLab gets the tanuki, anything else (or an unknown/missing
// type when the connections join is unavailable) falls back to the generic Git
// mark. Exported so the mapping is unit-testable without the DOM.
export function forgeIcon(forgeType: string | undefined): ReactNode {
  return forgeType === "gitlab" ? <GitLabIcon /> : <GitIcon />;
}

function NavItem({
  to,
  icon,
  label,
  exactOnly = false,
  excludeSubpath,
  indent = false,
  onNavigate,
  collapsed = false,
  badge = 0,
}: {
  to: string;
  icon?: ReactNode;
  label: string;
  exactOnly?: boolean;
  // excludeSubpath yields active state to a sibling that owns a nested route:
  // "Settings" (/settings) stays lit on /settings/forge but hands /settings/workers
  // to the Factory "Workers" entry, so the two never light up together.
  excludeSubpath?: string;
  indent?: boolean;
  onNavigate?: () => void;
  // When collapsed the item is an icon-only rail button; the label moves to a
  // native title tooltip so the destination is still identifiable on hover.
  collapsed?: boolean;
  // badge is an unread count (PRD #46 M2): expanded, a count pill trails the label;
  // collapsed, a dot overlaps the icon since a rail has no room for a number. 0
  // renders nothing.
  badge?: number;
}) {
  const { pathname } = useLocation();
  let active = exactOnly ? pathname === to : isNavActive(pathname, to);
  if (active && excludeSubpath && isNavActive(pathname, excludeSubpath)) active = false;
  const hasBadge = badge > 0;
  return (
    <Link
      to={to}
      onClick={onNavigate}
      aria-current={active ? "page" : undefined}
      title={collapsed ? label : undefined}
      className={cx(
        "group flex items-center rounded-lg text-sm transition-colors",
        collapsed ? "justify-center px-2.5 py-2" : "gap-2.5 px-2.5 py-1.5",
        indent && !collapsed && "ml-4",
        active ? "bg-raised font-medium text-fg" : "text-muted hover:bg-raised/60 hover:text-fg",
      )}
    >
      {icon && (
        <span className={cx("relative text-base", active ? "text-brand" : "text-faint group-hover:text-muted")}>
          {icon}
          {collapsed && hasBadge && (
            <span
              aria-hidden="true"
              className="absolute -right-1 -top-1 h-2 w-2 rounded-full bg-brand ring-2 ring-surface"
            />
          )}
        </span>
      )}
      {!collapsed && <span className="truncate">{label}</span>}
      {!collapsed && hasBadge && (
        <span
          aria-label={`${badge} unread`}
          className="ml-auto min-w-[1.25rem] rounded-full bg-brand px-1.5 py-0.5 text-center text-[10px] font-semibold leading-none text-on-brand"
        >
          {badge > 99 ? "99+" : badge}
        </span>
      )}
    </Link>
  );
}

function NavGroup({
  label,
  children,
  collapsed = false,
}: {
  label: string;
  children: ReactNode;
  collapsed?: boolean;
}) {
  return (
    <div className="space-y-0.5">
      {collapsed ? (
        // No room for a group label on the rail; a thin rule keeps the grouping.
        <div className="mx-2.5 my-2 border-t border-edge" aria-hidden="true" />
      ) : (
        <p className="px-2.5 pb-1 pt-4 text-[11px] font-semibold uppercase tracking-wider text-faint/80">
          {label}
        </p>
      )}
      {children}
    </div>
  );
}

function SidebarContent({
  onNavigate,
  collapsed = false,
  onToggleCollapse,
}: {
  onNavigate?: () => void;
  // Desktop-only icon-rail mode. The mobile sheet always renders expanded (it is
  // already a full-width overlay) and passes neither prop.
  collapsed?: boolean;
  onToggleCollapse?: () => void;
}) {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [repos, setRepos] = useState<Repo[]>([]);
  // connection_id → forge_type, joined web-side so board entries can show a forge
  // glyph (the Repo DTO has no forge_type). Kept separate from repos so a failed
  // connections call degrades to the Git fallback rather than blanking the boards.
  const [forgeTypeById, setForgeTypeById] = useState<Record<string, string>>({});
  // Notifications unread badge (PRD #46 M2).
  const [unread, setUnread] = useState(0);

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

  // Unread badge: poll on navigation (no WS — PRD #46 M2) and refresh on the
  // in-app change event (e.g. after marking one read on the inbox page). A failed
  // fetch is non-fatal: keep the last known count rather than blanking the badge.
  useEffect(() => {
    if (!user) {
      setUnread(0);
      return;
    }
    let alive = true;
    const load = () =>
      api
        .unreadNotificationCount()
        .then(({ unread }) => {
          if (alive) setUnread(unread);
        })
        .catch(() => {});
    load();
    const off = onNotificationsChanged(load);
    return () => {
      alive = false;
      off();
    };
  }, [user, location.pathname]);

  // Connections change rarely, so this join is fetched once per session, not per
  // navigation. Failure is non-fatal: the map stays empty and every board falls
  // back to the generic Git icon.
  useEffect(() => {
    if (!user) {
      setForgeTypeById({});
      return;
    }
    api
      .listConnections()
      .then(({ connections }) =>
        setForgeTypeById(Object.fromEntries(connections.map((c) => [c.id, c.forge_type]))),
      )
      .catch(() => setForgeTypeById({}));
  }, [user]);

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
        title={collapsed ? "uzi · uzinele întunecate" : undefined}
        className={cx(
          "flex items-center border-b border-edge py-4",
          collapsed ? "justify-center px-2" : "gap-2.5 px-4",
        )}
      >
        <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-brand/15 text-lg text-brand">
          <FactoryIcon />
        </span>
        {!collapsed && (
          <>
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
          </>
        )}
      </Link>

      <nav className="flex-1 space-y-1 overflow-y-auto px-2 pb-4">
        <div className="space-y-0.5 pt-3">
          <NavItem to="/dashboard" icon={<HomeIcon />} label="Overview" onNavigate={onNavigate} collapsed={collapsed} />
          <NavItem
            to="/notifications"
            icon={<BellIcon />}
            label="Notifications"
            badge={unread}
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
        </div>

        <NavGroup label="Work" collapsed={collapsed}>
          <NavItem to="/repos" icon={<BoardIcon />} label="Boards" exactOnly onNavigate={onNavigate} collapsed={collapsed} />
          {/* Board children collapse into the "Boards" parent: on the rail every
              repo would render as an identical forge glyph, so hide them and let
              /repos stand in (Decision: reviewer #7). */}
          {!collapsed &&
            repos.map((r) => (
              <NavItem
                key={r.id}
                to={`/repos/${r.id}/board`}
                icon={forgeIcon(forgeTypeById[r.connection_id])}
                label={r.path_with_namespace}
                indent
                onNavigate={onNavigate}
              />
            ))}
          <NavItem to="/runs" icon={<ActivityIcon />} label="Runs" onNavigate={onNavigate} collapsed={collapsed} />
          <NavItem to="/chat" icon={<ChatIcon />} label="Chat" onNavigate={onNavigate} collapsed={collapsed} />
        </NavGroup>

        <NavGroup label="Factory" collapsed={collapsed}>
          <NavItem to="/agents" icon={<BotIcon />} label="Agents" onNavigate={onNavigate} collapsed={collapsed} />
          <NavItem to="/skills" icon={<SkillIcon />} label="Skills" onNavigate={onNavigate} collapsed={collapsed} />
          <NavItem to="/settings/workers" icon={<ServerIcon />} label="Workers" onNavigate={onNavigate} collapsed={collapsed} />
        </NavGroup>

        <NavGroup label="Configure" collapsed={collapsed}>
          {/* Forge has no standalone entry (Decision 3): it lives only under the
              Settings tabs. Settings therefore stays lit across /settings/* —
              except /settings/workers, which the Factory "Workers" entry owns. */}
          <NavItem
            to="/settings"
            icon={<GearIcon />}
            label="Settings"
            excludeSubpath="/settings/workers"
            onNavigate={onNavigate}
            collapsed={collapsed}
          />
        </NavGroup>

        {user?.is_admin && (
          <NavGroup label="Admin" collapsed={collapsed}>
            <NavItem to="/admin/users" icon={<UsersIcon />} label="Users" onNavigate={onNavigate} collapsed={collapsed} />
            <NavItem
              to="/admin/rate-limits"
              icon={<GaugeIcon />}
              label="Rate limits"
              onNavigate={onNavigate}
              collapsed={collapsed}
            />
            <NavItem
              to="/admin/tool-allowlist"
              icon={<PackageIcon />}
              label="Tool allowlist"
              onNavigate={onNavigate}
              collapsed={collapsed}
            />
            <NavItem
              to="/admin/settings"
              icon={<GearIcon />}
              label="Instance settings"
              onNavigate={onNavigate}
              collapsed={collapsed}
            />
          </NavGroup>
        )}

        <NavGroup label="Help" collapsed={collapsed}>
          <NavItem to="/docs" icon={<BookIcon />} label="Docs" onNavigate={onNavigate} collapsed={collapsed} />
        </NavGroup>
      </nav>

      <div className="border-t border-edge">
        {/* Desktop-only collapse toggle, pinned at the footer edge (persistent, not
            hover-only). aria-expanded tracks the sidebar, not a popup, so screen
            readers announce the rail's state. */}
        {onToggleCollapse && (
          <div className={cx("hidden lg:flex px-3 pt-2", collapsed ? "justify-center" : "justify-end")}>
            <button
              type="button"
              onClick={onToggleCollapse}
              aria-expanded={!collapsed}
              aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
              title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
              className="rounded-md p-1.5 text-faint transition-colors hover:bg-raised hover:text-fg"
            >
              <span className={cx("inline-flex transition-transform", !collapsed && "rotate-180")}>
                <ChevronRightIcon />
              </span>
            </button>
          </div>
        )}

        {user &&
          (collapsed ? (
            <div className="flex flex-col items-center gap-2 px-3 py-3">
              {/* Vault status glyph (PRD #32); the lock/unlock action lives in
                  Settings and the app-wide banner. */}
              <VaultBadge compact />
              <span
                title={`${user.display_name ?? user.email} · ${user.is_admin ? "Administrator" : "User"}`}
                className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-raised text-xs font-semibold text-muted"
              >
                {(user.display_name?.[0] ?? user.email[0] ?? "?").toUpperCase()}
              </span>
              <button
                onClick={handleLogout}
                title="Log out"
                aria-label="Log out"
                className="rounded-md p-1.5 text-faint transition-colors hover:bg-raised hover:text-fg"
              >
                <LogOutIcon />
              </button>
            </div>
          ) : (
            <div className="px-3 py-3">
              <div className="mb-2">
                <VaultBadge />
              </div>
              <div className="flex items-center gap-2">
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
              {/* Claude rate-limit micro-meters (PRD #53): two 5px bars under the
                  user block. Self-gates — renders nothing without a live reading. */}
              <SidebarRateLimits />
            </div>
          ))}
      </div>
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
  // Desktop sidebar collapse, persisted per browser. Initialised lazily from
  // localStorage so the first paint already matches the stored state — a
  // post-mount effect would flash the sidebar expanded then snap it collapsed.
  const [collapsed, setCollapsed] = useState(() => prefs.get(SIDEBAR_COLLAPSED_KEY, false));
  useEffect(() => {
    prefs.set(SIDEBAR_COLLAPSED_KEY, collapsed);
  }, [collapsed]);

  if (!user) return <PublicShell>{children}</PublicShell>;

  // The board is the one screen that wants every pixel of width; text-heavy
  // pages read better on a measure.
  const fullBleed = /^\/repos\/[^/]+\/board/.test(location.pathname);

  return (
    // Width/padding are literal class strings in the ternary so Tailwind's JIT
    // emits both — an interpolated `lg:pl-${n}` would never be scanned.
    <div className={cx("min-h-screen", collapsed ? "lg:pl-14" : "lg:pl-60")}>
      {/* App-wide screen-reader alert for rate-limit tone crossings (PRD #54,
          Decision 4): a visually-hidden aria-live region mounted once so a
          window crossing into warn/danger announces on any route, not only
          while the Settings meters are on screen. */}
      <RateLimitAnnouncer />
      {/* Desktop sidebar */}
      <aside
        className={cx(
          "fixed inset-y-0 left-0 z-30 hidden border-r border-edge bg-surface lg:block",
          collapsed ? "w-14" : "w-60",
        )}
      >
        <SidebarContent collapsed={collapsed} onToggleCollapse={() => setCollapsed((c) => !c)} />
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
        <div className={cx(!fullBleed && "mx-auto w-full max-w-5xl")}>
          {/* Vault locked banner (PRD #32): app-wide so the user can unlock from
              any page. Self-gates — renders nothing while unlocked. */}
          <VaultLockedBanner />
          {children}
        </div>
      </main>
    </div>
  );
}
