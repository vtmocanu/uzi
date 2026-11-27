// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NAV_SECTIONS } from "../components/navItems";
import { parseModuleList } from "./manifest";
import { mergeModuleNav } from "./nav";

const valid = {
  id: "demo",
  version: "1.2.3",
  page: "Page",
  label: "Demo",
  icon: "package",
  navGroup: "work",
};

let errors: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  errors = vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => vi.restoreAllMocks());

const logged = () => errors.mock.calls.map((c: unknown[]) => String(c[0]));

describe("parseModuleList", () => {
  it("keeps a valid entry, defaults adminOnly to false and ignores unknown fields", () => {
    const out = parseModuleList([{ ...valid, upstream: "svc:80", future: 1 }]);
    expect(out).toEqual([{ ...valid, adminOnly: false }]);
    expect(errors).not.toHaveBeenCalled();
  });

  it("keeps adminOnly true", () => {
    expect(parseModuleList([{ ...valid, adminOnly: true }])[0].adminOnly).toBe(true);
  });

  it.each(["", "Demo", "1demo", "has_underscore", "a".repeat(33), "x y"])("skips invalid id %j with one log", (id) => {
    expect(parseModuleList([{ ...valid, id }])).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
  });

  it("accepts a 32-character id", () => {
    expect(parseModuleList([{ ...valid, id: "a".repeat(32) }])).toHaveLength(1);
  });

  it.each(["version", "page", "label", "icon", "navGroup"])("skips an entry missing %s with one log naming it", (field) => {
    const e: Record<string, unknown> = { ...valid };
    delete e[field];
    expect(parseModuleList([e])).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
    expect(logged()[0]).toContain("demo");
    expect(logged()[0]).toContain(field);
  });

  it("bounds the label to 1..40 characters", () => {
    expect(parseModuleList([{ ...valid, label: "x".repeat(40) }])).toHaveLength(1);
    expect(errors).not.toHaveBeenCalled();
    expect(parseModuleList([{ ...valid, label: "x".repeat(41) }])).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
  });

  it.each(["label", "version", "page"])("skips a %s with control or invisible characters, one log each", (field) => {
    for (const bad of ["a\u0000b", "a\tb", "a\u0085b", "a\u200bb", "a\u200fb", "a\u202eb", "a\u2066b", "a\u2069b", "a\u2028b", "a\u2029b", "a\u061cb", "a\ufeffb", "a\u2060b", "a\u2062b", "a\u2064b", "a\u00adb", "a\u180eb"]) {
      errors.mockClear();
      expect(parseModuleList([{ ...valid, [field]: bad }])).toEqual([]);
      expect(errors).toHaveBeenCalledTimes(1);
      expect(logged()[0]).toContain(field);
    }
  });

  it.each(["label", "version"])("skips a whitespace-only %s", (field) => {
    expect(parseModuleList([{ ...valid, [field]: "   " }])).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
  });

  it("skips an unknown icon with one log", () => {
    expect(parseModuleList([{ ...valid, icon: "rocket" }])).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
    expect(logged()[0]).toContain("icon");
  });

  it("keeps the first of a duplicated id and logs once for the later one", () => {
    const out = parseModuleList([valid, { ...valid, label: "Second" }]);
    expect(out.map((m) => m.label)).toEqual(["Demo"]);
    expect(errors).toHaveBeenCalledTimes(1);
    expect(logged()[0]).toContain("duplicate");
  });

  it("does not count a skipped entry as the first of its id", () => {
    const out = parseModuleList([{ ...valid, icon: "rocket" }, { ...valid, label: "Second" }]);
    expect(out.map((m) => m.label)).toEqual(["Second"]);
    expect(errors).toHaveBeenCalledTimes(1);
  });

  it.each(["yes", 1, null])("skips a non-boolean adminOnly %j with one log", (adminOnly) => {
    expect(parseModuleList([{ ...valid, adminOnly }])).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
    expect(logged()[0]).toContain("adminOnly");
  });

  it("maps an unknown navGroup to modules, keeps the entry and logs once", () => {
    const out = parseModuleList([{ ...valid, navGroup: "sidebar" }]);
    expect(out).toHaveLength(1);
    expect(out[0].navGroup).toBe("modules");
    expect(errors).toHaveBeenCalledTimes(1);
    expect(logged()[0]).toContain("navGroup");
  });

  it("logs once per skipped entry and keeps the good ones", () => {
    const out = parseModuleList([valid, { nope: true }, "text", { ...valid, id: "second" }]);
    expect(out.map((m) => m.id)).toEqual(["demo", "second"]);
    expect(errors).toHaveBeenCalledTimes(2);
  });

  it.each([{}, "x", 3, null])("treats a non-array %j as no modules with one log", (body) => {
    expect(parseModuleList(body)).toEqual([]);
    expect(errors).toHaveBeenCalledTimes(1);
  });

  describe("navSection", () => {
    const mods = { ...valid, navGroup: "modules" };

    it.each([
      ["Tools", "Tools"],
      ["Tools ", "Tools"],
      ["é".repeat(24), "é".repeat(24)],
      ["  " + "x".repeat(24) + "\u3000", "x".repeat(24)],
      ["\u00a0Tools\u00a0", "Tools"],
    ])("accepts %j as %j", (raw, want) => {
      const out = parseModuleList([{ ...mods, navSection: raw }]);
      expect(out[0].navSection).toBe(want);
      expect(errors).not.toHaveBeenCalled();
    });

    it("merges trimmed-equal values into one section", () => {
      const out = parseModuleList([
        { ...mods, id: "a", navSection: "Tools" },
        { ...mods, id: "b", navSection: "Tools " },
      ]);
      const merged = mergeModuleNav(NAV_SECTIONS, out);
      const own = merged.filter((s) => s.id.startsWith("modules"));
      expect(own.map((s) => s.id)).toEqual(["modules:Tools"]);
      expect(own[0].group).toBe("Tools");
      expect(own[0].items.map((i) => i.key)).toEqual(["module:a", "module:b"]);
    });

    const LENGTH = "navSection must be 1 to 24 characters after trimming whitespace";
    const UNSAFE = "navSection contains control or invisible formatting characters";
    it.each([
      ["blank", "   ", LENGTH],
      ["25 code points", "é".repeat(25), LENGTH],
      ["25 astral code points", "😀".repeat(25), LENGTH],
      ["trailing newline (trim would remove it)", "Tools\n", UNSAFE],
      ["trailing line separator (trim would remove it)", "Tools ", UNSAFE],
      ["leading BOM (trim would remove it)", "﻿Tools", UNSAFE],
      ["bidi", "\u202eX", UNSAFE],
      ["control", "a\u0000b", UNSAFE],
      ["lone surrogate", "\ud800", UNSAFE],
      ["number", 7, "navSection must be a string"],
      ["null", null, "navSection must be a string"],
    ])("drops an invalid navSection (%s) with one log and keeps the entry", (_n, navSection, reason) => {
      const out = parseModuleList([{ ...mods, navSection }]);
      expect(out).toHaveLength(1);
      expect("navSection" in out[0]).toBe(false);
      expect(errors).toHaveBeenCalledTimes(1);
      expect(logged()[0]).toContain("demo");
      expect(logged()[0]).toContain(reason);
      expect(logged()[0].endsWith('; using "Modules"')).toBe(true);
    });

    it("counts code points, not UTF-16 units: 24 astral characters pass", () => {
      const s = "😀".repeat(24);
      const out = parseModuleList([{ ...mods, navSection: s }]);
      expect(out[0].navSection).toBe(s);
      expect(errors).not.toHaveBeenCalled();
    });

    it.each(["work", "factory"])("ignores navSection on %s silently", (navGroup) => {
      const out = parseModuleList([{ ...valid, navGroup, navSection: "Tools" }]);
      expect(out).toHaveLength(1);
      expect("navSection" in out[0]).toBe(false);
      expect(errors).not.toHaveBeenCalled();
    });

    it("ignores navSection on an unknown navGroup, with only the navGroup warning", () => {
      const out = parseModuleList([{ ...valid, navGroup: "sidebar", navSection: "Tools" }]);
      expect(out).toHaveLength(1);
      expect(out[0].navGroup).toBe("modules");
      expect("navSection" in out[0]).toBe(false);
      expect(errors).toHaveBeenCalledTimes(1);
      expect(logged()[0]).toContain("navGroup");
      const merged = mergeModuleNav(NAV_SECTIONS, out);
      const ids = merged.map((s) => s.id);
      expect(ids).toContain("modules");
      expect(ids.some((id) => id.startsWith("modules:"))).toBe(false);
      const def = merged.find((s) => s.id === "modules")!;
      expect(def.group).toBe("Modules");
      expect(def.items.map((i) => i.key)).toEqual([`module:${out[0].id}`]);
    });
  });
});
