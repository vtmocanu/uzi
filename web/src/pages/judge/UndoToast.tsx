import type { Toast } from "./shared";
import { XIcon } from "../../components/icons";

// UndoToast confirms a bulk action and offers a one-click revert. Undo clears exactly the
// members the SERVER reported settling (the response's `settled` list), one deleteDisposition
// each, at bounded concurrency — not the members this page believed were open, which is a
// staler set and can include coordinates the action never touched (see UndoMember). A
// role="status" live region so the confirmation is announced.
export function UndoToast({ toast, onUndo, onDismiss }: { toast: Toast; onUndo: () => void; onDismiss: () => void }) {
  return (
    <div className="fixed inset-x-0 bottom-20 z-30 flex justify-center px-4">
      <div
        role="status"
        className="flex items-center gap-3 rounded-lg border border-edge-strong bg-surface px-4 py-2.5 text-sm shadow-lg"
      >
        <span className="text-fg">{toast.message}</span>
        {toast.undo.length > 0 && (
          <button
            type="button"
            onClick={onUndo}
            className="inline-flex min-h-[24px] items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-muted transition-colors hover:bg-raised hover:text-fg"
          >
            Undo
          </button>
        )}
        <button
          type="button"
          onClick={onDismiss}
          aria-label="Dismiss"
          className="rounded-md p-0.5 text-faint transition-colors hover:text-fg"
        >
          <XIcon />
        </button>
      </div>
    </div>
  );
}
