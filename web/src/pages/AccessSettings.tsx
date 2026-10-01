// Settings → Access: programmatic access to uzi. CLI tokens (the uzi CLI and CI), the products
// connected through OAuth (PRD #1910) and product tokens (external products on /api/v1,
// PRD #1907) sit side by side. Inside
// SettingsShell so it sits beside Account & tokens / Run defaults / Forge / Memory.
//
// The one "Revoke all" lives in CliTokens and revokes every kind (D8, PRD #1910 D6). This
// page is the seam between the cards: ProductTokens and OAuthConnections each report their
// live count up so the confirm can name it (a connection is counted as a grant, not as a
// token: one whose access tokens all expired still counts), and a Revoke all bumps reloadKey
// so both refetch. Revoking one connection refreshes the count through the same report.

import { useCallback, useState } from "react";
import { SettingsShell } from "../components/SettingsShell";
import { CliTokens } from "../components/CliTokens";
import { OAuthConnections } from "../components/OAuthConnections";
import { ProductTokens } from "../components/ProductTokens";

export function AccessSettings() {
  // 0 until ProductTokens reports; null once it reports an unknown count (a failed or
  // cut list), which keeps Revoke all offered without a product number.
  const [productActive, setProductActive] = useState<number | null>(0);
  // The live-connection count, reported by OAuthConnections. 0 until it loads; null when it
  // could not be read (a failed first load), which keeps Revoke all offered without a
  // connection number, since hiding the panic button on a load blip is the unsafe failure.
  const [connectionCount, setConnectionCount] = useState<number | null>(0);
  const [reloadKey, setReloadKey] = useState(0);
  const onRevokedAll = useCallback(() => setReloadKey((k) => k + 1), []);
  return (
    <SettingsShell description="Tokens for driving uzi from the terminal, CI or another product, without a browser session.">
      <div className="space-y-6">
        <CliTokens
          productTokenActiveCount={productActive}
          oauthConnectionCount={connectionCount}
          onRevokedAll={onRevokedAll}
        />
        <OAuthConnections reloadKey={reloadKey} onCountChange={setConnectionCount} />
        <ProductTokens reloadKey={reloadKey} onActiveCountChange={setProductActive} />
      </div>
    </SettingsShell>
  );
}
