package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type forgejoLimitTransport func(*http.Request) (*http.Response, error)

func (f forgejoLimitTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestForgejoTransportErrorCap(t *testing.T) {
	for _, global := range []int64{32 << 20, 1024} {
		for _, status := range []int{200, 299, 300, 401, 403, 429, 503} {
			boundary := 4096
			if global == 1024 {
				boundary = 1024
			}
			for _, size := range []int{boundary, boundary + 1} {
				t.Run(fmt.Sprintf("%d/%d/%d", global, status, size), func(t *testing.T) {
					withForgejoCap(t, global)
					transport := cappedTransport{base: forgejoLimitTransport(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", size)))}, nil
					})}
					req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
					if err != nil {
						t.Fatal(err)
					}
					resp, err := transport.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = resp.Body.Close() }()
					body, err := io.ReadAll(resp.Body)
					overflow := size > boundary && (status/100 != 2 || global == 1024)
					if overflow {
						if !errors.Is(err, errForgeResponseTooLarge) || len(body) != boundary {
							t.Fatalf("read %d bytes, error = %v", len(body), err)
						}
					} else if err != nil || string(body) != strings.Repeat("x", size) {
						t.Fatalf("read %d bytes, error = %v", len(body), err)
					}
				})
			}
		}
	}
}

type forgejoLimitReadCloser struct {
	io.Reader
	closed   bool
	closeErr error
}

func (r *forgejoLimitReadCloser) Close() error { r.closed = true; return r.closeErr }

func TestForgejoCappedBody(t *testing.T) {
	for _, size := range []int{4096, 4097} {
		for _, chunk := range []int{1, 17, 4096, 8192} {
			t.Run(fmt.Sprintf("%d/chunk=%d", size, chunk), func(t *testing.T) {
				closeErr := errors.New("close canary")
				rc := &forgejoLimitReadCloser{Reader: strings.NewReader(strings.Repeat("x", size)), closeErr: closeErr}
				body := &cappedBody{rc: rc, remaining: 4096}
				buf := make([]byte, chunk)
				var got strings.Builder
				var err error
				// At most 4097 reads deliver all fixture bytes, plus one EOF/probe.
				for attempts := 0; attempts < 4098; attempts++ {
					var n int
					n, err = body.Read(buf)
					got.Write(buf[:n])
					if err != nil {
						break
					}
				}
				if got.String() != strings.Repeat("x", 4096) {
					t.Fatalf("delivered %d bytes", got.Len())
				}
				if size == 4096 {
					if err != io.EOF {
						t.Fatalf("exact-cap error = %v", err)
					}
				} else {
					if !errors.Is(err, errForgeResponseTooLarge) {
						t.Fatalf("overflow error = %v", err)
					}
					for attempt := 0; attempt < 3; attempt++ {
						n, err := body.Read(buf)
						if n != 0 || !errors.Is(err, errForgeResponseTooLarge) {
							t.Fatalf("repeat read = %d, %v", n, err)
						}
					}
				}
				if err := body.Close(); err != closeErr || !rc.closed {
					t.Fatalf("Close = %v, closed = %t", err, rc.closed)
				}
			})
		}
	}
}

func TestForgejoRawErrorIgnoresCallerLimit(t *testing.T) {
	const callerLimit = 64
	token := strings.Join([]string{"deadbeef", "CANARY", strings.Repeat("d", 26)}, "")
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversized=%t", oversized), func(t *testing.T) {
			payload := strings.Repeat("x", callerLimit-20) + token + " tail"
			if oversized {
				payload += strings.Repeat("y", 4096)
			}
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/caller-limit": func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(payload))
				},
			})
			d := newForgejoDriver(t, m, token).(*forgejo)
			body, err := d.rawGetLimited(context.Background(), "/caller-limit", callerLimit)
			if err == nil || Class(err) != ErrorClassAuth || errors.Unwrap(err) != nil {
				t.Fatalf("error = %v", err)
			}
			for _, fragment := range []string{token, token[:20], token[20:]} {
				if strings.Contains(err.Error(), fragment) {
					t.Fatalf("token fragment survived: %q", fragment)
				}
			}
			if oversized {
				if body != nil || err.Error() != "forgejo: read response: forgejo: response body exceeds size limit" {
					t.Fatalf("body length = %d, error = %v", len(body), err)
				}
			} else {
				want := "forgejo: GET /caller-limit: status 401: " + strings.Repeat("x", 44) + redactPlaceholder + " tail"
				if string(body) != payload || err.Error() != want {
					t.Fatalf("body length = %d, error = %v", len(body), err)
				}
			}
		})
	}
}
