import { Link } from "react-router-dom";
import { Card, CardHeader } from "../components/ui";

// An id in /m/<id> that is not in the loaded module list. The id comes from the URL, so it
// is rendered as text only.
export function NoSuchModule({ id }: { id: string }) {
  return (
    <Card>
      <CardHeader title="Module not found" />
      <p className="mt-2 text-sm text-muted">
        No module named <code className="font-mono text-fg">{id}</code> is configured.
      </p>
      <p className="mt-3 text-sm">
        <Link to="/dashboard" className="text-brand hover:underline">
          Back to overview
        </Link>
      </p>
    </Card>
  );
}
