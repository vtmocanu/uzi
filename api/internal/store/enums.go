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

// LabelDefaultSecret is the label 00077 backfills onto every pre-existing
// user_secrets row, and the one the kind-path compatibility routes and the seed
// create a user's first secret under (PRD #104 D2/D14). It is a conventional name,
// not a reserved one: nothing stops a user renaming their default, and no code may
// infer default-ness from the label — is_default is the only source of truth.
const LabelDefaultSecret = "default"

// user_secrets.sealed_with (migration 00051): which key sealed the ciphertext.
// 'master' = the legacy UZI_SECRET_KEY box; 'dek' = the per-user vault DEK.
const (
	SealedWithMaster = "master"
	SealedWithDEK    = "dek"
)
