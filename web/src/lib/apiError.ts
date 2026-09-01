// ApiError lives in its own leaf module (not the `api.ts` barrel) so the
// mock-mode client `mocks/mockApi.ts` can import it as a runtime value without
// creating a runtime import cycle with `lib/api.ts` — the cycle
// (api → mockApi → api) that caused the vitest `No "ApiError" export is defined
// on the "../lib/api" mock` flake (issue #165). It needs no imports.

export class ApiError extends Error {
  status: number;
  // body is the full parsed error payload, so a caller can read structured
  // fields beyond the message (e.g. a 422's `violations` array).
  body: unknown;
  constructor(status: number, message: string, body: unknown = null) {
    super(message);
    this.status = status;
    this.body = body;
    this.name = "ApiError";
  }
}

// errorMessage pulls the server-supplied message off an ApiError and otherwise
// returns the caller's fallback — the one message-extraction idiom every catch site shares.
export function errorMessage(err: unknown, fallback: string): string {
  return err instanceof ApiError ? err.message : fallback;
}
