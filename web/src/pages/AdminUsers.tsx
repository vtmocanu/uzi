import { useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, type User } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Badge, Button, Card, ListSkeleton } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail, maskName } from "../lib/demoMask";

export function AdminUsers() {
  const demo = useDemoMode();
  const { user: me } = useAuth();
  // users is seeded by the load but ALSO updated optimistically by the toggle
  // handlers below (setUsers(prev => …)), so it stays a local state and the fetcher
  // seeds it as a side effect rather than riding in the hook's data bundle.
  const [users, setUsers] = useState<User[]>([]);
  // Kept local: the toggle handlers below still set this on failure, so it is merged
  // with the hook's load error at the one page-level Alert.
  const [error, setError] = useState("");
  const [busyId, setBusyId] = useState<string | null>(null);
  const [judgeBusyId, setJudgeBusyId] = useState<string | null>(null);
  const [ciAutofixBusyId, setCiAutofixBusyId] = useState<string | null>(null);
  // Whether the admin has ENFORCED the judge instance-wide (PRD #69 M4). Under
  // enforced mode the per-user judge flag is bypassed at enqueue (Decision 3), so the
  // per-user toggle in this table is INERT — greyed and annotated, not removed. Read
  // best-effort from the admin settings alongside the user list; a failed read leaves
  // it false so the toggle stays live rather than falsely claiming to be inert.
  const [judgeEnforceAll, setJudgeEnforceAll] = useState(false);
  // Whether the instance-wide CI-autofix kill-switch is OFF (PRD #914 M1). When the
  // global ci_autofix_enabled is "false" no user's pipeline is auto-fixed regardless
  // of their per-user flag, so the per-user toggle in this table is INERT — greyed and
  // annotated, not removed, exactly like judgeEnforceAll above. Read best-effort from
  // the admin settings; a failed read leaves it false so the toggle stays live rather
  // than falsely claiming to be inert.
  const [ciAutofixInstanceOff, setCiAutofixInstanceOff] = useState(false);

  const { loading, error: loadError } = useAsyncData(
    async ({ isCurrent }) => {
      // The user list is the page; a failure here is fatal (the fetcher rethrows and
      // the hook turns it into loadError). The settings read is best-effort per the
      // judgeEnforceAll comment above: it only decides whether the per-user toggle is
      // annotated inert, so a settings-endpoint blip must NOT blank the whole table.
      // Keep it OUT of the fatal path — leave enforce=false on error.
      const { users } = await api.listUsers();
      if (!isCurrent()) return;
      setUsers(users);
      try {
        const { settings } = await api.getSettings();
        if (isCurrent()) {
          setJudgeEnforceAll(settings.judge_enforce_all === "true");
          setCiAutofixInstanceOff(settings.ci_autofix_enabled === "false");
        }
      } catch {
        if (isCurrent()) {
          setJudgeEnforceAll(false);
          setCiAutofixInstanceOff(false);
        }
      }
    },
    [],
    { fallback: "Failed to load users" },
  );

  const toggle = async (u: User) => {
    setError("");
    setBusyId(u.id);
    try {
      const { user } = await api.setUserActive(u.id, !u.is_active);
      setUsers((prev) => prev.map((x) => (x.id === user.id ? user : x)));
    } catch (err) {
      setError(errorMessage(err, "Update failed"));
    } finally {
      setBusyId(null);
    }
  };

  // Admin per-user run-judge toggle (PRD #46 Decision 7): force any user's opt-in on
  // or off. The server sets the flag on the TARGET's own account, so the judge still
  // only ever spends that user's tokens — this is the "force-disable per user" control.
  const toggleJudge = async (u: User) => {
    setError("");
    setJudgeBusyId(u.id);
    try {
      const { user } = await api.setUserJudgeEnabled(u.id, !u.judge_enabled);
      setUsers((prev) => prev.map((x) => (x.id === user.id ? user : x)));
    } catch (err) {
      setError(errorMessage(err, "Update failed"));
    } finally {
      setJudgeBusyId(null);
    }
  };

  // Admin per-user CI-autofix toggle (PRD #71): force any user's opt-in on or off.
  // The server sets the flag on the TARGET's own account, so the auto-fix still only
  // ever spends that user's tokens — this is the "force-disable per user" control.
  const toggleCIAutofix = async (u: User) => {
    setError("");
    setCiAutofixBusyId(u.id);
    try {
      // NULL (inherit) and true both read as currently-on (PRD #914 M3); the admin
      // control is an explicit force-toggle, so it always sends the opposite bool
      // (never clears to inherit — that is a self-service capability only).
      const currentlyOn = u.ci_autofix_enabled !== false;
      const { user } = await api.setUserCIAutofixEnabled(u.id, !currentlyOn);
      setUsers((prev) => prev.map((x) => (x.id === user.id ? user : x)));
    } catch (err) {
      setError(errorMessage(err, "Update failed"));
    } finally {
      setCiAutofixBusyId(null);
    }
  };

  return (
    <AdminShell description="Deactivating a user blocks their login and immediately ends every active session.">
      {(error || loadError) && <Alert message={error || loadError} />}
      {loading ? (
        <ListSkeleton rows={4} />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Email</th>
                  <th className="px-4 py-3 font-medium">Name</th>
                  <th className="px-4 py-3 font-medium">Role</th>
                  <th className="px-4 py-3 font-medium">Status</th>
                  <th className="px-4 py-3 font-medium" title="Run-judge opt-in: reviews this user's finished runs on their Anthropic token">
                    Judge
                  </th>
                  <th className="px-4 py-3 font-medium" title="CI-autofix opt-in: auto-fixes this user's failed pipelines on their Anthropic token">
                    CI autofix
                  </th>
                  <th className="px-4 py-3 font-medium">Last login</th>
                  <th className="px-4 py-3 text-right font-medium">Action</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {users.map((u) => (
                  <tr key={u.id} className="transition-colors hover:bg-raised/30">
                    <td className="px-4 py-3 text-fg">{maskEmail(u.email, demo)}</td>
                    {/* ?? only catches null/undefined; an empty-string name would
                        render blank, so fall back on a trimmed-empty name too. */}
                    <td className="px-4 py-3 text-muted">{maskName(u.display_name?.trim(), demo) || "—"}</td>
                    <td className="px-4 py-3">
                      {u.is_admin ? <Badge tone="brand">Admin</Badge> : <Badge>User</Badge>}
                    </td>
                    <td className="px-4 py-3">
                      <Badge tone={u.is_active ? "ok" : "danger"} dot>
                        {u.is_active ? "Active" : "Deactivated"}
                      </Badge>
                    </td>
                    <td className="px-4 py-3">
                      <div className={`flex items-center gap-2 ${judgeEnforceAll ? "opacity-50" : ""}`}>
                        <Badge tone={u.judge_enabled ? "ok" : "neutral"} dot>
                          {u.judge_enabled ? "On" : "Off"}
                        </Badge>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={judgeBusyId === u.id || judgeEnforceAll}
                          onClick={() => toggleJudge(u)}
                        >
                          {u.judge_enabled ? "Disable" : "Enable"}
                        </Button>
                      </div>
                      {judgeEnforceAll && (
                        <p className="mt-1 text-xs text-faint">
                          Inert: enforced mode judges every run regardless of this flag.
                        </p>
                      )}
                    </td>
                    <td className="px-4 py-3">
                      <div className={`flex items-center gap-2 ${ciAutofixInstanceOff ? "opacity-50" : ""}`}>
                        <Badge
                          tone={!ciAutofixInstanceOff && u.ci_autofix_enabled !== false ? "ok" : "neutral"}
                          dot
                        >
                          {!ciAutofixInstanceOff && u.ci_autofix_enabled !== false ? "On" : "Off"}
                        </Badge>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={ciAutofixBusyId === u.id || ciAutofixInstanceOff}
                          onClick={() => toggleCIAutofix(u)}
                        >
                          {!ciAutofixInstanceOff && u.ci_autofix_enabled !== false ? "Disable" : "Enable"}
                        </Button>
                      </div>
                      {ciAutofixInstanceOff && (
                        <p className="mt-1 text-xs text-faint">
                          Inert: the instance kill-switch is off; autofix is disabled for every user.
                        </p>
                      )}
                    </td>
                    <td className="px-4 py-3 text-muted">
                      {u.last_login ? new Date(u.last_login).toLocaleString() : "—"}
                    </td>
                    <td className="px-4 py-3 text-right">
                      {u.id === me?.id ? (
                        <span className="text-xs text-faint">you</span>
                      ) : (
                        <Button
                          variant={u.is_active ? "danger" : "secondary"}
                          size="sm"
                          disabled={busyId === u.id}
                          onClick={() => toggle(u)}
                        >
                          {u.is_active ? "Deactivate" : "Activate"}
                        </Button>
                      )}
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
