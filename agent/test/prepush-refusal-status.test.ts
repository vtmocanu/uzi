import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { composePrepushRefusalStatus } from "../src/runner.js";

// Issue #4080: refresh refusal statuses point at held publication and recovery; every other
// status stays byte-identical to the text refusePrepush emitted before.
const RUN = "5e82d00e-0000-4000-8000-000000000001";
const POINTER = `Retained work, if any, is listed by uzi run held ${RUN} or uzi run recovery ${RUN}.`;
const CONTEXT = `refresh publication refused: source context could not be confirmed for candidate ${"a".repeat(40)}; publication stopped`;
const CLOSING = `non-closing delivery refused: commit message(s) close issue #3120 (${"b".repeat(40)}); publication stopped; correcting them needs a history rewrite, left to the owner`;
const CLOSING_TEXT = `${CLOSING}; work is preserved for owner history correction; partial or accept decisions cannot fix commit history.`;
const CLOSING_HELD = " For unpublished history on a run without a Stop, choose continue with guidance for the lead to reword its own commits; for published history or a Stop, cancel and deliver the preserved work with corrected messages.";
const UNKNOWN_STOPPED = "Completion state could not be confirmed before push; publication stopped.";
const UNKNOWN_HELD = "Completion state could not be confirmed before push; the run is held for the owner.";

describe("composePrepushRefusalStatus", () => {
  it("leaves non-refresh statuses byte-identical", () => {
    assert.equal(composePrepushRefusalStatus("context", CONTEXT, false, false, RUN), CONTEXT);
    assert.equal(composePrepushRefusalStatus("closing", CLOSING, false, false, RUN), CLOSING_TEXT);
    assert.equal(composePrepushRefusalStatus("closing", CLOSING, true, false, RUN), CLOSING_TEXT + CLOSING_HELD);
    assert.equal(composePrepushRefusalStatus("unknown", "ignored", false, false, RUN), UNKNOWN_STOPPED);
    assert.equal(composePrepushRefusalStatus("unknown", "ignored", true, false, RUN), UNKNOWN_HELD);
  });

  it("appends the pointer once to every refusal kind on a refresh", () => {
    assert.equal(composePrepushRefusalStatus("context", CONTEXT, false, true, RUN), `${CONTEXT}. ${POINTER}`);
    assert.equal(composePrepushRefusalStatus("closing", CLOSING, false, true, RUN), `${CLOSING_TEXT} ${POINTER}`);
    assert.equal(composePrepushRefusalStatus("unknown", "ignored", false, true, RUN), `${UNKNOWN_STOPPED} ${POINTER}`);
    for (const kind of ["context", "closing", "unknown"] as const) {
      const text = composePrepushRefusalStatus(kind, kind === "context" ? CONTEXT : CLOSING, false, true, RUN);
      assert.ok(!text.includes(".."), text);
      assert.equal(text.split("Retained work").length - 1, 1, text);
    }
  });

  it("shows a placeholder for a run id that is not a UUID", () => {
    const text = composePrepushRefusalStatus("unknown", "ignored", false, true, "x; Closes #1");
    assert.equal(text, `${UNKNOWN_STOPPED} Retained work, if any, is listed by uzi run held <run-id> or uzi run recovery <run-id>.`);
  });
});
