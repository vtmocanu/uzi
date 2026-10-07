import { describe, expect, it } from "vitest";
import { capacityCadence } from "./scheduleCapacity";

describe("capacity cadence", () => {
  it.each([
    ["0 2 * * *", "Europe/Bucharest", "daily at 02:00 (Europe/Bucharest)"],
    ["0 2 * * 1-5", "UTC", "weekdays at 02:00 (UTC)"],
    ["0 3 * * 1", "UTC", "every monday at 03:00 (UTC)"],
    ["0 */4 * * *", "UTC", "every 4 hours (UTC)"],
    ["0 */5 * * *", "UTC", "on cron 0 */5 * * * (UTC)"],
    ["10 */7 * * *", "Europe/Bucharest", "on cron 10 */7 * * * (Europe/Bucharest)"],
    ["*/1 * * * *", "UTC", "every minute (UTC)"],
    ["*/30 * * * *", "UTC", "every 30 minutes (UTC)"],
    ["*/40 * * * *", "UTC", "on cron */40 * * * * (UTC)"],
    ["*/10 * * * *", "UTC", "every 10 minutes (UTC)"],
    ["*/7 * * * *", "UTC", "on cron */7 * * * * (UTC)"],
    ["0 2 1 * *", "Asia/Tokyo", "on cron 0 2 1 * * (Asia/Tokyo)"],
  ])("describes %s in its actual timezone", (cron, timezone, expected) => {
    expect(capacityCadence(cron, timezone)).toBe(expected);
  });
});
