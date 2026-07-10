import { useCallback, useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type AgentTemplate, type AgentTemplateInput } from "../lib/api";
import { renderSubagent, summarizeTools } from "../lib/agentTemplates";
import { Alert, Button, Card } from "../components/ui";
import { AgentTemplateEditor } from "../components/AgentTemplateEditor";
import { SkillAllocationPanel } from "../components/SkillAllocationPanel";

export function AgentDetail() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;

  const [template, setTemplate] = useState<AgentTemplate | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [formError, setFormError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const { template } = await api.getAgentTemplate(id);
      setTemplate(template);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load template");
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => {
    load();
  }, [load]);

  const save = async (input: AgentTemplateInput) => {
    setFormError("");
    setNotice("");
    setBusy(true);
    try {
      const { template } = await api.updateAgentTemplate(id, input);
      setTemplate(template);
      setNotice("Saved.");
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Failed to save");
    } finally {
      setBusy(false);
    }
  };

  const reset = async () => {
    setFormError("");
    setNotice("");
    setBusy(true);
    try {
      const { template } = await api.resetAgentTemplate(id);
      setTemplate(template);
      setNotice("Reset to the builtin default.");
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Failed to reset");
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setFormError("");
    setBusy(true);
    try {
      await api.deleteAgentTemplate(id);
      navigate("/agents");
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Failed to delete");
      setBusy(false);
    }
  };

  if (loading) return <p className="text-faint">Loading…</p>;
  if (error) return <Alert message={error} />;
  if (!template) return <Alert message="Template not found." />;

  // Write authz mirrors the server (PRD #18 M6): a user template is owner-only,
  // builtin/global are admin-only.
  const canEdit = template.scope === "user" ? template.user_id === user?.id : isAdmin;

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="font-mono text-2xl font-semibold">{template.name}</h1>
          <p className="mt-1 text-muted">
            {template.is_builtin ? "Builtin template" : "Custom template"} · updated{" "}
            {new Date(template.updated_at).toLocaleString()}
          </p>
        </div>
        <Button variant="ghost" onClick={() => navigate("/agents")}>
          Back
        </Button>
      </div>

      {notice && <Alert message={notice} tone="success" />}

      {canEdit ? (
        <>
          <Card className="space-y-5">
            <h2 className="text-sm font-semibold uppercase tracking-wide text-faint">
              Edit
            </h2>
            <AgentTemplateEditor
              key={template.updated_at}
              initial={template}
              nameEditable={false}
              submitLabel="Save changes"
              busy={busy}
              error={formError}
              onSubmit={save}
            />
          </Card>

          <Card className="flex items-center justify-between gap-4">
            {template.is_builtin ? (
              <>
                <p className="text-sm text-muted">
                  Builtins cannot be deleted. Reset restores this template to its
                  shipped definition.
                </p>
                <Button variant="ghost" disabled={busy} onClick={reset}>
                  Reset to default
                </Button>
              </>
            ) : (
              <>
                <p className="text-sm text-muted">
                  Deleting a custom template is permanent.
                </p>
                <Button variant="danger" disabled={busy} onClick={remove}>
                  Delete template
                </Button>
              </>
            )}
          </Card>
        </>
      ) : (
        <ReadOnlyView template={template} />
      )}

      <SkillAllocationPanel templateId={template.id} isAdmin={isAdmin} userId={user?.id} />
    </div>
  );
}

function ReadOnlyView({ template }: { template: AgentTemplate }) {
  return (
    <Card className="space-y-4">
      <dl className="space-y-3 text-sm">
        <div>
          <dt className="text-faint">Description</dt>
          <dd className="text-fg">{template.description}</dd>
        </div>
        <div>
          <dt className="text-faint">Model</dt>
          <dd className="text-fg">{template.model ?? "inherit"}</dd>
        </div>
        <div>
          <dt className="text-faint">Tools</dt>
          <dd className="text-fg">{summarizeTools(template)}</dd>
        </div>
      </dl>
      <div>
        <span className="text-sm font-medium text-muted">Rendered subagent file</span>
        <pre className="mt-1.5 overflow-x-auto rounded-lg border border-edge bg-ink p-3 text-xs text-muted">
          {renderSubagent(template)}
        </pre>
      </div>
    </Card>
  );
}
