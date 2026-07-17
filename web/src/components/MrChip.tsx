import { cx } from "./ui";
import { CheckIcon } from "./icons";
import { type MrChipState, mrChipSuffix, mrChipTitle } from "../lib/runBadge";
import { mrRefSymbol } from "../lib/forgeNoun";

type MrChipVariant = "pill" | "inline";
// openTone is the colour of the OPEN state only: the issue-history link keeps brand,
// everywhere else open is ok-toned. merged is always ok-toned and closed is always
// muted, regardless of openTone.
type OpenTone = "ok" | "brand";

// MrChip renders a run's merge/pull-request chip from its derived MR-state
// (mrChipState), single-sourcing the tone / suffix / title / strike / accessible-name
// logic across every surface (PRD #33), so the call sites can never drift again. The
// reference sigil and hover-title noun are per-forge (forgeType, PRD #65 D2): "!42"
// on GitLab, "#42" on Forgejo. Two layouts:
//   "pill"   — the board card's bordered box (a check-marked "!N merged" etc.).
//   "inline" — a text chip in a run's meta line (dashboard / runs list / issue history).
// State tone: merged is ok-toned (green) EVERYWHERE; closed is muted (text-muted, the
// AA-contrast token) with the number struck through; open keeps the surface's base
// tone via openTone (brand for the issue-history link, ok elsewhere). The state word
// ("merged"/"closed") is rendered INSIDE the link/pill, so a screen reader hears
// "!42 merged", not a bare "!42". href makes the chip a link to the request; a null
// href is plain text (e.g. a dashboard row that already links to the run view). label
// is an optional forge-aware prefix ("MR "/"PR ") the meta surfaces put before the
// number.
export function MrChip({
  mrIid,
  mrState,
  href,
  forgeType,
  variant = "pill",
  label = "",
  openTone = "ok",
  className,
}: {
  mrIid: number;
  mrState: MrChipState;
  href: string | null;
  // The run/card's forge, so the chip picks the reference sigil ("!42" on GitLab,
  // "#42" on Forgejo) and the per-forge noun in its hover title (PRD #65 D2).
  forgeType: string;
  variant?: MrChipVariant;
  label?: string;
  openTone?: OpenTone;
  className?: string;
}) {
  const closed = mrState === "closed";
  const merged = mrState === "merged";
  const title = mrChipTitle(mrState, forgeType);

  // The accessible unit: prefix + number (struck when closed) + state word, all in one
  // element so the state is part of the link/chip's accessible name.
  const body = (
    <>
      {variant === "pill" && merged && <CheckIcon className="h-3 w-3" aria-hidden />}
      {label}
      <span className={closed ? "line-through" : undefined}>
        {mrRefSymbol(forgeType)}
        {mrIid}
      </span>
      {mrState !== "open" && <span>{mrChipSuffix(mrState)}</span>}
    </>
  );

  if (variant === "pill") {
    const cls = cx(
      "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-medium",
      closed ? "border-edge bg-raised/60 text-muted" : "border-ok/40 bg-ok/10 text-ok",
      href && (closed ? "transition-colors hover:bg-raised" : "transition-colors hover:bg-ok/20"),
      className,
    );
    return href ? (
      <a href={href} target="_blank" rel="noreferrer" draggable={false} title={title} className={cls}>
        {body}
      </a>
    ) : (
      <span title={title} className={cls}>
        {body}
      </span>
    );
  }

  // inline: colour only (no border). merged → ok, closed → muted, open → openTone.
  const color = closed ? "text-muted" : merged ? "text-ok" : openTone === "brand" ? "text-brand" : "text-ok";
  const hover = href
    ? closed
      ? "hover:text-fg"
      : merged
        ? "hover:text-ok"
        : openTone === "brand"
          ? "hover:text-brand-hover"
          : "hover:text-ok"
    : undefined;
  const cls = cx(color, hover, className);
  return href ? (
    <a href={href} target="_blank" rel="noreferrer" title={title} className={cls}>
      {body}
    </a>
  ) : (
    <span title={title} className={cls}>
      {body}
    </span>
  );
}
