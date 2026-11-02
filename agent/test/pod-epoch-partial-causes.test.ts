import assert from "node:assert/strict";
import { test } from "node:test";
import { countPartialCause, mergePartialCauses, readFailureCause, type PartialCauses, type PartialCause } from "../src/pod-epoch-partial-causes.js";

test("#3775 sparse allowlisted saturated nonthrowing counters", () => {
  const counts: PartialCauses = {};
  countPartialCause(counts, "group_cap", 0);
  countPartialCause(counts, "invented" as PartialCause);
  assert.deepEqual(counts, {});
  countPartialCause(counts, "group_cap", 65534);
  mergePartialCauses(counts, { group_cap: 10, census_unreadable: 2 });
  mergePartialCauses(counts, { census_unreadable: -1, selector_refused: NaN });
  assert.deepEqual(counts, { group_cap: 65535, census_unreadable: 2 });
  assert.doesNotThrow(() => countPartialCause(Object.freeze({}), "group_cap"));
  const hostile = new Proxy({}, { get() { throw new Error("private"); } });
  assert.doesNotThrow(() => mergePartialCauses(counts, hostile));
  assert.deepEqual(counts, { group_cap: 65535, census_unreadable: 2 });
});
test("#3775 API failure classifier bounds hostile objects and emits only fixed keys", () => {
  for (const stage of ["ownership", "holds"] as const) {
    for (const status of [100, 199, 200, 299, 300, 399, 400, 499, 500, 599])
      assert.equal(readFailureCause(stage, { status }), `${stage}_read_http_${Math.floor(status / 100)}xx`);
    for (const status of [0, 99, 600, -1, NaN, Infinity, 503.5, "503", null])
      assert.equal(readFailureCause(stage, { status }), `${stage}_read_other`);
    for (const error of [{ name: "TimeoutError" }, { code: "ETIMEDOUT" }, { cause: { code: "UND_ERR_HEADERS_TIMEOUT" } }])
      assert.equal(readFailureCause(stage, error), `${stage}_read_timeout`);
    for (const error of [{ name: "TypeError" }, { name: "FetchError" }, { code: "ECONNRESET" }, { cause: { code: "ENOTFOUND" } }])
      assert.equal(readFailureCause(stage, error), `${stage}_read_transport`);
    for (const error of [null, undefined, 1, "private", {}, { name: "TimeoutError" + "x".repeat(200) }])
      assert.equal(readFailureCause(stage, error), `${stage}_read_other`);
    const report: PartialCauses = {};
    countPartialCause(report, readFailureCause(stage, { status: 503,
      message: "https://example.invalid/private-run?detail=untrusted",
      path: "/retained/private-tree", id: "private-run", body: "private error text" }));
    assert.deepEqual(report, { [stage + "_read_http_5xx"]: 1 });
    assert.equal(JSON.stringify(report), '{"' + stage + '_read_http_5xx":1}');
    const cycle: { cause?: unknown } = {};
    cycle.cause = cycle;
    assert.equal(readFailureCause(stage, cycle), `${stage}_read_other`);
    const hostile = new Proxy({}, { get() { throw new Error("private"); } });
    assert.equal(readFailureCause(stage, hostile), `${stage}_read_other`);
    const revoked = Proxy.revocable({}, {}); revoked.revoke();
    assert.equal(readFailureCause(stage, revoked.proxy), `${stage}_read_other`);
    let reads = 0;
    const chain = (): object => ({ get cause() { reads++; return chain(); }, get message() { throw new Error("never read"); } });
    assert.equal(readFailureCause(stage, chain()), `${stage}_read_other`);
    assert.equal(reads, 4);
    assert.equal(readFailureCause(stage, { cause: { cause: { cause: { cause: { status: 500 } } } } }), `${stage}_read_other`);
  }
});
