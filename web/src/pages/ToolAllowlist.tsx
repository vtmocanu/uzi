// Admin → Tool allowlist (PRD #18 M4): the packages a user may put in a repo's
// tool profile. Admin-only page (gated by AdminRoute). The version policy is
// optional: leave "Pinned version" blank to allow any version.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type ToolAllowlistEntry } from "../lib/api";
import { Alert, Button, Card, EmptyState, Field, Input, ListSkeleton } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { PackageIcon } from "../components/icons";

export function ToolAllowlist() {
  const [entries, setEntries] = useState<ToolAllowlistEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [name, setName] = useState("");
  const [pinned, setPinned] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const { allowlist } = await api.listToolAllowlist();
      setEntries(allowlist);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load the allowlist");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      await api.createToolAllowlistEntry({
        name: name.trim(),
        pinned_version: pinned.trim() || undefined,
        note: note.trim() || undefined,
      });
      setName("");
      setPinned("");
      setNote("");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to add the package");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string) => {
    setError("");
    try {
      await api.deleteToolAllowlistEntry(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to remove the package");
    }
  };

  return (
    <AdminShell description="Packages a user may add to a repo's tool profile. The worker installs them with devbox before a run. Never add a pre-authenticated, credential-bearing CLI.">

      {error && <Alert message={error} />}

      <Card className="space-y-4">
        <form onSubmit={add} className="flex flex-wrap items-end gap-3">
          <div className="min-w-[12rem] flex-1">
            <Field label="Package name">
              <Input placeholder="e.g. kubectl" value={name} onChange={(e) => setName(e.target.value)} />
            </Field>
          </div>
          <div className="min-w-[8rem]">
            <Field label="Pinned version (optional)">
              <Input placeholder="any" value={pinned} onChange={(e) => setPinned(e.target.value)} />
            </Field>
          </div>
          <div className="min-w-[12rem] flex-1">
            <Field label="Note (optional)">
              <Input placeholder="why it's allowed" value={note} onChange={(e) => setNote(e.target.value)} />
            </Field>
          </div>
          <Button type="submit" disabled={busy || name.trim() === ""}>
            {busy ? "Adding…" : "Add package"}
          </Button>
        </form>
      </Card>

      {loading ? (
        <ListSkeleton rows={4} />
      ) : entries.length === 0 ? (
        <EmptyState
          icon={<PackageIcon />}
          title="No packages allowed yet"
          description="Add a package above before users can select it in a repo's tool profile."
        />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Package</th>
                  <th className="px-4 py-3 font-medium">Version policy</th>
                  <th className="px-4 py-3 font-medium">Note</th>
                  <th className="px-4 py-3 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {entries.map((e) => (
                  <tr key={e.id} className="transition-colors hover:bg-raised/30">
                    <td className="px-4 py-3 font-mono text-fg">{e.name}</td>
                    <td className="px-4 py-3 text-muted">
                      {e.pinned_version ? <code className="font-mono text-xs">= {e.pinned_version}</code> : "any"}
                    </td>
                    <td className="px-4 py-3 text-muted">{e.note ?? "—"}</td>
                    <td className="px-4 py-3 text-right">
                      <Button variant="danger" size="sm" onClick={() => remove(e.id)}>
                        Remove
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </AdminShell>
  );
}
