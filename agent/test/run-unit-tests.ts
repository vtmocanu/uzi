import { spawn } from "node:child_process";
import { readFileSync, readdirSync } from "node:fs";
import { constants } from "node:os";
import { fileURLToPath } from "node:url";
import { selectAgentTests } from "./agent-test-shards.js";

function reportError(error: unknown): void {
  const message = error instanceof Error ? error.message : String(error);
  // JSON escaping handles C0/ANSI bytes; also escape C1, line and bidi controls.
  const diagnostic = JSON.stringify(message).slice(1, -1)
    .replace(/[\u007f-\u009f\u061c\u200e\u200f\u2028-\u202e\u2066-\u2069]/g,
      (character) => "\\u" + character.charCodeAt(0).toString(16).padStart(4, "0"));
  console.error(diagnostic);
}

function filters(args: string[]): string[] {
  const result: string[] = [];
  for (let index = 0; index < args.length; index++) {
    const arg = args[index]!;
    if (arg === "--test-only") {
      result.push(arg);
      continue;
    }
    const match = /^(--test-(?:name|skip)-pattern)(?:=(.*))?$/.exec(arg);
    if (!match) throw new Error(`Unsupported test argument: ${arg}`);
    const value = match[2] ?? args[++index];
    if (value === undefined || (match[2] === undefined && value.startsWith("--"))) {
      throw new Error(`Missing pattern for ${match[1]}`);
    }
    new RegExp(value); // Reject malformed expressions before spawning.
    result.push(`${match[1]}=${value}`);
  }
  return result;
}

function main(): void {
  const root = fileURLToPath(new URL("../", import.meta.url));
  const inventory = readdirSync(new URL("./", import.meta.url), { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".test.ts"))
    .map((entry) => `test/${entry.name}`);
  const pins: unknown = JSON.parse(readFileSync(new URL("../test-shards.json", import.meta.url), "utf8"));
  const selected = selectAgentTests(inventory, pins, process.env.UZI_AGENT_TEST_SHARD || undefined);
  const args = ["--import", "tsx", "--import", "./test/setup/hermetic-proc.ts", "--test"];
  const concurrency = process.env.UZI_AGENT_TEST_CONCURRENCY;
  if (concurrency) {
    if (!/^[1-9]\d*$/.test(concurrency) || !Number.isSafeInteger(Number(concurrency))) {
      throw new Error("UZI_AGENT_TEST_CONCURRENCY must be a positive safe integer");
    }
    args.push(`--test-concurrency=${concurrency}`);
  }
  const junit = process.env.UZI_AGENT_TEST_JUNIT;
  if (junit) {
    args.push("--test-reporter=spec", "--test-reporter-destination=stdout",
      "--test-reporter=junit", `--test-reporter-destination=${junit}`);
  }
  args.push("--test-timeout=120000", ...filters(process.argv.slice(2)), ...selected);
  const child = spawn(process.execPath, args, { cwd: root, env: process.env, stdio: "inherit" });
  let interrupted: NodeJS.Signals | undefined;
  let escalation: NodeJS.Timeout | undefined;
  // Only this owned child is signalled. One grace period bounds cleanup; a
  // second signal escalates immediately. Completion always waits for close.
  const forward = (signal: NodeJS.Signals): void => {
    if (interrupted) {
      child.kill("SIGKILL");
      return;
    }
    interrupted = signal;
    child.kill(signal);
    escalation = setTimeout(() => child.kill("SIGKILL"), 2_000);
  };
  const onInt = (): void => forward("SIGINT");
  const onTerm = (): void => forward("SIGTERM");
  process.on("SIGINT", onInt);
  process.on("SIGTERM", onTerm);
  child.on("error", (error) => {
    reportError(error);
    process.exitCode = 1;
  });
  child.on("close", (code, signal) => {
    if (escalation) clearTimeout(escalation);
    process.off("SIGINT", onInt);
    process.off("SIGTERM", onTerm);
    const termination = interrupted ?? signal;
    process.exitCode = termination ? 128 + constants.signals[termination] : (code ?? 1);
    // Restore default signal handling only after the owned child has closed.
    if (termination) process.kill(process.pid, termination);
  });
}

try {
  main();
} catch (error) {
  reportError(error);
  process.exitCode = 1;
}
