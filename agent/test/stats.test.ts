import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { StatsCollector, type StatsCollectorOptions } from "../src/stats.js";

// Unit tests over fixture cgroup v2 trees (PRD #49 M1). Each test writes a throwaway
// /sys/fs/cgroup-shaped dir plus a /proc/self/cgroup file, so the collector's real
// file reads run against a controlled tree with no host cgroup dependency. The clock
// and CPU/RSS sources are injected so the CPU-delta math is deterministic.

const tmpdirs: string[] = [];
afterEach(() => {
  while (tmpdirs.length) fs.rmSync(tmpdirs.pop()!, { recursive: true, force: true });
});

/** A throwaway cgroup tree; returns the dir to pass as cgroupRoot. */
function cgroupTree(files: Record<string, string>): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-cgroup-"));
  tmpdirs.push(dir);
  for (const [name, content] of Object.entries(files)) {
    fs.writeFileSync(path.join(dir, name), content);
  }
  return dir;
}

/** A /proc/self/cgroup file with the given single line. */
function procCgroup(line: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-proc-"));
  tmpdirs.push(dir);
  const p = path.join(dir, "cgroup");
  fs.writeFileSync(p, line + "\n");
  return p;
}

/** A private-cgroupns (Docker/k8s default) tree with a limit + a 0.5-CPU quota. */
function limitedTree(usageUsec: string, memCurrent = "104857600"): { cgroupRoot: string; procCgroupPath: string } {
  return {
    cgroupRoot: cgroupTree({
      "memory.current": memCurrent, // 100 MiB
      "memory.stat": "anon 90000000\ninactive_file 4194304\nactive_file 100\n", // 4 MiB reclaimable
      "memory.max": "268435456", // 256 MiB
      "cpu.stat": `usage_usec ${usageUsec}\nuser_usec 1\nsystem_usec 1\n`,
      "cpu.max": "50000 100000", // 0.5 CPU
    }),
    procCgroupPath: procCgroup("0::/"),
  };
}

describe("StatsCollector — cgroup source", () => {
  it("reports docker-stats memory (current − inactive_file), the cgroup limit, and omits cpu_pct on the first tick", () => {
    const { cgroupRoot, procCgroupPath } = limitedTree("1000000");
    const c = new StatsCollector({ cgroupRoot, procCgroupPath, now: () => 0n, cpuCount: () => 8 });

    const s = c.collect();
    assert.ok(s);
    assert.strictEqual(s.source, "cgroup");
    assert.strictEqual(s.mem_bytes, 104857600 - 4194304, "mem is memory.current − inactive_file");
    assert.strictEqual(s.mem_limit_bytes, 268435456);
    assert.strictEqual(s.cpu_pct, undefined, "no CPU delta on the first tick");
  });

  it("computes cpu_pct from the usage_usec delta over measured elapsed, normalized by the cpu.max quota (period parsed)", () => {
    let nowNs = 0n;
    // First tick establishes the baseline (usage 1_000_000 µs).
    const first = limitedTree("1000000");
    const c = new StatsCollector({ ...first, now: () => nowNs, cpuCount: () => 8 });
    assert.strictEqual(c.collect()?.cpu_pct, undefined);

    // Second tick: +500_000 µs CPU over 1s elapsed at a 0.5-CPU quota → exactly 100%.
    // (500_000µs / 1_000_000µs) / 0.5 × 100 = 100. If the quota's PERIOD were ignored
    // the denominator would be wrong and this would not be 100.
    nowNs = 1_000_000_000n; // +1s
    const second = limitedTree("1500000");
    (c as unknown as { cgroupRoot: string }).cgroupRoot = second.cgroupRoot;
    (c as unknown as { procCgroupPath: string }).procCgroupPath = second.procCgroupPath;

    assert.strictEqual(c.collect()?.cpu_pct, 100);
  });

  it("normalizes cpu_pct by host cores when cpu.max is 'max', and reports no limit for memory.max 'max'", () => {
    let nowNs = 0n;
    const mk = (usage: string): StatsCollectorOptions => ({
      cgroupRoot: cgroupTree({
        "memory.current": "2097152",
        "memory.stat": "inactive_file 0\n",
        "memory.max": "max", // unlimited
        "cpu.stat": `usage_usec ${usage}\n`,
        "cpu.max": "max 100000", // no quota → normalize by host cores
      }),
      procCgroupPath: procCgroup("0::/"),
      now: () => nowNs,
      cpuCount: () => 4,
    });
    const opts = mk("0");
    const c = new StatsCollector(opts);
    const first = c.collect();
    assert.strictEqual(first?.mem_limit_bytes, null, "memory.max 'max' → null (no percentage bar)");
    assert.strictEqual(first?.source, "cgroup");
    assert.strictEqual(first?.cpu_pct, undefined);

    // +400_000 µs over 1s on a 4-core host → 0.4/4 × 100 = 10%.
    nowNs = 1_000_000_000n;
    const next = mk("400000");
    (c as unknown as { cgroupRoot: string }).cgroupRoot = (next.cgroupRoot as string);
    (c as unknown as { procCgroupPath: string }).procCgroupPath = (next.procCgroupPath as string);
    assert.strictEqual(c.collect()?.cpu_pct, 10);
  });

  it("omits cpu_pct (never a negative value) when the usage counter goes backwards", () => {
    let nowNs = 0n;
    const c = new StatsCollector({ ...limitedTree("2000000"), now: () => nowNs, cpuCount: () => 8 });
    c.collect();
    nowNs = 1_000_000_000n;
    const lower = limitedTree("1000000"); // counter reset / regression
    (c as unknown as { cgroupRoot: string }).cgroupRoot = lower.cgroupRoot;
    (c as unknown as { procCgroupPath: string }).procCgroupPath = lower.procCgroupPath;
    assert.strictEqual(c.collect()?.cpu_pct, undefined, "a backwards delta is omitted, never reported negative");
  });
});

describe("StatsCollector — fallback to process source", () => {
  const fallbackOpts = (over: Partial<StatsCollectorOptions> = {}): StatsCollectorOptions => ({
    now: () => 0n,
    cpuCount: () => 2,
    processRss: () => 55_000_000,
    processCpuUsage: () => ({ user: 0, system: 0 }),
    ...over,
  });

  it("falls back when the cgroup files are missing (cgroup v1 host / dev)", () => {
    const empty = cgroupTree({}); // no memory.*/cpu.* files
    const c = new StatsCollector(fallbackOpts({ cgroupRoot: empty, procCgroupPath: procCgroup("0::/") }));
    const s = c.collect();
    assert.strictEqual(s?.source, "process");
    assert.strictEqual(s?.mem_bytes, 55_000_000, "process RSS");
    assert.strictEqual(s?.mem_limit_bytes, null, "process source knows no container limit");
    assert.strictEqual(s?.cpu_pct, undefined, "first tick omits cpu_pct in the fallback too");
  });

  it("falls back when a cgroup file is malformed", () => {
    for (const bad of [
      { "memory.current": "not-a-number", "memory.stat": "inactive_file 0\n", "memory.max": "1", "cpu.stat": "usage_usec 0\n", "cpu.max": "max 100000" },
      { "memory.current": "1", "memory.stat": "inactive_file 0\n", "memory.max": "1", "cpu.stat": "usage_usec 0\n", "cpu.max": "garbage" },
      { "memory.current": "1", "memory.stat": "inactive_file 0\n", "memory.max": "1", "cpu.stat": "no_usage_here 0\n", "cpu.max": "max 100000" },
    ]) {
      const c = new StatsCollector(fallbackOpts({ cgroupRoot: cgroupTree(bad), procCgroupPath: procCgroup("0::/") }));
      assert.strictEqual(c.collect()?.source, "process", `malformed tree ${JSON.stringify(bad)} → process fallback`);
    }
  });

  it("falls back under cgroupns=host (a non-root /proc/self/cgroup path masquerades as the host)", () => {
    // Decision 2's masquerade case: /sys/fs/cgroup is the host root (memory.max=max)
    // while the container IS capped. The discriminator is /proc/self/cgroup showing a
    // NON-root path. (This is the corrected form of Decision 2 — see stats.ts.)
    const hostView = cgroupTree({
      "memory.current": "999999999999",
      "memory.stat": "inactive_file 0\n",
      "memory.max": "max",
      "cpu.stat": "usage_usec 0\n",
      "cpu.max": "max 100000",
    });
    const c = new StatsCollector(fallbackOpts({ cgroupRoot: hostView, procCgroupPath: procCgroup("0::/docker/4c2669c634fa") }));
    const s = c.collect();
    assert.strictEqual(s?.source, "process", "non-root proc cgroup → fall back rather than report the host");
    assert.strictEqual(s?.mem_bytes, 55_000_000, "reports process RSS, not the host's memory.current");
  });

  it("uses the cgroup source for the namespace root (0::/, the private-ns default)", () => {
    // The counterpart to the masquerade test: 0::/ is the GOOD containerized case.
    const { cgroupRoot, procCgroupPath } = limitedTree("0");
    const c = new StatsCollector({ cgroupRoot, procCgroupPath, now: () => 0n, cpuCount: () => 8 });
    assert.strictEqual(c.collect()?.source, "cgroup");
  });

  it("still yields a process sample (never undefined) when /proc/self/cgroup itself is unreadable", () => {
    const c = new StatsCollector(fallbackOpts({ cgroupRoot: cgroupTree({}), procCgroupPath: "/nonexistent/proc/cgroup" }));
    assert.strictEqual(c.collect()?.source, "process");
  });

  it("computes fallback cpu_pct from process.cpuUsage deltas normalized by host cores", () => {
    let nowNs = 0n;
    let cpu = { user: 0, system: 0 };
    const c = new StatsCollector(
      fallbackOpts({ cgroupRoot: cgroupTree({}), procCgroupPath: procCgroup("0::/"), now: () => nowNs, cpuCount: () => 2, processCpuUsage: () => cpu }),
    );
    assert.strictEqual(c.collect()?.cpu_pct, undefined, "first tick omits cpu_pct");
    // +200_000 µs CPU (user+system) over 1s on 2 cores → 0.2/2 × 100 = 10%.
    nowNs = 1_000_000_000n;
    cpu = { user: 150_000, system: 50_000 };
    assert.strictEqual(c.collect()?.cpu_pct, 10);
  });
});
