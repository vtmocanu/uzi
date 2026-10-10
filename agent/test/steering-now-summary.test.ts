// PRD #2603: the steering channel stores the newest /inputs poll's `now_summary` value; absent
// means OFF; and client.getInputs surfaces the field only when it is exactly true.

import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";

import { SteeringChannel } from "../src/steering.js";
import { WorkerClient } from "../src/client.js";
import { nullLogger, stopStartedChannels, withReceipts } from "./helpers.js";

afterEach(stopStartedChannels);

const tick = (ms = 15): Promise<void> => new Promise((r) => setTimeout(r, ms));

function channelFor(replies: Array<boolean | undefined>): SteeringChannel {
  let i = 0;
  const client = withReceipts({
    getInputs: async () => {
      const v = replies[Math.min(i++, replies.length - 1)];
      return { inputs: [], ...(v === undefined ? {} : { nowSummary: v }) };
    },
  } as unknown as WorkerClient);
  return new SteeringChannel(client, "run-1", 1, nullLogger(), new AbortController(), { sleep: () => tick(2) });
}

describe("SteeringChannel.nowSummaryEnabled (PRD #2603)", () => {
  it("is off before any poll", () => {
    assert.equal(channelFor([true]).nowSummaryEnabled(), false);
  });

  it("follows the newest poll: on, then off again when a reply omits it or says false", async () => {
    const on = channelFor([true]);
    on.start();
    await tick(60);
    assert.equal(on.nowSummaryEnabled(), true);

    const flip = channelFor([true, true, undefined]);
    flip.start();
    await tick(120);
    assert.equal(flip.nowSummaryEnabled(), false, "an omitted field reads as off");

    const off = channelFor([true, false]);
    off.start();
    await tick(120);
    assert.equal(off.nowSummaryEnabled(), false);
  });
});

describe("WorkerClient.getInputs now_summary (PRD #2603)", () => {
  async function read(body: unknown): Promise<boolean | undefined> {
    const c = new WorkerClient("http://api.test", "t", "0.1.0-test", nullLogger(), {
      sleep: async () => {},
      fetch: (async () => new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } })) as typeof fetch,
    });
    return (await c.getInputs("run-1")).nowSummary;
  }

  it("is true only for a literal true", async () => {
    assert.equal(await read({ inputs: [], now_summary: true }), true);
    assert.equal(await read({ inputs: [], now_summary: false }), false);
    assert.equal(await read({ inputs: [] }), false);
    assert.equal(await read({ inputs: [], now_summary: "true" }), false);
  });
});
