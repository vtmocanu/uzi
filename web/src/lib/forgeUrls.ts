// Pure GitLab web-URL helpers (GitLab-only product). Kept in one module so the
// `/-/issues/` and `/-/merge_requests/` path coupling lives in a single tested
// place instead of being reconstructed inline in each page (see forgeUrls.test.ts).

import { isHttpsUrl } from "./api";

// projectWebUrlFromIssue recovers a project's base web URL from one of its issue
// URLs. GitLab issue URLs are `${projectWebUrl}/-/issues/${iid}` (the project path
// may contain subgroups, e.g. .../group/sub/proj/-/issues/5), so stripping at the
// `/-/issues/` marker yields the base. Returns "" when the shape does not match,
// which mergeRequestUrl then declines to turn into a link.
export function projectWebUrlFromIssue(issueWebUrl: string): string {
  const i = issueWebUrl.indexOf("/-/issues/");
  return i >= 0 ? issueWebUrl.slice(0, i) : "";
}

// mergeRequestUrl builds the web URL for a project's merge request by iid, or null
// when the project base is not a usable https URL — so callers render a plain "!N"
// chip instead of a dead or hostile link.
export function mergeRequestUrl(projectWebUrl: string, mrIid: number): string | null {
  return isHttpsUrl(projectWebUrl) ? `${projectWebUrl}/-/merge_requests/${mrIid}` : null;
}
