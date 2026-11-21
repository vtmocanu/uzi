// Per-repo run environment variables (PRD #4416 M1): list, add, edit, disable/enable
// and delete. A secret's value is write-only: the API never returns it, the edit form
// never prefills it, and nothing here renders one back.

import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ClipboardEvent } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, isVaultLocked, type RunEnvList, type RunEnvVar, type RunEnvWriteInput } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Alert, Badge, Button, Input, Textarea, Toggle } from "./ui";

const VAULT_LOCKED = "Your vault is locked — unlock it with the banner above, then save again.";

interface Draft {
  // null while adding; the variable's name while editing it.
  editing: RunEnvVar | null;
  name: string;
  value: string;
  secret: boolean;
  description: string;
  // Multiline swaps the single-line input for a textarea; held only in React state.
  multiline: boolean;
}

const EMPTY_DRAFT: Draft = { editing: null, name: "", value: "", secret: true, description: "", multiline: false };

const hasLineBreak = (s: string) => /[\r\n]/.test(s);

// The "keep the stored value" hint holds only while an existing secret stays secret.
// Switching a secret to plain always sends the field's value, so an empty field
// replaces the secret with an empty plain value.
function valuePlaceholder(draft: Draft): string | undefined {
  if (!draft.editing?.secret) return undefined;
  return draft.secret ? "Leave empty to keep the stored value" : "Enter the new plain value; an empty field saves an empty value";
}

interface FieldProblem {
  field: string;
  rule: string;
}

// A 422 from the env endpoints names the field and the rule it broke.
function fieldProblem(err: unknown): FieldProblem | null {
  if (!(err instanceof ApiError) || err.status !== 422) return null;
  const body = err.body as { field?: unknown; rule?: unknown } | null;
  if (typeof body?.field !== "string" || typeof body.rule !== "string") return null;
  return { field: body.field, rule: body.rule };
}

function writeErrorMessage(err: unknown, fallback: string): string {
  if (isVaultLocked(err)) return VAULT_LOCKED;
  return errorMessage(err, fallback);
}

// A write refused because the owner's retired secret versions are at their limit answers 409
// env_history_full, optionally naming the runs that keep versions sealed (held_by_runs, at most 20,
// absent or empty when none is known). Returns those run ids, or null for any other error.
function historyFullRuns(err: unknown): string[] | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const body = err.body as { code?: unknown; held_by_runs?: unknown } | null;
  if (body?.code !== "env_history_full") return null;
  return Array.isArray(body.held_by_runs) ? body.held_by_runs.filter((id): id is string => typeof id === "string") : [];
}

function stateBadge(v: RunEnvVar) {
  if (v.state === "disabled") return <Badge tone="warning">Disabled</Badge>;
  if (v.state === "pending_deletion") return <Badge tone="neutral">Pending deletion</Badge>;
  return <Badge tone="ok">Enabled</Badge>;
}

export function RepoEnvPanel({
  repoId,
  repoLabel,
  onLiveCount,
}: {
  repoId: string;
  repoLabel: string;
  /** Reports the live variable count (enabled plus disabled) so the repo row stays current. */
  onLiveCount: (n: number) => void;
}) {
  const [list, setList] = useState<RunEnvList | null>(null);
  const [loadError, setLoadError] = useState("");
  const [draft, setDraft] = useState<Draft | null>(null);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<FieldProblem | null>(null);
  const [formError, setFormError] = useState("");
  const [rowError, setRowError] = useState("");
  const [notice, setNotice] = useState("");
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  // Run ids named by the latest env_history_full refusal; null when the last write was not one.
  const [heldByRuns, setHeldByRuns] = useState<string[] | null>(null);

  // Held in a ref so a parent that passes a fresh callback each render never reloads the list.
  const onLiveCountRef = useRef(onLiveCount);
  onLiveCountRef.current = onLiveCount;

  const load = useCallback(async () => {
    try {
      const next = await api.listRepoEnv(repoId);
      setList(next);
      setLoadError("");
      onLiveCountRef.current(next.counts.total - next.counts.pending_deletion);
    } catch (err) {
      setLoadError(errorMessage(err, "Failed to load the environment variables"));
    }
  }, [repoId]);

  useEffect(() => {
    void load();
  }, [load]);

  // After an intercepted paste, focus the value field with the caret after the pasted text. The
  // field may have been swapped for the textarea or may keep its input; the counter is bumped on
  // every intercepted paste so the effect runs even when the value itself did not change.
  const caretAfterPaste = useRef<number | null>(null);
  const [pasteSeq, setPasteSeq] = useState(0);
  useLayoutEffect(() => {
    if (caretAfterPaste.current === null) return;
    const el = document.getElementById("env-value") as HTMLInputElement | HTMLTextAreaElement | null;
    if (el) {
      el.focus();
      el.setSelectionRange(caretAfterPaste.current, caretAfterPaste.current);
    }
    caretAfterPaste.current = null;
  }, [pasteSeq]);

  const startAdd = () => {
    setDraft({ ...EMPTY_DRAFT });
    setProblem(null);
    setFormError("");
    setNotice("");
  };

  const startEdit = (v: RunEnvVar) => {
    // A secret's value is never prefilled; a plain value is, unless the vault withholds it.
    const value = v.secret ? "" : (v.value ?? "");
    setDraft({ editing: v, name: v.name, value, secret: v.secret, description: v.description, multiline: hasLineBreak(value) });
    setProblem(null);
    setFormError("");
    setNotice("");
  };

  // Records an env_history_full refusal so the held run ids render; clears them for any other outcome.
  const noteHistoryFull = (err: unknown) => setHeldByRuns(historyFullRuns(err));

  const closeForm = () => {
    setDraft(null);
    setProblem(null);
    setFormError("");
  };

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    setProblem(null);
    setFormError("");
    setNotice("");
    setHeldByRuns(null);
    try {
      const input: RunEnvWriteInput = { description: draft.description };
      const orig = draft.editing;
      if (!orig) {
        input.value = draft.value;
        input.secret = draft.secret;
      } else {
        const flagChanged = draft.secret !== orig.secret;
        if (flagChanged) input.secret = draft.secret;
        // Secret-to-plain always sends the value (the server refuses it without one). Plain-to-secret
        // and plain-to-plain send it only when edited from the original, so a value the vault
        // withholds (prefilled "") is resealed by the server instead of overwritten with "". A
        // secret that stays secret sends one only when re-entered.
        const sendValue =
          flagChanged && !draft.secret ? true : draft.secret && orig.secret ? draft.value !== "" : draft.value !== (orig.value ?? "");
        if (sendValue) input.value = draft.value;
      }
      await api.putRepoEnv(repoId, draft.name, input);
      setDraft(null);
      setNotice(`Saved ${draft.name}`);
      await load();
    } catch (err) {
      noteHistoryFull(err);
      const p = fieldProblem(err);
      if (p) setProblem(p);
      else setFormError(writeErrorMessage(err, "Failed to save the variable"));
    } finally {
      setBusy(false);
    }
  };

  const toggleDisabled = async (v: RunEnvVar) => {
    setBusy(true);
    setRowError("");
    setNotice("");
    setHeldByRuns(null);
    try {
      if (v.state === "disabled") await api.enableRepoEnv(repoId, v.name);
      else await api.disableRepoEnv(repoId, v.name);
      await load();
    } catch (err) {
      noteHistoryFull(err);
      setRowError(writeErrorMessage(err, `Failed to update ${v.name}`));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async (name: string) => {
    setBusy(true);
    setRowError("");
    setNotice("");
    setHeldByRuns(null);
    try {
      const res = await api.deleteRepoEnv(repoId, name);
      setConfirmDelete(null);
      setNotice(res.state === "pending_deletion" ? `${name}: Pending deletion, kept to mask runs` : `Deleted ${name}`);
      await load();
    } catch (err) {
      noteHistoryFull(err);
      setRowError(writeErrorMessage(err, `Failed to delete ${name}`));
    } finally {
      setBusy(false);
    }
  };

  // A single-line input strips line breaks from a pasted value. Catch a paste that carries
  // one and insert it at the selection (replacing any selected text). One trailing line break
  // (a copied line's newline) is ignored when deciding: if no break remains, insert the text
  // without it and stay single-line. Otherwise insert the whole paste as LF, trailing break
  // included, and switch to the textarea so the value survives. The layout effect above then
  // restores focus and the caret.
  const onSingleLinePaste = (e: ClipboardEvent<HTMLInputElement>) => {
    const text = e.clipboardData.getData("text");
    if (!draft || !hasLineBreak(text)) return;
    e.preventDefault();
    const input = e.currentTarget;
    const start = input.selectionStart ?? draft.value.length;
    const end = input.selectionEnd ?? start;
    const trimmed = text.replace(/(\r\n|\r|\n)$/, "");
    const switchToMultiline = hasLineBreak(trimmed);
    const pasted = switchToMultiline ? text.replace(/\r\n?/g, "\n") : trimmed;
    caretAfterPaste.current = start + pasted.length;
    setPasteSeq((n) => n + 1);
    setDraft({
      ...draft,
      multiline: switchToMultiline ? true : draft.multiline,
      value: draft.value.slice(0, start) + pasted + draft.value.slice(end),
    });
  };

  const lockedOn = !!draft && draft.multiline && hasLineBreak(draft.value);
  const counts = list?.counts;
  const fieldErr = (field: string) =>
    problem && problem.field === field ? (
      <p role="alert" className="text-xs text-danger">
        {problem.field}: {problem.rule}
      </p>
    ) : null;

  return (
    <div role="group" aria-label={`Environment variables for ${repoLabel}`} className="space-y-3 rounded-xl border border-edge bg-raised/20 p-4">
      <p className="text-sm text-fg">
        <span className="font-medium">Environment variables for {repoLabel}</span>
        {counts && (
          <span className="ml-2 text-xs text-muted">
            {counts.total} of {counts.limit}, {counts.disabled} disabled, {counts.pending_deletion} pending deletion
          </span>
        )}
      </p>
      <div className="space-y-1 text-xs text-muted">
        <p>
          The agent can read every value, secret or not. uzi only sets these variables in the run&rsquo;s environment; any login
          or tool configuration is done by the agent or the repo&rsquo;s scripts. Variables are set in the environment of every
          command a repo&rsquo;s issue, CI-fix, prompt, self-improvement, rework and task runs execute; review, isolated, chat, judge, job and cross-check runs do not receive them.
          Secret values are redacted from run output and checked before anything is pushed.
        </p>
        <p>Disabling or deleting a variable does not revoke the credential; revoke it at the provider.</p>
      </div>

      {loadError && <Alert message={loadError} />}
      {list?.vault_locked && (
        <Alert tone="warning" message="Your vault is locked: plain values are withheld until you unlock it with the banner above." />
      )}
      {notice && <Alert tone="success" message={notice} />}
      {rowError && <Alert message={rowError} />}
      {heldByRuns && (
        <div className="space-y-1 text-xs text-muted">
          <p>
            Retired secret versions are kept while the runs holding them are active, for at least 24 hours after they end, and longer
            while their retained work is kept. You can still Disable or Delete a variable.
          </p>
          {heldByRuns.length > 0 ? (
            <ul className="flex flex-wrap gap-x-3 gap-y-1">
              {heldByRuns.map((id) => (
                <li key={id}>
                  <Link to={`/runs/${encodeURIComponent(id)}`} className="font-mono underline">
                    {id}
                  </Link>
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      )}

      {list && list.variables.length === 0 && !draft && (
        <p className="text-sm text-faint">No variables yet. Add one to set it in this repo&rsquo;s run environment.</p>
      )}

      {list && list.variables.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="text-xs text-muted">
              <tr>
                <th className="px-2 py-1 font-medium">Name</th>
                <th className="px-2 py-1 font-medium">Kind</th>
                <th className="px-2 py-1 font-medium">Value</th>
                <th className="px-2 py-1 font-medium">Description</th>
                <th className="px-2 py-1 font-medium">Updated</th>
                <th className="px-2 py-1 font-medium">State</th>
                <th className="px-2 py-1 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {list.variables.map((v) => {
                const pending = v.state === "pending_deletion";
                return (
                  <tr key={v.var_id} className="border-t border-edge align-top">
                    <td className="px-2 py-2 font-mono text-xs">{v.name}</td>
                    <td className="px-2 py-2">{v.secret ? "Secret" : "Plain"}</td>
                    <td className="px-2 py-2 font-mono text-xs">
                      {v.secret ? (
                        <span aria-label="Secret value, set">••••</span>
                      ) : v.value_withheld ? (
                        <span className="text-faint">withheld</span>
                      ) : (
                        v.value
                      )}
                    </td>
                    <td className="px-2 py-2 text-xs text-muted">{v.description}</td>
                    <td className="px-2 py-2 text-xs text-muted">{new Date(v.updated_at).toLocaleString()}</td>
                    <td className="px-2 py-2">{stateBadge(v)}</td>
                    <td className="px-2 py-2">
                      {pending ? (
                        <span className="text-xs text-faint">Kept to mask runs</span>
                      ) : confirmDelete === v.name ? (
                        <div className="flex flex-wrap justify-end gap-2">
                          <span className="text-xs text-fg">Delete {v.name}?</span>
                          <Button variant="danger" size="sm" disabled={busy} onClick={() => doDelete(v.name)}>
                            Confirm delete
                          </Button>
                          <Button variant="ghost" size="sm" disabled={busy} onClick={() => setConfirmDelete(null)}>
                            Cancel
                          </Button>
                        </div>
                      ) : (
                        <div className="flex flex-wrap justify-end gap-2">
                          <Button variant="secondary" size="sm" disabled={busy} aria-label={`Edit ${v.name}`} onClick={() => startEdit(v)}>
                            Edit
                          </Button>
                          <Button
                            variant="secondary"
                            size="sm"
                            disabled={busy}
                            aria-label={`${v.state === "disabled" ? "Enable" : "Disable"} ${v.name}`}
                            onClick={() => toggleDisabled(v)}
                          >
                            {v.state === "disabled" ? "Enable" : "Disable"}
                          </Button>
                          <Button
                            variant="danger"
                            size="sm"
                            disabled={busy}
                            aria-label={`Delete ${v.name}`}
                            onClick={() => {
                              setRowError("");
                              setNotice("");
                              setConfirmDelete(v.name);
                            }}
                          >
                            Delete
                          </Button>
                        </div>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {draft ? (
        <div role="group" aria-label={draft.editing ? `Edit ${draft.name}` : "Add variable"} className="space-y-3 border-t border-edge pt-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <label htmlFor="env-name" className="text-xs text-muted">
                Name
              </label>
              <Input
                id="env-name"
                value={draft.name}
                disabled={draft.editing !== null}
                autoComplete="off"
                data-1p-ignore
                data-lpignore="true"
                data-bwignore
                spellCheck={false}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              />
              {fieldErr("name")}
            </div>
            <div className="space-y-1">
              <label htmlFor="env-value" className="text-xs text-muted">
                Value
              </label>
              {/* Single-line (default) uses a native password input for secrets. Multiline uses a
                  textarea, which keeps line breaks (PEM keys, JSON credentials) but can only be
                  concealed by CSS masking. Neither opts in to form-state restoration. */}
              {draft.multiline ? (
                <Textarea
                  id="env-value"
                  rows={3}
                  wrap="off"
                  value={draft.value}
                  className={draft.secret ? "font-mono [-webkit-text-security:disc]" : "font-mono"}
                  autoComplete="off"
                  data-1p-ignore
                  data-lpignore="true"
                  data-bwignore
                  autoCorrect="off"
                  autoCapitalize="off"
                  spellCheck={false}
                  placeholder={valuePlaceholder(draft)}
                  onChange={(e) => setDraft({ ...draft, value: e.target.value })}
                />
              ) : (
                <Input
                  id="env-value"
                  type={draft.secret ? "password" : "text"}
                  value={draft.value}
                  className="font-mono"
                  autoComplete={draft.secret ? "new-password" : "off"}
                  data-1p-ignore
                  data-lpignore="true"
                  data-bwignore
                  autoCorrect="off"
                  autoCapitalize="off"
                  spellCheck={false}
                  placeholder={valuePlaceholder(draft)}
                  onChange={(e) => setDraft({ ...draft, value: e.target.value })}
                  onPaste={onSingleLinePaste}
                />
              )}
              {fieldErr("value")}
              <div className="flex items-center gap-2 text-xs text-muted">
                <Toggle
                  label="Multiline"
                  checked={draft.multiline}
                  disabled={lockedOn}
                  focusableWhenDisabled
                  aria-describedby={
                    [lockedOn && "env-multiline-locked", draft.multiline && "env-multiline-help"].filter(Boolean).join(" ") || undefined
                  }
                  onChange={(next) => setDraft({ ...draft, multiline: next })}
                />
                <span>Multiline value</span>
              </div>
              {lockedOn && (
                <p id="env-multiline-locked" className="text-xs text-muted">
                  Multiline stays on while the value contains a line break.
                </p>
              )}
              {draft.multiline && (
                <p id="env-multiline-help" className="text-xs text-muted">
                  {draft.secret && "Masking a multiline secret is not portable across browsers. "}Line endings are stored as LF.
                </p>
              )}
            </div>
          </div>
          <div className="space-y-1">
            <label htmlFor="env-description" className="text-xs text-muted">
              Description
            </label>
            <Input
              id="env-description"
              value={draft.description}
              autoComplete="off"
              onChange={(e) => setDraft({ ...draft, description: e.target.value })}
            />
            {fieldErr("description")}
          </div>
          <div className="flex items-center gap-2 text-sm text-fg">
            <Toggle label="Secret" checked={draft.secret} onChange={(next) => setDraft({ ...draft, secret: next })} />
            <span>Secret: the value is never shown again after saving</span>
          </div>
          {problem && !["name", "value", "description"].includes(problem.field) && (
            <p role="alert" className="text-xs text-danger">
              {problem.field}: {problem.rule}
            </p>
          )}
          {formError && <Alert message={formError} />}
          <div className="flex gap-2">
            <Button size="sm" disabled={busy} onClick={save}>
              {busy ? "Saving…" : "Save variable"}
            </Button>
            <Button variant="ghost" size="sm" disabled={busy} onClick={closeForm}>
              Cancel
            </Button>
          </div>
        </div>
      ) : (
        <Button size="sm" disabled={busy || !list} onClick={startAdd}>
          Add variable
        </Button>
      )}
    </div>
  );
}
