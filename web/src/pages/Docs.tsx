import { Link } from "react-router-dom";
import { listUserDocs } from "../lib/docs";
import { Card } from "../components/ui";

// Public docs index: the `audience: user` howtos, ordered by frontmatter
// `order`. Adding a page never touches this file — it is driven entirely by the
// bundled docs' frontmatter.
export function Docs() {
  const docs = listUserDocs();
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Docs</h1>
        <p className="mt-1 text-slate-400">
          Howtos for getting from nothing to a working board.
        </p>
      </div>

      {docs.length === 0 ? (
        <Card>
          <p className="text-sm text-slate-500">No howtos published yet.</p>
        </Card>
      ) : (
        <ul className="space-y-3">
          {docs.map((doc) => (
            <li key={doc.slug}>
              <Link to={`/docs/${doc.slug}`} className="block">
                <Card className="transition-colors hover:border-indigo-700">
                  <h2 className="font-medium text-slate-100">{doc.meta.title || doc.slug}</h2>
                  {doc.summary && <p className="mt-1 text-sm text-slate-400">{doc.summary}</p>}
                </Card>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
