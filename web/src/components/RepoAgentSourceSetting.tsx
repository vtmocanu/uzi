import { useId } from "react";
import type { AgentSource } from "../lib/api";
import { Select } from "./ui";

export const AGENT_SOURCE_DEFAULT_LABELS: Record<AgentSource | "unset", string> = {
  own: "uzi templates",
  repo: "repo agents",
  unset: "not set (today’s behaviour)",
};

export const REPO_AGENTS_PLANNING_WARNING =
  "Repo agents can run shell commands while planning, before you review the plan.";

export function RepoAgentSourceSetting({
  value,
  disabled,
  onChange,
}: {
  value: AgentSource | null | undefined;
  disabled?: boolean;
  onChange: (next: AgentSource | null) => void;
}) {
  const id = useId();
  const current = value ?? "";
  return (
    <div className="flex items-start gap-3 rounded-md border border-edge bg-raised p-3">
      <div className="min-w-0 flex-1 space-y-2">
        <label htmlFor={id} className="block text-sm font-semibold text-fg">
          Default agents for my runs on this repo
        </label>
        <Select
          id={id}
          value={current}
          disabled={disabled}
          onChange={(e) => {
            const next = e.target.value;
            onChange(next === "own" || next === "repo" ? next : null);
          }}
        >
          <option value="own">{AGENT_SOURCE_DEFAULT_LABELS.own}</option>
          <option value="repo">{AGENT_SOURCE_DEFAULT_LABELS.repo}</option>
          <option value="">{AGENT_SOURCE_DEFAULT_LABELS.unset}</option>
        </Select>
        {current === "repo" && <p className="text-sm text-warn">{REPO_AGENTS_PLANNING_WARNING}</p>}
      </div>
    </div>
  );
}
