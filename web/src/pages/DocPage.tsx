import { Link, useParams } from "react-router-dom";
import { getDoc, isRoutableDoc } from "../lib/docs";
import { DocMarkdown } from "../components/DocMarkdown";
import { useAuth } from "../auth/AuthContext";

// Public single-doc view. `audience: user` pages are routable in-app for
// everyone; `audience: operator` pages are additionally routable for admins
// (role-aware in-app docs, issue #75 M1). Any other slug (unknown, or a repo-only
// design/operator-for-a-non-admin doc) renders a not-found state inside the docs
// shell rather than the App-level redirect to `/`.
export function DocPage() {
  const { slug = "" } = useParams();
  const doc = getDoc(slug);
  const isAdmin = useAuth().user?.is_admin ?? false;

  if (!doc || !isRoutableDoc(slug, isAdmin)) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-semibold">Doc not found</h1>
        <p className="text-muted">
          There is no published doc at <code>/docs/{slug}</code>.
        </p>
        <Link to="/docs" className="text-sm text-brand hover:text-brand-hover">
          ← All docs
        </Link>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <Link to="/docs" className="text-sm text-brand hover:text-brand-hover">
        ← All docs
      </Link>
      <article>
        <DocMarkdown content={doc.body} isAdmin={isAdmin} />
      </article>
    </div>
  );
}
