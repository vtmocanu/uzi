import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type AgentTemplateInput } from "../lib/api";
import { Button, Card } from "../components/ui";
import { AgentTemplateEditor } from "../components/AgentTemplateEditor";
import { DocLink } from "../components/DocLink";
import { DOC_AGENT_TEMPLATES } from "../lib/doclinks";

const BLANK = {
  name: "",
  description: "",
  model: null,
  tools: null,
  prompt_body: "",
};

export function AgentNew() {
  const navigate = useNavigate();
  const { user } = useAuth();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const create = async (input: AgentTemplateInput) => {
    setError("");
    setBusy(true);
    try {
      const { template } = await api.createAgentTemplate(input);
      // encodeURIComponent the id: per-call-site open-redirect hardening (see
      // safeNextPath in Login.tsx). A no-op for today's UUID ids.
      navigate(`/agents/${encodeURIComponent(template.id)}`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to create template");
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">New agent template</h1>
          <p className="mt-1 text-muted">
            The name becomes the subagent identity and cannot be changed later.
            {user?.is_admin
              ? " Publish it globally or keep it private to you."
              : " Your agent stays private to your runs."}
            {" "}See the <DocLink slug={DOC_AGENT_TEMPLATES}>agent templates</DocLink> guide.
          </p>
        </div>
        <Button variant="ghost" onClick={() => navigate("/agents")}>
          Back
        </Button>
      </div>

      <Card>
        <AgentTemplateEditor
          initial={BLANK}
          nameEditable
          scopeEditable
          isAdmin={!!user?.is_admin}
          submitLabel="Create template"
          busy={busy}
          error={error}
          onSubmit={create}
        />
      </Card>
    </div>
  );
}
