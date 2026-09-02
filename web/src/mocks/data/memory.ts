import type {
  CliAuthRequestMeta,
  Memory,
} from "../../lib/api";
import { daysAgo, minsAgo } from "./time";
import { mockAdmin } from "./users";

// ── Agent memory (PRD #90) ────────────────────────────────────────────────
// Seed cross-run learnings across TWO repos so the Settings → Memory tab shows
// its group-by-repo layout, plus a second repo with one entry. Each row carries a
// user_id so the mock filters by session user (the wire Memory type has none — the
// server owner-scopes it — so it lives only on the fixture and mockApi strips it).
// Attributed to the admin persona so logging in as the demo admin shows them.
export const mockMemories: (Memory & { user_id: string })[] = [
  {
    id: "mem-1",
    user_id: mockAdmin.id,
    repo_id: "repo-uzi",
    repo_name: "vtmocanu/uzi",
    title: "Worker image bakes the gcc toolchain since 0.8.3",
    body: "Building the api no longer needs an apt-get for build-essential — the worker chart 0.8.3 bakes gcc/g++/make. Skip the toolchain-install step; it just wastes a couple of minutes.",
    run_id: "e2d7427b",
    created_at: minsAgo(30),
    basis: "observed",
    evidence: "worker chart values.yaml @0.8.3: base image ships gcc/g++/make",
  },
  {
    id: "mem-2",
    user_id: mockAdmin.id,
    repo_id: "repo-uzi",
    repo_name: "vtmocanu/uzi",
    title: "sqlc must be regenerated after touching queries/",
    body: "After editing internal/store/migrations or queries/, run the pinned `sqlc generate` before `go build` — otherwise the generated code and the schema drift and the build fails on a missing method.",
    run_id: "a1f09c34",
    created_at: daysAgo(2),
    basis: "observed",
    evidence: "go build failed on a missing store method until sqlc regen",
  },
  {
    id: "mem-3",
    user_id: mockAdmin.id,
    repo_id: "repo-atlas",
    repo_name: "vtmocanu/atlas-api",
    title: "Integration tests need POSTGRES_DSN pointed at the throwaway db",
    body: "The atlas integration suite reads POSTGRES_DSN; without it the tests silently skip. Point it at the ephemeral compose db, not the dev one, or you'll clobber local fixtures.",
    run_id: "b7734de1",
    created_at: daysAgo(5),
    basis: "inferred",
  },
];

// A seeded PENDING browser-login request so /cli-auth?request=<id> renders the
// consent form in the demo. The code is fixed so the happy path is walkable
// (a real flow prints it in the terminal; a pure-web demo has none). Approving
// requires typing MOCK_CLI_AUTH_CODE below.
export const MOCK_CLI_AUTH_REQUEST_ID = "req-demo";
export const MOCK_CLI_AUTH_CODE = "ABCD-2345"; // canonical "ABCD2345"

export const mockCliAuthRequest: CliAuthRequestMeta & { user_code: string } = {
  client_desc: "uzi CLI on demo-laptop (darwin/arm64)",
  status: "pending",
  expires_at: minsAgo(-5), // ~5 minutes out
  user_code: "ABCD2345",
};
