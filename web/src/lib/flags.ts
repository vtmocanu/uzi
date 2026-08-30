// Build-time toggle for the license credit in the app chrome. Default OFF (hidden).
// Flip to true and rebuild to show "MIT © Vlad Mocanu" in the app chrome.
// Not exported: read it through licenseCreditEnabled() so the value has a single
// call surface (the knip dead-code gate flags a value export used only in-file).
const SHOW_LICENSE_CREDIT = false;
export function licenseCreditEnabled(): boolean {
  return SHOW_LICENSE_CREDIT;
}
