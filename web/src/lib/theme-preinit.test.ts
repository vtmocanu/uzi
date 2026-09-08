// Runs the SHIPPED pre-paint script (web/public/theme-preinit.js) — a classic IIFE
// loaded blocking in <head>, deliberately NOT an importable module — against
// constructed globals, and asserts the data-theme / data-font it stamps. Read from
// disk and executed via `new Function` so the test exercises the exact bytes nginx
// serves, catching any drift from the uzi.appearance contract / polarity map in
// theme.ts. This lives under src/lib/** so it runs in the node vitest project (no
// jsdom); the document/localStorage/matchMedia globals are hand-built and injected
// as function parameters, which shadow the real globals inside the IIFE body.
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const source = readFileSync(
  path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../public/theme-preinit.js"),
  "utf8",
);

// run executes the preinit against a fresh set of globals and returns the attributes
// it stamped on document.documentElement. `storage` is the localStorage backing map
// (omit uzi.appearance / uzi.theme to model an empty cache); `osDark` is what
// matchMedia("(prefers-color-scheme: dark)") reports.
function run({ storage, osDark }: { storage: Record<string, string>; osDark: boolean }) {
  const attrs: Record<string, string> = {};
  const documentStub = {
    documentElement: {
      setAttribute(name: string, value: string) {
        attrs[name] = value;
      },
    },
  };
  const localStorageStub = {
    getItem(key: string) {
      return Object.prototype.hasOwnProperty.call(storage, key) ? storage[key] : null;
    },
  };
  const windowStub = {
    matchMedia(query: string) {
      return { matches: query.includes("dark") ? osDark : false };
    },
  };
  // The script is `(function(){ … })();` referencing bare document/localStorage/
  // window; naming them as parameters shadows the real globals for the IIFE.
  const exec = new Function("document", "localStorage", "window", source);
  exec(documentStub, localStorageStub, windowStub);
  return attrs;
}

const appearance = (over: Record<string, string> = {}) =>
  JSON.stringify({ mode: "system", light: "hall", dark: "ember", typeface: "system", ...over });

describe("theme-preinit.js pre-paint stamp (PRD #1167 m5)", () => {
  it("system + OS-light stamps the light slot (hall) and the typeface", () => {
    const attrs = run({ storage: { "uzi.appearance": appearance() }, osDark: false });
    expect(attrs["data-theme"]).toBe("hall");
    expect(attrs["data-font"]).toBe("system");
  });

  it("system + OS-dark stamps the dark slot (ember)", () => {
    const attrs = run({ storage: { "uzi.appearance": appearance() }, osDark: true });
    expect(attrs["data-theme"]).toBe("ember");
  });

  it("mode=light stamps the cached light theme regardless of the OS", () => {
    const attrs = run({
      storage: { "uzi.appearance": appearance({ mode: "light", light: "dawn", typeface: "plex" }) },
      osDark: true,
    });
    expect(attrs["data-theme"]).toBe("dawn");
    expect(attrs["data-font"]).toBe("plex");
  });

  it("mode=dark stamps the cached dark theme regardless of the OS", () => {
    const attrs = run({
      storage: { "uzi.appearance": appearance({ mode: "dark", dark: "mission" }) },
      osDark: false,
    });
    expect(attrs["data-theme"]).toBe("mission");
  });

  it("no cache + OS-light stamps hall (never leaves data-theme unset)", () => {
    const attrs = run({ storage: {}, osDark: false });
    expect(attrs["data-theme"]).toBe("hall");
    expect(attrs["data-font"]).toBe("system");
  });

  it("no cache + OS-dark stamps ember", () => {
    const attrs = run({ storage: {}, osDark: true });
    expect(attrs["data-theme"]).toBe("ember");
  });

  it("migrates a stale uzi.theme dark id (mission) into the dark slot with mode=dark", () => {
    const attrs = run({ storage: { "uzi.theme": "mission" }, osDark: false });
    expect(attrs["data-theme"]).toBe("mission");
  });
});
