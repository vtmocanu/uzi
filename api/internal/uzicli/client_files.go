package uzicli

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_files.go holds the job-file verbs (PRD #1909 M7) over the stable /api/v1 API: the
// listing of one job's files, the multipart upload of an input file and the streaming download
// of one file. Upload and download move whole files, so they use a copy of the HTTP client with
// no overall Timeout (the 30 s ceiling fits a JSON call, not a large transfer); cancellation of
// the request context still ends them, and the CheckRedirect policy (no redirect, so the bearer
// token never follows one) is kept.

// FileDownload is an open GET /api/v1/files/{id} response. The caller owns Body and must close
// it. StorageName is the Content-Disposition filename, the content-derived `<sha256>.<ext>`
// (empty when the server sent none). Size is the declared Content-Length, -1 when unknown.
type FileDownload struct {
	Body        io.ReadCloser
	StorageName string
	Size        int64
}

// JobFiles lists one job's input and output files (GET /api/v1/jobs/{id}/files).
func (c *HTTPClient) JobFiles(ctx context.Context, id string) (apitypes.V1JobFilesDTO, error) {
	var out apitypes.V1JobFilesDTO
	if err := c.get(ctx, "/api/v1/jobs/"+url.PathEscape(id)+"/files", &out); err != nil {
		return apitypes.V1JobFilesDTO{}, err
	}
	return out, nil
}

// transferClient is the HTTP client for a file transfer: the configured one without its
// whole-request Timeout.
func (c *HTTPClient) transferClient() *http.Client {
	hc := *c.HTTP
	hc.Timeout = 0
	return &hc
}

// UploadFile stores one file for use as a job input (POST /api/v1/files). The body is a
// multipart form with one `file` part named name, streamed from r. size and sha256 (64 lowercase
// hex characters, or "" to skip the check) are sent as X-Uzi-File-Size and X-Uzi-File-Sha256; the
// server refuses bytes that differ from them.
func (c *HTTPClient) UploadFile(ctx context.Context, name string, size int64, sha256 string, r io.Reader) (apitypes.V1FileDTO, error) {
	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", name)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/files", pr)
	if err != nil {
		return apitypes.V1FileDTO{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Uzi-File-Size", strconv.FormatInt(size, 10))
	if sha256 != "" {
		req.Header.Set("X-Uzi-File-Sha256", sha256)
	}
	resp, err := c.transferClient().Do(req)
	if err != nil {
		return apitypes.V1FileDTO{}, transportExit(c.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return apitypes.V1FileDTO{}, Exitf(ExitUnreachable, "reading response from uzi: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		return apitypes.V1FileDTO{}, fileStatusError(resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	var out apitypes.V1FileDTO
	if err := json.Unmarshal(body, &out); err != nil {
		return apitypes.V1FileDTO{}, Exitf(ExitGeneric, "malformed response from uzi (/api/v1/files): %v", err)
	}
	return out, nil
}

// DownloadFile opens one file's bytes (GET /api/v1/files/{id}). An expired file (410) is an
// ExitNotFound error saying so; other failures map like every other verb.
func (c *HTTPClient) DownloadFile(ctx context.Context, id string) (*FileDownload, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/files/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.transferClient().Do(req)
	if err != nil {
		return nil, transportExit(c.BaseURL, err)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
		return nil, fileStatusError(resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	name := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	return &FileDownload{Body: resp.Body, StorageName: name, Size: resp.ContentLength}, nil
}

// fileStatusError maps the file endpoints' statuses. The shared mapping mislabels three of them:
// 410 (expired) would be a generic failure, 415 (unsupported type) a generic failure and 507
// (storage quota) a server outage (exit 6), so they get their own wording; everything else is
// the shared mapping.
func fileStatusError(status int, body []byte, retryAfter string) *ExitError {
	msg := serverErrMsg(body)
	switch status {
	case http.StatusGone:
		return Exitf(ExitNotFound, "file expired: its bytes are gone and cannot be downloaded")
	case http.StatusUnsupportedMediaType:
		if msg == "" {
			msg = "the file type is not supported"
		}
		return Exitf(ExitUsage, "unsupported file type: %s", msg)
	case http.StatusInsufficientStorage:
		if msg == "" {
			msg = "storage quota exceeded"
		}
		return Exitf(ExitGeneric, "storage quota exceeded: %s", strings.TrimSpace(msg))
	}
	return statusError(status, body, retryAfter)
}

// AdminProductSkills reads one product's skill set (GET /api/admin/products/{id}/skills): the
// source config, the applied set and the staged snapshot with its diff. Read-only; an admin
// read token works.
func (c *HTTPClient) AdminProductSkills(ctx context.Context, productID string) (apitypes.ProductSkillsDTO, error) {
	var out apitypes.ProductSkillsDTO
	if err := c.get(ctx, "/api/admin/products/"+url.PathEscape(productID)+"/skills", &out); err != nil {
		return apitypes.ProductSkillsDTO{}, err
	}
	return out, nil
}
