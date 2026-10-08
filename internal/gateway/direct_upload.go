package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"family-photo-cloud/internal/account"
	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/upload"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

const directUploadPath = "/v1/direct-uploads"

// UploadKeyVerifier resolves a Shortcut upload key to its owner.
type UploadKeyVerifier interface {
	UploadKeyOwner(ctx context.Context, keyHash [32]byte, now time.Time) (string, string, error)
}

// directUploader accepts a whole file in one request, for iOS Shortcuts and
// the web app. It drives the same durable state machine as the TUS path:
// session creation and admission control, tusd's filestore and per-upload
// lock, then MarkReceived so the verifier recomputes SHA-256 before the file
// becomes visible. The client must send the SHA-256 it computed.
type directUploader struct {
	repository          upload.Repository
	processor           *upload.Processor
	store               filestore.FileStore
	locker              filelocker.FileLocker
	limiter             *patchLimiter
	tokens              *auth.AccessTokenManager
	accounts            account.Repository
	keys                UploadKeyVerifier
	maxBytes            int64
	availableBytes      func() (int64, error)
	minimumFreeBytes    int64
	maxActiveSessions   int
	createWindow        time.Duration
	maxCreatesPerWindow int
	wake                func()
	logger              *slog.Logger
	readIdleTimeout     time.Duration
}

type directUploadResponse struct {
	ID        string       `json:"id,omitempty"`
	State     upload.State `json:"state"`
	Duplicate bool         `json:"duplicate,omitempty"`
}

// mediaTypesByExtension covers what the iPhone camera and common editors
// produce; Go's mime table does not know HEIC/HEIF or DNG.
var mediaTypesByExtension = map[string]string{
	".heic": "image/heic", ".heif": "image/heif", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".png": "image/png", ".gif": "image/gif", ".webp": "image/webp", ".tif": "image/tiff",
	".tiff": "image/tiff", ".dng": "image/x-adobe-dng", ".avif": "image/avif",
	".mov": "video/quicktime", ".mp4": "video/mp4", ".m4v": "video/x-m4v", ".3gp": "video/3gpp",
}

func (d *directUploader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeDirectProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	ownerID, ok := d.authenticate(r)
	if !ok {
		writeAuthError(w)
		return
	}
	size := r.ContentLength
	if size < 0 {
		writeDirectProblem(w, http.StatusLengthRequired, "length_required", "Content-Length is required")
		return
	}
	if size == 0 || size > d.maxBytes {
		writeDirectProblem(w, http.StatusRequestEntityTooLarge, "invalid_size", "file size is outside the configured upload limit")
		return
	}
	hashHex := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Content-SHA256")))
	hashBytes, err := hex.DecodeString(hashHex)
	if err != nil || len(hashBytes) != sha256.Size {
		writeDirectProblem(w, http.StatusBadRequest, "invalid_sha256", "X-Content-SHA256 must be the file's hex SHA-256")
		return
	}
	var clientHash [32]byte
	copy(clientHash[:], hashBytes)
	filename, ok := directFilename(r.Header.Get("X-File-Name"))
	if !ok {
		writeDirectProblem(w, http.StatusBadRequest, "invalid_filename", "X-File-Name must be a file name of at most 255 bytes")
		return
	}
	// Shortcuts may send a bare "IMG_1234" and a generic Content-Type, so the
	// first bytes decide when the headers and extension do not.
	limited := http.MaxBytesReader(w, r.Body, size)
	head := make([]byte, min(int64(64), size))
	if _, err := io.ReadFull(limited, head); err != nil {
		writeDirectProblem(w, http.StatusBadRequest, "incomplete_body", "request body ended early")
		return
	}
	mediaType, ok := directMediaType(r.Header.Get("X-Media-Type"), r.Header.Get("Content-Type"), filename)
	if !ok {
		if sniffed, extension, found := sniffMediaType(head); found {
			mediaType = sniffed
			if filepath.Ext(filename) == "" && len(filename)+len(extension) <= 255 {
				filename += extension
			}
		} else {
			writeDirectProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "only photos and videos can be uploaded")
			return
		}
	}
	if d.availableBytes == nil {
		writeDirectProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "storage admission control is unavailable")
		return
	}
	available, err := d.availableBytes()
	if err != nil {
		writeDirectProblem(w, http.StatusServiceUnavailable, "storage_unavailable", "storage capacity cannot be checked")
		return
	}

	// Idempotent per content and name: re-sharing the same photo maps to the
	// same session; identical content under another name still deduplicates
	// at the asset layer.
	nameHash := sha256.Sum256([]byte(filename))
	now := time.Now().UTC()
	session, _, err := d.repository.CreateSession(r.Context(), upload.CreateSessionInput{
		OwnerID:             ownerID,
		ClientAssetID:       "direct:" + hashHex + ":" + hex.EncodeToString(nameHash[:8]),
		OriginalFilename:    filename,
		MediaType:           mediaType,
		ExpectedSize:        size,
		ClientSHA256:        clientHash,
		ExpiresAt:           now.Add(7 * 24 * time.Hour),
		Now:                 now,
		AvailableBytes:      available,
		MinimumFreeBytes:    d.minimumFreeBytes,
		MaxActiveSessions:   d.maxActiveSessions,
		CreateWindow:        d.createWindow,
		MaxCreatesPerWindow: d.maxCreatesPerWindow,
	})
	switch {
	case errors.Is(err, upload.ErrConflict):
		writeDirectProblem(w, http.StatusConflict, "idempotency_conflict", "this file was already sent with a different size or type")
		return
	case errors.Is(err, upload.ErrInsufficientStorage):
		writeDirectProblem(w, http.StatusInsufficientStorage, "insufficient_storage", "storage quota or free-space reserve would be exceeded")
		return
	case errors.Is(err, upload.ErrSessionLimit):
		writeDirectProblem(w, http.StatusTooManyRequests, "active_upload_limit", "too many unfinished uploads")
		return
	case errors.Is(err, upload.ErrCreateRateLimit):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(d.createWindow.Seconds()))))
		writeDirectProblem(w, http.StatusTooManyRequests, "upload_create_rate_limited", "too many new uploads; retry later")
		return
	case err != nil:
		d.logger.Error("create direct upload session", "error", err)
		writeDirectProblem(w, http.StatusInternalServerError, "session_create_failed", "could not create upload")
		return
	}
	if done, status := directTerminalState(session.State); done {
		writeDirectJSON(w, status, directUploadResponse{ID: session.ID, State: session.State, Duplicate: session.State == upload.StateAvailable})
		return
	}

	if !d.limiter.Acquire(ownerID) {
		w.Header().Set("Retry-After", "2")
		writeDirectProblem(w, http.StatusTooManyRequests, "too_many_concurrent_uploads", "too many uploads in progress")
		return
	}
	defer d.limiter.Release(ownerID)
	unlock, err := lockTusResource(r.Context(), d.locker, session.ID)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		writeDirectProblem(w, http.StatusConflict, "upload_busy", "this file is already being uploaded")
		return
	}
	defer unlock()

	// Another request may have finished or started this session while we
	// waited for the lock.
	session, err = d.repository.SessionByID(r.Context(), session.ID)
	if err != nil {
		writeDirectProblem(w, http.StatusInternalServerError, "session_lookup_failed", "could not load upload")
		return
	}
	if done, status := directTerminalState(session.State); done {
		writeDirectJSON(w, status, directUploadResponse{ID: session.ID, State: session.State, Duplicate: session.State == upload.StateAvailable})
		return
	}
	if session.State == upload.StateUploading {
		// A previous attempt was cut off. Discard its partial bytes; this
		// request carries the whole file. If the earlier attempt had in fact
		// completed, ResetForRetry records it as received instead.
		reset, err := d.processor.ResetForRetry(r.Context(), session.ID, ownerID)
		if err != nil {
			d.logger.Error("reset direct upload", "upload_id", session.ID, "error", err)
			writeDirectProblem(w, http.StatusConflict, "upload_resource_inconsistent", "previous partial upload could not be reset")
			return
		}
		session = reset
		if done, status := directTerminalState(session.State); done {
			d.wake()
			writeDirectJSON(w, status, directUploadResponse{ID: session.ID, State: session.State})
			return
		}
	}
	if err := d.repository.ClaimTusCreation(r.Context(), session.ID, ownerID, size); err != nil {
		writeDirectProblem(w, http.StatusConflict, "upload_session_state", "upload cannot be started")
		return
	}
	stored, err := d.store.NewUpload(r.Context(), tusd.FileInfo{
		ID: session.ID, Size: size, MetaData: tusd.MetaData{"session_id": session.ID},
	})
	if err != nil {
		d.logger.Error("create direct upload resource", "upload_id", session.ID, "error", err)
		writeDirectProblem(w, http.StatusInternalServerError, "upload_store_failed", "could not store upload")
		return
	}
	body := &progressDeadlineReader{
		reader:     io.MultiReader(bytes.NewReader(head), limited),
		controller: http.NewResponseController(w),
		idle:       d.readIdleTimeout,
	}
	written, err := stored.WriteChunk(r.Context(), 0, body)
	if err != nil || written != size {
		// The session stays "uploading" with partial bytes; a retry resets it
		// and stale ones expire through the normal reconciliation path.
		writeDirectProblem(w, http.StatusBadRequest, "incomplete_body", fmt.Sprintf("received %d of %d bytes", written, size))
		return
	}
	if err := d.repository.MarkReceived(r.Context(), session.ID, size); err != nil {
		d.logger.Error("mark direct upload received", "upload_id", session.ID, "error", err)
		writeDirectProblem(w, http.StatusInternalServerError, "upload_state_failed", "could not record upload")
		return
	}
	d.wake()
	writeDirectJSON(w, http.StatusAccepted, directUploadResponse{ID: session.ID, State: upload.StateReceived})
}

func (d *directUploader) authenticate(r *http.Request) (string, bool) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	if keyHash, isKey := account.UploadKeyHash(parts[1]); isKey {
		if d.keys == nil {
			return "", false
		}
		ownerID, _, err := d.keys.UploadKeyOwner(r.Context(), keyHash, time.Now().UTC())
		return ownerID, err == nil && ownerID != ""
	}
	principal, err := d.tokens.Verify(parts[1])
	if err != nil {
		return "", false
	}
	if d.accounts != nil {
		active, err := d.accounts.SessionActive(r.Context(), principal.UserID, principal.SessionID)
		if err != nil || !active {
			return "", false
		}
	}
	return principal.UserID, true
}

// directTerminalState reports states where the bytes were already accepted
// (or rejected) and the request must not write again.
func directTerminalState(state upload.State) (bool, int) {
	switch state {
	case upload.StateAvailable:
		return true, http.StatusOK
	case upload.StateReceived, upload.StateVerifying, upload.StateVerified, upload.StateCommitting, upload.StateQuarantining:
		return true, http.StatusAccepted
	case upload.StateQuarantined, upload.StateFailed:
		return true, http.StatusConflict
	default:
		return false, 0
	}
}

func directFilename(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\/\x00") ||
		filepath.Base(name) != name || name == "." || name == ".." {
		return "", false
	}
	return name, true
}

// sniffMediaType recognises the formats an iPhone and common cameras produce
// from their leading bytes, returning the media type and a file extension.
func sniffMediaType(head []byte) (string, string, bool) {
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg", ".JPG", true
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", ".PNG", true
	case bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif", ".GIF", true
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp", ".WEBP", true
	case bytes.HasPrefix(head, []byte("II*\x00")) || bytes.HasPrefix(head, []byte("MM\x00*")):
		return "image/tiff", ".TIF", true
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		switch brand := string(head[8:12]); brand {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			return "image/heic", ".HEIC", true
		case "avif", "avis":
			return "image/avif", ".AVIF", true
		case "qt  ":
			return "video/quicktime", ".MOV", true
		case "M4V ", "M4VH", "M4VP":
			return "video/x-m4v", ".M4V", true
		case "3gp4", "3gp5", "3gp6", "3g2a":
			return "video/3gpp", ".3GP", true
		case "isom", "iso2", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "dash", "MSNV":
			return "video/mp4", ".MP4", true
		}
	}
	return "", "", false
}

func directMediaType(explicit, contentType, filename string) (string, bool) {
	for _, candidate := range []string{explicit, contentType} {
		if candidate == "" {
			continue
		}
		parsed, _, err := mime.ParseMediaType(candidate)
		if err == nil && (strings.HasPrefix(parsed, "image/") || strings.HasPrefix(parsed, "video/")) {
			return parsed, true
		}
	}
	if byExtension, ok := mediaTypesByExtension[strings.ToLower(filepath.Ext(filename))]; ok {
		return byExtension, true
	}
	return "", false
}

// progressDeadlineReader renews the connection read deadline as bytes arrive,
// so a long video upload is limited by inactivity rather than by the
// server-wide absolute read timeout.
type progressDeadlineReader struct {
	reader     io.Reader
	controller *http.ResponseController
	idle       time.Duration
}

func (r *progressDeadlineReader) Read(p []byte) (int, error) {
	if r.idle > 0 {
		_ = r.controller.SetReadDeadline(time.Now().Add(r.idle))
	}
	return r.reader.Read(p)
}

func writeDirectJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeDirectProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code, "detail": detail})
}
