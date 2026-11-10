// issue #1786 M0b — the operator client of the worker's retained-source channel.
//
// Self-contained on purpose: it imports ONLY `node:` builtins, so it runs with the image's
// production dependencies and a scrubbed environment, never through tsx or node_modules:
//
//   env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /app/src/ops/retained-source.mjs \
//     inspect --run <uuid> --generation <n|none> --attempt <id|canonical>
//   env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /app/src/ops/retained-source.mjs \
//     discard --run <uuid> --generation <n|none> --attempt <id|canonical> --confirm <digest> [--protected]
//
//   ... retained-source.mjs local-records --run <id>
//   ... retained-source.mjs resolve-local-record --run <id> --generation <n> --confirm <digest> [--case-b]
//
// --case-b (resolve-local-record only, a flag without a value) asks the coordinator for the Case B
// disposition; it must match the class local-records reported for that generation.
//
// It connects to the worker's Unix socket, sends ONE NDJSON request line, prints the response line
// and exits non-zero on a refusal, an unknown outcome or any error. It has no filesystem logic.
//
// The protocol constants below are duplicated from retained-source-server.ts; the test
// retained-source-client-packaging.test.ts asserts they agree.
import net from "node:net";
import process from "node:process";
import path from "node:path";

const REQUEST_MAX_BYTES = 8192;
const RESPONSE_MAX_BYTES = 65536;
const SOCKET_RELATIVE = "ops/retained-source.sock";
const DEFAULT_DATA_DIR = "/data";
const IDLE_TIMEOUT_MS = 120000;
// Test-only override of the idle timeout. Production runs this client under `env -i`, so the variable
// is never set there and the default applies.
const idleOverride = Number(process.env.UZI_RETAINED_SOURCE_IDLE_TIMEOUT_MS);
const idleTimeoutMs = Number.isSafeInteger(idleOverride) && idleOverride > 0 ? idleOverride : IDLE_TIMEOUT_MS;

function usage(message) {
  process.stderr.write(`${message}\n` +
    "usage: retained-source.mjs inspect [--run <id> --generation <n|none> --attempt <id|canonical>] [--socket <path>]\n" +
    "       retained-source.mjs discard --run <id> --generation <n|none> --attempt <id|canonical> --confirm <digest> [--protected] [--socket <path>]\n" +
    "       retained-source.mjs local-records --run <id> [--socket <path>]\n" +
    "       retained-source.mjs resolve-local-record --run <id> --generation <n> --confirm <digest> [--case-b] [--socket <path>]\n");
  process.exit(2);
}

function parseLocalRecordArgs(op, rest) {
  const flags = {};
  for (let i = 0; i < rest.length; i++) {
    const arg = rest[i];
    if (arg === "--case-b") {
      if (op !== "resolve-local-record") usage("--case-b applies to resolve-local-record only");
      flags.caseB = true;
      continue;
    }
    if (!["--run", "--generation", "--confirm", "--socket"].includes(arg)) usage(`unknown argument ${arg}`);
    const value = rest[++i];
    if (value === undefined || value.startsWith("--")) usage(`${arg} needs a value`);
    flags[arg.slice(2)] = value;
  }
  if (flags.run === undefined) usage(`${op} needs --run`);
  const request = { op, run: flags.run };
  if (op === "resolve-local-record") {
    if (flags.generation === undefined || flags.confirm === undefined) usage("resolve-local-record needs --generation and --confirm <digest from local-records>");
    if (!/^[0-9]+$/.test(flags.generation) || !Number.isSafeInteger(Number(flags.generation)) || Number(flags.generation) < 1) usage("--generation must be a positive integer");
    request.generation = Number(flags.generation);
    request.confirm = flags.confirm;
    if (flags.caseB) request.action = "case_b";
  } else if (flags.generation !== undefined || flags.confirm !== undefined) {
    usage("--generation and --confirm apply to resolve-local-record only");
  }
  return { request, socket: flags.socket ?? path.join(DEFAULT_DATA_DIR, SOCKET_RELATIVE) };
}

function parseArgs(argv) {
  const [op, ...rest] = argv;
  if (op === "local-records" || op === "resolve-local-record") return parseLocalRecordArgs(op, rest);
  if (op !== "inspect" && op !== "discard") usage("the first argument must be inspect, discard, local-records or resolve-local-record");
  const flags = { protected: false };
  for (let i = 0; i < rest.length; i++) {
    const arg = rest[i];
    if (arg === "--protected") {
      flags.protected = true;
      continue;
    }
    if (!["--run", "--generation", "--attempt", "--confirm", "--socket"].includes(arg)) usage(`unknown argument ${arg}`);
    const value = rest[++i];
    if (value === undefined || value.startsWith("--")) usage(`${arg} needs a value`);
    flags[arg.slice(2)] = value;
  }
  const request = { op };
  const selected = [flags.run, flags.generation, flags.attempt].filter((v) => v !== undefined).length;
  if (selected !== 0 && selected !== 3) usage("--run, --generation and --attempt go together");
  if (op === "discard" && selected !== 3) usage("discard needs --run, --generation and --attempt");
  if (selected === 3) {
    request.run = flags.run;
    if (flags.generation === "none") request.generation = "none";
    else if (/^[0-9]+$/.test(flags.generation) && Number.isSafeInteger(Number(flags.generation))) request.generation = Number(flags.generation);
    else usage("--generation must be a non-negative integer or none");
    request.attempt = flags.attempt;
  }
  if (op === "discard") {
    if (flags.confirm === undefined) usage("discard needs --confirm <digest from inspect>");
    request.confirm = flags.confirm;
    if (flags.protected) request.protected = true;
  } else if (flags.confirm !== undefined || flags.protected) {
    usage("--confirm and --protected apply to discard only");
  }
  return { request, socket: flags.socket ?? path.join(DEFAULT_DATA_DIR, SOCKET_RELATIVE) };
}

function fail(message) {
  process.stderr.write(`retained-source: ${message}\n`);
  process.exit(1);
}

const { request, socket: socketPath } = parseArgs(process.argv.slice(2));
const line = `${JSON.stringify(request)}\n`;
if (Buffer.byteLength(line) > REQUEST_MAX_BYTES) usage("the request is too large");

const chunks = [];
let received = 0;
const conn = net.createConnection(socketPath);
conn.setTimeout(idleTimeoutMs, () => fail(request.op === "resolve-local-record"
  ? "timed out waiting for the worker; the resolution outcome is unknown: re-inspect (local-records / inspect) before retrying"
  : "timed out waiting for the worker"));
conn.on("error", (err) => fail(`cannot reach the worker channel at ${socketPath}: ${err.code ?? err.message}`));
conn.on("connect", () => conn.write(line));
conn.on("data", (chunk) => {
  received += chunk.length;
  if (received > RESPONSE_MAX_BYTES) fail("the response exceeds the size limit");
  chunks.push(chunk);
});
conn.on("end", () => {
  const text = Buffer.concat(chunks).toString("utf8");
  if (text.trim() === "") fail("the worker closed the connection without a response");
  process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
  let response;
  try {
    response = JSON.parse(text);
  } catch {
    fail("the response is not JSON");
  }
  // inspect succeeded ⇒ ok; discard is ok only when the source was removed.
  process.exit(response && response.ok === true ? 0 : 1);
});
