import {
  forwardRef,
  useImperativeHandle,
  useMemo,
  useState,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { diffLines, diffWords, type Change } from "diff";
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
  toolsSummary,
  driftedColumns,
  type TemplateContent,
  unknownTools,
} from "../lib/agentTemplates";

export interface EditorInitial {
  name: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
}

// AgentTemplateEditorHandle exposes the form's LIVE values to whoever owns the
// Reset button (issue #201 M4a F14). Reset discards unsaved edits, so its
// confirmation has to name what is on screen, not what was last saved.
//
// A REF RATHER THAN A CALLBACK, deliberately: the value is needed at exactly one
// instant — the click — so lifting it through an onChange would re-render the
// page, the diff panel and the skills panel on every keystroke to serve a read
// that happens once. A ref costs nothing until it is read.
export interface AgentTemplateEditorHandle {
  currentContent(): TemplateContent;
}

interface AgentTemplateEditorProps {
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
  // storedDiffers is the SAVED row's server-computed verdict. The diff panel
  // compares against live form state, so the two disagree the moment anyone
  // edits; passing it in is what lets the panel say WHICH it is talking about
  // instead of leaving the header badge to contradict it from 370px away.
  storedDiffers?: boolean;
  submitLabel: string;
  busy: boolean;
  error: string;
  onSubmit: (input: AgentTemplateInput) => void;
}

// AgentTemplateEditor is the shared form for creating and editing a template.
// name is editable only on create (the immutable subagent identity afterwards);
// scope is chosen on create too (PRD #18 M6): any user authors a private "Mine"
// template, and an admin can additionally publish a "Global" one. It carries a
// live preview of the exported subagent file.
export const AgentTemplateEditor = forwardRef<
  AgentTemplateEditorHandle,
  AgentTemplateEditorProps
>(function AgentTemplateEditor(
  {
    initial,
    nameEditable,
    scopeEditable = false,
    isAdmin = false,
    builtin = null,
    storedDiffers = false,
    submitLabel,
    busy,
    error,
    onSubmit,
  },
  ref,
) {
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

  // currentContent is the form's live values in the shape the comparison reads.
  // ONE object feeds both the diff panel and the imperative handle, so the panel
  // and the Reset confirmation cannot end up describing different states — the
  // failure F14 is about, one level down.
  const currentContent = useMemo<TemplateContent>(
    () => ({
      description,
      model: model.trim() || null,
      tools: tools.length ? tools : null,
      prompt_body: promptBody,
    }),
    [description, model, tools, promptBody],
  );

  useImperativeHandle(ref, () => ({ currentContent: () => currentContent }), [currentContent]);

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
          storedDiffers={storedDiffers}
          current={currentContent}
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
});

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

// NBSP keeps a blank diff line from collapsing to zero height. Written as an
// escape rather than as a literal on purpose: as a raw U+00A0 it is
// indistinguishable from an ASCII space in every editor and terminal, and it
// already cost one reviewer a mutation run whose pattern silently failed to
// match. An invisible character in source is a trap for the next person folding
// this code, not a formatting detail.
const NBSP = "\u00a0";

function BuiltinDiff({
  shipped,
  current,
  storedDiffers,
}: {
  shipped: BuiltinDefinition;
  current: TemplateContent;
  // storedDiffers is the SAVED row's verdict, straight from the server DTO. The
  // panel below compares against the CURRENT FORM STATE, so the two legitimately
  // disagree the moment anyone edits — and they sit ~370px apart, far enough that
  // an admin reads one with the other off screen. Whichever is true, the panel
  // says so explicitly rather than leaving two unqualified sentences to contradict
  // each other in different parts of the page.
  storedDiffers: boolean;
}) {
  const changed = useMemo(() => driftedColumns(shipped, current), [shipped, current]);

  if (changed.length === 0) {
    return (
      <div className="rounded-lg border border-edge bg-raised/40 p-3 text-sm text-muted">
        <span className="font-medium text-fg">Matches the shipped definition.</span> Nothing
        here differs from what this release ships for <code className="font-mono">{shipped.name}</code>.
        {storedDiffers && (
          <span>
            {" "}
            The <strong className="font-medium text-fg">saved</strong> template still differs —
            save these changes to clear the badge.
          </span>
        )}
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
          {!storedDiffers && (
            <span>
              {" "}
              These are <strong className="font-medium text-fg">unsaved</strong> edits; the
              saved template still matches what is shipped.
            </span>
          )}
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
          <PromptBodyDiff shipped={shipped.prompt_body} current={current.prompt_body} />
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

// SR_MARKER labels a diff side for assistive tech. WCAG 1.4.1: colour may not be
// the only carrier of meaning, and the word-diff below has no +/- prefix to lean
// on the way the line and tools diffs do.
//
// THESE ARE sr-only SPANS AND DELIBERATELY NOT `<ins>`/`<del>`, which would be the
// obvious semantic choice. The XSS canary in AgentDetail.test.tsx asserts the
// rendered diff contains ZERO ins/del elements — that is the whole of how it
// detects jsdiff's convertChangesToXML, which escapes its payload but not its own
// wrappers. Adopting the semantic elements here would silently destroy that
// canary. If a future change genuinely needs them, RESHAPE THE CANARY IN THE SAME
// COMMIT and record why; a guard deleted to make an unrelated change compile is
// how this class of guard dies.
const SR_MARKER = { added: "now: ", removed: "shipped: " } as const;

// WORD_MARK wraps a changed run in git's `--word-diff` delimiters. This is the
// VISIBLE half of the distinction (issue #201 M4a F15): an sr-only span is
// clipped to 1px, so before this a sighted colour-blind reader had nothing but
// the tints, while LineDiff and ToolsDiff both carried visible +/-.
//
// A word diff runs INSIDE a sentence, so it cannot take a line prefix the way its
// two siblings do — hence delimiters rather than a leading +/-. The direction
// still reads the same way round: `-` is shipped, `+` is current.
//
// 🔴 NOT `<ins>`/`<del>`, which are the semantically obvious elements and are
// banned here. F2's canary asserts the rendered diff contains ZERO ins/del,
// because that is precisely what convertChangesToXML emits and it is the only
// assertion that discriminates an HTML-string renderer — the img assertion passes
// under every unsafe form. Adopting them would delete the guard silently. This is
// the same conflict Amendment 4 resolved for the screen-reader half, arriving
// from the visual side.
const WORD_MARK = {
  added: ["{+", "+}"],
  removed: ["[-", "-]"],
} as const;

// InlineDiff renders a word-level diff as text nodes: shipped-only words in the
// shipped tone, current-only words in the edited tone, each carrying BOTH a
// visible delimiter and an sr-only marker.
//
// The two channels are deliberately disjoint. The delimiters are aria-hidden, so
// a screen reader gets the clean "shipped:"/"now:" wording instead of hearing
// "bracket minus"; a sighted reader gets the marks and never the sr-only text.
// Neither channel is colour.
function InlineDiff({ parts }: { parts: Change[] }) {
  return (
    <>
      {parts.map((p, i) => {
        const kind = p.added ? "added" : p.removed ? "removed" : null;
        if (!kind) {
          return (
            <span key={i} className="text-muted">
              {p.value}
            </span>
          );
        }
        const [open, close] = WORD_MARK[kind];
        return (
          <span
            key={i}
            className={kind === "added" ? "bg-danger/15 text-danger" : "bg-ok/15 text-ok"}
          >
            <span className="sr-only">{SR_MARKER[kind]}</span>
            <span aria-hidden="true">{open}</span>
            {p.value}
            <span aria-hidden="true">{close}</span>
          </span>
        );
      })}
    </>
  );
}

// ToolsDiff shows the allowlist as BEFORE and AFTER rather than as a sequence
// diff.
//
// The sequence form was correct and unreadable: the case this milestone most
// needs to communicate is a pure REORDER, and a sequence diff renders that as
// "Bash removed … Bash added" — two rows naming the same tool, which reads as a
// rendering bug rather than as the answer to "what changed". Order still matters
// and is still compared order-sensitively upstream; it is only the DISPLAY that
// stops trying to express a move. This is the shape web-ux rated the most legible
// of the four, already used by the model row.
function ToolsDiff({ shipped, current }: { shipped: string[] | null; current: string[] | null }) {
  return (
    <>
      <span className="block bg-ok/15 text-ok">
        <span className="sr-only">{SR_MARKER.removed}</span>- {toolsSummary(shipped)}
      </span>
      <span className="block bg-danger/15 text-danger">
        <span className="sr-only">{SR_MARKER.added}</span>+ {toolsSummary(current)}
      </span>
    </>
  );
}

// PromptBodyDiff handles the one difference `diffLines` cannot render usefully: a
// trailing-newline mismatch.
//
// `diffLines` keeps the newline inside its token, so a body ending "…main." and
// one ending "…main.\n" come back as one removed and one added line of
// BYTE-IDENTICAL text — one green, one red, with nothing on screen saying why.
// It is reachable on real data rather than theoretical: every builtin `.md` ends
// with a newline, the editor's textarea adds none, and `prompt_body` is submitted
// verbatim, so an admin who retypes the last line earns a permanent unexplained
// "changed" row.
//
// A trailing newline is a TERMINATOR, not content, so it is normalized away for
// the DISPLAY only. Nothing upstream is trimmed: driftedColumns and the server's
// SameContent both still compare exactly, so a whitespace-only edit still badges
// and still lists "prompt body" here — which is why the whitespace-only case gets
// its own sentence rather than an empty panel. Without it, the panel would name a
// changed column and then show nothing that changed.
function PromptBodyDiff({ shipped, current }: { shipped: string; current: string }) {
  const oneTrailingNewline = (s: string) => s.replace(/\n*$/, "\n");
  const a = oneTrailingNewline(shipped);
  const b = oneTrailingNewline(current);

  if (a === b) {
    return (
      <span className="text-muted">
        Identical except for trailing whitespace at the end of the body.
      </span>
    );
  }
  return <LineDiff parts={diffLines(a, b)} />;
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
          {r.text || NBSP}
        </span>
      ))}
    </>
  );
}
