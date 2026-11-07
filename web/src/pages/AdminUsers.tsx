import { useEffect, useRef, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, type AccountResetLink, type User } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Badge, Button, Card, ListSkeleton } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { Modal } from "../components/Modal";
import { XIcon } from "../components/icons";
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
  // The user an admin is issuing a reset link for (PRD #3989); null when no dialog is open.
  const [resetTarget, setResetTarget] = useState<User | null>(null);

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
        <Card flush>
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
                        <div className="flex items-center justify-end gap-2">
                          {/* A reset link needs a live account to land on: hidden for an
                              inactive user (the server refuses it too) and for yourself. */}
                          {u.is_active && (
                            <Button variant="ghost" size="sm" onClick={() => setResetTarget(u)}>
                              Reset link
                            </Button>
                          )}
                          <Button
                            variant={u.is_active ? "danger" : "secondary"}
                            size="sm"
                            disabled={busyId === u.id}
                            onClick={() => toggle(u)}
                          >
                            {u.is_active ? "Deactivate" : "Activate"}
                          </Button>
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      {resetTarget && (
        <ResetLinkDialog
          user={resetTarget}
          label={maskEmail(resetTarget.email, demo)}
          onClose={() => setResetTarget(null)}
        />
      )}
    </AdminShell>
  );
}

// ResetLinkDialog issues a one-time account reset link (PRD #3989) in two steps: a
// confirmation that spells out what redeeming destroys, then the link itself, shown
// exactly once. Only the token's hash is stored, so closing the dialog drops the only
// copy; the admin issues a new link (which cancels this one) if it is lost.
function ResetLinkDialog({ user, label, onClose }: { user: User; label: string; onClose: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [issued, setIssued] = useState<AccountResetLink | null>(null);
  const [copied, setCopied] = useState(false);
  const resultRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (issued) resultRef.current?.focus();
  }, [issued]);

  // The token rides the URL fragment, never the path or a query (D9), so it stays out of
  // access logs and Referer headers.
  const url = issued ? `${window.location.origin}/reset#${issued.token}` : "";
  // The issue response's snapshot decides the admin warning once a link exists; before
  // that, the row's role is the best available answer.
  const targetIsAdmin = issued ? issued.target_is_admin : user.is_admin;

  const create = async () => {
    setError("");
    setBusy(true);
    try {
      setIssued(await api.createResetLink(user.id));
    } catch (err) {
      setError(errorMessage(err, "Could not create the reset link"));
    } finally {
      setBusy(false);
    }
  };

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the link stays visible to copy by hand.
    }
  };

  return (
    <Modal label={`Reset link for ${label}`} onClose={onClose} closeOnBackdrop={!busy && !issued}>
      <div className="my-8 w-full max-w-lg overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
        <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
          <div className="min-w-0">
            <h2 className="text-base font-semibold">
              {issued ? "Reset link created" : "Create a reset link?"}
            </h2>
            <p className="mt-0.5 truncate text-xs text-muted">{label}</p>
          </div>
          <button
            type="button"
            onClick={onClose}
            disabled={busy}
            aria-label="Close"
            className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
          >
            <XIcon />
          </button>
        </div>

        {issued ? (
          <div ref={resultRef} tabIndex={-1} className="space-y-4 px-5 py-5 outline-hidden">
            <p className="text-sm text-muted">
              Copy it now. It is shown <strong className="text-fg">only once</strong> and cannot be retrieved
              again; if it is lost, create a new link, which cancels this one.
            </p>
            <div className="flex items-center gap-2">
              <code
                aria-label="Reset link"
                className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap rounded-lg border border-edge bg-ink console px-3 py-2 font-mono text-sm text-ok"
              >
                {url}
              </code>
              <Button variant="secondary" onClick={copy}>
                {copied ? "Copied" : "Copy"}
              </Button>
            </div>
            <dl className="divide-y divide-edge rounded-lg border border-edge bg-raised/40 text-sm">
              <div className="flex justify-between gap-3 px-3 py-2">
                <dt className="text-muted">Expires</dt>
                <dd className="text-fg">{new Date(issued.expires_at).toLocaleString()}</dd>
              </div>
              <div className="flex justify-between gap-3 px-3 py-2">
                <dt className="text-muted">Resets</dt>
                <dd className="text-fg">
                  {issued.kind === "vault_only" ? "Vault only (single sign-on account)" : "Password and vault"}
                </dd>
              </div>
            </dl>
            {issued.kind === "vault_only" && (
              <p className="text-sm text-muted">
                This account has no password. The user signs in with single sign-on, then sets a new vault passphrase.
              </p>
            )}
            {targetIsAdmin && <AdminResetWarning kind={issued.kind} />}
          </div>
        ) : (
          <div className="space-y-4 px-5 py-5">
            {error && <Alert message={error} />}
            <p className="text-sm text-muted">
              Hand the link to this person yourself; uzi sends no email. It works once and expires an hour after
              you create it. When they redeem it:
            </p>
            <ul className="list-disc space-y-1.5 pl-5 text-sm text-fg">
              <li>
                Their model credentials (Anthropic and OpenAI tokens, Codex logins) and their vault contents are
                permanently deleted.
              </li>
              <li>
                All their browser sessions end. A CLI login they already approved but have not finished is refused; one
                still waiting for approval stays pending, but their old sessions can no longer approve it.
              </li>
              <li>Their queued Codex runs will fail; they must start new ones.</li>
            </ul>
            {targetIsAdmin && <AdminResetWarning kind={null} />}
          </div>
        )}

        <div className="flex items-center justify-end gap-2 border-t border-edge bg-ink/40 inset-panel px-5 py-3.5">
          {issued ? (
            <Button size="sm" onClick={onClose}>
              Done
            </Button>
          ) : (
            <>
              <Button variant="ghost" size="sm" disabled={busy} onClick={onClose}>
                Cancel
              </Button>
              <Button variant="dangerSolid" size="sm" disabled={busy} onClick={create}>
                {busy ? "Creating…" : "Create reset link"}
              </Button>
            </>
          )}
        </div>
      </div>
    </Modal>
  );
}

// AdminResetWarning words the admin caveat for what is known at that point. Before the link
// exists its kind is unknown (the row carries no has-password flag), so the pre-issue text
// holds for both kinds; the issued result knows the kind and states only what is true for it.
function AdminResetWarning({ kind }: { kind: AccountResetLink["kind"] | null }) {
  return (
    <div role="note" className="rounded-md border border-warn/40 bg-warn/10 p-3 text-sm text-warn">
      {kind === "password" && (
        <>
          This is an admin account. Whoever holds the link takes over an admin account: share it only with that
          person, over a trusted channel.
        </>
      )}
      {kind === "vault_only" && (
        <>
          This is an admin account. The link sets no password and gives no access to the account, but whoever holds
          it can delete the admin&apos;s credentials, vault and sessions: share it only with that person.
        </>
      )}
      {kind === null && (
        <>
          This is an admin account. If it has a password, whoever holds the link can set a new one and take the
          account over. Either way the link deletes the admin&apos;s credentials and vault: share it only with that
          person, over a trusted channel.
        </>
      )}
    </div>
  );
}
