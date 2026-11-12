// Platform detection for keyboard-shortcut modifiers (PRD #3875): Cmd on Apple platforms,
// Ctrl elsewhere. navigator.userAgentData is not in every lib.dom version, hence the cast.

export function isApplePlatform(): boolean {
  if (typeof navigator === "undefined") return false;
  const nav = navigator as Navigator & { userAgentData?: { platform?: string } };
  const platform = nav.userAgentData?.platform ?? nav.platform ?? "";
  return /mac|iphone|ipad/i.test(platform);
}

// The command menu shortcut as aria-keyshortcuts names it.
export function commandShortcutKeys(): string {
  return isApplePlatform() ? "Meta+K" : "Control+K";
}

// The command menu shortcut as shown on the Search button.
export function commandShortcutLabel(): string {
  return isApplePlatform() ? "⌘K" : "Ctrl K";
}
