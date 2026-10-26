package settings

import "context"

// VaultPolicy returns the display policy, failing closed on malformed data or a
// failed refresh (including when an expired optional snapshot exists). Admission
// must use store.GetVaultPolicy after store.LockSettingsAdmission instead.
func (c *Cache) VaultPolicy(ctx context.Context) (string, error) {
	m, err := c.strictSnapshot(ctx)
	if err == nil && m[KeyVaultPolicy] == "optional" {
		return "optional", nil
	}
	return DefaultVaultPolicy, err
}
