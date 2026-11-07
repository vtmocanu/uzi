// The Overview preview's "Issue work" card (PRD #3879): the issue lifecycle drawn as a line
// of five stations (Queued, Planning, Plan review as a gate, Building, PR open) with three
// bays under it (Asking you, Parked, Other work). Each station and bay is a toggle that
// reports the stage the viewer picked; the page owns that state. Counts and captions come
// from issueLineModel; this file only draws them. Every run field printed here is untrusted
// forge text and goes through stripUnsafeChars.

import { useId, type ReactNode } from "react";
import type { RunSummaryItem } from "../lib/api";
import { maskEmail } from "../lib/demoMask";
import { runRef, type IssueLineModel, type IssueStage, type IssueWorkScope, type LineEntry } from "../lib/issueLine";
import { stripUnsafeChars } from "../lib/safeText";
import { ActivityIcon, ChatIcon, ClockIcon } from "./icons";
import { Badge, RUN_STATUS_TONES, type BadgeTone } from "./ui";
import { SegmentedToggle } from "./usageParts";

// At most this many badges per station; the rest collapse into one "+N" badge.
const MAX_BADGES = 12;

// The classes a tone draws with: the node's outline, its filled form and halo, the gate's
// tint, and the custom property the flowing segment reads (see .issue-line-flow in index.css).
interface ToneClasses {
  outline: string;
  fill: string;
  halo: string;
  tint: string;
  flow: string;
}

const OK_TONE: ToneClasses = { outline: "border-ok", fill: "bg-ok", halo: "ring-ok/20", tint: "bg-ok/20", flow: "[--tone:var(--ok)]" };

const TONE_CLASSES: Record<string, ToneClasses> = {
  queue: { outline: "border-faint", fill: "bg-faint", halo: "ring-faint/20", tint: "bg-faint/20", flow: "[--tone:var(--faint)]" },
  plan: { outline: "border-plan", fill: "bg-plan", halo: "ring-plan/20", tint: "bg-plan/20", flow: "[--tone:var(--plan)]" },
  warning: { outline: "border-warn", fill: "bg-warn", halo: "ring-warn/20", tint: "bg-warn/20", flow: "[--tone:var(--warn)]" },
  info: { outline: "border-info", fill: "bg-info", halo: "ring-info/20", tint: "bg-info/20", flow: "[--tone:var(--info)]" },
  ok: OK_TONE,
};

type StationStage = "queued" | "planning" | "review" | "building" | "pr";

interface Station {
  stage: StationStage;
  label: string;
  // The run status tone (RUN_STATUS_TONES); PR open has no status, so it reads "ok".
  tone: BadgeTone;
  // Planning and Building carry a flowing segment into them while they hold live work.
  flows: boolean;
  gate?: boolean;
}

const STATIONS: Station[] = [
  { stage: "queued", label: "Queued", tone: RUN_STATUS_TONES.queued.tone, flows: false },
  { stage: "planning", label: "Planning", tone: RUN_STATUS_TONES.planning.tone, flows: true },
  { stage: "review", label: "Plan review", tone: RUN_STATUS_TONES.awaiting_approval.tone, flows: false, gate: true },
  { stage: "building", label: "Building", tone: RUN_STATUS_TONES.running.tone, flows: true },
  { stage: "pr", label: "PR open", tone: "ok", flows: false },
];

function RunBadges({
  runs,
  tone,
  demo,
}: {
  runs: { run: RunSummaryItem; own: boolean }[];
  tone: BadgeTone;
  demo: boolean;
}) {
  if (runs.length === 0) return null;
  const shown = runs.slice(0, MAX_BADGES);
  return (
    <span className="mt-2 flex min-w-0 flex-wrap gap-1">
      {shown.map(({ run, own }) => (
        <Badge
          key={run.id}
          tone={tone}
          // Another user's run names its owner on hover, masked in demo mode.
          title={!own && run.owner_email ? stripUnsafeChars(maskEmail(run.owner_email, demo)) : undefined}
        >
          {runRef(run)}
        </Badge>
      ))}
      {runs.length > shown.length && <Badge tone="neutral">+{runs.length - shown.length}</Badge>}
    </span>
  );
}

function StationButton({
  station,
  index,
  count,
  caption,
  note,
  entries,
  selected,
  onSelect,
  demo,
}: {
  station: Station;
  index: number;
  count: number;
  caption: string;
  note?: string;
  entries: LineEntry[];
  selected: boolean;
  onSelect: () => void;
  demo: boolean;
}) {
  const tone = TONE_CLASSES[station.tone] ?? OK_TONE;
  const live = station.flows && count > 0;
  const first = index === 0;
  const last = index === STATIONS.length - 1;
  return (
    <button
      type="button"
      aria-pressed={selected}
      data-stage={station.stage}
      data-count={count}
      onClick={onSelect}
      className="relative mx-0.5 flex min-w-0 flex-col items-start gap-0.5 rounded-xl border border-transparent py-3 pl-11 pr-2.5 text-left hover:bg-raised/55 aria-pressed:border-edge aria-pressed:bg-raised md:px-2.5 md:pb-3 md:pt-11"
    >
      {/* Into this station, horizontal from md up; below md the vertical rail alone carries
          the line. It flows only while the station holds live work; the flow is stopped
          under prefers-reduced-motion. */}
      {!first && (
        <span
          aria-hidden="true"
          data-segment={live ? "flow" : "static"}
          className={`absolute hidden md:left-[calc(25px-100%)] md:top-[25px] md:block md:h-0.5 md:w-[calc(100%+4px)] ${
            live ? `issue-line-flow ${tone.flow}` : "bg-edge-strong"
          }`}
        />
      )}
      {/* Vertical layout only: one continuous rail from the first node's centre to the last
          node's centre. Each piece reaches 1px past the button's padding box to cross its
          transparent border; the node centre sits 23px down (top-3.5 plus half the node). */}
      <span
        aria-hidden="true"
        data-rail=""
        className={`absolute left-6 w-0.5 bg-edge-strong md:hidden ${first ? "top-[23px]" : "-top-px"} ${
          last ? "h-6" : "-bottom-px"
        }`}
      />

      <span
        aria-hidden="true"
        data-node={station.gate ? "gate" : live ? "fill" : "outline"}
        className={`absolute left-4 top-3.5 size-[18px] border-2 md:top-[17px] ${tone.outline} ${
          station.gate
            ? `rotate-45 scale-[.88] rounded-[3px] ${tone.tint}`
            : live
              ? `rounded-full ${tone.fill} ring-4 ${tone.halo}`
              : "rounded-full bg-surface"
        }`}
      />
      <span className="text-xs font-medium uppercase tracking-wider text-faint">{station.label}</span>
      <span
        className={`text-3xl font-semibold leading-tight tabular-nums ${station.gate && count > 0 ? "text-warn" : "text-fg"}`}
      >
        {count}
        {station.stage === "pr" && (
          <small className="ml-1 text-[13px] font-medium text-faint">{count === 1 ? "PR" : "PRs"}</small>
        )}
      </span>
      <span className="text-xs text-faint">{caption}</span>
      {note && <span className="text-xs text-faint opacity-80">{note}</span>}
      <RunBadges runs={entries} tone={station.tone} demo={demo} />
    </button>
  );
}

function BayButton({
  stage,
  icon,
  warn,
  title,
  count,
  lines,
  selected,
  onSelect,
}: {
  stage: IssueStage;
  icon: ReactNode;
  warn?: boolean;
  title: string;
  count: number;
  lines: (string | null)[];
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      aria-pressed={selected}
      data-stage={stage}
      data-count={count}
      onClick={onSelect}
      className="flex min-w-0 items-center gap-3 rounded-lg border border-dashed border-edge-strong px-3 py-2.5 text-left hover:bg-raised/55 aria-pressed:border-solid aria-pressed:bg-raised"
    >
      <span
        aria-hidden="true"
        className={`flex size-8 shrink-0 items-center justify-center rounded-lg border text-base ${
          warn ? "border-warn/40 bg-warn/10 text-warn" : "border-neutral-border bg-neutral-surface text-neutral-fg"
        }`}
      >
        {icon}
      </span>
      <span className="flex min-w-0 flex-1 flex-col">
        <span className="text-[13px] font-medium text-fg">
          <b className="mr-1.5 font-semibold">{count}</b>
          {title}
        </span>
        {lines.map(
          (line, i) =>
            line && (
              <span key={i} title={line} className="truncate text-xs text-faint">
                {line}
              </span>
            ),
        )}
      </span>
    </button>
  );
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

export function IssueLine({
  model,
  selected,
  onSelect,
  demo,
  scope,
  scopeToggle,
}: {
  model: IssueLineModel;
  selected: IssueStage | null;
  onSelect: (stage: IssueStage | null) => void;
  demo: boolean;
  // "all" is the admin's every-user scope: the note says the counts cover every user.
  scope: IssueWorkScope;
  // The admin's You / All users switch (#4052); absent for a non-admin, who gets no switch.
  scopeToggle?: { value: IssueWorkScope; onChange: (scope: IssueWorkScope) => void; allDisabled: boolean };
}) {
  const titleId = useId();
  const { entries, captions } = model;
  // Pressing the pressed stage again lets go of it.
  const toggle = (stage: IssueStage) => () => onSelect(selected === stage ? null : stage);
  const note = `${plural(model.onLine, "run", "runs")} on the line, ${entries.other.length} other${scope === "all" ? ", all users" : ""}`;
  const prEntries: LineEntry[] = model.prs.map((run) => ({ run, own: true }));
  // Card (components/ui) has no attribute passthrough, so this repeats its classes to carry
  // data-live, which only the Shadow theme styles.
  return (
    <section aria-labelledby={titleId} data-live="" className="rounded-xl border border-edge bg-surface p-5">
      {/* Title, note, then the admin's scope switch last, at the far right. Below md the note
          drops to its own full-width row so the switch stays on the title row. */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <h2 id={titleId} className="text-xs font-semibold uppercase tracking-wider text-faint">
          Issue work
        </h2>
        <span
          className={`min-w-0 text-xs text-faint md:flex-1 md:text-right ${
            scopeToggle ? "order-last basis-full md:order-none md:basis-auto" : "ml-auto"
          } md:ml-0`}
        >
          {note}
        </span>
        {scopeToggle && (
          <SegmentedToggle
            label="Issue work scope"
            options={[
              { value: "you", label: "You" },
              { value: "all", label: "All users", disabled: scopeToggle.allDisabled },
            ]}
            value={scopeToggle.value}
            onChange={scopeToggle.onChange}
            className="ml-auto md:ml-0"
          />
        )}
      </div>
      <div role="group" aria-label="Filter runs by stage" className="relative mt-2 grid grid-cols-1 md:grid-cols-5">
        <span aria-hidden="true" className="absolute left-[26px] right-[calc(20%-26px)] top-[25px] hidden h-0.5 bg-edge-strong md:block" />
        {STATIONS.map((station, index) => (
          <StationButton
            key={station.stage}
            station={station}
            index={index}
            count={station.stage === "pr" ? model.prs.length : entries[station.stage].length}
            caption={captions[station.stage]}
            note={station.stage === "pr" ? captions.prNote : undefined}
            entries={station.stage === "pr" ? prEntries : entries[station.stage]}
            selected={selected === station.stage}
            onSelect={toggle(station.stage)}
            demo={demo}
          />
        ))}
      </div>
      <div className="mt-2.5 grid grid-cols-1 gap-2.5 md:grid-cols-3">
        <BayButton
          stage="asking"
          icon={<ChatIcon />}
          warn
          title={captions.askingTitle}
          count={entries.asking.length}
          lines={[captions.asking]}
          selected={selected === "asking"}
          onSelect={toggle("asking")}
        />
        <BayButton
          stage="parked"
          icon={<ClockIcon />}
          title="Parked"
          count={entries.parked.length}
          lines={[captions.parked, captions.parkedReason]}
          selected={selected === "parked"}
          onSelect={toggle("parked")}
        />
        <BayButton
          stage="other"
          icon={<ActivityIcon />}
          title="Other work"
          count={entries.other.length}
          lines={[captions.other]}
          selected={selected === "other"}
          onSelect={toggle("other")}
        />
      </div>
    </section>
  );
}
