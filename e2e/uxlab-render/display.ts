// Escape directional controls only in display JSON; callers retain raw values.
export function displayJSON(value: unknown): string {
  return JSON.stringify(value).replace(/[\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/g,
    (char) => "\\u" + char.charCodeAt(0).toString(16).padStart(4, "0"));
}
