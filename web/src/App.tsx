import { Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "./components/AppShell";
import { AdminRoute, ProtectedRoute } from "./components/RouteGuards";
import { Landing } from "./pages/Landing";
import { Login } from "./pages/Login";
import { Register } from "./pages/Register";
import { Dashboard } from "./pages/Dashboard";
import { AdminUsers } from "./pages/AdminUsers";
import { Settings } from "./pages/Settings";
import { Agents } from "./pages/Agents";
import { AgentNew } from "./pages/AgentNew";
import { AgentDetail } from "./pages/AgentDetail";
import { ForgeSettings } from "./pages/ForgeSettings";
import { Repos } from "./pages/Repos";
import { Board } from "./pages/Board";
import { IssueView } from "./pages/IssueView";
import { RunsList } from "./pages/RunsList";
import { RunView } from "./pages/RunView";
import { WorkersSettings } from "./pages/WorkersSettings";
import { Docs } from "./pages/Docs";
import { DocPage } from "./pages/DocPage";

export default function App() {
  return (
    <AppShell>
      <Routes>
        <Route path="/" element={<Landing />} />
        <Route path="/login" element={<Login />} />
        <Route path="/register" element={<Register />} />
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
            <AdminRoute>
              <AgentNew />
            </AdminRoute>
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
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </AppShell>
  );
}
