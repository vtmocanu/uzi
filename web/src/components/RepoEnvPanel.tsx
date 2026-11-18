// Per-repo run environment variables (PRD #4416 M1): list, add, edit, disable/enable
// and delete. A secret's value is write-only: the API never returns it, the edit form
// never prefills it, and nothing here renders one back.

import { useCallback, useEffect, useRef, useState } from "react";
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
}

const EMPTY_DRAFT: Draft = { editing: null, name: "", value: "", secret: true, description: "" };

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

  const startAdd = () => {
    setDraft({ ...EMPTY_DRAFT });
    setProblem(null);
    setFormError("");
    setNotice("");
  };

  const startEdit = (v: RunEnvVar) => {
    // A secret's value is never prefilled; a plain value is, unless the vault withholds it.
    setDraft({ editing: v, name: v.name, value: v.secret ? "" : (v.value ?? ""), secret: v.secret, description: v.description });
    setProblem(null);
    setFormError("");
    setNotice("");
  };

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
    try {
      const input: RunEnvWriteInput = { description: draft.description };
      const orig = draft.editing;
      if (!orig) {
        input.value = draft.value;
        input.secret = draft.secret;
      } else {
        if (draft.secret !== orig.secret) input.secret = draft.secret;
        // Editing a secret without a value changes only the description (and the flag).
        const changed = draft.secret ? draft.value !== "" : draft.value !== (orig.value ?? "");
        if (changed) input.value = draft.value;
      }
      await api.putRepoEnv(repoId, draft.name, input);
      setDraft(null);
      setNotice(`Saved ${draft.name}`);
      await load();
    } catch (err) {
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
    try {
      if (v.state === "disabled") await api.enableRepoEnv(repoId, v.name);
      else await api.disableRepoEnv(repoId, v.name);
      await load();
    } catch (err) {
      setRowError(writeErrorMessage(err, `Failed to update ${v.name}`));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async (name: string) => {
    setBusy(true);
    setRowError("");
    setNotice("");
    try {
      const res = await api.deleteRepoEnv(repoId, name);
      setConfirmDelete(null);
      setNotice(res.state === "pending_deletion" ? `${name}: Pending deletion, kept to mask runs` : `Deleted ${name}`);
      await load();
    } catch (err) {
      setRowError(writeErrorMessage(err, `Failed to delete ${name}`));
    } finally {
      setBusy(false);
    }
  };

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
          or tool configuration is done by the agent or the repo&rsquo;s scripts. Variables are not yet injected into runs.
        </p>
        <p>Disabling or deleting a variable does not revoke the credential; revoke it at the provider.</p>
      </div>

      {loadError && <Alert message={loadError} />}
      {list?.vault_locked && (
        <Alert tone="warning" message="Your vault is locked: plain values are withheld until you unlock it with the banner above." />
      )}
      {notice && <Alert tone="success" message={notice} />}
      {rowError && <Alert message={rowError} />}

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
                spellCheck={false}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              />
              {fieldErr("name")}
            </div>
            <div className="space-y-1">
              <label htmlFor="env-value" className="text-xs text-muted">
                Value
              </label>
              {/* A textarea keeps line breaks exactly (PEM keys, JSON credentials); a secret is
                  concealed by CSS masking, since a password input would strip them. */}
              <Textarea
                id="env-value"
                rows={3}
                wrap="off"
                value={draft.value}
                className={draft.secret ? "font-mono [-webkit-text-security:disc]" : "font-mono"}
                autoComplete="off"
                autoCorrect="off"
                autoCapitalize="off"
                spellCheck={false}
                placeholder={draft.editing?.secret ? "Leave empty to keep the stored value" : undefined}
                onChange={(e) => setDraft({ ...draft, value: e.target.value })}
              />
              {fieldErr("value")}
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
