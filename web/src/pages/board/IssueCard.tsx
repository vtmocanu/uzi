import { Link } from "react-router-dom";
import { isHttpsUrl, preferForgeUrl, type Card as CardData } from "../../lib/api";
import type { StartRunGate } from "../../lib/runStream";
import {
  canOpenRunView,
  effectiveRunStatus,
  needsHumanAttention,
  retryHint,
  runBadge,
} from "../../lib/runBadge";
import { runDurationLabel } from "../../lib/runDuration";
import { boundedChips } from "../../lib/labelChips";
import { highlightSegments } from "../../lib/boardCards";
import { Badge, Button, cx } from "../../components/ui";
import { FixCiButton, PipelineBadge } from "../../components/PipelineBadge";
import { MrChip } from "../../components/MrChip";
import { forgePlatform } from "../../lib/forgeNoun";
import { ExternalLinkIcon } from "../../components/icons";
import { stripUnsafeChars } from "../../lib/safeText";
import { useDemoMode } from "../../lib/demoMode";
import { maskName, maskUsername } from "../../lib/demoMask";

// Highlighted renders `text`, wrapping every case-insensitive occurrence of `query` in
// a <mark> (PRD #304 M2/M5). <mark> is SEMANTIC — it carries the "this is a search hit"
// meaning to assistive tech, not color alone. The caller passes the ALREADY
// display-sanitized string (titles/labels go through stripUnsafeChars), so
// highlightSegments does no sanitizing and the segments are safe to render. An empty
// query yields a single non-hit segment, so the text renders unchanged off-search.
function Highlighted({ text, query }: { text: string; query: string }) {
  return (
    <>
      {highlightSegments(text, query).map((seg, i) =>
        seg.hit ? (
          <mark key={i} className="rounded-[2px] bg-warn/30 text-fg">
            {seg.text}
          </mark>
        ) : (
          // A BARE string, not a wrapping <span>: off-search this leaves the parent
          // (the title Link / the chip span) with a single direct text node, so an
          // existing getByText/title-attribute lookup still resolves to that parent
          // exactly as it did before highlighting existed. React does not require a key
          // on a plain-string array child, so no warning.
          seg.text
        ),
      )}
    </>
  );
}

/**
 * One board card. Exported for the same reason RunView factors out its panels: `Board`
 * itself needs routing, four API mocks and a drag context to mount, and this file had NO
 * test at all — so an assertion about a card could not otherwise be written, and the #124
 * strip on `card.title` would have shipped unverified.
 */
export function IssueCard({
  card,
  repoId,
  projectWebUrl,
  chips,
  highlightLabels = [],
  query,
  maxChips,
  laneLabel,
  canMoveUp,
  canMoveDown,
  onMoveUp,
  onMoveDown,
  insertionEdge,
  onCardDragOver,
  onCardDragLeave,
  onCardDrop,
  gate,
  starting,
  onStart,
  fixCiBusy,
  onFixCi,
  uziLabel,
  isEligible,
  canPromote: promotable,
  promoting,
  onPromote,
  onDragStart,
  onDragEnd,
  dimmed,
}: {
  card: CardData;
  repoId: string;
  projectWebUrl?: string;
  // The card's chip-worthy labels, already filtered by chipLabels() (PRD #102 M4).
  // Passed in rather than derived here: the exclusion set is board-level state
  // (columns + the configured workflow labels), and keeping it out of the card is
  // what lets this component mount in a test without an auth provider.
  chips: string[];
  // The labels this card carries that should get the brand "hit" treatment when shown
  // (PRD #764: the matched `uzi` label, marking why the card is runnable). Defaults to
  // none so the direct-render tests need not supply it.
  highlightLabels?: string[];
  // The active board search query (PRD #304 M2). When non-empty, the card highlights
  // every case-insensitive occurrence of it in the title and in any matching chip via
  // <mark>. Optional and defaulting to none so the direct-render tests need not supply
  // it, and so a card outside a search renders exactly as before.
  query?: string;
  // Overrides MAX_CARD_CHIPS. Exists so the cap-0 edge — where every label is overflow
  // and a shownChips-gated row would drop the "+N" along with them — is reachable from
  // a test without changing the shipped cap.
  maxChips?: number;
  // The card's lane name, for the reorder buttons' accessible names (PRD #102 M5).
  laneLabel: string;
  canMoveUp: boolean;
  canMoveDown: boolean;
  // NOTE: there is deliberately no `reordering` prop. It used to disable these buttons
  // while a reorder was in flight, and DISABLING A FOCUSED BUTTON BLURS IT — that was
  // B1, the focus falling to <body> and never coming back. Since #18 the re-entry guard
  // is reorderRef, which refuses synchronously, so `disabled` was only ever an
  // affordance here and dropping it removes the whole failure mode rather than
  // compensating for it.
  onMoveUp: () => void;
  onMoveDown: () => void;
  // Which edge of this card the pointer is nearest during a drag, or null. Rendered as
  // an INSET ring rather than a border-width change: growing the box would reflow the
  // lane under the pointer mid-drag.
  insertionEdge: "top" | "bottom" | null;
  // Card-level drag handlers, attached only in a droppable lane (undefined otherwise,
  // which is what keeps the Closed lane inert without a second guard in the handler).
  onCardDragOver?: (e: React.DragEvent<HTMLDivElement>) => void;
  onCardDragLeave?: () => void;
  onCardDrop?: (e: React.DragEvent<HTMLDivElement>) => void;
  gate: StartRunGate;
  starting: boolean;
  onStart: () => void;
  fixCiBusy: boolean;
  onFixCi: () => void;
  // PRD #764. isEligible drives the treatment and the affordances: a card carrying the
  // `uzi` label is runnable and renders solid with Start run; one without it is quiet
  // and gets Promote. canPromote is the narrower question (open, not already `uzi`, not
  // the self-improve tracker), so the two are separate props. uziLabel rides down for
  // the Promote button copy so an instance that renamed the label reads right.
  uziLabel: string;
  isEligible: boolean;
  canPromote: boolean;
  promoting: boolean;
  onPromote: () => void;
  onDragStart: (e: React.DragEvent) => void;
  onDragEnd: () => void;
  dimmed: boolean;
}) {
  // Chips are bounded (PRD #102 M4): a lane is a fixed w-72, so an issue wearing a
  // dozen labels would push the card several rows taller than its neighbours and
  // bury the run badges. The remainder is not dropped — it rides the "+N" title.
  const demo = useDemoMode();
  const { shown: shownChips, overflow: chipOverflow, hidden: hiddenChips } = boundedChips(chips, maxChips);
  // Every card is draggable (PRD #1034 M4). A closed card is now dragged OUT of the
  // Closed lane to reopen it (dropping it on an open lane), and an open card is dragged
  // INTO Closed to close it — so "draggable" no longer means "open". Keyboard reorder
  // still stays disabled for closed cards, gated separately via canMoveUp/canMoveDown
  // (Board.tsx passes `!card.closed` for both), since the Closed lane has no order.
  const draggable = true;
  const run = card.latest_run;
  const badge = run ? runBadge(run, Date.now()) : null;
  // Uniform per-card duration token (issue #256 M4, Decision 4): a faint mono span
  // beside the badge carrying `running 1h 30m` / `queued 4m` / `ran 42m`, "" for
  // terminal (Decision 6). Rides the existing 10s poll via the same Date.now() the
  // badge reads — no new timer (Decision 3).
  const duration = run ? runDurationLabel(run, Date.now()) : "";
  const hint = run ? retryHint(run.run_count) : null;
  // A human-blocked run is the loudest card state: a person owes it an action while a
  // worker is held busy. Give the whole card a warn ring so it can't be missed.
  // PRD #88 D-O #4 puts awaiting_input on exactly this treatment — same debt, and the
  // PRD's own framing is that it is the third human-in-the-loop channel. The ring is
  // presentation, so the two share it; the STRIP above still names them separately.
  // issue #750: feed the EFFECTIVE status so a run re-planning after a revise loses the
  // loud attention ring. Such a run keeps status === "awaiting_approval" server-side, but
  // effectiveRunStatus returns "revising" for it, so needsHumanAttention reads false. Its
  // badge (runBadge → effectiveRunStatus) already reads "revising" in the calm info tone.
  const loud = needsHumanAttention(effectiveRunStatus(run ?? { status: "" }));
  // The MR/PR link (PRD #65 D8): prefer the forge-supplied URL the worker persisted
  // (the only correct link on Forgejo), guarded through isHttpsUrl by preferForgeUrl
  // before it becomes an anchor. A null (rows created before it landed — all GitLab)
  // falls back to the legacy GitLab reconstruction from the project base.
  const mrHref =
    badge?.kind === "mr"
      ? preferForgeUrl(
          run?.mr_web_url,
          isHttpsUrl(projectWebUrl) ? `${projectWebUrl}/-/merge_requests/${badge.mrIid}` : null,
        )
      : null;
  return (
    <div
      draggable={draggable}
      onDragStart={draggable ? onDragStart : undefined}
      onDragEnd={draggable ? onDragEnd : undefined}
      onDragOver={onCardDragOver}
      onDragLeave={onCardDragLeave}
      onDrop={onCardDrop}
      className={cx(
        "group rounded-lg border p-3 text-sm transition-colors",
        // D17. Dashed rather than dimmed: opacity-40 already means "being dragged
        // right now", and border-warn/60 ring-2 is `loud`, reserved for a run blocked
        // on a human (awaiting_approval, or awaiting_input since PRD #88 — see
        // needsHumanAttention). A permanently dim card reads as perpetually mid-drag,
        // and ring treatment would put the least urgent card on the board at the top
        // of the hierarchy.
        //
        // THE BORDER TOKEN IS border-faint, NOT border-edge, AND THAT IS THE WHOLE
        // POINT OF THIS BLOCK. Measured with getComputedStyle on rendered cards of a
        // mock build, in both themes, compositing each card's effective background up
        // the tree (agreeing to 0.01 with the same figures derived from the index.css
        // tokens), against WCAG 1.4.11's 3:1 for a non-text indicator:
        //
        //                                          ember    mission
        //   border-edge  vs the non-PRD card       1.39:1   1.35:1   <- fails
        //   border-faint vs the non-PRD card       5.16:1   5.09:1   <- ships
        //   PRD card bg  vs non-PRD card bg        1.09:1   1.09:1
        //
        // border-edge was the original choice and it is near-invisible: an ordinary
        // card's border is ALREADY border-edge, so dashed-versus-solid at 1.39:1 was
        // the entire distinction. bg-transparent was picked as a second cue on the
        // reasoning that it is how the Closed LANE earns its separation — but that
        // does not survive measurement either: the Closed lane's own background
        // separation is 1.03:1 (ember) / 1.04:1 (mission), WEAKER than the border it
        // was supposed to be supplementing. What distinguishes that lane is its header
        // text. bg-transparent is kept because it is a real if minor cue and it
        // composes, but it is not what carries this.
        //
        // border-faint is reused rather than invented: it is the app-wide
        // de-emphasised token, so a card wearing it reads as quiet, which is what a
        // non-eligible card is. It collides with neither treatment Decision 17 ruled out
        // — `loud` (the warn ring, reserved for a human-blocked run) nor opacity-40
        // (being dragged right now).
        //
        // Second cue: the BUTTON below reads "Promote to uzi" where a runnable card
        // reads "Start run". It is present on every non-runnable card the sync can
        // produce, INCLUDING one that has closed on the forge and not yet been evicted.
        // Under PRD #764 the single `uzi` label is what makes a card runnable ("Start
        // run"); a card without it — a `bug` card, a visibility-only `documentation`
        // card (mock §7) — is non-runnable and wears the quiet/Promote treatment.
        //
        // An earlier version of this comment claimed that window renders no button at
        // all — promotable false, isEligible false. That was WRONG, and measured wrong:
        // canPromote({labels:["documentation"], assignee_ids:[], closed:false}, uziLabel, botForgeUserID)
        // is true because a documentation card lacks the `uzi` label and is not assigned to the bot. During the window the row is never
        // re-upserted (the PRD fetch is label-filtered, the additive fetch is
        // StateOpened, so neither returns a closed non-eligible issue), so issues.state
        // stays 'opened', cardDTO.Closed derives false, and the card renders Promote
        // exactly as it did before it closed. docs/board.md had this right all along —
        // and the false sentence closed by citing docs/board.md as corroboration,
        // which is what let it survive three readers.
        //
        // The state it described is real but is NOT reachable through the sync: it
        // needs closed=true AND isEligible=false, i.e. a row cached while closed (only
        // the state=all PRD fetch writes those) that later stops being eligible — an
        // operator rename while closed PRD cards are cached. Untested, and left
        // untested deliberately rather than asserted.
        isEligible ? "bg-raised/80" : "border-dashed bg-transparent",
        // One border-color class per branch, never two: Tailwind utilities of equal
        // specificity resolve by the order they appear in the generated STYLESHEET,
        // not by their order in this string, so listing border-edge and border-faint
        // together would pick a winner nobody chose.
        loud ? "border-warn/60 ring-2 ring-warn/40" : isEligible ? "border-edge" : "border-faint",
        draggable ? "cursor-grab hover:border-edge-strong active:cursor-grabbing" : "cursor-default",
        dimmed && "opacity-40",
        // An INSET shadow, so the card's box size never changes and the lane does not
        // reflow under the pointer mid-drag — the one interaction where layout
        // stability matters most. `--brand` is the theme's brand channel triple
        // (index.css), so the rule follows ember/mission like every other accent.
        insertionEdge === "top" && "shadow-[inset_0_2px_0_0_rgb(var(--brand))]",
        insertionEdge === "bottom" && "shadow-[inset_0_-2px_0_0_rgb(var(--brand))]",
      )}
    >
      <div className="flex items-start justify-between gap-2">
        {/* In-app issue view. draggable={false}: a native <a> is draggable and
            would hijack the card's HTML5 drag payload. */}
        <Link
          to={`/repos/${repoId}/issues/${card.iid}`}
          draggable={false}
          // min-w-0 break-words (issue #562): a title with a long unbreakable token
          // (e.g. CLAUDE_CODE_ENABLE_TODO_TOOLS=1) must wrap within the fixed w-72
          // lane. break-words lets the token wrap; min-w-0 overrides the flex child's
          // default min-width:auto so it can shrink below the token's intrinsic width.
          className="min-w-0 break-words font-medium leading-snug text-fg hover:text-brand-hover"
        >
          {/* Issue #124: the forge issue title, writable by anyone who can open an issue
              on the repo. Display only — the link targets card.iid, not this string.
              Highlighted passes the ALREADY-sanitized string to highlightSegments, so
              the marked segments are safe for the same reason their input is (PRD #304). */}
          <Highlighted text={stripUnsafeChars(card.title)} query={query ?? ""} />
        </Link>
        <div className="flex shrink-0 items-center gap-1.5">
          {/* Keyboard reorder (PRD #102 M5). NOT redundant with the drag: native HTML5
              drag-and-drop has no keyboard initiation path in any mainstream browser,
              so without these a keyboard or screen-reader user cannot reorder at all
              (WCAG 2.1.1 level A) and has no affordance telling them ordering exists.
              No title/aria-label on a draggable card changes that.

              Revealed on hover/focus with OPACITY, never `hidden` or `display:none` —
              those remove the buttons from the tab order and recreate exactly the
              problem they exist to solve. group-focus-within keeps them visible while
              either one is focused. They drive the same applyDrop, hence the same
              unconditional whole-board freeze, as a drop. */}
          {(canMoveUp || canMoveDown) && (
            <span
              // S6: the [@media(hover:none)]:opacity-100 is the touch fallback, and
              // without it these buttons are UNREACHABLE on a touch device. Reveal was
              // hover- and focus-within-only; the card root is not focusable
              // (confirmed in the a11y tree), so the only way to reveal them without a
              // mouse was to focus the title link — which navigates on tap. So a touch
              // user had no way to reorder at all, and no way to learn that ordering
              // exists.
              //
              // A coarse pointer has no hover state to reveal anything, so the
              // buttons are simply always visible there. That is the same reasoning
              // that made them opacity-based rather than `hidden` in the first place:
              // an affordance you cannot reach is the same as one that is not there.
              //
              // Tailwind 3.4 has no built-in hover-none variant; the arbitrary variant
              // is the supported spelling.
              className="flex items-center opacity-0 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100 [@media(hover:none)]:opacity-100"
            >
              <button
                type="button"
                draggable={false}
                disabled={!canMoveUp}
                onClick={onMoveUp}
                aria-label={`Move issue #${card.iid} up in ${laneLabel}`}
                title={`Move issue #${card.iid} up in ${laneLabel}`}
                className="inline-flex min-h-6 min-w-6 items-center justify-center rounded text-xs leading-none text-faint transition-colors hover:text-fg disabled:opacity-30"
              >
                ↑
              </button>
              <button
                type="button"
                draggable={false}
                disabled={!canMoveDown}
                onClick={onMoveDown}
                aria-label={`Move issue #${card.iid} down in ${laneLabel}`}
                title={`Move issue #${card.iid} down in ${laneLabel}`}
                className="inline-flex min-h-6 min-w-6 items-center justify-center rounded text-xs leading-none text-faint transition-colors hover:text-fg disabled:opacity-30"
              >
                ↓
              </button>
            </span>
          )}
          {isHttpsUrl(card.web_url) && (
            <a
              href={card.web_url}
              target="_blank"
              rel="noreferrer"
              draggable={false}
              aria-label={`Open on ${forgePlatform(card.forge_type)}`}
              title={`Open on ${forgePlatform(card.forge_type)}`}
              className="text-faint transition-colors hover:text-brand"
            >
              <ExternalLinkIcon />
            </a>
          )}
          <span className="font-mono text-xs text-faint">#{card.iid}</span>
        </div>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-1.5">
        {badge &&
          (badge.kind === "mr" ? (
            <MrChip mrIid={badge.mrIid} mrState={badge.mrState} href={mrHref} forgeType={card.forge_type} />
          ) : (
            <span className={badge.pulse ? "animate-pulse" : ""}>
              <Badge tone={badge.tone} title={badge.title}>
                {badge.label}
              </Badge>
            </span>
          ))}
        {duration && (
          <span className="font-mono tabular-nums text-[11px] text-faint">{duration}</span>
        )}
        {hint && (
          <span className="text-[11px] text-faint" title="Number of runs on this issue">
            {hint}
          </span>
        )}
        {/* Neutral PRD-presence marker (PRD #764): a linked prds/*.md is optional but
            still detected, so a card that has one shows a quiet "PRD" badge. */}
        {card.has_prd_link && (
          <Badge tone="neutral" title="This issue links a prds/*.md file">
            PRD
          </Badge>
        )}
        {card.conflict && (
          <Badge tone="danger" title="Issue carries multiple column labels; shown in the highest column until the next move">
            conflict
          </Badge>
        )}
        {card.pipeline && <PipelineBadge pipeline={card.pipeline} />}
        {card.pipeline && <FixCiButton pipeline={card.pipeline} busy={fixCiBusy} onClick={onFixCi} />}
      </div>
      {/* Label chips (PRD #102 M4). Deliberately their OWN row below the badges and
          in the quiet border/muted treatment the issue view already uses, so they
          never compete with the run/pipeline badges for the eye — those stay the
          card's loudest element after the title. Wrapping (not scrolling) keeps the
          lane's width fixed; per-chip truncation keeps one long label from doing the
          same job a hundred short ones would. */}
      {/* Gated on `chips`, NOT on `shownChips`. The distinction is not pedantry: at a
          cap of 0 every label is overflow, so a shownChips guard would hide the row
          AND the "+N" that is supposed to account for the hidden ones — the labels
          would vanish with nothing saying so, contradicting boundedChips' own
          contract that the remainder is withheld rather than lost. */}
      {chips.length > 0 && (
        <div role="group" aria-label="Labels" className="mt-1.5 flex flex-wrap items-center gap-1">
          {/* Issue #124: a label name is forge-supplied like the title, so it is
              stripped where it is DISPLAYED (text and the truncation tooltip) while
              the React key stays the raw string — identity is never built from the
              stripped value. */}
          {shownChips.map((l) => {
            // A matched membership extra is tinted brand (mock §4's `.chip.hit`) to say
            // THIS is why the card is on the board; every other chip stays quiet. The
            // React key is the RAW label — identity is never built from the stripped
            // display value (Issue #124).
            const hit = highlightLabels.includes(l);
            return (
              <span
                key={l}
                title={stripUnsafeChars(l)}
                className={cx(
                  "max-w-full truncate rounded-md border px-1.5 py-0.5 text-[11px]",
                  hit ? "border-brand/50 bg-brand/10 text-brand" : "border-edge bg-raised text-muted",
                )}
              >
                <Highlighted text={stripUnsafeChars(l)} query={query ?? ""} />
              </span>
            );
          })}
          {chipOverflow > 0 && (
            <span
              // Focusable and named, not hover-only. The withheld labels rode a `title`
              // alone, which no keyboard or screen-reader user can reach — the same
              // accessibility class as the drag gesture the ↑/↓ buttons exist for.
              //
              // N1: role="img" is REQUIRED here, not decoration. A bare <span> has the
              // implicit role `generic`, and ARIA 1.2 prohibits naming a generic
              // element — so aria-label on it is invalid and a conforming AT drops it.
              // Chromium exposes the name anyway, which is why neither of this repo's
              // two web instruments can see the defect: the jsdom test passes and a
              // Chromium browser check passes. Firefox announces only "+2", and the
              // withheld labels are lost exactly for the users this attribute exists
              // to serve. role="img" permits a name, replaces the abbreviated content
              // with it, and stays non-interactive, which is what this token is.
              role="img"
              tabIndex={0}
              aria-label={`${chipOverflow} more label${chipOverflow > 1 ? "s" : ""}: ${stripUnsafeChars(hiddenChips.join(", "))}`}
              className="rounded text-[11px] text-faint"
              title={stripUnsafeChars(hiddenChips.join(", "))}
            >
              +{chipOverflow}
            </span>
          )}
        </div>
      )}
      {(run || card.author) && (
        <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-faint">
          {run?.status === "running" && run.worker_name && <span>{run.worker_name}</span>}
          {run && !run.is_mine && run.owner_name && <span>started by {maskName(run.owner_name, demo)}</span>}
          {run && canOpenRunView(run) && (
            <Link
              to={`/runs/${run.id}`}
              draggable={false}
              className="text-brand transition-colors hover:text-brand-hover"
            >
              view run
            </Link>
          )}
          {card.author && <span>{maskUsername(card.author, "human", demo)}</span>}
        </div>
      )}
      {/* D15: Promote stands IN PLACE OF Start run, never beside it. A non-`uzi` card
          cannot start a run (the gate refuses it server-side), so showing a gated Start
          run here would be a disabled button whose tooltip explains a rule the user has a
          one-click answer to. Promote IS that answer — it adds the `uzi` label — and once
          taken the card becomes runnable and Start run appears. */}
      {promotable ? (
        <div className="mt-2.5">
          <Button
            variant="secondary"
            size="sm"
            disabled={promoting}
            title={`Add the ${uziLabel} label so uzi can work this issue`}
            onClick={onPromote}
            className="w-full"
          >
            {promoting ? "Promoting…" : `Promote to ${uziLabel}`}
          </Button>
        </div>
      ) : (
        !card.closed &&
        isEligible && (
          <div className="mt-2.5">
            <Button
              variant={gate.enabled ? "primary" : "secondary"}
              size="sm"
              disabled={!gate.enabled || starting}
              title={gate.enabled ? "Queue an agent run for this issue" : gate.reason}
              onClick={onStart}
              className="w-full"
            >
              {starting ? "Starting…" : gate.enabled ? "Start run" : "Start run (gated)"}
            </Button>
          </div>
        )
      )}
    </div>
  );
}
