import { describe, it, expect } from "vitest";
import {
  ABSOLUTE_MAX,
  BUILTIN_WORKER_SIZES,
  BUILTIN_WORKER_SIZE_NAMES,
  cpuCores,
  MAX_WORKER_SIZES,
  memoryGiB,
  memoryQuantity,
  newSizeNameError,
  parseCores,
  parseDiskGiB,
  parseMemoryGiB,
  SIZE_NAME_PATTERN,
  sizeChoices,
  sizeOptionLabel,
  sizeSummary,
} from "./workerSizes";

// The drift gates for the builtin size seed (PRD #58, reshaped by PRD #4213).
//
// Sizes are admin-editable data since PRD #4213: the live list and its integers come from
// the api, so nothing here mirrors a live table any more. What is left to pin is the
// BUILTIN seed the web falls back to (an older api with no `sizes`, and the names a
// default-size setting accepts) and the formatting of the integers:
//
//   * api/internal/hostedsvc/testdata/hosted_sizes.json is the api's builtin seed, name
//     and six integers. BUILTIN_WORKER_SIZES must equal it exactly.
//   * controller/internal/preset/testdata/hosted_size_specs.json is how the controller
//     spells each builtin limit as a k8s quantity. Formatting the seed integers must
//     produce those spellings, which is what keeps every builtin label identical to the
//     pre-#4213 strings and to the pod.
//
// Reaching into another module's tree from a web test mirrors
// agent/test/claim-skills-contract.test.ts, and the same caveat applies: this catches
// DEV-TIME drift, never deployment skew.
//
// The `?raw` import, not node:fs, and that is load-bearing. web/Dockerfile copies web/
// and docs/ and nothing else, so a shipped source file that imported either golden
// would build locally and break the image. `?raw` matches vite/client's ambient
// wildcard module declaration, so tsc never resolves the path — `npm run build` inside
// the image type-checks this file clean with api/ and controller/ absent, while vitest
// (which runs from a full checkout) resolves it for real and the assertion has teeth.
// node:fs would need @types/node, which this browser project deliberately lacks.
//
// The flip side of tsc not resolving these paths is that tsc cannot VALIDATE them
// either. Move or rename a golden and `npm run typecheck` stays green (measured: it
// exits 0); only `npm test` tells you, and it fails as a bare ENOENT that takes the
// whole suite down before any assertion runs — so you get no drift message, just a
// missing file. CI runs the web tests from a full checkout, so the gate always fires;
// simply do not expect the compiler to have your back on these two lines.
import namesRaw from "../../../api/internal/hostedsvc/testdata/hosted_sizes.json?raw";
import specsRaw from "../../../controller/internal/preset/testdata/hosted_size_specs.json?raw";
// Same ?raw rationale: the api's absolute caps, read from their Go source so ABSOLUTE_MAX
// cannot drift from them silently.
import builtinGoRaw from "../../../api/internal/workersize/builtin/builtin.go?raw";

const NAMES_GOLDEN = "api/internal/hostedsvc/testdata/hosted_sizes.json";
const SPECS_GOLDEN = "controller/internal/preset/testdata/hosted_size_specs.json";

type SpecEntry = {
  name: string;
  cpu_request: string;
  cpu_limit: string;
  memory_request: string;
  memory_limit: string;
  data: string;
};

// hosted_sizes.json is the builtin seed contract (PRD #4213): name plus the six integers.
type SeedEntry = { name: string; cpu_request_millicores: number; cpu_limit_millicores: number; memory_request_mib: number; memory_limit_mib: number; data_gib: number; dind_data_gib: number };
const seed = (JSON.parse(namesRaw) as { sizes: SeedEntry[] }).sizes;
const specs = JSON.parse(specsRaw) as { sizes: SpecEntry[]; nix: string };

describe("ABSOLUTE_MAX mirrors the api's absolute caps", () => {
  const goConst = (name: string): number => {
    const m = new RegExp(`\\b${name}\\s+int64\\s*=\\s*(\\d+)`).exec(builtinGoRaw);
    if (!m) throw new Error(`${name} not found in api/internal/workersize/builtin/builtin.go`);
    return Number(m[1]);
  };
  it("equals MaxMillicores / MaxMemoryMiB / MaxDiskGiB in builtin.go", () => {
    expect(ABSOLUTE_MAX).toEqual({
      millicores: goConst("MaxMillicores"),
      memoryMiB: goConst("MaxMemoryMiB"),
      diskGiB: goConst("MaxDiskGiB"),
    });
  });
});

describe("BUILTIN_WORKER_SIZES mirrors the api's builtin seed", () => {
  it("matches the cross-module golden, integer for integer and in order", () => {
    expect(
      BUILTIN_WORKER_SIZES.map((s) => ({ ...s })),
      `BUILTIN_WORKER_SIZES (web/src/lib/workerSizes.ts) drifted from ${NAMES_GOLDEN}.\n` +
        "The golden is generated from the api's builtin seed, which is the authority: if the " +
        "seed deliberately changed, copy its integers here. The live list always comes from " +
        "the api; this constant is only the fallback for an older api and for the default-size " +
        "selectors while the size list cannot be read.",
    ).toEqual(seed);
    expect([...BUILTIN_WORKER_SIZE_NAMES]).toEqual(seed.map((s) => s.name));
  });
});

describe("the builtin integers format to the controller's quantities", () => {
  it("the specs golden covers exactly the api's sizes, in the same order", () => {
    expect(
      specs.sizes.map((s) => s.name),
      `${SPECS_GOLDEN} and ${NAMES_GOLDEN} disagree: one module's table changed and its golden was not regenerated.`,
    ).toEqual(seed.map((s) => s.name));
  });

  it("renders each builtin limit exactly as the controller spells it", () => {
    for (const entry of specs.sizes) {
      const size = BUILTIN_WORKER_SIZES.find((s) => s.name === entry.name)!;
      expect(
        { cpuLimit: cpuCores(size.cpu_limit_millicores), memoryLimit: memoryQuantity(size.memory_limit_mib), data: `${size.data_gib}Gi` },
        `the ${entry.name} label drifted from ${SPECS_GOLDEN}: a size label that disagrees with the pod is believed by the user.`,
      ).toEqual({ cpuLimit: entry.cpu_limit, memoryLimit: entry.memory_limit, data: entry.data });
    }
  });

  it("carries no /nix — it is flat across every size and cannot inform a choice", () => {
    expect(specs.nix).toBeTruthy();
    for (const entry of specs.sizes) expect(Object.keys(entry)).not.toContain("nix");
  });
});

describe("the picker's option text", () => {
  const byName = (n: string) => BUILTIN_WORKER_SIZES.find((s) => s.name === n)!;

  it("keeps the builtin labels byte-identical to the pre-#4213 strings", () => {
    expect(BUILTIN_WORKER_SIZES.map(sizeOptionLabel)).toEqual([
      "S — up to 1 CPU / 4Gi RAM / 5Gi disk",
      "M — up to 4 CPU / 12Gi RAM / 25Gi disk",
      "L — up to 4 CPU / 20Gi RAM / 50Gi disk",
      "L-HIGHMEM — up to 4 CPU / 32Gi RAM / 50Gi disk",
    ]);
    expect(sizeSummary(byName("s"))).toBe("up to 1 CPU / 4Gi RAM / 5Gi disk");
  });

  it("shows an edited size's own quantities, never the builtin ones", () => {
    const edited = { ...byName("m"), cpu_limit_millicores: 2500, memory_limit_mib: 1536, data_gib: 30 };
    expect(sizeOptionLabel(edited)).toBe("M — up to 2.5 CPU / 1536Mi RAM / 30Gi disk");
  });

  it("falls back to the builtins only when the api sent no list", () => {
    expect(sizeChoices(undefined)).toBe(BUILTIN_WORKER_SIZES);
    expect(sizeChoices(null)).toBe(BUILTIN_WORKER_SIZES);
    const live = [{ ...byName("s"), name: "s" }];
    expect(sizeChoices(live)).toBe(live);
    expect(sizeChoices([])).toEqual([]);
  });
});

describe("human units", () => {
  it("formats CPU and memory exactly", () => {
    expect([cpuCores(250), cpuCores(1000), cpuCores(4000), cpuCores(1500), cpuCores(1)]).toEqual(["0.25", "1", "4", "1.5", "0.001"]);
    expect([memoryGiB(4096), memoryGiB(1536), memoryGiB(1000)]).toEqual(["4", "1.5", "0.9765625"]);
    expect([memoryQuantity(4096), memoryQuantity(1536), memoryQuantity(512)]).toEqual(["4Gi", "1536Mi", "512Mi"]);
  });

  it("parses cores to exact millicores, refusing more than 3 decimal places rather than rounding", () => {
    expect(parseCores("0.25")).toEqual({ ok: true, value: 250 });
    expect(parseCores("4")).toEqual({ ok: true, value: 4000 });
    expect(parseCores(" 4 ")).toEqual({ ok: true, value: 4000 });
    expect(parseCores("0.001")).toEqual({ ok: true, value: 1 });
    // 0.1 + 0.2 style float drift cannot reach the digit-string parse.
    expect(parseCores("4.105")).toEqual({ ok: true, value: 4105 });
    const tooFine = { ok: false, error: "Use at most 3 decimal places (millicores)" };
    expect(parseCores("1.0005")).toEqual(tooFine);
    expect(parseCores("0.0005")).toEqual(tooFine);
    expect(parseCores("0.000")).toEqual({ ok: false, error: "Must be more than 0 CPU" });
    expect(parseCores("0").ok).toBe(false);
    expect(parseCores("-1").ok).toBe(false);
    expect(parseCores("two").ok).toBe(false);
    expect(parseCores("").ok).toBe(false);
  });

  it("parses GiB to MiB only when the result is a whole MiB", () => {
    expect(parseMemoryGiB("1.5")).toEqual({ ok: true, value: 1536 });
    expect(parseMemoryGiB("32")).toEqual({ ok: true, value: 32768 });
    expect(parseMemoryGiB("0.9765625")).toEqual({ ok: true, value: 1000 });
    expect(parseMemoryGiB("0.0009765625")).toEqual({ ok: true, value: 1 });
    expect(parseMemoryGiB("0.1")).toEqual({ ok: false, error: "Must be a whole number of MiB (GiB × 1024)" });
    // Exact, not within a float tolerance: this used to save as 1024.
    expect(parseMemoryGiB("1.0000000001")).toEqual({ ok: false, error: "Must be a whole number of MiB (GiB × 1024)" });
    expect(parseMemoryGiB("1.50000000000")).toEqual({ ok: true, value: 1536 }); // trailing zeros are not digits
    for (const bad of ["1.5.0", "1.0.0", "4.0.", "1.", "10."]) expect(parseMemoryGiB(bad).ok).toBe(false); // malformed stays refused
    expect(parseMemoryGiB("1.00000000000")).toEqual({ ok: true, value: 1024 });
    expect(parseMemoryGiB("1.50000000001")).toEqual({ ok: false, error: "Must be a whole number of MiB (GiB × 1024)" }); // > 10 significant places
    expect(parseMemoryGiB("1.5000000000")).toEqual({ ok: true, value: 1536 });
    expect(parseMemoryGiB("0").ok).toBe(false);
    expect(parseMemoryGiB("1e3").ok).toBe(false);
  });

  it("parses disks in whole GiB", () => {
    expect(parseDiskGiB("50")).toEqual({ ok: true, value: 50 });
    expect(parseDiskGiB("1.5").ok).toBe(false);
    expect(parseDiskGiB("0").ok).toBe(false);
  });

  it("round-trips every builtin through the human units exactly", () => {
    for (const s of BUILTIN_WORKER_SIZES) {
      expect(parseCores(cpuCores(s.cpu_request_millicores))).toEqual({ ok: true, value: s.cpu_request_millicores });
      expect(parseMemoryGiB(memoryGiB(s.memory_limit_mib))).toEqual({ ok: true, value: s.memory_limit_mib });
    }
  });
});

describe("newSizeNameError (PRD #4213 M3)", () => {
  const existing = ["s", "m", "l", "l-highmem"];
  it("accepts a well-formed new name, up to 16 characters", () => {
    for (const ok of ["xs", "x", "gpu-2", "a".repeat(16), "x-"]) expect(newSizeNameError(ok, existing), ok).toBeNull();
  });
  it("matches the api's builtin.WellFormed pattern", () => {
    expect(SIZE_NAME_PATTERN.source).toBe("^[a-z][a-z0-9-]{0,15}$");
    expect(MAX_WORKER_SIZES).toBe(16);
  });
  it("says why a name cannot be a new size", () => {
    expect(newSizeNameError("", existing)).toBe("Enter a name for the size, such as xs");
    expect(newSizeNameError("XS", existing)).toBe("Use lowercase letters only, such as xs");
    expect(newSizeNameError("a".repeat(17), existing)).toBe("Use at most 16 characters");
    for (const bad of ["1x", "-x", "x_s", "x s", "é"]) {
      expect(newSizeNameError(bad, existing), bad).toBe("Start with a lowercase letter, then use only lowercase letters, digits or hyphens");
    }
    // The api's PUT on an existing name edits it, so adding one is refused locally.
    expect(newSizeNameError("l-highmem", existing)).toBe("A size named l-highmem already exists; edit it in the table instead");
  });
});
