import fs from "node:fs";
import { X509Certificate } from "node:crypto";

const MAX_BYTES = 1024 * 1024;
const MAX_CERTIFICATES = 64;
const captured = new WeakSet<object>();
const validated = Symbol("validated startup CA");

/** Immutable, byte-exact ASCII PEM captured from the trusted worker startup env. */
export interface ValidatedExtraCa {
  readonly [validated]: true;
  readonly pem: string;
}

/** No repo input, alternate env sources, trust merging or certificate network lookup. */
export function captureExtraCa(file: string | undefined): ValidatedExtraCa | undefined {
  if (file === undefined) return undefined;
  try {
    // Follow Secret-volume symlinks; inspect and read the same opened regular file.
    const fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NONBLOCK);
    let bytes: Buffer;
    try {
      if (!fs.fstatSync(fd).isFile()) throw new Error();
      const bounded = Buffer.alloc(MAX_BYTES + 1);
      let total = 0;
      // At most MAX_BYTES+1 successful one-byte reads, plus one EOF read.
      while (total < bounded.length) {
        const n = fs.readSync(fd, bounded, total, bounded.length - total, null);
        if (n === 0) break;
        total += n;
      }
      if (total > MAX_BYTES) throw new Error();
      bytes = bounded.subarray(0, total);
    } finally {
      fs.closeSync(fd);
    }
    const pem = bytes.toString("ascii");
    if (!Buffer.from(pem, "ascii").equals(bytes)) throw new Error();
    const block = /-----BEGIN CERTIFICATE-----\r?\n([A-Za-z0-9+/=\r\n]+)-----END CERTIFICATE-----/g;
    let end = 0;
    let count = 0;
    for (const match of pem.matchAll(block)) {
      if (!/^[ \t\r\n]*$/.test(pem.slice(end, match.index))) throw new Error();
      if (++count > MAX_CERTIFICATES) throw new Error();
      const base64 = match[1]!.replace(/[\r\n]/g, "");
      const der = Buffer.from(base64, "base64");
      if (der.toString("base64") !== base64) throw new Error();
      const certificate = new X509Certificate(match[0]);
      if (!certificate.raw.equals(der)) throw new Error();
      end = match.index + match[0].length;
    }
    if (count === 0 || !/^[ \t\r\n]*$/.test(pem.slice(end))) throw new Error();
    const bundle = Object.freeze({ [validated]: true as const, pem });
    captured.add(bundle);
    return bundle;
  } catch {
    const error = new Error("Invalid UZI_CODEX_CA_FILE configuration");
    // Never expose filesystem/OpenSSL diagnostics, source paths or certificate contents.
    error.stack = error.message;
    throw error;
  }
}

/** Refuse structural copies, including objects carrying a copied private symbol. */
export function extraCaPem(value: ValidatedExtraCa): string {
  if (!captured.has(value)) throw new Error("Untrusted Codex extra CA");
  return value.pem;
}
