import { Agent, EnvHttpProxyAgent, type Dispatcher } from "undici";

// NODE_OPTIONS uses spaces and double quotes, with backslash escapes only inside double quotes
// (Node v26 src/node_options.cc, ParseNodeOptionsEnvVar). It is not a shell command.
function nodeOptionsArgs(text: string): string[] {
  const args: string[] = [];
  let quoted = false, start = true;
  for (let i = 0; i < text.length; i++) {
    let char = text[i]!;
    if (char === "\\" && quoted) {
      if (++i === text.length) throw new Error("invalid NODE_OPTIONS escape");
      char = text[i]!;
    } else if (char === " " && !quoted) { start = true; continue; }
    else if (char === '"') { quoted = !quoted; continue; }
    if (start) { args.push(char); start = false; }
    else args[args.length - 1] += char;
  }
  if (quoted) throw new Error("unterminated NODE_OPTIONS quote");
  return args;
}

// Node CLI precedence: environment default, then NODE_OPTIONS, then command-line singleton flags.
// https://nodejs.org/api/cli.html#node_optionsoptions
// Boolean flags accept a --no- form; Node treats --use-env-proxy=false as enabled, too.
const envProxyEnabled = (() => {
  let enabled = process.env.NODE_USE_ENV_PROXY === "1";
  for (const arg of [...nodeOptionsArgs(process.env.NODE_OPTIONS ?? ""), ...process.execArgv]) {
    const name = arg.split("=", 1)[0]!.replaceAll("_", "-");
    if (name === "--use-env-proxy") enabled = true;
    else if (name === "--no-use-env-proxy") enabled = false;
  }
  return enabled;
})();

/** Own one publish's connections through its acknowledgement body. Native Node 26 fetch and the
 * pinned undici use Dispatcher API v2, covered by a real bundled-fetch compatibility test.
 * Dispatcher1Wrapper forces HTTP/1.1, so it must not wrap this dispatcher. TLS retains Node's
 * default trust configuration, including NODE_EXTRA_CA_CERTS and system/OpenSSL CA options.
 * Mirror Node's documented startup proxy opt-in without enabling proxies merely because HTTP_PROXY
 * is set. Runtime http.setGlobalProxyFromEnv() overrides are not tracked; the worker has no such
 * caller. Other RPCs retain their existing dispatcher and timeouts. */
export function checkpointPublishDispatcher(timeoutMs: number): Dispatcher {
  const options = { headersTimeout: timeoutMs, bodyTimeout: timeoutMs };
  const owned = envProxyEnabled ? new EnvHttpProxyAgent() : new Agent();
  // Set per-dispatch timeouts: the proxy's HTTP forwarding Client does not inherit Agent
  // constructor timeout defaults. The composed dispatcher owns the same lifecycle.
  return owned.compose(dispatch => (opts, handler) => dispatch({ ...opts, ...options }, handler));
}
