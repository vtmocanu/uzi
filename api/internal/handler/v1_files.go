package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
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
//	403 insufficient_scope         RequireScope
//	413 file_too_large             the declared size is over the per-file cap, or the request body
//	                               is over the per-file cap plus multipart overhead (payload_too_large)
//	415 unsupported_file_type      the bytes are not an allowlisted type, or disagree with the type
//	                               the part's Content-Type or file name declares
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
)

// v1IsTextType reports whether t is one of the UTF-8 text family the input allowlist takes.
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

func v1Unsupported(msg string) error { return &v1UploadRefusal{msg: msg} }

// v1UploadInspector detects the type from the bytes (PDF, PNG, JPEG and ZIP by magic number; UTF-8
// text otherwise) and requires the type the uploader DECLARED (through the part's Content-Type and
// the file name's extension) to agree. Text is checked over the whole stream: valid UTF-8, no NUL,
// and, for JSON, json.Valid over the whole body (the body is buffered for that one type only,
// bounded by the per-file cap the reservation already enforced).
type v1UploadInspector struct {
	declared []string // the types the Content-Type and the extension declare; may be empty.
	detected string
	text     bool
	carry    []byte // an incomplete UTF-8 sequence at the end of the previous chunk.
	json     bytes.Buffer
}

// v1DeclaredTypes returns the types a part declares: its Content-Type (application/octet-stream and
// an absent header declare nothing) and its file name's extension. An unrecognised Content-Type or
// non-empty extension is returned as its own string so it disagrees with every detection.
func v1DeclaredTypes(partContentType, displayName string) []string {
	var out []string
	if ct := strings.TrimSpace(partContentType); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		mt = strings.ToLower(mt)
		switch {
		case err != nil:
			out = append(out, "invalid/"+ct)
		case mt == "application/octet-stream":
		default:
			out = append(out, mt)
		}
	}
	if ext := strings.ToLower(path.Ext(displayName)); ext != "" {
		if t, ok := v1UploadTypes[ext]; ok {
			out = append(out, t)
		} else {
			out = append(out, "unsupported/"+ext)
		}
	}
	return out
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
				return "", v1Unsupported("the file's content is " + in.detected + " but it was declared as " + d)
			}
		}
		return in.detected, nil
	}
	// Text: every declared type must be in the text family, and the two declarations must not
	// name different specific types (text/plain is the generic one and yields to the other).
	specific := ""
	for _, d := range in.declared {
		if !v1IsTextType(d) {
			return "", v1Unsupported("the file's content is text but it was declared as " + d)
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
	if in.detected == v1TypeJSON && !json.Valid(in.json.Bytes()) {
		return v1Unsupported("the file is declared as JSON but is not valid JSON")
	}
	return nil
}

// sanitizeUploadName reduces an uploader-supplied file name to a safe display name: the basename
// only (both / and \ separate components), no control, format, line or paragraph separator
// characters, no invalid UTF-8, no edge whitespace, at most v1FileDisplayNameMaxBytes bytes, and
// v1FileDefaultDisplayName when nothing usable is left. The result passes termsafe.Validate and
// the workersvc display-name rules.
func sanitizeUploadName(raw string) string {
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
	for len(name) > v1FileDisplayNameMaxBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || termsafe.Validate("display_name", name) != nil {
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
		reason := ref.Reason
		if reason == workersvc.RefusalFileTooLarge {
			reason = v1ReasonFileTooLarge
		}
		httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the file is over a size limit ("+ref.Reason+")", reason)
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
			httpx.ErrorReason(w, http.StatusUnsupportedMediaType, "the file is not a supported type, or its content does not match its declared type", v1ReasonUnsupported)
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

	declared, err := strconv.ParseInt(r.Header.Get("X-Uzi-File-Size"), 10, 64)
	if err != nil || declared < 0 {
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
			if errors.As(perr, &mbe) {
				httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the request body is too large", v1ReasonPayloadTooLarge)
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
		writeV1FileError(w, "reserve", err)
		return
	}
	stored, err := jf.Write(r.Context(), file.ID, caller.UserID, part, workersvc.WriteOptions{
		Inspector: &v1UploadInspector{declared: v1DeclaredTypes(part.Header.Get("Content-Type"), name)},
	})
	if err != nil {
		writeV1FileError(w, "write", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, v1FileDTO(stored))
}

// writeV1FileError maps a job-file store error. An unrecognised one is a logged 500 with no detail.
func writeV1FileError(w http.ResponseWriter, op string, err error) {
	var ref *workersvc.JobFileRefusedError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &ref):
		writeV1FileRefusal(w, ref)
	case errors.As(err, &mbe):
		httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "the request body is too large", v1ReasonPayloadTooLarge)
	case errors.Is(err, workersvc.ErrJobFilesUnavailable):
		httpx.ErrorReason(w, http.StatusServiceUnavailable, "file uploads are not available on this server", v1ReasonFilesDisabled)
	case errors.Is(err, workersvc.ErrJobFileInvalid):
		httpx.ErrorReason(w, http.StatusUnprocessableEntity, "the file's name or digest is invalid", v1ReasonInvalidRequest)
	default:
		slog.Error("v1 files: "+op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
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
