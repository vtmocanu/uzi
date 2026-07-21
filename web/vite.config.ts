import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In production the SPA is served by nginx, which also proxies /api to the API
// service (same origin, no CORS). The dev proxy below mirrors that so `npm run
// dev` works against a locally running API on :8080. ws: true upgrades the
// /api/ws WebSocket too (nginx does this via its dedicated /api/ws location), so
// the live run view streams under `npm run dev` and not just behind nginx.
export default defineConfig({
  plugins: [react()],
  // Vitest's default testTimeout is 5s, and this project had no `test` block at all — so
  // every one of the 862 tests ran on that default. Measured during PRD #98: under full-suite
  // CPU contention, THREE unrelated tests each timed out once across ~20 runs —
  // mockApi's persistence reload, the Judge nav-badge cross-mount, and Repos' repo-skills
  // opt-in. Different files, different owners, one symptom: "Test timed out in 5000ms",
  // never a wrong assertion.
  //
  // So this is a suite-wide default that is marginal on a loaded machine, not three broken
  // tests, and raising the ceiling here fixes the class instead of patching whichever one
  // surfaced last. It only ever delays a failure — no assertion is loosened, and a passing
  // test costs nothing extra. A 1-in-10 red trains people to re-run instead of read, which is
  // the actual damage.
  test: {
    testTimeout: 20000,
    // Raises Testing Library's own 1s async default too — a SEPARATE knob from testTimeout,
    // and the one the Repos/Agents failures were hitting. See src/test-setup.ts.
    setupFiles: ["./src/test-setup.ts"],
  },
  server: {
    // The docs viewer raw-imports repo-root `docs/*.md` (a sibling of `web/`),
    // which lives outside Vite's project root; allow reading the repo root in
    // dev. The production rollup build is unaffected by this.
    fs: { allow: [".."] },
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8080",
        ws: true,
      },
    },
  },
});
