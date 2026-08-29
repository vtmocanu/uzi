// browserTimezone resolves the browser's IANA timezone (e.g. "Europe/Bucharest") so
// the schedule create modal and the default-job enable fan-out (issue #660) can seed a
// schedule's zone from where the user actually is. It falls back to "UTC" when the
// runtime cannot resolve a zone (an empty string) or Intl throws, so a caller always
// gets a valid IANA name it can send or display.
export function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}
