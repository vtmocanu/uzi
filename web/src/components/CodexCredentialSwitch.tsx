import { useEffect, useId, useRef, useState } from "react";
import { api, type Run, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Alert, Button, Select } from "./ui";

function usable(secret: SecretMeta) {
  return secret.enabled === true && (
    (secret.kind === "codex_auth" && secret.codex_status === "linked") ||
    (secret.kind === "openai_api_key" && secret.codex_status === "static")
  );
}

export function CodexCredentialSwitch(props: {
  run: Run;
  canSteer: boolean;
  tokens?: SecretMeta[];
  triggerLabel?: string;
  onSwitched?: (run: Run) => void;
}) {
  const { run, canSteer } = props;
  // The API also checks review-target restrictions that are not exposed in Run.
  // A concurrent state change or additional server refusal stays visible below.
  const eligible = canSteer && run.harness === "codex" &&
    ["running", "awaiting_approval", "awaiting_input", "awaiting_followup"].includes(run.status) &&
    run.trigger_source !== "task_review" &&
    !["chat", "judge", "job", "cross_check"].includes(run.kind);
  // Unmounting the editor drops its fetch result and selection when eligibility changes.
  return eligible ? <CodexSwitchEditor key={run.id} {...props} /> : null;
}

function CodexSwitchEditor({
  run, tokens, triggerLabel = "Switch credential", onSwitched,
}: {
  run: Run;
  tokens?: SecretMeta[];
  triggerLabel?: string;
  onSwitched?: (run: Run) => void;
}) {
  const id = useId();
  const trigger = useRef<HTMLSpanElement>(null);
  const focusAfterClose = useRef(false);
  const [open, setOpen] = useState(false);
  const [secrets, setSecrets] = useState<SecretMeta[]>(tokens ?? []);
  const [loading, setLoading] = useState(tokens === undefined);
  const [fetchError, setFetchError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [selected, setSelected] = useState(run.codex_secret_id ?? "");
  const [explicit, setExplicit] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [warning, setWarning] = useState("");

  useEffect(() => {
    if (tokens !== undefined) {
      setSecrets(tokens);
      setLoading(false);
      setFetchError("");
      return;
    }
    let active = true;
    setLoading(true);
    setFetchError("");
    // One request per mount or explicit retry; failure does not retry automatically.
    void api.listSecrets().then(
      result => { if (active) setSecrets(result.secrets); },
      err => { if (active) setFetchError(errorMessage(err, "Could not load credentials")); },
    ).finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [tokens, attempt]);

  const choices = secrets.filter(s => s.kind === "codex_auth" || s.kind === "openai_api_key");
  const available = !loading && !fetchError && choices.some(s => s.id === selected && usable(s));
  useEffect(() => {
    if (!open && !busy && focusAfterClose.current) {
      trigger.current?.querySelector("button")?.focus();
      focusAfterClose.current = false;
    }
  }, [open, busy]);
  const close = () => { focusAfterClose.current = true; setOpen(false); };
  const submit = async () => {
    if (!explicit || !available || busy) return;
    setBusy(true);
    setError("");
    try {
      const result = await api.setRunCredential(run.id, { mode: "pinned", secret_id: selected });
      setWarning(result.warning ?? "");
      close();
      onSwitched?.(result.run);
    } catch (err) {
      setError(errorMessage(err, "Could not switch the credential"));
    } finally {
      setBusy(false);
    }
  };
  const missing = selected && !choices.some(s => s.id === selected);
  return (
    <div className="min-w-0 max-w-full space-y-2 text-xs">
      <span ref={trigger}>
        <Button size="sm" onClick={() => {
          setSelected(run.codex_secret_id ?? "");
          setExplicit(false);
          setError("");
          setOpen(true);
        }} disabled={busy} aria-expanded={open} aria-controls={id}>{triggerLabel}</Button>
      </span>
      {warning && <Alert tone="warning" message={warning} />}
      {open && <div id={id} className="min-w-0 space-y-3 rounded-lg border border-edge bg-surface p-3">
        <p className="text-muted">
          Switching may stop the current step, then continues with your work in the same workspace.
          It attempts to resume the conversation; if that is refused, a new conversation may start.
          An uncertain switch pauses the run for you to resume.
        </p>
        <label htmlFor={id + "-choice"} className="block font-medium">Codex credential</label>
        <Select id={id + "-choice"} className="min-w-0 max-w-full" value={selected}
          title={sanitizeLabel(choices.find(s => s.id === selected)?.label ?? run.codex_secret_label ?? "")}
          aria-describedby={id + "-status"} disabled={loading || busy || !!fetchError}
          onChange={event => { setSelected(event.target.value); setExplicit(true); }}>
          <option value="" disabled>Choose a credential</option>
          {missing && <option value={selected} disabled>
            {sanitizeLabel(run.codex_secret_label ?? "Current credential")} — unavailable
          </option>}
          {choices.map(s => <option key={s.id} value={s.id} disabled={!usable(s)}>
            {sanitizeLabel(s.label)} — {s.kind === "codex_auth" ? "Codex subscription" : "OpenAI API key"} · {s.id.slice(-8)}
            {!usable(s) ? " — unavailable" : ""}
          </option>)}
        </Select>
        <div id={id + "-status"} aria-live="polite">
          {loading ? "Loading credentials…" : fetchError ? fetchError :
            choices.length === 0 ? "No Codex credentials available." :
              !available && selected ? "Current credential is unavailable. Choose an available credential." :
                "Choose an available credential explicitly to confirm."}
        </div>
        {fetchError && <Button size="sm" onClick={() => setAttempt(a => a + 1)}>Retry</Button>}
        {error && <Alert tone="danger" message={error} />}
        <div className="flex flex-wrap gap-2">
          <Button size="sm" disabled={!explicit || !available || busy} onClick={() => void submit()}>
            {busy ? "Switching…" : "Switch to this credential"}
          </Button>
          <Button size="sm" variant="secondary" disabled={busy} onClick={close}>Cancel</Button>
        </div>
      </div>}
    </div>
  );
}
