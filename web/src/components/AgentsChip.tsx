// The new Overview's configured-agent count and source drill-down (#4367). It shares
// the same one-popover focus and dismissal behavior as the other header chips.

import { Link } from "react-router-dom";
import type { AgentSourceChipModel } from "../lib/agentSourceChip";
import { ChipPopoverPanel } from "./ChipPopoverPanel";
import { BotIcon } from "./icons";
import { useChipPopover } from "./useChipPopover";

const INFO_TEXT =
  "The agents uzi can use: shared builtin and global templates, plus your own private templates. Grouped by where each definition comes from.";

const SUFFIX_TONE = {
  danger: "text-danger",
  warn: "text-warn",
  muted: "text-muted",
} as const;

export function AgentsChip({ model }: { model: AgentSourceChipModel }) {
  const { open, setOpen, hostRef, panelRef, triggerRef, panelId, close } = useChipPopover();
  const { total, groups, suffix, sourceMeta, sourceAppliedSha, sourceNotice, reviewInSettings } = model;
  const sourceTitle = [sourceMeta, sourceAppliedSha && `(${sourceAppliedSha})`].filter(Boolean).join(", ");
  const displayGroups = groups.some((group) => group.key === "source")
    ? groups
    : sourceMeta || sourceAppliedSha || sourceNotice
      ? [{ key: "source" as const, label: "From your agent source", agents: [] }, ...groups]
      : groups;

  return (
    <div ref={hostRef} className="relative inline-block min-w-0 max-w-full">
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        aria-label={`Agents: ${total} configured${suffix ? `, ${suffix.text}` : ""}`}
        onClick={() => setOpen((v) => !v)}
        className="inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised"
      >
        <span aria-hidden="true" className="inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-[5px] border border-edge bg-raised text-muted ring-1 ring-surface">
          <BotIcon className="h-3.5 w-3.5" />
        </span>
        <span className="min-w-0 truncate">
          <b className="font-semibold">{total} {total === 1 ? "agent" : "agents"}</b>
          {suffix && <span className={SUFFIX_TONE[suffix.tone]}> · {suffix.text}</span>}
        </span>
      </button>
      {open && (
        <ChipPopoverPanel id={panelId} panelRef={panelRef} label="Agents configured">
          <div className="space-y-3 p-3">
            <div className="flex items-start justify-between gap-2">
              <h3 className="text-sm font-semibold text-fg">{total} {total === 1 ? "agent" : "agents"} configured</h3>
              <span
                className="inline-flex h-4 w-4 shrink-0 cursor-help items-center justify-center rounded-full border border-edge text-[10px] font-semibold leading-none text-faint"
                title={INFO_TEXT}
                aria-hidden="true"
              >i</span>
              <span className="sr-only">{INFO_TEXT}</span>
            </div>

            {displayGroups.map((group) => (
              <div key={group.key}>
                <p className="mb-1 text-[11px] font-medium uppercase tracking-wide text-faint">{group.label}</p>
                {group.key === "source" && sourceTitle && (
                  <p className="text-xs text-muted" title={sourceTitle}>
                    {sourceMeta && <span className="break-all">{sourceMeta}</span>}
                    {sourceAppliedSha && <span className="whitespace-nowrap">{sourceMeta ? ", " : ""}({sourceAppliedSha})</span>}
                  </p>
                )}
                {group.key === "source" && sourceNotice && <p className={`mt-1 text-xs ${suffix ? SUFFIX_TONE[suffix.tone] : "text-muted"}`}>{sourceNotice}</p>}
                {group.agents.length > 0 && (
                  <ul className="mt-1 space-y-1.5">
                    {group.agents.map((agent, index) => (
                      <li key={`${group.key}-${index}`} className="flex min-w-0 flex-wrap items-baseline justify-between gap-x-2 text-xs">
                        <span className="min-w-0 break-all text-fg">{agent.name}</span>
                        {agent.detail && <span className="text-muted">{agent.detail}</span>}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            ))}
          </div>
          <div className="sticky bottom-0 flex items-center justify-end border-t border-edge bg-raised px-3 py-2 text-xs">
            <Link
              to={reviewInSettings ? "/admin/settings#agent-source" : "/agents"}
              onClick={() => close(false)}
              className="font-medium text-brand hover:text-brand-hover"
            >
              {reviewInSettings ? "Review in Settings" : "Manage agents"}
            </Link>
          </div>
        </ChipPopoverPanel>
      )}
    </div>
  );
}
