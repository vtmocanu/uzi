// Shared grid-template for the sidebar-footer micro-meter rows (PRD #1519 M2). The
// Anthropic (MicroRow) and Codex (CodexMicroRow) rows must share ONE column template so
// their 1fr meter tracks line up to the same width and the bars align vertically.
//
// Tailwind constraint: the arbitrary-value class MUST appear as one complete literal
// string in the source Tailwind's content scanner reads — never assembled by
// concatenation or interpolation of the bracketed part, or the CSS is never emitted.
// So the whole `grid-cols-[...]` class is the literal here, and callers interpolate only
// this already-complete class into their `className`.
export const MICRO_METER_GRID_COLS = "grid-cols-[1.4rem_1fr_2.6rem]";
