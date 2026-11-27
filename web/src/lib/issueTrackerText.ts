// Plain-language text for issue tracker data (PRD #4173 M6), shared by Settings → Issue
// trackers, the Repos page's link dialog and the States panel. Every value that came
// from the API passes through stripUnsafeChars here; callers render the result as JSX
// text, never as HTML or Markdown.

import type { IssueTrackerGroup, IssueTrackerSharing } from "./apiTypes";
import { stripUnsafeChars } from "./safeText";

// issueTrackerKindLabel names a kind the way the issue tracker names itself. An
// unknown kind (a newer API than this bundle) shows its raw, sanitized value.
export function issueTrackerKindLabel(kind: string): string {
  if (kind === "youtrack") return "YouTrack";
  return stripUnsafeChars(kind);
}

// A named group counts for "others can apply" only when the issue tracker reports at
// least two members for it (PRD #4173 D14); a missing count (0) never counts.
function groupText(g: IssueTrackerGroup): string {
  if (g.all_users) return "All Users";
  const name = stripUnsafeChars(g.name);
  if (g.users_count <= 0) return `the group ${name} (member count not reported)`;
  return `the group ${name} (${g.users_count} ${g.users_count === 1 ? "member" : "members"})`;
}

// describeSharing lists who a tag's see (`read`) or apply (`use`) setting is shared
// with, beyond its owner: All Users first, then named groups with member counts, then
// named users. An empty setting reads as the owner only. showLogin masks a login in
// demo mode; it receives the already sanitized login.
export function describeSharing(
  sharing: IssueTrackerSharing,
  showLogin: (login: string) => string = (login) => login,
): string {
  const parts = [
    ...sharing.groups.filter((g) => g.all_users).map(groupText),
    ...sharing.groups.filter((g) => !g.all_users).map(groupText),
    ...sharing.users.map((u) => showLogin(stripUnsafeChars(u.login))),
  ];
  if (parts.length === 0) return "only the owner";
  return `the owner and ${parts.join(", ")}`;
}

// issueKey is the issue tracker key of issue number under a project short name, the short
// name, a dash and the number ("ABC-12"). It is the one place the web assembles the key,
// matching the API's issuetracker.IssueKey.
export function issueKey(projectShort: string, number: number): string {
  return `${projectShort}-${number}`;
}
