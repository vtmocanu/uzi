// The "Get the factory running" checklist, shared by both Overview pages. The steps are
// derived from the same preconditions the board's start-run gate checks
// (lib/runStream.ts startRunGate); the caller decides when to show it (setupSteps in
// lib/overview.ts says when every step is done).

import { Link } from "react-router-dom";
import { Button, Card, cx, SectionTitle } from "./ui";
import { CheckIcon, ChevronRightIcon } from "./icons";

function Step({
  done,
  index,
  title,
  hint,
  to,
  cta,
}: {
  done: boolean;
  index: number;
  title: string;
  hint: string;
  to: string;
  cta: string;
}) {
  return (
    <li className="flex items-center gap-3 py-2.5">
      <span
        className={cx(
          "flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-xs",
          done ? "bg-ok/15 text-ok" : "bg-raised text-faint",
        )}
      >
        {done ? <CheckIcon /> : index}
      </span>
      <div className="min-w-0 flex-1">
        <p className={cx("text-sm font-medium", done ? "text-muted line-through" : "text-fg")}>
          {title}
        </p>
        {!done && <p className="text-xs text-faint">{hint}</p>}
      </div>
      {!done && (
        <Link to={to}>
          <Button variant="secondary" size="sm">
            {cta} <ChevronRightIcon />
          </Button>
        </Link>
      )}
    </li>
  );
}

export function OnboardingChecklist({
  hasToken,
  hasForge,
  reposEnabled,
  workerJoined,
}: {
  hasToken: boolean;
  hasForge: boolean;
  reposEnabled: number;
  workerJoined: boolean;
}) {
  const steps = [hasToken, hasForge, reposEnabled > 0, workerJoined];
  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle>Get the factory running</SectionTitle>
        <span className="text-xs text-faint">{steps.filter(Boolean).length}/4 done</span>
      </div>
      <ol className="mt-2 divide-y divide-edge">
        <Step
          done={hasToken}
          index={1}
          title="Add your Anthropic token"
          hint="Agents run on your own credentials; the token is encrypted at rest."
          to="/settings"
          cta="Settings"
        />
        <Step
          done={hasForge}
          index={2}
          title="Connect your bot"
          hint="A bot PAT lets uzi see and move your issues."
          to="/settings/forge"
          cta="Forge"
        />
        <Step
          done={reposEnabled > 0}
          index={3}
          title="Enable a repo"
          hint="Enabled repos get a kanban board of their uzi-labeled issues."
          to="/repos"
          cta="Repos"
        />
        <Step
          done={workerJoined}
          index={4}
          title="Bring a worker online"
          hint="Generate a join token and start the uzi-agent container with it."
          to="/workers"
          cta="Workers"
        />
      </ol>
    </Card>
  );
}
