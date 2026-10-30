import type { ForgeOriginCooldown } from "../lib/apiTypes";
import { forgeCooldownText } from "../lib/forgeCooldown";
import { useNow } from "../lib/useNow";

export function ForgeOriginStatus({ cooldown }: { cooldown?: ForgeOriginCooldown }) {
  const now = useNow(30_000);
  if (!cooldown) return null;
  const text = forgeCooldownText(cooldown, now);
  return <p className="mt-1 break-words text-xs text-warn" title={text}>{text}</p>;
}
