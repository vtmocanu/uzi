// Notifications inbox (PRD #46 M2). A generic in-app inbox: users see their own
// notifications, admins can flip to every user's (each row shows its owner). The
// judge is the seeded tenant, but this page knows nothing about judges — it renders
// the { title, body } payload convention and deep-links to a run when present.
// Marking read is own-row only; the bell badge in AppShell refreshes via the shared
// change event.

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type Notification } from "../lib/api";
import { Alert, Badge, Button, cx, EmptyState, ListSkeleton, PageHeader } from "../components/ui";
import { BellIcon } from "../components/icons";
import {
  emitNotificationsChanged,
  notificationBody,
  notificationTitle,
} from "../lib/notifications";

// PAGE_SIZE matches the API's default page; Load-more fetches the next page.
const PAGE_SIZE = 30;

function NotificationRow({
  n,
  canMarkRead,
  onMarkRead,
}: {
  n: Notification;
  // canMarkRead is false for another user's rows in the admin all-view (mark-read
  // is own-row only on the server — a foreign id 404s).
  canMarkRead: boolean;
  onMarkRead: (id: string) => void;
}) {
  const unread = !n.read_at;
  return (
    <li
      className={cx(
        "rounded-lg border px-3 py-2.5",
        unread ? "border-brand/30 bg-brand/5" : "border-edge bg-raised/40",
      )}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            {unread && (
              <span aria-hidden="true" className="h-1.5 w-1.5 shrink-0 rounded-full bg-brand" />
            )}
            <p className="truncate text-sm font-medium text-fg">
              {notificationTitle(n.kind, n.payload)}
            </p>
          </div>
          {notificationBody(n.payload) && (
            <p className="mt-0.5 text-sm text-muted">{notificationBody(n.payload)}</p>
          )}
          <p className="mt-1 flex flex-wrap items-center gap-x-2 text-xs text-faint">
            {n.owner && <span>{n.owner.display_name ?? n.owner.email}</span>}
            <span>{new Date(n.created_at).toLocaleString()}</span>
            {n.run_id && (
              <Link to={`/runs/${n.run_id}`} className="font-medium text-brand hover:text-brand-hover">
                · Open run
              </Link>
            )}
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {unread ? (
            canMarkRead && (
              <Button variant="ghost" size="sm" onClick={() => onMarkRead(n.id)}>
                Mark read
              </Button>
            )
          ) : (
            <Badge tone="neutral">read</Badge>
          )}
        </div>
      </div>
    </li>
  );
}

export function Notifications() {
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;

  const [scope, setScope] = useState<"mine" | "all">("mine");
  const [items, setItems] = useState<Notification[]>([]);
  const [unread, setUnread] = useState(0);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState("");

  // The API paginates (offset/limit); the page walks it with a Load-more button
  // rather than pulling the whole 200-row cap at once. total (from the envelope)
  // tells us when there is more to fetch. offset paging is stable here because
  // mark-read flips read_at in place (it never removes a row).
  const fetchPage = useCallback(
    (offset: number) =>
      api.listNotifications({
        ...(scope === "all" ? { all: true } : {}),
        limit: PAGE_SIZE,
        offset,
      }),
    [scope],
  );

  const load = useCallback(async () => {
    setError("");
    try {
      const res = await fetchPage(0);
      setItems(res.notifications);
      setUnread(res.unread);
      setTotal(res.total);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load notifications");
    } finally {
      setLoading(false);
    }
  }, [fetchPage]);

  useEffect(() => {
    setLoading(true);
    load();
  }, [load]);

  const loadMore = async () => {
    setLoadingMore(true);
    setError("");
    try {
      const res = await fetchPage(items.length);
      setItems((prev) => [...prev, ...res.notifications]);
      setUnread(res.unread);
      setTotal(res.total);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load more");
    } finally {
      setLoadingMore(false);
    }
  };

  const markRead = async (id: string) => {
    try {
      const { notification } = await api.markNotificationRead(id);
      setItems((prev) => prev.map((n) => (n.id === id ? notification : n)));
      setUnread((u) => Math.max(0, u - 1));
      // Nudge the bell badge to re-poll immediately.
      emitNotificationsChanged();
    } catch {
      // A concurrent read or a vanished row is harmless; the next load reconciles.
    }
  };

  // In the all-view a row is markable only when it's the current admin's own.
  const canMark = (n: Notification) => !n.owner || n.owner.id === user?.id;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Notifications"
        description="Run reviews and other updates. Marking one read clears it from your unread count."
        actions={
          isAdmin ? (
            <div className="inline-flex overflow-hidden rounded-lg border border-edge text-sm">
              <button
                type="button"
                onClick={() => setScope("mine")}
                aria-pressed={scope === "mine"}
                className={cx(
                  "px-3 py-1.5 transition-colors",
                  scope === "mine" ? "bg-raised font-medium text-fg" : "text-muted hover:bg-raised/60",
                )}
              >
                Mine
                {unread > 0 && scope === "mine" && (
                  <span className="ml-1.5 text-xs text-brand">{unread}</span>
                )}
              </button>
              <button
                type="button"
                onClick={() => setScope("all")}
                aria-pressed={scope === "all"}
                className={cx(
                  "border-l border-edge px-3 py-1.5 transition-colors",
                  scope === "all" ? "bg-raised font-medium text-fg" : "text-muted hover:bg-raised/60",
                )}
              >
                All users
              </button>
            </div>
          ) : undefined
        }
      />

      {error && <Alert message={error} />}
      {loading && <ListSkeleton rows={4} />}

      {!loading && !error && (
        <>
          {items.length === 0 ? (
            <EmptyState
              icon={<BellIcon />}
              title={scope === "all" ? "No notifications yet" : "You're all caught up"}
              description="Run reviews and other updates land here. Nothing to show right now."
            />
          ) : (
            <>
              <ul className="space-y-2">
                {items.map((n) => (
                  <NotificationRow key={n.id} n={n} canMarkRead={canMark(n)} onMarkRead={markRead} />
                ))}
              </ul>
              {items.length < total && (
                <div className="flex justify-center pt-1">
                  <Button variant="secondary" size="sm" disabled={loadingMore} onClick={loadMore}>
                    {loadingMore ? "Loading…" : `Load more (${items.length} of ${total})`}
                  </Button>
                </div>
              )}
            </>
          )}
        </>
      )}
    </div>
  );
}
