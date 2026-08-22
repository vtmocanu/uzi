// Skills page (PRD #16): the three server scopes a user can see — Builtin,
// Global, and Mine — plus, for admins, other users' private skills (the landed
// read returns them; admins see all scopes). Everyone can author their own
// "Mine" skills; only admins create Global or edit/reset Builtin & Global.

import { useCallback, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type Skill, type SkillScope } from "../lib/api";
import {
  bodyError,
  byteLength,
  descriptionError,
  scopeBadgeTone,
  SCOPE_LABEL,
  SKILL_MAX_BYTES,
  skillNameError,
} from "../lib/skills";
import {
  Alert,
  Badge,
  Button,
  Card,
  EmptyState,
  Field,
  Input,
  ListSkeleton,
  PageHeader,
  Select,
} from "../components/ui";
import { PlusIcon, SkillIcon } from "../components/icons";
import { DocLink } from "../components/DocLink";
import { DOC_SKILLS } from "../lib/doclinks";

type EditState =
  | { mode: "create" }
  | { mode: "edit"; skill: Skill }
  | { mode: "view"; skill: Skill }
  | null;

type ConfirmState = { id: string; action: "reset" | "delete" } | null;

export function Skills() {
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;
  const [skills, setSkills] = useState<Skill[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [edit, setEdit] = useState<EditState>(null);
  const [confirm, setConfirm] = useState<ConfirmState>(null);
  const [busyId, setBusyId] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const { skills } = await api.listSkills();
      setSkills(skills);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load skills");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const groups = useMemo(() => {
    const builtin: Skill[] = [];
    const global: Skill[] = [];
    const mine: Skill[] = [];
    const others: Skill[] = [];
    for (const s of skills) {
      if (s.scope === "builtin") builtin.push(s);
      else if (s.scope === "global") global.push(s);
      else if (s.user_id === user?.id) mine.push(s);
      else others.push(s); // admin-only: another user's private skill
    }
    return { builtin, global, mine, others };
  }, [skills, user?.id]);

  const afterSave = (msg: string) => {
    setEdit(null);
    setNotice(msg);
    load();
  };

  const runReset = async (id: string) => {
    setConfirm(null);
    setBusyId(id);
    setError("");
    try {
      await api.resetSkill(id);
      setNotice("Reset to the shipped default.");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to reset");
    } finally {
      setBusyId(null);
    }
  };

  const runDelete = async (id: string) => {
    setConfirm(null);
    setBusyId(id);
    setError("");
    try {
      await api.deleteSkill(id);
      setNotice("Skill deleted.");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to delete");
    } finally {
      setBusyId(null);
    }
  };

  // canEdit mirrors the server write authz: builtin/global admin-only, user
  // owner-only.
  const canEdit = (s: Skill) =>
    s.scope === "user" ? s.user_id === user?.id : isAdmin;

  if (edit?.mode === "create") {
    return (
      <SkillEditor
        mode="create"
        isAdmin={isAdmin}
        onCancel={() => setEdit(null)}
        onSaved={() => afterSave("Skill created.")}
      />
    );
  }
  if (edit?.mode === "edit") {
    return (
      <SkillEditor
        mode="edit"
        skill={edit.skill}
        isAdmin={isAdmin}
        onCancel={() => setEdit(null)}
        onSaved={() => afterSave("Saved.")}
      />
    );
  }
  if (edit?.mode === "view") {
    return <SkillView skill={edit.skill} viewerId={user?.id} onBack={() => setEdit(null)} />;
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Skills"
        description={
          <>
            Reusable SKILL.md playbooks. Their one-line description sits cheaply in an agent's
            context; the body loads only when relevant. Allocate them to agents on an agent's page.
            See the <DocLink slug={DOC_SKILLS}>agent skills</DocLink> guide.
          </>
        }
        actions={
          <Button size="sm" onClick={() => setEdit({ mode: "create" })}>
            <PlusIcon /> New skill
          </Button>
        }
      />

      {error && <Alert message={error} />}
      {notice && <Alert message={notice} tone="success" />}

      {loading ? (
        <ListSkeleton rows={5} />
      ) : (
        <div className="space-y-8">
          <SkillGroup
            scope="builtin"
            heading="Builtin"
            blurb="Shipped with uzi. Admins can edit or reset them; they can never be deleted."
            skills={groups.builtin}
            emptyText="No builtin skills."
            isAdmin={isAdmin}
            canEdit={canEdit}
            confirm={confirm}
            busyId={busyId}
            onView={(s) => setEdit({ mode: "view", skill: s })}
            onEdit={(s) => setEdit({ mode: "edit", skill: s })}
            onConfirm={setConfirm}
            onReset={runReset}
            onDelete={runDelete}
          />
          <SkillGroup
            scope="global"
            heading="Global"
            blurb={
              isAdmin
                ? "Visible to everyone. Only admins create or edit global skills."
                : "Shared skills an admin has published for everyone."
            }
            skills={groups.global}
            emptyText="No global skills yet."
            isAdmin={isAdmin}
            canEdit={canEdit}
            confirm={confirm}
            busyId={busyId}
            onView={(s) => setEdit({ mode: "view", skill: s })}
            onEdit={(s) => setEdit({ mode: "edit", skill: s })}
            onConfirm={setConfirm}
            onReset={runReset}
            onDelete={runDelete}
          />
          <SkillGroup
            scope="user"
            heading="Mine"
            blurb="Your own private skills. Only your runs ever see them."
            skills={groups.mine}
            emptyNode={
              <EmptyState
                icon={<SkillIcon />}
                title="No personal skills yet"
                description="Write a playbook only your agents will use."
                action={
                  <Button size="sm" onClick={() => setEdit({ mode: "create" })}>
                    <PlusIcon /> New skill
                  </Button>
                }
              />
            }
            isAdmin={isAdmin}
            canEdit={canEdit}
            confirm={confirm}
            busyId={busyId}
            onView={(s) => setEdit({ mode: "view", skill: s })}
            onEdit={(s) => setEdit({ mode: "edit", skill: s })}
            onConfirm={setConfirm}
            onReset={runReset}
            onDelete={runDelete}
          />
          {isAdmin && groups.others.length > 0 && (
            <SkillGroup
              scope="user"
              heading="Other users"
              blurb="Private skills owned by other users. As an admin you can see them, but only their owner can edit or delete them."
              skills={groups.others}
              emptyText=""
              isAdmin={isAdmin}
              canEdit={canEdit}
              confirm={confirm}
              busyId={busyId}
              onView={(s) => setEdit({ mode: "view", skill: s })}
              onEdit={(s) => setEdit({ mode: "edit", skill: s })}
              onConfirm={setConfirm}
              onReset={runReset}
              onDelete={runDelete}
            />
          )}
        </div>
      )}
    </div>
  );
}

// ── Group table ──────────────────────────────────────────────────────────────

function SkillGroup({
  heading,
  blurb,
  skills,
  emptyText,
  emptyNode,
  isAdmin,
  canEdit,
  confirm,
  busyId,
  onView,
  onEdit,
  onConfirm,
  onReset,
  onDelete,
}: {
  scope: SkillScope;
  heading: string;
  blurb: string;
  skills: Skill[];
  emptyText?: string;
  emptyNode?: ReactNode;
  isAdmin: boolean;
  canEdit: (s: Skill) => boolean;
  confirm: ConfirmState;
  busyId: string | null;
  onView: (s: Skill) => void;
  onEdit: (s: Skill) => void;
  onConfirm: (c: ConfirmState) => void;
  onReset: (id: string) => void;
  onDelete: (id: string) => void;
}) {
  return (
    <section className="space-y-2">
      <div>
        <h2 className="text-sm font-semibold text-fg">{heading}</h2>
        <p className="text-xs text-faint">{blurb}</p>
      </div>
      {skills.length === 0 ? (
        emptyNode ?? (emptyText ? <p className="text-sm text-faint">{emptyText}</p> : null)
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Name</th>
                  <th className="px-4 py-3 font-medium">Description</th>
                  <th className="px-4 py-3 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {skills.map((s) => (
                  <SkillRow
                    key={s.id}
                    skill={s}
                    editable={canEdit(s)}
                    isAdmin={isAdmin}
                    confirm={confirm}
                    busy={busyId === s.id}
                    onView={onView}
                    onEdit={onEdit}
                    onConfirm={onConfirm}
                    onReset={onReset}
                    onDelete={onDelete}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </section>
  );
}

function SkillRow({
  skill,
  editable,
  confirm,
  busy,
  onView,
  onEdit,
  onConfirm,
  onReset,
  onDelete,
}: {
  skill: Skill;
  editable: boolean;
  isAdmin: boolean;
  confirm: ConfirmState;
  busy: boolean;
  onView: (s: Skill) => void;
  onEdit: (s: Skill) => void;
  onConfirm: (c: ConfirmState) => void;
  onReset: (id: string) => void;
  onDelete: (id: string) => void;
}) {
  const confirming = confirm?.id === skill.id ? confirm.action : null;
  const isBuiltin = skill.scope === "builtin";

  return (
    <tr className="align-top transition-colors hover:bg-raised/30">
      <td className="px-4 py-3">
        <button
          type="button"
          onClick={() => (editable ? onEdit(skill) : onView(skill))}
          className="font-mono text-brand hover:text-brand-hover"
        >
          {skill.name}
        </button>
      </td>
      <td className="max-w-md px-4 py-3 text-muted">{skill.description}</td>
      <td className="px-4 py-3">
        <div className="flex justify-end gap-2">
          {confirming ? (
            <>
              <span className="self-center text-xs text-muted">
                {confirming === "reset" ? "Reset to shipped default?" : "Delete this skill?"}
              </span>
              <Button
                variant={confirming === "reset" ? "secondary" : "dangerSolid"}
                size="sm"
                disabled={busy}
                onClick={() => (confirming === "reset" ? onReset(skill.id) : onDelete(skill.id))}
              >
                Confirm
              </Button>
              <Button variant="ghost" size="sm" disabled={busy} onClick={() => onConfirm(null)}>
                Cancel
              </Button>
            </>
          ) : editable ? (
            <>
              <Button variant="secondary" size="sm" disabled={busy} onClick={() => onEdit(skill)}>
                Edit
              </Button>
              {isBuiltin ? (
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy}
                  onClick={() => onConfirm({ id: skill.id, action: "reset" })}
                >
                  Reset
                </Button>
              ) : (
                <Button
                  variant="danger"
                  size="sm"
                  disabled={busy}
                  onClick={() => onConfirm({ id: skill.id, action: "delete" })}
                >
                  Delete
                </Button>
              )}
            </>
          ) : (
            <Button variant="secondary" size="sm" onClick={() => onView(skill)}>
              View
            </Button>
          )}
        </div>
      </td>
    </tr>
  );
}

// ── Read-only view ───────────────────────────────────────────────────────────

function SkillView({
  skill,
  viewerId,
  onBack,
}: {
  skill: Skill;
  viewerId: string | undefined;
  onBack: () => void;
}) {
  // Don't label another user's private skill "Mine" (an admin can view one from
  // the "Other users" group). The DTO has no owner name, only user_id, so the
  // honest label without an API change is a generic "Other user".
  const scopeLabel =
    skill.scope === "user" && skill.user_id !== viewerId ? "Other user" : SCOPE_LABEL[skill.scope];
  return (
    <div className="space-y-6">
      <PageHeader
        titleNode={
          <div className="flex items-center gap-2">
            <h1 className="font-mono text-2xl font-semibold">{skill.name}</h1>
            <Badge tone={scopeBadgeTone(skill.scope)}>{scopeLabel}</Badge>
          </div>
        }
        description={skill.description}
        actions={
          <Button variant="ghost" size="sm" onClick={onBack}>
            Back
          </Button>
        }
      />
      <Card>
        <span className="text-sm font-medium text-muted">Body</span>
        <pre className="mt-1.5 overflow-x-auto whitespace-pre-wrap rounded-lg border border-edge bg-ink p-3 text-xs text-muted">
          {skill.body}
        </pre>
      </Card>
    </div>
  );
}

// ── Create / edit form ───────────────────────────────────────────────────────

function SkillEditor({
  mode,
  skill,
  isAdmin,
  onCancel,
  onSaved,
}: {
  mode: "create" | "edit";
  skill?: Skill;
  isAdmin: boolean;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(skill?.name ?? "");
  // Create defaults to a personal ("user") skill; admins can switch to global.
  const [scope, setScope] = useState<"global" | "user">(
    skill?.scope === "global" ? "global" : "user",
  );
  const [description, setDescription] = useState(skill?.description ?? "");
  const [body, setBody] = useState(skill?.body ?? "");
  const [busy, setBusy] = useState(false);
  const [serverError, setServerError] = useState("");

  const nameErr = mode === "create" ? skillNameError(name) : null;
  const descErr = descriptionError(description);
  const bErr = bodyError(body);
  const bytes = byteLength(body);
  const canSubmit = !nameErr && !descErr && !bErr && !busy;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!canSubmit) return;
    setBusy(true);
    setServerError("");
    try {
      if (mode === "create") {
        await api.createSkill({ name: name.trim(), description: description.trim(), body, scope });
      } else if (skill) {
        await api.updateSkill(skill.id, { description: description.trim(), body });
      }
      onSaved();
    } catch (err) {
      setServerError(err instanceof ApiError ? err.message : "Failed to save");
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title={mode === "create" ? "New skill" : `Edit ${skill?.name ?? "skill"}`}
        description={
          mode === "create"
            ? "The name is permanent once saved (it is the skill's identity). The description is always in an agent's context, so keep it to one focused sentence."
            : "The name and scope are fixed after creation. Description and body are editable."
        }
      />

      <Card className="space-y-5">
        <form onSubmit={submit} className="space-y-5">
          {serverError && <Alert message={serverError} />}

          {mode === "create" ? (
            <div className="space-y-1.5">
              <Field label="Name (kebab-case, permanent after creation)">
                <Input
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="team-runbook"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                />
              </Field>
              {name.trim() !== "" && nameErr && <p className="text-xs text-warn">{nameErr}</p>}
            </div>
          ) : (
            <div>
              <span className="text-sm font-medium text-muted">Name</span>
              <p className="mt-1 font-mono text-sm text-muted">{skill?.name}</p>
            </div>
          )}

          {mode === "create" ? (
            isAdmin ? (
              <Field label="Scope" htmlFor="skill-scope">
                <Select
                  id="skill-scope"
                  value={scope}
                  onChange={(e) => setScope(e.target.value as "global" | "user")}
                >
                  <option value="user">Mine — private to you</option>
                  <option value="global">Global — visible to everyone</option>
                </Select>
              </Field>
            ) : (
              <div>
                <span className="text-sm font-medium text-muted">Scope</span>
                <p className="mt-1 text-sm text-muted">
                  Personal (Mine) — only your runs will see this skill.
                </p>
              </div>
            )
          ) : (
            <div>
              <span className="text-sm font-medium text-muted">Scope</span>
              <p className="mt-1">
                <Badge tone={scopeBadgeTone(skill?.scope ?? "user")}>
                  {SCOPE_LABEL[skill?.scope ?? "user"]}
                </Badge>
              </p>
            </div>
          )}

          <div className="space-y-1.5">
            <Field label="Description (single line — the model routes on this)">
              <Input
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="When and why an agent should reach for this skill."
              />
            </Field>
            {description.trim() !== "" && descErr && <p className="text-xs text-warn">{descErr}</p>}
          </div>

          <div className="space-y-1.5">
            <div className="flex items-center justify-between">
              <span className="text-sm font-medium text-muted">Body (Markdown, SKILL.md)</span>
              <span className={bytes > SKILL_MAX_BYTES ? "text-xs text-warn" : "text-xs text-faint"}>
                {bytes.toLocaleString()} / {SKILL_MAX_BYTES.toLocaleString()} bytes
              </span>
            </div>
            <textarea
              value={body}
              onChange={(e) => setBody(e.target.value)}
              rows={16}
              aria-label="Body (Markdown, SKILL.md)"
              placeholder={"# my-skill\n\nWhat this playbook covers and the steps to follow."}
              className="w-full rounded-lg border border-edge bg-raised px-3 py-2 font-mono text-sm text-fg outline-hidden focus:border-brand/70"
            />
            {body.trim() !== "" && bErr && <p className="text-xs text-warn">{bErr}</p>}
          </div>

          <div className="flex gap-2">
            <Button type="submit" disabled={!canSubmit}>
              {mode === "create" ? "Create skill" : "Save changes"}
            </Button>
            <Button type="button" variant="ghost" disabled={busy} onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </form>
      </Card>
    </div>
  );
}
