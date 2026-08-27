// SweepLabelWarn — the sweep-label WARN affordance (PRD #589 M4, success criterion 6).
//
// A sweep only fires on issues carrying its selector label, so a label that does not
// exist on the target repo's forge means the sweep silently matches nothing. This
// advisory (never blocking) surface checks the selector against the repo and, when a
// label is missing, warns with a one-click "Create label" that creates it on the forge.
//
// It is deliberately self-contained (its own check/ensure calls) so both the modal (a
// custom sweep's form labels) and the Default-jobs enable flow (a default's catalog
// labels) can drop it in with just { repoId, repoPath, labels }.

import { useEffect, useRef, useState } from "react";
import { api } from "../lib/api";
import { Alert, Button, cx } from "./ui";
import { AlertIcon } from "./icons";

export function SweepLabelWarn({
  repoId,
  repoPath,
  labels,
  className,
  // Called after a successful ensure, so a parent can refresh any dependent view.
  onEnsured,
}: {
  repoId: string;
  repoPath?: string;
  labels: string[];
  className?: string;
  onEnsured?: (ensured: string[]) => void;
}) {
  const [missing, setMissing] = useState<string[]>([]);
  // The labels a "Create label(s)" click just created — on success the warn resolves, and
  // instead of vanishing silently we confirm with a brief success state so the forge
  // mutation has visible/SR feedback (role=status). Cleared when the selector changes.
  const [created, setCreated] = useState<string[]>([]);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const debounceRef = useRef<number | undefined>(undefined);

  // The non-blank, deduped selector; a sweep with no explicit label (empty ⇒ the PRD
  // label at fire time) has nothing to check, so the warn stays silent.
  const selector = [...new Set(labels.map((l) => l.trim()).filter(Boolean))];
  const selectorKey = selector.join(",");

  useEffect(() => {
    window.clearTimeout(debounceRef.current);
    setError("");
    // A new selector/repo re-checks from scratch, so a prior confirmation no longer applies.
    setCreated([]);
    if (!repoId || selector.length === 0) {
      setMissing([]);
      return;
    }
    let alive = true;
    debounceRef.current = window.setTimeout(() => {
      api
        .checkRepoLabels(repoId, selector)
        .then(({ missing }) => {
          if (alive) setMissing(missing);
        })
        .catch(() => {
          // A check failure is advisory too — never block the user on it.
          if (alive) setMissing([]);
        });
    }, 300);
    return () => {
      alive = false;
      window.clearTimeout(debounceRef.current);
    };
    // selectorKey captures the label set by value; repoId is the other input.
  }, [repoId, selectorKey]); // eslint-disable-line react-hooks/exhaustive-deps

  const on = repoPath ? ` on ${repoPath}` : "";

  if (missing.length === 0) {
    // Not warning: show the brief created-confirmation if a create just landed, else nothing.
    if (created.length > 0) {
      const createdMsg =
        created.length === 1
          ? `Label “${created[0]}” created${on}.`
          : `Labels ${created.map((l) => `“${l}”`).join(", ")} created${on}.`;
      return <Alert tone="success" message={createdMsg} />;
    }
    return null;
  }

  const createLabels = async () => {
    setCreating(true);
    setError("");
    // Snapshot the set we're creating so the confirmation names them even though
    // setMissing([]) clears the warn (ensured may be a subset the forge reports back).
    const requested = missing;
    try {
      const { ensured } = await api.ensureRepoLabels(repoId, requested);
      setMissing([]);
      setCreated(requested);
      onEnsured?.(ensured);
    } catch {
      setError("Could not create the label on the forge. Try again, or add it in the forge.");
    } finally {
      setCreating(false);
    }
  };

  const message =
    missing.length === 1
      ? `Label “${missing[0]}” doesn't exist${on}, so this sweep would match nothing.`
      : `Labels ${missing.map((l) => `“${l}”`).join(", ")} don't exist${on}, so this sweep would match nothing.`;

  return (
    <div
      className={cx(
        "flex flex-col gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2.5 text-[12.5px] text-warn",
        className,
      )}
      role="status"
    >
      <div className="flex items-start gap-2">
        <span aria-hidden="true" className="mt-0.5 shrink-0">
          <AlertIcon />
        </span>
        <p>{message}</p>
      </div>
      {error && <p className="text-danger">{error}</p>}
      <div>
        <Button type="button" variant="secondary" size="sm" disabled={creating} onClick={createLabels}>
          {creating
            ? "Creating…"
            : missing.length === 1
              ? `Create label “${missing[0]}”`
              : "Create labels"}
        </Button>
      </div>
    </div>
  );
}
