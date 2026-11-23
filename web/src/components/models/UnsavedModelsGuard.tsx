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

const UNSAVED_TITLE = "You have unsaved model changes";
const UNCONFIRMED_TITLE = "Your model changes may already be saved";
const MAY_FINISH_TITLE = "An earlier model settings save may still finish";

/** The last save's outcome when it may have committed: "unknown" (commit not confirmed,
 *  network or gateway failure) or "saved_unreadable" (committed, readback failed). */
type SaveUncertainty = "unknown" | "saved_unreadable";

export function UnsavedModelsGuard({
  dirtyCount,
  saving = false,
  saveUncertainty = null,
  requestMayStillFinish = false,
  viewerId,
  onDiscard,
}: {
  dirtyCount: number;
  /** A save is in flight: discarding now could not stop the PUT from committing the
   *  "discarded" values, so Discard waits. A success leaves nothing dirty and the blocked
   *  navigation proceeds; a failure re-enables Discard. */
  saving?: boolean;
  /** The last save may have been stored although the draft is still dirty, so leaving is not
   *  a discard: the dialog explains the uncertainty and offers "Leave anyway". */
  saveUncertainty?: SaveUncertainty | null;
  /** An earlier save's response was lost, so it may still commit later: the "unknown" copy
   *  says that instead of claiming the save was merely unconfirmed. */
  requestMayStillFinish?: boolean;
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
  // proceed() may run at most once per blocked navigation: Discard proceeds and clears the
  // draft in one handler, so the effect below would otherwise see the still-"blocked" blocker
  // with a clean draft and proceed a second time, which the router rejects
  // ("Invalid blocker state transition: unblocked -> proceeding").
  const proceeded = useRef(false);
  if (blocker.state === "unblocked") proceeded.current = false;
  const proceedOnce = useCallback(() => {
    if (proceeded.current || blocker.state !== "blocked") return;
    proceeded.current = true;
    blocker.proceed();
  }, [blocker]);
  const blockedClean = blocker.state === "blocked" && !dirty;
  useEffect(() => {
    if (blockedClean) proceedOnce();
  }, [blockedClean, proceedOnce]);

  if (blocker.state !== "blocked" || !dirty) return null;
  const stay = () => blocker.reset();
  const discard = () => {
    if (saving) return;
    onDiscard();
    proceedOnce();
  };
  const changes = dirtyCount === 1 ? "1 change" : `${dirtyCount} changes`;
  // An uncertain save is never "not saved": the PUT may have committed, and leaving cannot
  // undo it, so the copy says so, points at Stay + Reload saved settings, and the leave
  // button makes no discard (rollback) claim.
  const uncertain = !saving && saveUncertainty ? saveUncertainty : null;
  const lostResponse = uncertain === "unknown" && requestMayStillFinish;
  const title = lostResponse ? MAY_FINISH_TITLE : uncertain ? UNCONFIRMED_TITLE : UNSAVED_TITLE;
  const message = saving
    ? `Saving ${changes}… You continue once the save finishes.`
    : lostResponse
      ? "An earlier save whose response was lost may still finish and change what is stored. Stay and choose Reload saved settings to see what is stored. Leaving does not undo anything the server stores."
    : uncertain === "unknown"
      ? `The server could not confirm whether an earlier save was applied, so ${changes} may already be saved. Stay and choose Reload saved settings to see what is stored. Leaving does not undo anything the server stored.`
      : uncertain === "saved_unreadable"
        ? `An earlier save was stored, but reading it back failed, so ${changes} may already be saved and could not be confirmed. Stay and choose Reload saved settings to see what is stored. Leaving does not undo the save.`
        : `${dirtyCount === 1 ? "1 change is" : `${dirtyCount} changes are`} not saved yet. Stay to review and save, or discard and continue.`;
  return (
    <Modal label={title} onClose={stay}>
      <div className="w-full max-w-md space-y-4 rounded-xl border border-edge bg-surface p-5 shadow-2xl">
        <h2 className="text-base font-semibold text-fg">{title}</h2>
        <p role="status" aria-live="polite" className="text-sm text-muted">
          {message}
        </p>
        <div className="flex flex-wrap justify-end gap-2">
          <Button type="button" variant={uncertain ? "primary" : "secondary"} onClick={stay}>
            Stay
          </Button>
          <Button type="button" variant={uncertain ? "secondary" : "danger"} disabled={saving} onClick={discard}>
            {uncertain ? "Leave anyway" : "Discard changes"}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
