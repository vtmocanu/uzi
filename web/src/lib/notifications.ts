// A tiny in-app event so the bell badge (in AppShell) refreshes the moment the
// user marks something read on the inbox page, without threading state through the
// router. The badge also polls on navigation (PRD #46 M2: "poll on navigation, no
// WS needed"), so this is a responsiveness nicety layered on top of that poll, not
// the source of truth.

export const NOTIFICATIONS_CHANGED_EVENT = "uzi:notifications-changed";

// emitNotificationsChanged signals that the caller's unread set may have changed.
export function emitNotificationsChanged(): void {
  window.dispatchEvent(new Event(NOTIFICATIONS_CHANGED_EVENT));
}

// onNotificationsChanged subscribes to the change signal and returns an
// unsubscribe function (for a useEffect cleanup).
export function onNotificationsChanged(cb: () => void): () => void {
  window.addEventListener(NOTIFICATIONS_CHANGED_EVENT, cb);
  return () => window.removeEventListener(NOTIFICATIONS_CHANGED_EVENT, cb);
}

// notificationTitle / notificationBody read the soft { title, body } convention
// notification producers follow (the judge is tenant #1), tolerating any payload
// shape. An absent title falls back to a humanized kind so an unknown producer
// still renders something legible.
export function notificationTitle(kind: string, payload: Record<string, unknown>): string {
  const t = payload.title;
  if (typeof t === "string" && t.trim() !== "") return t;
  return kind.replace(/_/g, " ");
}

export function notificationBody(payload: Record<string, unknown>): string {
  const b = payload.body;
  return typeof b === "string" ? b : "";
}
