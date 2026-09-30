package fetcher

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The api routes HTTPControl calls (see the package doc for the contract).
const (
	BeginPath    = "/api/fetcher/v1/begin"
	CompletePath = "/api/fetcher/v1/complete"
	// maxControlResponse caps an api answer on the control routes.
	maxControlResponse = 64 << 10
	// DefaultControlTimeout bounds one control call.
	DefaultControlTimeout = 10 * time.Second
)

// admissionCode is the shape an api admission code must have to be relayed to a worker.
var admissionCode = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// HTTPControl is Control over the api's fetcher routes, authenticated with the fetcher's
// own service token.
type HTTPControl struct {
	base   string
	token  string
	client *http.Client
}

// NewHTTPControl returns a Control for the api at baseURL (scheme://host[:port]).
// rootCAs, when non-nil, is the ONLY trust anchor for the api's certificate (the api is
// one operator-set destination with one known issuer); nil means the system roots. The
// transport ignores proxy environment: the service token and every run credential cross
// this hop.
func NewHTTPControl(baseURL, token string, rootCAs *x509.CertPool, timeout time.Duration) (*HTTPControl, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("api URL %q must be an absolute https URL", baseURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("api URL %q must be scheme://host[:port] only", baseURL)
	}
	if token == "" {
		return nil, errors.New("the fetcher service token is empty")
	}
	if timeout <= 0 {
		timeout = DefaultControlTimeout
	}
	tr := &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: timeout,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	return &HTTPControl{
		base:   u.Scheme + "://" + u.Host,
		token:  token,
		client: &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

// Begin implements Control.
func (c *HTTPControl) Begin(ctx context.Context, credential, rawURL string) (Admission, error) {
	status, body, err := c.post(ctx, BeginPath, apitypes.FetcherBeginRequest{Credential: credential, URL: rawURL})
	if err != nil {
		return Admission{}, err
	}
	switch status {
	case http.StatusOK:
		var out apitypes.FetcherBeginResponse
		if err := decodeStrict(bytes.NewReader(body), &out); err != nil {
			return Admission{}, fmt.Errorf("begin: malformed api answer: %w", err)
		}
		if out.ReservationID == "" || out.MaxBytes <= 0 {
			return Admission{}, errors.New("begin: api answer lacks a reservation id or a positive max_bytes")
		}
		return Admission{ReservationID: out.ReservationID, Entries: out.Entries, MaxBytes: out.MaxBytes}, nil
	case http.StatusForbidden:
		if controlReason(body) == ReasonCredentialInvalid {
			return Admission{}, ErrCredentialInvalid
		}
		return Admission{}, fmt.Errorf("begin: api answered %d", status)
	case http.StatusTooManyRequests:
		reason := controlReason(body)
		if !admissionCode.MatchString(reason) {
			reason = "unspecified"
		}
		return Admission{}, &AdmissionRefusedError{Reason: reason}
	case http.StatusUnauthorized:
		return Admission{}, errors.New("begin: the api refused the fetcher's service credential")
	default:
		return Admission{}, fmt.Errorf("begin: api answered %d", status)
	}
}

// Complete implements Control. Anything but a 2xx is a failure.
func (c *HTTPControl) Complete(ctx context.Context, credential string, rec AttemptRecord) error {
	status, _, err := c.post(ctx, CompletePath, apitypes.FetcherCompleteRequest{
		Credential:    credential,
		ReservationID: rec.ReservationID,
		URL:           rec.URL,
		FinalURL:      rec.FinalURL,
		Verdict:       rec.Verdict,
		Reason:        rec.Reason,
		HTTPStatus:    rec.HTTPStatus,
		ContentType:   rec.ContentType,
		Bytes:         rec.Bytes,
		SHA256:        rec.SHA256,
		StartedAt:     rec.StartedAt,
		FinishedAt:    rec.FinishedAt,
	})
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("complete: api answered %d", status)
	}
	return nil
}

func (c *HTTPControl) post(ctx context.Context, path string, in any) (int, []byte, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b)) //nolint:gosec // G704: c.base is the operator-set UZI_API_URL (validated https scheme://host in NewHTTPControl) and path is one of two constants; no request input reaches the URL.
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req) //nolint:gosec // G704: the request URL is the operator-set api base plus a constant path (see above).
	if err != nil {
		// The error names the api URL only; the body (which carries the credential) is
		// never part of it.
		return 0, nil, fmt.Errorf("%s: %w", strings.TrimPrefix(path, "/api/fetcher/v1/"), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxControlResponse+1))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > maxControlResponse {
		return 0, nil, errors.New("api answer too large")
	}
	return resp.StatusCode, body, nil
}

// controlReason reads the reason code from an api refusal body, or "".
func controlReason(body []byte) string {
	var e apitypes.FetcherControlErrorDTO
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Reason
}
