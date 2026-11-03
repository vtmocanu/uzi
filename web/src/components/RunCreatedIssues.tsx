import type { Run } from "../lib/apiTypes";
import { stripDisplayCell } from "../lib/safeText";

// Reject raw ambiguity before URL can trim controls or normalize paths/authorities.
function safeIssueUrl(raw: string): boolean {
  if (!raw.startsWith("https://") || /[\s\p{Cc}\p{Cf}\\\\?#]/u.test(raw)) return false;
  const parts = /^https:\/\/([A-Za-z0-9.-]+|\[[A-Fa-f0-9:.]+\])(?::(0|[1-9][0-9]*))?(\/.*)$/.exec(raw);
  if (!parts) return false;
  const [, host, , pathname] = parts;
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" || !url.hostname || url.username || url.password ||
        url.search || url.hash || url.pathname !== pathname) return false;
    // Only DNS case folding and valid IPv6 serialization may change the host.
    if (!host.startsWith("[") && url.hostname !== host.toLowerCase()) return false;
    return url.pathname.slice(1).split("/").every(segment => {
      const decoded = decodeURIComponent(segment);
      return decoded !== "" && decoded !== "." && decoded !== ".." &&
        !/[\p{Cc}\p{Cf}\\\\/%]/u.test(decoded);
    });
  } catch {
    return false;
  }
}

export function RunCreatedIssues({ issues }: { issues: Run["created_issues"] }) {
  if (!issues?.length) return null;
  return (
    <div role="group" aria-label="Created issues" className="mt-2 space-y-1 text-sm">
      {issues.map((issue, index) => {
        const label = stripDisplayCell(`Created issue #${issue.iid}`);
        const title = stripDisplayCell(issue.title);
        return (
          <div key={index} className="flex flex-wrap items-baseline gap-x-2">
            {safeIssueUrl(issue.web_url) ? (
              <a href={issue.web_url} target="_blank" rel="noopener noreferrer"
                className="text-brand hover:underline">{label}</a>
            ) : <span className="text-muted">{label}</span>}
            {title && <span className="text-muted">{title}</span>}
          </div>
        );
      })}
    </div>
  );
}
