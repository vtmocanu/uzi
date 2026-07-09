// Boards home: the projects the bot can see. Enabling one starts tracking its
// PRD issues on a kanban board.

import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, isHttpsUrl, type ForgeConnection, type Repo, type ToolAllowlistEntry } from "../lib/api";
import { repoFindings } from "../lib/privilege";
import { Alert, Badge, Button, Card, EmptyState, ListSkeleton, PageHeader, Select } from "../components/ui";
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
  // The repo whose "enable repo skills" warning is currently expanded (enabling
  // is a trust decision, so it is gated behind a confirm; disabling is immediate).
  // The warning renders OUTSIDE the horizontally-scrolling table so its security
  // caveats are never clipped at narrow widths.
  const [warnRepoId, setWarnRepoId] = useState<string | null>(null);
  const [skillsBusyId, setSkillsBusyId] = useState<string | null>(null);
  // Tool profile picker (PRD #18 M4): the repo whose package picker is open, the
  // admin allowlist (loaded once), the currently-selected package strings for the
  // open repo, and the in-flight save.
  const [toolsRepoId, setToolsRepoId] = useState<string | null>(null);
  const [allowlist, setAllowlist] = useState<ToolAllowlistEntry[] | null>(null);
  const [toolSelection, setToolSelection] = useState<Set<string>>(new Set());
  const [toolsBusy, setToolsBusy] = useState(false);
  // Focus management: remember the "Load repo skills" trigger so focus returns to
  // it on cancel, and move focus into the warning (its first button) when it opens.
  const warnTriggerRef = useRef<HTMLButtonElement | null>(null);
  const warnBannerRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (warnRepoId) warnBannerRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, [warnRepoId]);

  const cancelWarn = () => {
    setWarnRepoId(null);
    warnTriggerRef.current?.focus();
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

  const setRepoSkills = async (repo: Repo, enabled: boolean) => {
    setError("");
    setSkillsBusyId(repo.id);
    try {
      const { repo: updated } = await api.setRepoSkillsEnabled(repo.id, enabled);
      setRepos((prev) => prev.map((r) => (r.id === updated.id ? updated : r)));
      setWarnRepoId(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
    } finally {
      setSkillsBusyId(null);
    }
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

  // The repo whose trust warning is expanded (rendered as a banner below the
  // table, outside its horizontal scroll container).
  const warnRepo = repos.find((r) => r.id === warnRepoId) ?? null;
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
          description="uzi needs a GitLab bot before it can see any projects."
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
                    <th className="px-4 py-3 font-medium">Repo skills</th>
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
                              const hasViolations = f.violations.length > 0;
                              const n = hasViolations ? f.violations.length : f.warnings.length;
                              return (
                                <Badge
                                  tone={hasViolations ? "danger" : "warning"}
                                  dot
                                  title={[...f.violations, ...f.warnings].join("\n")}
                                >
                                  {n} privilege {hasViolations ? "issue" : "warning"}
                                  {n === 1 ? "" : "s"}
                                </Badge>
                              );
                            })()}
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          {!r.enabled ? (
                            <span className="text-xs text-faint">—</span>
                          ) : r.repo_skills_enabled ? (
                            <div className="flex flex-wrap items-center gap-2">
                              <Badge tone="ok" dot>
                                On
                              </Badge>
                              <Button
                                variant="ghost"
                                size="sm"
                                aria-label="Disable repo skills"
                                disabled={skillsBusyId === r.id}
                                onClick={() => setRepoSkills(r, false)}
                              >
                                Disable
                              </Button>
                            </div>
                          ) : (
                            <Button
                              variant="secondary"
                              size="sm"
                              aria-expanded={warnRepoId === r.id}
                              disabled={skillsBusyId === r.id}
                              onClick={(e) => {
                                warnTriggerRef.current = e.currentTarget;
                                setWarnRepoId((id) => (id === r.id ? null : r.id));
                              }}
                            >
                              Load repo skills
                            </Button>
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

            {/* Rendered OUTSIDE the overflow-x-auto div above so the security
                caveats stay fully readable at any width (never clipped behind a
                horizontal scroll). Only one repo's warning shows at a time. */}
            {warnRepo && (
              <div
                ref={warnBannerRef}
                role="group"
                aria-label={`Load repo skills for ${warnRepo.path_with_namespace}`}
                className="space-y-3 border-t border-edge bg-warn/5 p-4"
              >
                <p className="text-sm text-fg">
                  <span className="font-medium">
                    Load skills from {warnRepo.path_with_namespace}?
                  </span>{" "}
                  Enabling this loads skills from the repo&rsquo;s own{" "}
                  <code className="rounded bg-raised px-1 py-0.5 font-mono text-xs">
                    .claude/skills/
                  </code>{" "}
                  into every run on this repo. Only <code className="font-mono text-xs">SKILL.md</code>{" "}
                  files load, never the repo&rsquo;s hooks, settings, commands, or{" "}
                  <code className="font-mono text-xs">CLAUDE.md</code>. A skill can steer an agent, so
                  enable this only for a repo whose merge-request review discipline you trust.
                </p>
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    disabled={skillsBusyId === warnRepo.id}
                    onClick={() => setRepoSkills(warnRepo, true)}
                  >
                    {skillsBusyId === warnRepo.id ? "Enabling…" : "Enable repo skills"}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={skillsBusyId === warnRepo.id}
                    onClick={cancelWarn}
                  >
                    Cancel
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
