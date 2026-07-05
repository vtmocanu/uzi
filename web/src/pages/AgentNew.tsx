import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError, type AgentTemplateInput } from "../lib/api";
import { Button, Card } from "../components/ui";
import { AgentTemplateEditor } from "../components/AgentTemplateEditor";

const BLANK = {
  name: "",
  description: "",
  model: null,
  tools: null,
  prompt_body: "",
};

export function AgentNew() {
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const create = async (input: AgentTemplateInput) => {
    setError("");
    setBusy(true);
    try {
      const { template } = await api.createAgentTemplate(input);
      navigate(`/agents/${template.id}`);
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
          submitLabel="Create template"
          busy={busy}
          error={error}
          onSubmit={create}
        />
      </Card>
    </div>
  );
}
