import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import type { Worker } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { stripDisplayCell } from "../lib/safeText";
import { Button, Field } from "./ui";

export function WorkerCordonControls({ worker, disabled, hideAction = false, onChange, children }: {
  worker: Worker;
  disabled: boolean;
  hideAction?: boolean;
  children: (button: ReactNode, panel: ReactNode) => ReactNode;
  onChange: (cordon: boolean, reason?: string) => Promise<void>;
}) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [restore, setRestore] = useState(0);
  const alert = useRef<HTMLDivElement>(null);
  const busy = useRef(false);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => { if (open) document.getElementById(`${id}-reason`)?.focus(); }, [open, id]);
  useEffect(() => { if (error) alert.current?.focus(); }, [error]);
  useEffect(() => {
    if (restore && !open && !disabled && !pending) {
      document.getElementById(id)?.focus();
      setRestore(0);
    }
  }, [restore, id, open, disabled, pending]);
  const close = () => { setOpen(false); setError(""); setRestore((n) => n + 1); };
  const submit = async (cordon: boolean, e?: FormEvent) => {
    e?.preventDefault();
    if (busy.current || disabled) return;
    if (cordon && Array.from(reason).length > 500) {
      setError("Reason must be 500 Unicode characters or fewer.");
      alert.current?.focus();
      return;
    }
    busy.current = true;
    setPending(true);
    setError("");
    try {
      await onChange(cordon, reason === "" ? undefined : reason);
      if (alive.current) { setReason(""); close(); }
    } catch (err) {
      if (alive.current) setError(stripDisplayCell(errorMessage(err, "Failed to change manual cordon")));
    } finally {
      busy.current = false;
      if (alive.current) setPending(false);
    }
  };
  if (worker.ephemeral === true) return children(null, null);
  const name = stripDisplayCell(worker.name);
  const button = !open && !hideAction ? <>
    <Button id={id} size="sm" className="h-8" variant="secondary" disabled={disabled || pending}
      aria-label={`${worker.manual_cordoned_at ? "Uncordon" : "Cordon"} ${name}`}
      aria-describedby={worker.manual_cordoned_at ? `${id}-uncordon-help` : undefined}
      onClick={() => worker.manual_cordoned_at ? void submit(false) : setOpen(true)}>
      {worker.manual_cordoned_at ? "Uncordon" : "Cordon"}
    </Button>
    {worker.manual_cordoned_at && <span id={`${id}-uncordon-help`} className="sr-only">
      Uncordon removes only the manual hold.
    </span>}
  </> : null;
  const panel = (open || error) ? (
    <div className="w-full space-y-2">
      {open && <form className="space-y-2" onSubmit={(e) => submit(true, e)} onKeyDown={(e) => {
        if (e.key === "Escape" && !pending) { e.preventDefault(); close(); }
      }}>
        <p className="text-xs text-muted">
          Manual cordon stops new intake. Assigned work can continue subject to other restrictions.
        </p>
        <Field label="Reason (optional)">
          <textarea id={`${id}-reason`} className="w-full rounded-lg border border-edge bg-raised px-3 py-2 text-sm text-fg"
            value={reason} disabled={disabled || pending}
            onChange={(e) => setReason(e.target.value)} />
        </Field>
        <div className="flex items-center gap-2">
          <Button size="sm" disabled={disabled || pending} type="submit">Cordon</Button>
          <Button type="button" size="sm" variant="ghost" disabled={pending} onClick={close}>Cancel</Button>
        </div>
      </form>}
      {error && <div ref={alert} role="alert" tabIndex={-1} className="text-sm text-danger">{error}</div>}
    </div>
  ) : null;
  return children(button, panel);
}
