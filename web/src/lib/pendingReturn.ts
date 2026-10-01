// The pending-return entry (PRD #1910 D3): where to send a user after a login that cannot carry
// `?next=`. A password login returns through /login?next=, but an OIDC login always lands on "/"
// (the server has no return path, which closed an open redirect), so the consent page stores
// its own path in sessionStorage before leaving and AppShell navigates there once the session
// exists.
//
// The entry is deliberately narrow. consumePendingReturn() returns it only when it passes
// safeNextPath unchanged AND is exactly /connect?request=<uuid>: sessionStorage is writable by
// any script on the origin and by a previous page, so it is treated as untrusted input at the
// read, not trusted for having been written by us. It is deleted by every read, so it is used at
// most once.

import { safeNextPath } from "./safeNextPath";

const KEY = "uzi.pendingReturn";

const CONNECT_PATH =
  /^\/connect\?request=[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** True when path is a return target this helper will ever hand back. */
function isReturnable(path: string): boolean {
  return safeNextPath(path) === path && CONNECT_PATH.test(path);
}

/** Remember path for the next sign-in; a path that would never be returned is not stored. */
export function setPendingReturn(path: string): void {
  if (!isReturnable(path)) return;
  try {
    window.sessionStorage.setItem(KEY, path);
  } catch {
    // Private mode / storage disabled: the ?next= return still works for a password login.
  }
}

/** Drop any stored entry. */
export function clearPendingReturn(): void {
  try {
    window.sessionStorage.removeItem(KEY);
  } catch {
    // Nothing to clear.
  }
}

/**
 * Return the stored path if it is still a valid /connect?request=<uuid>, else null, and delete
 * the entry either way: it is consumed once.
 */
export function consumePendingReturn(): string | null {
  let raw: string | null = null;
  try {
    raw = window.sessionStorage.getItem(KEY);
  } catch {
    return null;
  }
  clearPendingReturn();
  return raw !== null && isReturnable(raw) ? raw : null;
}
