import { humanizeCron, presetFromCron } from "./schedulePresets";

export function capacityCadence(cron: string, timezone: string): string {
  const expression = cron.trim();
  const preset = presetFromCron(expression);
  // Hour steps reset at midnight; non-divisors do not describe a uniform interval.
  if (preset.preset === "everyNHours" && 24 % preset.everyN !== 0) {
    return `on cron ${expression} (${timezone})`;
  }
  if (preset.preset !== "custom") {
    return `${humanizeCron(expression).replace("Every day", "Daily").toLowerCase()} (${timezone})`;
  }
  return `on cron ${expression} (${timezone})`;
}
