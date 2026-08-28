import { Navigate, Route, Routes } from "react-router-dom";
import type { ReactElement } from "react";
import { AppShell } from "./components/AppShell";
import { AdminRoute, GuestRoute, ProtectedRoute } from "./components/RouteGuards";
import { Landing } from "./pages/Landing";
import { Login } from "./pages/Login";
import { Register } from "./pages/Register";
import { Dashboard } from "./pages/Dashboard";
import { AdminUsers } from "./pages/AdminUsers";
import { AdminRateLimits } from "./pages/AdminRateLimits";
import { AdminSettings } from "./pages/AdminSettings";
import { AdminBranding } from "./pages/AdminBranding";
import { Settings } from "./pages/Settings";
import { RunDefaults } from "./pages/RunDefaults";
import { AccessSettings } from "./pages/AccessSettings";
import { MemorySettings } from "./pages/MemorySettings";
import { CliAuth } from "./pages/CliAuth";
import { Agents } from "./pages/Agents";
import { AgentNew } from "./pages/AgentNew";
import { AgentDetail } from "./pages/AgentDetail";
import { Skills } from "./pages/Skills";
import { ToolAllowlist } from "./pages/ToolAllowlist";
import { AdminBlockedRepos } from "./pages/AdminBlockedRepos";
import { ForgeSettings } from "./pages/ForgeSettings";
import { Repos } from "./pages/Repos";
import { Board } from "./pages/Board";
import { IssueView } from "./pages/IssueView";
import { RunsHistory, RunsLayout, RunsList } from "./pages/RunsList";
import { RunView } from "./pages/RunView";
import { Schedules } from "./pages/Schedules";
import { Judge } from "./pages/Judge";
import { Findings } from "./pages/Findings";
import { Notifications } from "./pages/Notifications";
import { ChatList, ChatConversation } from "./pages/Chat";
import { WorkersSettings } from "./pages/WorkersSettings";
import { Docs } from "./pages/Docs";
import { DocPage } from "./pages/DocPage";

// The guard wrapping a route's page. "public" means the page renders with NO
// guard (still inside AppShell) — the docs pages and the CLI-auth consent page,
// which handle their own auth. "guest" is the inverse of "protected": the
// landing/login/register pages are for signed-out visitors only.
export type RouteGuard = "guest" | "public" | "protected" | "admin";

// One row of the route table. This array is the SINGLE SOURCE the router and the
// mock-mode route smoke test both consume, so a new route cannot be added without
// it appearing in the test (loud failure by construction).
export interface AppRoute {
  // The path pattern. May contain :params; unique across the array.
  path: string;
  // The page element, UNWRAPPED by any guard — withGuard applies the guard at
  // render time, and the smoke test mounts it inside its own providers.
  element: ReactElement;
  // Which guard wraps the element; "public" renders it unwrapped.
  guard: RouteGuard;
  // A concrete, fixture-valid path for a :param route, which drives the smoke
  // test's mount (a bare pattern like /runs/:id would not match).
  sample?: string;
  // Renders as a CHILD of the shared RunsLayout parent rather than as its own
  // top-level guarded route, so the shared layout survives tab switches.
  layout?: "runs";
}

// Every route the app serves. React Router v6 ranks matches by specificity, not
// source/array order, so building the <Routes> by mapping this in any order is
// safe — /runs/history out-ranks /runs/:id by construction (a static segment
// beats a param), and run ids are "run-…" prefixes or UUIDs, never "history".
export const APP_ROUTES: AppRoute[] = [
  // Signed-out only: a signed-in user is redirected to /dashboard rather than
  // rendering the public page inside the authenticated shell.
  { path: "/", element: <Landing />, guard: "guest" },
  { path: "/login", element: <Login />, guard: "guest" },
  { path: "/register", element: <Register />, guard: "guest" },
  // Public, unauthenticated: onboarding howtos are needed before login.
  { path: "/docs", element: <Docs />, guard: "public" },
  { path: "/docs/:slug", element: <DocPage />, guard: "public", sample: "/docs/agent-templates" },
  // CLI browser-login consent (PRD #64). Not guarded: it handles its own auth so
  // it can preserve ?request= across the login redirect (a ProtectedRoute would
  // drop the query on the way to /login).
  { path: "/cli-auth", element: <CliAuth />, guard: "public" },
  // Workers moved out of the Settings tabs to a first-class Factory page; the old
  // URL keeps working for bookmarks and stale deep links.
  // The <Navigate> renders unwrapped via the "public" guard, exactly as the old
  // bare redirect route did.
  { path: "/settings/workers", element: <Navigate to="/workers" replace />, guard: "public" },
  // The sidebar's single Admin entry lands on the first AdminShell tab.
  { path: "/admin", element: <Navigate to="/admin/users" replace />, guard: "public" },
  // Authenticated app.
  { path: "/dashboard", element: <Dashboard />, guard: "protected" },
  { path: "/settings", element: <Settings />, guard: "protected" },
  // Run-behavior defaults, split out of the overloaded Account tab.
  { path: "/settings/run-defaults", element: <RunDefaults />, guard: "protected" },
  { path: "/settings/forge", element: <ForgeSettings />, guard: "protected" },
  { path: "/workers", element: <WorkersSettings />, guard: "protected" },
  { path: "/settings/access", element: <AccessSettings />, guard: "protected" },
  { path: "/settings/memory", element: <MemorySettings />, guard: "protected" },
  // The Runs tabs (ux-tweaks amendment 3) nest under a LAYOUT route: the shared
  // header + tab strip + the one runs fetch live in RunsLayout, which survives tab
  // switches — so the counted "Past runs · N" tab neither refetches nor blanks on
  // a switch (the 2026-08-14 nit).
  { path: "/runs", element: <RunsList />, guard: "protected", layout: "runs" },
  { path: "/runs/history", element: <RunsHistory />, guard: "protected", layout: "runs" },
  { path: "/runs/:id", element: <RunView />, guard: "protected", sample: "/runs/run-done" },
  // PRD #241: scheduled runs (one-time + recurring).
  { path: "/schedules", element: <Schedules />, guard: "protected" },
  // PRD #98: the cross-run recommendation workbench. ?run= is the judge
  // notification deep-link anchor.
  { path: "/judge", element: <Judge />, guard: "protected" },
  // PRD #333: the per-repo incidental-findings backlog. ?run= is the finding
  // notification deep-link anchor; ?repo= / ?bucket= drive the scope + segmented control.
  { path: "/findings", element: <Findings />, guard: "protected" },
  { path: "/notifications", element: <Notifications />, guard: "protected" },
  { path: "/chat", element: <ChatList />, guard: "protected" },
  { path: "/chat/:id", element: <ChatConversation />, guard: "protected", sample: "/chat/chat-uzi-1" },
  { path: "/agents", element: <Agents />, guard: "protected" },
  { path: "/agents/new", element: <AgentNew />, guard: "protected" },
  { path: "/agents/:id", element: <AgentDetail />, guard: "protected", sample: "/agents/t-spec-keeper" },
  { path: "/skills", element: <Skills />, guard: "protected" },
  { path: "/repos", element: <Repos />, guard: "protected" },
  { path: "/repos/:id/board", element: <Board />, guard: "protected", sample: "/repos/repo-uzi/board" },
  {
    path: "/repos/:repoId/issues/:iid",
    element: <IssueView />,
    guard: "protected",
    sample: "/repos/repo-uzi/issues/31",
  },
  // Admin.
  { path: "/admin/users", element: <AdminUsers />, guard: "admin" },
  { path: "/admin/rate-limits", element: <AdminRateLimits />, guard: "admin" },
  { path: "/admin/settings", element: <AdminSettings />, guard: "admin" },
  { path: "/admin/branding", element: <AdminBranding />, guard: "admin" },
  { path: "/admin/tool-allowlist", element: <ToolAllowlist />, guard: "admin" },
  { path: "/admin/blocked-repos", element: <AdminBlockedRepos />, guard: "admin" },
];

// withGuard wraps a page element in its guard. "public" (and the two redirects,
// which are "public") returns the element unwrapped.
function withGuard(guard: RouteGuard, element: ReactElement): ReactElement {
  switch (guard) {
    case "guest":
      return <GuestRoute>{element}</GuestRoute>;
    case "protected":
      return <ProtectedRoute>{element}</ProtectedRoute>;
    case "admin":
      return <AdminRoute>{element}</AdminRoute>;
    case "public":
      return element;
  }
}

export default function App() {
  return (
    <AppShell>
      <Routes>
        {APP_ROUTES.filter((r) => !r.layout).map((r) => (
          <Route key={r.path} path={r.path} element={withGuard(r.guard, r.element)} />
        ))}
        {/* The two Runs tabs render as CHILD routes of a single guarded RunsLayout
            parent, so the shared layout (and its one runs fetch) survives tab
            switches instead of remounting per leaf. */}
        <Route
          element={
            <ProtectedRoute>
              <RunsLayout />
            </ProtectedRoute>
          }
        >
          {APP_ROUTES.filter((r) => r.layout === "runs").map((r) => (
            <Route key={r.path} path={r.path} element={r.element} />
          ))}
        </Route>
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </AppShell>
  );
}
