// PRD #1287 C3 (D7 layer P) — shell + file policy reaching PRODUCTION policy on the REAL protocol.
//
// The real app-server delivers each model-selected callback to a REAL CodexCallbackBroker, whose
// shared `screenBashCommand` / `screenToolPath` guardrails decide it. These cases prove the
// screening is REACHED on the real protocol (not only in a unit call) and that a denial prevents
// its side effect (D4): the command-identity spawn seam and the openat2 fileop client are the
// negative-effect oracles — a denied command never reaches the spawn seam (so it can neither run a
// marker nor disclose a synthetic secret), and a denied path never reaches the fileop client. Each
// leg pairs the denials with a permitted in-worktree control that DOES reach its effect.
//
// The synthetic "secret" is a runtime path string threaded through the SAME
// `screenPolicy.extraSecretPaths` the D6 executor repair builds (homeRoot/codex-data/) — no real
// credential is used; the oracle is that a read of a path under that prefix never reaches the seam.

import { before, test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";

import { bashArgCanary, dummyCredential, scriptedStepsResponder } from "./fake-provider.js";
import { loadProtocolModules, runProtocolTurn, P_SUITE_DEADLINE_MS, type ProtocolModules } from "./harness-p.js";
import { recordEvidence } from "./evidence.js";
import { CODEX_P_SHELL_POLICY_TITLE, CODEX_P_FILE_POLICY_TITLE } from "./titles-c3.js";

import type { RunGrants, SpawnCommandSeam, FileopClient, FileopRequest } from "../../agent/src/codex/broker.js";

let mods: ProtocolModules;
before(async () => {
  mods = await loadProtocolModules();
});

/** A synthetic provider-HOME secret prefix (the D6 shape) — a runtime path string, not a secret. */
const SYNTH_HOME = "/data/agent-home/codex-m4-p-synthetic";
const SYNTH_SECRET_PREFIX = path.join(SYNTH_HOME, "codex-data") + path.sep;
const SYNTH_AUTH_PATH = path.join(SYNTH_HOME, "codex-data", "epoch-0", "codex", "auth.json");

function leadGrants(): RunGrants {
  return {
    role: "lead",
    phase: "implement",
    allowedTools: new Set(["Bash", "Read", "apply_patch"]),
    allowedSkills: new Set<string>(),
    isRoot: true,
  };
}

test(CODEX_P_SHELL_POLICY_TITLE, async (t) => {
  const credential = dummyCredential();
  const bashArg = bashArgCanary();
  const spawnCalls: { argv: readonly string[] }[] = [];
  const spawnCommand: SpawnCommandSeam = async (argv) => {
    spawnCalls.push({ argv });
    return { code: 0, stdout: `${bashArg}\n`, stderr: "" };
  };
  const fileop: FileopClient = { op: async () => ({ ok: true }) };

  const obs = await runProtocolTurn(mods, {
    grants: leadGrants(),
    screenPolicy: { dockerWired: false, extraSecretPaths: [SYNTH_SECRET_PREFIX] },
    credential,
    respond: scriptedStepsResponder([
      { kind: "call", callId: "s-push", name: "uzi_bash", args: { command: "git push" } },
      { kind: "call", callId: "s-secret", name: "uzi_bash", args: { command: `cat ${SYNTH_AUTH_PATH}` } },
      { kind: "call", callId: "s-allowed", name: "uzi_bash", args: { command: `echo ${bashArg}` } },
      { kind: "finish" },
    ]),
    spawnCommand,
    fileop,
    turnDeadlineMs: 60_000,
  });

  t.diagnostic(`shell callbacks=${JSON.stringify(obs.callbacks.map((c) => ({ id: c.callId, ok: c.result.ok })))} (${obs.elapsedMs}ms)`);

  const resultOf = (id: string): boolean | undefined => obs.callbacks.find((c) => c.callId === id)?.result.ok;
  // DENIED before the spawn seam: git push and the synthetic-secret read.
  assert.equal(resultOf("s-push"), false, "git push is denied on the real protocol");
  assert.equal(resultOf("s-secret"), false, "a read of the synthetic provider-HOME secret is denied");
  // NEGATIVE-EFFECT ORACLE: neither forbidden command reached the command spawn seam — it could
  // neither run a marker nor disclose the synthetic secret.
  assert.equal(spawnCalls.some((s) => s.argv.some((a) => a.includes("git push"))), false, "git push never reached the command spawn seam");
  assert.equal(spawnCalls.some((s) => s.argv.some((a) => a.includes(SYNTH_AUTH_PATH))), false, "the secret read never reached the command spawn seam");
  // POSITIVE CONTROL: the harmless in-worktree command reached its effect exactly once.
  assert.equal(resultOf("s-allowed"), true, "the harmless command succeeded");
  assert.equal(spawnCalls.length, 1, "exactly one (allowed) command reached the spawn seam");
  assert.equal(obs.turnStatus, "completed", "the turn completed");
  assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");
  assert.ok(obs.elapsedMs < P_SUITE_DEADLINE_MS, `startup+turn ${obs.elapsedMs}ms under the ${P_SUITE_DEADLINE_MS}ms bound`);

  recordEvidence(CODEX_P_SHELL_POLICY_TITLE, "pass");
});

test(CODEX_P_FILE_POLICY_TITLE, async (t) => {
  const credential = dummyCredential();
  const fileopOps: FileopRequest[] = [];
  const fileop: FileopClient = {
    op: async (request) => {
      fileopOps.push(request);
      if (request.op === "read") return { ok: true, size: 3, data: Buffer.from("hi\n").toString("base64") };
      return { ok: true, size: 3 };
    },
  };
  const spawnCommand: SpawnCommandSeam = async () => ({ code: 0, stdout: "", stderr: "" });

  const obs = await runProtocolTurn(mods, {
    grants: leadGrants(),
    screenPolicy: { dockerWired: false, extraSecretPaths: [SYNTH_SECRET_PREFIX] },
    credential,
    respond: scriptedStepsResponder([
      // Outside-worktree read, credential/secret path read, then allowed in-worktree write + read.
      { kind: "call", callId: "f-outside", name: "uzi_read", args: { path: "/etc/passwd" } },
      { kind: "call", callId: "f-secret", name: "uzi_read", args: { path: SYNTH_AUTH_PATH } },
      { kind: "call", callId: "f-write", name: "uzi_apply_patch", args: { path: "note.txt", content: "hi\n" } },
      { kind: "call", callId: "f-read", name: "uzi_read", args: { path: "note.txt" } },
      { kind: "finish" },
    ]),
    spawnCommand,
    fileop,
    turnDeadlineMs: 60_000,
  });

  t.diagnostic(`file callbacks=${JSON.stringify(obs.callbacks.map((c) => ({ id: c.callId, ok: c.result.ok })))} fileops=${JSON.stringify(fileopOps.map((o) => ({ op: o.op, path: o.path })))}`);

  const resultOf = (id: string): boolean | undefined => obs.callbacks.find((c) => c.callId === id)?.result.ok;
  // DENIED before the fileop client: outside-worktree and the credential/secret path.
  assert.equal(resultOf("f-outside"), false, "an outside-worktree read is denied on the real protocol");
  assert.equal(resultOf("f-secret"), false, "a read of the credential/secret path is denied on the real protocol");
  // NEGATIVE-EFFECT ORACLE: no fileop op ever ran against a forbidden path.
  const forbiddenOps = fileopOps.filter((o) => (o.path ?? "").includes("passwd") || (o.path ?? "").includes("codex-data") || (o.path ?? "").includes("auth.json"));
  assert.deepEqual(forbiddenOps, [], "no fileop op reached a forbidden path");
  // POSITIVE CONTROL: the allowed in-worktree write + read reached the fileop client.
  assert.equal(resultOf("f-write"), true, "the allowed in-worktree write succeeded");
  assert.equal(resultOf("f-read"), true, "the allowed in-worktree read succeeded");
  assert.deepEqual(
    fileopOps.map((o) => ({ op: o.op, path: o.path })).sort((a, b) => a.op < b.op ? -1 : 1),
    [{ op: "read", path: "note.txt" }, { op: "write", path: "note.txt" }],
    "only the allowed in-worktree ops reached the fileop client",
  );
  assert.equal(obs.turnStatus, "completed", "the turn completed");
  assert.deepEqual(obs.providerErrors, [], "the fake provider recorded no errors");

  recordEvidence(CODEX_P_FILE_POLICY_TITLE, "pass");
});
