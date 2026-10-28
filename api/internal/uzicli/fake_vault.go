package uzicli

import "context"

// VaultStatus records the read and returns the configured state.
func (f *FakeClient) VaultStatus(_ context.Context) (VaultStatusResult, error) {
	f.VaultStatusCalls++
	if f.VaultStatusErr != nil {
		return VaultStatusResult{}, f.VaultStatusErr
	}
	if f.Err != nil {
		return VaultStatusResult{}, f.Err
	}
	return VaultStatusResult{Unlocked: !f.VaultLocked, Mode: f.VaultMode}, nil
}

// VaultUnlock records every attempted password, including failed attempts.
// Errors stay injectable so command tests can exercise their sanitization.
func (f *FakeClient) VaultUnlock(_ context.Context, password string) error {
	f.VaultUnlockCalls++
	f.VaultUnlockPasswords = append(f.VaultUnlockPasswords, password)
	if f.VaultUnlockErr != nil {
		return f.VaultUnlockErr
	}
	if f.Err != nil {
		return f.Err
	}
	f.VaultLocked = false
	return nil
}
