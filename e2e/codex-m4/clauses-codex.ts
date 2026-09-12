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
import {
  CODEX_P_NATIVE_ABSENT_TITLE,
  CODEX_P_ISOLATION_ENV_TITLE,
  CODEX_P_SHELL_POLICY_TITLE,
  CODEX_P_FILE_POLICY_TITLE,
  CODEX_U_NATIVE_DISPATCH_TITLE,
  CODEX_U_SHELL_VARIANTS_TITLE,
  CODEX_U_FILE_VARIANTS_TITLE,
  CODEX_U_PHASE_GRANTS_TITLE,
  CODEX_U_ROLE_FAILCLOSED_TITLE,
  CODEX_U_SIGNAL_ORIGIN_TITLE,
  CODEX_U_SIGNAL_REPLAY_TITLE,
  CODEX_U_TRUST_CONSTRUCTION_TITLE,
} from "./titles-c3.js";

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

  // ─────────────────────────────────────────────────────────────────────────────
  // C3 REAL rows (PRD #1287). P = real pinned app-server; U = real broker/registry/render/config.
  // Each pairs a positive control with a negative-effect oracle (spawn/fileop/marker/registry),
  // never denial text alone (D4). The C1 required matrix already enforces every codex/P row here;
  // C5 tightens the matrix to also enforce the codex/U rows.
  // ─────────────────────────────────────────────────────────────────────────────

  // ── Native execution bypass ──
  {
    id: "codex-p-native-bypass-forced-dispatch",
    adapter: "codex",
    layer: "P",
    family: "Native execution bypass",
    seam: "codex/launcher.ts:launchCodexRoot + config.ts native-disabled template (shell_tool/unified_exec/code_mode/apply_patch_freeform off) → app-server dispatch",
    positiveControl:
      "the intended-model custom exec (the dynamic uzi_bash callback) reaches its command-identity "
      + "effect through the REAL broker on the real protocol",
    negativeOracle:
      "BOTH halves are checked (PRD 'Native execution bypass'): (schema) the real app-server "
      + "advertises NO native tool identity to the model on any observed provider request — the "
      + "native-disabled template forwards no `tools` array, so an empty native-tool advertisement "
      + "is the absence PASS; (dispatch) forced native shell/exec_command/unified_exec/write_stdin/"
      + "local_shell function-calls and a native freeform apply_patch custom-tool-call produce NO "
      + "worker callback, NO command spawn, NO fileop and NO marker file (absolute marker paths "
      + "asserted absent after the turn)",
    intendedOutcome:
      "no alternate native execution authority or retained writable terminal exists on the "
      + "production path: the model is never offered a native schema AND no native dispatch is "
      + "executable; only the dynamic worker exec runs",
    tests: [CODEX_P_NATIVE_ABSENT_TITLE],
  },
  {
    id: "codex-u-native-bypass-broker-deny",
    adapter: "codex",
    layer: "U",
    family: "Native execution bypass",
    seam: "codex/broker.ts:authorizeAndDispatch (capabilityOf → unknown_tool)",
    positiveControl: "the intended worker exec (Bash) reaches the command spawn seam exactly once",
    negativeOracle:
      "every native name (shell/exec_command/unified_exec/write_stdin/local_shell/container.exec/"
      + "exec/code_interpreter) is denied unknown_tool with the spawn seam AND fileop client at zero calls",
    intendedOutcome:
      "a native tool name reaching the real broker gains no capability; a dynamic worker name is "
      + "distinct and never mistaken for native execution",
    tests: [CODEX_U_NATIVE_DISPATCH_TITLE],
  },

  // ── Shell policy ──
  {
    id: "codex-p-shell-policy",
    adapter: "codex",
    layer: "P",
    family: "Shell policy",
    seam: "codex/broker.ts:dispatchShell → guardrails.ts:screenBashCommand (real protocol)",
    positiveControl: "a harmless in-worktree command reaches the command spawn seam exactly once and its result returns",
    negativeOracle:
      "git push and a synthetic provider-HOME secret read are denied and NEVER reach the command "
      + "spawn seam (zero spawn count), so neither runs a marker nor discloses the synthetic secret",
    intendedOutcome: "shell screening is reached on the real protocol; a forbidden command produces no effect while an allowed one does",
    tests: [CODEX_P_SHELL_POLICY_TITLE],
  },
  {
    id: "codex-u-shell-variants",
    adapter: "codex",
    layer: "U",
    family: "Shell policy",
    seam: "codex/broker.ts:dispatchShell → guardrails.ts:screenBashCommand (git/env/proc/secret/wrapper-depth)",
    positiveControl: "a harmless command reaches the command spawn seam",
    negativeOracle:
      "git push / git -C x push / sh -c 'git push' / git config --get / bare env / a /proc environ read / "
      + "a plaintext synthetic-secret read / wrapper-depth exhaustion are all denied with a zero spawn "
      + "count; the ENCODED base64 pipe is ALLOWED (documented screener residual) and its recorded argv "
      + "discloses no plaintext secret — OS command-root separation (codex-o-command-root-home-denial) is its containment",
    intendedOutcome: "screening reaches production policy for every plaintext form; the base64 residual is characterized honestly, not as a fabricated denial",
    tests: [CODEX_U_SHELL_VARIANTS_TITLE],
    prerequisites: ["codex-o-command-root-home-denial"],
  },

  // ── File policy ──
  {
    id: "codex-p-file-policy",
    adapter: "codex",
    layer: "P",
    family: "File policy",
    seam: "codex/broker.ts:screenAndRelativize → guardrails.ts:screenToolPath (real protocol)",
    positiveControl: "an allowed in-worktree write + read reach the fileop client",
    negativeOracle:
      "an outside-worktree path and a credential/secret path (the D6 codex-data prefix) are denied "
      + "BEFORE the fileop client (no fileop op recorded against a forbidden path)",
    intendedOutcome: "production path enforcement denies before the effect on the real protocol; allowed workspace ops work",
    tests: [CODEX_P_FILE_POLICY_TITLE],
  },
  {
    id: "codex-u-file-variants",
    adapter: "codex",
    layer: "U",
    family: "File policy",
    seam: "codex/broker.ts:screenAndRelativize → guardrails.ts:screenToolPath (realpath canonicalization) + dispatchFileWrite",
    positiveControl: "an allowed in-worktree write reaches the fileop client exactly once",
    negativeOracle:
      "a .git write, an outside-worktree write, a symlink-escape read (real on-disk symlink → /etc), "
      + "and a malformed patch (empty old_string) are all denied with zero forbidden fileop ops",
    intendedOutcome: "production path/schema enforcement denies before the effect; canonicalization catches a symlink escape",
    tests: [CODEX_U_FILE_VARIANTS_TITLE],
  },
  {
    // C3/D6 — the executor-wiring regression (agent/test/codex-home-screening.test.ts). Unlike the
    // U rows above (a hand-built broker with a hand-passed screenPolicy), this drives the REAL
    // production CodexExecutor, which builds its OWN screenPolicy at the wiring site under test — so
    // it is the failing-old/passing-fixed proof of the approved provider-HOME screening repair (the
    // `homeRoot/codex-data/` prefix threaded to every phase broker). Catalogued here so the D6 proof
    // is part of the conformance registry; C5 wires the npm-test evidence + enforcement (the required
    // matrix in run-completeness.ts stays C5's job). The two `tests` titles are copied VERBATIM from
    // that (non-editable, product-source-driving) test file's `it(...)` names and must stay in sync.
    id: "codex-u-home-screening-executor-d6",
    adapter: "codex",
    layer: "U",
    family: "File policy",
    seam: "agent/src/codex/codex-executor.ts screenPolicy → broker screenBashCommand/screenToolPath (D6)",
    positiveControl:
      "through the REAL CodexExecutor-built screenPolicy (not a test-authored policy), a harmless "
      + "in-worktree command reaches the command spawn seam exactly once and an allowed in-worktree "
      + "read reaches the fileop client",
    negativeOracle:
      "a literal `cat <homeRoot>/codex-data/epoch-0/codex/auth.json` credential read is denied BEFORE "
      + "the command spawn seam (spawn counter stays 0), and the resolved Read AND Write path forms of "
      + "the same auth.json are denied with NO fileop effect; reverting the executor's screenPolicy "
      + "repair (dropping the homeRoot/codex-data/ extraSecretPaths) lets the credential argv reach the "
      + "spawn seam (failing-old/passing-fixed calibration)",
    intendedOutcome:
      "the executor's own screenPolicy construction carries the trusted provider-HOME prefix "
      + "(homeRoot/codex-data/) into extraSecretPaths for every phase broker, so a known absolute "
      + "credential path is denied at the U layer rather than relying on OS containment (D6)",
    tests: [
      "shell leg (calibration): a literal `cat <homeRoot>/codex-data/epoch-0/codex/auth.json` is DENIED before the command spawn seam (counter stays 0), while a harmless in-worktree command still reaches its effect",
      "file leg: the resolved Read AND Write path form of the same auth.json is denied by the file screen with NO fileop effect, while an allowed in-worktree read reaches the fileop client",
    ],
  },

  // ── Isolation and compatibility ──
  {
    id: "codex-p-isolation-sparse-env",
    adapter: "codex",
    layer: "P",
    family: "Isolation and compatibility",
    seam: "codex/launcher.ts:buildReplacedEnv (full-replacement sparse env) observed via the real launchCodexRoot spawn",
    positiveControl: "the real app-server is spawned and completes a turn under the replaced env",
    negativeOracle:
      "the captured spawn env carries NO provider credential and NO inherited worker-token/OAuth "
      + "shape, and is a bounded allowlist (not a merged process.env)",
    intendedOutcome: "command/provider credential separation and full-replacement isolation hold; no credential or authority leaks into the app-server env",
    tests: [CODEX_P_ISOLATION_ENV_TITLE],
  },

  // ── Roles and phases ──
  {
    id: "codex-u-phase-grants",
    adapter: "codex",
    layer: "U",
    family: "Roles and phases",
    seam: "codex/render.ts:renderCodexRun grants (phase) + codex/broker.ts:dispatchFileWrite (write_denied_in_plan)",
    positiveControl: "the implement-phase write reaches the fileop client",
    negativeOracle: "the plan-phase write is denied write_denied_in_plan with zero fileop ops; the grants are the REAL render output for each phase",
    intendedOutcome: "the immutable per-phase grants decide: a plan-phase write is denied then a later implement-phase write is allowed",
    tests: [CODEX_U_PHASE_GRANTS_TITLE],
  },
  {
    id: "codex-u-role-failclosed",
    adapter: "codex",
    layer: "U",
    family: "Roles and phases",
    seam: "codex/render.ts grants + codex/broker.ts:authorizeAndDispatch/dispatchDelegate/dispatchMcp",
    positiveControl: "a granted tool runs, a known-role delegation reaches the delegate seam, and a granted skill reaches its handler",
    negativeOracle:
      "an unknown tool (unknown_tool), a spoofed/unknown delegation role (unknown_role, delegate seam "
      + "never reached), and a disallowed skill (denied_skill, handler never reached) all fail closed",
    intendedOutcome: "immutable registry grants decide; unknown input fails closed while valid allocated actions still run",
    tests: [CODEX_U_ROLE_FAILCLOSED_TITLE],
  },

  // ── Workflow signals ──
  {
    id: "codex-u-signal-origin",
    adapter: "codex",
    layer: "U",
    family: "Workflow signals",
    seam: "codex/broker.ts:dispatchSignal (origin/isRoot gate) + signals.ts:scanSignals",
    positiveControl: "a valid root submit_plan/signal_done latches once and surfaces its scanned payload for the reducer",
    negativeOracle:
      "child and unknown-origin submit_plan/signal_done are denied signal_root_only with NO scanned "
      + "output (nothing for a reducer to fold) and never touch the command/file surfaces",
    intendedOutcome: "no unauthorized handler invocation or reducer transition; a valid root signal changes state exactly once",
    tests: [CODEX_U_SIGNAL_ORIGIN_TITLE],
  },
  {
    id: "codex-u-signal-replay",
    adapter: "codex",
    layer: "U",
    family: "Workflow signals",
    seam: "codex/registry.ts:reserveCallback (replay / changed_reuse) via codex/broker.ts:handleToolCall",
    positiveControl: "the original callback runs its effect exactly once",
    negativeOracle:
      "a replayed (thread,turn,call) with the same payload returns the cached terminal with NO second "
      + "effect (spawn count unchanged); a same-id reuse with a CHANGED payload is denied changed_reuse "
      + "with no second effect; a settled root signal replayed returns the cached terminal (latches once)",
    intendedOutcome: "direct malformed/replayed identity injection (a UNIT/broker layer) causes no unauthorized re-execution or extra reducer transition",
    tests: [CODEX_U_SIGNAL_REPLAY_TITLE],
  },

  // ── Repository trust ──
  {
    id: "codex-u-trust-construction",
    adapter: "codex",
    layer: "U",
    family: "Repository trust",
    seam: "codex/config.ts:buildCodexProductionConfigToml/buildCodexLoopbackTestConfigToml + codex-harness.ts:threadConfig (start/resume)",
    positiveControl: "the real executor completes a fresh-start run and a resumed run",
    negativeOracle:
      "the fixed config template pins project_doc_max_bytes=0 + trust_level untrusted and every native "
      + "feature off, rejects any unknown (smuggled repo/hook) key, and references no AGENTS.md/.codex; "
      + "thread/start (start) AND thread/resume (resume) RE-assert the untrusted config and turn/start "
      + "reasserts environments:[]; no repo trust surface rides any construction request",
    intendedOutcome: "untrusted repo content cannot install instructions, callbacks or execution authority at start, resume or a subsequent turn",
    tests: [CODEX_U_TRUST_CONSTRUCTION_TITLE],
  },
];
