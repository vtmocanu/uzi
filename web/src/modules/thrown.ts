// Safe text for a thrown or remote-supplied value. `String(x)` itself throws for
// `Object.create(null)` and for an object whose toString throws; a conversion that throws inside an error path would
// escape the module's own boundary and reach the router's.

const UNPRINTABLE_THROWN = "non-Error value thrown";

export function describeThrown(value: unknown): string {
  if (value instanceof Error) {
    try {
      return String(value.message);
    } catch {
      return UNPRINTABLE_THROWN;
    }
  }
  try {
    return String(value);
  } catch {
    return UNPRINTABLE_THROWN;
  }
}
