import type { MouseEvent } from "react";
import { isHttpsUrl } from "../lib/api";
import { ExternalLinkIcon } from "./icons";

// ForgeIssueAnchor is the shared low-level guarded forge-issue anchor (NB3): given a
// (possibly untrusted/absent) issue web URL and an issue iid, it renders an external
// link to the forge issue ONLY when the URL is a valid https URL, otherwise a plain
// `#<iid>` span. The https-guard + target/rel + external-link icon invariant lives
// here so callers (RunIssueRef, Schedules' IssueRef) cannot drift.
export function ForgeIssueAnchor({
  webUrl,
  iid,
  label,
  className,
  fallbackClassName,
  onClick,
}: {
  webUrl: string | null | undefined;
  iid: number;
  label: string; // used as both aria-label and title on the anchor
  className?: string; // anchor path (https url)
  fallbackClassName?: string; // non-https plain-#iid span path
  onClick?: (e: MouseEvent) => void;
}) {
  if (isHttpsUrl(webUrl)) {
    return (
      <a
        href={webUrl ?? undefined}
        target="_blank"
        rel="noreferrer"
        aria-label={label}
        title={label}
        onClick={onClick}
        className={`inline-flex items-center gap-0.5${className ? ` ${className}` : ""}`}
      >
        #{iid}
        <ExternalLinkIcon className="h-3 w-3" />
      </a>
    );
  }
  return <span className={fallbackClassName}>#{iid}</span>;
}
