import { MessageBatcher } from "../src/batcher.js";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import type { HarnessEvent } from "../src/harness.js";
import type { EmittedMessage } from "../src/executor.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { makeRedactor } from "../src/redact.js";
import { nullLogger } from "./helpers.js";

export async function m3Persist(messages: EmittedMessage[]): Promise<OutgoingMessage[]> {
  const sent: OutgoingMessage[] = [];
  const client = {
    postUsage: async () => {},
    postMessages: async (_id: string, batch: OutgoingMessage[]) => { sent.push(...structuredClone(batch)); },
  } as unknown as WorkerClient;
  const redact = makeRedactor(["projection-private-value"]);
  const batcher = new MessageBatcher(client, "m3-run", 0, 60_000, nullLogger(), redact, text =>
    text.replaceAll("projection-private-value", "***REDACTED***"), { generation: 3 });
  try {
    for (const message of messages) batcher.emit(message);
  } finally {
    await batcher.close();
  }
  return sent;
}

export async function m3ReduceAndPersist(events: HarnessEvent[]): Promise<OutgoingMessage[]> {
  const reducer = new RunTurnReducerImpl({ request() {}, async get() { return undefined; } });
  reducer.beginTurn();
  const messages: EmittedMessage[] = [];
  for (const event of events) messages.push(...(await reducer.accept(event)).messages);
  return m3Persist(messages);
}
