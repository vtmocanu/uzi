package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const boundedLookupRef = "refs/uzi-checkpoints/bounded"
const boundedLookupTip = "1111111111111111111111111111111111111111"

func refPacket(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// sizedAdvertisement places the wanted ref first, followed by unrelated refs.
// The entire advertisement remains valid, including its final flush packet.
func sizedAdvertisement(size int) []byte {
	var body strings.Builder
	body.Grow(size)
	body.WriteString(refPacket("# service=git-upload-pack\n") + "0000" +
		refPacket(boundedLookupTip+" "+boundedLookupRef+"\x00multi_ack\n"))
	for i := 0; body.Len()+4 < size; i++ {
		remaining := size - body.Len() - 4
		n := min(remaining, 1024)
		if remaining-n > 0 && remaining-n < 80 {
			n = remaining - 80
		}
		prefix := boundedLookupTip + " " + fmt.Sprintf("refs/heads/padding-%06d-", i)
		body.WriteString(refPacket(prefix + strings.Repeat("x", n-4-len(prefix)-1) + "\n"))
	}
	body.WriteString("0000")
	return []byte(body.String())
}

func TestListRefTipsAdvertisementBound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		stream    bool
		wantLarge bool
	}{
		{"small", 256, false, false},
		{"exact", MaxRefAdvertisementBytes, false, false},
		{"over", MaxRefAdvertisementBytes + 1, false, true},
		{"stream-exact", MaxRefAdvertisementBytes, true, false},
		{"stream-over", MaxRefAdvertisementBytes + 1, true, true},
		{"auditor-100000-refs", 7200048, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := sizedAdvertisement(tc.size)
			if tc.name == "auditor-100000-refs" {
				var advertisement strings.Builder
				advertisement.WriteString(refPacket("# service=git-upload-pack\n") + "0000")
				advertisement.WriteString(refPacket(boundedLookupTip + " " + boundedLookupRef + "\x00multi_ack\n"))
				for i := 1; i < 100000; i++ {
					advertisement.WriteString(refPacket(boundedLookupTip + " " + fmt.Sprintf("refs/heads/%015d", i) + "\n"))
				}
				advertisement.WriteString("0000")
				body = []byte(advertisement.String())
				t.Logf("advertisement: 100000 refs, %d bytes; requesting one ref", len(body))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Query().Get("service") != "git-upload-pack" {
					t.Errorf("unexpected discovery: %s %s", req.Method, req.URL)
				}
				user, password, ok := req.BasicAuth()
				if !ok || user != "bot" || password != "fixture-password" {
					t.Error("missing discovery auth")
				}
				w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
				if tc.stream {
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			tips, err := ListRefTips(context.Background(), ListRefsOptions{CloneURL: server.URL + "/repo", Username: "bot", PAT: "fixture-password"}, boundedLookupRef)
			if tc.wantLarge {
				if !errors.Is(err, ErrRefAdvertisementTooLarge) || tips != nil {
					t.Fatalf("lookup = %v, %v; want nil tips and typed overflow", tips, err)
				}
			} else if err != nil || len(tips) != 1 || tips[boundedLookupRef] != boundedLookupTip {
				t.Fatalf("lookup = %v, %v; want the small requested lookup", tips, err)
			}
		})
	}
}

type advertisementFixtureTransport struct {
	body   []byte
	length int64
	read   int
	closed bool
}
type advertisementFixtureBody struct {
	*advertisementFixtureTransport
	reader *bytes.Reader
}

func (b *advertisementFixtureBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}
func (b *advertisementFixtureBody) Close() error { b.closed = true; return nil }
func (f *advertisementFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-git-upload-pack-advertisement"}}, ContentLength: f.length,
		Body: &advertisementFixtureBody{f, bytes.NewReader(f.body)}, Request: req}, nil
}

func TestListRefTipsMisleadingContentLength(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		length int64
		large  bool
	}{
		{"claimed-small", MaxRefAdvertisementBytes + 1, 1, true},
		{"claimed-large", 256, MaxRefAdvertisementBytes + 100, false},
		{"no-length", MaxRefAdvertisementBytes + 1, -1, true},
		{"target-and-flush-before-overflow", MaxRefAdvertisementBytes + 1, 256, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := sizedAdvertisement(tc.size)
			if tc.name == "target-and-flush-before-overflow" {
				body = append(sizedAdvertisement(256), bytes.Repeat([]byte("x"), tc.size-256)...)
			}
			fixture := &advertisementFixtureTransport{body: body, length: tc.length}
			previous := brokerHTTPClient.Transport
			brokerHTTPClient.Transport = fixture
			defer func() { brokerHTTPClient.Transport = previous }()
			tips, err := ListRefTips(context.Background(), ListRefsOptions{CloneURL: "http://example.com/repo"}, boundedLookupRef)
			if tc.large {
				if !errors.Is(err, ErrRefAdvertisementTooLarge) || tips != nil {
					t.Fatalf("lookup = %v, %v; want typed overflow without partial proof", tips, err)
				}
			} else if err != nil || tips[boundedLookupRef] != boundedLookupTip {
				t.Fatalf("lookup = %v, %v", tips, err)
			}
			if fixture.read > MaxRefAdvertisementBytes+1 || !fixture.closed {
				t.Fatalf("body read=%d closed=%v", fixture.read, fixture.closed)
			}
		})
	}
}

func TestListRefTipsDiscoveryRedirects(t *testing.T) {
	for _, mode := range []string{"same-origin", "different-port", "different-scheme"} {
		t.Run(mode, func(t *testing.T) {
			forbiddenRequests := 0
			forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				forbiddenRequests++
				t.Error("off-origin destination was reached")
			}))
			defer forbidden.Close()
			redirectURL := ""
			lookups := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/repo/info/refs" {
					http.Redirect(w, req, redirectURL, http.StatusFound)
					return
				}
				lookups++
				user, password, ok := req.BasicAuth()
				if !ok || user != "bot" || password != "fixture-password" {
					t.Error("same-origin redirect lost auth")
				}
				w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
				_, _ = w.Write(sizedAdvertisement(256))
			}))
			defer server.Close()
			switch mode {
			case "same-origin":
				redirectURL = server.URL + "/canonical/info/refs?service=git-upload-pack"
			case "different-port":
				redirectURL = forbidden.URL + "/canonical/info/refs?service=git-upload-pack"
			case "different-scheme":
				redirectURL = strings.Replace(server.URL, "http:", "https:", 1) + "/canonical/info/refs?service=git-upload-pack"
			}
			tips, err := ListRefTips(context.Background(), ListRefsOptions{CloneURL: server.URL + "/repo", Username: "bot", PAT: "fixture-password"}, boundedLookupRef)
			if mode == "same-origin" {
				if err != nil || tips[boundedLookupRef] != boundedLookupTip || lookups != 1 {
					t.Fatalf("redirected lookup = %v, %v; requests=%d", tips, err, lookups)
				}
			} else if err == nil || tips != nil || forbiddenRequests != 0 || lookups != 0 {
				t.Fatalf("off-origin lookup = %v, %v; forbidden=%d lookups=%d", tips, err, forbiddenRequests, lookups)
			}
		})
	}
}
