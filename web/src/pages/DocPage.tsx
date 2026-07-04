import { Link, useParams } from "react-router-dom";
import { getDoc } from "../lib/docs";
import { DocMarkdown } from "../components/DocMarkdown";

// Public single-doc view. Only `audience: user` pages are routable in-app; any
// other slug (unknown, or a repo-only design/operator doc) renders a not-found
// state inside the docs shell rather than the App-level redirect to `/`.
export function DocPage() {
  const { slug = "" } = useParams();
  const doc = getDoc(slug);

  if (!doc || doc.meta.audience !== "user") {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-semibold">Doc not found</h1>
        <p className="text-slate-400">
          There is no published doc at <code>/docs/{slug}</code>.
        </p>
        <Link to="/docs" className="text-sm text-indigo-400 hover:text-indigo-300">
          ← All docs
        </Link>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <Link to="/docs" className="text-sm text-indigo-400 hover:text-indigo-300">
        ← All docs
      </Link>
      <article>
        <DocMarkdown content={doc.body} />
      </article>
    </div>
  );
}
