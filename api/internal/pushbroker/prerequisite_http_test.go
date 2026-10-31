package pushbroker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

func prereqOpts(url string) pushbroker.PrereqCreateOptions {
	return pushbroker.PrereqCreateOptions{CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(), DefaultBranch: "main", CaptureID: heldRunID}
}

func TestPrereqCreateReceivePack(t *testing.T) {
	for _, race := range []string{"none", "same", "foreign"} {
		t.Run(race, func(t *testing.T) {
			f, remote, url, base, tip, _ := heldHTTPFixture(t)
			// Observe the actual POST before the backend can reject an old-value mismatch.
			commands := make(chan []byte, 8)
			proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
					body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					commands <- body
				}
				remote.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			url = proxy.URL + strings.TrimPrefix(url, remote.srv.URL)
			p, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(url))
			if err != nil {
				t.Fatal(err)
			}
			if p.Tip() != base || f.originRef(p.Ref()) != "" {
				t.Fatal("prepare must capture default without writing")
			}
			if race != "none" {
				value := base
				if race == "foreign" {
					value = tip
				}
				f.git("push", "origin", value+":"+p.Ref())
			}
			out, err := pushbroker.SendPrereqCreate(context.Background(), p)
			select {
			case body := <-commands:
				scanner := pktline.NewScanner(bytes.NewReader(body))
				if !scanner.Scan() {
					t.Fatalf("missing POST command: %v", scanner.Err())
				}
				command := strings.SplitN(string(scanner.Bytes()), "\x00", 2)[0]
				want := strings.Repeat("0", 40) + " " + base + " " + p.Ref()
				if command != want {
					t.Fatalf("wire create command = %q, want zero-old command %q", command, want)
				}
			default:
				t.Fatal("missing receive-pack POST")
			}
			switch race {
			case "none":
				if err != nil || out != pushbroker.PrereqCreateCreated {
					t.Fatalf("%v %v", out, err)
				}
			case "same":
				if out != pushbroker.PrereqCreateCreated || err != nil {
					t.Fatalf("matching readback: %v %v", out, err)
				}
			case "foreign":
				if out != pushbroker.PrereqCreateForeignTip || !errors.Is(err, pushbroker.ErrRefExists) {
					t.Fatalf("%v %v", out, err)
				}
			}
			if _, err := pushbroker.SendPrereqCreate(context.Background(), p); !errors.Is(err, pushbroker.ErrInvalidRef) {
				t.Fatalf("resend: %v", err)
			}
			if _, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(url)); !errors.Is(err, pushbroker.ErrRefExists) {
				t.Fatalf("preexisting: %v", err)
			}
			if race == "none" {
				if err := pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: url, Ref: p.Ref(), ExpectedOldTip: base}); err != nil {
					t.Fatal(err)
				}
				if f.originRef(p.Ref()) != "" {
					t.Fatal("matching CAS did not remove pin")
				}
				next, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(url))
				if err != nil {
					t.Fatal(err)
				}
				if out, err := pushbroker.SendPrereqCreate(context.Background(), next); out != pushbroker.PrereqCreateCreated || err != nil {
					t.Fatalf("new capture attempt: %v %v", out, err)
				}
				f.git("push", "origin", tip+":"+p.Ref())
				if err := pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: url, Ref: p.Ref(), ExpectedOldTip: base}); err != nil {
					t.Fatal(err)
				}
				if f.originRef(p.Ref()) != tip {
					t.Fatal("moved tip deleted")
				}
			}
		})
	}
}

func TestPrereqCreateAuthority(t *testing.T) {
	pin := "refs/uzi-prereq/" + heldRunID.String()
	for _, pair := range [][2]string{
		{pin, "refs/heads/main"},
		{pin, "refs/uzi-checkpoints/x"},
		{"refs/uzi-recovery/x", pin},
		{"refs/uzi-salvage/x", pin},
		{"refs/uzi-held/x/1", pin},
	} {
		err := pushbroker.CreateRef(context.Background(), pushbroker.CreateRefOptions{Ref: pair[0], SourceRef: pair[1], Tip: strings.Repeat("a", 40)})
		if !errors.Is(err, pushbroker.ErrInvalidRef) {
			t.Fatalf("%v: %v", pair, err)
		}
	}
}

func TestPrereqBoundedAdvertisements(t *testing.T) {
	for _, stage := range []string{"prepare", "send", "readback", "delete-list", "delete-receive", "delete-readback"} {
		t.Run(stage, func(t *testing.T) {
			f, remote, url, base, _, _ := heldHTTPFixture(t)
			p, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(url))
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(stage, "delete") {
				f.git("push", "origin", base+":"+p.Ref())
			}
			uploads, receives := 0, 0
			active := stage != "readback"
			proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc := r.URL.Query().Get("service")
				if svc == "git-upload-pack" {
					uploads++
				}
				if svc == "git-receive-pack" {
					receives++
				}
				overflow := stage == "prepare" && uploads == 1 ||
					stage == "send" && receives == 1 ||
					stage == "readback" && uploads == 1 ||
					stage == "delete-list" && uploads == 1 ||
					stage == "delete-receive" && receives == 1 ||
					stage == "delete-readback" && uploads == 2
				if active && overflow && r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
					_, _ = w.Write([]byte(strings.Repeat("x", pushbroker.MaxRefAdvertisementBytes+1)))
					return
				}
				// Delegate to the existing fixture handler, retaining the request context.
				remote.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			proxyURL := proxy.URL + "/" + strings.TrimPrefix(url, remote.srv.URL+"/")
			o := prereqOpts(proxyURL)
			switch {
			case stage == "prepare":
				_, err = pushbroker.PreparePrereqCreate(context.Background(), o)
			case strings.HasPrefix(stage, "delete"):
				err = pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: proxyURL, Ref: p.Ref(), ExpectedOldTip: base})
			default:
				prepared, prepErr := pushbroker.PreparePrereqCreate(context.Background(), o)
				if prepErr != nil {
					t.Fatal(prepErr)
				}
				uploads = 0
				active = true
				_, err = pushbroker.SendPrereqCreate(context.Background(), prepared)
			}
			if !errors.Is(err, pushbroker.ErrRefAdvertisementTooLarge) {
				t.Fatalf("overflow: %v", err)
			}
		})
	}
}

func TestPrereqRedirects(t *testing.T) {
	for _, stage := range []string{"prepare", "send", "readback", "delete-receive", "delete-readback"} {
		t.Run(stage, func(t *testing.T) {
			f, remote, url, base, _, _ := heldHTTPFixture(t)
			bad := newForbiddenRemote(t, true)
			o := prereqOpts(url)
			p, err := pushbroker.PreparePrereqCreate(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(stage, "delete") {
				f.git("push", "origin", base+":"+p.Ref())
			}
			remote.redirSvc = "git-upload-pack"
			remote.redirNth = 2
			if stage == "send" || stage == "delete-receive" {
				remote.redirSvc = "git-receive-pack"
				remote.redirNth = 1
			}
			if stage == "delete-readback" {
				remote.redirNth = 3
			}
			remote.redirTo = func(path, query string) string { return bad.srv.URL + path + "?" + query }
			switch {
			case stage == "prepare":
				_, err = pushbroker.PreparePrereqCreate(context.Background(), o)
			case strings.HasPrefix(stage, "delete"):
				err = pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: url, Ref: p.Ref(), ExpectedOldTip: base})
			default:
				_, err = pushbroker.SendPrereqCreate(context.Background(), p)
			}
			if err == nil || remote.redirected.Load() != 1 || bad.hits.Load() != 0 {
				t.Fatalf("redirect err=%v redirected=%d hits=%d", err, remote.redirected.Load(), bad.hits.Load())
			}
		})
	}
}

func TestPrereqCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pushbroker.PreparePrereqCreate(ctx, prereqOpts("https://example.invalid/repo"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("deadline: %v", err)
	}
}
