import type {
  BuildInfo,
} from "../../lib/api";
import { daysAgo } from "./time";

// ── Build info (PRD #175) ───────────────────────────────────────────────────
// THREE fixtures, because these shapes exercise different code and only the first
// is type-enforced. `api: typeof realApi` typechecks mockApi against the real
// client, but every field below except version and founded is OPTIONAL — so a mock
// returning `{version}` alone compiles clean, and the degraded renders have no
// type safety at all.
//
// Each one names the situation that produces it, because "degraded" is not one
// shape and an earlier version of this comment got that wrong: it claimed a local
// `docker compose` stack reports `mockBuildInfoUnstamped`, which was measured
// FALSE against a live unstamped stack. `handler.New()` always sets `startedAt`
// (api/internal/handler/handler.go), and Version emits `uptime_seconds` whenever
// it is non-zero — so a laptop build omits the three LDFLAGS fields and keeps
// uptime. The two-key body is a struct-literal `Handler`, a test construction that
// no server ever serves. The comment had labelled the one shape a laptop never
// produces as "the COMMON case".
//
// The commit is this repo's real, public first commit rather than an invented
// 40-char hex string: a fixture should not carry a high-entropy literal that reads
// like a credential to a secret scanner, and a published SHA cannot be one.

// A stamped release build — what a published image serves. Matches a live stamped
// body key for key.
export const mockBuildInfo: BuildInfo = {
  version: "0.4.2", // matches the worker fleet's target release in this demo
  founded: "2026-07-03",
  built_at: daysAgo(2),
  commit: "366a282d52095312f54b99698b241ac872e20284",
  commits: 2105,
  uptime_seconds: 3 * 86_400 + 4 * 3_600 + 12 * 60, // 3d 4h 12m
  prds_done: 80,
  prds_open: 32,
  // Upstream-release signal (PRD #836 M4) so mock-mode + demos exercise the pip and
  // the popover's update row: a newer release than the running 0.4.2, checked a few
  // days ago. `latest.version` is `v`-prefixed exactly as the server serves it.
  // Only the stamped release fixture carries this; the two `dev` fixtures below are
  // left WITHOUT it, representing the unknown (never-checked) state.
  latest: {
    version: "v0.5.0",
    name: "Hosted worker drain controls",
    published_at: daysAgo(3),
    notes_url: "https://github.com/vtmocanu/uzi/releases/tag/v0.5.0",
    security: false,
  },
  update_available: true,
  far_behind: false,
};

// THE LAPTOP SHAPE, and the one a developer actually hits: `docker-compose.yml`
// builds the api with no ldflags, so version falls back to "dev" and built_at,
// commit and commits are all omitted — but the process is running, so uptime is
// there. Three keys, not two.
export const mockBuildInfoUnstamped: BuildInfo = {
  version: "dev",
  founded: "2026-07-03",
  uptime_seconds: 4 * 60 + 12, // 4m 12s — a stack somebody just brought up
};

// Uptime UNKNOWN as well: a `Handler` built as a struct literal leaves startedAt
// the zero time, and Version omits rather than reporting roughly two millennia.
// Kept as its own fixture because it is a real wire shape the renderer must handle
// and NOT a laptop's — conflating the two is what the note above is about.
export const mockBuildInfoNoUptime: BuildInfo = {
  version: "dev",
  founded: "2026-07-03",
};
