package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
)

// PodEpochWatch owns a single serial LIST/WATCH subscription. Stop joins Run;
// cancellation closes the HTTP stream and bounds reconnect waits.
type PodEpochWatch struct {
	client             rest.Interface
	namespace, runtime string
	session            *PodEpochSession
	synced             chan struct{}
	done               chan struct{}
	cancel             context.CancelFunc
	once               sync.Once
	mu                 sync.Mutex
	baselineFence      uint64
}

func NewPodEpochWatch(client rest.Interface, namespace, runtime string, session *PodEpochSession) *PodEpochWatch {
	return &PodEpochWatch{client: client, namespace: namespace, runtime: runtime, session: session, synced: make(chan struct{}), done: make(chan struct{})}
}
func (w *PodEpochWatch) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	go w.Run(ctx)
}
func (w *PodEpochWatch) Stop() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
	}
}

// WaitForSync permits normal reconciliation after the caller's bounded startup
// timer expires. Until a full subscription exists, no events yield evidence.
func (w *PodEpochWatch) WaitForSync(ctx context.Context, timeout <-chan time.Time) bool {
	select {
	case <-w.synced:
		return true
	case <-timeout:
		w.mu.Lock()
		defer w.mu.Unlock()
		// Readiness and the timeout fence share a lock: an established watch
		// wins a racing timeout even when select chose the timer.
		select {
		case <-w.synced:
			return true
		default:
		}
		w.baselineFence++
		if w.session != nil {
			// Reset with no pods performs no HTTP and invalidates outstanding
			// generation-bound decisions before reconciliation may proceed.
			_, _ = w.session.Reset(ctx, nil)
		}
		return false
	case <-ctx.Done():
		return false
	}
}
func (w *PodEpochWatch) register(p *corev1.Pod) error {
	if id := p.Labels[LabelWorkerID]; id != "" {
		return w.session.RegisterWorker(PodEpochWorker{ID: id, Runtime: w.runtime})
	}
	return nil
}
func (w *PodEpochWatch) baseline(ctx context.Context) (string, error) {
	var pods []corev1.Pod
	token, rv := "", ""
	bytes := 0
	seen := map[string]bool{}
	for pages := 0; pages < PodEpochMaxIdentities; pages++ {
		reqCtx, cancel := context.WithTimeout(markMaintenanceRead(ctx), podEpochRequestTimeout)
		req := w.client.Get().Namespace(w.namespace).Resource("pods").SetHeader("Accept", "application/json").Param("limit", "100")
		if token != "" {
			req.Param("continue", token)
		}
		body, err := req.Stream(reqCtx)
		var raw []byte
		if err == nil {
			raw, err = io.ReadAll(io.LimitReader(body, (8<<20)+1))
			if closeErr := body.Close(); err == nil {
				err = closeErr
			}
		}
		cancel()
		if err != nil {
			return "", err
		}
		if len(raw) > 8<<20 {
			return "", errors.New("pod epoch LIST page exceeds byte cap")
		}
		bytes += len(raw)
		if bytes > PodEpochMaxMetadata {
			return "", errors.New("pod epoch LIST exceeds session budget")
		}
		var list corev1.PodList
		if err = json.Unmarshal(raw, &list); err != nil {
			return "", err
		}
		if len(list.Items) > 100 || list.ResourceVersion == "" || (rv != "" && rv != list.ResourceVersion) {
			return "", errors.New("invalid pod epoch LIST")
		}
		rv = list.ResourceVersion
		for i := range list.Items {
			if list.Items[i].Namespace != w.namespace {
				return "", errors.New("pod epoch namespace mismatch")
			}
			if err = w.register(&list.Items[i]); err != nil {
				return "", err
			}
		}
		pods = append(pods, list.Items...)
		if len(pods) > PodEpochMaxIdentities {
			return "", errors.New("pod epoch LIST identity overflow")
		}
		token = list.Continue
		if token == "" {
			_, err = w.session.Reset(ctx, pods)
			return rv, err
		}
		if seen[token] {
			return "", errors.New("pod epoch repeated continuation")
		}
		seen[token] = true
	}
	return "", errors.New("pod epoch LIST page overflow")
}

var errPodEpochReset = errors.New("pod epoch subscription reset")

// consume caps each NDJSON frame before any Kubernetes or JSON decoder sees it.
// Delivery is synchronous: rv advances only after Handle and its sole patch attempt.
func (w *PodEpochWatch) consume(ctx context.Context, body io.Reader, rv *string) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), (2<<20)+1)
	g := w.session.CurrentGeneration()
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) > 2<<20 {
			return errPodEpochReset
		}
		var frame struct {
			Type   watch.EventType `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if json.Unmarshal(raw, &frame) != nil {
			return errPodEpochReset
		}
		if frame.Type == watch.Error {
			var status metav1.Status
			if json.Unmarshal(frame.Object, &status) != nil || status.Code < 400 || status.Code == 410 {
				return errPodEpochReset
			}
			return errors.New("pod epoch watch status error")
		}
		if frame.Type != watch.Added && frame.Type != watch.Modified && frame.Type != watch.Deleted {
			return errPodEpochReset
		}
		var p corev1.Pod
		if json.Unmarshal(frame.Object, &p) != nil || p.Namespace != w.namespace || p.ResourceVersion == "" {
			return errPodEpochReset
		}
		if frame.Type == watch.Added {
			if w.register(&p) != nil {
				return errPodEpochReset
			}
		}
		d, err := w.session.Handle(ctx, g, frame.Type, &p)
		if err != nil {
			return errPodEpochReset
		}
		if d != nil {
			_ = w.session.Deliver(ctx, d)
		}
		*rv = p.ResourceVersion
	}
	if err := scanner.Err(); err != nil {
		// Scanner's size error is a framing/decode failure; transport failures resume.
		if errors.Is(err, bufio.ErrTooLong) {
			return errPodEpochReset
		}
		return err
	}
	return io.EOF
}

// Run retries transport errors indefinitely with one context-bounded delay per
// attempt. A reset discards proof before relisting; normal reconciliation is independent.
func (w *PodEpochWatch) Run(ctx context.Context) {
	defer close(w.done)
	rv := ""
	var fence uint64
	for ctx.Err() == nil {
		if rv == "" {
			w.mu.Lock()
			fence = w.baselineFence
			w.mu.Unlock()
			_, _ = w.session.Reset(ctx, nil)
			next, err := w.baseline(ctx)
			if err != nil {
				if !podEpochRetry(ctx) {
					return
				}
				continue
			}
			rv = next
		}
		// A timeout during LIST requires a new baseline before opening WATCH.
		w.mu.Lock()
		stale := fence != w.baselineFence
		w.mu.Unlock()
		if stale {
			rv = ""
			_, _ = w.session.Reset(ctx, nil)
			continue
		}
		body, err := w.client.Get().Namespace(w.namespace).Resource("pods").SetHeader("Accept", "application/json").Param("watch", "true").Param("resourceVersion", rv).Stream(ctx)
		if err == nil {
			w.mu.Lock()
			stale = fence != w.baselineFence
			if !stale {
				w.once.Do(func() { close(w.synced) })
			}
			w.mu.Unlock()
			if stale {
				// Discard the entire late stream, including buffered events.
				// The next attempt captures the new fence before its LIST.
				_ = body.Close() // This entire subscription is discarded, including close failures.
				rv = ""
				_, _ = w.session.Reset(ctx, nil)
				continue
			}
			err = w.consume(ctx, body, &rv)
			if closeErr := body.Close(); closeErr != nil {
				err = errPodEpochReset
			}
		}
		if errors.Is(err, errPodEpochReset) || apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
			rv = ""
			_, _ = w.session.Reset(ctx, nil)
		}
		if !podEpochRetry(ctx) {
			return
		}
	}
}
func podEpochRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
