// The in-process MCP server that carries the guarded command tool (issue #2471, M2). On a
// guard-bound Claude flight the native Bash tool is aliased to `mcp__uzi_cmd__bash`; the tool
// handler is the command lane. The server `timeout` is transport headroom only: the lane owns
// every command's deadline, and the CLI does not stop a handler after an abort.

import { createSdkMcpServer, tool } from "@anthropic-ai/claude-agent-sdk";
import type { McpSdkServerConfigWithInstance } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";
import { GUARDED_COMMAND_SERVER } from "./grants.js";
import {
  DEFAULT_COMMAND_TIMEOUT_MS, MAX_COMMAND_BYTES, MAX_COMMAND_TIMEOUT_MS, MAX_CONCURRENT_COMMANDS,
  type ClaudeCommandLane,
} from "./lane.js";
import { INLINE_CAP_CHARS } from "./bounded-output.js";

/** Transport headroom over the longest command: not a command deadline. */
export const GUARDED_SERVER_TIMEOUT_MS = MAX_COMMAND_TIMEOUT_MS + 60_000;

export const GUARDED_COMMAND_DESCRIPTION = [
  "Run a shell command in the foreground and return its exit code and output.",
  "Foreground only: run_in_background is refused and there is no long-running mode or extension.",
  `The default deadline is 2 minutes (${String(DEFAULT_COMMAND_TIMEOUT_MS)} ms) and the maximum is 60 minutes (${String(MAX_COMMAND_TIMEOUT_MS)} ms); pass an explicit timeout in milliseconds for a long required check.`,
  "At the deadline the command and every descendant process are stopped and drained. A required check that cannot finish within 60 minutes is a blocker to report, not something to skip.",
  "Every call starts at the repository root: use `cd dir && command` to work elsewhere.",
  `Inline output is capped at ${String(INLINE_CAP_CHARS)} characters (a start-and-end preview). A best-effort copy of the output may be written to .uzi/scratch/uzi-cmd/<id>.log; it may be absent or truncated, is capped at 64 MiB and truncated beyond that (bounded, not the full output), and a previewed result names its path.`,
  `At most ${String(MAX_CONCURRENT_COMMANDS)} commands may run at once; further calls are refused until one finishes.`,
].join(" ");

export function buildGuardedCommandServer(lane: ClaudeCommandLane): McpSdkServerConfigWithInstance {
  return createSdkMcpServer({
    name: GUARDED_COMMAND_SERVER,
    version: "1.0.0",
    alwaysLoad: true,
    timeout: GUARDED_SERVER_TIMEOUT_MS,
    tools: [
      tool("bash", GUARDED_COMMAND_DESCRIPTION, {
        command: z.string().refine((value) => {
          const bytes = Buffer.byteLength(value);
          return bytes >= 1 && bytes <= MAX_COMMAND_BYTES;
        }, { message: `command must be 1 to ${String(MAX_COMMAND_BYTES)} bytes` }),
        timeout: z.number().int().min(1).max(MAX_COMMAND_TIMEOUT_MS).optional(),
        description: z.string().max(1000).optional(),
        run_in_background: z.boolean().optional(),
      }, (args, extra) => lane.handle(args, extra)),
    ],
  });
}
