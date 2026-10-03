import { it } from "node:test";
import assert from "node:assert/strict";
import { legacyTrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";

it("trusted refusal: legacy parameter guards accept only complete producer domains", () => {
  for (const reason of [
    "command root failed registry admission",
    "expected snapshot evidence, got dispose",
    "supervised provider child exited unexpectedly (code=255)",
    "started deadline exceeded (0ms)",
    "runner uid resolution produced an invalid uid: garbage",
    "resolved runner uid is invalid: NaN",
    "cannot resolve the runner uid (id -u runner exited null)",
    'Codex provider name must match /^[A-Za-z0-9._-]+$/ (got "bad name")',
    'Codex M3a supports only the "responses" wire (got "chat")',
    "autopilot plan not durably stored — the run is future_status",
    "could not park the run to ask a question (server reports future_status)",
    "supervisor reported an unsafe start posture (pid=[1,2], expectedPid=42, subreaper=(bad), nondumpable=true, liveCapsZero=true, capBoundingSet=0xc0, noNewPrivs=true, uid=(bad), expected 10002)",
  ]) assert.equal(legacyTrustedExecutionRefusal(reason), true, reason);

  for (const reason of [
    "expected snapshot evidence, got snapshot",
    "startup: Error: expected snapshot evidence, got snapshot",
    "invalid root failed registry admission",
    "command root failed registry admission trailing",
    "supervised provider child exited unexpectedly (code=256)",
    "supervised provider child exited unexpectedly (code=01)",
    "started deadline exceeded (-1ms)",
    "started deadline exceeded (01ms)",
    "runner uid resolution produced an invalid uid: 10002",
    "resolved runner uid is invalid: 10002",
    "cannot resolve the runner uid (id -u runner exited 0)",
    'Codex provider name must match /^[A-Za-z0-9._-]+$/ (got "valid_name")',
    'Codex M3a supports only the "responses" wire (got "responses")',
    'Codex M3a supports only the "responses" wire (got "chat") trailing',
    'filesystem says "command root failed registry admission"',
    'filesystem says: "command root failed registry admission"',
    "filesystem says\ncommand root failed registry admission",
    "supervisor reported an unsafe start posture (pid=1, uid=2, expected 10002)",
  ]) assert.equal(legacyTrustedExecutionRefusal(reason), false, reason);
});

it("trusted refusal: legacy context peeling exceeds eight prefixes and allows bracketed contexts", () => {
  const reason = "command launch admission is closed";
  assert.equal(legacyTrustedExecutionRefusal("Error: ".repeat(12) + reason), true);
  assert.equal(legacyTrustedExecutionRefusal("(startup): [provider]: " + reason), true);
  assert.equal(legacyTrustedExecutionRefusal('quoted "context": ' + reason), false);
  assert.equal(legacyTrustedExecutionRefusal("context\nmore: " + reason), false);
});

it("trusted refusal: apostrophe contexts preserve the literal colon-space envelope", () => {
  const reason = "command launch admission is closed";
  assert.equal(legacyTrustedExecutionRefusal("worker's startup: " + reason), true);
  assert.equal(legacyTrustedExecutionRefusal("worker's startup: Error: Error: " + reason), true);
  assert.equal(legacyTrustedExecutionRefusal('quoted "context": ' + reason), false);
  assert.equal(legacyTrustedExecutionRefusal("context\nmore: " + reason), false);
  assert.equal(legacyTrustedExecutionRefusal("context\rmore: " + reason), false);
  assert.equal(legacyTrustedExecutionRefusal("filesystem says: '" + reason + "'"), false);
  assert.equal(legacyTrustedExecutionRefusal("startup:\n" + reason), false);
});

it("trusted refusal: legacy helper honors the caller's existing control predicate", () => {
  const reason = "denied by guardrail: other trusted policy";
  const existing = (candidate: string) => candidate.startsWith("denied by guardrail: ");
  assert.equal(legacyTrustedExecutionRefusal(reason), false);
  assert.equal(legacyTrustedExecutionRefusal("startup: " + reason, existing), true);
  assert.equal(legacyTrustedExecutionRefusal('I/O says "' + reason + '"', existing), false);
});
