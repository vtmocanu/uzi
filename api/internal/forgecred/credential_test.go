package forgecred

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestAppEnvelopeCannotBeOpenedByLegacyAPI(t *testing.T) {
	box := testBox(t)
	value := `{"auth_type":"github_app","private_key":"runtime-credential-canary"}`
	sealed, err := Seal(box, value)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Open(box, sealed)
	if err != nil || string(plain) != value {
		t.Fatal("new API could not open its App envelope")
	}
	if _, err := box.Open(sealed); err == nil {
		t.Fatal("legacy API decrypted an App private-key document")
	}
	if _, err := box.Open(sealed[len(appPrefix):]); err == nil {
		t.Fatal("removing the prefix defeated the encryption domain")
	}
	if _, err := box.OpenWithAAD(sealed[len(appPrefix):], []byte("wrong-domain")); err == nil {
		t.Fatal("incorrect domain opened the key")
	}
	for _, position := range []int{0, len(sealed) - 1} {
		bad := bytes.Clone(sealed)
		bad[position] ^= 1
		if _, err := Open(box, bad); err == nil {
			t.Fatal("tampered App envelope was accepted")
		}
	}
}

func TestPATEnvelopeRemainsLegacyCompatible(t *testing.T) {
	box := testBox(t)
	value := "opaque-pat-fixture"
	sealed, err := Seal(box, value)
	if err != nil {
		t.Fatal(err)
	}
	for _, open := range []func([]byte) ([]byte, error){box.Open, func(b []byte) ([]byte, error) { return Open(box, b) }} {
		plain, err := open(sealed)
		if err != nil || string(plain) != value {
			t.Fatal("PAT compatibility changed")
		}
	}
}

func TestUnversionedAppDocumentsFailClosed(t *testing.T) {
	box := testBox(t)
	old, err := box.Seal([]byte(`{"auth_type":"github_app","private_key":"canary"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(box, old); err == nil {
		t.Fatal("unversioned App envelope was accepted")
	}
	if _, err := Seal(nil, "fixture"); err == nil {
		t.Fatal("nil encryption box accepted")
	}
	if _, err := Open(nil, old); err == nil {
		t.Fatal("nil decryption box accepted")
	}
}
