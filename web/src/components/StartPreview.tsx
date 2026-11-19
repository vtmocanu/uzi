// StartPreview (issue #3261): the read-only "if started now" line shared by the board card,
// the issue page, the Run options dialog and the chat start card. It only DISPLAYS what the
// server predicts (lib/startPreviewStore.tsx fetches, lib/startPreview.ts builds the view);
// it never applies precedence and never touches a Start button's state, so a loading or
// failed preview can never gate or break Start.
//
// Variants: `compact` (a quiet one-line disclosure, closed by default), `open` (the same
// disclosure, open by default, headed "If it started now"), and `static` (no disclosure:
// one line, plus one line per chosen-at-create alternative). The disclosure is a real
// <button> with aria-expanded/aria-controls and a >= 24px target; nothing depends on hover.

import { useId, useState } from "react";
import { Link } from "react-router-dom";
import type { StartPreviewRequest } from "../lib/apiTypes";
import { compactSummary, type BranchView, type StartPreviewViewModel, type ValueView } from "../lib/startPreview";
import { useStartPreview } from "../lib/startPreviewStore";
import { cx } from "./ui";
import { ChevronDownIcon, ChevronRightIcon } from "./icons";

export type StartPreviewVariant = "compact" | "open" | "static";

const RUN_DEFAULTS_PATH = "/settings/run-defaults";
const LOADING_TEXT = "Resolving…";
const ERROR_TEXT = "Preview unavailable; Start will validate your choices.";

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function valueWithSource(v: ValueView | null): string {
  if (!v) return "unknown";
  return v.source ? `${v.text} (${v.source})` : v.text;
}

// branchText is the one-line text of a chosen-at-create alternative.
function branchText(b: BranchView): string {
  if (b.refusal) return `${capitalize(b.harness)}: ${b.refusal}`;
  const parts = [capitalize(b.harness), valueWithSource(b.model), valueWithSource(b.effort)];
  if (b.credential) parts.push(`token: ${b.credential}`);
  return parts.join(" · ");
}

function staticSources(view: StartPreviewViewModel): string {
  const parts: string[] = [];
  if (view.harnessSource) parts.push(`harness: ${view.harnessSource}`);
  if (view.model?.source) parts.push(`model: ${view.model.source}`);
  if (view.effort?.source) parts.push(`effort: ${view.effort.source}`);
  if (view.credential) parts.push(`token: ${view.credential}`);
  return parts.length > 0 ? ` (${parts.join(", ")})` : "";
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 break-words text-fg">{children}</dd>
    </>
  );
}

function Mono({ children }: { children: React.ReactNode }) {
  return <span className="font-mono">{children}</span>;
}

function ValueRow({ label, v }: { label: string; v: ValueView | null }) {
  return (
    <Row label={label}>
      <Mono>{v ? v.text : "unknown"}</Mono>
      {v?.source ? ` · ${v.source}` : ""}
    </Row>
  );
}

function Note({ view }: { view: StartPreviewViewModel }) {
  return (
    <>
      {view.fixed.claim && (
        <p>A prediction: claim-time settings are resolved when a worker picks the run up.</p>
      )}
      {view.fixed.override && <p>A choice made here is fixed on the run now.</p>}
    </>
  );
}

function Panel({
  view,
  id,
  hidden,
  showNote,
}: {
  view: StartPreviewViewModel;
  id: string;
  hidden: boolean;
  showNote: boolean;
}) {
  return (
    <div id={id} hidden={hidden} className="mt-1.5 space-y-1.5 text-xs text-fg">
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5">
        {view.chosenAtCreate ? (
          <Row label="Harness">
            Chosen when started{view.notes.length > 0 ? ` · ${view.notes.join(", ")}` : ""}
          </Row>
        ) : view.harness ? (
          <Row label="Harness">
            <Mono>{capitalize(view.harness)}</Mono>
            {view.harnessSource ? ` · ${view.harnessSource}` : ""}
            {view.notes.length > 0 ? ` · ${view.notes.join(", ")}` : ""}
          </Row>
        ) : null}
        {view.refusal && <Row label="Refused">{view.refusal}</Row>}
        {!view.refusal && !view.chosenAtCreate && (
          <>
            <ValueRow label="Model" v={view.model} />
            <ValueRow label="Effort" v={view.effort} />
          </>
        )}
        {view.credential && <Row label="Token">{view.credential}</Row>}
        {view.alternatives.map((b) => (
          <Row key={b.harness} label={capitalize(b.harness)}>
            {b.refusal ? (
              b.refusal
            ) : (
              <>
                <Mono>{b.model?.text ?? "unknown"}</Mono>
                {b.model?.source ? ` (${b.model.source})` : ""} · <Mono>{b.effort?.text ?? "unknown"}</Mono>
                {b.effort?.source ? ` (${b.effort.source})` : ""}
                {b.credential ? ` · token: ${b.credential}` : ""}
              </>
            )}
          </Row>
        ))}
      </dl>
      <div className="space-y-0.5 text-muted">
        {showNote && <Note view={view} />}
        <p>
          <RunDefaultsLink />
        </p>
      </div>
    </div>
  );
}

// The link is its own >= 24px target; a bare 12px text line is too small to hit.
function RunDefaultsLink() {
  return (
    <Link
      to={RUN_DEFAULTS_PATH}
      draggable={false}
      className="inline-flex min-h-6 items-center text-brand hover:text-brand-hover"
    >
      Run defaults
    </Link>
  );
}

export function StartPreview({
  request,
  variant,
  className,
  showNote = true,
}: {
  request: StartPreviewRequest | null;
  variant: StartPreviewVariant;
  className?: string;
  /** False where the surface already carries its own defaults hint (the Run options dialog). */
  showNote?: boolean;
}) {
  const result = useStartPreview(request);
  const [open, setOpen] = useState(variant === "open");
  const panelId = `${useId()}-start-preview`;

  // No provider: the surface shows nothing. No request (the dialog's invalid model): nothing.
  if (!result || result.status === "idle") return null;

  const wrap = (children: React.ReactNode) => (
    <div className={cx("text-xs text-fg", className)} data-start-preview={variant}>
      {children}
    </div>
  );

  if (result.status === "loading") return wrap(<p>{LOADING_TEXT}</p>);
  if (result.status === "error" || !result.view) return wrap(<p>{ERROR_TEXT}</p>);
  const view = result.view;

  if (variant === "static") {
    return wrap(
      <div className="space-y-0.5">
        {view.chosenAtCreate ? (
          <p>If started now: {compactSummary(view)}</p>
        ) : (
          <p>
            If started now: <Mono>{compactSummary(view)}</Mono>
            {view.refusal ? "" : staticSources(view)}
          </p>
        )}
        {view.chosenAtCreate && view.notes.length > 0 && <p className="text-muted">{view.notes.join(", ")}</p>}
        {view.alternatives.map((b) => (
          <p key={b.harness} className="pl-3 text-muted">
            {branchText(b)}
          </p>
        ))}
        <p>
          <RunDefaultsLink />
        </p>
      </div>,
    );
  }

  const label = variant === "open" ? "If it started now" : "If started now";
  return wrap(
    <>
      <button
        type="button"
        draggable={false}
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setOpen((o) => !o)}
        className="inline-flex min-h-6 max-w-full items-center gap-1 rounded text-left text-xs text-fg focus-visible:outline focus-visible:outline-2 focus-visible:outline-brand"
      >
        <span className="min-w-0 break-words">
          {label}: <Mono>{compactSummary(view)}</Mono>
        </span>
        <span aria-hidden="true" className="shrink-0">
          {open ? <ChevronDownIcon width={12} height={12} /> : <ChevronRightIcon width={12} height={12} />}
        </span>
      </button>
      <Panel view={view} id={panelId} hidden={!open} showNote={showNote} />
    </>,
  );
}
