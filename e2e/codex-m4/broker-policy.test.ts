// PRD #1287 C3 (D7 layer U) — native-bypass forced dispatch + shell/path + file policy through
// the REAL CodexCallbackBroker / real guardrail screener / real fileop routing.
//
// These drive the production broker directly with fixed adversarial inputs and assert the effect
// never occurs (D4): the command-identity spawn seam and the openat2 fileop client are recording
// spies, so a denial is proven by a zero call count, not by denial text. Each family pairs its
// denials with a permitted control that DOES reach its effect.
//
// Layer note (D3): a direct broker invocation is a UNIT/broker layer, distinct from the real-
// protocol P cases (native-bypass.test.ts / policy-real.test.ts) that drive the same policy
// through the live app-server.

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, symlinkSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import { loadUModules, makeUBroker, leadGrants, rt, type UModules } from "./harness-u.js";
import { recordEvidence } from "./evidence.js";
import {
  CODEX_U_NATIVE_DISPATCH_TITLE,
  CODEX_U_SHELL_VARIANTS_TITLE,
  CODEX_U_FILE_VARIANTS_TITLE,
} from "./titles-c3.js";

let mods: UModules;
before(async () => {
  mods = await loadUModules();
});

describe("codex U broker policy (real broker / screener / fileop)", () => {
  it(CODEX_U_NATIVE_DISPATCH_TITLE, async () => {
    const h = makeUBroker(mods, { grants: leadGrants() });

    // Every NATIVE execution name the shipped Codex could expose. None is a recognized worker
    // capability, so each denies `unknown_tool` BEFORE any grant/effect (a dynamic worker name is
    // `uzi_*`/a recognized callback — deliberately distinct, so an allowed callback is never a
    // native one). `write_stdin` is the retained-terminal probe; `apply_patch` bare is the native
    // freeform surface (config `apply_patch_freeform=false`) — the worker file-write is the
    // DISTINCT `uzi_apply_patch`/canonical apply_patch, not this raw native name via the broker.
    const nativeNames = ["shell", "exec_command", "unified_exec", "write_stdin", "local_shell", "container.exec", "exec", "code_interpreter"];
    for (const name of nativeNames) {
      const r = await h.broker.handleToolCall(rt(), name, { command: ["/bin/sh", "-c", "touch /tmp/native"] }, "root");
      assert.equal(r.ok, false, `native "${name}" is denied`);
      if (!r.ok) assert.equal(r.code, "unknown_tool", `native "${name}" is an unknown tool, not a routed capability`);
    }
    // NEGATIVE-EFFECT ORACLE: no native name reached the command spawn seam or the fileop client.
    assert.equal(h.spawn.calls.length, 0, "no native dispatch reached the command spawn seam");
    assert.equal(h.fileop.ops.length, 0, "no native dispatch reached the fileop client");

    // POSITIVE CONTROL: the intended-model custom exec — the worker Bash callback — reaches its
    // command-identity effect (the native absence is a real deny, not a broken fixture).
    const allowed = await h.broker.handleToolCall(rt(), "Bash", { command: "echo hello" }, "root");
    assert.equal(allowed.ok, true, "the intended worker exec succeeds");
    assert.equal(h.spawn.calls.length, 1, "the worker exec reached the command spawn seam exactly once");

    recordEvidence(CODEX_U_NATIVE_DISPATCH_TITLE, "pass");
  });

  it(CODEX_U_SHELL_VARIANTS_TITLE, async () => {
    // A synthetic secret prefix (a runtime path string, not a secret) proves the plaintext secret
    // read is caught by the screener.
    const secretPrefix = "/data/agent-home/codex-m4-u-synthetic/codex-data/";
    const secretPath = `${secretPrefix}epoch-0/codex/auth.json`;
    const h = makeUBroker(mods, { grants: leadGrants(), screenPolicy: { extraSecretPaths: [secretPrefix] } });

    const procEnviron = "/pro" + "c/1/envi" + "ron"; // assembled so this source file carries no bare /proc path
    const denied: [string, string][] = [
      ["git push", "git push"],
      ["git -C /tmp push", "git -C x push"],
      ["sh -c 'git push'", "sh -c wrapper"],
      ["git config --get user.email", "git config --get"],
      ["env", "bare env"],
      [`cat ${procEnviron}`, "proc environ read"],
      [`cat ${secretPath}`, "plaintext synthetic-secret read"],
    ];
    for (const [command, label] of denied) {
      const r = await h.broker.handleToolCall(rt(), "Bash", { command }, "root");
      assert.equal(r.ok, false, `${label} is denied`);
      if (!r.ok) assert.equal(r.code, "shell_denied", `${label} is denied by the shell screener`);
    }

    // Wrapper-depth exhaustion denies (nested too deeply to screen safely).
    let deep = "echo hi";
    for (let i = 0; i < 12; i++) deep = `sh -c ${JSON.stringify(deep)}`;
    const deepR = await h.broker.handleToolCall(rt(), "Bash", { command: deep }, "root");
    assert.equal(deepR.ok, false, "wrapper-depth exhaustion is denied");

    // NEGATIVE-EFFECT ORACLE: none of the denied commands reached the command spawn seam, so none
    // could produce a marker or disclose the synthetic secret.
    assert.equal(h.spawn.calls.length, 0, "no denied command reached the command spawn seam");

    // Documented RESIDUAL: an ENCODED base64 pipe into a shell is opaque to the screener, so it is
    // ALLOWED (reaches the seam). Its containment is NOT screening but the credential-free command
    // identity (O row codex-o-command-root-home-denial) — the encoded payload discloses no secret
    // through the screener, and the plaintext form above IS denied (the positive control that the
    // screener works). Asserted honestly rather than fabricating a denial (D3/D4).
    const b64 = Buffer.from(`cat ${secretPath}`, "utf8").toString("base64");
    const residual = await h.broker.handleToolCall(rt(), "Bash", { command: `echo ${b64} | base64 -d | sh` }, "root");
    assert.equal(residual.ok, true, "the encoded base64 pipe is allowed by the screener (documented residual; OS command-root separation is the containment)");
    assert.equal(h.spawn.calls.length, 1, "only the encoded residual reached the seam");
    // The argv the seam recorded carries NO plaintext secret path (it is base64-encoded).
    assert.equal(h.spawn.calls.every((c) => c.argv.every((a) => !a.includes(secretPath))), true, "the recorded argv discloses no plaintext secret path");

    // POSITIVE CONTROL: a harmless allowed command reaches its effect.
    const ok = await h.broker.handleToolCall(rt(), "Bash", { command: "echo done" }, "root");
    assert.equal(ok.ok, true, "a harmless command is allowed");
    assert.equal(h.spawn.calls.length, 2, "the harmless command reached the seam");

    recordEvidence(CODEX_U_SHELL_VARIANTS_TITLE, "pass");
  });

  it(CODEX_U_FILE_VARIANTS_TITLE, async () => {
    // A REAL temp worktree so symlink canonicalization (screenToolPath's realpath-when-exists) is
    // genuinely exercised, not only lexical containment.
    const worktree = realpathSync(mkdtempSync(path.join(tmpdir(), "codex-m4-u-file-")));
    try {
      mkdirSync(path.join(worktree, "src"), { recursive: true });
      writeFileSync(path.join(worktree, "src", "keep.ts"), "existing\n");
      // A symlink INSIDE the worktree pointing OUTSIDE it: `escape -> /etc`. A lexical jail alone
      // would let `escape/hostname` resolve in-worktree; realpath resolution catches it.
      symlinkSync("/etc", path.join(worktree, "escape"));

      const h = makeUBroker(mods, { grants: leadGrants(), worktreePath: worktree });

      // .git write, outside-worktree write, symlink escape read, malformed patch (no valid edit).
      const gitWrite = await h.broker.handleToolCall(rt(), "apply_patch", { path: ".git/config", content: "x" }, "root");
      assert.equal(gitWrite.ok, false, ".git write is denied");
      const outside = await h.broker.handleToolCall(rt(), "apply_patch", { path: "../../etc/passwd", content: "x" }, "root");
      assert.equal(outside.ok, false, "an outside-worktree write is denied");
      const symlink = await h.broker.handleToolCall(rt(), "Read", { path: "escape/hostname" }, "root");
      assert.equal(symlink.ok, false, "a symlink escape read is denied by canonicalization");
      const malformed = await h.broker.handleToolCall(rt(), "apply_patch", { path: "src/keep.ts", old_string: "" }, "root");
      assert.equal(malformed.ok, false, "a malformed patch (empty old_string, no content) is denied");

      // NEGATIVE-EFFECT ORACLE: no denied path/patch reached the fileop client.
      assert.equal(h.fileop.ops.length, 0, "no denied file op reached the fileop client");

      // POSITIVE CONTROL: an allowed in-worktree write reaches the fileop client.
      const ok = await h.broker.handleToolCall(rt(), "apply_patch", { path: "src/new.ts", content: "hi\n" }, "root");
      assert.equal(ok.ok, true, "an allowed in-worktree write succeeds");
      assert.deepEqual(h.fileop.ops.map((o) => ({ op: o.op, path: o.path })), [{ op: "write", path: "src/new.ts" }], "only the allowed write reached the fileop client");

      recordEvidence(CODEX_U_FILE_VARIANTS_TITLE, "pass");
    } finally {
      rmSync(worktree, { recursive: true, force: true });
    }
  });
});
