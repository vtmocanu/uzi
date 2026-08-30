import { describe, it, expect } from "vitest";
import { licenseCreditEnabled } from "./flags";

// Pins the SHIPPED default of the build-time license-credit flag. Every other test
// auto-mocks ../lib/flags, so nothing else protects the real value; this file
// imports the REAL module and asserts the credit is OFF by default. The flag is
// module-private (SHOW_LICENSE_CREDIT), read only through licenseCreditEnabled(), so
// this asserts via the exported function rather than the const.
describe("licenseCreditEnabled — shipped default", () => {
  it("is OFF (license credit hidden) unless flipped and rebuilt", () => {
    expect(licenseCreditEnabled()).toBe(false);
  });
});
