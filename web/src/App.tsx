import { Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "./components/AppShell";
import { AdminRoute, GuestRoute, ProtectedRoute } from "./components/RouteGuards";
import { Landing } from "./pages/Landing";
import { Login } from "./pages/Login";
import { Register } from "./pages/Register";
import { Dashboard } from "./pages/Dashboard";
import { AdminUsers } from "./pages/AdminUsers";
import { AdminRateLimits } from "./pages/AdminRateLimits";
import { AdminSettings } from "./pages/AdminSettings";
import { Settings } from "./pages/Settings";
import { AccessSettings } from "./pages/AccessSettings";
import { MemorySettings } from "./pages/MemorySettings";
import { CliAuth } from "./pages/CliAuth";
import { Agents } from "./pages/Agents";
import { AgentNew } from "./pages/AgentNew";
import { AgentDetail } from "./pages/AgentDetail";
import { Skills } from "./pages/Skills";
import { ToolAllowlist } from "./pages/ToolAllowlist";
import { ForgeSettings } from "./pages/ForgeSettings";
import { Repos } from "./pages/Repos";
import { Board } from "./pages/Board";
import { IssueView } from "./pages/IssueView";
import { RunsList } from "./pages/RunsList";
import { RunView } from "./pages/RunView";
import { Notifications } from "./pages/Notifications";
import { ChatList, ChatConversation } from "./pages/Chat";
import { WorkersSettings } from "./pages/WorkersSettings";
import { Docs } from "./pages/Docs";
import { DocPage } from "./pages/DocPage";

export default function App() {
  return (
    <AppShell>
      <Routes>
        {/* Signed-out only: a signed-in user is redirected to /dashboard rather
            than rendering the public page inside the authenticated shell. */}
        <Route
          path="/"
          element={
            <GuestRoute>
              <Landing />
            </GuestRoute>
          }
        />
        <Route
          path="/login"
          element={
            <GuestRoute>
              <Login />
            </GuestRoute>
          }
        />
        <Route
          path="/register"
          element={
            <GuestRoute>
              <Register />
            </GuestRoute>
          }
        />
        {/* Public, unauthenticated: onboarding howtos are needed before login. */}
        <Route path="/docs" element={<Docs />} />
        <Route path="/docs/:slug" element={<DocPage />} />
        <Route
          path="/dashboard"
          element={
            <ProtectedRoute>
              <Dashboard />
            </ProtectedRoute>
          }
        />
        <Route
          path="/settings"
          element={
            <ProtectedRoute>
              <Settings />
            </ProtectedRoute>
          }
        />
        <Route
          path="/settings/forge"
          element={
            <ProtectedRoute>
              <ForgeSettings />
            </ProtectedRoute>
          }
        />
        <Route
          path="/settings/workers"
          element={
            <ProtectedRoute>
              <WorkersSettings />
            </ProtectedRoute>
          }
        />
        <Route
          path="/settings/access"
          element={
            <ProtectedRoute>
              <AccessSettings />
            </ProtectedRoute>
          }
        />
        <Route
          path="/settings/memory"
          element={
            <ProtectedRoute>
              <MemorySettings />
            </ProtectedRoute>
          }
        />
        {/* CLI browser-login consent (PRD #64). Not wrapped in ProtectedRoute: it
            handles its own auth so it can preserve ?request= across the login
            redirect (ProtectedRoute would drop the query on the way to /login). */}
        <Route path="/cli-auth" element={<CliAuth />} />
        <Route
          path="/runs"
          element={
            <ProtectedRoute>
              <RunsList />
            </ProtectedRoute>
          }
        />
        <Route
          path="/runs/:id"
          element={
            <ProtectedRoute>
              <RunView />
            </ProtectedRoute>
          }
        />
        <Route
          path="/notifications"
          element={
            <ProtectedRoute>
              <Notifications />
            </ProtectedRoute>
          }
        />
        <Route
          path="/chat"
          element={
            <ProtectedRoute>
              <ChatList />
            </ProtectedRoute>
          }
        />
        <Route
          path="/chat/:id"
          element={
            <ProtectedRoute>
              <ChatConversation />
            </ProtectedRoute>
          }
        />
        <Route
          path="/agents"
          element={
            <ProtectedRoute>
              <Agents />
            </ProtectedRoute>
          }
        />
        <Route
          path="/agents/new"
          element={
            <ProtectedRoute>
              <AgentNew />
            </ProtectedRoute>
          }
        />
        <Route
          path="/agents/:id"
          element={
            <ProtectedRoute>
              <AgentDetail />
            </ProtectedRoute>
          }
        />
        <Route
          path="/skills"
          element={
            <ProtectedRoute>
              <Skills />
            </ProtectedRoute>
          }
        />
        <Route
          path="/repos"
          element={
            <ProtectedRoute>
              <Repos />
            </ProtectedRoute>
          }
        />
        <Route
          path="/repos/:id/board"
          element={
            <ProtectedRoute>
              <Board />
            </ProtectedRoute>
          }
        />
        <Route
          path="/repos/:repoId/issues/:iid"
          element={
            <ProtectedRoute>
              <IssueView />
            </ProtectedRoute>
          }
        />
        <Route
          path="/admin/users"
          element={
            <AdminRoute>
              <AdminUsers />
            </AdminRoute>
          }
        />
        <Route
          path="/admin/rate-limits"
          element={
            <AdminRoute>
              <AdminRateLimits />
            </AdminRoute>
          }
        />
        <Route
          path="/admin/settings"
          element={
            <AdminRoute>
              <AdminSettings />
            </AdminRoute>
          }
        />
        <Route
          path="/admin/tool-allowlist"
          element={
            <AdminRoute>
              <ToolAllowlist />
            </AdminRoute>
          }
        />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </AppShell>
  );
}
