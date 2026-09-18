package main

import (
	"errors"
	"net"
	"testing"
)

// fakeListener is a no-op net.Listener the bind-order test hands back from its injectable
// listen func. It records nothing itself; the recording lives in the test's closure.
type fakeListener struct{ addr string }

func (f fakeListener) Accept() (net.Conn, error) { return nil, errors.New("not used") }
func (f fakeListener) Close() error              { return nil }
func (f fakeListener) Addr() net.Addr            { return nil }

// TestBindWorkerListenersReadyOrder pins PRD #1390 M1 (D1): SetReadyAt (here `setReady`) fires
// only AFTER every enabled listener has bound — after both when TLS is on, and after the plain
// one alone when TLS is off — and never at all when a bind fails.
func TestBindWorkerListenersReadyOrder(t *testing.T) {
	t.Run("tls enabled: ready fires after both binds", func(t *testing.T) {
		var events []string
		listen := func(_, address string) (net.Listener, error) {
			events = append(events, "bind:"+address)
			return fakeListener{addr: address}, nil
		}
		setReady := func() { events = append(events, "ready") }

		lnPlain, lnTLS, err := bindWorkerListeners("127.0.0.1:8080", "127.0.0.1:8443", true, listen, setReady)
		if err != nil {
			t.Fatalf("bindWorkerListeners err = %v, want nil", err)
		}
		if lnPlain == nil || lnTLS == nil {
			t.Fatalf("both listeners must be returned, got plain=%v tls=%v", lnPlain, lnTLS)
		}
		// Ready must be LAST, after both binds, and both binds must have happened.
		want := []string{"bind:127.0.0.1:8080", "bind:127.0.0.1:8443", "ready"}
		if !equalStrs(events, want) {
			t.Fatalf("event order = %v, want %v", events, want)
		}
	})

	t.Run("tls disabled: ready fires after the plain bind only", func(t *testing.T) {
		var events []string
		listen := func(_, address string) (net.Listener, error) {
			events = append(events, "bind:"+address)
			return fakeListener{addr: address}, nil
		}
		setReady := func() { events = append(events, "ready") }

		lnPlain, lnTLS, err := bindWorkerListeners("127.0.0.1:8080", "127.0.0.1:8443", false, listen, setReady)
		if err != nil {
			t.Fatalf("bindWorkerListeners err = %v, want nil", err)
		}
		if lnPlain == nil {
			t.Fatal("plain listener must be returned")
		}
		if lnTLS != nil {
			t.Fatalf("tls listener must be nil when disabled, got %v", lnTLS)
		}
		want := []string{"bind:127.0.0.1:8080", "ready"}
		if !equalStrs(events, want) {
			t.Fatalf("event order = %v, want %v", events, want)
		}
	})

	t.Run("plain bind fails: ready never fires", func(t *testing.T) {
		var events []string
		listen := func(_, address string) (net.Listener, error) {
			events = append(events, "bind:"+address)
			return nil, errors.New("bind refused")
		}
		setReady := func() { events = append(events, "ready") }

		_, _, err := bindWorkerListeners("127.0.0.1:8080", "127.0.0.1:8443", true, listen, setReady)
		if err == nil {
			t.Fatal("bindWorkerListeners must return the bind error")
		}
		for _, e := range events {
			if e == "ready" {
				t.Fatal("setReady must NOT fire when a bind fails")
			}
		}
	})

	t.Run("tls bind fails after plain succeeds: ready never fires and plain is closed", func(t *testing.T) {
		var events []string
		var closed bool
		listen := func(_, address string) (net.Listener, error) {
			events = append(events, "bind:"+address)
			if address == "127.0.0.1:8443" {
				return nil, errors.New("tls bind refused")
			}
			return closeRecordingListener{onClose: func() { closed = true }}, nil
		}
		setReady := func() { events = append(events, "ready") }

		_, _, err := bindWorkerListeners("127.0.0.1:8080", "127.0.0.1:8443", true, listen, setReady)
		if err == nil {
			t.Fatal("bindWorkerListeners must return the tls bind error")
		}
		for _, e := range events {
			if e == "ready" {
				t.Fatal("setReady must NOT fire when the tls bind fails")
			}
		}
		if !closed {
			t.Fatal("the already-bound plain listener must be closed on a later tls-bind failure")
		}
	})
}

// closeRecordingListener records that Close was called, for the partial-failure cleanup test.
type closeRecordingListener struct{ onClose func() }

func (l closeRecordingListener) Accept() (net.Conn, error) { return nil, errors.New("not used") }
func (l closeRecordingListener) Close() error              { l.onClose(); return nil }
func (l closeRecordingListener) Addr() net.Addr            { return nil }

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
