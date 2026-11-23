// The Models tab's unsaved-changes guard (PRD #3263 example 6): the data router's route-local
// blocker for in-app navigation (tabs, sidebar, command menu, back/forward), plus the native
// beforeunload prompt for reload/close. No click interception or patched history.
//
// It never traps a user in a form that no longer belongs to them: the viewer is read when the
// router asks, from a ref refreshed on every render (not captured with the draft), so a logout
// or a 401 that cleared the session is not blocked, and /login is never blocked either.
// AppShell's logout runs `await logout(); navigate("/login")`, which can reach the check before
// React has re-rendered with the cleared user; the /login rule covers that ordering.

import { useCallback, useEffect, useRef } from "react";
import { useBlocker, type BlockerFunction } from "react-router-dom";
import { useAuth } from "../../auth/AuthContext";
import { Modal } from "../Modal";
import { Button } from "../ui";

export function UnsavedModelsGuard({
  dirtyCount,
  viewerId,
  onDiscard,
}: {
  dirtyCount: number;
  /** The viewer whose draft this is. */
  viewerId: string;
  /** Drops the draft back to the saved baseline before the blocked navigation proceeds. */
  onDiscard: () => void;
}) {
  const { user } = useAuth();
  const liveViewer = useRef<string | null>(user?.id ?? null);
  liveViewer.current = user?.id ?? null;
  const dirty = dirtyCount > 0;
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;

  const shouldBlock = useCallback<BlockerFunction>(
    ({ currentLocation, nextLocation }) =>
      dirtyRef.current &&
      liveViewer.current !== null &&
      liveViewer.current === viewerId &&
      currentLocation.pathname !== nextLocation.pathname &&
      nextLocation.pathname !== "/login",
    [viewerId],
  );
  const blocker = useBlocker(shouldBlock);

  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Older engines show the prompt only when returnValue is set.
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  // Nothing left unsaved while the dialog is open (a pending save finished, or the draft was
  // edited back): the user asked to leave and there is nothing to lose, so let them go rather
  // than ask about "0 changes".
  const blockedClean = blocker.state === "blocked" && !dirty;
  useEffect(() => {
    if (blockedClean) blocker.proceed?.();
  }, [blockedClean, blocker]);

  if (blocker.state !== "blocked" || !dirty) return null;
  const stay = () => blocker.reset();
  const discard = () => {
    onDiscard();
    blocker.proceed();
  };
  return (
    <Modal label="You have unsaved model changes" onClose={stay}>
      <div className="w-full max-w-md space-y-4 rounded-xl border border-edge bg-surface p-5 shadow-2xl">
        <h2 className="text-base font-semibold text-fg">You have unsaved model changes</h2>
        <p className="text-sm text-muted">
          {dirtyCount === 1 ? "1 change is" : `${dirtyCount} changes are`} not saved yet. Stay to review and
          save, or discard and continue.
        </p>
        <div className="flex flex-wrap justify-end gap-2">
          <Button type="button" variant="secondary" onClick={stay}>
            Stay
          </Button>
          <Button type="button" variant="danger" onClick={discard}>
            Discard changes
          </Button>
        </div>
      </div>
    </Modal>
  );
}
