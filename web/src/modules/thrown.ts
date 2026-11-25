// Safe text for a thrown or remote-supplied value. `String(x)` itself throws for
// `Object.create(null)`, for an object whose toString throws and for a revoked Proxy; a conversion that throws inside an error path would
// escape the module's own boundary and reach the router's.

const UNPRINTABLE_THROWN = "non-Error value thrown";

// Every operation on the value, `instanceof` included, sits inside the try: a revoked Proxy
// throws from `instanceof` itself, and a getter-backed `message` throws on read.
export function describeThrown(value: unknown): string {
  try {
    return String(value instanceof Error ? value.message : value);
  } catch {
    return UNPRINTABLE_THROWN;
  }
}
