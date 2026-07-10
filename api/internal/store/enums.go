package store

// Hand-maintained mirrors of DB CHECK-constraint enum values. Binding Go code to
// one symbol per value (instead of scattering the string literal across handlers,
// the worker service, the seed, and the vault) removes the drift hazard where one
// seal site's kind/sealed_with disagrees with another's — which, once the vault
// binds user_id||kind as AAD, would silently break decryption. Each constant MUST
// match its migration's CHECK exactly.

// KindAnthropicToken is user_secrets.kind for the per-user Anthropic token
// (migration 00010). Adding a kind is one ALTER-CHECK migration; the table shape
// never changes.
const KindAnthropicToken = "anthropic_token"

// user_secrets.sealed_with (migration 00051): which key sealed the ciphertext.
// 'master' = the legacy UZI_SECRET_KEY box; 'dek' = the per-user vault DEK.
const (
	SealedWithMaster = "master"
	SealedWithDEK    = "dek"
)
