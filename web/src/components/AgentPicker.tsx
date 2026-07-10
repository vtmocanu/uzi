// The plan-gate agent picker (PRD #37 M4). Mirrors prds/mockups/37-agent-picker-
// mock.html: two source radio cards (repo agents / my templates), per-agent
// exclusion chips, a pinned `lead` summary line, and a live selection the caller
// turns into the approve-button label. States A (repo detected → repo default) and
// B (none detected → repo card inert, own default).
//
// SECURITY (PRD #37 F4): repo agent names + descriptions are REPO-SUPPLIED,
// untrusted text. They render as plain JSX text (React escapes them) and, for the
// description, only into a `title` attribute — NEVER through <Markdown>. An
// attacker-authored `http://…` in a description is therefore inert: it can never
// become a clickable link inside the approval panel.

import { useEffect, useMemo, useState } from "react";
import type { AgentSelectionInput, AgentSource, RepoAgent } from "../lib/api";
import { cx } from "./ui";

/** One selectable template on the "My agent templates" card. `custom` marks a
 *  user-scope template (badged, matching the mock's "custom" tag). */
export interface OwnTemplate {
  name: string;
  description: string;
  custom: boolean;
}

const EMPTY: ReadonlySet<string> = new Set();

/** The number of agents that survive the current exclusions, and the human label
 *  the approve button and summary line use. */
export function selectionLabel(source: AgentSource, activeCount: number): string {
  return source === "repo"
    ? `${activeCount} repo agent${activeCount === 1 ? "" : "s"}`
    : `${activeCount} of your templates`;
}

export function AgentPicker({
  repoAgents,
  ownTemplates,
  onChange,
}: {
  repoAgents: RepoAgent[];
  ownTemplates: OwnTemplate[];
  /** Called with the resolved selection on mount and on every change. MUST be
   *  stable (wrap in useCallback) — it drives an effect. */
  onChange: (selection: AgentSelectionInput) => void;
}) {
  const repoDetected = repoAgents.length > 0;
  const defaultSource: AgentSource = repoDetected ? "repo" : "own";
  const [source, setSource] = useState<AgentSource>(defaultSource);
  // Exclusions are tracked per source so switching cards preserves each card's
  // choices (the mock's per-card chips).
  const [excludedRepo, setExcludedRepo] = useState<ReadonlySet<string>>(EMPTY);
  const [excludedOwn, setExcludedOwn] = useState<ReadonlySet<string>>(EMPTY);

  const repoNames = useMemo(() => repoAgents.map((a) => a.name), [repoAgents]);
  const ownNames = useMemo(() => ownTemplates.map((t) => t.name), [ownTemplates]);

  const active = source === "repo" ? excludedRepo : excludedOwn;
  const roster = source === "repo" ? repoNames : ownNames;
  const activeCount = roster.length - active.size;

  // Stable, sorted exclusion list — recomputed only when the active set changes.
  const exclusions = useMemo(() => [...active].sort(), [active]);
  useEffect(() => {
    onChange({ source, exclusions });
  }, [source, exclusions, onChange]);

  /** Toggle a chip. Clicking a chip on the OTHER card switches to that source
   *  first (mock behavior). Never allows the roster to drop to zero — excluding
   *  the last surviving agent is a no-op. */
  const toggle = (src: AgentSource, name: string) => {
    if (src === "repo" && !repoDetected) return; // inert card
    const cur = src === "repo" ? excludedRepo : excludedOwn;
    const srcRoster = src === "repo" ? repoNames : ownNames;
    const next = new Set(cur);
    if (next.has(name)) {
      next.delete(name);
    } else {
      if (srcRoster.length - next.size <= 1) return; // would leave zero — refuse
      next.add(name);
    }
    if (src === "repo") setExcludedRepo(next);
    else setExcludedOwn(next);
    if (src !== source) setSource(src);
  };

  return (
    <section className="flex flex-col gap-2.5" aria-label="Agents for this run">
      <header className="flex flex-wrap items-baseline justify-between gap-3">
        <h3 className="text-[13px] font-semibold text-fg">Agents for this run</h3>
        <span className="text-xs text-faint">Detected during clone, before planning</span>
      </header>

      <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
        <SourceCard
          kind="repo"
          selected={source === "repo"}
          isDefault={defaultSource === "repo"}
          disabled={!repoDetected}
          detectedCount={repoDetected ? repoAgents.length : undefined}
          title="Repo agents"
          description={
            repoDetected ? (
              <>
                Found in the repo's <code className="font-mono text-[11px] text-fg">.claude/agents/</code>. The project
                team defined these for this codebase.
              </>
            ) : (
              <>
                This repo has no <code className="font-mono text-[11px] text-fg">.claude/agents/</code> directory.
                Nothing to load.
              </>
            )
          }
          onSelect={() => repoDetected && setSource("repo")}
        >
          {repoDetected && (
            <>
              <ChipRow>
                {repoAgents.map((a) => (
                  <AgentChip
                    key={a.name}
                    name={a.name}
                    title={a.description}
                    off={excludedRepo.has(a.name)}
                    onToggle={() => toggle("repo", a.name)}
                  />
                ))}
              </ChipRow>
              <span className="text-[11px] text-faint">Click a chip to exclude it from the run.</span>
            </>
          )}
        </SourceCard>

        <SourceCard
          kind="own"
          selected={source === "own"}
          isDefault={defaultSource === "own"}
          disabled={false}
          title="My agent templates"
          description={<>Your uzi templates — builtins plus any you customized under Settings → Agents.</>}
          onSelect={() => setSource("own")}
        >
          <ChipRow>
            {ownTemplates.map((t) => (
              <AgentChip
                key={t.name}
                name={t.name}
                title={t.description}
                custom={t.custom}
                off={excludedOwn.has(t.name)}
                onToggle={() => toggle("own", t.name)}
              />
            ))}
          </ChipRow>
          <span className="text-[11px] text-faint">Click a chip to include or exclude it.</span>
        </SourceCard>
      </div>

      <div className="flex flex-wrap items-center gap-2 border-t border-edge pt-3 text-xs text-muted">
        <span className="inline-flex items-center gap-1.5 rounded-full border border-brand/45 bg-brand/[0.08] px-2.5 py-[3px] font-mono text-[11.5px] text-fg">
          <span aria-hidden className="text-[10px] text-brand">
            ●
          </span>
          lead
        </span>
        <span>orchestrates every run (uzi builtin, not selectable) · delegating to</span>
        <b className="font-semibold text-fg">{selectionLabel(source, activeCount)}</b>
      </div>
    </section>
  );
}

function SourceCard({
  kind,
  selected,
  isDefault,
  disabled,
  detectedCount,
  title,
  description,
  onSelect,
  children,
}: {
  kind: AgentSource;
  selected: boolean;
  isDefault: boolean;
  disabled: boolean;
  detectedCount?: number;
  title: string;
  description: React.ReactNode;
  onSelect: () => void;
  children?: React.ReactNode;
}) {
  return (
    <label
      className={cx(
        "relative flex cursor-pointer flex-col gap-2.5 rounded-lg border p-3.5 transition-colors",
        selected ? "border-brand/70 bg-brand/5" : "border-edge bg-surface hover:border-edge-strong",
        disabled && "cursor-not-allowed opacity-55 hover:border-edge",
      )}
      title={disabled ? "No .claude/agents/ directory in this repo" : undefined}
    >
      <input
        type="radio"
        name="agent-source"
        value={kind}
        checked={selected}
        disabled={disabled}
        onChange={onSelect}
        className="pointer-events-none absolute opacity-0"
      />
      <div className="flex items-center gap-2.5">
        <span
          aria-hidden
          className={cx(
            "grid size-4 flex-none place-items-center rounded-full border-[1.5px]",
            selected ? "border-brand" : "border-edge-strong",
          )}
        >
          {selected && <span className="size-2 rounded-full bg-brand" />}
        </span>
        <span className="flex flex-wrap items-center gap-2 text-[13px] font-semibold">
          {title}
          {detectedCount !== undefined && <Pill tone="detected">detected · {detectedCount}</Pill>}
          {disabled && <Pill tone="none">none detected</Pill>}
          {isDefault && !disabled && <Pill tone="default">default</Pill>}
        </span>
      </div>
      <p className="m-0 text-xs text-muted">{description}</p>
      {children}
    </label>
  );
}

function Pill({ tone, children }: { tone: "detected" | "default" | "none"; children: React.ReactNode }) {
  const tones = {
    detected: "text-ok border-ok/40 bg-ok/10",
    default: "text-brand border-brand/40 bg-brand/10",
    none: "text-faint border-edge bg-raised",
  } as const;
  return (
    <span
      className={cx(
        "rounded-full border px-2 py-[1.5px] text-[10px] font-bold uppercase tracking-wider",
        tones[tone],
      )}
    >
      {children}
    </span>
  );
}

function ChipRow({ children }: { children: React.ReactNode }) {
  return <div className="flex flex-wrap gap-1.5">{children}</div>;
}

// AgentChip renders one selectable agent. `name` is a validated kebab identifier;
// `title` is the UNTRUSTED repo/template description, placed only in the title
// attribute (a hover tooltip) — never linkified, never markdown.
function AgentChip({
  name,
  title,
  custom,
  off,
  onToggle,
}: {
  name: string;
  title: string;
  custom?: boolean;
  off: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      title={title}
      aria-pressed={!off}
      onClick={(e) => {
        e.preventDefault();
        onToggle();
      }}
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-[3px] font-mono text-[11.5px] transition-colors",
        off
          ? "border-dashed border-edge-strong bg-raised text-fg opacity-45"
          : "border-edge-strong bg-raised text-fg hover:border-brand/60",
      )}
    >
      <span aria-hidden className={cx("text-[10px] text-ok", off && "invisible")}>
        ●
      </span>
      {name}
      {custom && <span className="text-[10px] text-info">custom</span>}
    </button>
  );
}
