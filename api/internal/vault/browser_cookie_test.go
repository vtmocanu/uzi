package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"strings"
	"testing"
)

func TestBrowserCookieAndCrypto(t *testing.T) {
	owner, slot := uuid.New(), uuid.New()
	k := &BrowserKey{slot: slot}
	copy(k.key[:], bytes.Repeat([]byte{37}, 32))
	value, err := k.CookieValue()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBrowserKey(value)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Destroy()
	if decoded.slot != slot || decoded.key != k.key {
		t.Fatal("cookie round trip")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%x"} {
		if got := fmt.Sprintf(format, k); got != "[vault browser key]" {
			t.Fatalf("unsafe format %q", format)
		}
		if got := fmt.Sprintf(format, *k); got != "[vault browser key]" {
			t.Fatalf("unsafe value format %q", format)
		}
	}
	if _, err := json.Marshal(k); err == nil {
		t.Fatal("key JSON allowed")
	}
	for _, bad := range []string{"", value + "x", "v2" + value[2:], strings.ToUpper(value), value[:82] + "=", strings.Repeat("x", 10000)} {
		if key, err := DecodeBrowserKey(bad); err == nil || key != nil {
			t.Fatal("malformed cookie accepted")
		}
	}
	box, _ := secretbox.New(k.key[:])
	dek := bytes.Repeat([]byte{19}, 32)
	wrapped, err := box.SealWithAAD(dek, browserAAD(owner, slot))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := box.OpenWithAAD(wrapped, browserAAD(owner, slot))
	if err != nil || !bytes.Equal(opened, dek) {
		t.Fatal("Ks and wrapped DEK must open")
	}
	for _, aad := range [][]byte{browserAAD(uuid.New(), slot), browserAAD(owner, uuid.New()), wrapAAD(owner)} {
		if _, err := box.OpenWithAAD(wrapped, aad); err == nil {
			t.Fatal("AAD domain mismatch accepted")
		}
	}
	master, _ := secretbox.New(bytes.Repeat([]byte{72}, 32))
	if _, err := master.OpenWithAAD(wrapped, browserAAD(owner, slot)); err == nil {
		t.Fatal("master opened browser slot")
	}
	dbFieldsKey, _ := secretbox.New(bytes.Repeat([]byte{0}, 32))
	if _, err := dbFieldsKey.OpenWithAAD(wrapped, browserAAD(owner, slot)); err == nil {
		t.Fatal("DB metadata opened browser slot")
	}
	k.Destroy()
	if _, err := k.CookieValue(); err == nil {
		t.Fatal("destroyed key encoded")
	}
}
