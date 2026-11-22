// Agents page (PRD #18 M6/M7): every template visible to the caller (builtin ∪
// global ∪ own) with its scope badge and an "In my runs" allocation toggle. The
// resolved toggle is the caller's overlay when set, else the global default;
// admins additionally toggle which builtin/global templates are global defaults.
// Anyone can author a private ("Mine") agent; admins can also publish globals.

import { Fragment, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, type AgentTemplate, type TemplateAllocation } from "../lib/api";
import type { EffectiveSettings } from "../lib/apiTypes";
import { codexRuntimeDefaultLabel, modelSettingSourceLabel } from "../lib/modelSettings";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { agentCountModel, isShadowedTemplate, sharedTemplateNames } from "../lib/agentCounts";
import {
  isLeadTemplateName,
  provenanceBadgeKind,
  summarizeTools,
  SYNCED_BADGE_HINT,
  SYNCED_BADGE_LABEL,
  SYNCED_BADGE_TONE,
  TEMPLATE_SCOPE_LABEL,
  templatePinRows,
  templateScopeBadgeTone,
} from "../lib/agentTemplates";
import { Alert, Badge, Button, Card, ListSkeleton, PageHeader } from "../components/ui";
import { PlusIcon } from "../components/icons";
import { DocLink } from "../components/DocLink";
import { DOC_AGENT_TEMPLATES } from "../lib/doclinks";

// SHADOWED_HINT is stated once and used twice — as the badge's `title` and as the
// text of the sr-only span it points at. Two copies of the same sentence is how a
// tooltip and its accessible description quietly stop agreeing.
const SHADOWED_HINT =
  "A builtin or global template shares this name and takes precedence, so this agent is dropped from your runs. Rename it to use it.";

export function Agents() {
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;
  // `allocations` is also written by the toggle handlers (setMyOverride /
  // setGlobalDefault), so it stays local and the fetcher sets it as a side effect
  // rather than routing through the hook's read-only `data`.
  const [allocations, setAllocations] = useState<TemplateAllocation[]>([]);
  // `error` is the load error (from the hook, as `loadError`) OR a toggle-handler
  // error kept here — the two share one Alert slot below.
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // A single global busy flag disables ALL toggles during any in-flight
  // allocation PUT: each PUT replace-sets the whole half, so two rapid toggles
  // must not fire from stale state and drop one (reviewer M7 ride-along).
  const [busy, setBusy] = useState(false);

  const userId = user?.id;
  // The viewer the loaded allocations belong to. A viewer change re-runs the load
  // (deps below) but the previous viewer's allocations stay on screen until it
  // lands, so the personal line is gated on this matching the current viewer.
  const [allocFor, setAllocFor] = useState<string | null>(null);

  const { data, loading, error: loadError } = useAsyncData(
    async ({ isCurrent }) => {
      const [{ templates }, { templates: alloc }] = await Promise.all([
        api.listAgentTemplates(),
        api.getTemplateAllocations(),
      ]);
      if (isCurrent()) {
        setAllocations(alloc);
        setAllocFor(userId ?? null);
      }
      return { templates };
    },
    [userId],
    { fallback: "Failed to load templates" },
  );
  const templates = useMemo(() => data?.templates ?? [], [data]);

  const allocById = useMemo(() => new Map(allocations.map((a) => [a.id, a])), [allocations]);

  // A user template whose name collides with a builtin/global is SHADOWED: the
  // claim drops it (shared precedence, PRD #18 M7), so it never reaches a run even
  // when toggled on. Surface it so the toggle isn't silently a no-op — the fix is
  // to rename. Shared names are always visible, so this set is exact.
  const sharedNames = useMemo(() => sharedTemplateNames(templates), [templates]);

  const counts = useMemo(
    () => agentCountModel(allocations, templates, { isAdmin, viewerId: user?.id ?? "" }),
    [allocations, templates, isAdmin, user?.id],
  );

  // The personal line depends on which lead template is effective, so it is
  // fetched separately from the list (its failure never breaks the page) and is
  // keyed by allocVersion + viewer: a response is shown only for the allocations
  // and user it was requested for. allocVersion bumps on every successful PUT.
  const [allocVersion, setAllocVersion] = useState(0);
  const viewerRef = useRef(userId);
  useEffect(() => {
    viewerRef.current = userId;
  }, [userId]);
  const [personal, setPersonal] = useState<{
    forVersion: number;
    forUserId: string;
    data: EffectiveSettings;
  } | null>(null);
  useEffect(() => {
    if (!userId) return;
    const v = allocVersion;
    const uid = userId;
    let cancelled = false;
    void (async () => {
      try {
        const res = await api.getEffectiveSettings();
        if (cancelled) return;
        if (!res || !Array.isArray(res.models) || !Array.isArray(res.efforts) || !Array.isArray(res.harnesses)) return;
        setPersonal({ forVersion: v, forUserId: uid, data: res });
      } catch {
        // Omit the line; the list is unaffected.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [allocVersion, userId]);

  const leadIds = useMemo(
    () =>
      templates
        .filter((t) => isLeadTemplateName(t.name) && !isShadowedTemplate(t, sharedNames) && allocById.get(t.id)?.effective)
        .map((t) => t.id),
    [templates, allocById, sharedNames],
  );
  const personalData =
    !loading &&
    !loadError &&
    personal &&
    personal.forVersion === allocVersion &&
    personal.forUserId === user?.id &&
    allocFor === user?.id
      ? personal.data
      : null;
  const rowLine = (id: string): ReactNode => {
    if (!personalData || leadIds.length !== 1 || leadIds[0] !== id) return null;
    const lines = personalHarnessLines(personalData, (f) => `For your ${f} runs: `);
    return lines.length ? <div data-testid="personal-lead-line">{lines}</div> : null;
  };
  const noteLines =
    personalData && leadIds.length !== 1 ? personalHarnessLines(personalData, (f) => `${f}: `) : [];

  // Replace-set writes: the PUT fully replaces the half it carries, so each
  // toggle recomputes the whole set from current state and changes just one entry.
  const setMyOverride = async (id: string, enabled: boolean) => {
    const initiator = userId;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const my_overrides = allocations
        .filter((a) => a.id === id || a.my_override !== null)
        .map((a) => ({ template_id: a.id, enabled: a.id === id ? enabled : (a.my_override as boolean) }));
      const { templates: next } = await api.setTemplateAllocations({ my_overrides });
      // The viewer changed while the PUT was in flight: its response belongs to
      // the previous viewer and must not replace the new viewer's allocations.
      if (viewerRef.current !== initiator) return;
      setAllocations(next);
      setAllocVersion((v) => v + 1);
    } catch (err) {
      setError(errorMessage(err, "Failed to update allocation"));
    } finally {
      setBusy(false);
    }
  };

  const setGlobalDefault = async (id: string, on: boolean) => {
    const initiator = userId;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const global_default_ids = allocations
        .filter((a) => a.scope !== "user")
        .filter((a) => (a.id === id ? on : a.global_default))
        .map((a) => a.id);
      const { templates: next } = await api.setTemplateAllocations({ global_default_ids });
      // The viewer changed while the PUT was in flight: its response belongs to
      // the previous viewer and must not replace the new viewer's allocations.
      if (viewerRef.current !== initiator) return;
      setAllocations(next);
      setAllocVersion((v) => v + 1);
    } catch (err) {
      setError(errorMessage(err, "Failed to update global default"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Agents"
        description={
          <>
            Agent templates define the lead and the subagents your runs use. Toggle which ride your runs
            {isAdmin
              ? "; admins publish globals and set the defaults everyone starts from."
              : "; author your own private agents anytime."}{" "}
            See the <DocLink slug={DOC_AGENT_TEMPLATES}>agent templates</DocLink> guide.
            {!loading && !loadError && (
              <span data-testid="agents-summary" className="mt-2 flex flex-wrap gap-x-2 gap-y-1 text-sm text-muted">
                <b className="font-semibold text-fg">
                  {counts.total} {counts.total === 1 ? "agent" : "agents"}
                </b>
                <SummarySep />
                <span>
                  <b className="font-semibold text-fg">{counts.inRuns}</b>{" "}
                  {counts.inRuns === 1 ? "rides" : "ride"} your runs
                </span>
                {counts.byScope.map((s) => (
                  <Fragment key={s.scope}>
                    <SummarySep />
                    <span>
                      {s.total} {s.label.toLowerCase()}
                    </span>
                  </Fragment>
                ))}
                {counts.globalDefaults !== undefined && (
                  <>
                    <SummarySep />
                    <span>
                      {counts.globalDefaults} global {counts.globalDefaults === 1 ? "default" : "defaults"}
                    </span>
                  </>
                )}
              </span>
            )}
          </>
        }
        actions={
          <Link to="/agents/new">
            <Button size="sm">
              <PlusIcon /> New agent
            </Button>
          </Link>
        }
      />

      {(loadError || error) && <Alert message={loadError || error} />}
      {notice && <Alert message={notice} tone="success" />}

      {loading ? (
        <ListSkeleton rows={6} />
      ) : (
        <>
        {noteLines.length > 0 && (
          <div data-testid="personal-lead-note" className="text-sm text-muted">
            <div className="font-medium text-fg">For your lead runs:</div>
            <div className="space-y-0.5 text-xs text-faint">{noteLines}</div>
          </div>
        )}
        <Card flush>
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Name</th>
                  <th className="px-4 py-3 font-medium">Scope</th>
                  <th className="px-4 py-3 font-medium">Description</th>
                  <th className="min-w-[14rem] px-4 py-3 font-medium">Model & effort</th>
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
                      shadowed={isShadowedTemplate(t, sharedNames)}
                      busy={busy || allocFor !== user?.id}
                      personalLine={rowLine(t.id)}
                      onToggleMine={setMyOverride}
                      onToggleGlobal={setGlobalDefault}
                    />
                  ))
                )}
              </tbody>
            </table>
          </div>
        </Card>
        </>
      )}
    </div>
  );
}

// The summary line's separator: real text, so the line reads "4 agents · 2 ride your runs".
function SummarySep() {
  return <span className="text-faint"> · </span>;
}

// personalHarnessLines renders the caller's own worker_model / worker_effort per
// usable harness. It is personal data from /me/effective-settings and is never
// mixed into the shared stored-pin cells.
function personalHarnessLines(data: EffectiveSettings, prefix: (family: string) => string): ReactNode[] {
  const out: ReactNode[] = [];
  for (const h of ["claude", "codex"] as const) {
    if (!data.harnesses.find((x) => x.harness === h)?.usable) continue;
    const m = data.models.find((c) => c.harness === h && c.setting === "worker_model");
    const e = data.efforts.find((c) => c.harness === h && c.setting === "worker_effort");
    if (!m && !e) continue;
    const family = h === "claude" ? "Claude" : "Codex";
    const parts: string[] = [];
    if (m) {
      const unset = modelSettingSourceLabel("note", "sdk_default");
      parts.push(`${m.resolved_model || unset} · ${modelSettingSourceLabel("model", m.source)}`);
    }
    if (e) {
      const runtime = e.mode === "runtime_default" || !e.resolved_effort;
      const label = !runtime ? e.resolved_effort : h === "codex" ? codexRuntimeDefaultLabel(e) : "Runtime default";
      parts.push(`effort ${label} · ${modelSettingSourceLabel("effort", e.source)}`);
    }
    out.push(<div key={h}>{`${prefix(family)}${parts.join("; ")}`}</div>);
  }
  return out;
}

function AgentRow({
  template: t,
  alloc,
  isAdmin,
  shadowed,
  busy,
  personalLine,
  onToggleMine,
  onToggleGlobal,
}: {
  personalLine: ReactNode;
  template: AgentTemplate;
  alloc: TemplateAllocation | undefined;
  isAdmin: boolean;
  shadowed: boolean;
  busy: boolean;
  onToggleMine: (id: string, enabled: boolean) => void;
  onToggleGlobal: (id: string, on: boolean) => void;
}) {
  const effective = alloc?.effective ?? false;
  const pins = templatePinRows(t);
  const globalDefault = alloc?.global_default ?? false;
  // Per-row ids: these hints sit in a table, so a single shared id would point
  // every row's badge at the first row's description.
  const shadowedHintId = `shadowed-hint-${t.id}`;
  const driftHintId = `drift-hint-${t.id}`;
  // F4: the second sentence is admin-only because the diff is not universally
  // reachable — a non-admin gets ReadOnlyView on the detail page, which has no
  // diff panel, and /{id}/builtin 403s them anyway. The badge itself is honest
  // for everyone; only the invitation to open it was not.
  const driftHint = isAdmin
    ? "This template no longer matches the definition shipped in this release. Open it to see the diff."
    : "This template no longer matches the definition shipped in this release.";
  // Source-aware provenance (PRD #602 M5): a synced row shows the SYNCED chip
  // instead of the drift chip, whatever differs_from_builtin says.
  const provenance = provenanceBadgeKind(t);
  const syncedHintId = `synced-hint-${t.id}`;
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
          {/* F10: every badge here explains itself through a `title` alone, which
              reaches mouse users only. The explanation is DESCRIBED via an sr-only
              span (RunCredential's pattern) and never via aria-label, which would
              REPLACE the visible word and break voice control (WCAG 2.5.3).
              `shadowed` is fixed alongside because it has the identical gap — it
              is page-wide, not something the drift badge introduced. */}
          {shadowed && (
            <>
              <Badge
                tone="warning"
                aria-describedby={shadowedHintId}
                title={SHADOWED_HINT}
              >
                shadowed
              </Badge>
              <span id={shadowedHintId} className="sr-only">
                {SHADOWED_HINT}
              </span>
            </>
          )}
          {/* Provenance chips (PRD #602 M5): at most one shows. A synced row's body
              came from the configured agent-source repo — the SYNCED chip, never the
              drift chip. */}
          {provenance === "synced" && (
            <>
              <Badge tone={SYNCED_BADGE_TONE} aria-describedby={syncedHintId} title={SYNCED_BADGE_HINT}>
                {SYNCED_BADGE_LABEL}
              </Badge>
              <span id={syncedHintId} className="sr-only">
                {SYNCED_BADGE_HINT}
              </span>
            </>
          )}
          {/* The copy is "differs from shipped", never "customized" or "edited"
              (issue #201 M4a). This answers "does this row differ from the body
              shipped in THIS release?", which a row nobody has touched can answer
              yes to simply by predating the release. Calling it an edit would make
              M4b's auto-update read as undoing someone's work. A synced row shows
              the SYNCED chip above INSTEAD (PRD #602 M5). */}
          {provenance === "drift" && (
            <>
              <Badge tone="info" aria-describedby={driftHintId} title={driftHint}>
                differs from shipped
              </Badge>
              <span id={driftHintId} className="sr-only">
                {driftHint}
              </span>
            </>
          )}
        </span>
      </td>
      <td className="px-4 py-3">
        <Badge tone={templateScopeBadgeTone(t.scope)}>{TEMPLATE_SCOPE_LABEL[t.scope]}</Badge>
      </td>
      <td className="max-w-md px-4 py-3 text-muted">{t.description}</td>
      <td className="min-w-[14rem] px-4 py-3 text-muted">
        {pins.collapsed ? (
          <div role="group" aria-label="Claude and Codex model and effort" className="space-y-0.5">
            <div>{pins.rows[0].model.label}</div>
            <div className="text-faint">{`effort: ${pins.rows[0].effort.label}`}</div>
            <div className="text-xs text-faint">both harnesses</div>
          </div>
        ) : (
          <div className="space-y-1.5">
            {pins.rows.map((r) => (
              <div key={r.harness} role="group" aria-label={`${r.family} model and effort`}>
                <div>{`${r.family}: ${r.model.label}`}</div>
                <div className="text-faint">{`effort: ${r.effort.label}`}</div>
              </div>
            ))}
          </div>
        )}
        {personalLine && <div className="mt-1 space-y-0.5 text-xs text-faint">{personalLine}</div>}
      </td>
      <td className="max-w-[12rem] truncate px-4 py-3 font-mono text-xs text-muted">{summarizeTools(t)}</td>
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
