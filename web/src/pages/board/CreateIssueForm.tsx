import { useState } from "react";
import { api } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { forgePlatform } from "../../lib/forgeNoun";
import { Button, Card, Field, Input, SectionTitle, Textarea } from "../../components/ui";
import { useAuth } from "../../auth/AuthContext";

// CreateIssueForm opens a runnable issue on the forge: the server labels it with the
// configured `uzi` label so uzi can work it (PRD #764). A prds/*.md link is optional
// but still detected, so the description carries a link slot as a hint.
export function CreateIssueForm({
  repoId,
  forgeType,
  onCreated,
  onError,
}: {
  repoId: string;
  forgeType: string;
  onCreated: () => void;
  onError: (m: string) => void;
}) {
  const { uziLabel } = useAuth();
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [saving, setSaving] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    onError("");
    setSaving(true);
    try {
      await api.createIssue(repoId, title.trim(), description);
      onCreated();
    } catch (err) {
      onError(errorMessage(err, "Could not create the issue"));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card className="max-w-2xl space-y-3">
      <SectionTitle>Create an issue</SectionTitle>
      <p className="text-xs text-faint">
        Opened on {forgePlatform(forgeType)} with the <span className="font-medium text-muted">{uziLabel}</span> label,
        so a run can be started from it. Optionally link a{" "}
        <code className="rounded bg-raised px-1 py-0.5 text-muted">prds/*.md</code> file in the description — a linked
        PRD is detected and shown with a badge.
      </p>
      <form onSubmit={submit} className="space-y-3">
        <Field label="Title">
          <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Issue title" />
        </Field>
        <Field label="Description">
          <Textarea
            rows={5}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder={"What to build…\n\nSee prds/N-feature.md"}
          />
        </Field>
        <Button type="submit" disabled={saving || title.trim() === ""}>
          {saving ? "Creating…" : "Create issue"}
        </Button>
      </form>
    </Card>
  );
}
