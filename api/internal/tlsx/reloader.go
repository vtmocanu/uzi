// Package tlsx backs the api's optional TLS listener (PRD #58 Decision 4): a
// certificate source that survives rotation of the mounted PEM files.
//
// Why this is not two lines of ListenAndServeTLS(cert, key): that reads the pair
// ONCE, at boot. The cert is cert-manager's, mounted from a Secret, and
// cert-manager renews it in place (by default at two thirds of its lifetime)
// without restarting anything. A boot-snapshotted cert therefore serves fine for
// weeks and then expires — the pod is Ready, the files on disk are valid and
// current, and every worker's claim fails certificate verification at once. The
// failure lands months after the deploy that caused it, on nobody's change.
package tlsx

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Reloader serves the current certificate from a PEM pair on disk, re-reading it
// when the files change.
//
// The zero value is not usable; construct with NewReloader.
type Reloader struct {
	certFile string
	keyFile  string
	log      *slog.Logger

	mu     sync.Mutex
	cached *tls.Certificate
	// stamp identifies the file contents the cached certificate was parsed from.
	// Only ever updated together with cached, and only on a successful load.
	stamp stamp
}

// stamp is a cheap identity for the on-disk pair: mtime and size of both files.
// Not a hash — this is checked per handshake, and a rotation that preserved both
// mtime and size of both files (and thus fooled it) is not a thing kubelet's
// atomic symlink swap can produce.
type stamp struct {
	certMod  time.Time
	certSize int64
	keyMod   time.Time
	keySize  int64
}

// NewReloader loads the pair and returns a Reloader serving it.
//
// It parses eagerly so a missing or malformed pair is a loud boot failure rather
// than a listener that binds and then fails every handshake — the same
// fail-at-boot stance config.Load takes on the rest of the runtime settings.
func NewReloader(certFile, keyFile string, log *slog.Logger) (*Reloader, error) {
	if log == nil {
		log = slog.Default()
	}
	r := &Reloader{certFile: certFile, keyFile: keyFile, log: log}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// ServerConfig returns a *tls.Config that sources its certificate from r.
//
// MinVersion is TLS 1.2: Go negotiates 1.3 with everything current (both peers on
// this hop are ours — a Go controller and a Node worker), and the floor exists to
// refuse the obsolete versions, not to pin the modern one.
func (r *Reloader) ServerConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: r.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}

// GetCertificate satisfies tls.Config.GetCertificate. It returns the cached
// certificate, reloading first if the files changed since it was parsed.
//
// The stat runs per handshake, which sounds worse than it is: this listener serves
// a handful of long-lived worker connections, so handshakes are rare and two stats
// on a page-cached file are nothing next to the RSA/ECDSA work of the handshake
// itself. The alternative (a background ticker) buys nothing and adds a goroutine
// whose lifetime someone has to own.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur, err := r.statPair()
	switch {
	case err != nil:
		// A stat failure mid-rotation must not fail the handshake: the cached cert
		// is still valid material, and the next handshake stats again.
		r.log.Warn("tls: stat certificate files failed; serving the cached certificate", "error", err)
		return r.cached, nil
	case cur == r.stamp:
		return r.cached, nil
	}

	if err := r.reload(); err != nil {
		// Same reasoning, one step later: a torn or half-written pair is transient
		// (kubelet swaps the mount atomically, but a hand-edited file or an
		// interrupted write is not this process's problem to have an opinion about).
		// Serve the last good certificate and retry on the next handshake — the
		// stamp is only advanced on success, so this does retry.
		r.log.Error("tls: reloading the rotated certificate failed; serving the cached one", "error", err)
		return r.cached, nil
	}
	return r.cached, nil
}

// reload parses the pair and swaps it in. The caller holds r.mu (NewReloader is
// the exception: nothing else can see r yet).
func (r *Reloader) reload() error {
	// Stat BEFORE the read: if the files are rewritten between the two, the stamp
	// belongs to the older content and the mismatch is caught on the next
	// handshake. Stat after, and the stamp would claim content we never parsed.
	cur, err := r.statPair()
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		// LoadX509KeyPair's error names the files and describes the parse failure;
		// it never echoes key material.
		return fmt.Errorf("tls: load key pair (API_TLS_CERT=%s, API_TLS_KEY=%s): %w", r.certFile, r.keyFile, err)
	}
	if r.cached != nil {
		r.log.Info("tls: reloaded rotated certificate", "cert_file", r.certFile)
	}
	r.cached = &cert
	r.stamp = cur
	return nil
}

func (r *Reloader) statPair() (stamp, error) {
	cs, err := os.Stat(r.certFile)
	if err != nil {
		return stamp{}, fmt.Errorf("stat %s: %w", r.certFile, err)
	}
	ks, err := os.Stat(r.keyFile)
	if err != nil {
		return stamp{}, fmt.Errorf("stat %s: %w", r.keyFile, err)
	}
	return stamp{
		certMod:  cs.ModTime(),
		certSize: cs.Size(),
		keyMod:   ks.ModTime(),
		keySize:  ks.Size(),
	}, nil
}
