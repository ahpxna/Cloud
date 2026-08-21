package upload

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"family-photo-cloud/internal/auth"
)

type API struct {
	repository Repository
	maxBytes   int64
	chunkBytes int64
	tokens     *auth.AccessTokenManager
	now        func() time.Time
}

func NewAPI(repository Repository, maxBytes, chunkBytes int64, tokens *auth.AccessTokenManager) *API {
	return &API{
		repository: repository,
		maxBytes:   maxBytes,
		chunkBytes: chunkBytes,
		tokens:     tokens,
		now:        time.Now,
	}
}

type createSessionRequest struct {
	ClientAssetID    string `json:"client_asset_id"`
	OriginalFilename string `json:"original_filename"`
	MediaType        string `json:"media_type"`
	ExpectedSize     int64  `json:"expected_size"`
	ClientSHA256     string `json:"client_sha256"`
}

type sessionResponse struct {
	ID             string `json:"id"`
	State          State  `json:"state"`
	ExpectedSize   int64  `json:"expected_size"`
	ReceivedSize   int64  `json:"received_size"`
	UploadEndpoint string `json:"upload_endpoint"`
	SessionID      string `json:"session_id_metadata"`
	ChunkBytes     int64  `json:"recommended_chunk_bytes"`
	Created        bool   `json:"created"`
	UploadToken    string `json:"upload_token,omitempty"`
}

func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/v1/upload-sessions")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodPost:
		api.create(w, r, principal)
	case strings.HasPrefix(path, "/") && len(path) > 1 && r.Method == http.MethodGet:
		api.get(w, r, principal, strings.TrimPrefix(path, "/"))
	default:
		w.Header().Set("Allow", "GET, POST")
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (api *API) create(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var request createSessionRequest
	if err := decoder.Decode(&request); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, http.StatusBadRequest, "invalid_json", "request must contain one JSON object")
		return
	}
	if err := validateCreateRequest(request, api.maxBytes); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_upload", err.Error())
		return
	}
	hashBytes, _ := hex.DecodeString(request.ClientSHA256)
	var hash [32]byte
	copy(hash[:], hashBytes)

	session, created, err := api.repository.CreateSession(r.Context(), CreateSessionInput{
		OwnerID:          principal.UserID,
		ClientAssetID:    request.ClientAssetID,
		OriginalFilename: request.OriginalFilename,
		MediaType:        request.MediaType,
		ExpectedSize:     request.ExpectedSize,
		ClientSHA256:     hash,
		ExpiresAt:        api.now().UTC().Add(7 * 24 * time.Hour),
	})
	if errors.Is(err, ErrConflict) {
		writeProblem(w, http.StatusConflict, "idempotency_conflict", "client_asset_id already has different immutable metadata")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "session_create_failed", "could not create upload session")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	now := api.now().UTC()
	uploadToken, err := api.tokens.IssueUpload(principal.UserID, session.ID, now, session.ExpiresAt.Sub(now))
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "upload_token_failed", "could not issue upload capability")
		return
	}
	writeJSON(w, status, sessionResponse{
		ID:             session.ID,
		State:          session.State,
		ExpectedSize:   session.ExpectedSize,
		ReceivedSize:   session.ReceivedSize,
		UploadEndpoint: "/v1/uploads/",
		SessionID:      session.ID,
		ChunkBytes:     api.chunkBytes,
		Created:        created,
		UploadToken:    uploadToken,
	})
}

func (api *API) get(w http.ResponseWriter, r *http.Request, principal auth.Principal, id string) {
	if strings.Contains(id, "/") {
		writeProblem(w, http.StatusNotFound, "not_found", "upload session not found")
		return
	}
	session, err := api.repository.SessionByID(r.Context(), id)
	if err != nil || subtle.ConstantTimeCompare([]byte(session.OwnerID), []byte(principal.UserID)) != 1 {
		writeProblem(w, http.StatusNotFound, "not_found", "upload session not found")
		return
	}
	now := api.now().UTC()
	uploadToken, err := api.tokens.IssueUpload(principal.UserID, session.ID, now, session.ExpiresAt.Sub(now))
	if err != nil {
		writeProblem(w, http.StatusGone, "upload_session_expired", "upload session has expired")
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		ID:             session.ID,
		State:          session.State,
		ExpectedSize:   session.ExpectedSize,
		ReceivedSize:   session.ReceivedSize,
		UploadEndpoint: "/v1/uploads/",
		SessionID:      session.ID,
		ChunkBytes:     api.chunkBytes,
		UploadToken:    uploadToken,
	})
}

func validateCreateRequest(request createSessionRequest, maxBytes int64) error {
	if request.ClientAssetID == "" || len(request.ClientAssetID) > 200 || !utf8.ValidString(request.ClientAssetID) {
		return errors.New("client_asset_id must contain 1 to 200 valid UTF-8 bytes")
	}
	if request.OriginalFilename == "" || len(request.OriginalFilename) > 255 ||
		!utf8.ValidString(request.OriginalFilename) || filepath.Base(request.OriginalFilename) != request.OriginalFilename ||
		strings.ContainsAny(request.OriginalFilename, "\\/\x00") {
		return errors.New("original_filename must be a basename of at most 255 UTF-8 bytes")
	}
	parsedMediaType, _, err := mime.ParseMediaType(request.MediaType)
	if err != nil || (!strings.HasPrefix(parsedMediaType, "image/") && !strings.HasPrefix(parsedMediaType, "video/")) {
		return errors.New("media_type must be an image/* or video/* MIME type")
	}
	if request.ExpectedSize <= 0 || (maxBytes > 0 && request.ExpectedSize > maxBytes) {
		return errors.New("expected_size is outside the configured upload limit")
	}
	hash, err := hex.DecodeString(request.ClientSHA256)
	if err != nil || len(hash) != 32 {
		return errors.New("client_sha256 must be a 64-character hexadecimal SHA-256 digest")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"code":   code,
		"detail": detail,
	})
}
