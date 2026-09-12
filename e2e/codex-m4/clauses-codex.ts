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
import {
  CODEX_U_LIFECYCLE_SYNC_CHILD_TITLE,
  CODEX_U_LIFECYCLE_HELD_CALLBACK_TITLE,
  CODEX_U_LIFECYCLE_LATE_CALLBACK_TITLE,
  CODEX_U_LIFECYCLE_ROOT_ORDERING_TITLE,
  CODEX_U_LIFECYCLE_TIMEOUT_UNCONFIRMED_TITLE,
  CODEX_U_ADVICE_CEILING_TITLE,
  CODEX_U_ADVICE_DISPOSE_RACE_TITLE,
  CODEX_U_ADVICE_APIKEY_ZERO_REFRESH_TITLE,
  CODEX_U_ADVICE_APIKEY_NO_FALLBACK_TITLE,
  CODEX_U_FAILURE_HOOKS_DISABLED_TITLE,
  CODEX_U_FAILURE_BROKER_CLOSURE_TITLE,
} from "./titles-c4.js";

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
        "C1-C4 change no agent/codex/** supervisor/fileop/launcher code — the C3 D6 repair is a "
        + "POLICY-ONLY extra-secret-path addition (agent/src/codex/codex-executor.ts screenPolicy) "
        + "and C4 adds only tests, so per D8 neither invalidates the unchanged OS mechanism: the "
        + "command-root HOME/credential separation is byte-identical to the M3b-proven images. Its "
        + "required regression is the C3 U/P HOME-screener case (codex-u-home-screening-executor-d6) "
        + "plus the M3b host-side Block A evidence, now run via `task test:codex-m3b:host` (C4 §4). "
        + "The digest stays the PENDING placeholder; the maintainer pins the real merge-candidate "
        + "digest and re-runs UZI_CODEX_M3B_PACKAGED=1 test:codex-m3b:packaged before parent M4 (D8).",
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

  // ─────────────────────────────────────────────────────────────────────────────
  // C4 REAL rows (PRD #1287). All layer U: the REAL ExecutionRegistry + CodexExecutionSafety
  // facade, the REAL CodexAdviceHarness (render ceiling + disposeOnce + appserver-auth), and the
  // REAL broker/config builders — driven with injected fakes (D5: reuse the M3 primitives, not a
  // replacement broker). Each pairs a positive control with a negative-effect oracle (sink-called
  // counter / spawn counter / registry terminal / sticky poison / dispose-count / deny code),
  // never denial text alone (D4). Distinct-angle additions to the agent/test unit corpus, not
  // copies. C5 tightens the required matrix to also enforce these codex/U rows.
  // ─────────────────────────────────────────────────────────────────────────────

  // ── Delegation and cleanup (lifecycle) ──
  {
    id: "codex-u-lifecycle-synchronous-child",
    adapter: "codex",
    layer: "U",
    family: "Delegation and cleanup",
    seam: "codex/registry.ts:reserveCallback/settleCallback + codex/safety.ts:withBoundary (quiesce)",
    positiveControl:
      "once the delegated child callback settles, the parent's real durability boundary quiesces "
      + "and runs its sink exactly once (sink-called counter === 1)",
    negativeOracle:
      "with the child callback still in flight, withBoundary throws CodexBoundaryError(quiesce), "
      + "the parent result sink stays UNCALLED (counter 0) and the epoch is poisoned — the parent "
      + "cannot outrun the child",
    intendedOutcome:
      "a parent result/durability outcome cannot settle before a delegated child callback settles",
    tests: [CODEX_U_LIFECYCLE_SYNC_CHILD_TITLE],
  },
  {
    id: "codex-u-lifecycle-held-callback-drain",
    adapter: "codex",
    layer: "U",
    family: "Delegation and cleanup",
    seam: "codex/registry.ts:quiesceChildren (bounded settlement wait) via codex/safety.ts:withBoundary",
    positiveControl:
      "a held callback drained (settled) mid-wait lets the bounded quiesce complete clean and the "
      + "protected sink runs once; the epoch is not poisoned",
    negativeOracle:
      "while the callback is held the boundary quiesce stays PENDING and the protected sink stays "
      + "UNCALLED; a never-drained hold poisons at the deadline with the sink still uncalled",
    intendedOutcome:
      "a held/revoked callback reservation blocks quiesce (incomplete) until it settles; a protected "
      + "sink stays uncalled while the callback is unconfirmed",
    tests: [CODEX_U_LIFECYCLE_HELD_CALLBACK_TITLE],
  },
  {
    id: "codex-u-lifecycle-late-callback",
    adapter: "codex",
    layer: "U",
    family: "Delegation and cleanup",
    seam: "codex/broker.ts:handleToolCall (cached terminal / admission_closed) over a closed codex/registry.ts epoch",
    positiveControl:
      "the original callback runs its command effect exactly once (spawn counter === 1) before the "
      + "parent turn quiesces to closed",
    negativeOracle:
      "after completion, a replay of the same identity+payload returns the cached terminal (output "
      + "{replay:true}) with NO second command spawn, and a NEW callback is denied admission_closed "
      + "with the spawn counter unchanged — no second effect, no reducer transition",
    intendedOutcome:
      "a callback arriving after the parent completed is refused (cached terminal / admission closed) "
      + "with no second effect",
    tests: [CODEX_U_LIFECYCLE_LATE_CALLBACK_TITLE],
  },
  {
    id: "codex-u-lifecycle-root-ordering",
    adapter: "codex",
    layer: "U",
    family: "Delegation and cleanup",
    seam: "codex/safety.ts:spawnBoundaryAction [R3-2] guard (hasLiveCommandRoot) + codex/registry.ts:reapRoot",
    positiveControl:
      "with a provider root and TWO command roots present, the worker_pat git action settles and its "
      + "spawn seam fires exactly once — but ONLY after every command root reaps (lastIdentity worker_pat)",
    negativeOracle:
      "the worker_pat action is refused command_roots_live while ANY command root is still live "
      + "(refused after reaping only one of two), and the boundary-action spawn seam stays at zero "
      + "calls the whole time it is refused",
    intendedOutcome:
      "a worker_pat boundary action is refused while any command root is live and permitted only once "
      + "EVERY command root reaps, so a model-authorized command process can never read the PAT",
    // No `prerequisites`: this U row proves the registry/safety reap CONTRACT with injected fake
    // roots on its own. The OS-level whole-root reaping it mirrors is the SEPARATE owed O row
    // (codex-o-packaged-descendant-reaping); coupling a U row to an `owed` O prereq would (once C5
    // makes codex/U required) read as an unmet prerequisite and wrongly deadlock the worker gate.
    tests: [CODEX_U_LIFECYCLE_ROOT_ORDERING_TITLE],
  },
  {
    id: "codex-u-lifecycle-timeout-unconfirmed",
    adapter: "codex",
    layer: "U",
    family: "Delegation and cleanup",
    seam: "codex/safety.ts:withBoundary (deadline poison) + codex/registry.ts sticky poison",
    positiveControl:
      "a clean boundary on a fresh registry reaches its durability sink (counter 1), proving the sink "
      + "is reachable",
    negativeOracle:
      "a boundary whose quiesce settles only after the deadline poisons and leaves the durability sink "
      + "UNCALLED (counter 0); the poison is STICKY so a later clean-seam boundary still fails at quiesce "
      + "and the sink stays uncalled — an unconfirmed boundary permanently blocks the protected outcome",
    intendedOutcome:
      "a timed-out / deadline-aborted boundary leaves the sink uncalled and its unconfirmed (poisoned) "
      + "state prevents protected durability outcomes thereafter",
    tests: [CODEX_U_LIFECYCLE_TIMEOUT_UNCONFIRMED_TITLE],
  },

  // ── Advice ceiling ──
  {
    id: "codex-u-advice-ceiling",
    adapter: "codex",
    layer: "U",
    family: "Advice ceiling",
    seam: "codex/render.ts:renderCodexAdvice (runtime ceiling) + codex/codex-advice-harness.ts:CodexAdviceHarness options",
    positiveControl:
      "a clean advice pass (no run-tool keys) returns its accumulated text and disposes the isolated "
      + "HOME once — the ceiling does not break legitimate advice",
    negativeOracle:
      "renderCodexAdvice THROWS for a request carrying tools/toolServers/cwd, the harness rejects a "
      + "cwd-carrying request and launches NOTHING (launchSpecs 0, so no isolated root and hence no "
      + "shell/file/network/delegation/credential surface), and the options type exposes no handler "
      + "registry / registry / run workspace / cwd / delegate (compile-time)",
    intendedOutcome:
      "forbidden advice effects never occur: the advice lane offers no shell/files/network/delegation/"
      + "run-worker-signal/credential surface, enforced at render runtime and by construction",
    tests: [CODEX_U_ADVICE_CEILING_TITLE],
  },
  {
    id: "codex-u-advice-dispose-race",
    adapter: "codex",
    layer: "U",
    family: "Advice ceiling",
    seam: "codex/codex-advice-harness.ts:disposeOnce (finally + late-work owners)",
    positiveControl: "a clean advice pass disposes the isolated HOME exactly once (dispose-count === 1)",
    negativeOracle:
      "an external abort fired WHILE the disposer is already in flight does NOT cause a second "
      + "disposal — dispose-count stays === 1 (never 2, never 0) and the primary result still stands",
    intendedOutcome:
      "the returned promise respects required cleanup: disposeOnce runs EXACTLY once even when abort "
      + "races an in-flight disposal",
    tests: [CODEX_U_ADVICE_DISPOSE_RACE_TITLE],
  },
  {
    id: "codex-u-advice-apikey-zero-refresh",
    adapter: "codex",
    layer: "U",
    family: "Advice ceiling",
    seam: "codex/appserver-auth.ts:createCodexAppServerAuth(api_key) via codex/codex-advice-harness.ts:run",
    positiveControl:
      "the refresh seam is detectable: a SUBSCRIPTION advice pass whose stream carries a refresh "
      + "request calls the bridge exactly once and delivers a token",
    negativeOracle:
      "a clean api_key advice pass performs ZERO refresh — the wire is exactly "
      + "[initialize, account/login/start, thread/start, turn/start] with login type apiKey, no "
      + "account/chatgptAuthTokens/refresh request, and zero refresh responses — while its accumulated "
      + "text and turn-basis usage are preserved",
    intendedOutcome:
      "an api_key advice run assembles a synthetic credential at runtime, performs no credential "
      + "refresh and no subscription fallback, and preserves the existing control's semantics",
    tests: [CODEX_U_ADVICE_APIKEY_ZERO_REFRESH_TITLE],
  },
  {
    id: "codex-u-advice-apikey-no-fallback",
    adapter: "codex",
    layer: "U",
    family: "Advice ceiling",
    seam: "codex/appserver-auth.ts refresh pump (api_key branch) via codex/codex-advice-harness.ts:run",
    positiveControl:
      "the api_key session authenticates (initialize + login) first, so the refusal below is a real "
      + "deny rather than a broken setup",
    negativeOracle:
      "a subscription-refresh server request during an api_key advice pass is refused fail-closed: the "
      + "response is an ERROR (no result), NO access token is minted anywhere, the run fails closed, and "
      + "the isolated HOME is still disposed once",
    intendedOutcome:
      "the api_key advice lane never falls back to re-presenting the api key as a refreshed credential; "
      + "an unsupported refresh fails closed with no credential effect",
    tests: [CODEX_U_ADVICE_APIKEY_NO_FALLBACK_TITLE],
  },

  // ── Failure closure ──
  {
    id: "codex-u-failure-hooks-disabled",
    adapter: "codex",
    layer: "U",
    family: "Failure closure",
    seam: "codex/config.ts:buildCodexProductionConfigToml + buildCodexLoopbackTestConfigToml (native-disabled template)",
    positiveControl:
      "the builders emit their intended authenticated surface — production keeps the built-in openai "
      + "provider (no override block) and the loopback builder wires an authenticated fake provider "
      + "(requires_openai_auth=true) — so the disabled hooks are a real deny inside a working config",
    negativeOracle:
      "every builder (prod api_key, prod subscription, loopback) pins hooks=false with NO hooks=true, "
      + "NO bypass_hook_trust / dangerously-bypass-hook-trust, every native execution feature "
      + "(shell_tool/unified_exec/code_mode*/apply_patch_freeform/multi_agent*/plugins/apps/remote_models) "
      + "off, project_doc_max_bytes=0 and trust_level=untrusted; a non-loopback provider URL is rejected. "
      + "The upstream hook serialization/spawn/timeout/malformed-output CHARACTERIZATION is the M0 "
      + "test-only process (e2e/codex-m0/hooks-stdin.test.mjs, harness-errors.test.mjs) — referenced "
      + "(its files exist), NOT re-run in this gate; production hooks being off is the production-side proof",
    intendedOutcome:
      "an upstream hook serialization/spawn/timeout/malformed-output failure can NEVER become production "
      + "permission, because production Codex hooks (and every native feature) are disabled",
    tests: [CODEX_U_FAILURE_HOOKS_DISABLED_TITLE],
  },
  {
    id: "codex-u-failure-broker-closure",
    adapter: "codex",
    layer: "U",
    family: "Failure closure",
    seam: "codex/broker.ts:dispatchMcp (handler_error/denied_tool) + handleToolCall try/catch (broker_error) over the real registry",
    positiveControl:
      "the SAME handler path returns success when the worker-tool handler behaves — the fail-closed "
      + "denies are real refusals, not a broken fixture",
    negativeOracle:
      "a THROWING worker-tool handler denies handler_error, an UNWIRED granted handler denies "
      + "denied_tool, and a REJECTING command spawn (callback/transport failure) denies broker_error — "
      + "each with the command-spawn and fileop counters at 0 and NO fabricated success (the callback "
      + "settled ERROR, so a replay returns replayed_error); an honest failure is a bounded deny, not a poison",
    intendedOutcome:
      "a throwing/malformed worker policy/handler or a callback/transport failure cannot authorize an "
      + "effect or fabricate a successful completion — production policy/handler/transport fail closed",
    tests: [CODEX_U_FAILURE_BROKER_CLOSURE_TITLE],
  },

  // ── C4 NEW O row (D8): a lifecycle/descendant PACKAGED invariant with no prior case that
  //    proves it, so it is OWED to the maintainer (rejected by check:codex-m4-receipts, the lead's
  //    pre-merge gate). The C4 U lifecycle cases prove the registry/safety REAP CONTRACT at the U
  //    layer; the OS-level "the real Go supervisor reaps a boundary-action root's WHOLE descendant
  //    set to ECHILD(+__WALL) before publication" is a packaged effect a direct harness spawn
  //    cannot establish (PRD "Required clause inventory"). The lifecycle root-ordering U row
  //    (codex-u-lifecycle-root-ordering) does NOT — and MUST NOT — prerequisite this owed O row:
  //    that U row proves the registry/safety reap ordering CONTRACT with injected fake roots on its
  //    own, and coupling it to an `owed` O prereq would make checkCompleteness read it as an unmet
  //    prerequisite and wrongly fail the worker gate once codex/U is required (which C5 now makes
  //    it). The two rows are related in intent but deliberately decoupled in the registry.
  {
    id: "codex-o-packaged-descendant-reaping",
    adapter: "codex",
    layer: "O",
    family: "Delegation and cleanup",
    seam: "agent/codex/supervisor: whole-root ECHILD(+__WALL) reaping of a boundary-action/command root",
    positiveControl:
      "M3b Block B drives a boundary-action/command root to completion on the packaged image and "
      + "observes the supervisor reap its whole descendant set before the durability sink publishes",
    negativeOracle:
      "a packaged snapshot must show the supervisor reaching ECHILD(+__WALL) for every registered root "
      + "(including a backgrounded/process-group-escapee descendant) BEFORE any checkpoint/finalize "
      + "publication — a drained-but-not-reaped descendant must block observed_empty, never publish",
    intendedOutcome:
      "the registry/safety reap contract exercised at the U layer is backed by real supervisor "
      + "descendant reaping on the packaged path: a durability outcome never outruns whole-root ECHILD",
    tests: [],
    o: {
      kind: "owed",
      target: "codex worker image (base + jvm) supervisor whole-root reaping snapshot",
      reason:
        "whole-root ECHILD(+__WALL) descendant reaping under the REAL Go supervisor is a packaged OS "
        + "effect; the uzi worker's C4 U cases prove only the registry/safety CONTRACT with injected "
        + "fake roots, so a fresh packaged snapshot below the real supervisor on a Landlock-capable "
        + "runtime is required and is maintainer-owned (D8).",
      owner: "maintainer (D8: fresh packaged O proof, k8s-first Linux runtime)",
    },
  },
];
