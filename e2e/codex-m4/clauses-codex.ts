// PRD #1287 — Codex adapter clause contributions. C3/C4 APPEND the real U/P policy,
// construction, trust, lifecycle and failure-closure rows here (shared fake-provider
// scenarios are owned by C3).
//
// C1 SEEDS this file with:
//   1. the C1 P-smoke's OWN row (the real, executed startup + allowed-callback clause), and
//   2. two O rows demonstrating each of the three-state model's non-`receipt-present` arms —
//      one `inherited` (a prior M3b packaged assertion) and one `owed` (descendant /
//      code-mode-host absence, owned by the maintainer). The `owed` row is a deliberate C1
//      seed: it makes `check:codex-m4-receipts` (the LEAD merge gate) exit non-zero, which is
//      correct — an owed O assertion blocks merge until the maintainer records its receipt.

import type { ClauseRow } from "./clause.js";
import { CODEX_STARTUP_SMOKE_TITLE } from "./titles.js";

export const CODEX_CLAUSES: ClauseRow[] = [
  {
    // C1 REAL, EXECUTED P row — the startup/allowed-callback smoke (startup-smoke.test.ts).
    id: "codex-p-startup-allowed-callback",
    adapter: "codex",
    layer: "P",
    family: "Shell policy",
    seam: "codex/launcher.ts:launchCodexRoot + codex/broker.ts:handleToolCall (uzi_bash)",
    positiveControl:
      "the real pinned app-server, launched through launchCodexRoot with the production "
      + "loopback config builder, admits one allowed uzi_bash callback and completes the turn",
    negativeOracle:
      "the injected spawnCommand seam records the exact allowed argv (reachability) and the "
      + "fake provider observes the function_call_output fed back — proving the broker executed "
      + "and returned, not merely that a frame was emitted",
    intendedOutcome:
      "screening reaches production policy on the REAL protocol; a harmless allowed command "
      + "reaches its effect and its result is delivered back to the model",
    tests: [CODEX_STARTUP_SMOKE_TITLE],
  },
  {
    // C1 SEED — an `inherited` O row: a prior M3b packaged assertion whose OS mechanism is
    // unchanged at this candidate (D8). C4/C5 replace the citation with the exact merge
    // candidate's digest; this seeds the shape and is accepted (well-formed) by the checker.
    id: "codex-o-command-root-home-denial",
    adapter: "codex",
    layer: "O",
    family: "File policy",
    seam: "agent/codex/supervisor + fileop: command-root HOME/credential OS separation",
    positiveControl:
      "M3b Block B drove a harmless command-root effect to completion on the packaged image",
    negativeOracle:
      "M3b Block B proved the injected credential/capability canaries never reached the "
      + "command-root state under uid-10003 Landlock separation",
    intendedOutcome:
      "OS-level credential separation for the command root is evidenced by an unchanged "
      + "packaged mechanism, not inferred from path screening",
    tests: [],
    o: {
      kind: "inherited",
      source: "e2e/codex-m3b/lifecycle.test.ts Block B (PRD #1171 m5)",
      imageDigest: "sha256:PENDING-CANDIDATE-DIGEST",
      target: "codex worker image (base + jvm)",
      unchangedJustification:
        "C1 changes no agent/codex/** supervisor/fileop/launcher code, so the command-root "
        + "HOME/credential separation mechanism is byte-identical to the M3b-proven images; "
        + "the digest is refreshed to the merge candidate before parent M4 acceptance (D8).",
    },
  },
  {
    // C1 SEED — an `owed` O row: no descendant code-mode-host reaping proof exists in this
    // candidate. A direct harness spawn cannot prove the ABSENCE of a descendant code-mode
    // host (PRD "Required clause inventory"); only verified supervisor/descendant evidence
    // can. It is OWED to the maintainer, which is why the receipts gate rejects it at C1.
    id: "codex-o-descendant-code-mode-host-absence",
    adapter: "codex",
    layer: "O",
    family: "Native execution bypass",
    seam: "agent/codex/supervisor: subreaper descendant reaping (no code-mode-host descendant)",
    positiveControl:
      "M3a control A proves a code-mode host DOES appear below the supervisor when enabled",
    negativeOracle:
      "a packaged snapshot below the real supervisor must show NO codex-code-mode-host "
      + "descendant under the shipped stock (code_mode_host=false) config",
    intendedOutcome:
      "no retained descendant native execution host exists on the production packaged path",
    tests: [],
    o: {
      kind: "owed",
      target: "codex worker image (base + jvm) descendant-reaping snapshot",
      reason:
        "descendant/code-mode-host absence needs a fresh packaged snapshot below the REAL Go "
        + "supervisor on a Landlock-capable runtime; the uzi worker cannot prove a descendant "
        + "ABSENCE from a direct harness spawn.",
      owner: "maintainer (D8: fresh packaged O proof, k8s-first Linux runtime)",
    },
  },
];
