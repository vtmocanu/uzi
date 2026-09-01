import { useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  ApiError,
  type AgentTemplate,
  type AgentTemplateInput,
  type BuiltinDefinition,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import {
  driftedColumns,
  provenanceBadgeKind,
  renderSubagent,
  summarizeTools,
  SYNCED_BADGE_HINT,
  SYNCED_BADGE_LABEL,
  SYNCED_BADGE_TONE,
} from "../lib/agentTemplates";
import { Alert, Badge, Button, Card } from "../components/ui";
import {
  AgentTemplateEditor,
  type AgentTemplateEditorHandle,
} from "../components/AgentTemplateEditor";
import { SkillAllocationPanel } from "../components/SkillAllocationPanel";

// ShippedDefinition is what the page knows about the definition this release
// ships for the row being viewed — the shipped side of the editor's diff, and the
// input that decides whether Reset is offered.
//
// IT IS FOUR STATES RATHER THAN A NULLABLE VALUE BECAUSE TWO OF THEM READ
// IDENTICALLY FROM A NULL AND MEAN OPPOSITE THINGS:
//
//   - "absent" is a fact about the RELEASE. This binary ships no definition under
//     that name, Reset would answer 409, and the page may say so.
//   - "unavailable" is a fact about ONE REQUEST. A 500, a 502 or a dropped
//     connection says nothing at all about whether the definition exists, so the
//     page must not claim it does not — and must leave Reset exactly where it was.
//
// Collapsing the two was a regression in REACH as well as a false sentence: before
// this milestone Reset was offered for every builtin row, so no transient failure
// could take the button away. It also made the 409 in the test that looks like it
// pins this decorative — a parameterless catch passes identically on a 500.
type ShippedDefinition =
  | { kind: "none" } // not a builtin, or not a caller who could reset it: never fetched
  | { kind: "ok"; def: BuiltinDefinition }
  | { kind: "absent" }
  | { kind: "unavailable" };

export function AgentDetail() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const { user } = useAuth();
  const isAdmin = !!user?.is_admin;

  const [template, setTemplate] = useState<AgentTemplate | null>(null);
  const [formError, setFormError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [shipped, setShipped] = useState<ShippedDefinition>({ kind: "none" });
  // Read only when Reset is clicked — see AgentTemplateEditorHandle for why this
  // is a ref rather than lifted state.
  const editorRef = useRef<AgentTemplateEditorHandle>(null);

  const { loading, error } = useAsyncData(
    async ({ isCurrent }) => {
      const { template } = await api.getAgentTemplate(id);
      if (!isCurrent()) return;
      setTemplate(template);
      // Only a caller who could actually press Reset has any use for the shipped
      // side, and only that caller passes the endpoint's authz — for a builtin row
      // canEdit is exactly isAdmin, so without this guard every non-admin opening
      // any builtin detail page fires a request guaranteed to 403. Routine authz
      // denials are what a real one hides in.
      if (template.is_builtin && isAdmin) {
        try {
          const { builtin } = await api.getBuiltinAgentTemplate(id);
          if (isCurrent()) setShipped({ kind: "ok", def: builtin });
        } catch (err) {
          // THE STATUS IS THE WHOLE POINT: 409 is a fact about the RELEASE, any
          // other failure is a fact about ONE REQUEST, and only the first licenses
          // the page to say the definition does not exist. 403 joins the first
          // because it is likewise a settled answer rather than a transient one —
          // and it is unreachable from here anyway, since the Reset card renders
          // only for callers the endpoint would not refuse.
          const status = err instanceof ApiError ? err.status : 0;
          if (isCurrent())
            setShipped(
              status === 409 || status === 403 ? { kind: "absent" } : { kind: "unavailable" },
            );
        }
      }
    },
    [id, isAdmin],
    { fallback: "Failed to load template" },
  );

  const save = async (input: AgentTemplateInput) => {
    setFormError("");
    setNotice("");
    setBusy(true);
    try {
      const { template } = await api.updateAgentTemplate(id, input);
      setTemplate(template);
      setNotice("Saved.");
    } catch (err) {
      setFormError(errorMessage(err, "Failed to save"));
    } finally {
      setBusy(false);
    }
  };

  const reset = async () => {
    // F5: THE DIFF AND THIS BUTTON CANNOT BE ON SCREEN TOGETHER. Measured at
    // 1280x633 the diff panel ends at 871px and this button starts at 1524px — a
    // 653px gap on a 633px viewport, with the full-body preview between them, and
    // that was against an 11-line mock body where real builtins run 27-138 lines.
    // So the evidence this milestone exists to produce sits where it cannot reach
    // the click, and "the diff view is what makes Reset safe to press" would have
    // shipped false.
    //
    // The confirmation NAMES the columns rather than asking a generic
    // are-you-sure, following the deleteWarning precedent in AnthropicTokens.tsx
    // and for the same stated reason: a destructive action must say what it will
    // take, not leave the user to discover it afterwards. It reads the shared
    // driftedColumns, so it can never name a different set than the panel above.
    if (template && shipped.kind === "ok") {
      // COMPARE AGAINST WHAT IS ON SCREEN, NOT AGAINST THE SAVED ROW (F14).
      // Reset overwrites the row with the shipped definition and remounts the
      // form, so what the admin loses is the delta between what they are LOOKING
      // AT and what will replace it — which is unsaved edits and saved drift in
      // one set. Naming saved drift alone under-warns precisely the admin who is
      // mid-edit: measured, the dialog said "discards the current prompt body"
      // while silently discarding an unsaved description edit it never mentioned.
      // Falls back to the stored row only if the form is not mounted.
      const current = editorRef.current?.currentContent() ?? template;
      const columns = driftedColumns(shipped.def, current);
      const warning =
        columns.length > 0
          ? `Reset "${template.name}" to the definition shipped in this release?\n\n` +
            `This discards the current ${columns.join(", ")} and cannot be undone.`
          : `Reset "${template.name}" to the definition shipped in this release?\n\n` +
            `This template already matches what is shipped, so nothing will change.`;
      if (!window.confirm(warning)) return;
    }
    setFormError("");
    setNotice("");
    setBusy(true);
    try {
      const { template } = await api.resetAgentTemplate(id);
      setTemplate(template);
      // "shipped", not "builtin default": the badge, the panel and the reset card
      // all say shipped, and Amendment 1 §3 settled that noun deliberately —
      // M4b's classifier answers a different question ("does this match what it
      // was SEEDED with?"), and the shared vocabulary is what keeps the two from
      // reading as the same claim.
      setNotice("Reset to the shipped definition.");
    } catch (err) {
      setFormError(errorMessage(err, "Failed to reset"));
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
      setFormError(errorMessage(err, "Failed to delete"));
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
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="font-mono text-2xl font-semibold">{template.name}</h1>
            {/* Same copy as the Agents list, and deliberately not "customized":
                the question is whether this row matches THIS release's shipped
                body, not whether a human edited it.

                F10: the explanation is DESCRIBED and also carried in an sr-only
                span, because a bare `title` reaches mouse users only — the
                RunCredential pattern. It is never an aria-label: that would
                REPLACE the visible text, so a voice-control user saying "differs
                from shipped" would match nothing (WCAG 2.5.3 Label in Name). */}
            {/* PRD #602 M5: a synced row shows the SYNCED chip instead of the drift
                chip. Its body came from the configured agent-source repo; an
                overridden builtin still resets to its embedded default below. */}
            {provenanceBadgeKind(template) === "synced" && (
              <>
                <Badge tone={SYNCED_BADGE_TONE} aria-describedby="synced-hint" title={SYNCED_BADGE_HINT}>
                  {SYNCED_BADGE_LABEL}
                </Badge>
                <span id="synced-hint" className="sr-only">
                  {SYNCED_BADGE_HINT}
                </span>
              </>
            )}
            {provenanceBadgeKind(template) === "drift" && (
              <>
                <Badge
                  tone="info"
                  aria-describedby="drift-hint"
                  title="This template no longer matches the definition shipped in this release. The diff below shows what Reset would restore."
                >
                  differs from shipped
                </Badge>
                <span id="drift-hint" className="sr-only">
                  This template no longer matches the definition shipped in this release. The
                  diff below shows what Reset would restore.
                </span>
              </>
            )}
          </div>
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
              ref={editorRef}
              builtin={shipped.kind === "ok" ? shipped.def : null}
              storedDiffers={template.differs_from_builtin}
              submitLabel="Save changes"
              busy={busy}
              error={formError}
              onSubmit={save}
            />
          </Card>

          <Card className="flex items-center justify-between gap-4">
            {/* Reset is withheld ONLY on a settled answer that this release ships
                no such definition (409), where the button was previously offered
                and simply failed. A failed REQUEST must not take it away: that
                would be a new way to lose the recovery path, on exactly the page
                #210's ten templates have to be recovered through. */}
            {template.is_builtin && shipped.kind === "absent" ? (
              <p className="text-sm text-muted">
                Builtins cannot be deleted. This release no longer ships a
                definition for <code className="font-mono">{template.name}</code>,
                so there is nothing to reset it to.
              </p>
            ) : template.is_builtin ? (
              <>
                <p className="text-sm text-muted">
                  Builtins cannot be deleted. Reset restores this template to its
                  shipped definition.
                  {shipped.kind === "unavailable" && (
                    <span className="text-faint">
                      {" "}
                      The shipped definition could not be loaded, so no diff is
                      shown — this says nothing about whether one exists, and Reset
                      still works.
                    </span>
                  )}
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
