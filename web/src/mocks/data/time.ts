// Seed data for the fully in-browser mock mode (VITE_UZI_MOCK=1). Everything
// here is plain in-memory state: no request ever leaves the browser. Timestamps
// are derived from Date.now() at module load so relative times ("last seen 2m
// ago") always look fresh in a demo.

export const NOW = Date.now();
export const minsAgo = (m: number) => new Date(NOW - m * 60_000).toISOString();
// secsAgo gives sub-minute granularity, needed for the judge run's start/finish stamps
// (PRD #69 M6): a retrospective takes seconds, so its Duration tile needs second-level
// timestamps rather than the minute-granular minsAgo the reviews otherwise use.
export const secsAgo = (s: number) => new Date(NOW - s * 1000).toISOString();
export const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();
// minsAhead is the FUTURE direction, which nothing needed until PRD #35: a parked
// run's whole surface is a countdown, and a countdown seeded in the past renders the
// already-expired state instead of the one worth looking at. Relative to the same
// frozen NOW as its siblings, so the demo's clocks stay consistent with each other.
export const minsAhead = (m: number) => new Date(NOW + m * 60_000).toISOString();
