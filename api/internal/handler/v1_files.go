package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// v1_files.go serves POST /api/v1/files (PRD #1909 D5, D6): one multipart upload of one input file,
// admitted atomically at the DECLARED size, type-checked against magic bytes while it streams, and
// stored 'unattached' until a job create (input_file_ids) attaches it. Authentication, the per-user
// limiter and the jobs:run scope are the route mount's (routes_v1.go).
//
// Request contract:
//
//	Content-Type: multipart/form-data with a part named "file" (the first such part is used;
//	              parts before it are skipped and anything after it is ignored)
//	X-Uzi-File-Size    REQUIRED, the file's exact size in bytes: the reservation is exactly this
//	X-Uzi-File-Sha256  optional, 64 lowercase hex characters, verified against the streamed bytes
//
// Error contract (api/openapi/v1.yaml documents each):
//
//	401 (no reason)                RequireV1Caller
//	503 auth_unavailable           RequireV1Caller, when a token-store lookup fails (not an invalid token)
//	403 insufficient_scope         RequireScope
//	413 file_too_large             the declared size is over the per-file cap, or the request body
//	                               is over the per-file cap plus multipart overhead (payload_too_large)
//	408 request_timeout            the body was not fully received within the job-files request
//	                               deadline (a stalled client is cut off and its slot released)
//	415 unsupported_file_type      the bytes are not an allowlisted type, or disagree with the type
//	                               the part's Content-Type or file name declares; the message names
//	                               the specific refusal (fixed text, never client input)
//	400 invalid_request            the body ended or broke before the file was read (a disconnect)
//	503 uploads_busy               too many uploads are streaming (process-wide or this caller's
//	                               share): retry after the Retry-After seconds
//	422 invalid_request            no or malformed X-Uzi-File-Size / X-Uzi-File-Sha256, not multipart,
//	                               no "file" part
//	422 empty_file                 a zero-byte file
//	422 size_mismatch              the streamed size is not the declared size
//	422 sha256_mismatch            the streamed digest is not the declared digest
//	507 storage_quota_exceeded     the owner's, the instance's or the shared stored-file budget is
//	                               full (the message says which)
//	503 files_unavailable          the deployment has no job-file store
const (
	v1ReasonFileTooLarge  = "file_too_large"
	v1ReasonUnsupported   = "unsupported_file_type"
	v1ReasonEmptyFile     = "empty_file"
	v1ReasonSizeMismatch  = "size_mismatch"
	v1ReasonSHAMismatch   = "sha256_mismatch"
	v1ReasonStorageQuota  = "storage_quota_exceeded"
	v1ReasonFilesDisabled = "files_unavailable"
	v1ReasonFileGone      = "file_unavailable"
	v1ReasonUploadsBusy   = "uploads_busy"
	v1ReasonReqTimeout    = "request_timeout"

	// v1FileBusyRetryAfter is the Retry-After (seconds) an uploads_busy refusal carries.
	v1FileBusyRetryAfter = 5
	// v1FileWriteGrace is how long past the read deadline the response may still be written: the
	// server's own WriteTimeout runs from the request start, so a long upload would otherwise
	// finish with no way to answer.
	v1FileWriteGrace = 15 * time.Second

	// v1FileMultipartOverhead is what the request body may carry beyond the file's own bytes: the
	// boundary lines, the part headers and any small preceding parts.
	v1FileMultipartOverhead = 64 << 10
	// v1FileDisplayNameMaxBytes bounds a sanitised display name (the column allows 255).
	v1FileDisplayNameMaxBytes = 200
	v1FileDefaultDisplayName  = "upload"
)

// v1UploadTypes maps a declared file-name extension to the content type it declares.
var v1UploadTypes = map[string]string{
	".pdf":      "application/pdf",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".txt":      "text/plain",
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".csv":      "text/csv",
	".json":     "application/json",
	".docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

const (
	v1TypePDF  = "application/pdf"
	v1TypePNG  = "image/png"
	v1TypeJPEG = "image/jpeg"
	v1TypeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	v1TypeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	v1TypeText = "text/plain"
	v1TypeJSON = "application/json"
	v1TypeHTML = "text/html"
)

// v1OutputTypes are the extra types a worker OUTPUT may be, on top of v1UploadTypes (PRD #1909 D6:
// the output allowlist is the input allowlist plus HTML). HTML is stored and served as data
// (attachment, nosniff, octet-stream); it is never rendered by the API.
var v1OutputTypes = map[string]string{
	".html": v1TypeHTML,
	".htm":  v1TypeHTML,
}

// v1IsTextType reports whether t is one of the UTF-8 text family the INPUT allowlist takes (HTML is
// not: only an inspector built with allowHTML takes it, see v1UploadInspector.textType).
func v1IsTextType(t string) bool {
	switch t {
	case v1TypeText, "text/markdown", "text/csv", v1TypeJSON:
		return true
	}
	return false
}

// v1UploadRefusal is an Inspector error: it names the refusal reason Write records and reports.
type v1UploadRefusal struct{ msg string }

func (e *v1UploadRefusal) Error() string         { return e.msg }
func (e *v1UploadRefusal) RefusalReason() string { return v1ReasonUnsupported }

// RefusalDetail is the message the 415 carries. Every v1Unsupported call site passes a fixed
// sentence; none is built from the uploaded name, type or bytes.
func (e *v1UploadRefusal) RefusalDetail() string { return e.msg }

func v1Unsupported(msg string) error { return &v1UploadRefusal{msg: msg} }

// v1UploadInspector detects the type from the bytes (PDF, PNG, JPEG and ZIP by magic number; UTF-8
// text otherwise) and requires the type the uploader DECLARED (through the part's Content-Type and
// the file name's extension) to agree. Text is checked over the whole stream: valid UTF-8, no NUL,
// and, for JSON, json.Valid over the whole body (the body is buffered for that one type only,
// bounded by the per-file cap the reservation already enforced).
type v1UploadInspector struct {
	declared []string // the types the Content-Type and the extension declare; may be empty.
	// allowHTML widens the text family with text/html: the worker OUTPUT allowlist. The input
	// route leaves it false, so an uploaded HTML file is refused exactly as before.
	allowHTML bool
	detected  string
	text      bool
	carry     []byte // an incomplete UTF-8 sequence at the end of the previous chunk.
	json      bytes.Buffer
}

// v1DeclaredTypes returns the types a part declares: its Content-Type (application/octet-stream and
// an absent header declare nothing) and its file name's extension. An unrecognised Content-Type or
// non-empty extension is returned as its own string so it disagrees with every detection.
//
// Common aliases are folded first: text/x-markdown is markdown, image/jpg is JPEG, and
// application/vnd.ms-excel is CSV when the file is named .csv (spreadsheet tools label a CSV that
// way; the text check still runs over the bytes, so a real workbook is refused).
func v1DeclaredTypes(partContentType, displayName string) []string {
	return declaredTypes(partContentType, displayName, false)
}

// v1OutputDeclaredTypes is v1DeclaredTypes for a worker output: no part Content-Type, and the file
// name's extension may also declare HTML.
func v1OutputDeclaredTypes(displayName string) []string {
	return declaredTypes("", displayName, true)
}

func declaredTypes(partContentType, displayName string, allowHTML bool) []string {
	var out []string
	ext := strings.ToLower(path.Ext(displayName))
	if ct := strings.TrimSpace(partContentType); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		mt = strings.ToLower(mt)
		switch {
		case err != nil:
			out = append(out, "invalid/"+ct)
		case mt == "application/octet-stream":
		case mt == "text/x-markdown":
			out = append(out, "text/markdown")
		case mt == "image/jpg":
			out = append(out, v1TypeJPEG)
		case mt == "application/vnd.ms-excel" && ext == ".csv":
			out = append(out, "text/csv")
		default:
			out = append(out, mt)
		}
	}
	if ext != "" {
		if t, ok := v1UploadTypes[ext]; ok {
			out = append(out, t)
		} else if t, ok := v1OutputTypes[ext]; ok && allowHTML {
			out = append(out, t)
		} else {
			out = append(out, "unsupported/"+ext)
		}
	}
	return out
}

// v1DeclaredLabel names a declared type in a refusal message. Only an allowlisted type is echoed;
// anything else (an unknown extension or content type, both client input) reads as a fixed phrase
// followed by the allowed set.
// allowHTML is the worker OUTPUT allowlist: text/html is then an allowlisted type too, and the
// allowed extensions include .htm and .html.
func v1DeclaredLabel(d string, allowHTML bool) string {
	for _, t := range v1UploadTypes {
		if t == d {
			return d
		}
	}
	if allowHTML && d == v1TypeHTML {
		return d
	}
	return "an unsupported type; the allowed extensions are " + v1AllowedExtensions(allowHTML)
}

// v1AllowedExtensions lists the accepted file-name extensions, sorted, for a refusal message; the
// worker output allowlist (allowHTML) also names the HTML extensions.
func v1AllowedExtensions(allowHTML bool) string {
	exts := make([]string, 0, len(v1UploadTypes)+len(v1OutputTypes))
	for e := range v1UploadTypes {
		exts = append(exts, e)
	}
	if allowHTML {
		for e := range v1OutputTypes {
			exts = append(exts, e)
		}
	}
	sort.Strings(exts)
	return strings.Join(exts, ", ")
}

// textType reports whether d is in the UTF-8 text family this inspector takes.
func (in *v1UploadInspector) textType(d string) bool {
	return v1IsTextType(d) || (in.allowHTML && d == v1TypeHTML)
}

func (in *v1UploadInspector) Begin(head []byte) (string, error) {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		in.detected = v1TypePDF
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		in.detected = v1TypePNG
	case bytes.HasPrefix(head, []byte{0xff, 0xd8, 0xff}):
		in.detected = v1TypeJPEG
	case bytes.HasPrefix(head, []byte{'P', 'K', 3, 4}):
		// A ZIP container: only the two Office types, and only when the upload says which.
		for _, d := range in.declared {
			if d == v1TypeDOCX || d == v1TypeXLSX {
				in.detected = d
			}
		}
		if in.detected == "" {
			return "", v1Unsupported("a ZIP container is accepted only as a .docx or .xlsx with its declared type")
		}
	default:
		in.text = true
	}
	if !in.text {
		for _, d := range in.declared {
			if d != in.detected {
				return "", v1Unsupported("the file's content is " + in.detected + " but it was declared as " + v1DeclaredLabel(d, in.allowHTML))
			}
		}
		return in.detected, nil
	}
	// Text: every declared type must be in the text family, and the two declarations must not
	// name different specific types (text/plain is the generic one and yields to the other).
	specific := ""
	for _, d := range in.declared {
		if !in.textType(d) {
			return "", v1Unsupported("the file's content is text but it was declared as " + v1DeclaredLabel(d, in.allowHTML))
		}
		if d == v1TypeText {
			continue
		}
		if specific != "" && specific != d {
			return "", v1Unsupported("the declared type and the file extension disagree")
		}
		specific = d
	}
	if specific == "" {
		specific = v1TypeText
	}
	in.detected = specific
	return specific, nil
}

func (in *v1UploadInspector) Chunk(p []byte) error {
	if !in.text {
		return nil
	}
	if bytes.IndexByte(p, 0) >= 0 {
		return v1Unsupported("text files must not contain NUL bytes")
	}
	buf := p
	if len(in.carry) > 0 {
		buf = append(in.carry, p...)
	}
	// Hold back an incomplete final rune for the next chunk; anything else invalid is refused.
	end := len(buf)
	for i := 1; i <= 3 && i <= len(buf); i++ {
		if b := buf[len(buf)-i]; utf8.RuneStart(b) {
			if b >= 0xc0 && !utf8.FullRune(buf[len(buf)-i:]) {
				end = len(buf) - i
			}
			break
		}
	}
	if !utf8.Valid(buf[:end]) {
		return v1Unsupported("text files must be valid UTF-8")
	}
	in.carry = append(in.carry[:0], buf[end:]...)
	if in.detected == v1TypeJSON {
		in.json.Write(buf[:end])
	}
	return nil
}

func (in *v1UploadInspector) End() error {
	if !in.text {
		return nil
	}
	if len(in.carry) > 0 {
		return v1Unsupported("text files must be valid UTF-8")
	}
	// A leading UTF-8 byte order mark is not JSON; spreadsheet and Windows tools write one.
	if in.detected == v1TypeJSON && !json.Valid(bytes.TrimPrefix(in.json.Bytes(), []byte("\xef\xbb\xbf"))) {
		return v1Unsupported("the file is declared as JSON but is not valid JSON")
	}
	return nil
}

// v1FileNameExtMaxBytes bounds the extension kept when a long name is shortened.
const v1FileNameExtMaxBytes = 16

// cleanUploadName reduces an uploader-supplied file name to its safe basename, NOT yet bounded in
// length: the basename only (both / and \ separate components), no control, format, line or
// paragraph separator characters, no invalid UTF-8, no edge whitespace, and
// v1FileDefaultDisplayName when nothing usable is left. The declared type derives from this
// untruncated name, so a long name keeps its extension's meaning.
func cleanUploadName(raw string) string {
	name := raw
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		if termsafe.Unsafe(r) || unicode.In(r, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return v1FileDefaultDisplayName
	}
	return name
}

// boundUploadName shortens a clean name to v1FileDisplayNameMaxBytes by cutting the STEM on a rune
// boundary and keeping the extension (up to v1FileNameExtMaxBytes bytes; a longer "extension" is
// not one and is cut like the rest of the name), so a long name is still recognisable as what it
// is.
func boundUploadName(name string) string {
	if len(name) <= v1FileDisplayNameMaxBytes {
		return name
	}
	ext := path.Ext(name)
	if len(ext) > v1FileNameExtMaxBytes {
		ext = ""
	}
	stem := name[:len(name)-len(ext)]
	for len(stem)+len(ext) > v1FileDisplayNameMaxBytes {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	stem = strings.TrimSpace(stem)
	if stem == "" && ext == "" {
		return v1FileDefaultDisplayName
	}
	return stem + ext
}

// sanitizeUploadName is the stored display name of an uploader-supplied file name: cleanUploadName
// then boundUploadName, defaulted when the result fails the workersvc display-name rules
// (termsafe.Validate).
func sanitizeUploadName(raw string) string {
	name := boundUploadName(cleanUploadName(raw))
	if termsafe.Validate("display_name", name) != nil {
		return v1FileDefaultDisplayName
	}
	return name
}

func v1FileDTO(f store.JobFile) apitypes.V1FileDTO {
	dto := apitypes.V1FileDTO{
		ID:          f.ID.String(),
		DisplayName: f.DisplayName,
		StorageName: f.StorageName.String,
		ContentType: f.ContentType.String,
		ByteSize:    f.ByteSize,
		Sha256:      f.Sha256.String,
		State:       f.State,
	}
	if f.ExpiresAt.Valid {
		t := f.ExpiresAt.Time
		dto.ExpiresAt = &t
	}
	return dto
}

// writeV1FileRefusal maps a refused upload or attach to its status and reason.
func writeV1FileRefusal(w http.ResponseWriter, ref *workersvc.JobFileRefusedError) {
	switch ref.Kind {
	case workersvc.RefusalLimit:
		reason, msg := ref.Reason, "the file is over a size limit"
		switch ref.Reason {
		case workersvc.RefusalFileTooLarge:
			reason, msg = v1ReasonFileTooLarge, "the file is over the per-file size limit"
		case workersvc.RefusalTooManyFiles:
			msg = "the job has more input files than the per-job limit allows"
		case workersvc.RefusalJobBytes:
			msg = "the job's input files are over the per-job total size limit"
		}
		httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, msg, reason)
	case workersvc.RefusalQuota:
		msg := "your stored files are at their limit; wait for them to expire and retry"
		if ref.Reason != workersvc.RefusalOwnerQuota {
			msg = "the server's file storage is full; retry later"
		}
		httpx.ErrorReason(w, http.StatusInsufficientStorage, msg, v1ReasonStorageQuota)
	default:
		switch ref.Reason {
		case workersvc.RefusalEmptyFile:
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the file is empty", v1ReasonEmptyFile)
		case workersvc.RefusalSizeMismatch:
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the uploaded bytes do not match X-Uzi-File-Size", v1ReasonSizeMismatch)
		case workersvc.RefusalSHAMismatch:
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the uploaded bytes do not match X-Uzi-File-Sha256", v1ReasonSHAMismatch)
		case v1ReasonUnsupported, workersvc.RefusalUnsupported, workersvc.RefusalContentInvalid:
			msg := "the file is not a supported type, or its content does not match its declared type"
			if ref.Detail != "" {
				msg += ": " + ref.Detail
			}
			httpx.ErrorReason(w, http.StatusUnsupportedMediaType, msg, v1ReasonUnsupported)
		default:
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the file was refused", v1ReasonInvalidRequest)
		}
	}
}

// V1FileUpload serves POST /api/v1/files: 201 with the stored file, state 'unattached'.
func (h *Handler) V1FileUpload(w http.ResponseWriter, r *http.Request) {
	caller, ok := v1JobCaller(w, r)
	if !ok {
		return
	}
	jf := h.wsvc.JobFiles()
	if jf == nil {
		httpx.ErrorReason(w, http.StatusServiceUnavailable, "file uploads are not available on this server", v1ReasonFilesDisabled)
		return
	}

	declared, err := parseV1FileSize(r.Header.Get("X-Uzi-File-Size"))
	if err != nil {
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the X-Uzi-File-Size header is required and must be the file's size in bytes", v1ReasonInvalidRequest)
		return
	}
	declaredSHA := r.Header.Get("X-Uzi-File-Sha256")
	if declaredSHA != "" && !v1SHA256Hex(declaredSHA) {
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "X-Uzi-File-Sha256 must be 64 lowercase hexadecimal characters", v1ReasonInvalidRequest)
		return
	}

	limit := jf.Limits().InputFileMaxBytes + v1FileMultipartOverhead
	if r.ContentLength > limit {
		httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the request body is too large", v1ReasonPayloadTooLarge)
		return
	}
	// Bound the wall time of the body read. A body read does not observe the request context, so a
	// client that stalls would otherwise hold its goroutine (and, once streaming, its database
	// connection) until the server's ReadTimeout, and that timeout (15 s in cmd/server) is also
	// shorter than a maximum-size upload needs. The read deadline is now + UploadDeadline: at least
	// the job-files RequestDeadline (120 s by default), longer for a large DECLARED size
	// (declared / 100 KiB/s, so a maximum-size upload at the slowest assumed rate is not cut off
	// mid-body: 256 s at 25 MiB), and never more than max(RequestDeadline, MaxUploadDeadline). It
	// replaces the server-wide deadline for this route only, and the write deadline follows it by
	// v1FileWriteGrace so the response can still be written. A writer that does not support
	// deadlines (a test recorder) keeps the server's own.
	uploadDeadline := jf.Limits().UploadDeadline(declared, jf.Limits().InputFileMaxBytes)
	deadline := setJobFileDeadlines(w, uploadDeadline)

	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the body must be multipart/form-data with a part named file", v1ReasonInvalidRequest)
		return
	}
	var part *multipart.Part
	for {
		p, perr := mr.NextPart()
		if perr != nil {
			var mbe *http.MaxBytesError
			switch {
			case errors.As(perr, &mbe):
				httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the request body is too large", v1ReasonPayloadTooLarge)
				return
			case v1IsTimeout(perr):
				slog.Warn("v1 files: reading the upload body timed out")
				httpx.ErrorReason(w, http.StatusRequestTimeout, "the upload was not received in time", v1ReasonReqTimeout)
				return
			}
			httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the body must be multipart/form-data with a part named file", v1ReasonInvalidRequest)
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		}
		_, _ = io.Copy(io.Discard, p)
	}

	// One upload slot per streaming write, taken BEFORE Reserve so a refused request holds neither
	// a reservation nor a connection (see workersvc.JobFiles.AcquireWrite).
	release, err := jf.AcquireWrite(caller.UserID)
	if err != nil {
		w.Header().Set("Retry-After", strconv.Itoa(v1FileBusyRetryAfter))
		httpx.ErrorReason(w, http.StatusServiceUnavailable, "too many uploads are in progress; retry shortly", v1ReasonUploadsBusy)
		return
	}
	defer release()

	// The declared type derives from the FULL cleaned name; only the stored display name is
	// shortened, and it keeps its extension.
	clean := cleanUploadName(part.FileName())
	name := sanitizeUploadName(part.FileName())
	file, err := jf.Reserve(r.Context(), workersvc.ReserveParams{
		UserID:         caller.UserID,
		ProductID:      caller.ProductID,
		Direction:      workersvc.JobFileInput,
		DisplayName:    name,
		DeclaredSize:   declared,
		DeclaredSHA256: declaredSHA,
	})
	if err != nil {
		writeV1FileError(w, "reserve", err, deadline)
		return
	}
	stored, err := jf.Write(r.Context(), file.ID, caller.UserID, part, workersvc.WriteOptions{
		Inspector: &v1UploadInspector{declared: v1DeclaredTypes(part.Header.Get("Content-Type"), clean)},
		Deadline:  uploadDeadline,
	})
	if err != nil {
		writeV1FileError(w, "write", err, deadline)
		return
	}
	httpx.JSON(w, http.StatusCreated, v1FileDTO(stored))
}

// setJobFileDeadlines applies the job-files request deadline to this route's connection (see the
// comment at V1FileUpload's call): the read deadline is now + d, the write deadline follows it by
// v1FileWriteGrace, and the read deadline is returned. Shared by the v1 input upload and the
// worker output upload.
func setJobFileDeadlines(w http.ResponseWriter, d time.Duration) time.Time {
	deadline := time.Now().Add(d)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline.Add(v1FileWriteGrace))
	return deadline
}

// writeV1FileError maps a job-file store error. deadline is the request's read deadline: when the
// connection's read deadline expires the server also cancels the request context, so a database
// step then fails with context.Canceled, which reads as a timeout once the deadline has passed and
// as a dropped client before it. An unrecognised error is a logged 500 with no detail.
func writeV1FileError(w http.ResponseWriter, op string, err error, deadline time.Time) {
	var ref *workersvc.JobFileRefusedError
	var mbe *http.MaxBytesError
	var body *workersvc.JobFileBodyError
	switch {
	case errors.As(err, &ref):
		writeV1FileRefusal(w, ref)
	case errors.As(err, &mbe):
		httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the request body is too large", v1ReasonPayloadTooLarge)
	case v1IsTimeout(err):
		// The request deadline ran out mid-write (the body read, or a database step under the
		// deadline'd context): the upload was too slow, not a server fault.
		slog.Warn("v1 files: the upload ran out of time", "op", op)
		httpx.ErrorReason(w, http.StatusRequestTimeout, "the upload was not received in time", v1ReasonReqTimeout)
	case errors.Is(err, context.Canceled):
		if !time.Now().Before(deadline) {
			slog.Warn("v1 files: the upload ran out of time", "op", op)
			httpx.ErrorReason(w, http.StatusRequestTimeout, "the upload was not received in time", v1ReasonReqTimeout)
			return
		}
		slog.Warn("v1 files: the client went away mid-upload", "op", op)
		httpx.ErrorReason(w, http.StatusBadRequest, "the upload was cancelled", v1ReasonInvalidRequest)
	case errors.As(err, &body):
		// The client's own doing (a dropped upload): a warning, not a server error.
		slog.Warn("v1 files: reading the upload body failed", "op", op)
		httpx.ErrorReason(w, http.StatusBadRequest, "the upload body could not be read", v1ReasonInvalidRequest)
	case errors.Is(err, workersvc.ErrJobFilesUnavailable):
		httpx.ErrorReason(w, http.StatusServiceUnavailable, "file uploads are not available on this server", v1ReasonFilesDisabled)
	case errors.Is(err, workersvc.ErrJobFileInvalid):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the file's name or digest is invalid", v1ReasonInvalidRequest)
	default:
		slog.Error("v1 files: "+op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// parseV1FileSize reads X-Uzi-File-Size: plain decimal digits only (no sign, space or separator).
func parseV1FileSize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("not a plain decimal")
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// v1IsTimeout reports a deadline failure: the connection's read deadline or a context deadline.
func v1IsTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func v1SHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
