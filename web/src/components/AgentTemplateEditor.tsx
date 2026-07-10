import { useMemo, useState, type FormEvent, type KeyboardEvent } from "react";
import type { AgentTemplateInput } from "../lib/api";
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
  submitLabel,
  busy,
  error,
  onSubmit,
}: {
  initial: EditorInitial;
  nameEditable: boolean;
  scopeEditable?: boolean;
  isAdmin?: boolean;
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
