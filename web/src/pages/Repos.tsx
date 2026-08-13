// Boards home: the projects the bot can see. Enabling one starts tracking its
// PRD issues on a kanban board.

import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, isHttpsUrl, type ForgeConnection, type Repo, type ToolAllowlistEntry } from "../lib/api";
import { repoFindings } from "../lib/privilege";
import { Alert, Badge, Button, Card, EmptyState, ListSkeleton, PageHeader, Select, Toggle } from "../components/ui";
import { PipelineBadge } from "../components/PipelineBadge";
import { BoardIcon } from "../components/icons";

export function Repos() {
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [connectionId, setConnectionId] = useState("");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);
  // The repo whose "Trusted repo" panel is currently expanded. The panel groups a
  // master control over two independently-revocable capabilities — Repo skills and
  // Repo instructions (PRD #246). It renders OUTSIDE the horizontally-scrolling
  // table so its security copy is never clipped at narrow widths.
  const [trustRepoId, setTrustRepoId] = useState<string | null>(null);
  // Enabling the master is a trust decision, so it is gated behind a confirm step;
  // this holds the repo whose enable-confirm is showing. Disabling (master or a
  // sub-toggle) is immediate, matching the existing opt-in patterns.
  const [confirmTrustId, setConfirmTrustId] = useState<string | null>(null);
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
  // Focus management: remember the cell trigger so focus returns to it when the
  // panel closes, and move focus into the panel (its master switch) when it opens.
  const trustTriggerRef = useRef<HTMLButtonElement | null>(null);
  const trustPanelRef = useRef<HTMLDivElement | null>(null);
  // The enable-confirm block; when it opens, focus moves to its primary button
  // ("Mark as trusted", the first button inside) so a screen-reader user hears it —
  // the master switch's aria-checked stays false until the PATCH lands.
  const confirmTrustRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (trustRepoId) trustPanelRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [trustRepoId]);
  useEffect(() => {
    if (confirmTrustId) confirmTrustRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [confirmTrustId]);

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
    setBusyId(repo.id);
    try {
      const { repo: updated } = await api.setRepoEnabled(repo.id, !repo.enabled);
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
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

  return (
    <div className="space-y-6">
      <PageHeader
        title="Boards"
        description="Projects your bot can see. Enable one to track its PRD issues on a board."
        actions={
          connectionId ? (
            <Button variant="secondary" size="sm" disabled={refreshing} onClick={() => loadProjects(connectionId)}>
              {refreshing ? "Refreshing…" : "Refresh list"}
            </Button>
          ) : undefined
        }
      />

      {error && <Alert message={error} />}

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
                              if (!f) return null;
                              // D4 (PRD #66 M4): a block-severity finding means uzi
                              // REFUSES runs on this repo (the bot can push/merge to
                              // main). That is a distinct, actionable state from the
                              // advisory "N privilege issue" wording, which does not
                              // tell the user their runs are refused — so it gets its
                              // own "runs blocked" danger badge, the sign on the wall.
                              // Warn-only repos keep the existing advisory badge.
                              if (f.violations.length > 0) {
                                return (
                                  <Badge
                                    tone="danger"
                                    dot
                                    title={f.violations.map((x) => x.message).join("\n")}
                                  >
                                    runs blocked
                                  </Badge>
                                );
                              }
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
                          </div>
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
    </div>
  );
}
