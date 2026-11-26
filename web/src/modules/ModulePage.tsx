// The generic /m/:id/* route element (PRD #4686, "Routes and navigation"). The route sits
// inside ProtectedRoute (App.tsx APP_ROUTES), so signed-out visitors are redirected before
// this renders; an adminOnly module adds AdminRoute here, before the loader mounts.

import { useParams } from "react-router-dom";
import { AdminRoute } from "../components/RouteGuards";
import { Spinner } from "../components/ui";
import { ModuleLoader } from "./ModuleLoader";
import { NoSuchModule } from "./NoSuchModule";
import { useModules } from "./registry";

export function ModulePage() {
  const { id = "" } = useParams();
  const { status, modules } = useModules();
  if (status === "loading") {
    return (
      <div role="status" className="p-6 text-sm text-faint">
        <Spinner /> Loading…
      </div>
    );
  }
  const entry = modules.find((m) => m.id === id);
  if (!entry) return <NoSuchModule id={id} />;
  const loader = <ModuleLoader key={entry.id} entry={entry} />;
  return entry.adminOnly ? <AdminRoute>{loader}</AdminRoute> : loader;
}
