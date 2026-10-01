import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

// Issue #2044: a unix socket path is capped by sockaddr_un.sun_path (108 bytes on Linux, 104 on
// macOS, both including the NUL). In a Codex run the test TMPDIR is already 83 bytes: the
// command's /tmp/uzi-codex-command-<uuid> plus tmpdir-leak-guard's /uzi-tmpdir-guard.XXXXXX.
// So a fake-Docker socket built under os.tmpdir() overflows it: listen() emits EINVAL, and a bare
// `server.listen(socket, resolve)` has no error handler, so the test hangs to the runner timeout.
// Fixtures take a short socket path from shortUnixSocket and bind through listenUnix, which fails
// fast and names the path.
const UNIX_SOCKET_PATH_MAX_BYTES = 103;

function assertSocketPathFits(socket: string): void {
  const bytes = Buffer.byteLength(socket);
  if (bytes > UNIX_SOCKET_PATH_MAX_BYTES) {
    throw new Error(`unix socket path is ${bytes} bytes, over the ${UNIX_SOCKET_PATH_MAX_BYTES}-byte limit: ${socket}`);
  }
}

/** A socket path in its own fresh short directory under `base`; `dispose` removes that directory. */
export function shortUnixSocket(base = os.tmpdir()): { socket: string; dispose: () => void } {
  const dir = fs.mkdtempSync(path.join(base, "us-"));
  const dispose = (): void => fs.rmSync(dir, { recursive: true, force: true });
  const socket = path.join(dir, "d.sock");
  try {
    assertSocketPathFits(socket);
  } catch (err) {
    dispose();
    throw err;
  }
  return { socket, dispose };
}

/** Bind `server` to a unix socket; rejects on a listen error or an over-long path instead of hanging. */
export function listenUnix(server: net.Server, socket: string): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    try {
      assertSocketPathFits(socket);
    } catch (err) {
      reject(err);
      return;
    }
    server.once("error", reject);
    server.listen(socket, () => {
      server.off("error", reject);
      resolve();
    });
  });
}
