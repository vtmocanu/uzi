import { cx } from "./ui";
import { CheckIcon } from "./icons";
import { type MrChipState, mrChipTitle } from "../lib/runBadge";

// MrChip is the board card's merge-request pill (PRD #12), extended in PRD #33 to
// show the run's derived MR-state variant (mrChipState):
//   open   → exactly as before — an ok-toned "!N" pill (success criterion 2);
//   merged → ok-toned, a check mark and a "merged" label;
//   closed → muted, the number struck through and a "closed" label.
// Rendered as a link when href is an https MR URL, else a plain span. The state is a
// best-effort hint (Decision 1): the title scopes it to "as of last sync".
export function MrChip({ mrIid, mrState, href }: { mrIid: number; mrState: MrChipState; href: string | null }) {
  const closed = mrState === "closed";
  const cls = cx(
    "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-medium",
    closed ? "border-edge bg-raised/60 text-faint" : "border-ok/40 bg-ok/10 text-ok",
    href && (closed ? "transition-colors hover:bg-raised" : "transition-colors hover:bg-ok/20"),
  );
  const inner = (
    <>
      {mrState === "merged" && <CheckIcon className="h-3 w-3" />}
      <span className={closed ? "line-through" : undefined}>!{mrIid}</span>
      {mrState !== "open" && <span>{mrState}</span>}
    </>
  );
  const title = mrChipTitle(mrState);
  return href ? (
    <a href={href} target="_blank" rel="noreferrer" draggable={false} title={title} className={cls}>
      {inner}
    </a>
  ) : (
    <span title={title} className={cls}>
      {inner}
    </span>
  );
}
