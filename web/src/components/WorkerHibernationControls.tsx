import { useEffect, useRef, useState } from "react";
import { api, type Worker } from "../lib/api";
import { stripDisplayCell } from "../lib/safeText";
import { hibernationErrorMessage, hibernationState } from "../lib/workerHibernation";
import { Button } from "./ui";

/**
 * Admin hibernate/wake control for one worker (PRD #4205). Hibernate is offered for a
 * persistent hosted worker that is not hibernating; Wake for any worker that is. A refused
 * request (409) shows its mapped reason inline; onChanged refreshes the list after success.
 */
export function WorkerHibernationControls({ worker, onChanged }: { worker: Worker; onChanged: () => void }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const state = hibernationState(worker);
  const canHibernate = state === "none" && worker.kind === "hosted" && worker.ephemeral !== true;
  if (!canHibernate && state === "none") return null;

  const name = stripDisplayCell(worker.name);
  const run = async (hibernate: boolean) => {
    setPending(true);
    setError("");
    try {
      if (hibernate) await api.adminHibernateWorker(worker.id);
      else await api.adminWakeWorker(worker.id);
      onChanged();
    } catch (err) {
      if (alive.current) setError(hibernationErrorMessage(err, hibernate ? "Failed to hibernate worker" : "Failed to wake worker"));
    } finally {
      if (alive.current) setPending(false);
    }
  };

  return (
    <div className="mt-1 space-y-1">
      {state === "none" ? (
        <Button size="sm" variant="secondary" disabled={pending} aria-label={`Hibernate ${name}`} onClick={() => void run(true)}>
          {pending ? "Hibernating…" : "Hibernate"}
        </Button>
      ) : (
        <Button size="sm" variant="secondary" disabled={pending} aria-label={`Wake ${name}`} onClick={() => void run(false)}>
          {pending ? "Waking…" : "Wake"}
        </Button>
      )}
      {error && (
        <p role="alert" className="max-w-xs whitespace-normal text-xs text-danger">
          {error}
        </p>
      )}
    </div>
  );
}
