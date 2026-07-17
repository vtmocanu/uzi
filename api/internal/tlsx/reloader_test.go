package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair generates a self-signed cert for cn and writes the PEM pair to dir,
// returning the two paths. Regenerating with the same paths is how a test
// simulates cert-manager rotating the mounted Secret in place.
func writePair(t *testing.T, dir, cn string) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// commonName parses the certificate the reloader is currently serving. The tests
// use it as the identity of a generation of the pair.
func commonName(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatalf("parse served certificate: %v", err)
	}
	return leaf.Subject.CommonName
}

func TestNewReloaderServesTheMountedPair(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "api.uzi.svc")

	r, err := NewReloader(certFile, keyFile, nil)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	got, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cn := commonName(t, got); cn != "api.uzi.svc" {
		t.Fatalf("served CN = %q, want api.uzi.svc", cn)
	}
}

// A malformed or missing pair must abort boot. The listener binding and then
// failing every handshake is the outcome this rules out: the pod would go Ready
// and every worker claim would fail, which reads as a network problem.
func TestNewReloaderFailsLoudOnABadPair(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "api.uzi.svc")

	t.Run("missing", func(t *testing.T) {
		if _, err := NewReloader(filepath.Join(dir, "absent.crt"), keyFile, nil); err == nil {
			t.Fatal("NewReloader on a missing cert file: want error, got nil")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.crt")
		writeFile(t, bad, []byte("this is not a PEM certificate"))
		if _, err := NewReloader(bad, keyFile, nil); err == nil {
			t.Fatal("NewReloader on a malformed cert: want error, got nil")
		}
	})
	t.Run("mismatched", func(t *testing.T) {
		// A cert and a key from different pairs: individually parseable, together
		// meaningless. This is the shape of a botched manual Secret edit.
		other := t.TempDir()
		_, otherKey := writePair(t, other, "someone.else")
		if _, err := NewReloader(certFile, otherKey, nil); err == nil {
			t.Fatal("NewReloader on a mismatched pair: want error, got nil")
		}
	})
}

// The point of the package: cert-manager renews in place, and the new material
// must be served without a restart.
func TestGetCertificatePicksUpARotatedPair(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "before.rotation")

	r, err := NewReloader(certFile, keyFile, nil)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	first, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cn := commonName(t, first); cn != "before.rotation" {
		t.Fatalf("served CN = %q, want before.rotation", cn)
	}

	// Rotate: same paths, new material. The mtime must move for the stamp to
	// notice; a test fast enough to land in the same filesystem timestamp tick
	// would otherwise be testing nothing.
	time.Sleep(10 * time.Millisecond)
	writePair(t, dir, "after.rotation")

	second, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after rotation: %v", err)
	}
	if cn := commonName(t, second); cn != "after.rotation" {
		t.Fatalf("after rotation served CN = %q, want after.rotation (the reloader kept the boot-time snapshot)", cn)
	}
}

// An unchanged pair must not be re-parsed: the handshake path takes the stamp
// shortcut, and a fresh *tls.Certificate per handshake would be pure garbage.
func TestGetCertificateReusesTheCachedCertificateWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "api.uzi.svc")

	r, err := NewReloader(certFile, keyFile, nil)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	first, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	second, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if first != second {
		t.Fatal("GetCertificate re-parsed an unchanged pair; want the cached pointer")
	}
}

// Rotation is not atomic from this process's point of view, and a handshake that
// lands mid-write must not fail. The last good certificate is strictly better than
// an error: it is still valid material, and the next handshake retries.
func TestGetCertificateServesTheCachedPairWhenAReloadFails(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "api.uzi.svc")

	r, err := NewReloader(certFile, keyFile, nil)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	writeFile(t, certFile, []byte("-----BEGIN CERTIFICATE-----\ntorn write\n"))

	got, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate on a torn pair: want the cached certificate, got error %v", err)
	}
	if cn := commonName(t, got); cn != "api.uzi.svc" {
		t.Fatalf("served CN = %q, want the cached api.uzi.svc", cn)
	}

	// And it must RETRY: the failed load must not have advanced the stamp, or the
	// pair would stay stale until a restart.
	time.Sleep(10 * time.Millisecond)
	writePair(t, dir, "recovered")
	got, err = r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after recovery: %v", err)
	}
	if cn := commonName(t, got); cn != "recovered" {
		t.Fatalf("after recovery served CN = %q, want recovered (a failed reload poisoned the stamp)", cn)
	}
}

// The whole point of serving TLS: a real client completes a handshake and verifies
// the certificate against the CA it was given. Nothing below trusts the system
// roots — this is exactly the posture the controller and the hosted workers take.
func TestServerConfigServesAVerifiableHandshake(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "api.uzi.svc")

	r, err := NewReloader(certFile, keyFile, nil)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", r.ServerConfig())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("ok"))
	}()

	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		t.Fatal("AppendCertsFromPEM: no certificate parsed")
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		RootCAs:    roots,
		ServerName: "api.uzi.svc",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("handshake with CA verification: %v", err)
	}
	defer conn.Close()
}
