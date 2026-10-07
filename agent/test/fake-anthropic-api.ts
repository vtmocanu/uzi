import http from "node:http";
import type { AddressInfo } from "node:net";

/** One scripted assistant turn: plain text (end_turn) or a single tool call. */
export type ScriptedTurn =
  | { kind: "text"; text: string }
  | { kind: "tool_use"; name: string; input: Record<string, unknown> };

/** The slice of a Messages API request body the scripts route on. */
export interface MessagesRequest {
  model?: string;
  max_tokens?: number;
  stream?: boolean;
  system?: unknown;
  tools?: Array<{ name: string }>;
  messages: Array<{ role: string; content: unknown }>;
}

export interface RecordedRequest {
  method: string;
  path: string;
}

/**
 * A local stand-in for the Anthropic Messages API, just enough for the real bundled
 * Claude CLI to run a scripted tool-using conversation (issue #2332 AC6). The caller's
 * `respond` picks the next turn from the request CONTENT, never from call order, because
 * the CLI makes side calls (titles, topic detection) that must not advance a script.
 * Every request is logged so a failing test can print what the CLI actually asked for.
 */
export class FakeAnthropicApi {
  private readonly server: http.Server;
  readonly requests: RecordedRequest[] = [];
  readonly messageBodies: MessagesRequest[] = [];

  constructor(private readonly respond: (body: MessagesRequest) => ScriptedTurn) {
    this.server = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => this.handle(req, res, Buffer.concat(chunks).toString("utf8")));
    });
  }

  async listen(): Promise<string> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    // Unref so a leaked listening handle can never hang the test process (see FakeApi.listen).
    this.server.unref();
    return `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }

  async close(): Promise<void> {
    this.server.closeAllConnections();
    await new Promise<void>((resolve, reject) => this.server.close((err) => (err ? reject(err) : resolve())));
  }

  /** "METHOD path" lines, for failure messages. */
  requestLog(): string {
    return this.requests.map((r) => `${r.method} ${r.path}`).join("\n");
  }

  private handle(req: http.IncomingMessage, res: http.ServerResponse, raw: string): void {
    const path = (req.url ?? "").split("?")[0] ?? "";
    this.requests.push({ method: req.method ?? "", path });
    const json = (status: number, body: unknown): void => {
      res.writeHead(status, { "content-type": "application/json" });
      res.end(JSON.stringify(body));
    };
    if (req.method === "POST" && path === "/v1/messages/count_tokens") return json(200, { input_tokens: 100 });
    if (req.method !== "POST" || path !== "/v1/messages") return json(404, { type: "error", error: { type: "not_found_error", message: "not found" } });
    let body: MessagesRequest;
    try {
      body = JSON.parse(raw) as MessagesRequest;
    } catch {
      return json(400, { type: "error", error: { type: "invalid_request_error", message: "bad json" } });
    }
    this.messageBodies.push(body);
    const turn = this.respond(body);
    type Block = { type: "text"; text: string } | { type: "tool_use"; id: string; name: string; input: Record<string, unknown> };
    const content: Block[] =
      turn.kind === "text"
        ? [{ type: "text", text: turn.text }]
        : [{ type: "tool_use", id: `toolu_${this.messageBodies.length}`, name: turn.name, input: turn.input }];
    const stopReason = turn.kind === "text" ? "end_turn" : "tool_use";
    const message = {
      id: `msg_${this.messageBodies.length}`,
      type: "message",
      role: "assistant",
      model: body.model ?? "claude-sonnet-4-5",
      stop_reason: null as string | null,
      stop_sequence: null,
      usage: { input_tokens: 100, output_tokens: 1 },
    };
    if (!body.stream) {
      return json(200, { ...message, content, stop_reason: stopReason, usage: { input_tokens: 100, output_tokens: 20 } });
    }
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    const send = (event: string, data: unknown): void => {
      res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
    };
    send("message_start", { type: "message_start", message: { ...message, content: [] } });
    content.forEach((block, index) => {
      if (block.type === "text") {
        send("content_block_start", { type: "content_block_start", index, content_block: { type: "text", text: "" } });
        send("content_block_delta", { type: "content_block_delta", index, delta: { type: "text_delta", text: block.text } });
      } else {
        send("content_block_start", {
          type: "content_block_start",
          index,
          content_block: { type: "tool_use", id: block.id, name: block.name, input: {} },
        });
        send("content_block_delta", {
          type: "content_block_delta",
          index,
          delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) },
        });
      }
      send("content_block_stop", { type: "content_block_stop", index });
    });
    send("message_delta", {
      type: "message_delta",
      delta: { stop_reason: stopReason, stop_sequence: null },
      usage: { output_tokens: 20 },
    });
    send("message_stop", { type: "message_stop" });
    res.end();
  }
}
