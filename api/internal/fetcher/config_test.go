package fetcher

import (
	"encoding/pem"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseEnv(t *testing.T) map[string]string {
	return map[string]string{
		"UZI_FETCHER_TLS_CERT":   "/tls/tls.crt",
		"UZI_FETCHER_TLS_KEY":    "/tls/tls.key",
		"UZI_API_URL":            "https://uzi-api.uzi.svc.cluster.local:8443/",
		"UZI_FETCHER_TOKEN_FILE": writeFile(t, "token", testServiceToken+"\n"),
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(envOf(baseEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8443" || cfg.ServiceToken != testServiceToken || cfg.APIURL != "https://uzi-api.uzi.svc.cluster.local:8443" ||
		cfg.MaxFileBytes != DefaultMaxFileBytes || cfg.Timeout != DefaultTimeout || cfg.MaxInflight != DefaultMaxInflight ||
		cfg.APICAPool != nil || cfg.TLSCertFile != "/tls/tls.crt" || cfg.TLSKeyFile != "/tls/tls.key" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	ca := newTestCA(t)
	m := baseEnv(t)
	m["UZI_FETCHER_ADDR"] = ":9443"
	m["UZI_API_CA_FILE"] = writeFile(t, "ca.crt", string(pemCert(ca)))
	m["UZI_FETCHER_BLOCKED_CIDRS"] = "10.244.0.0/16, 10.96.0.0/12,fd00:10::/56"
	m["UZI_FETCHER_MAX_FILE_BYTES"] = "1048576"
	m["UZI_FETCHER_TIMEOUT"] = "30s"
	m["UZI_FETCHER_MAX_INFLIGHT"] = "4"
	cfg, err := LoadConfig(envOf(m))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9443" || cfg.APICAPool == nil || cfg.MaxFileBytes != 1<<20 || cfg.Timeout != 30*time.Second || cfg.MaxInflight != 4 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if len(cfg.Policy.extra) != 3 {
		t.Fatalf("policy = %+v", cfg.Policy)
	}
}

func TestLoadConfigRefusals(t *testing.T) {
	cases := map[string]func(map[string]string){
		"no tls cert":        func(m map[string]string) { delete(m, "UZI_FETCHER_TLS_CERT") },
		"no tls key":         func(m map[string]string) { delete(m, "UZI_FETCHER_TLS_KEY") },
		"no api url":         func(m map[string]string) { delete(m, "UZI_API_URL") },
		"http api url":       func(m map[string]string) { m["UZI_API_URL"] = "http://uzi-api:8080" },
		"no token file":      func(m map[string]string) { delete(m, "UZI_FETCHER_TOKEN_FILE") },
		"missing token file": func(m map[string]string) { m["UZI_FETCHER_TOKEN_FILE"] = "/nonexistent/token" },
		"empty token":        func(m map[string]string) { m["UZI_FETCHER_TOKEN_FILE"] = writeFile(t, "empty", " \n") },
		"bad ca":             func(m map[string]string) { m["UZI_API_CA_FILE"] = writeFile(t, "ca", "not pem") },
		"missing ca":         func(m map[string]string) { m["UZI_API_CA_FILE"] = "/nonexistent/ca" },
		"bad cidr":           func(m map[string]string) { m["UZI_FETCHER_BLOCKED_CIDRS"] = "10.0.0.0/33" },
		"bad max bytes":      func(m map[string]string) { m["UZI_FETCHER_MAX_FILE_BYTES"] = "0" },
		"bad timeout":        func(m map[string]string) { m["UZI_FETCHER_TIMEOUT"] = "soon" },
		"bad inflight":       func(m map[string]string) { m["UZI_FETCHER_MAX_INFLIGHT"] = "-1" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := baseEnv(t)
			mut(m)
			if _, err := LoadConfig(envOf(m)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// There is no plain-HTTP mode: the variable the removed dev mode used is ignored, and
// TLS and an https api URL are still required with it set.
func TestLoadConfigHasNoPlainHTTPMode(t *testing.T) {
	m := baseEnv(t)
	m["UZI_FETCHER_INSECURE_DEV_HTTP"] = "true"
	delete(m, "UZI_FETCHER_TLS_CERT")
	delete(m, "UZI_FETCHER_TLS_KEY")
	if _, err := LoadConfig(envOf(m)); err == nil {
		t.Fatal("started without TLS")
	}
	m = baseEnv(t)
	m["UZI_FETCHER_INSECURE_DEV_HTTP"] = "true"
	m["UZI_API_URL"] = "http://localhost:8080"
	if _, err := LoadConfig(envOf(m)); err == nil {
		t.Fatal("accepted an http api URL")
	}
}

// The Options production runs with (Config.FetcherOptions, the only way cmd/fetcher
// builds them; see cmd/fetcher's TestMainBuildsOptionsFromConfigOnly) carry no test
// seam: no testDial, no custom roots, no custom resolver, and the configured policy.
func TestFetcherOptionsHaveNoTestSeam(t *testing.T) {
	m := baseEnv(t)
	m["UZI_FETCHER_BLOCKED_CIDRS"] = "10.244.0.0/16"
	cfg, err := LoadConfig(envOf(m))
	if err != nil {
		t.Fatal(err)
	}
	opts := cfg.FetcherOptions("uzi-fetcher/test")
	if opts.testDial != nil || opts.RootCAs != nil || opts.Resolver != nil {
		t.Fatalf("production options carry a test seam: %+v", opts)
	}
	f := New(opts)
	if f.opts.testDial != nil || f.opts.RootCAs != nil || f.opts.Resolver != net.DefaultResolver {
		t.Fatalf("New added a seam: %+v", f.opts)
	}
	if len(f.opts.Policy.extra) != 1 || f.opts.UserAgent != "uzi-fetcher/test" || f.opts.MaxFileBytes != cfg.MaxFileBytes || f.opts.Timeout != cfg.Timeout {
		t.Fatalf("options %+v", f.opts)
	}
	for _, a := range []string{"127.0.0.1", "::1", "10.244.1.1"} {
		if f.opts.Policy.Allowed(netip.MustParseAddr(a)) {
			t.Errorf("production policy allows %s", a)
		}
	}
}

func pemCert(ca *testCA) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}
