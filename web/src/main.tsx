import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { createBrowserRouter, isRouteErrorResponse, RouterProvider, useRouteError } from "react-router-dom";
import App from "./App";
import { AuthProvider } from "./auth/AuthContext";
import "./index.css";

// Bundled IBM Plex family (PRD #1167 M6): the weights the UI uses, latin +
// latin-ext (latin-ext carries the product name / Romanian diacritics). These
// fetch the woff2 same-origin only when an element uses the family (font-display:
// swap), so a System-typeface user downloads nothing. The typeface layer in
// index.css switches --font-* to these families under :root[data-font="plex"].
// Sans ships 400/500/600/700: `font-bold` (700) is used widely, so a real 700
// face is bundled instead of letting the browser synthesise bold from 600. Mono
// ships 400/500/600 (`font-semibold` is used on the mono usage/plan/CLI
// surfaces); mono 700 is deliberately omitted — no monospace surface uses
// `font-bold`. src/lib/font-imports.test.ts pins this set.
import "@fontsource/ibm-plex-sans/latin-400.css";
import "@fontsource/ibm-plex-sans/latin-500.css";
import "@fontsource/ibm-plex-sans/latin-600.css";
import "@fontsource/ibm-plex-sans/latin-700.css";
import "@fontsource/ibm-plex-sans/latin-ext-400.css";
import "@fontsource/ibm-plex-sans/latin-ext-500.css";
import "@fontsource/ibm-plex-sans/latin-ext-600.css";
import "@fontsource/ibm-plex-sans/latin-ext-700.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-500.css";
import "@fontsource/ibm-plex-mono/latin-600.css";
import "@fontsource/ibm-plex-mono/latin-ext-400.css";
import "@fontsource/ibm-plex-mono/latin-ext-500.css";
import "@fontsource/ibm-plex-mono/latin-ext-600.css";

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
const router = createBrowserRouter([
  {
    path: "*",
    element: (
      <AuthProvider>
        <App />
      </AuthProvider>
    ),
    errorElement: <AppErrorElement />,
  },
]);

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>,
);
