import { humanizeCron, presetFromCron } from "./schedulePresets";

export function capacityCadence(cron: string, timezone: string): string {
  const expression = cron.trim();
  const preset = presetFromCron(expression);
  if (preset.preset !== "custom") {
    return `${humanizeCron(expression).replace("Every day", "Daily").toLowerCase()} (${timezone})`;
  }
  const minutes = /^\*\/(\d+) \* \* \* \*$/.exec(expression);
  if (minutes && Number(minutes[1]) > 0 && 60 % Number(minutes[1]) === 0) {
    return `every ${Number(minutes[1])} minutes (${timezone})`;
  }
  return `on cron ${expression} (${timezone})`;
}
