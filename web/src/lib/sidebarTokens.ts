// The user's choice of which token meters ride the sidebar rail (round-3 UX
// feedback: the rail showed an automatic "hottest token" pick, and the user
// wants explicit control instead). The default token ALWAYS shows; the
// preference lists the non-default extras, persisted per user as
// UserSettings.sidebar_token_ids.
//
// The change event mirrors lib/notifications.ts: a toggle saved on the Settings
// page must reach the sidebar (a separate mount) the moment it lands, without
// threading state through the router. The rail also refetches on its own poll,
// so this is a responsiveness nicety, not the source of truth.

const SIDEBAR_TOKENS_CHANGED_EVENT = "uzi:sidebar-tokens-changed";

export function emitSidebarTokensChanged(): void {
  window.dispatchEvent(new Event(SIDEBAR_TOKENS_CHANGED_EVENT));
}

export function onSidebarTokensChanged(cb: () => void): () => void {
  window.addEventListener(SIDEBAR_TOKENS_CHANGED_EVENT, cb);
  return () => window.removeEventListener(SIDEBAR_TOKENS_CHANGED_EVENT, cb);
}

// isShownInSidebar is the ONE place the "default always, extras by choice" rule
// is spelled, so the Settings checkbox and the rail cannot disagree about what a
// checked box means.
export function isShownInSidebar(
  token: { secret_id?: string; id?: string; is_default: boolean },
  sidebarTokenIds: string[],
): boolean {
  if (token.is_default) return true;
  const id = token.secret_id ?? token.id ?? "";
  return sidebarTokenIds.includes(id);
}
