import { isRouteErrorResponse, useRouteError, type RouteObject } from "react-router-dom";
import App from "./App";
import { AuthProvider } from "./auth/AuthContext";
import { ModulesProvider } from "./modules/registry";

// The data router's root error element (PRD #3263): what renders if the app tree throws while
// rendering, instead of the router's developer fallback. Deliberately self-contained (no auth,
// no api, no shell), because the tree that provides those is what failed.
function AppErrorElement() {
  const error = useRouteError();
  const detail = isRouteErrorResponse(error)
    ? `${error.status} ${error.statusText}`
    : error instanceof Error
      ? error.message
      : "";
  return (
    <div role="alert" className="flex min-h-screen flex-col items-center justify-center gap-3 px-4 text-center">
      <h1 className="text-lg font-medium text-fg">Something went wrong</h1>
      <p className="max-w-md text-sm text-faint">
        uzi hit an unexpected error while showing this page. Reload to try again.
        {detail && <span className="mt-1 block font-mono text-xs">{detail}</span>}
      </p>
      <button
        type="button"
        onClick={() => window.location.reload()}
        className="rounded-lg border border-edge px-3 py-1.5 text-sm text-muted hover:bg-raised/60"
      >
        Reload
      </button>
    </div>
  );
}

// A data router (PRD #3263) so a page can use the router's own unsaved-changes blocker.
// It is a minimal bootstrap adaptation, not a route-data architecture: ONE wildcard root
// wraps the existing AuthProvider/App tree, and App keeps its descendant <Routes> manifest,
// layouts and guards unchanged. No loaders or actions.
export const appRoutes: RouteObject[] = [
  {
    path: "*",
    element: (
      <AuthProvider>
        <ModulesProvider>
          <App />
        </ModulesProvider>
      </AuthProvider>
    ),
    errorElement: <AppErrorElement />,
  },
];
