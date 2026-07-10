package seed

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSecretStore records the Anthropic-token seed's reads/writes, standing in
// for *store.Queries.
type fakeSecretStore struct {
	user     store.User
	userErr  error
	secrets  []store.ListUserSecretsMetaRow
	listErr  error
	upserted *store.UpsertUserSecretParams
}

func (s *fakeSecretStore) GetUserByEmail(context.Context, string) (store.User, error) {
	return s.user, s.userErr
}
func (s *fakeSecretStore) ListUserSecretsMeta(context.Context, uuid.UUID) ([]store.ListUserSecretsMetaRow, error) {
	return s.secrets, s.listErr
}
func (s *fakeSecretStore) UpsertUserSecret(_ context.Context, arg store.UpsertUserSecretParams) (store.UpsertUserSecretRow, error) {
	s.upserted = &arg
	return store.UpsertUserSecretRow{Kind: arg.Kind}, nil
}

// fakeSealer stands in for the vault: it DEK-seals with a recognizable prefix so
// the stored ciphertext can be asserted without a real cipher, records the user +
// kind it was bound to, and counts calls to prove the no-op paths never seal.
type fakeSealer struct {
	sealed     int
	lastUserID uuid.UUID
	lastKind   string
}

func (s *fakeSealer) Seal(userID uuid.UUID, kind string, plaintext []byte) ([]byte, error) {
	s.sealed++
	s.lastUserID = userID
	s.lastKind = kind
	return append([]byte("dek:"), plaintext...), nil
}

func anthropicCfg() config.Config {
	return config.Config{
		SeedEmail:          "admin@uzi.test",
		SeedAnthropicToken: "sk-ant-oat-abc123",
	}
}

func TestAnthropicTokenSeedsWhenAbsent(t *testing.T) {
	userID := uuid.New()
	st := &fakeSecretStore{user: store.User{ID: userID, Email: "admin@uzi.test"}}
	box := &fakeSealer{}

	if err := AnthropicToken(context.Background(), st, box, anthropicCfg()); err != nil {
		t.Fatalf("AnthropicToken: %v", err)
	}
	if st.upserted == nil {
		t.Fatal("expected the token to be stored")
	}
	if st.upserted.UserID != userID {
		t.Fatalf("token stored for the wrong user: %v", st.upserted.UserID)
	}
	if st.upserted.Kind != store.KindAnthropicToken {
		t.Fatalf("token stored under the wrong kind: %q", st.upserted.Kind)
	}
	if got := string(st.upserted.Ciphertext); got != "dek:sk-ant-oat-abc123" {
		t.Fatalf("token not DEK-sealed via the vault: %q", got)
	}
	// M2 seeds a DEK-sealed token (the seed admin's vault is boot-unlocked by main);
	// sealed_with must record 'dek' (an empty value would violate the migration's
	// CHECK at runtime, which the fake hides), and the seal must be bound to the
	// seed admin + the anthropic kind.
	if st.upserted.SealedWith != store.SealedWithDEK {
		t.Fatalf("seeded secret sealed_with = %q, want %q", st.upserted.SealedWith, store.SealedWithDEK)
	}
	if box.lastUserID != userID || box.lastKind != store.KindAnthropicToken {
		t.Fatalf("seal bound to (%v,%q), want (%v,%q)", box.lastUserID, box.lastKind, userID, store.KindAnthropicToken)
	}
}

func TestAnthropicTokenExistingIsNoOp(t *testing.T) {
	st := &fakeSecretStore{
		user:    store.User{ID: uuid.New()},
		secrets: []store.ListUserSecretsMetaRow{{Kind: store.KindAnthropicToken}},
	}
	box := &fakeSealer{}

	if err := AnthropicToken(context.Background(), st, box, anthropicCfg()); err != nil {
		t.Fatalf("AnthropicToken: %v", err)
	}
	if box.sealed != 0 {
		t.Fatal("an existing token must not be re-sealed")
	}
	if st.upserted != nil {
		t.Fatal("an existing token must be left entirely untouched")
	}
}

func TestAnthropicTokenDisabledIsNoOp(t *testing.T) {
	st := &fakeSecretStore{}
	box := &fakeSealer{}
	cfg := anthropicCfg()
	cfg.SeedAnthropicToken = "" // disabled

	if err := AnthropicToken(context.Background(), st, box, cfg); err != nil {
		t.Fatalf("disabled seed must be a no-op, got: %v", err)
	}
	if box.sealed != 0 || st.upserted != nil {
		t.Fatal("disabled seed must not touch the store or the cipher")
	}
}

func TestAnthropicTokenUserLookupErrorIsFatal(t *testing.T) {
	st := &fakeSecretStore{userErr: errors.New("db down")}
	box := &fakeSealer{}

	if err := AnthropicToken(context.Background(), st, box, anthropicCfg()); err == nil {
		t.Fatal("a DB error looking up the seed admin must be fatal (returned)")
	}
}

func TestAnthropicTokenMalformedIsFatalAndRedacted(t *testing.T) {
	st := &fakeSecretStore{user: store.User{ID: uuid.New()}}
	box := &fakeSealer{}
	cfg := anthropicCfg()
	cfg.SeedAnthropicToken = "has a space" // interior whitespace => invalid format

	err := AnthropicToken(context.Background(), st, box, cfg)
	if err == nil {
		t.Fatal("a malformed configured token must be fatal (returned)")
	}
	if strings.Contains(err.Error(), "has a space") {
		t.Fatalf("error must not leak the token value: %v", err)
	}
	if st.upserted != nil {
		t.Fatal("nothing should be stored when the token format is invalid")
	}
}

// TestAnthropicSeedConfigRejectsTokenWithoutEmail asserts the config-layer guard:
// a token set without a seed email is a loud boot failure, so a misconfig can
// never reach AnthropicToken.
func TestAnthropicSeedConfigRejectsTokenWithoutEmail(t *testing.T) {
	// A real (varied-byte) 32-byte at-rest key, built at runtime so no key
	// literal lands in source (mirrors config_test.go).
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}

	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/uzi")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
	t.Setenv("UZI_SEED_EMAIL", "") // no seed admin
	t.Setenv("UZI_SEED_ANTHROPIC_TOKEN", "unit-test-token-value")

	if _, err := config.Load(); err == nil {
		t.Fatal("config.Load must reject UZI_SEED_ANTHROPIC_TOKEN without UZI_SEED_EMAIL")
	}
}
