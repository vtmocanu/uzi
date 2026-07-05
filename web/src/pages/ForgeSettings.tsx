// Settings → Forge: connect/verify/delete the GitLab bot PAT, plus PRD #5
// least-privilege surfacing (per-connection badge + expandable findings + an
// on-demand "Check privileges" action). Inside SettingsShell; per-row busy
// state instead of one page-wide busy flag.

import { Fragment, useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type ForgeConnection, type PrivilegeReport } from "../lib/api";
import { privilegeBadge } from "../lib/privilege";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Select, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";
import { BranchIcon } from "../components/icons";

const BOT_SETUP_DOC = "/docs/gitlab-bot-setup";

export function ForgeSettings() {
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [allowedUrls, setAllowedUrls] = useState<string[]>([]);
  const [baseUrl, setBaseUrl] = useState("");
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // A 422 over-privilege rejection carries a violation list we render with a doc
  // link, distinct from the plain string errors above.
  const [connectViolations, setConnectViolations] = useState<string[] | null>(null);
  const [connecting, setConnecting] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [checkingId, setCheckingId] = useState<string | null>(null);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    try {
      const [cfg, conns] = await Promise.all([api.forgeConfig(), api.listConnections()]);
      setAllowedUrls(cfg.allowed_base_urls);
      setBaseUrl((prev) => prev || cfg.allowed_base_urls[0] || "");
      setConnections(conns.connections);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load forge settings");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const resetMessages = () => {
    setError("");
    setNotice("");
    setConnectViolations(null);
  };

  const connect = async (e: React.FormEvent) => {
    e.preventDefault();
    resetMessages();
    setConnecting(true);
    try {
      const { connection } = await api.createConnection(baseUrl, token);
      setToken("");
      setNotice(`Connected as ${connection.bot_username}.`);
      await load();
    } catch (err) {
      if (err instanceof ApiError && err.status === 422) {
        const body = err.body as { violations?: string[] } | null;
        setError(err.message);
        setConnectViolations(body?.violations ?? []);
      } else {
        setError(err instanceof ApiError ? err.message : "Connect failed");
      }
    } finally {
      setConnecting(false);
    }
  };

  const verify = async (id: string) => {
    resetMessages();
    setBusyId(id);
    try {
      const { connection } = await api.verifyConnection(id);
      setNotice(`Verified ${connection.bot_username}.`);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Verify failed");
    } finally {
      setBusyId(null);
    }
  };

  const check = async (id: string) => {
    resetMessages();
    setCheckingId(id);
    try {
      const { report } = await api.privilegeCheck(id);
      // Patch the checked connection in place so the badge updates without a
      // full reload (and keep the panel open on it).
      setConnections((prev) =>
        prev.map((c) =>
          c.id === id
            ? { ...c, privilege_status: report.status, privilege_checked_at: report.checked_at, privilege_report: report }
            : c,
        ),
      );
      setExpandedId(id);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Privilege check failed");
    } finally {
      setCheckingId(null);
    }
  };

  const remove = async (id: string) => {
    resetMessages();
    setBusyId(id);
    try {
      await api.deleteConnection(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Delete failed");
    } finally {
      setBusyId(null);
    }
  };

  return (
    <SettingsShell description="Connect the GitLab bot account uzi acts through.">
      {error && <Alert message={error} />}
      {connectViolations && (
        <Card className="border-danger/40 bg-danger/5">
          <p className="text-sm font-medium text-danger">The token was not saved — it is over-privileged:</p>
          <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-fg">
            {connectViolations.map((v, i) => (
              <li key={i}>{v}</li>
            ))}
          </ul>
          <p className="mt-3 text-sm text-muted">
            Mint a least-privilege token (scope <code className="rounded bg-raised px-1 py-0.5 text-fg">api</code> only,
            non-admin bot) — see the{" "}
            <Link to={BOT_SETUP_DOC} className="text-brand hover:underline">
              bot setup guide
            </Link>
            .
          </p>
        </Card>
      )}
      {notice && <Alert tone="success" message={notice} />}

      <Card>
        <SectionTitle>Connect a bot PAT</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Create a bot account, give it a personal access token with the{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">api</code> scope, and add it as
          Developer to the projects uzi should see. The token is stored encrypted and never shown
          again.
        </p>
        <form className="mt-4 space-y-4" onSubmit={connect}>
          <Field label="Forge base URL">
            <Select value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)}>
              {allowedUrls.map((u) => (
                <option key={u} value={u}>
                  {u}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Bot personal access token (scope: api)">
            <Input
              type="password"
              autoComplete="off"
              placeholder="glpat-…"
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
          </Field>
          <Button type="submit" disabled={connecting || !baseUrl || !token}>
            {connecting ? "Verifying…" : "Connect"}
          </Button>
        </form>
      </Card>

      {loading ? (
        <Card>
          <Skeleton className="h-5 w-full" />
          <Skeleton className="mt-3 h-5 w-2/3" />
        </Card>
      ) : connections.length === 0 ? (
        <EmptyState
          icon={<BranchIcon />}
          title="No forge connection yet"
          description="Connect a bot PAT above — repos the bot can see become boards."
        />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Bot</th>
                  <th className="px-4 py-3 font-medium">Base URL</th>
                  <th className="px-4 py-3 font-medium">Privileges</th>
                  <th className="px-4 py-3 font-medium">Last verified</th>
                  <th className="px-4 py-3 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {connections.map((c) => {
                  const badge = privilegeBadge(c.privilege_status, c.privilege_report);
                  const expandable = c.privilege_report !== null;
                  const expanded = expandedId === c.id;
                  return (
                    <Fragment key={c.id}>
                      <tr>
                        <td className="px-4 py-3">
                          <span className="font-medium text-fg">{c.bot_username}</span>{" "}
                          <Badge>{c.forge_type}</Badge>
                        </td>
                        <td className="px-4 py-3 text-muted">{c.base_url}</td>
                        <td className="px-4 py-3">
                          <div className="flex flex-col items-start gap-0.5">
                            {expandable ? (
                              <button
                                type="button"
                                onClick={() => setExpandedId(expanded ? null : c.id)}
                                className="cursor-pointer"
                                aria-expanded={expanded}
                                title="Show findings"
                              >
                                <Badge tone={badge.tone} dot>
                                  {badge.label}
                                </Badge>
                              </button>
                            ) : (
                              <Badge tone={badge.tone} dot>
                                {badge.label}
                              </Badge>
                            )}
                            {c.privilege_checked_at && (
                              <span className="text-[11px] text-faint">
                                {new Date(c.privilege_checked_at).toLocaleString()}
                              </span>
                            )}
                          </div>
                        </td>
                        <td className="px-4 py-3 text-muted">
                          {c.last_verified_at ? new Date(c.last_verified_at).toLocaleString() : "—"}
                        </td>
                        <td className="px-4 py-3 text-right">
                          <div className="flex justify-end gap-2">
                            <Button
                              variant="secondary"
                              size="sm"
                              disabled={checkingId === c.id}
                              onClick={() => check(c.id)}
                            >
                              {checkingId === c.id ? "Checking…" : "Check privileges"}
                            </Button>
                            <Button
                              variant="secondary"
                              size="sm"
                              disabled={busyId === c.id}
                              onClick={() => verify(c.id)}
                            >
                              Verify
                            </Button>
                            <Button
                              variant="danger"
                              size="sm"
                              disabled={busyId === c.id}
                              onClick={() => remove(c.id)}
                            >
                              Delete
                            </Button>
                          </div>
                        </td>
                      </tr>
                      {expanded && c.privilege_report && (
                        <tr className="bg-raised/30">
                          <td colSpan={5} className="px-4 py-3">
                            <PrivilegeFindings report={c.privilege_report} />
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      <p className="text-xs text-faint">
        To rotate a token, connect again with the same base URL — the new PAT replaces the old one.
      </p>
    </SettingsShell>
  );
}

// PrivilegeFindings renders the token + per-repo findings of one report. A clean
// report says so explicitly rather than showing an empty panel.
function PrivilegeFindings({ report }: { report: PrivilegeReport }) {
  const clean =
    report.token.violations.length === 0 &&
    report.token.warnings.length === 0 &&
    report.repos.every((r) => r.violations.length === 0 && r.warnings.length === 0);

  return (
    <div className="space-y-3 text-sm">
      <div>
        <p className="font-medium text-fg">Token</p>
        <p className="text-xs text-muted">
          scopes [{report.token.scopes.join(", ") || "—"}] · {report.token.active ? "active" : "inactive"}
        </p>
        <FindingList violations={report.token.violations} warnings={report.token.warnings} />
      </div>
      {report.repos.map((r) => (
        <div key={r.repo_id}>
          <p className="font-medium text-fg">{r.path}</p>
          {r.violations.length === 0 && r.warnings.length === 0 ? (
            <p className="text-xs text-muted">Developer on a protected default branch.</p>
          ) : (
            <FindingList violations={r.violations} warnings={r.warnings} />
          )}
        </div>
      ))}
      {clean && <p className="text-xs text-ok">Everything is least-privilege.</p>}
    </div>
  );
}

function FindingList({ violations, warnings }: { violations: string[]; warnings: string[] }) {
  if (violations.length === 0 && warnings.length === 0) return null;
  return (
    <ul className="mt-1 space-y-1">
      {violations.map((v, i) => (
        <li key={`v${i}`} className="flex items-start gap-2 text-danger">
          <span aria-hidden="true">✕</span>
          <span>{v}</span>
        </li>
      ))}
      {warnings.map((w, i) => (
        <li key={`w${i}`} className="flex items-start gap-2 text-warn">
          <span aria-hidden="true">!</span>
          <span>{w}</span>
        </li>
      ))}
    </ul>
  );
}
