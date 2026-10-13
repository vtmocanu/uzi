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

function usage(message) {
  process.stderr.write(`${message}\n` +
    "usage: retained-source.mjs inspect [--run <id> --generation <n|none> --attempt <id|canonical>] [--socket <path>]\n" +
    "       retained-source.mjs discard --run <id> --generation <n|none> --attempt <id|canonical> --confirm <digest> [--protected] [--socket <path>]\n");
  process.exit(2);
}

function parseArgs(argv) {
  const [op, ...rest] = argv;
  if (op !== "inspect" && op !== "discard") usage("the first argument must be inspect or discard");
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
conn.setTimeout(IDLE_TIMEOUT_MS, () => fail("timed out waiting for the worker"));
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
