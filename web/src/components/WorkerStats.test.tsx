// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { WorkerStatGauges, WorkerStatLine, formatBytes, formatBytesPair } from "./WorkerStats";
import type { Worker } from "../lib/api";

afterEach(cleanup);

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    kind: "external",
    hosted_size: null,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: null,
    version: null,
    upgrade_status: "unknown",
    upgrade_detail: null,
    upgrade_target: "",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: null,
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    stats_disk_dind_bytes: null,
    stats_disk_dind_total_bytes: null,
    stats_disk_dind_inodes: null,
    stats_disk_dind_total_inodes: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
    ...over,
  };
}

describe("byte formatting", () => {
  it("formats a byte count in its own binary unit, trimming a trailing .0", () => {
    expect(formatBytes(4294967296)).toBe("4 GiB");
    expect(formatBytes(2254857830)).toBe("2.1 GiB");
    expect(formatBytes(503316480)).toBe("480 MiB");
    expect(formatBytes(512)).toBe("512 B");
  });

  it("formats a used/limit pair sharing the limit's unit", () => {
    expect(formatBytesPair(2254857830, 4294967296)).toBe("2.1/4 GiB");
    expect(formatBytesPair(1610612736, 2147483648)).toBe("1.5/2 GiB");
  });
});

describe("WorkerStatGauges", () => {
  it("renders nothing until the worker has reported a sample", () => {
    const { container } = render(<WorkerStatGauges worker={aWorker()} />);
    expect(container.firstChild).toBeNull();
  });

  it("shows a CPU bar and a used/limit memory bar with a percentage when a limit is known", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ stats_cpu_pct: 34, stats_mem_bytes: 2254857830, stats_mem_limit_bytes: 4294967296, stats_source: "cgroup" })}
      />,
    );
    expect(screen.getByText("34%")).toBeTruthy();
    expect(screen.getByText(/2\.1\/4 GiB · 52%/)).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "CPU" }).getAttribute("aria-valuenow")).toBe("34");
    expect(screen.getByRole("progressbar", { name: "Memory" }).getAttribute("aria-valuenow")).toBe("52");
    // aria-valuetext gives a screen reader the byte figures + percent, not a bare "N%".
    expect(screen.getByRole("progressbar", { name: "CPU" }).getAttribute("aria-valuetext")).toBe("34%");
    expect(screen.getByRole("progressbar", { name: "Memory" }).getAttribute("aria-valuetext")).toBe("2.1/4 GiB, 52%");
  });

  it("shows absolute memory with NO percentage bar when the limit is unknown", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ stats_cpu_pct: 8, stats_mem_bytes: 503316480, stats_mem_limit_bytes: null, stats_source: "process" })}
      />,
    );
    expect(screen.getByText(/480 MiB/)).toBeTruthy();
    expect(screen.getByText(/no limit/)).toBeTruthy();
    // Only the CPU bar is a progressbar; memory has no bar without a limit.
    expect(screen.getAllByRole("progressbar")).toHaveLength(1);
  });

  it("omits the CPU value on the first tick (cpu_pct null) but still renders the empty bar", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ stats_cpu_pct: null, stats_mem_bytes: 503316480, stats_mem_limit_bytes: null, stats_source: "cgroup" })}
      />,
    );
    expect(screen.getByText("—")).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "CPU" }).getAttribute("aria-valuenow")).toBe("0");
    // A screen reader hears "no reading yet", not "0 percent" (which would read as real 0% usage).
    expect(screen.getByRole("progressbar", { name: "CPU" }).getAttribute("aria-valuetext")).toBe("no reading yet");
  });

  it("labels a process-source sample 'worker process only'", () => {
    render(<WorkerStatGauges worker={aWorker({ stats_mem_bytes: 1, stats_source: "process" })} />);
    expect(screen.getByText("worker process only")).toBeTruthy();
  });

  it("dims the gauges for an offline worker (last-known, not live-looking)", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ status: "offline", stats_cpu_pct: 12, stats_mem_bytes: 1, stats_mem_limit_bytes: 2, stats_source: "cgroup" })}
      />,
    );
    const block = screen.getByLabelText(/worker offline/i);
    expect(block.className).toMatch(/opacity-50/);
  });

  it("clamps the bar width to 100% even when the server stored an over-100 cpu_pct", () => {
    render(<WorkerStatGauges worker={aWorker({ stats_cpu_pct: 640, stats_mem_bytes: 1, stats_source: "cgroup" })} />);
    const cpuBar = screen.getByRole("progressbar", { name: "CPU" });
    expect(cpuBar.getAttribute("aria-valuenow")).toBe("100");
    expect((cpuBar.firstChild as HTMLElement).style.width).toBe("100%");
    // The true value is still shown in the label — only the DOM bar is clamped.
    expect(screen.getByText("640%")).toBeTruthy();
  });

  it("applies the danger tone to bars at/above 85%", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ stats_cpu_pct: 90, stats_mem_bytes: 7554662400, stats_mem_limit_bytes: 8589934592, stats_source: "cgroup" })}
      />,
    );
    // ~88% memory (7554662400/8589934592) and 90% cpu — both in the new ≥85 danger band.
    expect((screen.getByRole("progressbar", { name: "CPU" }).firstChild as HTMLElement).className).toMatch(/bg-danger/);
    expect((screen.getByRole("progressbar", { name: "Memory" }).firstChild as HTMLElement).className).toMatch(/bg-danger/);
  });

  it("applies the warn tone to a bar in the 40–84 band", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ stats_cpu_pct: 50, stats_mem_bytes: 1, stats_mem_limit_bytes: 2, stats_source: "cgroup" })}
      />,
    );
    const cpuBar = screen.getByRole("progressbar", { name: "CPU" }).firstChild as HTMLElement;
    expect(cpuBar.className).toMatch(/bg-warn/);
    expect(cpuBar.className).not.toMatch(/bg-danger/);
  });

  it("renders TWO disk bars at the right width/tone when both volumes are reported", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({
          stats_mem_bytes: 1,
          stats_source: "cgroup",
          stats_disk_nix_bytes: 19327352832, // 18 GiB
          stats_disk_nix_total_bytes: 21474836480, // 20 GiB → 90% (danger)
          stats_disk_data_bytes: 4294967296, // 4 GiB
          stats_disk_data_total_bytes: 10737418240, // 10 GiB → 40% (warn)
        })}
      />,
    );
    const nix = screen.getByRole("progressbar", { name: "Disk /nix" });
    const data = screen.getByRole("progressbar", { name: "Disk /data" });
    expect(nix.getAttribute("aria-valuenow")).toBe("90");
    expect(data.getAttribute("aria-valuenow")).toBe("40");
    expect((nix.firstChild as HTMLElement).className).toMatch(/bg-danger/);
    expect((data.firstChild as HTMLElement).className).toMatch(/bg-warn/);
    expect(screen.getByText(/18\/20 GiB · 90%/)).toBeTruthy();
    expect(screen.getByText(/4\/10 GiB · 40%/)).toBeTruthy();
    // aria-valuetext carries the byte figures + percent for a screen reader.
    expect(nix.getAttribute("aria-valuetext")).toBe("18/20 GiB, 90%");
  });

  it("renders ONE disk bar when only a single volume is reported", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({
          stats_mem_bytes: 1,
          stats_source: "process",
          stats_disk_data_bytes: 5368709120, // 5 GiB
          stats_disk_data_total_bytes: 10737418240, // 10 GiB → 50%
        })}
      />,
    );
    expect(screen.getByRole("progressbar", { name: "Disk /data" }).getAttribute("aria-valuenow")).toBe("50");
    // /nix was not reported → no bar for it.
    expect(screen.queryByRole("progressbar", { name: "Disk /nix" })).toBeNull();
  });

  it("renders NO disk section when neither volume is reported (mem-only worker)", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ stats_cpu_pct: 10, stats_mem_bytes: 1, stats_mem_limit_bytes: 2, stats_source: "cgroup" })}
      />,
    );
    // Only CPU + Memory bars, no Disk bars.
    expect(screen.getAllByRole("progressbar")).toHaveLength(2);
    expect(screen.queryByRole("progressbar", { name: "Disk /nix" })).toBeNull();
    expect(screen.queryByRole("progressbar", { name: "Disk /data" })).toBeNull();
  });
});

describe("WorkerStatGauges: docker-tier dind volume (issue #1759)", () => {
  const GIB20 = 21474836480;
  const sampled = { stats_mem_bytes: 1, stats_source: "cgroup" } as const;

  it("renders no dind bar while the dind fields are null, alongside a reported /data bar", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({ ...sampled, stats_disk_data_bytes: 5368709120, stats_disk_data_total_bytes: 10737418240 })}
      />,
    );
    // Control: the sibling disk bar IS rendered, so the dind absence is not vacuous.
    expect(screen.getByRole("progressbar", { name: "Disk /data" })).toBeTruthy();
    expect(screen.queryByRole("progressbar", { name: "Disk dind" })).toBeNull();
  });

  it("renders no dind bar when only one side of the byte pair is reported", () => {
    render(<WorkerStatGauges worker={aWorker({ ...sampled, stats_disk_dind_bytes: 1, stats_disk_dind_total_bytes: null })} />);
    expect(screen.getByRole("progressbar", { name: "CPU" })).toBeTruthy();
    expect(screen.queryByRole("progressbar", { name: "Disk dind" })).toBeNull();
  });

  it("renders no dind bar when bytes used is null but the total is reported", () => {
    // Mirror of the case above: pins the `used == null` half of the present-guard.
    render(<WorkerStatGauges worker={aWorker({ ...sampled, stats_disk_dind_bytes: null, stats_disk_dind_total_bytes: GIB20 })} />);
    // Positive control: the gauge block rendered, so the dind absence is not vacuous.
    expect(screen.getByRole("progressbar", { name: "CPU" })).toBeTruthy();
    expect(screen.queryByRole("progressbar", { name: "Disk dind" })).toBeNull();
  });

  it("does not name inodes when they only out-fill bytes below the displayed whole percent", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({
          ...sampled,
          stats_disk_dind_bytes: 16793322127, // 78.2%
          stats_disk_dind_total_bytes: GIB20,
          stats_disk_dind_inodes: 1027604, // 78.4%: raw-fuller, but both read "78%"
          stats_disk_dind_total_inodes: 1310720,
        })}
      />,
    );
    const dind = screen.getByRole("progressbar", { name: "Disk dind" });
    expect(dind.getAttribute("aria-valuenow")).toBe("78");
    expect(dind.getAttribute("aria-valuetext")).toBe("15.6/20 GiB, 78%");
    // Positive: the value text rendered; negative: no "inodes 78%" beside it.
    expect(screen.getByText("15.6/20 GiB · 78%")).toBeTruthy();
    expect(screen.queryByText(/inodes/)).toBeNull();
  });

  it("fills the dind bar by the bytes ratio when bytes are the fuller dimension", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({
          ...sampled,
          stats_disk_dind_bytes: 16750372454, // ~15.6 GiB → 78% (warn)
          stats_disk_dind_total_bytes: GIB20,
          stats_disk_dind_inodes: 412000, // ~31%
          stats_disk_dind_total_inodes: 1310720,
        })}
      />,
    );
    const dind = screen.getByRole("progressbar", { name: "Disk dind" });
    expect(dind.getAttribute("aria-valuenow")).toBe("78");
    expect((dind.firstChild as HTMLElement).className).toMatch(/bg-warn/);
    expect(dind.getAttribute("aria-valuetext")).toBe("15.6/20 GiB, 78%");
    expect(screen.getByText("15.6/20 GiB · 78%")).toBeTruthy();
    // The label explains what "dind" is.
    expect(screen.getByText("Disk dind").getAttribute("title")).toMatch(/docker daemon data/);
    // ...and assistive tech gets the same explanation as the bar's description.
    const describedBy = dind.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy!)?.textContent).toMatch(/docker daemon data/);
    // Only hinted bars carry a description.
    expect(screen.getByRole("progressbar", { name: "CPU" }).hasAttribute("aria-describedby")).toBe(false);
  });

  it("fills the dind bar by the inode ratio and says so when inodes dominate", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({
          ...sampled,
          stats_disk_dind_bytes: 8589934592, // 8 GiB → 40%
          stats_disk_dind_total_bytes: GIB20,
          stats_disk_dind_inodes: 1284506, // → 98%
          stats_disk_dind_total_inodes: 1310720,
        })}
      />,
    );
    const dind = screen.getByRole("progressbar", { name: "Disk dind" });
    expect(dind.getAttribute("aria-valuenow")).toBe("98");
    expect((dind.firstChild as HTMLElement).style.width).toBe("98%");
    expect((dind.firstChild as HTMLElement).className).toMatch(/bg-danger/);
    expect(screen.getByText("8/20 GiB · 40% · inodes 98%")).toBeTruthy();
    expect(dind.getAttribute("aria-valuetext")).toBe("8/20 GiB, 40%, inodes 98%");
  });

  it("ignores a malformed zero-total inode pair: bytes drive the bar, no inode text, no crash", () => {
    render(
      <WorkerStatGauges
        worker={aWorker({
          ...sampled,
          stats_disk_dind_bytes: 8589934592, // 40%
          stats_disk_dind_total_bytes: GIB20,
          stats_disk_dind_inodes: 5000,
          stats_disk_dind_total_inodes: 0,
        })}
      />,
    );
    const dind = screen.getByRole("progressbar", { name: "Disk dind" });
    expect(dind.getAttribute("aria-valuenow")).toBe("40");
    expect(dind.getAttribute("aria-valuetext")).toBe("8/20 GiB, 40%");
    // Paired with the positive above: the value text rendered, and it carries no inode note.
    expect(screen.getByText("8/20 GiB · 40%")).toBeTruthy();
    expect(screen.queryByText(/inodes/)).toBeNull();
  });
});

describe("WorkerStatLine", () => {
  it("renders a compact 'cpu X% · mem used/limit' line", () => {
    render(
      <WorkerStatLine
        worker={aWorker({ stats_cpu_pct: 34, stats_mem_bytes: 2254857830, stats_mem_limit_bytes: 4294967296, stats_source: "cgroup" })}
      />,
    );
    expect(screen.getByText(/cpu 34% · mem 2\.1\/4 GiB/)).toBeTruthy();
  });

  it("renders nothing without a sample", () => {
    const { container } = render(<WorkerStatLine worker={aWorker()} />);
    expect(container.firstChild).toBeNull();
  });

  it("appends '· disk used/total' for the fuller volume when disk is reported", () => {
    const { container } = render(
      <WorkerStatLine
        worker={aWorker({
          stats_cpu_pct: 34,
          stats_mem_bytes: 2254857830,
          stats_mem_limit_bytes: 4294967296,
          stats_source: "cgroup",
          stats_disk_nix_bytes: 19327352832, // 18 GiB → 90% (fuller)
          stats_disk_nix_total_bytes: 21474836480, // 20 GiB
          stats_disk_data_bytes: 4294967296, // 4 GiB → 40%
          stats_disk_data_total_bytes: 10737418240, // 10 GiB
        })}
      />,
    );
    // The fuller volume (/nix at 90%) stands in for disk, not /data.
    expect(container.textContent).toContain("cpu 34% · mem 2.1/4 GiB · disk 18/20 GiB");
  });

  it("omits the disk segment entirely when no volume is reported", () => {
    const { container } = render(
      <WorkerStatLine
        worker={aWorker({ stats_cpu_pct: 34, stats_mem_bytes: 2254857830, stats_mem_limit_bytes: 4294967296, stats_source: "cgroup" })}
      />,
    );
    expect(container.textContent).toContain("cpu 34% · mem 2.1/4 GiB");
    expect(container.textContent).not.toContain("disk");
  });

  it("names inodes on the disk segment when the inode-dominant dind volume is the fullest", () => {
    const { container } = render(
      <WorkerStatLine
        worker={aWorker({
          stats_cpu_pct: 34,
          stats_mem_bytes: 2254857830,
          stats_mem_limit_bytes: 4294967296,
          stats_source: "cgroup",
          stats_disk_data_bytes: 5368709120, // 50% — fuller than dind's bytes, emptier than its inodes
          stats_disk_data_total_bytes: 10737418240,
          stats_disk_dind_bytes: 8589934592, // 40% bytes
          stats_disk_dind_total_bytes: 21474836480,
          stats_disk_dind_inodes: 1284506, // 98% inodes → the fullest volume
          stats_disk_dind_total_inodes: 1310720,
        })}
      />,
    );
    expect(container.textContent).toContain("cpu 34% · mem 2.1/4 GiB · disk 8/20 GiB (inodes 98%)");
  });
});
