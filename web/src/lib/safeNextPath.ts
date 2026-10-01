// safeNextPath returns a same-origin internal path to return to after login, or
// "/dashboard" for anything unsafe. It admits only a rooted path ("/…") that is
// NOT protocol-relative ("//host") and contains NO backslash: this feeds
// react-router navigate() today (client-side, so "/\evil.com" is inert), but
// window.location normalizes "\"→"/", so the day a refactor points it at
// window.location.assign, "/\evil.com" would become a live open redirect. Reject
// the backslash here so that refactor can't silently reopen the hole.
//
// INVARIANT (per-call-site, not global): this guard covers exactly its two callers' sinks:
// Login's returnTo (navigate(returnTo) after a password login, from ?next=) and the pending
// consent return (lib/pendingReturn validates a stored path with it, and AppShell then calls
// navigate(back)). It is not a global open-redirect guard. Every OTHER navigate() of a value
// derived from the URL or the server must protect its own dynamic path segment — the sinks that
// interpolate a server id wrap it in encodeURIComponent(...) so a future non-UUID id (slug,
// name, forge identifier) cannot inject a "/", "//", or "\" path segment. A new navigate() sink
// is NOT covered by anything here; encode its dynamic segment at the call site.
export function safeNextPath(next: string | null): string {
  if (next && next.startsWith("/") && !next.startsWith("//") && !next.includes("\\")) {
    return next;
  }
  return "/dashboard";
}
