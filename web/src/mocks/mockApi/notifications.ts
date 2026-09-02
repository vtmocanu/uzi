import type { Notification, NotificationList } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { isTerminalRun } from "../../lib/runStatus";
import { mockNotifications, type MockNotification, mockOtherRunOwners } from "../data";
import { state } from "../store";
import { delay, requireSession } from "./shared";

let notifications: MockNotification[] = mockNotifications.map((n) => ({ ...n }));

// notifDTO maps an internal mock notification row to the API shape, attaching the
// owner block only for the admin all-view (own-scope rows carry no owner), exactly
// like the server's two query paths.
function notifDTO(n: MockNotification, includeOwner: boolean): Notification {
  return {
    id: n.id,
    kind: n.kind,
    payload: n.payload,
    run_id: n.run_id,
    review_id: n.review_id,
    read_at: n.read_at,
    created_at: n.created_at,
    ...(includeOwner
      ? { owner: { id: n.user_id, email: n.owner_email, display_name: n.owner_display_name } }
      : {}),
  };
}

export const notificationsApi = {
  // ── Notifications inbox (PRD #46 M2) ─────────────────────────────────────────
  // Own view filters to the session user; { all: true } shows everyone but only
  // for an admin (else 403, like the server). `unread` is always the caller's own
  // count. Rows come back newest-first, paginated.
  listNotifications: async (params?: { all?: boolean; limit?: number; offset?: number }): Promise<NotificationList> => {
    const me = requireSession();
    const all = params?.all ?? false;
    if (all && !me.is_admin) throw new ApiError(403, "admin only");
    const limit = Math.min(Math.max(params?.limit ?? 30, 1), 100);
    const offset = Math.max(params?.offset ?? 0, 0);
    const scope = all ? notifications : notifications.filter((n) => n.user_id === me.id);
    const sorted = [...scope].sort((a, b) => b.created_at.localeCompare(a.created_at));
    const page = sorted.slice(offset, offset + limit).map((n) => notifDTO(n, all));
    const unread = notifications.filter((n) => n.user_id === me.id && !n.read_at).length;
    return delay({ notifications: page, unread, total: scope.length });
  },
  unreadNotificationCount: async () => {
    const me = requireSession();
    return delay({ unread: notifications.filter((n) => n.user_id === me.id && !n.read_at).length }, 40);
  },
  // Runs-in-progress count for the Runs nav badge (PRD #239). Counted LIVE from the
  // fixtures the same way the real endpoint counts rows: non-terminal runs, excluding
  // chat/judge kinds (Decision 1 + Decision 4), so the demo build shows a real number
  // that moves as runs start and finish rather than a hardcoded constant.
  runsInProgressCount: async () => {
    // Other users' runs are excluded exactly as the real /me/runs/in-progress-count
    // is caller-scoped — the badge counts YOUR queue, not the factory's.
    const count = [...state.runs.values()].filter(
      (r) =>
        !isTerminalRun(r.status) &&
        r.kind !== "chat" &&
        r.kind !== "judge" &&
        !(r.id in mockOtherRunOwners),
    ).length;
    return delay({ count }, 40);
  },
  markNotificationRead: async (id: string) => {
    const me = requireSession();
    // Ownership is the (id, user_id) match — a foreign or unknown id is a 404,
    // exactly like the server's query.
    const n = notifications.find((x) => x.id === id && x.user_id === me.id);
    if (!n) throw new ApiError(404, "notification not found");
    if (!n.read_at) n.read_at = new Date().toISOString();
    return delay({ notification: notifDTO(n, false) });
  },
};
