import { it } from "node:test";
import assert from "node:assert/strict";
import { chunkedTimeout } from "../src/chunked-timeout.js";
import { overflowClock, NATIVE_LIMIT, REPRO_DELAY } from "./overflow-clock.js";

for (const delay of [-1, 0, 1, 0.25, 10.5, NATIVE_LIMIT - 1, NATIVE_LIMIT, NATIVE_LIMIT + 1, REPRO_DELAY, NATIVE_LIMIT * 3 + 7]) {
  it(`chunked timeout expires once at relative deadline ${delay}`, t => {
    const clock = overflowClock(t);
    let calls = 0;
    const handle = chunkedTimeout(() => calls++, delay, { now: clock.now, unref: true });
    assert.equal(calls, 0, "even zero is asynchronous");
    let elapsed = 0;
    for (let i = 0; i < 5 && calls === 0; i++) {
      const timer = clock.pending()[0]!;
      const step = Math.max(1, timer.delay);
      clock.move(step - 0.5);
      clock.deliver();
      assert.equal(calls, 0, "fractional positive remainder cannot expire early");
      clock.move(0.5);
      clock.deliver();
      elapsed += step;
    }
    assert.equal(calls, 1);
    assert.ok(elapsed >= delay);
    clock.assertSafe();
    assert.ok(clock.requests.every(timer => timer.unrefs === 1), "every chunk unref'd");
    handle.cancel(); handle.cancel();
    clock.move(delay + 1); clock.deliver();
    assert.equal(calls, 1);
    assert.equal(clock.pending().length, 0);
  });
}

for (const afterRearm of [false, true]) {
  it(`cancel is idempotent ${afterRearm ? "after" : "before"} rearm`, t => {
    const clock = overflowClock(t);
    const handle = chunkedTimeout(() => assert.fail("cancelled callback"), REPRO_DELAY, { now: clock.now });
    if (afterRearm) { clock.move(NATIVE_LIMIT); clock.deliver(); }
    assert.ok(clock.requests.every(timer => timer.unrefs === 0), "default chunks stay ref'd");
    handle.cancel(); handle.cancel();
    clock.move(REPRO_DELAY); clock.deliver();
    assert.equal(clock.pending().length, 0);
  });
}

it("late wake expires directly without extra chunks", t => {
  const clock = overflowClock(t);
  let calls = 0;
  chunkedTimeout(() => calls++, REPRO_DELAY, { now: clock.now });
  clock.move(REPRO_DELAY + 99); clock.deliver();
  assert.equal(calls, 1);
  assert.equal(clock.requests.length, 1);
});

for (const productionDefault of [false, true]) {
  for (const jump of [-REPRO_DELAY * 3, REPRO_DELAY * 3]) {
    it(`wall jumps do not change relative expiry default=${productionDefault} jump=${jump}`, t => {
      const clock = overflowClock(t);
      if (productionDefault) clock.defaultClock();
      let calls = 0;
      chunkedTimeout(() => calls++, REPRO_DELAY, productionDefault ? {} : { now: clock.now });
      clock.jumpWall(jump);
      clock.move(NATIVE_LIMIT); clock.deliver();
      assert.equal(calls, 0, "first native chunk only rearms");
      assert.equal(clock.requests[1]!.delay, 98_999_354, "remainder uses monotonic time");
      clock.jumpWall(-jump * 2);
      clock.move(98_999_353); clock.deliver();
      assert.equal(calls, 0);
      clock.move(1); clock.deliver();
      assert.equal(calls, 1, "expires despite backward/forward wall jumps");
      clock.assertSafe();
    });
  }
}

it("native early wakes recheck a separately advancing monotonic clock", t => {
  const clock = overflowClock(t);
  let calls = 0;
  chunkedTimeout(() => calls++, 10, { now: clock.now });
  clock.move(10, 100000, 3); clock.deliver();
  assert.equal(calls, 0, "native wake alone cannot expire a monotonic deadline");
  assert.equal(clock.requests[1]!.delay, 7);
  clock.move(7, -200000, 7); clock.deliver();
  assert.equal(calls, 1);
  clock.assertSafe();
});
