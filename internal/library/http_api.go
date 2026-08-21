package library

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/upload"
)

const (
	defaultPageSize = 50
	maximumPageSize = 100
)

type API struct {
	repository upload.AssetRepository
	mediaRoot  string
}

func NewAPI(repository upload.AssetRepository, mediaRoot string) (*API, error) {
	if repository == nil {
		return nil, errors.New("asset repository is required")
	}
	root, err := filepath.Abs(mediaRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve media root: %w", err)
	}
	return &API{repository: repository, mediaRoot: root}, nil
}

type assetResponse struct {
	ID               string    `json:"id"`
	OriginalFilename string    `json:"original_filename"`
	MediaType        string    `json:"media_type"`
	ByteSize         int64     `json:"byte_size"`
	ContentSHA256    string    `json:"content_sha256"`
	CreatedAt        time.Time `json:"created_at"`
	OriginalURL      string    `json:"original_url"`
}

type listResponse struct {
	Assets     []assetResponse `json:"assets"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type cursorPayload struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.SessionID == "" {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/v1/assets")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodGet:
		api.list(w, r, principal)
	case strings.HasSuffix(path, "/original") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		assetID := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/original")
		if assetID == "" || strings.Contains(assetID, "/") {
			writeProblem(w, http.StatusNotFound, "not_found", "asset not found")
			return
		}
		api.original(w, r, principal, assetID)
	default:
		w.Header().Set("Allow", "GET, HEAD")
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (api *API) list(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	limit, err := pageSize(r.URL.Query().Get("limit"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 through 100")
		return
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
		return
	}
	assets, err := api.repository.ListAssets(r.Context(), principal.UserID, cursor, limit+1)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "asset_list_failed", "could not list assets")
		return
	}

	response := listResponse{Assets: make([]assetResponse, 0, min(limit, len(assets)))}
	for _, asset := range assets[:min(limit, len(assets))] {
		response.Assets = append(response.Assets, assetForResponse(asset))
	}
	if len(assets) > limit {
		last := assets[limit-1]
		response.NextCursor, err = encodeCursor(upload.AssetCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "cursor_encode_failed", "could not list assets")
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) original(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) {
	asset, err := api.repository.AssetByID(r.Context(), principal.UserID, assetID)
	if errors.Is(err, upload.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "not_found", "asset not found")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "asset_lookup_failed", "could not load asset")
		return
	}
	path, err := api.originalPath(asset.StorageKey)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "asset_storage_key_invalid", "asset storage is unavailable")
		return
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeProblem(w, http.StatusGone, "asset_bytes_missing", "asset bytes are missing")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "asset_read_failed", "could not read asset")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeProblem(w, http.StatusInternalServerError, "asset_read_failed", "could not read asset")
		return
	}
	if info.Size() != asset.ByteSize {
		writeProblem(w, http.StatusConflict, "asset_size_mismatch", "asset failed integrity precondition")
		return
	}

	w.Header().Set("Content-Type", asset.MediaType)
	w.Header().Set("Content-Disposition", "inline; filename=\"original\"")
	w.Header().Set("ETag", `"sha256-`+hex.EncodeToString(asset.ContentSHA256[:])+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "original", asset.CreatedAt, file)
}

func (api *API) originalPath(storageKey string) (string, error) {
	if storageKey == "" || filepath.IsAbs(storageKey) {
		return "", errors.New("storage key must be relative")
	}
	candidate := filepath.Join(api.mediaRoot, filepath.FromSlash(storageKey))
	relative, err := filepath.Rel(api.mediaRoot, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("storage key leaves media root")
	}
	return candidate, nil
}

func assetForResponse(asset upload.Asset) assetResponse {
	return assetResponse{
		ID:               asset.ID,
		OriginalFilename: asset.OriginalFilename,
		MediaType:        asset.MediaType,
		ByteSize:         asset.ByteSize,
		ContentSHA256:    hex.EncodeToString(asset.ContentSHA256[:]),
		CreatedAt:        asset.CreatedAt,
		OriginalURL:      "/v1/assets/" + asset.ID + "/original",
	}
}

func pageSize(raw string) (int, error) {
	if raw == "" {
		return defaultPageSize, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maximumPageSize {
		return 0, errors.New("invalid page size")
	}
	return value, nil
}

func encodeCursor(cursor upload.AssetCursor) (string, error) {
	payload, err := json.Marshal(cursorPayload{CreatedAt: cursor.CreatedAt.UTC(), ID: cursor.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(raw string) (*upload.AssetCursor, error) {
	if raw == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(payload) > 512 {
		return nil, errors.New("invalid cursor")
	}
	var decoded cursorPayload
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.ID == "" || decoded.CreatedAt.IsZero() {
		return nil, errors.New("invalid cursor")
	}
	return &upload.AssetCursor{CreatedAt: decoded.CreatedAt, ID: decoded.ID}, nil
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
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
