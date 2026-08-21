// The Repos-page per-repo "Setup" chip and its popover (PRD #361 M4). A repo has
// four OPTIONAL capabilities, every one deliberately OFF by default for safety
// (repo skills, repo instructions, tool profile, Docker workers). The chip is a
// calm, neutral at-a-glance readiness view that NEVER nags: it stays muted while a
// repo sits happily on its defaults, reads a quiet "Ready" only when all four are
// on, and escalates to an INFO tone (sky, never a red/amber warning) in the one
// case that is genuinely actionable — a run on this repo is actually blocked
// because the repo is not on the Docker allowlist and no worker can run it.
//
// The "warning on anything not maxed out" design was explicitly REJECTED (PRD
// Decision log): three of the four capabilities are security opt-ins, so a warning
// on every fresh repo produces alarm fatigue and pressures users to weaken the
// default posture. The copy here frames every off capability as optional, off on
// purpose — never "missing"/"incomplete".
//
// PRESENTATIONAL ON PURPOSE — it takes the `repo` as a prop and fetches nothing,
// mirroring BuildInfoPopover. The accessibility mechanics are modeled CLOSELY on
// BuildInfoPopover (its comments are the authority): hover + focus-within on the
// HOST wrapper (not the button) so the panel survives the pointer crossing onto it
// and stays open while focus is anywhere inside; click OPENS (never toggles) so a
// click on an already-hovered chip can't close it under the pointer; Escape
// dismisses via a document keydown listener attached only while open; the panel
// STAYS MOUNTED when closed (opacity-0 + pointer-events-none, not `hidden`) so its
// full text remains in the trigger's accessible description; associated via
// aria-describedby + role="tooltip"; NO native `title` tooltip anywhere.

import { useEffect, useId, useState } from "react";
import type { Repo } from "../lib/api";
import { Badge, type BadgeTone, cx } from "./ui";

// The five booleans the chip reasons about — a subset of Repo, so the pure tone
// helper is exercisable in a test without hand-building a whole Repo.
export interface RepoSetupFlags {
  repo_skills_enabled: boolean;
  repo_claudemd_enabled: boolean;
  repo_devbox_opt_in: boolean;
  docker_allowlisted: boolean;
  docker_blocked: boolean;
}

// setupTone is the tone-selection rule, exported so a test asserts it directly
// rather than reverse-engineering it from rendered classes. Priority order, and it
// NEVER returns a red/amber tone:
//   1. docker_blocked → INFO (sky). The chip escalates because a run is genuinely
//      stuck — this is the only escalation, and it keys on a real block, not a
//      config gap.
//   2. all four capabilities on → quiet OK "Ready".
//   3. else → NEUTRAL/muted "Setup" (the default; a repo on its safe defaults).
export function setupTone(repo: RepoSetupFlags): { tone: BadgeTone; label: string } {
  if (repo.docker_blocked) return { tone: "info", label: "Setup" };
  if (
    repo.repo_skills_enabled &&
    repo.repo_claudemd_enabled &&
    repo.repo_devbox_opt_in &&
    repo.docker_allowlisted
  ) {
    return { tone: "ok", label: "Ready" };
  }
  return { tone: "neutral", label: "Setup" };
}

// One capability row's static copy. `where` names the surface that flips it; the
// Docker row is admin-only (tagged), the other three are self-serve on this page.
interface Capability {
  key: keyof Pick<
    RepoSetupFlags,
    "repo_skills_enabled" | "repo_claudemd_enabled" | "repo_devbox_opt_in" | "docker_allowlisted"
  >;
  name: string;
  explainer: string;
  where: string;
  admin?: boolean;
}

const CAPABILITIES: Capability[] = [
  {
    key: "repo_skills_enabled",
    name: "Repo skills",
    explainer: "Load skills from the repo’s own .claude/skills/.",
    where: "Trusted repo settings",
  },
  {
    key: "repo_claudemd_enabled",
    name: "Repo instructions",
    explainer: "Let the lead read the repo’s root CLAUDE.md as reference context.",
    where: "Trusted repo settings",
  },
  {
    key: "repo_devbox_opt_in",
    name: "Tool profile",
    explainer: "Install this repo’s own devbox packages before each run.",
    where: "Tools",
  },
  {
    key: "docker_allowlisted",
    name: "Docker workers",
    explainer: "Let Docker workers run this repo.",
    where: "Admin → Docker workers",
    admin: true,
  },
];

export function RepoSetupChip({ repo }: { repo: Repo }) {
  const [open, setOpen] = useState(false);
  // Instance-scoped: many rows mount this at once, so a hardcoded id would make
  // aria-describedby ambiguous across the table.
  const popId = useId();

  // Escape dismisses however it was opened (hover OR focus), matching the APG
  // tooltip pattern. Listener attached only while open — a closed chip costs
  // nothing, and there is one per repo row.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open]);

  const { tone, label } = setupTone(repo);

  return (
    <div
      className="relative inline-flex"
      // Hover/focus on the HOST, not the button, so the panel does not vanish the
      // instant the pointer crosses from the chip down onto the panel below it, and
      // stays open while focus is anywhere inside (Escape/no-interactive-controls
      // today, but the pattern is the proven one and cheap to keep).
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOpen(false);
      }}
    >
      <button
        type="button"
        // aria-describedby, not aria-label: the chip's own name is its label
        // ("Setup"/"Ready"); the popover DESCRIBES the capability breakdown. The
        // panel stays MOUNTED when closed (see its opacity classes) so this
        // description reaches a screen-reader user who never hovers or focuses.
        aria-describedby={popId}
        // Click always OPENS (never toggles), so a desktop click landing on an
        // already-hovered chip cannot close it under the pointer. Escape (document
        // listener) closes it; hover-out / focus-out on the host closes it.
        onClick={() => setOpen(true)}
        // The visual pill reuses the shared Badge tone tokens (BADGE_TONES via the
        // Badge component) so the chip's neutral/ok/info surfaces match every other
        // badge; the button is a bare focusable wrapper carrying the a11y wiring and
        // the keyboard focus ring (global :focus-visible, index.css).
        className={cx("inline-flex rounded-md", "cursor-help focus-visible:text-fg")}
      >
        <Badge tone={tone} dot>
          {label}
        </Badge>
      </button>
      <div
        id={popId}
        role="tooltip"
        data-open={open ? "true" : "false"}
        className={cx(
          // Anchored BELOW the trigger, left-aligned. Unlike the sidebar footer
          // popover there IS space below a table row, and the Repos table sits in an
          // overflow-x-auto container only (no overflow-y clip), so a panel dropping
          // below the row is not clipped.
          "absolute left-0 top-full z-20 mt-2 w-72 rounded-lg border border-edge bg-raised p-3 text-left shadow-2xl",
          "transition-opacity duration-150 motion-reduce:transition-none",
          open ? "opacity-100" : "pointer-events-none opacity-0",
        )}
      >
        {/* Hover bridge: a transparent descendant filling the mt-2 gap ABOVE the
            panel, so moving the pointer from the chip down into the panel never
            lands on non-host geometry and the host's onMouseLeave never fires
            mid-transit. Inside the panel, so it inherits pointer-events-none while
            the panel is closed — no hover target when shut. */}
        <div aria-hidden="true" data-hover-bridge className="absolute inset-x-0 bottom-full h-[9px]" />
        <div className="text-xs font-semibold text-fg">Setup</div>
        <p className="mt-0.5 text-[11px] text-faint">
          Four optional capabilities, each off by default for safety.
        </p>
        <ul className="mt-2 space-y-2">
          {CAPABILITIES.map((cap) => {
            const on = repo[cap.key];
            // The only escalation copy: when Docker is off AND a run is actually
            // blocked, the Docker row explains the stuck run and points to where to
            // fix it. Every other off capability stays framed as optional.
            const blockedNote = cap.key === "docker_allowlisted" && !on && repo.docker_blocked;
            return (
              <li key={cap.key} className="flex items-start gap-2">
                {/* On = filled ok dot + "on"; off = a muted "off". NEVER a red X —
                    off is a valid, safe resting state, not an error. The label text
                    is lowercase and rendered small-caps via `uppercase`: it reads
                    "ON"/"OFF" but keeps the word "Off" out of the page's exact text,
                    so the Repos page's row-scoped getByText("Off") (the Trusted cell
                    badge) stays unambiguous with this always-mounted panel present. */}
                {on ? (
                  <span className="mt-0.5 inline-flex w-8 shrink-0 items-center gap-1 text-[10px] font-semibold uppercase tracking-wide text-ok">
                    <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-current" />
                    on
                  </span>
                ) : (
                  <span className="mt-0.5 w-8 shrink-0 text-[10px] font-semibold uppercase tracking-wide text-faint">
                    off
                  </span>
                )}
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-1.5">
                    <span className="text-xs font-medium text-fg">{cap.name}</span>
                    {cap.admin && (
                      <span className="rounded border border-edge px-1 py-px text-[9px] font-medium uppercase tracking-wide text-faint">
                        admin
                      </span>
                    )}
                  </div>
                  <p className="text-[11px] text-muted">{cap.explainer}</p>
                  <p className="text-[11px] text-faint">
                    Set in <span className="text-muted">{cap.where}</span>
                  </p>
                  {blockedNote && (
                    <p className="mt-0.5 text-[11px] text-info">
                      A queued run is waiting for a worker. Add this repo under Admin
                      {" → "}Docker workers to let a Docker worker run it.
                    </p>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
        <p className="mt-2 border-t border-edge pt-2 text-[11px] text-faint">
          Defaults are off on purpose; each one widens what an agent may do.
        </p>
      </div>
    </div>
  );
}
