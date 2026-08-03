import { useMemo, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import { diffArrays, diffLines, diffWords, type Change } from "diff";
import type { AgentTemplateInput, BuiltinDefinition } from "../lib/api";
import { Alert, Button, Field, Input, Select } from "./ui";
import { ModelSelect } from "./ModelSelect";
import {
  frontmatterFieldWarning,
  KNOWN_TOOLS,
  looseSecretWarning,
  modelFieldWarning,
  renderSubagent,
  splitToolInput,
  unknownTools,
} from "../lib/agentTemplates";

export interface EditorInitial {
  name: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
}

// AgentTemplateEditor is the shared form for creating and editing a template.
// name is editable only on create (the immutable subagent identity afterwards);
// scope is chosen on create too (PRD #18 M6): any user authors a private "Mine"
// template, and an admin can additionally publish a "Global" one. It carries a
// live preview of the exported subagent file.
export function AgentTemplateEditor({
  initial,
  nameEditable,
  scopeEditable = false,
  isAdmin = false,
  builtin = null,
  submitLabel,
  busy,
  error,
  onSubmit,
}: {
  initial: EditorInitial;
  nameEditable: boolean;
  scopeEditable?: boolean;
  isAdmin?: boolean;
  // builtin is the definition the running release ships for this row, when there
  // is one (issue #201 M4a). Given it, the editor shows a shipped-vs-stored diff
  // so an admin can see what Reset would take away BEFORE pressing it. null for
  // every non-builtin row and for a builtin this release no longer ships — both
  // states the caller learns from GET /agent-templates/{id}/builtin.
  builtin?: BuiltinDefinition | null;
  submitLabel: string;
  busy: boolean;
  error: string;
  onSubmit: (input: AgentTemplateInput) => void;
}) {
  const [name, setName] = useState(initial.name);
  // Create defaults to a personal ("user") template; admins can switch to global.
  const [scope, setScope] = useState<"global" | "user">("user");
  const [description, setDescription] = useState(initial.description);
  const [model, setModel] = useState(initial.model ?? "");
  const [tools, setTools] = useState<string[]>(initial.tools ?? []);
  const [promptBody, setPromptBody] = useState(initial.prompt_body);
  const [toolDraft, setToolDraft] = useState("");

  const secretWarning = useMemo(
    () => looseSecretWarning(description) || looseSecretWarning(promptBody),
    [description, promptBody],
  );
  const unknown = useMemo(() => unknownTools(tools), [tools]);
  const modelWarning = useMemo(() => modelFieldWarning(model), [model]);
  const injectionWarning = useMemo(
    () => frontmatterFieldWarning({ description, model, tools }),
    [description, model, tools],
  );

  const preview = useMemo(
    () =>
      renderSubagent({
        name: name || "unnamed",
        description,
        model: model.trim() || null,
        tools: tools.length ? tools : null,
        prompt_body: promptBody,
      }),
    [name, description, model, tools, promptBody],
  );

  const addTool = (raw: string) => {
    const parts = splitToolInput(raw);
    if (parts.length) {
      const next = [...tools];
      for (const p of parts) if (!next.includes(p)) next.push(p);
      setTools(next);
    }
    setToolDraft("");
  };

  const onToolKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      addTool(toolDraft);
    } else if (e.key === "Backspace" && toolDraft === "" && tools.length) {
      setTools(tools.slice(0, -1));
    }
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    onSubmit({
      ...(nameEditable ? { name: name.trim() } : {}),
      // A non-admin can only author a user template; scope rides only on create.
      ...(scopeEditable ? { scope: isAdmin ? scope : "user" } : {}),
      description: description.trim(),
      model: model.trim() || null,
      tools: tools.length ? tools : null,
      prompt_body: promptBody,
    });
  };

  return (
    <form onSubmit={submit} className="space-y-5">
      {error && <Alert message={error} />}

      {nameEditable ? (
        <Field label="Name (kebab-case, immutable after creation)">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="my-helper"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
          />
        </Field>
      ) : (
        <div>
          <span className="text-sm font-medium text-muted">Name</span>
          <p className="mt-1 font-mono text-sm text-muted">{name}</p>
        </div>
      )}

      {scopeEditable &&
        (isAdmin ? (
          <Field label="Scope" htmlFor="template-scope">
            <Select
              id="template-scope"
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
              Personal (Mine) — only your runs will see this agent.
            </p>
          </div>
        ))}

      <Field label="Description (single sentence, used for routing)">
        <Input value={description} onChange={(e) => setDescription(e.target.value)} />
      </Field>

      <div className="space-y-2">
        <Field label="Model" htmlFor="template-model">
          <ModelSelect id="template-model" value={model} onChange={setModel} />
        </Field>
        {modelWarning && <Alert message={modelWarning} tone="warning" />}
      </div>

      <div className="space-y-1.5">
        <span className="text-sm font-medium text-muted">
          Tools (blank = inherit all)
        </span>
        <div className="flex flex-wrap items-center gap-1.5 rounded-lg border border-edge bg-raised px-2 py-2">
          {tools.map((t) => {
            const bad = !KNOWN_TOOLS.includes(t);
            return (
              <span
                key={t}
                className={`inline-flex items-center gap-1 rounded px-2 py-0.5 text-xs ${
                  bad
                    ? "bg-warn/10 text-warn"
                    : "bg-edge text-fg"
                }`}
              >
                {t}
                <button
                  type="button"
                  onClick={() => setTools(tools.filter((x) => x !== t))}
                  className="text-faint hover:text-fg"
                  aria-label={`Remove ${t}`}
                >
                  ×
                </button>
              </span>
            );
          })}
          <input
            list="known-tools"
            value={toolDraft}
            onChange={(e) => setToolDraft(e.target.value)}
            onKeyDown={onToolKey}
            onBlur={() => toolDraft && addTool(toolDraft)}
            placeholder={tools.length ? "" : "Bash, Read, …"}
            className="min-w-[8rem] flex-1 bg-transparent text-sm text-fg outline-none"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
          />
          <datalist id="known-tools">
            {KNOWN_TOOLS.map((t) => (
              <option key={t} value={t} />
            ))}
          </datalist>
        </div>
        {unknown.length > 0 && (
          <p className="text-xs text-warn">
            Unrecognised tool{unknown.length > 1 ? "s" : ""}: {unknown.join(", ")}. MCP
            tool names are fine; double-check for typos in core tools.
          </p>
        )}
      </div>

      <div className="space-y-1.5">
        <span className="text-sm font-medium text-muted">Prompt body (Markdown)</span>
        <textarea
          value={promptBody}
          onChange={(e) => setPromptBody(e.target.value)}
          rows={12}
          className="w-full rounded-lg border border-edge bg-raised px-3 py-2 font-mono text-sm text-fg outline-none focus:border-brand/70"
        />
      </div>

      {secretWarning && <Alert message={secretWarning} tone="warning" />}

      {injectionWarning && <Alert message={injectionWarning} tone="danger" />}

      {builtin && (
        <BuiltinDiff
          shipped={builtin}
          current={{
            description,
            model: model.trim() || null,
            tools: tools.length ? tools : null,
            prompt_body: promptBody,
          }}
        />
      )}

      <div>
        <span className="text-sm font-medium text-muted">
          Rendered subagent file (preview)
        </span>
        <pre className="mt-1.5 overflow-x-auto rounded-lg border border-edge bg-ink p-3 text-xs text-muted">
          {preview}
        </pre>
      </div>

      <Button type="submit" disabled={busy || !!injectionWarning}>
        {submitLabel}
      </Button>
    </form>
  );
}

// ── Shipped-vs-stored diff (issue #201 M4a) ──────────────────────────────────
//
// EVERY HUNK BELOW IS RENDERED AS REACT ELEMENTS, never as an HTML string. Most
// JS diff libraries hand back markup (diff2html, jsdiff's own
// convertChangesToXML), and using one would introduce the first
// dangerouslySetInnerHTML in web/src — a tree that today has zero call sites and
// eleven comments saying not to. The content being diffed is admin-editable
// template text, so it is exactly the input that must never reach an HTML sink.
// jsdiff's structured Change[] is used instead and each part becomes a text node.
//
// The comparison is shipped-vs-CURRENT-FORM-STATE rather than shipped-vs-stored,
// which is a superset: on load the form holds the stored values, so it opens as
// the shipped-vs-stored diff the milestone asks for, and it then stays honest as
// the admin types.

// DIFF_CONTEXT is how many unchanged lines flank a changed one in the prompt-body
// diff. Builtin bodies run to hundreds of lines, so rendering every unchanged one
// would bury the change; collapsing them keeps the hunk readable while still
// saying how much was skipped.
const DIFF_CONTEXT = 3;

// changedFields lists which of the four compared columns actually differ, using
// the SAME rules as the server's agenttmpl.SameContent: tools order-sensitive,
// null and [] both meaning inherit-all, and no trimming anywhere.
function changedFields(shipped: BuiltinDefinition, current: EditorDiffState): string[] {
  const out: string[] = [];
  if (shipped.description !== current.description) out.push("description");
  if ((shipped.model ?? "") !== (current.model ?? "")) out.push("model");
  const a = shipped.tools ?? [];
  const b = current.tools ?? [];
  if (a.length !== b.length || a.some((t, i) => t !== b[i])) out.push("tools");
  if (shipped.prompt_body !== current.prompt_body) out.push("prompt body");
  return out;
}

interface EditorDiffState {
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
}

function BuiltinDiff({
  shipped,
  current,
}: {
  shipped: BuiltinDefinition;
  current: EditorDiffState;
}) {
  const changed = useMemo(() => changedFields(shipped, current), [shipped, current]);

  if (changed.length === 0) {
    return (
      <div className="rounded-lg border border-edge bg-raised/40 p-3 text-sm text-muted">
        <span className="font-medium text-fg">Matches the shipped definition.</span> Nothing
        here differs from what this release ships for <code className="font-mono">{shipped.name}</code>.
      </div>
    );
  }

  return (
    <div className="space-y-3 rounded-lg border border-edge bg-raised/40 p-3">
      <div className="text-sm">
        <span className="font-medium text-fg">Differs from shipped</span>
        <span className="text-muted">
          {" "}
          — {changed.join(", ")}. Green is what this release ships; red is what this
          template says now. Reset replaces the red with the green.
        </span>
      </div>

      {changed.includes("description") && (
        <DiffField label="Description">
          <InlineDiff parts={diffWords(shipped.description, current.description)} />
        </DiffField>
      )}

      {changed.includes("model") && (
        <DiffField label="Model">
          <span className="text-ok">{shipped.model ?? "inherit"}</span>
          <span className="text-faint"> → </span>
          <span className="text-danger">{current.model ?? "inherit"}</span>
        </DiffField>
      )}

      {changed.includes("tools") && (
        <DiffField label="Tools">
          <ToolsDiff shipped={shipped.tools} current={current.tools} />
        </DiffField>
      )}

      {changed.includes("prompt body") && (
        <DiffField label="Prompt body">
          <LineDiff parts={diffLines(shipped.prompt_body, current.prompt_body)} />
        </DiffField>
      )}
    </div>
  );
}

function DiffField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="space-y-1">
      <span className="text-xs font-medium uppercase tracking-wide text-faint">{label}</span>
      <pre className="overflow-x-auto whitespace-pre-wrap rounded border border-edge bg-ink p-2 font-mono text-xs">
        {children}
      </pre>
    </div>
  );
}

// InlineDiff renders a word-level diff as text nodes: shipped-only words in the
// added tone, current-only words in the removed tone.
function InlineDiff({ parts }: { parts: Change[] }) {
  return (
    <>
      {parts.map((p, i) => (
        <span
          key={i}
          className={p.added ? "bg-danger/15 text-danger" : p.removed ? "bg-ok/15 text-ok" : "text-muted"}
        >
          {p.value}
        </span>
      ))}
    </>
  );
}

// ToolsDiff diffs the allowlist as a SEQUENCE, not a set: order is rendered into
// the subagent file, so a reordering is a real difference and shows up here as a
// move rather than as "no change".
function ToolsDiff({ shipped, current }: { shipped: string[] | null; current: string[] | null }) {
  const parts = diffArrays(shipped ?? [], current ?? []);
  return (
    <>
      {(shipped ?? []).length === 0 && <span className="text-ok">(shipped: inherit all){"\n"}</span>}
      {(current ?? []).length === 0 && <span className="text-danger">(now: inherit all){"\n"}</span>}
      {parts.map((p, i) => (
        <span
          key={i}
          className={p.added ? "bg-danger/15 text-danger" : p.removed ? "bg-ok/15 text-ok" : "text-muted"}
        >
          {`${p.added ? "+" : p.removed ? "-" : " "} ${(p.value as string[]).join(", ")}\n`}
        </span>
      ))}
    </>
  );
}

// LineDiff renders a line-level diff, collapsing long unchanged runs to a
// "N unchanged lines" marker so a one-line edit in a 300-line prompt stays
// findable. The marker states the count rather than hiding it silently — an
// elided run the reader cannot size is indistinguishable from a diff that missed
// something.
function LineDiff({ parts }: { parts: Change[] }) {
  const rows: { tone: "added" | "removed" | "same" | "elided"; text: string }[] = [];

  parts.forEach((p, idx) => {
    const lines = p.value.split("\n");
    // split() on a trailing newline yields a final "" that is not a line.
    if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();

    if (p.added || p.removed) {
      for (const line of lines) {
        rows.push({ tone: p.added ? "added" : "removed", text: line });
      }
      return;
    }
    const first = idx === 0;
    const last = idx === parts.length - 1;
    // Keep context on the side that faces a change; a leading or trailing
    // unchanged run only faces one.
    const head = first ? [] : lines.slice(0, DIFF_CONTEXT);
    const tail = last ? [] : lines.slice(-DIFF_CONTEXT);
    if (lines.length <= head.length + tail.length) {
      for (const line of lines) rows.push({ tone: "same", text: line });
      return;
    }
    for (const line of head) rows.push({ tone: "same", text: line });
    rows.push({ tone: "elided", text: `… ${lines.length - head.length - tail.length} unchanged lines …` });
    for (const line of tail) rows.push({ tone: "same", text: line });
  });

  const TONE = {
    // added/removed are inverted on purpose: the "added" side of this diff is the
    // CURRENT template (what an edit introduced), which Reset would take away.
    added: "bg-danger/15 text-danger",
    removed: "bg-ok/15 text-ok",
    same: "text-muted",
    elided: "text-faint",
  } as const;

  return (
    <>
      {rows.map((r, i) => (
        <span key={i} className={`block ${TONE[r.tone]}`}>
          {r.tone === "added" ? "+ " : r.tone === "removed" ? "- " : r.tone === "same" ? "  " : ""}
          {r.text || " "}
        </span>
      ))}
    </>
  );
}
