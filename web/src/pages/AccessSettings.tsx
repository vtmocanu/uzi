// Settings → Access: programmatic access to uzi. CLI tokens (the uzi CLI and CI) and
// product tokens (external products on /api/v1, PRD #1907) sit side by side. Inside
// SettingsShell so it sits beside Account & tokens / Run defaults / Forge / Memory.
//
// The one "Revoke all" lives in CliTokens and revokes every kind (D8, PRD #1910 D6). This
// page is the seam between the cards: ProductTokens reports its active count up so the
// confirm can name it, this page counts the user's live OAuth connections (grants, not
// tokens: a connection whose access tokens all expired still counts) and passes that down
// too, and a Revoke all bumps reloadKey so both refetch.

import { useCallback, useState } from "react";
import { api } from "../lib/api";
import { useAsyncData } from "../lib/useAsyncData";
import { SettingsShell } from "../components/SettingsShell";
import { CliTokens } from "../components/CliTokens";
import { ProductTokens } from "../components/ProductTokens";

export function AccessSettings() {
  // 0 until ProductTokens reports; null once it reports an unknown count (a failed or
  // cut list), which keeps Revoke all offered without a product number.
  const [productActive, setProductActive] = useState<number | null>(0);
  const [reloadKey, setReloadKey] = useState(0);
  const onRevokedAll = useCallback(() => setReloadKey((k) => k + 1), []);
  // The live-connection count from the connections list. 0 until it loads; null when it could
  // not be read (a failed first load), which keeps Revoke all offered without a connection
  // number, since hiding the panic button on a load blip is the unsafe failure.
  const { data: connections, error: connectionsError } = useAsyncData<number>(
    async () => (await api.listOAuthConnections()).connections.length,
    [reloadKey],
    { fallback: "Failed to load connections" },
  );
  const connectionCount = connections ?? (connectionsError ? null : 0);
  return (
    <SettingsShell description="Tokens for driving uzi from the terminal, CI or another product, without a browser session.">
      <div className="space-y-6">
        <CliTokens
          productTokenActiveCount={productActive}
          oauthConnectionCount={connectionCount}
          onRevokedAll={onRevokedAll}
        />
        <ProductTokens reloadKey={reloadKey} onActiveCountChange={setProductActive} />
      </div>
    </SettingsShell>
  );
}
