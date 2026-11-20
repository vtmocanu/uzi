// SchedulePreview (issue #3260): the read-only "if fired and claimed now" block of the schedule
// modal. It only DISPLAYS what the server predicts (lib/useSchedulePreview.ts fetches,
// lib/schedulePreview.ts builds the rows); it applies no precedence and never touches the save
// button, so a loading or failed preview can never gate or break Save.

import { useId } from "react";
import type { ScheduleSettingsPreviewRequest } from "../lib/apiTypes";
import type {
  IgnoredPinView,
  ScheduleBranchView,
  SchedulePreviewView,
  SubagentsView,
  ValueRow,
} from "../lib/schedulePreview";
import { useSchedulePreview } from "../lib/useSchedulePreview";
import { Alert } from "./ui";

const RUN_DEFAULTS_PATH = "/settings/run-defaults";
const LOADING_TEXT = "Resolving…";
const ERROR_TEXT = "Preview unavailable; Save will validate your choices.";
const REFUSED_NOTE = "Save will validate your choices.";
const DEFAULT_DEBOUNCE_MS = 250;

const FOOTER = [
  "Pinned values are fixed when the schedule fires.",
  'Predicted values marked "fixed when it fires" (such as the harness) are decided when the schedule fires, from your settings and credentials at that moment.',
  'Values marked "read when a worker picks the run up" can still change after the schedule fires, until a worker claims the run.',
  "This preview is advisory; Save validates again.",
];

function Row({ label, value, children }: { label: string; value: React.ReactNode; children?: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 break-words font-mono text-fg">{value}</dd>
      <dd className="col-start-2 min-w-0 break-words text-muted sm:col-start-auto">{children}</dd>
    </>
  );
}

function ValueRows({ label, row, children }: { label: string; row: ValueRow | null; children?: React.ReactNode }) {
  if (!row) return null;
  return (
    <Row label={label} value={row.value}>
      {[row.source, row.timing].filter(Boolean).join(" · ")}
      {children}
    </Row>
  );
}

// The link is its own >= 24px target; a bare 12px text line is too small to hit. It opens in a
// new tab: a same-tab navigation would close the modal and lose the draft.
function RunDefaultsLink() {
  return (
    <a
      href={RUN_DEFAULTS_PATH}
      target="_blank"
      rel="noopener noreferrer"
      draggable={false}
      className="inline-flex min-h-6 items-center text-brand hover:text-brand-hover"
    >
      Run defaults (opens in new tab)
    </a>
  );
}

function EffortRow({ label, row, link }: { label: string; row: ValueRow | null; link: boolean }) {
  return (
    <ValueRows label={label} row={row}>
      {" · schedules can't change it"}
      {link && (
        <>
          {" · "}
          <RunDefaultsLink />
        </>
      )}
    </ValueRows>
  );
}

function subagentsDetail(s: SubagentsView): string {
  const parts = [`lead: ${s.lead}`, ...s.pins];
  if (s.more) parts.push("and more");
  parts.push(...s.notes);
  return parts.join(" · ");
}

function SubagentsRow({ s, prefix }: { s: SubagentsView | null; prefix?: string }) {
  if (!s) return null;
  return (
    <Row label={prefix ? `${prefix} subagents` : "Subagents"} value={s.mode}>
      {subagentsDetail(s)}
    </Row>
  );
}

function BranchRows({ b, link }: { b: ScheduleBranchView; link: boolean }) {
  if (b.refusal) {
    return (
      <Row label={b.harness} value="Refused">
        {b.refusal}
      </Row>
    );
  }
  return (
    <>
      <ValueRows label={`${b.harness} model`} row={b.model} />
      <EffortRow label={`${b.harness} effort`} row={b.effort} link={link} />
      <ValueRows label={`${b.harness} credential`} row={b.credential} />
      <SubagentsRow s={b.subagents} prefix={b.harness} />
    </>
  );
}

function otherHarness(h: string | null): string | null {
  if (h === "Claude") return "Codex";
  if (h === "Codex") return "Claude";
  return null;
}

function ignoredPinMessage(p: IgnoredPinView, bothUsable: boolean): string {
  const fallback = p.fallbackSource ? `${p.fallback} (${p.fallbackSource})` : p.fallback;
  const other = otherHarness(p.harness);
  const advice = bothUsable && other ? `Pin the harness to ${other}.` : `Pick a ${p.harness} model.`;
  return `The ${p.pin} pin does not apply to ${p.harness} runs; this schedule would use ${fallback}. ${advice}`;
}

function Rows({ view }: { view: SchedulePreviewView }) {
  if (view.refusal) {
    return (
      <Row label="Refused" value={view.refusal}>
        {REFUSED_NOTE}
      </Row>
    );
  }
  const h = view.harness;
  const notes = view.notes.join(", ");
  // The Run defaults link appears once: on the top-level effort row, else the first branch with one.
  const firstWithEffort = view.effort ? null : view.alternatives.find((b) => b.effort);
  return (
    <>
      {h && (
        <Row label="Harness" value={h.chosenAtCreate ? "chosen when it fires" : h.value}>
          {[h.source, h.timing, notes].filter(Boolean).join(" · ")}
        </Row>
      )}
      {h?.chosenAtCreate &&
        view.alternatives.map((b) => <BranchRows key={b.harness} b={b} link={b === firstWithEffort} />)}
      <ValueRows label="Model" row={view.model} />
      <EffortRow label="Effort" row={view.effort} link />
      <ValueRows label="Credential" row={view.credential} />
      <SubagentsRow s={view.subagents} />
    </>
  );
}

export function SchedulePreview({
  request,
  bothUsable,
  debounceMs,
}: {
  request: ScheduleSettingsPreviewRequest | null;
  /** Whether the harness picker is shown, i.e. both harnesses are usable. */
  bothUsable: boolean;
  debounceMs?: number;
}) {
  const result = useSchedulePreview(request, { debounceMs: debounceMs ?? DEFAULT_DEBOUNCE_MS });
  const headingId = useId();
  if (result.status === "idle") return null;
  const view = result.status === "ready" ? result.view : null;
  const ignored = view && !view.refusal ? view.ignoredPins : [];

  return (
    <section
      aria-labelledby={headingId}
      aria-busy={result.status === "loading"}
      className="space-y-2 border-t border-edge pt-3 text-xs text-fg"
    >
      <div className="flex flex-wrap items-baseline justify-between gap-x-3">
        <h3 id={headingId} className="text-[13px] font-semibold text-fg">
          If fired and claimed now
        </h3>
        <span className="text-faint">updates as you change the picks above</span>
      </div>
      {result.status === "loading" && <p>{LOADING_TEXT}</p>}
      {result.status === "error" && <p>{ERROR_TEXT}</p>}
      {view && (
        <>
          <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5 sm:grid-cols-[auto_minmax(0,auto)_minmax(0,1fr)]">
            <Rows view={view} />
          </dl>
          {ignored.map((p) => (
            <Alert key={`${p.harness}:${p.pin}`} tone="warning" message={ignoredPinMessage(p, bothUsable)} />
          ))}
          <div className="space-y-0.5 text-muted">
            {FOOTER.map((line) => (
              <p key={line}>{line}</p>
            ))}
          </div>
        </>
      )}
    </section>
  );
}
