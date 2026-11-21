package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// A plain env row marked sealed_with=master (nil AAD) is never opened for a response.
func TestRunEnvDTORefusesMasterMarkedRow(t *testing.T) {
	// A real nil-AAD ciphertext under a configured master box: without the master-marker
	// check the vault would open it, so only that check keeps the value withheld.
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("legacy-plain"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{vault: vault.New(box, nil, func(context.Context, func(context.Context, vault.Store) error) error {
		return errors.New("unused")
	})}
	row := runEnvRow{Name: "PLAIN_ONE", Ciphertext: sealed, SealedWith: store.SealedWithMaster,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	dto, locked := h.runEnvDTO(uuid.New(), uuid.New(), row, false)
	if dto.Value != nil || !dto.ValueWithheld || locked {
		t.Fatalf("master-marked row: %+v locked=%v", dto, locked)
	}
}

// A locked vault withholds a plain value even when its row still holds master_aad
// material the master key would open (a row read before the vault was protected and
// locked): the list asks RequiresUnlock after the read and must not open such a row.
func TestRunEnvDTOWithholdsWhenVaultLocked(t *testing.T) {
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{vault: vault.New(box, nil, func(context.Context, func(context.Context, vault.Store) error) error {
		return errors.New("unused")
	})}
	userID, repoID := uuid.New(), uuid.New()
	kind := runenv.AADKind(runenv.ScopeRepo, &repoID, false, "PLAIN_ONE")
	// master_aad layout (vault.secretAAD): user id, NUL, kind. The unlocked control below
	// fails loudly if this drifts from the vault's own layout.
	aad := append(append(append([]byte{}, userID[:]...), 0), kind...)
	sealed, err := box.SealWithAAD([]byte("stale-plain"), aad)
	if err != nil {
		t.Fatal(err)
	}
	row := runEnvRow{Name: "PLAIN_ONE", Ciphertext: sealed, SealedWith: store.SealedWithMasterAAD, AadVersion: runenv.AADVersion1,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	// Control: unlocked, the same row opens, so only the vaultLocked branch withholds it.
	if dto, _ := h.runEnvDTO(userID, repoID, row, false); dto.Value == nil || *dto.Value != "stale-plain" {
		t.Fatalf("unlocked control did not open: %+v", dto)
	}
	dto, locked := h.runEnvDTO(userID, repoID, row, true)
	if dto.Value != nil || !dto.ValueWithheld || !locked {
		t.Fatalf("locked vault: %+v locked=%v", dto, locked)
	}
}

// A write response asks the vault for its lock state after the write committed: a vault
// that reports locked (here its state lookup fails, which fails closed) withholds a
// master_aad plain value the master key would otherwise open.
func TestRunEnvWriteDTOWithholdsWhenVaultLocked(t *testing.T) {
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{vault: vault.New(box, nil, func(context.Context, func(context.Context, vault.Store) error) error {
		return errors.New("state lookup unavailable")
	})}
	userID, repoID := uuid.New(), uuid.New()
	kind := runenv.AADKind(runenv.ScopeRepo, &repoID, false, "PLAIN_ONE")
	aad := append(append(append([]byte{}, userID[:]...), 0), kind...)
	sealed, err := box.SealWithAAD([]byte("stale-plain"), aad)
	if err != nil {
		t.Fatal(err)
	}
	row := runEnvRow{Name: "PLAIN_ONE", Ciphertext: sealed, SealedWith: store.SealedWithMasterAAD, AadVersion: runenv.AADVersion1,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}
	if dto, _ := h.runEnvDTO(userID, repoID, row, false); dto.Value == nil {
		t.Fatalf("control: the row must open when not reported locked: %+v", dto)
	}
	dto := h.runEnvWriteDTO(context.Background(), userID, repoID, row)
	if dto.Value != nil || !dto.ValueWithheld {
		t.Fatalf("write response under a locked vault: %+v", dto)
	}
}

func TestHasLoneSurrogateEscape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"lone high", `{"value":"\ud800"}`, true},
		{"lone low", `{"value":"\udc00"}`, true},
		{"reversed pair", `{"value":"\udc00\ud800"}`, true},
		{"high at end of string", `{"value":"abc\uD83D"}`, true},
		{"uppercase hex", `{"value":"\uDBFF"}`, true},
		{"mixed case low", `{"value":"\uDfFf"}`, true},
		{"high then non-surrogate", `{"value":"\ud800\u0041"}`, true},
		{"in description", `{"description":"\udc00"}`, true},
		{"valid pair", `{"value":"\ud83d\ude00"}`, false},
		{"valid pair uppercase", `{"value":"\uD83D\uDE00"}`, false},
		{"escaped backslash then text", `{"value":"\\ud800"}`, false},
		{"escaped backslash then pair", `{"value":"\\\ud83d\ude00"}`, false},
		{"plain bmp escape", `{"value":"\u00e9"}`, false},
		{"literal emoji", "{\"value\":\"\U0001F600\"}", false},
		{"truncated escape", `\u12`, false},
	}
	for _, c := range cases {
		if got := hasLoneSurrogateEscape([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %v, want %v for %s", c.name, got, c.want, c.body)
		}
	}
}
