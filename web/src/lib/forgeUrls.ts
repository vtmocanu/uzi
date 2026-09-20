// Forge-aware web-URL helpers (GitLab, GitHub, Forgejo). Kept in one module so the
// per-forge `/issues/` and merge/pull-request path coupling lives in a single tested
// place instead of being reconstructed inline in each page (see forgeUrls.test.ts).

import { isHttpsUrl } from "./api";
import { mrPathSegment } from "./forgeNoun";

// projectWebUrlFromIssue recovers a project's base web URL from one of its issue
// URLs, tolerating both forge grammars. GitLab issue URLs are
// `${projectWebUrl}/-/issues/${iid}` (the project path may contain subgroups, e.g.
// .../group/sub/proj/-/issues/5); GitHub/Forgejo use `${projectWebUrl}/issues/${n}`.
// The GitLab `/-/issues/` marker is checked FIRST because a GitLab URL also contains
// the plain `/issues/` substring. For the GitHub/Forgejo branch we split at the LAST
// `/issues/` (lastIndexOf), not the first: the segment before the issue number is
// always the trailing one, so a repo or owner literally named "issues"
// (.../ns/issues/issues/42) still yields the correct base. Returns "" when neither
// shape matches, which mergeRequestUrl then declines to turn into a link.
export function projectWebUrlFromIssue(issueWebUrl: string): string {
  const g = issueWebUrl.indexOf("/-/issues/");
  if (g >= 0) return issueWebUrl.slice(0, g);
  const h = issueWebUrl.lastIndexOf("/issues/");
  return h >= 0 ? issueWebUrl.slice(0, h) : "";
}

// mergeRequestUrl builds the web URL for a project's merge/pull request by iid, with
// the path segment chosen per forge (mrPathSegment): GitLab `/-/merge_requests/`,
// GitHub `/pull/`, Forgejo/Gitea `/pulls/`. Returns null when the project base is not
// a usable https URL — so callers render a plain "!N"/"#N" chip instead of a dead or
// hostile link.
export function mergeRequestUrl(
  projectWebUrl: string | null | undefined,
  mrIid: number,
  forgeType: string | null | undefined,
): string | null {
  return isHttpsUrl(projectWebUrl) ? `${projectWebUrl}${mrPathSegment(forgeType)}${mrIid}` : null;
}
