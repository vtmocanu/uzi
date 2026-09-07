package secretopen

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type fakeStore struct {
	row store.GetUserSecretCiphertextRow
	err error

	// byID is the row GetUserSecretCiphertextByID returns, and byIDArg records what
	// it was asked for — the owner scoping is a property of the query, so the test
	// asserts on the arguments rather than re-implementing the predicate.
	byID    store.GetUserSecretCiphertextByIDRow
	byIDErr error
	byIDArg store.GetUserSecretCiphertextByIDParams
}

func (f *fakeStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	return f.row, f.err
}

func (f *fakeStore) GetUserSecretCiphertextByID(_ context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	f.byIDArg = arg
	return f.byID, f.byIDErr
}

// key is a valid 32-byte AES key for a real master box (nil-vault path).
var key = []byte("0123456789abcdef0123456789abcdef")

func TestOpenNoSecret(t *testing.T) {
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), &fakeStore{err: pgx.ErrNoRows}, nil, box, uuid.New(), "anthropic_token")
	if !errors.Is(err, ErrNoSecret) {
		t.Fatalf("want ErrNoSecret, got %v", err)
	}
}

func TestOpenLookupError(t *testing.T) {
	box, _ := secretbox.New(key)
	sentinel := errors.New("db down")
	_, err := Open(context.Background(), &fakeStore{err: sentinel}, nil, box, uuid.New(), "anthropic_token")
	if errors.Is(err, ErrNoSecret) || errors.Is(err, ErrUndecryptable) {
		t.Fatalf("a non-ErrNoRows lookup error must not collapse to a credential sentinel: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("lookup error should wrap the underlying error, got %v", err)
	}
}

func TestOpenUndecryptable(t *testing.T) {
	box, _ := secretbox.New(key)
	// Ciphertext that the master box cannot open.
	row := store.GetUserSecretCiphertextRow{Ciphertext: []byte("not a valid sealed blob"), SealedWith: store.SealedWithMaster}
	_, err := Open(context.Background(), &fakeStore{row: row}, nil, box, uuid.New(), "anthropic_token")
	if !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("want ErrUndecryptable, got %v", err)
	}
}

func TestOpenSealed(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, err := box.Seal([]byte("s3cr3t-token"))
	if err != nil {
		t.Fatal(err)
	}
	// Round-trips a master-sealed row with no vault (the tick path).
	plain, err := OpenSealed(nil, box, uuid.New(), "anthropic_token", store.SealedWithMaster, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "s3cr3t-token" {
		t.Fatalf("plaintext = %q, want s3cr3t-token", plain)
	}

	// A bad ciphertext returns ErrUndecryptable, and the ciphertext bytes must not
	// leak into the error string (auditor: ciphertext out of every error).
	bad := []byte("BADCIPHERTEXT-sentinel-bytes")
	_, err = OpenSealed(nil, box, uuid.New(), "anthropic_token", store.SealedWithMaster, bad)
	if !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("want ErrUndecryptable, got %v", err)
	}
	if strings.Contains(err.Error(), string(bad)) {
		t.Fatalf("error leaked the ciphertext: %q", err.Error())
	}
}

// TestOpenSealedRealVaultLocked exercises the D3 dispatch through a REAL
// *vault.Vault whose cache is empty (i.e. the user is locked), not the nil-vault
// shortcut: a master-sealed secret opens regardless of the lock, while a
// dek-sealed one surfaces ErrVaultLocked. vault.Open never touches the store for
// either path, so a nil store is sufficient.
func TestOpenSealedRealVaultLocked(t *testing.T) {
	box, _ := secretbox.New(key)
	vlt := vault.New(box, nil) // empty DEK cache ⇒ every user is locked
	userID := uuid.New()
	if vlt.Unlocked(userID) {
		t.Fatal("a fresh vault must report the user as locked")
	}

	// Master-sealed: opens under the master box even though the vault is locked.
	sealed, err := box.Seal([]byte("legacy-token"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := OpenSealed(vlt, box, userID, "anthropic_token", store.SealedWithMaster, sealed)
	if err != nil {
		t.Fatalf("master-sealed must open while locked, got %v", err)
	}
	if string(plain) != "legacy-token" {
		t.Fatalf("plaintext = %q, want legacy-token", plain)
	}

	// Dek-sealed: the locked vault refuses before any decryption, so the ciphertext
	// value is irrelevant — the lock is what gates it.
	_, err = OpenSealed(vlt, box, userID, "anthropic_token", store.SealedWithDEK, []byte("irrelevant"))
	if !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("dek-sealed while locked must return ErrVaultLocked, got %v", err)
	}
}

// TestOpenByIDRoundTrip: a by-id open reaches the named row, scopes the lookup to
// the caller, and takes the KIND FROM THE ROW (the DEK AAD is user_id||kind, so a
// caller-supplied kind would be a guess).
func TestOpenByIDRoundTrip(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, err := box.Seal([]byte("console-key-token"))
	if err != nil {
		t.Fatal(err)
	}
	userID, secretID := uuid.New(), uuid.New()
	st := &fakeStore{byID: store.GetUserSecretCiphertextByIDRow{
		UserID: userID, Kind: "anthropic_token", Ciphertext: sealed, SealedWith: store.SealedWithMaster,
	}}

	plain, err := OpenByID(context.Background(), st, nil, box, userID, secretID)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "console-key-token" {
		t.Fatalf("plaintext = %q, want console-key-token", plain)
	}
	if st.byIDArg.ID != secretID || st.byIDArg.UserID != userID {
		t.Fatalf("lookup was (%v,%v), want (%v,%v) — the query must be owner-scoped",
			st.byIDArg.ID, st.byIDArg.UserID, secretID, userID)
	}
}

// TestOpenByIDForeignRowIsNoSecret is the D11 defense-in-depth check: even if the
// query's owner scope were removed, a row belonging to someone else must surface as
// ErrNoSecret — never as that user's credential.
func TestOpenByIDForeignRowIsNoSecret(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, _ := box.Seal([]byte("someone-elses-token"))
	caller := uuid.New()
	st := &fakeStore{byID: store.GetUserSecretCiphertextByIDRow{
		UserID: uuid.New(), // a DIFFERENT owner than the caller
		Kind:   "anthropic_token", Ciphertext: sealed, SealedWith: store.SealedWithMaster,
	}}

	plain, err := OpenByID(context.Background(), st, nil, box, caller, uuid.New())
	if !errors.Is(err, ErrNoSecret) {
		t.Fatalf("want ErrNoSecret for a row owned by another user, got %v", err)
	}
	if plain != nil {
		t.Fatalf("a foreign row must yield no plaintext, got %q", plain)
	}
}

// TestOpenByIDSentinels: an unknown id and a lookup fault map to the same sentinels
// as the by-kind Open, so callers need one error-handling shape for both.
func TestOpenByIDSentinels(t *testing.T) {
	box, _ := secretbox.New(key)
	uid := uuid.New()

	if _, err := OpenByID(context.Background(), &fakeStore{byIDErr: pgx.ErrNoRows}, nil, box, uid, uuid.New()); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("unknown id: want ErrNoSecret, got %v", err)
	}

	sentinel := errors.New("db down")
	_, err := OpenByID(context.Background(), &fakeStore{byIDErr: sentinel}, nil, box, uid, uuid.New())
	if errors.Is(err, ErrNoSecret) || errors.Is(err, ErrUndecryptable) {
		t.Fatalf("a non-ErrNoRows lookup error must not collapse to a credential sentinel: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("lookup error should wrap the underlying error, got %v", err)
	}

	st := &fakeStore{byID: store.GetUserSecretCiphertextByIDRow{
		UserID: uid, Kind: "anthropic_token",
		Ciphertext: []byte("not a valid sealed blob"), SealedWith: store.SealedWithMaster,
	}}
	if _, err := OpenByID(context.Background(), st, nil, box, uid, uuid.New()); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("undecryptable: want ErrUndecryptable, got %v", err)
	}
}

// TestOpenByIDVaultLocked: the by-id path shares Open's vault dispatch, so a
// dek-sealed row belonging to a locked user is transient (ErrVaultLocked), not a
// terminal credential failure.
func TestOpenByIDVaultLocked(t *testing.T) {
	box, _ := secretbox.New(key)
	vlt := vault.New(box, nil) // empty DEK cache ⇒ locked
	uid := uuid.New()
	st := &fakeStore{byID: store.GetUserSecretCiphertextByIDRow{
		UserID: uid, Kind: "anthropic_token", Ciphertext: []byte("irrelevant"), SealedWith: store.SealedWithDEK,
	}}
	if _, err := OpenByID(context.Background(), st, vlt, box, uid, uuid.New()); !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("dek-sealed while locked must return ErrVaultLocked, got %v", err)
	}
}

// TestOpenByIDOfKindMatchingKind: a row whose kind equals expectedKind opens exactly
// like OpenByID — the kind guard is transparent on a match.
func TestOpenByIDOfKindMatchingKind(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, err := box.Seal([]byte("sk-static-key"))
	if err != nil {
		t.Fatal(err)
	}
	userID, secretID := uuid.New(), uuid.New()
	st := &fakeStore{byID: store.GetUserSecretCiphertextByIDRow{
		UserID: userID, Kind: store.KindOpenAIAPIKey, Ciphertext: sealed, SealedWith: store.SealedWithMaster,
	}}
	plain, err := OpenByIDOfKind(context.Background(), st, nil, box, userID, secretID, store.KindOpenAIAPIKey)
	if err != nil {
		t.Fatalf("matching kind must open: %v", err)
	}
	if string(plain) != "sk-static-key" {
		t.Fatalf("plaintext = %q, want sk-static-key", plain)
	}
	if st.byIDArg.ID != secretID || st.byIDArg.UserID != userID {
		t.Fatalf("lookup was (%v,%v), want (%v,%v) — the query must be owner-scoped",
			st.byIDArg.ID, st.byIDArg.UserID, secretID, userID)
	}
}

// TestOpenByIDOfKindMismatchIsNotFound pins the non-disclosing kind guard (PRD #1147
// audit #6): a codex_auth login blob (which carries a refresh_token) opened via the
// api_key path — expectedKind=openai_api_key — returns the not-found sentinel and NO
// bytes, never the codex_auth plaintext, and never a distinct "wrong kind" error that
// would confirm the row exists.
//
// FAIL-OLD / PASS-FIXED: the pre-existing OpenByID decrypts and returns a row's plaintext
// regardless of its kind, so an api_key path built on it WOULD have disclosed this
// codex_auth blob. OpenByIDOfKind refuses it. This test fails against OpenByID and passes
// against OpenByIDOfKind.
func TestOpenByIDOfKindMismatchIsNotFound(t *testing.T) {
	box, _ := secretbox.New(key)
	loginBlob, err := box.Seal([]byte(`{"access_token":"a","refresh_token":"SECRET-REFRESH-TOKEN"}`))
	if err != nil {
		t.Fatal(err)
	}
	userID, secretID := uuid.New(), uuid.New()
	st := &fakeStore{byID: store.GetUserSecretCiphertextByIDRow{
		UserID: userID, Kind: store.KindCodexAuth, Ciphertext: loginBlob, SealedWith: store.SealedWithMaster,
	}}

	plain, err := OpenByIDOfKind(context.Background(), st, nil, box, userID, secretID, store.KindOpenAIAPIKey)
	if !errors.Is(err, ErrNoSecret) {
		t.Fatalf("kind mismatch: want ErrNoSecret, got %v", err)
	}
	if plain != nil {
		t.Fatalf("kind mismatch must yield no plaintext, got %q", plain)
	}
	if err != nil && strings.Contains(err.Error(), "codex") {
		t.Fatalf("the not-found error must not disclose the row's real kind: %q", err.Error())
	}

	// Sanity: the SAME row DOES open when the expected kind matches — proving the guard,
	// not a broken fixture, is what refused above.
	plain, err = OpenByIDOfKind(context.Background(), st, nil, box, userID, secretID, store.KindCodexAuth)
	if err != nil {
		t.Fatalf("matching kind must open the same row: %v", err)
	}
	if !strings.Contains(string(plain), "SECRET-REFRESH-TOKEN") {
		t.Fatal("matching-kind open returned unexpected plaintext")
	}
}

// TestOpenByIDOfKindForeignRowIsNotFound: the kind guard does not weaken the owner-scope
// defense — a row belonging to another user is still ErrNoSecret, never that user's
// credential, even when the kind matches.
func TestOpenByIDOfKindForeignRowIsNotFound(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, _ := box.Seal([]byte("someone-elses-key"))
	caller := uuid.New()
	st := &fakeStore{byID: store.GetUserSecretCiphertextByIDRow{
		UserID: uuid.New(), // a DIFFERENT owner than the caller
		Kind:   store.KindOpenAIAPIKey, Ciphertext: sealed, SealedWith: store.SealedWithMaster,
	}}
	plain, err := OpenByIDOfKind(context.Background(), st, nil, box, caller, uuid.New(), store.KindOpenAIAPIKey)
	if !errors.Is(err, ErrNoSecret) {
		t.Fatalf("foreign row: want ErrNoSecret, got %v", err)
	}
	if plain != nil {
		t.Fatalf("a foreign row must yield no plaintext, got %q", plain)
	}
}

func TestOpenRoundTripNilVault(t *testing.T) {
	box, _ := secretbox.New(key)
	sealed, err := box.Seal([]byte("s3cr3t-token"))
	if err != nil {
		t.Fatal(err)
	}
	row := store.GetUserSecretCiphertextRow{Ciphertext: sealed, SealedWith: store.SealedWithMaster}
	plain, err := Open(context.Background(), &fakeStore{row: row}, nil, box, uuid.New(), "anthropic_token")
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "s3cr3t-token" {
		t.Fatalf("plaintext = %q, want s3cr3t-token", plain)
	}
}
