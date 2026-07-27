import type { ReactNode } from "react";
import type { AgentUsage, RunUsage } from "../lib/runUsage";
import { formatTokens, formatCost } from "../lib/formatTokens";
import { formatDuration } from "./RunEvent";
import { cx } from "./ui";

// PRD #40 §1: the run view's usage surfaces — a header strip, a collapsible
// per-phase table, and a collapsible per-agent table — all derived client-side
// from the message stream (lib/runUsage.ts), so they fold in live as new result
// frames arrive (Decision 9) with no accumulator. Pre-feature runs derive
// hasUsage=false upstream, so this renders nothing rather than a fabricated 0.

const K_CLASS = "text-[10.5px] font-semibold uppercase tracking-[0.07em] text-faint";

function Stat({ label, value, cost, children }: { label: string; value: string; cost?: boolean; children?: ReactNode }) {
  return (
    <div className="bg-raised/75 px-3.5 py-2.5">
      <div className={K_CLASS}>{label}</div>
      <div className={cx("mt-0.5 font-mono text-[17px] font-semibold tabular-nums", cost && "text-brand")}>{value}</div>
      {children}
    </div>
  );
}

function Th({ children, left }: { children: ReactNode; left?: boolean }) {
  return (
    <th
      // WCAG 1.3.1: without scope, a screen reader cannot tie a data cell to its header,
      // and these tables are all numbers — a figure read without its column is worse than
      // unread. Every Th here is a column header; there are no row headers in this file.
      scope="col"
      className={cx(
        "border-b border-edge px-2.5 py-1.5 text-[10.5px] font-semibold uppercase tracking-[0.06em] text-faint",
        left ? "text-left" : "text-right",
      )}
    >
      {children}
    </th>
  );
}

// `mono` is the PRD #93 Model column: left-aligned like `left`, but a model id is a
// machine string, so it keeps the monospace face (per the approved mock).
function Td({ children, left, total, mono }: { children: ReactNode; left?: boolean; total?: boolean; mono?: boolean }) {
  return (
    <td
      className={cx(
        "px-2.5 py-1.5",
        left ? cx("text-left", mono ? "font-mono" : "font-sans") : "text-right font-mono tabular-nums",
        // Issue #152: these three are mutually EXCLUSIVE, and the non-total branch used to
        // be additive — a left cell got `text-muted` AND `text-fg`. Same specificity, so
        // stylesheet order decides it, and in the built CSS `.text-fg` precedes
        // `.text-muted` (re-measured 2026-07-27 on `npm run build`: 24360 vs 24602; the
        // issue measured 24294/24536, so the ORDER is the durable fact, not the offsets).
        // `text-muted` therefore always won and the left-cell rule was dead, silently
        // dimming the Agent, Phase and Model columns against the approved mocks. Cosmetic,
        // but a contrast regression — and the shape reappears easily: never hand cx() two
        // colour classes for one element and expect the later argument to win.
        total ? "font-semibold text-fg" : left ? "text-fg" : "text-muted",
        !total && "border-b border-edge/50",
      )}
    >
      {/* A model id is an unbounded machine string, so the Model column clips like
          the strip's own model line does. `max-w` on a bare <td> is not honored by
          table layout — the constraint has to sit on an inner block-level span. */}
      {mono ? (
        <span className="block max-w-[220px] truncate" title={typeof children === "string" ? children : undefined}>
          {children}
        </span>
      ) : (
        children
      )}
    </td>
  );
}

// The lead orchestrator gets the brand accent; every subagent gets the info accent
// (matches the mock's `.agent-chip` vs `.agent-chip.sub`).
function AgentChip({ agent }: { agent: string }) {
  const sub = agent !== "lead";
  return (
    <span className="inline-flex items-center gap-1.5">
      <span aria-hidden="true" className={cx("h-[7px] w-[7px] rounded-[2px]", sub ? "bg-info" : "bg-brand")} />
      {agent}
    </span>
  );
}

const money = (usd: number): string => (usd > 0 ? formatCost(usd) : "—");

/** The mock's `td.model.dim`: a derived/absent value, not a model id the agent ran on. */
const Dim = ({ children }: { children: ReactNode }) => <span className="text-faint">{children}</span>;

// PRD #93 Decision 4: one model → the bare string; several → the primary plus the
// count of the others ("claude-opus-4-8 +1"). Decision 6: no model → "—", never the
// strip's init model.
function agentModelCell(a: AgentUsage): ReactNode {
  if (!a.model) return <Dim>—</Dim>;
  return a.otherModels > 0 ? `${a.model} +${a.otherModels}` : a.model;
}

// The total row spans agents: the single model string when the whole run used one,
// "N models" when it used several, "—" when none was recorded.
function totalModelCell(models: string[]): ReactNode {
  if (models.length === 0) return <Dim>—</Dim>;
  return models.length === 1 ? models[0] : <Dim>{models.length} models</Dim>;
}

export function RunUsagePanel({ usage }: { usage: RunUsage }) {
  if (!usage.hasUsage) return null;
  const { total, model, phases, agents, agentTotal, agentModels } = usage;
  const cachePct = Math.round(usage.cacheHitRatio * 100);
  const tokensIn = total.fresh + total.cached;

  return (
    <div>
      <div
        className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-edge bg-edge sm:grid-cols-4"
        aria-label="Run usage totals"
      >
        <Stat label="Tokens in" value={formatTokens(tokensIn)}>
          <div className="text-[11px] text-muted">{cachePct}% from cache</div>
          <div
            className="mt-1.5 flex h-1 overflow-hidden rounded bg-edge"
            role="img"
            aria-label={`${cachePct} percent cache reads`}
          >
            <span className="h-full bg-info" style={{ width: `${cachePct}%` }} />
            <span className="h-full bg-warn/80" style={{ width: `${100 - cachePct}%` }} />
          </div>
        </Stat>
        <Stat label="Tokens out" value={formatTokens(total.out)}>
          <div className="text-[11px] text-muted">
            {total.phaseCount} phase{total.phaseCount === 1 ? "" : "s"} · {total.turns} turns
          </div>
        </Stat>
        <Stat label="Duration" value={formatDuration(total.durationMs)}>
          {model && <div className="truncate text-[11px] text-muted">{model}</div>}
        </Stat>
        <Stat label="Cost" value={money(total.costUsd)} cost>
          <div className="text-[11px] text-muted">
            {total.costUsd > 0 ? "your Anthropic token" : "subscription auth · no cost"}
          </div>
        </Stat>
      </div>

      <details className="mt-3 group" open>
        <summary className="cursor-pointer list-none text-xs text-muted marker:content-none">
          {/* <details>/<summary> already conveys expanded state natively, so an announced
              triangle is a second, contradictory reading of the same fact. Matches
              AgentChip's status dot two functions down — consistency, not a new rule. */}
          <span aria-hidden="true" className="text-faint group-open:hidden">▸ </span>
          <span aria-hidden="true" className="hidden text-faint group-open:inline">▾ </span>
          Per-phase breakdown
        </summary>
        {/* This scrolls at narrow widths (560 wide in a 301 viewport) and holds nothing
            focusable. It carries role + a name and DELIBERATELY NO `tabIndex`.
            Driven for real in Chrome 150 at 375px: Tab from the summary already lands on
            this div (Chrome focuses overflowing scrollers natively, `tabIndex` -1, no
            attribute) and ArrowRight scrolls it 0 -> 171 of max 299. So there is no 2.1.1
            failure to fix — and Chrome makes it focusable ONLY while it actually overflows,
            so an unconditional tabIndex={0} would plant a permanent empty tab stop at every
            desktop width (1280px: scrollWidth == clientWidth == 950, Tab skips it). That is
            a regression, not a fix. The real defect was the missing role/name: a keyboard
            user landed on an unlabelled generic div with no announced purpose.
            Safari/Firefox are UNTESTED here (agent-browser drives Chrome only). */}
        <div className="mt-2 overflow-x-auto" role="region" aria-label="Per-phase usage table, scrollable">
          <table aria-label="Per-phase usage" className="w-full min-w-[560px] border-collapse text-xs">
            <thead>
              <tr>
                <Th left>Phase</Th>
                <Th>Turns</Th>
                <Th>In (fresh)</Th>
                <Th>In (cached)</Th>
                <Th>Out</Th>
                <Th>Cost</Th>
              </tr>
            </thead>
            <tbody>
              {phases.map((p) => (
                <tr key={p.seq}>
                  <Td left>{p.label}</Td>
                  <Td>{p.turns}</Td>
                  <Td>{formatTokens(p.fresh)}</Td>
                  <Td>{formatTokens(p.cached)}</Td>
                  <Td>{formatTokens(p.out)}</Td>
                  <Td>{money(p.costUsd)}</Td>
                </tr>
              ))}
              <tr>
                <Td left total>Run total</Td>
                <Td total>{total.turns}</Td>
                <Td total>{formatTokens(total.fresh)}</Td>
                <Td total>{formatTokens(total.cached)}</Td>
                <Td total>{formatTokens(total.out)}</Td>
                <Td total>{money(total.costUsd)}</Td>
              </tr>
            </tbody>
          </table>
        </div>
      </details>

      {agents.length > 0 && (
        <details className="mt-3 group" open>
          <summary className="cursor-pointer list-none text-xs text-muted marker:content-none">
            <span aria-hidden="true" className="text-faint group-open:hidden">▸ </span>
            <span aria-hidden="true" className="hidden text-faint group-open:inline">▾ </span>
            Per-agent breakdown
          </summary>
          {/* See the per-phase wrapper above. The names differ because two ADJACENT
              unlabelled data tables is precisely where a screen-reader user loses which
              one they are in. */}
          <div className="mt-2 overflow-x-auto" role="region" aria-label="Per-agent usage table, scrollable">
            <table aria-label="Per-agent usage" className="w-full min-w-[600px] border-collapse text-xs">
              <thead>
                <tr>
                  <Th left>Agent</Th>
                  <Th left>Model</Th>
                  <Th>In (fresh)</Th>
                  <Th>In (cached)</Th>
                  <Th>Out</Th>
                </tr>
              </thead>
              <tbody>
                {agents.map((a) => (
                  <tr key={a.agent}>
                    <Td left>
                      <AgentChip agent={a.agent} />
                    </Td>
                    <Td left mono>{agentModelCell(a)}</Td>
                    <Td>{formatTokens(a.fresh)}</Td>
                    <Td>{formatTokens(a.cached)}</Td>
                    <Td>{formatTokens(a.out)}</Td>
                  </tr>
                ))}
                <tr>
                  <Td left total>Attributed total</Td>
                  <Td left total mono>{totalModelCell(agentModels)}</Td>
                  <Td total>{formatTokens(agentTotal.fresh)}</Td>
                  <Td total>{formatTokens(agentTotal.cached)}</Td>
                  <Td total>{formatTokens(agentTotal.out)}</Td>
                </tr>
              </tbody>
            </table>
          </div>
          <p className="mt-1.5 text-[11px] text-faint">
            Attributed from each agent's assistant messages; may not sum to the run total (tokens only — per-agent
            cost is not available).
          </p>
        </details>
      )}
    </div>
  );
}
