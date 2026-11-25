package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestWorkerSecretActualDataLimit(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		api, extra, fetcher, tokenBytes, wantBytes int
		isolated, reject                           bool
	}{
		{name: "loader maximum duplicated", api: 400, extra: 512 * 1024, tokenBytes: 6, wantBytes: 1048983, reject: true},
		{name: "exact boundary", api: 400, extra: 524084, tokenBytes: 7, wantBytes: 1048576},
		{name: "actual token crosses boundary", api: 400, extra: 524084, tokenBytes: 8, wantBytes: 1048577, reject: true},
		{name: "no CA", tokenBytes: 7, wantBytes: 7},
		{name: "API only boundary", api: 1048569, tokenBytes: 7, wantBytes: 1048576},
		{name: "API only oversized", api: 1048570, tokenBytes: 7, wantBytes: 1048577, reject: true},
		{name: "native isolated excludes extra copy", api: 400, extra: 512 * 1024, fetcher: 100, tokenBytes: 6, wantBytes: 524795, isolated: true},
		{name: "isolated fetcher crosses boundary", fetcher: 1048571, tokenBytes: 6, wantBytes: 1048577, isolated: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, client := newMat(t)
			m.cfg.APICAPEM = []byte(strings.Repeat("A", tc.api))
			m.cfg.ExtraCAPEM = []byte(strings.Repeat("E", tc.extra))
			m.cfg.FetcherCAPEM = []byte(strings.Repeat("F", tc.fetcher))
			m.cfg.IsolatedNamespace = "isolated"
			w := newWorkerFixture(protocol.DesiredWorker{ID: "size-boundary", Template: "base", Size: "m", Isolated: tc.isolated, JoinToken: token(strings.Repeat("T", tc.tokenBytes))})
			rendered := RenderSecret(m.cfg, w, *w.JoinToken)
			size := 0
			for _, value := range rendered.Data {
				size += len(value)
			}
			if size != tc.wantBytes {
				t.Fatalf("fixture data bytes = %d, want %d", size, tc.wantBytes)
			}
			err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil)
			if tc.reject {
				if err == nil {
					t.Error("oversized actual Secret accepted")
				} else if !strings.Contains(err.Error(), "worker secret data exceeds Kubernetes 1048576-byte limit") {
					t.Errorf("unexpected sanitized error: %v", err)
				}
				if len(secretVerbs(client)) != 0 {
					t.Errorf("oversized Secret reached client: %v", secretVerbs(client))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var created *corev1.Secret
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" && action.GetResource().Resource == "secrets" {
					created = action.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
				}
			}
			if created == nil {
				t.Fatal("valid Secret was not created")
			}
			if string(created.Data[tokenKey]) != *w.JoinToken {
				t.Error("join token changed")
			}
			if got, ok := created.Data[codexExtraCACertKey]; ok != (!tc.isolated && tc.extra > 0) || (ok && string(got) != string(m.cfg.ExtraCAPEM)) {
				t.Error("ordinary extra CA key or isolated exclusion changed")
			}
		})
	}
}
