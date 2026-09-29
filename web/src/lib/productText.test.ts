import { describe, it, expect } from "vitest";
import { PRODUCT_NAME_MAX_BYTES, productTextError, trimProductText } from "./productText";

describe("trimProductText mirrors Go's strings.TrimSpace", () => {
  it("strips a leading and trailing U+0085 (NEL), which String.prototype.trim keeps", () => {
    expect("Acme\u0085".trim()).toBe("Acme\u0085");
    expect(trimProductText("\u0085 Acme \u0085")).toBe("Acme");
  });

  it("keeps a leading or trailing U+FEFF, which String.prototype.trim strips but Go does not", () => {
    expect(trimProductText("Acme﻿")).toBe("Acme﻿");
  });

  it("strips ordinary and Unicode spaces and keeps interior ones", () => {
    expect(trimProductText(" \t 　Acme  Corp \n")).toBe("Acme  Corp");
  });
});

describe("productTextError", () => {
  it("accepts a name whose only control character is trailing U+0085 (the server trims it)", () => {
    expect(productTextError("Name", "Acme\u0085", PRODUCT_NAME_MAX_BYTES)).toBeNull();
  });

  it("still refuses an interior U+0085", () => {
    expect(productTextError("Name", "Ac\u0085me", PRODUCT_NAME_MAX_BYTES)).toBe(
      "Name can’t contain tabs, line breaks or invisible formatting characters.",
    );
  });

  it("refuses a trailing U+FEFF the server would keep and refuse", () => {
    expect(productTextError("Name", "Acme﻿", PRODUCT_NAME_MAX_BYTES)).toBe(
      "Name can’t contain tabs, line breaks or invisible formatting characters.",
    );
  });

  it("measures bytes after trimming", () => {
    expect(productTextError("Name", `${"a".repeat(200)}\u0085`, PRODUCT_NAME_MAX_BYTES)).toBeNull();
    expect(productTextError("Name", "a".repeat(201), PRODUCT_NAME_MAX_BYTES)).toMatch(
      /^Name is 201 bytes; the limit is 200\./,
    );
  });
});
