// The app-wide banner stack, extracted unchanged from AppShell (PRD #3878). AppShell
// renders exactly one of these on every route, at one fixed tree position, so no banner
// remounts on navigation (the health banners keep their snooze/dismiss state there).

import { useAuth } from "../auth/AuthContext";
import { VaultLockedBanner, VaultRequiredPrompt } from "./VaultControls";
import { UpdateEscalationBanner } from "./UpdateEscalationBanner";
import { HealthDangerBanner } from "./HealthDangerBanner";
import { HealthAttentionBanner } from "./HealthAttentionBanner";
import { MaintenanceBanner } from "./MaintenanceBanner";

export function AppBanners() {
  const { user } = useAuth();
  return (
    <>
      {/* Vault locked banner (PRD #32): app-wide so the user can unlock from
          any page. Self-gates — renders nothing while unlocked. */}
      <VaultRequiredPrompt />
      <VaultLockedBanner />
      {/* Maintenance mode (PRD #3454): app-wide, every signed-in user, info tone, self-gating
          — renders nothing unless an admin has paused intake. */}
      <MaintenanceBanner />
      {/* Update escalation banner (PRD #836 M6): app-wide, admin-only, self-gating —
          renders nothing unless the server says the instance is far behind and the
          banner is enabled and not snoozed for the current release. */}
      <UpdateEscalationBanner />
      {/* App-wide instance-health danger banner (PRD #1484 M5): admin-only, danger-only,
          self-gating, with a 1 h per-episode snooze. Reads the shared HealthStatusProvider,
          so a non-admin never fetches the admin endpoint. */}
      <HealthDangerBanner />
      {user?.is_admin === true && <HealthAttentionBanner key={user.id} />}
    </>
  );
}
