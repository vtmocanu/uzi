// Boards home: the projects the bot can see. Enabling one starts tracking its
// PRD issues on a kanban board.

import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, isHttpsUrl, type ForgeConnection, type Repo, type ToolAllowlistEntry } from "../lib/api";
import { repoFindings } from "../lib/privilege";
import { useAuth } from "../auth/AuthContext";
import { Alert, Badge, Button, Card, EmptyState, ListSkeleton, PageHeader, Select, Textarea, Toggle } from "../components/ui";
import { PipelineBadge } from "../components/PipelineBadge";
import { RepoSetupChip } from "../components/RepoSetupChip";
import { Modal } from "../components/Modal";
import { BoardIcon, XIcon } from "../components/icons";
import { DocLink } from "../components/DocLink";
import { DOC_REPO_AGENTS } from "../lib/doclinks";

export function Repos() {
  // The guardrail override write is admin-only (PRD #66 D8): a member sees the block
  // and a pointer to ask an admin, never an Allow/Revoke control.
  const { user } = useAuth();
  const isAdmin = user?.is_admin ?? false;
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [connectionId, setConnectionId] = useState("");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);
  // A 422 enable-guardrail refusal carries a violation list we render below the
  // page Alert, mirroring the identical 422 contract in ForgeSettings (PRD #345).
  const [enableViolations, setEnableViolations] = useState<string[] | null>(null);
  // The repo whose "Trusted repo" panel is currently expanded. The panel groups a
  // master control over two independently-revocable capabilities — Repo skills and
  // Repo instructions (PRD #246). It renders OUTSIDE the horizontally-scrolling
  // table so its security copy is never clipped at narrow widths.
  const [trustRepoId, setTrustRepoId] = useState<string | null>(null);
  // Enabling the master is a trust decision, so it is gated behind a confirm step;
  // this holds the repo whose enable-confirm is showing. Disabling (master or a
  // sub-toggle) is immediate, matching the existing opt-in patterns.
  const [confirmTrustId, setConfirmTrustId] = useState<string | null>(null);
  // Explicit per-repo remove (PRD #357): the disabled repo whose row-inline
  // "Confirm remove / Cancel" affordance is showing. Its own state, distinct from
  // confirmTrustId — that confirm renders in the panel OUTSIDE the table, this one
  // stays in the row's action cell.
  const [confirmRemoveId, setConfirmRemoveId] = useState<string | null>(null);
  // Per-capability busy state so each PATCH disables only its own control.
  const [skillsBusyId, setSkillsBusyId] = useState<string | null>(null);
  const [claudemdBusyId, setClaudemdBusyId] = useState<string | null>(null);
  const [trustBusyId, setTrustBusyId] = useState<string | null>(null);
  // Tool profile picker (PRD #18 M4): the repo whose package picker is open, the
  // admin allowlist (loaded once), the currently-selected package strings for the
  // open repo, and the in-flight save.
  const [toolsRepoId, setToolsRepoId] = useState<string | null>(null);
  const [allowlist, setAllowlist] = useState<ToolAllowlistEntry[] | null>(null);
  const [toolSelection, setToolSelection] = useState<Set<string>>(new Set());
  const [toolsBusy, setToolsBusy] = useState(false);
  // The guardrail Allow-anyway modal (PRD #66 M9, D8): the repo whose modal is open,
  // the admin's reason, and the in-flight POST. Revoke reuses overrideBusyId.
  const [allowRepoId, setAllowRepoId] = useState<string | null>(null);
  const [allowReason, setAllowReason] = useState("");
  const [allowBusy, setAllowBusy] = useState(false);
  // Submit error for the Allow-anyway modal, rendered INSIDE the dialog (a page-level
  // Alert would sit behind the backdrop, unseen). Distinct from the page `error`.
  const [allowError, setAllowError] = useState("");
  const [overrideBusyId, setOverrideBusyId] = useState<string | null>(null);
  // Focus management: remember the cell trigger so focus returns to it when the
  // panel closes, and move focus into the panel (its master switch) when it opens.
  const trustTriggerRef = useRef<HTMLButtonElement | null>(null);
  const trustPanelRef = useRef<HTMLDivElement | null>(null);
  // The enable-confirm block; when it opens, focus moves to its primary button
  // ("Mark as trusted", the first button inside) so a screen-reader user hears it —
  // the master switch's aria-checked stays false until the PATCH lands.
  const confirmTrustRef = useRef<HTMLDivElement | null>(null);
  // The row-inline remove-confirm block; when it opens, focus moves to its primary
  // button ("Confirm remove", the first button inside) for screen-reader users —
  // same a11y pattern as the trust confirm above.
  const confirmRemoveRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (trustRepoId) trustPanelRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [trustRepoId]);
  useEffect(() => {
    if (confirmTrustId) confirmTrustRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [confirmTrustId]);
  useEffect(() => {
    if (confirmRemoveId) confirmRemoveRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [confirmRemoveId]);

  const closeTrust = () => {
    setTrustRepoId(null);
    setConfirmTrustId(null);
    trustTriggerRef.current?.focus();
  };

  useEffect(() => {
    (async () => {
      try {
        const { connections } = await api.listConnections();
        setConnections(connections);
        if (connections.length > 0) setConnectionId(connections[0].id);
      } catch (err) {
        setError(err instanceof ApiError ? err.message : "Failed to load connections");
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  const loadProjects = useCallback(async (connId: string) => {
    if (!connId) return;
    setError("");
    setRefreshing(true);
    try {
      const { repos } = await api.listProjects(connId);
      setRepos(repos);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load projects");
    } finally {
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    if (connectionId) loadProjects(connectionId);
  }, [connectionId, loadProjects]);

  const toggle = async (repo: Repo) => {
    setError("");
    setEnableViolations(null);
    setBusyId(repo.id);
    try {
      const { repo: updated } = await api.setRepoEnabled(repo.id, !repo.enabled);
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
      setEnableViolations(null);
    } catch (err) {
      if (err instanceof ApiError && err.status === 422) {
        const body = err.body as { violations?: string[] } | null;
        setError(err.message);
        setEnableViolations(body?.violations ?? []);
      } else {
        setError(err instanceof ApiError ? err.message : "Update failed");
      }
    } finally {
      setBusyId(null);
    }
  };

  // Explicit per-repo remove (PRD #357), modeled on `toggle`: clear any error,
  // DELETE the repo, then drop it from local state so the row disappears. On a
  // failure (e.g. the server's 409 for an enabled/in-flight repo) surface the
  // message the same way `toggle` does. Reachable only from the row-inline confirm.
  const removeRepo = async (repo: Repo) => {
    setError("");
    setBusyId(repo.id);
    try {
      await api.deleteRepo(repo.id);
      setRepos((prev) => prev.filter((r) => r.id !== repo.id));
      setConfirmRemoveId(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Remove failed");
    } finally {
      setBusyId(null);
    }
  };

  // A repo is "trusted" when either capability is on; the master control is derived
  // from that, never stored (PRD #246 open question 1).
  const isTrusted = (repo: Repo) => repo.repo_skills_enabled || repo.repo_claudemd_enabled;

  // Repo skills sub-toggle → repo_skills_enabled. Immediate; refining an already-
  // trusted repo is not a new trust decision.
  const setRepoSkills = async (repo: Repo, enabled: boolean) => {
    setError("");
    setSkillsBusyId(repo.id);
    try {
      const { repo: updated } = await api.setRepoSkillsEnabled(repo.id, enabled);
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
    } finally {
      setSkillsBusyId(null);
    }
  };

  // Repo instructions sub-toggle → repo_claudemd_enabled. Immediate.
  const setRepoClaudemd = async (repo: Repo, enabled: boolean) => {
    setError("");
    setClaudemdBusyId(repo.id);
    try {
      const { repo: updated } = await api.setRepoClaudemdEnabled(repo.id, enabled);
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
    } finally {
      setClaudemdBusyId(null);
    }
  };

  // Master control → both trust flags in one request. Enabling defaults both on
  // (the trust decision, gated behind the confirm); disabling turns both off.
  const setRepoTrust = async (repo: Repo, enabled: boolean) => {
    setError("");
    setTrustBusyId(repo.id);
    try {
      const { repo: updated } = await api.setRepoTrustFlags(repo.id, {
        repo_skills_enabled: enabled,
        repo_claudemd_enabled: enabled,
      });
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
      setConfirmTrustId(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
    } finally {
      setTrustBusyId(null);
    }
  };

  // Master switch click: turning off is immediate; turning on reveals the confirm.
  const onMasterToggle = (repo: Repo) => {
    if (isTrusted(repo)) setRepoTrust(repo, false);
    else setConfirmTrustId(repo.id);
  };

  // The package string an allowlist entry contributes: name@version when the entry
  // pins a version, else the bare name (any version).
  const entryPkg = (e: ToolAllowlistEntry): string =>
    e.pinned_version ? `${e.name}@${e.pinned_version}` : e.name;

  const openTools = async (repo: Repo) => {
    if (toolsRepoId === repo.id) {
      setToolsRepoId(null);
      return;
    }
    setError("");
    setToolsBusy(true);
    try {
      const [profile, list] = await Promise.all([
        api.getRepoToolProfile(repo.id),
        allowlist ? Promise.resolve({ allowlist }) : api.listToolAllowlist(),
      ]);
      setAllowlist(list.allowlist);
      setToolSelection(new Set(profile.packages));
      setToolsRepoId(repo.id);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load the tool profile");
    } finally {
      setToolsBusy(false);
    }
  };

  const toggleTool = (pkg: string) => {
    setToolSelection((prev) => {
      const next = new Set(prev);
      if (next.has(pkg)) next.delete(pkg);
      else next.add(pkg);
      return next;
    });
  };

  const saveTools = async (repoId: string) => {
    setError("");
    setToolsBusy(true);
    try {
      await api.setRepoToolProfile(repoId, [...toolSelection]);
      setToolsRepoId(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save the tool profile");
    } finally {
      setToolsBusy(false);
    }
  };

  // Tier-2 opt-in (PRD #18 M5): trust the repo's own devbox.json packages. Applied
  // immediately (a trust decision, like the repo-skills toggle) and reflected in
  // repo state so the checkbox stays in sync.
  const setRepoDevbox = async (repo: Repo, enabled: boolean) => {
    setError("");
    setToolsBusy(true);
    try {
      const { repo: updated } = await api.setRepoDevboxOptIn(repo.id, enabled);
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
    } finally {
      setToolsBusy(false);
    }
  };

  // Open the Allow-anyway modal for a blocked repo (admin only). Resets the reason.
  const openAllow = (repo: Repo) => {
    setError("");
    setAllowReason("");
    setAllowError("");
    setAllowRepoId(repo.id);
  };

  // Close the Allow-anyway modal. A no-op while a POST is in flight so neither Escape
  // nor a backdrop click can dismiss a submitting form (matching the disabled ×).
  const closeAllow = () => {
    if (allowBusy) return;
    setAllowRepoId(null);
    setAllowError("");
  };

  // POST the admin per-repo override with the typed reason, then refetch so the
  // server-recomputed guardrail_blocked / guardrail_override drive the badge (the web
  // never re-derives the block rule). A blank reason is rejected client- and
  // server-side (D8).
  const submitAllow = async (repoId: string) => {
    const reason = allowReason.trim();
    if (!reason) return;
    setAllowError("");
    setAllowBusy(true);
    try {
      await api.setRepoGuardrailOverride(repoId, reason);
      setAllowRepoId(null);
      setAllowReason("");
      await loadProjects(connectionId);
    } catch (err) {
      setAllowError(err instanceof ApiError ? err.message : "Failed to allow the repo");
    } finally {
      setAllowBusy(false);
    }
  };

  // Revoke an active override (admin only): re-arms the guardrail immediately, then
  // refetch so the badge flips back to blocked/clean per the fresh server state.
  const revokeOverride = async (repo: Repo) => {
    setError("");
    setOverrideBusyId(repo.id);
    try {
      await api.clearRepoGuardrailOverride(repo.id);
      await loadProjects(connectionId);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to revoke the override");
    } finally {
      setOverrideBusyId(null);
    }
  };

  // The selected connection's latest privilege report drives the per-repo
  // findings badges (null until a check has run).
  const privilegeReport = connections.find((c) => c.id === connectionId)?.privilege_report ?? null;

  // The repo whose Trusted-repo panel is expanded (rendered below the table,
  // outside its horizontal scroll container).
  const trustRepo = repos.find((r) => r.id === trustRepoId) ?? null;
  // True while ANY of the three trust-related PATCHes (skills / instructions /
  // master) is in flight for the open repo. Each PATCH response replaces the whole
  // repo object, so two in flight at once can clobber each other with a stale
  // snapshot; disabling every trust control while one runs serializes them.
  const anyTrustBusy =
    trustRepo != null &&
    (skillsBusyId === trustRepo.id || claudemdBusyId === trustRepo.id || trustBusyId === trustRepo.id);
  // The repo whose tool-profile picker is expanded (rendered below the table).
  const toolsRepo = repos.find((r) => r.id === toolsRepoId) ?? null;
  // The repo whose Allow-anyway modal is open, and the exact block findings it will
  // accept — read from the connection report for DISPLAY only (the STATE decision is
  // r.guardrail_blocked; the block rule is never re-implemented here).
  const allowRepo = repos.find((r) => r.id === allowRepoId) ?? null;
  const allowFindings = allowRepo ? repoFindings(privilegeReport, allowRepo.id)?.violations ?? [] : [];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Boards"
        description={
          <>
            Projects your bot can see. Enable one to track its PRD issues on a board. See the{" "}
            <DocLink slug={DOC_REPO_AGENTS}>repo agents</DocLink> guide.
          </>
        }
        actions={
          connectionId ? (
            <Button variant="secondary" size="sm" disabled={refreshing} onClick={() => loadProjects(connectionId)}>
              {refreshing ? "Refreshing…" : "Refresh list"}
            </Button>
          ) : undefined
        }
      />

      {error && <Alert message={error} />}
      {enableViolations && (
        <Card className="border-danger/40 bg-danger/5">
          {/* role="alert" so the reasons are announced to assistive tech when they
              appear; Card does not forward a role, so it sits on the wrapper (PRD #345 M3). */}
          <div role="alert">
            <p className="text-sm font-medium text-danger">This repository was not enabled — the guardrail refused it:</p>
            <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-fg">
              {enableViolations.map((v, i) => (
                <li key={i}>{v}</li>
              ))}
            </ul>
            <p className="mt-3 text-sm text-muted">
              Fix the repository's default-branch protection on the forge so the bot cannot push or
              merge to it directly, then click Enable again.
            </p>
          </div>
        </Card>
      )}

      {loading ? (
        <ListSkeleton rows={4} />
      ) : connections.length === 0 ? (
        <EmptyState
          icon={<BoardIcon />}
          title="No forge connection yet"
          description="uzi needs a bot before it can see any projects."
          action={
            <Link to="/settings/forge">
              <Button size="sm">Connect the forge</Button>
            </Link>
          }
        />
      ) : (
        <>
          {connections.length > 1 && (
            <div className="max-w-md">
              <Select value={connectionId} onChange={(e) => setConnectionId(e.target.value)}>
                {connections.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.bot_username} — {c.base_url}
                  </option>
                ))}
              </Select>
            </div>
          )}

          <Card className="p-0">
            <div className="overflow-x-auto">
              <table className="w-full text-left text-sm">
                <thead className="border-b border-edge text-muted">
                  <tr>
                    <th className="px-4 py-3 font-medium">Project</th>
                    <th className="px-4 py-3 font-medium">Default branch</th>
                    <th className="px-4 py-3 font-medium">Status</th>
                    <th className="px-4 py-3 font-medium">Trusted repo</th>
                    <th className="px-4 py-3 font-medium">Tools</th>
                    <th className="px-4 py-3 text-right font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-edge">
                  {repos.length === 0 ? (
                    <tr>
                      <td colSpan={6} className="px-4 py-6 text-center text-faint">
                        {refreshing ? "Loading…" : "No projects found for this bot."}
                      </td>
                    </tr>
                  ) : (
                    repos.map((r) => (
                      <tr key={r.id} className="transition-colors hover:bg-raised/30">
                        <td className="px-4 py-3">
                          {isHttpsUrl(r.web_url) ? (
                            <a
                              href={r.web_url}
                              target="_blank"
                              rel="noreferrer"
                              className="font-medium text-fg hover:text-brand-hover"
                            >
                              {r.path_with_namespace}
                            </a>
                          ) : (
                            <span className="font-medium text-fg">{r.path_with_namespace}</span>
                          )}
                        </td>
                        <td className="px-4 py-3 font-mono text-xs text-muted">
                          <div className="flex flex-wrap items-center gap-1.5">
                            <span>{r.default_branch ?? "—"}</span>
                            {r.pipeline && <PipelineBadge pipeline={r.pipeline} />}
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          <div className="flex flex-wrap items-center gap-1.5">
                            <Badge tone={r.enabled ? "ok" : "neutral"} dot>
                              {r.enabled ? "Enabled" : "Disabled"}
                            </Badge>
                            {(() => {
                              const f = repoFindings(privilegeReport, r.id);
                              // PRD #66 M9 (D8): the badge STATE reads the SERVER's
                              // guardrail_blocked (the single Go block rule, override
                              // already applied) — never re-derived here. Three states:
                              // blocked, allowed-by-admin, and (fall-through) warn-only.
                              if (r.guardrail_blocked) {
                                // uzi REFUSES runs on this repo (the bot can push/merge
                                // to main, or it could not be verified). The sign on the
                                // wall (D4). Admins get an inline Allow-anyway; members
                                // get a pointer to ask an admin (they cannot self-allow).
                                return (
                                  <>
                                    <Badge
                                      tone="danger"
                                      dot
                                      title={(f?.violations ?? []).map((x) => x.message).join("\n")}
                                    >
                                      runs blocked
                                    </Badge>
                                    {isAdmin ? (
                                      <Button
                                        variant="secondary"
                                        size="sm"
                                        onClick={() => openAllow(r)}
                                      >
                                        Allow anyway
                                      </Button>
                                    ) : (
                                      <span
                                        className="text-xs text-faint"
                                        title="Only an instance admin can allow a repo through this guardrail."
                                      >
                                        ask an admin to allow this repo
                                      </span>
                                    )}
                                  </>
                                );
                              }
                              if (r.guardrail_override) {
                                // An admin explicitly allowed this repo (D8): a distinct,
                                // neutral-warning tone, with the reason/actor/when in the
                                // title. Admins can Revoke, re-arming the guardrail.
                                const ov = r.guardrail_override;
                                return (
                                  <>
                                    <Badge
                                      tone="warning"
                                      dot
                                      title={`Allowed by ${ov.by} on ${new Date(ov.at).toLocaleString()}\nReason: ${ov.reason}`}
                                    >
                                      allowed by admin
                                    </Badge>
                                    {isAdmin && (
                                      <Button
                                        variant="ghost"
                                        size="sm"
                                        disabled={overrideBusyId === r.id}
                                        onClick={() => revokeOverride(r)}
                                      >
                                        {overrideBusyId === r.id ? "Revoking…" : "Revoke"}
                                      </Button>
                                    )}
                                  </>
                                );
                              }
                              // Warn-only repos keep the existing advisory badge.
                              if (!f || f.warnings.length === 0) return null;
                              const n = f.warnings.length;
                              return (
                                <Badge
                                  tone="warning"
                                  dot
                                  title={f.warnings.map((x) => x.message).join("\n")}
                                >
                                  {n} privilege warning{n === 1 ? "" : "s"}
                                </Badge>
                              );
                            })()}
                            {/* PRD #361 M4: the neutral per-repo Setup chip, appended
                                after the guardrail badges. Enabled-only, matching the
                                Trusted/Tools cells' `!r.enabled` guards — a disabled
                                repo has no capabilities to report. */}
                            {r.enabled && <RepoSetupChip repo={r} />}
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          {!r.enabled ? (
                            <span className="text-xs text-faint">—</span>
                          ) : (
                            <div className="flex flex-wrap items-center gap-2">
                              <Badge tone={isTrusted(r) ? "ok" : "neutral"} dot>
                                {isTrusted(r) ? "Trusted" : "Off"}
                              </Badge>
                              <Button
                                variant="secondary"
                                size="sm"
                                aria-expanded={trustRepoId === r.id}
                                aria-label={`Trusted repo settings for ${r.path_with_namespace}`}
                                onClick={(e) => {
                                  trustTriggerRef.current = e.currentTarget;
                                  setConfirmTrustId(null);
                                  setTrustRepoId((id) => (id === r.id ? null : r.id));
                                }}
                              >
                                {trustRepoId === r.id ? "Close" : "Manage"}
                              </Button>
                            </div>
                          )}
                        </td>
                        <td className="px-4 py-3">
                          {!r.enabled ? (
                            <span className="text-xs text-faint">—</span>
                          ) : (
                            <Button
                              variant="secondary"
                              size="sm"
                              aria-expanded={toolsRepoId === r.id}
                              disabled={toolsBusy && toolsRepoId !== r.id}
                              onClick={() => openTools(r)}
                            >
                              {toolsRepoId === r.id ? "Close" : "Tools"}
                            </Button>
                          )}
                        </td>
                        <td className="px-4 py-3 text-right">
                          {/* Remove is reachable only from the disabled state (PRD #357
                              D2); confirming it is permanent. The confirm is row-inline
                              (its own state), replacing the action buttons in place. */}
                          {confirmRemoveId === r.id ? (
                            <div
                              ref={confirmRemoveRef}
                              role="group"
                              aria-label={`Confirm removing ${r.path_with_namespace}`}
                              className="ml-auto max-w-xs space-y-2 rounded-md border border-danger/40 bg-danger/5 p-3 text-left"
                            >
                              <p className="text-xs text-fg">
                                <span className="font-medium">Remove {r.path_with_namespace}?</span> This is
                                permanent and deletes its board and run history. If the bot still has access to
                                the project, a refresh re-adds it as a disabled repo.
                              </p>
                              <div className="flex justify-end gap-2">
                                <Button
                                  variant="danger"
                                  size="sm"
                                  disabled={busyId === r.id}
                                  onClick={() => removeRepo(r)}
                                >
                                  {busyId === r.id ? "Removing…" : "Confirm remove"}
                                </Button>
                                <Button
                                  variant="ghost"
                                  size="sm"
                                  disabled={busyId === r.id}
                                  onClick={() => setConfirmRemoveId(null)}
                                >
                                  Cancel
                                </Button>
                              </div>
                            </div>
                          ) : (
                            <div className="flex justify-end gap-2">
                              {r.enabled && (
                                <Link to={`/repos/${r.id}/board`}>
                                  <Button variant="secondary" size="sm">
                                    Open board
                                  </Button>
                                </Link>
                              )}
                              <Button
                                variant={r.enabled ? "danger" : "primary"}
                                size="sm"
                                disabled={busyId === r.id}
                                onClick={() => toggle(r)}
                              >
                                {r.enabled ? "Disable" : "Enable"}
                              </Button>
                              {/* Remove only on a DISABLED repo (PRD #357 D2). An enabled
                                  repo shows no Remove button; disable it first. */}
                              {!r.enabled && (
                                <Button
                                  variant="danger"
                                  size="sm"
                                  disabled={busyId === r.id}
                                  onClick={() => setConfirmRemoveId(r.id)}
                                >
                                  Remove
                                </Button>
                              )}
                            </div>
                          )}
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>

            {/* The Trusted-repo panel (PRD #246). Rendered OUTSIDE the
                overflow-x-auto div above so its security copy stays fully readable
                at any width (never clipped behind a horizontal scroll). Only one
                repo's panel shows at a time. */}
            {trustRepo && (
              <div
                ref={trustPanelRef}
                role="group"
                aria-label={`Trusted repo for ${trustRepo.path_with_namespace}`}
                className="space-y-4 border-t border-edge bg-raised/20 p-4"
              >
                {/* Header: what "trusted" means, plus the master switch. */}
                <div className="flex items-start gap-4">
                  <div className="min-w-0 flex-1 space-y-1">
                    <h3 className="flex items-center gap-2 text-sm font-semibold text-fg">
                      Trusted repo
                      <Badge tone={isTrusted(trustRepo) ? "ok" : "neutral"} dot>
                        {isTrusted(trustRepo) ? "Trusted" : "Off"}
                      </Badge>
                    </h3>
                    <p className="max-w-2xl text-sm text-muted">
                      You vouch that every committer to {trustRepo.path_with_namespace} is trusted. This
                      unlocks repo-authored context below, applied to new runs immediately. It grants{" "}
                      <span className="font-medium text-fg">context, not permissions</span>: the guardrails do
                      not change.
                    </p>
                  </div>
                  <Toggle
                    label="Trusted repo"
                    checked={isTrusted(trustRepo)}
                    disabled={anyTrustBusy}
                    onChange={() => onMasterToggle(trustRepo)}
                  />
                </div>

                {/* Enable is a trust decision → confirm before the master turns on. */}
                {confirmTrustId === trustRepo.id && !isTrusted(trustRepo) && (
                  <div
                    ref={confirmTrustRef}
                    role="group"
                    aria-label={`Confirm trusting ${trustRepo.path_with_namespace}`}
                    className="space-y-3 rounded-md border border-warn/40 bg-warn/5 p-3"
                  >
                    <p className="text-sm text-fg">
                      <span className="font-medium">Mark {trustRepo.path_with_namespace} as trusted?</span>{" "}
                      This turns on both capabilities: it loads skills from the repo&rsquo;s own{" "}
                      <code className="rounded bg-raised px-1 py-0.5 font-mono text-xs">.claude/skills/</code>, and
                      lets the lead read the repo&rsquo;s root{" "}
                      <code className="rounded bg-raised px-1 py-0.5 font-mono text-xs">CLAUDE.md</code> as{" "}
                      <span className="font-medium">advisory</span> context. Repo content can steer an agent, so
                      enable this only for a repo whose merge-request review discipline you trust. The{" "}
                      <span className="font-medium">guardrails are unchanged</span> — trust grants context, never
                      permissions. You can turn either capability off on its own afterward.
                    </p>
                    <div className="flex gap-2">
                      <Button
                        size="sm"
                        disabled={anyTrustBusy}
                        onClick={() => setRepoTrust(trustRepo, true)}
                      >
                        {trustBusyId === trustRepo.id ? "Enabling…" : "Mark as trusted"}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={anyTrustBusy}
                        onClick={() => setConfirmTrustId(null)}
                      >
                        Cancel
                      </Button>
                    </div>
                  </div>
                )}

                {/* Two independently-revocable capabilities, shown once trusted. */}
                {isTrusted(trustRepo) && (
                  <div className="grid gap-3">
                    {/* Repo skills */}
                    <div className="flex items-start gap-3 rounded-md border border-edge bg-raised p-3">
                      <div className="min-w-0 flex-1 space-y-2">
                        <h4 className="text-sm font-semibold text-fg">Repo skills</h4>
                        <p className="text-sm text-muted">
                          Load skills from the repo&rsquo;s own{" "}
                          <code className="rounded bg-surface px-1 py-0.5 font-mono text-xs">.claude/skills/</code>.
                          The agent invokes them on demand; they rank below every built-in skill.
                        </p>
                        <div className="flex flex-wrap gap-1.5 font-mono text-[11px] text-faint">
                          <span className="rounded border border-edge px-1.5 py-0.5">.claude/skills/</span>
                          <span className="rounded border border-ok/40 px-1.5 py-0.5 text-ok">name + description kept</span>
                          <span className="rounded border border-danger/40 px-1.5 py-0.5 text-danger">tool grants stripped</span>
                          <span className="rounded border border-edge px-1.5 py-0.5">lowest precedence</span>
                        </div>
                      </div>
                      <Toggle
                        label="Repo skills"
                        checked={trustRepo.repo_skills_enabled}
                        disabled={anyTrustBusy}
                        onChange={(next) => setRepoSkills(trustRepo, next)}
                      />
                    </div>

                    {/* Repo instructions */}
                    <div className="flex items-start gap-3 rounded-md border border-edge bg-raised p-3">
                      <div className="min-w-0 flex-1 space-y-2">
                        <h4 className="text-sm font-semibold text-fg">Repo instructions</h4>
                        <p className="text-sm text-muted">
                          Let the lead read the repo&rsquo;s root{" "}
                          <code className="rounded bg-surface px-1 py-0.5 font-mono text-xs">CLAUDE.md</code> as{" "}
                          <span className="font-medium text-fg">advisory context</span> about project conventions.
                          The lead verifies tools and paths against the worker before relying on any of it.
                        </p>
                        <div className="flex flex-wrap gap-1.5 font-mono text-[11px] text-faint">
                          <span className="rounded border border-edge px-1.5 py-0.5">root CLAUDE.md</span>
                          <span className="rounded border border-ok/40 px-1.5 py-0.5 text-ok">lead only</span>
                          <span className="rounded border border-danger/40 px-1.5 py-0.5 text-danger">@-imports stripped</span>
                          <span className="rounded border border-danger/40 px-1.5 py-0.5 text-danger">cannot override guardrails</span>
                          <span className="rounded border border-edge px-1.5 py-0.5">64 KB cap</span>
                        </div>
                      </div>
                      <Toggle
                        label="Repo instructions"
                        checked={trustRepo.repo_claudemd_enabled}
                        disabled={anyTrustBusy}
                        onChange={(next) => setRepoClaudemd(trustRepo, next)}
                      />
                    </div>
                  </div>
                )}

                {/* Guardrails-unchanged strip — true whether or not the repo is trusted. */}
                <div className="rounded-md border border-ok/30 bg-ok/5 p-3">
                  <h4 className="mb-2 text-sm font-semibold text-fg">Guardrails are unchanged</h4>
                  <ul className="grid gap-1.5 text-sm text-muted sm:grid-cols-2">
                    <li>
                      <code className="font-mono text-xs">main</code> is never touched
                    </li>
                    <li>The worker holds the token, not the agent</li>
                    <li>Protected branch + Developer role on the forge</li>
                    <li>
                      <code className="font-mono text-xs">settingSources</code> stays empty — no hooks or settings load
                    </li>
                  </ul>
                  <p className="mt-2 text-xs text-faint">
                    Trust adds what the agent reads. It never adds what the agent can do. Every merge still goes
                    through human review.
                  </p>
                </div>

                <div className="flex">
                  <Button variant="ghost" size="sm" onClick={closeTrust}>
                    Close
                  </Button>
                </div>
              </div>
            )}

            {/* Tool-profile picker for one repo, below the table so the checkbox
                list is never clipped by the horizontal scroll. The selectable set
                is the admin allowlist; saving replaces the repo's package list. */}
            {toolsRepo && (
              <div
                role="group"
                aria-label={`Tool profile for ${toolsRepo.path_with_namespace}`}
                className="space-y-3 border-t border-edge bg-raised/20 p-4"
              >
                <p className="text-sm text-fg">
                  <span className="font-medium">Tools for {toolsRepo.path_with_namespace}</span> — the worker installs
                  these with devbox before every run on this repo. The list is what an admin has allowed.
                </p>
                {allowlist && allowlist.length === 0 ? (
                  <p className="text-sm text-faint">
                    No packages are on the allowlist yet. An admin adds them under Admin → Tool allowlist.
                  </p>
                ) : (
                  <div className="flex flex-wrap gap-x-6 gap-y-2">
                    {(allowlist ?? []).map((e) => {
                      const pkg = entryPkg(e);
                      return (
                        <label key={e.id} className="flex items-center gap-2 text-sm text-fg">
                          <input
                            type="checkbox"
                            checked={toolSelection.has(pkg)}
                            onChange={() => toggleTool(pkg)}
                          />
                          <span className="font-mono text-xs">{pkg}</span>
                        </label>
                      );
                    })}
                  </div>
                )}
                <div className="flex gap-2">
                  <Button size="sm" disabled={toolsBusy} onClick={() => saveTools(toolsRepo.id)}>
                    {toolsBusy ? "Saving…" : "Save tools"}
                  </Button>
                  <Button variant="ghost" size="sm" disabled={toolsBusy} onClick={() => setToolsRepoId(null)}>
                    Cancel
                  </Button>
                </div>

                {/* Tier-2 opt-in (PRD #18 M5): trust the repo's own devbox.json. */}
                <div className="space-y-1.5 border-t border-edge pt-3">
                  <label className="flex items-start gap-2 text-sm text-fg">
                    <input
                      type="checkbox"
                      className="mt-0.5"
                      checked={toolsRepo.repo_devbox_opt_in}
                      disabled={toolsBusy}
                      onChange={(e) => setRepoDevbox(toolsRepo, e.target.checked)}
                    />
                    <span>
                      Also trust this repo&rsquo;s own{" "}
                      <code className="rounded bg-raised px-1 py-0.5 font-mono text-xs">devbox.json</code> packages
                    </span>
                  </label>
                  <p className="pl-6 text-xs text-muted">
                    Only the <code className="font-mono">packages</code> list is read from the repo&rsquo;s{" "}
                    <code className="font-mono">devbox.json</code>; its <code className="font-mono">shell.init_hook</code>,{" "}
                    <code className="font-mono">scripts</code>, flake references, and every other key are ignored and
                    never run. Even so, a package it lists gets installed on your worker (bypassing the admin allowlist),
                    so enable this only for a repo whose review discipline you trust. Your tools above always win a version
                    conflict.
                  </p>
                </div>
              </div>
            )}
          </Card>
        </>
      )}

      {/* Allow-anyway modal (PRD #66 M9, D8): admin-only. It names the EXACT block
          findings being accepted (display-only, read from the connection report) and
          requires a non-empty reason before it will POST the override. */}
      {allowRepo && (
        <Modal
          label={`Allow runs on ${allowRepo.path_with_namespace}`}
          onClose={closeAllow}
          closeOnBackdrop={!allowBusy}
        >
          <div className="my-8 w-full max-w-lg overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
            <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
              <div>
                <h2 className="text-base font-semibold">Allow runs on this repo?</h2>
                <p className="mt-0.5 text-xs text-muted">{allowRepo.path_with_namespace}</p>
              </div>
              <button
                type="button"
                onClick={closeAllow}
                disabled={allowBusy}
                aria-label="Close"
                className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
              >
                <XIcon />
              </button>
            </div>
            <div className="space-y-4 px-5 py-5">
              {allowError && <Alert message={allowError} />}
              <p className="text-sm text-muted">
                uzi refuses runs on this repo because the bot can reach the default branch. Allowing it is a
                per-repo, recorded exception — it accepts the risk; it does <span className="font-medium text-fg">not</span>{" "}
                change branch protection, and it never waives a protection that could not be read.
              </p>
              {allowFindings.length > 0 ? (
                <div className="rounded-md border border-danger/40 bg-danger/5 p-3">
                  <h3 className="mb-1.5 text-sm font-semibold text-fg">You are accepting these findings</h3>
                  <ul className="list-disc space-y-1 pl-5 text-sm text-muted">
                    {allowFindings.map((v) => (
                      <li key={v.code}>{v.message}</li>
                    ))}
                  </ul>
                </div>
              ) : (
                <p className="text-xs text-faint">
                  The specific findings are not in the last sync; the run gates enforce them live regardless.
                </p>
              )}
              <label className="block space-y-1.5">
                <span className="text-sm font-medium text-fg">
                  Reason <span className="text-danger">*</span>
                </span>
                <Textarea
                  rows={3}
                  value={allowReason}
                  disabled={allowBusy}
                  placeholder="Why this repo is allowed through the guardrail (recorded with your name)"
                  onChange={(e) => setAllowReason(e.target.value)}
                />
              </label>
            </div>
            <div className="flex items-center justify-end gap-2 border-t border-edge bg-ink/40 px-5 py-3.5">
              <Button variant="ghost" size="sm" disabled={allowBusy} onClick={closeAllow}>
                Cancel
              </Button>
              <Button
                size="sm"
                disabled={allowBusy || allowReason.trim() === ""}
                onClick={() => submitAllow(allowRepo.id)}
              >
                {allowBusy ? "Allowing…" : "Allow anyway"}
              </Button>
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}
