// Skill allocation panel (PRD #16), shown on an agent template's detail page.
// It renders the two allocation halves the caller may see — shared (admin-
// managed) and "mine" (the caller's private overlay) — and the union of the two,
// which is exactly the set of skills the caller's runs on this template receive.
//
// Write authz mirrors the API: the shared half is admin-only, the mine half is
// any user's own overlay. PUT is replace-set with only the halves the caller may
// edit — a non-admin sends my_skill_ids only, so the shared half is left
// untouched (nil = untouched, server-side).

import { useCallback, useEffect, useMemo, useState } from "react";
import { api, type Skill, type TemplateSkills } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { scopeBadgeTone, SCOPE_LABEL } from "../lib/skills";
import { Alert, Badge, Button, Card, Spinner } from "./ui";

export function SkillAllocationPanel({
  templateId,
  isAdmin,
  userId,
}: {
  templateId: string;
  isAdmin: boolean;
  userId: string | undefined;
}) {
  const [allSkills, setAllSkills] = useState<Skill[]>([]);
  const [shared, setShared] = useState<Set<string>>(new Set());
  const [mine, setMine] = useState<Set<string>>(new Set());
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  // Baseline snapshots, used to compute the dirty state (enables Save) and to
  // send only the halves that changed.
  const [baseShared, setBaseShared] = useState<Set<string>>(new Set());
  const [baseMine, setBaseMine] = useState<Set<string>>(new Set());

  const load = useCallback(async () => {
    setError("");
    try {
      const [{ skills }, { allocations }] = await Promise.all([
        api.listSkills(),
        api.getTemplateSkills(templateId),
      ]);
      setAllSkills(skills);
      applyAllocations(allocations);
    } catch (err) {
      setError(errorMessage(err, "Failed to load skill allocations"));
    } finally {
      setLoading(false);
    }
  }, [templateId]);

  const applyAllocations = (a: TemplateSkills) => {
    const s = new Set(a.shared.map((x) => x.skill_id));
    const m = new Set(a.mine.map((x) => x.skill_id));
    setShared(s);
    setMine(m);
    setBaseShared(new Set(s));
    setBaseMine(new Set(m));
  };

  useEffect(() => {
    load();
  }, [load]);

  const byId = useMemo(() => new Map(allSkills.map((s) => [s.id, s])), [allSkills]);

  // A shared allocation may reference only builtin/global skills; a mine overlay
  // may also reference the caller's own user skills.
  const sharedOptions = useMemo(
    () => allSkills.filter((s) => s.scope === "builtin" || s.scope === "global"),
    [allSkills],
  );
  const mineOptions = useMemo(
    () =>
      allSkills.filter(
        (s) => s.scope === "builtin" || s.scope === "global" || (s.scope === "user" && s.user_id === userId),
      ),
    [allSkills, userId],
  );

  // The union the caller's runs actually get: shared ∪ mine, deduped by id.
  const union = useMemo(() => {
    const ids = new Set<string>([...shared, ...mine]);
    return [...ids]
      .map((id) => byId.get(id))
      .filter((s): s is Skill => !!s)
      .sort((a, b) => a.name.localeCompare(b.name));
  }, [shared, mine, byId]);

  const sharedDirty = isAdmin && !setsEqual(shared, baseShared);
  const mineDirty = !setsEqual(mine, baseMine);
  const dirty = sharedDirty || mineDirty;

  const save = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const { allocations } = await api.setTemplateSkills(templateId, {
        // Only send a half the caller may edit AND has changed; an omitted half
        // is left untouched server-side.
        ...(sharedDirty ? { shared_skill_ids: [...shared] } : {}),
        ...(mineDirty ? { my_skill_ids: [...mine] } : {}),
      });
      applyAllocations(allocations);
      setNotice("Allocations saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save allocations"));
    } finally {
      setBusy(false);
    }
  };

  const reset = () => {
    setShared(new Set(baseShared));
    setMine(new Set(baseMine));
    setNotice("");
  };

  if (loading) {
    return (
      <Card>
        <p className="text-sm text-faint">
          <Spinner /> Loading skill allocations…
        </p>
      </Card>
    );
  }

  return (
    <Card className="space-y-5">
      <div>
        <h2 className="text-sm font-semibold uppercase tracking-wide text-faint">Skills</h2>
        <p className="mt-1 text-sm text-muted">
          Skills allocated to this agent load into its runs. Your runs get the union of the shared
          allocations and your own overlay.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert message={notice} tone="success" />}

      <div>
        <span className="text-xs font-semibold uppercase tracking-wider text-faint">
          Your runs get ({union.length})
        </span>
        {union.length === 0 ? (
          <p className="mt-1 text-sm text-faint">No skills allocated yet.</p>
        ) : (
          <ul aria-label="Skills your runs get" className="mt-2 flex flex-wrap gap-1.5">
            {union.map((s) => (
              <li key={s.id}>
                <Badge tone={scopeBadgeTone(s.scope)} title={s.description}>
                  {s.name}
                </Badge>
              </li>
            ))}
          </ul>
        )}
      </div>

      {isAdmin && (
        <SkillChecklist
          title="Shared (everyone's runs on this agent)"
          hint="Admin-managed. Builtin and global skills only."
          options={sharedOptions}
          selected={shared}
          onToggle={(id) => setShared((prev) => toggle(prev, id))}
          emptyText="No builtin or global skills exist yet."
        />
      )}

      <SkillChecklist
        title="My skills for this agent"
        hint="Only your runs see these. Builtin, global, or your own private skills."
        options={mineOptions}
        selected={mine}
        onToggle={(id) => setMine((prev) => toggle(prev, id))}
        emptyText="You have no skills to allocate yet."
      />

      <div className="flex items-center gap-2">
        <Button size="sm" disabled={!dirty || busy} onClick={save}>
          {busy ? "Saving…" : "Save allocations"}
        </Button>
        <Button size="sm" variant="ghost" disabled={!dirty || busy} onClick={reset}>
          Reset
        </Button>
      </div>
    </Card>
  );
}

function SkillChecklist({
  title,
  hint,
  options,
  selected,
  onToggle,
  emptyText,
}: {
  title: string;
  hint: string;
  options: Skill[];
  selected: Set<string>;
  onToggle: (id: string) => void;
  emptyText: string;
}) {
  return (
    <fieldset className="space-y-2">
      <legend className="text-sm font-medium text-fg">{title}</legend>
      <p className="text-xs text-faint">{hint}</p>
      {options.length === 0 ? (
        <p className="text-sm text-faint">{emptyText}</p>
      ) : (
        <div className="space-y-1.5 rounded-lg border border-edge bg-raised/40 p-2">
          {options.map((s) => (
            <label
              key={s.id}
              className="flex cursor-pointer items-start gap-2.5 rounded-md px-2 py-1.5 hover:bg-raised"
            >
              <input
                type="checkbox"
                checked={selected.has(s.id)}
                onChange={() => onToggle(s.id)}
                className="mt-0.5 h-4 w-4 shrink-0 accent-brand"
              />
              <span className="min-w-0">
                <span className="flex items-center gap-1.5">
                  <span className="font-mono text-sm text-fg">{s.name}</span>
                  <Badge tone={scopeBadgeTone(s.scope)}>{SCOPE_LABEL[s.scope]}</Badge>
                </span>
                <span className="block text-xs text-muted">{s.description}</span>
              </span>
            </label>
          ))}
        </div>
      )}
    </fieldset>
  );
}

function toggle(set: Set<string>, id: string): Set<string> {
  const next = new Set(set);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  return next;
}

function setsEqual(a: Set<string>, b: Set<string>): boolean {
  if (a.size !== b.size) return false;
  for (const x of a) if (!b.has(x)) return false;
  return true;
}
