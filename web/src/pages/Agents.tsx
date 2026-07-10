// Agents page (PRD #18 M6/M7): every template visible to the caller (builtin ∪
// global ∪ own) with its scope badge and an "In my runs" allocation toggle. The
// resolved toggle is the caller's overlay when set, else the global default;
// admins additionally toggle which builtin/global templates are global defaults.
// Anyone can author a private ("Mine") agent; admins can also publish globals.

import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type AgentTemplate, type TemplateAllocation } from "../lib/api";
import {
  isLeadTemplateName,
  summarizeTools,
  TEMPLATE_SCOPE_LABEL,
  templateScopeBadgeTone,
} from "../lib/agentTemplates";
import { Alert, Badge, Button, Card, ListSkeleton, PageHeader } from "../components/ui";
import { PlusIcon } from "../components/icons";

export function Agents() {
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;
  const [templates, setTemplates] = useState<AgentTemplate[]>([]);
  const [allocations, setAllocations] = useState<TemplateAllocation[]>([]);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [loading, setLoading] = useState(true);
  const [busyId, setBusyId] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError("");
    try {
      const [{ templates }, { templates: alloc }] = await Promise.all([
        api.listAgentTemplates(),
        api.getTemplateAllocations(),
      ]);
      setTemplates(templates);
      setAllocations(alloc);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load templates");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const allocById = useMemo(() => new Map(allocations.map((a) => [a.id, a])), [allocations]);

  // A user template whose name collides with a builtin/global is SHADOWED: the
  // claim drops it (shared precedence, PRD #18 M7), so it never reaches a run even
  // when toggled on. Surface it so the toggle isn't silently a no-op — the fix is
  // to rename. Shared names are always visible, so this set is exact.
  const sharedNames = useMemo(
    () => new Set(templates.filter((t) => t.scope !== "user").map((t) => t.name)),
    [templates],
  );

  // Replace-set writes: the PUT fully replaces the half it carries, so each
  // toggle recomputes the whole set from current state and changes just one entry.
  const setMyOverride = async (id: string, enabled: boolean) => {
    setBusyId(id);
    setError("");
    setNotice("");
    try {
      const my_overrides = allocations
        .filter((a) => a.id === id || a.my_override !== null)
        .map((a) => ({ template_id: a.id, enabled: a.id === id ? enabled : (a.my_override as boolean) }));
      const { templates: next } = await api.setTemplateAllocations({ my_overrides });
      setAllocations(next);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to update allocation");
    } finally {
      setBusyId(null);
    }
  };

  const setGlobalDefault = async (id: string, on: boolean) => {
    setBusyId(id);
    setError("");
    setNotice("");
    try {
      const global_default_ids = allocations
        .filter((a) => a.scope !== "user")
        .filter((a) => (a.id === id ? on : a.global_default))
        .map((a) => a.id);
      const { templates: next } = await api.setTemplateAllocations({ global_default_ids });
      setAllocations(next);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to update global default");
    } finally {
      setBusyId(null);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Agents"
        description={`Agent templates render to Claude Code subagent files. Toggle which ride your runs${isAdmin ? "; admins publish globals and set the defaults everyone starts from." : "; author your own private agents anytime."}`}
        actions={
          <Link to="/agents/new">
            <Button size="sm">
              <PlusIcon /> New agent
            </Button>
          </Link>
        }
      />

      {error && <Alert message={error} />}
      {notice && <Alert message={notice} tone="success" />}

      {loading ? (
        <ListSkeleton rows={6} />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Name</th>
                  <th className="px-4 py-3 font-medium">Scope</th>
                  <th className="px-4 py-3 font-medium">Description</th>
                  <th className="px-4 py-3 font-medium">Model</th>
                  <th className="px-4 py-3 font-medium">Tools</th>
                  <th className="px-4 py-3 text-center font-medium">In my runs</th>
                  {isAdmin && <th className="px-4 py-3 text-center font-medium">Global default</th>}
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {templates.length === 0 ? (
                  <tr>
                    <td colSpan={isAdmin ? 7 : 6} className="px-4 py-6 text-center text-faint">
                      No templates.
                    </td>
                  </tr>
                ) : (
                  templates.map((t) => (
                    <AgentRow
                      key={t.id}
                      template={t}
                      alloc={allocById.get(t.id)}
                      isAdmin={isAdmin}
                      shadowed={t.scope === "user" && sharedNames.has(t.name)}
                      busy={busyId === t.id}
                      onToggleMine={setMyOverride}
                      onToggleGlobal={setGlobalDefault}
                    />
                  ))
                )}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  );
}

function AgentRow({
  template: t,
  alloc,
  isAdmin,
  shadowed,
  busy,
  onToggleMine,
  onToggleGlobal,
}: {
  template: AgentTemplate;
  alloc: TemplateAllocation | undefined;
  isAdmin: boolean;
  shadowed: boolean;
  busy: boolean;
  onToggleMine: (id: string, enabled: boolean) => void;
  onToggleGlobal: (id: string, on: boolean) => void;
}) {
  const effective = alloc?.effective ?? false;
  const globalDefault = alloc?.global_default ?? false;
  return (
    <tr className="transition-colors hover:bg-raised/30">
      <td className="px-4 py-3">
        <span className="inline-flex items-center gap-2">
          <Link to={`/agents/${t.id}`} className="font-mono text-brand hover:text-brand-hover">
            {t.name}
          </Link>
          {isLeadTemplateName(t.name) && (
            <Badge tone="brand" title="The orchestrator: the main agent thread that plans and delegates.">
              orchestrator
            </Badge>
          )}
          {shadowed && (
            <Badge
              tone="warning"
              title="A builtin or global template shares this name and takes precedence, so this agent is dropped from your runs. Rename it to use it."
            >
              shadowed
            </Badge>
          )}
        </span>
      </td>
      <td className="px-4 py-3">
        <Badge tone={templateScopeBadgeTone(t.scope)}>{TEMPLATE_SCOPE_LABEL[t.scope]}</Badge>
      </td>
      <td className="max-w-md px-4 py-3 text-muted">{t.description}</td>
      <td className="px-4 py-3 text-muted">{t.model ?? "inherit"}</td>
      <td className="max-w-xs truncate px-4 py-3 font-mono text-xs text-muted">{summarizeTools(t)}</td>
      <td className="px-4 py-3 text-center">
        <input
          type="checkbox"
          className="h-4 w-4 accent-brand"
          checked={effective}
          disabled={busy}
          aria-label={`Include ${t.name} in my runs`}
          onChange={(e) => onToggleMine(t.id, e.target.checked)}
        />
      </td>
      {isAdmin && (
        <td className="px-4 py-3 text-center">
          {t.scope === "user" ? (
            <span className="text-xs text-faint" title="User templates are never global defaults.">
              —
            </span>
          ) : (
            <input
              type="checkbox"
              className="h-4 w-4 accent-brand"
              checked={globalDefault}
              disabled={busy}
              aria-label={`Make ${t.name} a global default`}
              onChange={(e) => onToggleGlobal(t.id, e.target.checked)}
            />
          )}
        </td>
      )}
    </tr>
  );
}
