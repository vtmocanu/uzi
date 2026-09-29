// Settings → Access: programmatic access to uzi. CLI tokens (the uzi CLI and CI) and
// product tokens (external products on /api/v1, PRD #1907) sit side by side. Inside
// SettingsShell so it sits beside Account & tokens / Run defaults / Forge / Memory.
//
// The one "Revoke all" lives in CliTokens and revokes both kinds (D8). This page is
// the seam between the two cards: ProductTokens reports its active count up so the
// confirm can name it, and a Revoke all bumps reloadKey so the product list refetches.

import { useCallback, useState } from "react";
import { SettingsShell } from "../components/SettingsShell";
import { CliTokens } from "../components/CliTokens";
import { ProductTokens } from "../components/ProductTokens";

export function AccessSettings() {
  const [productActive, setProductActive] = useState(0);
  const [reloadKey, setReloadKey] = useState(0);
  const onRevokedAll = useCallback(() => setReloadKey((k) => k + 1), []);
  return (
    <SettingsShell description="Tokens for driving uzi from the terminal, CI or another product, without a browser session.">
      <div className="space-y-6">
        <CliTokens productTokenActiveCount={productActive} onRevokedAll={onRevokedAll} />
        <ProductTokens reloadKey={reloadKey} onActiveCountChange={setProductActive} />
      </div>
    </SettingsShell>
  );
}
