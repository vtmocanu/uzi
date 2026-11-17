package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadReadsProviderExtraCA(t *testing.T) {
	autoselectEnv(t)
	const env = "UZI_PROVIDER_EXTRA_CA_FILE"
	t.Setenv(env, "x")
	if err := os.Unsetenv(env); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.ProviderExtraCAFile != "" || cfg.ProviderExtraCAPEM != nil {
		t.Fatalf("unset: %q %d bytes, %v; want empty and no error", cfg.ProviderExtraCAFile, len(cfg.ProviderExtraCAPEM), err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	good := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(goodPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env, goodPath)
	cfg, err = Load()
	if err != nil || cfg.ProviderExtraCAFile != goodPath || string(cfg.ProviderExtraCAPEM) != string(good) {
		t.Fatalf("good bundle: %q, %v", cfg.ProviderExtraCAFile, err)
	}

	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"unparseable": junk, "missing": filepath.Join(dir, "absent.pem")} {
		t.Setenv(env, path)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), env) {
			t.Errorf("%s bundle: Load error = %v, want a boot refusal naming %s", name, err, env)
		}
	}
}
