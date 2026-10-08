package library

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/upload"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	defaultPageSize = 50
	maximumPageSize = 100

	// Originals can be multi-gigabyte videos served over a home uplink, so the
	// server-wide absolute HTTP_WRITE_TIMEOUT would cut them off. Downloads
	// instead get an idle deadline that moves with progress, bounded by an
	// overall ceiling so a trickling client cannot hold a connection forever.
	originalWriteIdleTimeout = 2 * time.Minute
	originalWriteMaxDuration = 12 * time.Hour
)

// Purger permanently deletes an item from Recently Deleted.
type Purger interface {
	Purge(ctx context.Context, ownerID, assetID string) error
}

type API struct {
	store            Store
	mediaRoot        string
	writeIdleTimeout time.Duration
	writeMaxDuration time.Duration
	viewTickets      *auth.AccessTokenManager
	purger           Purger
	downloads        *downloadSets
}

// viewTicketTTL bounds how long a listed view_url keeps working.
const viewTicketTTL = 10 * time.Minute

// EnableViewTickets lets GET /v1/assets?tickets=1 attach a short-lived,
// asset-scoped view_url to each item for browser <img>/<video> elements.
func (api *API) EnableViewTickets(tokens *auth.AccessTokenManager) {
	api.viewTickets = tokens
}

// UseStore switches the API to the full library: smart albums, albums and
// folders, sharing, Recently Deleted, places and downloads.
func (api *API) UseStore(store Store, purger Purger) {
	api.store = store
	api.purger = purger
}

// NewAPI serves the read-only library over an upload.AssetRepository. Call
// UseStore to enable everything else.
func NewAPI(repository upload.AssetRepository, mediaRoot string) (*API, error) {
	if repository == nil {
		return nil, errors.New("asset repository is required")
	}
	root, err := filepath.Abs(mediaRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve media root: %w", err)
	}
	return &API{
		store:            legacyStore{assets: repository},
		mediaRoot:        root,
		writeIdleTimeout: originalWriteIdleTimeout,
		writeMaxDuration: originalWriteMaxDuration,
		downloads:        newDownloadSets(),
	}, nil
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.SessionID == "" {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "valid access token required")
		return
	}
	switch path := r.URL.Path; {
	case path == "/v1/assets" || strings.HasPrefix(path, "/v1/assets/"):
		api.serveAssets(w, r, principal, strings.TrimPrefix(path, "/v1/assets"))
	case path == "/v1/albums" || strings.HasPrefix(path, "/v1/albums/"):
		api.serveAlbums(w, r, principal, strings.Trim(strings.TrimPrefix(path, "/v1/albums"), "/"))
	case path == "/v1/album-folders" || strings.HasPrefix(path, "/v1/album-folders/"):
		api.serveFolders(w, r, principal, strings.Trim(strings.TrimPrefix(path, "/v1/album-folders"), "/"))
	case path == "/v1/places" || strings.HasPrefix(path, "/v1/places/"):
		api.servePlaces(w, r, principal, strings.Trim(strings.TrimPrefix(path, "/v1/places"), "/"))
	case path == "/v1/library/summary" && r.Method == http.MethodGet:
		api.summary(w, r, principal)
	case path == "/v1/people" || path == "/v1/people/me":
		api.servePeople(w, r, principal, path)
	case path == "/v1/activity" && r.Method == http.MethodGet:
		api.activity(w, r, principal)
	default:
		writeProblem(w, http.StatusNotFound, "not_found", "not found")
	}
}

func (api *API) serveAssets(w http.ResponseWriter, r *http.Request, principal auth.Principal, path string) {
	method := r.Method
	switch {
	case (path == "" || path == "/") && method == http.MethodGet:
		api.list(w, r, principal)
		return
	case path == "/batch" && method == http.MethodPost:
		api.batch(w, r, principal)
		return
	case path == "/trash/empty" && method == http.MethodPost:
		api.emptyTrash(w, r, principal)
		return
	case path == "/trash/restore" && method == http.MethodPost:
		api.restoreAll(w, r, principal)
		return
	case path == "/download" && method == http.MethodPost:
		api.createDownload(w, r, principal)
		return
	case strings.HasPrefix(path, "/download/") && method == http.MethodGet:
		api.serveDownload(w, r, principal, strings.TrimPrefix(path, "/download/"))
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if path == "" || path == "/" {
		w.Header().Set("Allow", "GET")
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if len(parts) > 2 || !safeID.MatchString(parts[0]) {
		writeProblem(w, http.StatusNotFound, "not_found", "asset not found")
		return
	}
	assetID := parts[0]
	if len(parts) == 1 {
		switch method {
		case http.MethodGet:
			api.detail(w, r, principal, assetID)
		case http.MethodPatch:
			api.patchAsset(w, r, principal, assetID)
		default:
			w.Header().Set("Allow", "GET, PATCH")
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}
	switch parts[1] {
	case "original":
		if method != http.MethodGet && method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		api.original(w, r, principal, assetID)
	case "clip":
		if method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		api.clip(w, r, principal, assetID)
	case "thumbnail":
		switch method {
		case http.MethodGet, http.MethodHead:
			api.serveThumbnail(w, r, principal, assetID)
		case http.MethodPut:
			api.storeThumbnail(w, r, principal, assetID)
		default:
			w.Header().Set("Allow", "GET, HEAD, PUT")
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	default:
		writeProblem(w, http.StatusNotFound, "not_found", "not found")
	}
}

// ------------------------------------------------------------------ items

type liveVideoResponse struct {
	ID               string `json:"id"`
	OriginalFilename string `json:"original_filename"`
	MediaType        string `json:"media_type"`
	ByteSize         int64  `json:"byte_size"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	OriginalURL      string `json:"original_url"`
	ViewURL          string `json:"view_url,omitempty"`
}

type itemResponse struct {
	ID               string             `json:"id"`
	OriginalFilename string             `json:"original_filename"`
	MediaType        string             `json:"media_type"`
	ByteSize         int64              `json:"byte_size"`
	ContentSHA256    string             `json:"content_sha256"`
	CreatedAt        time.Time          `json:"created_at"`
	TakenAt          time.Time          `json:"taken_at"`
	CapturedAt       *time.Time         `json:"captured_at,omitempty"`
	Width            int                `json:"width,omitempty"`
	Height           int                `json:"height,omitempty"`
	DurationMS       int64              `json:"duration_ms,omitempty"`
	Favorite         bool               `json:"favorite"`
	Hidden           bool               `json:"hidden,omitempty"`
	TrashedAt        *time.Time         `json:"trashed_at,omitempty"`
	PurgeAt          *time.Time         `json:"purge_at,omitempty"`
	Caption          string             `json:"caption,omitempty"`
	Subtype          string             `json:"subtype,omitempty"`
	LivePhotoID      string             `json:"live_photo_id,omitempty"`
	LiveVideo        *liveVideoResponse `json:"live_video,omitempty"`
	Latitude         *float64           `json:"latitude,omitempty"`
	Longitude        *float64           `json:"longitude,omitempty"`
	PlaceCell        string             `json:"place_cell,omitempty"`
	PlaceName        string             `json:"place_name,omitempty"`
	OwnerID          string             `json:"owner_id"`
	OwnerName        string             `json:"owner_name,omitempty"`
	Mine             bool               `json:"mine"`
	AddedByName      string             `json:"added_by_name,omitempty"`
	LikeCount        int                `json:"like_count,omitempty"`
	Liked            bool               `json:"liked,omitempty"`
	CommentCount     int                `json:"comment_count,omitempty"`
	Details          map[string]any     `json:"details,omitempty"`
	Albums           []albumRef         `json:"albums,omitempty"`
	OriginalURL      string             `json:"original_url"`
	ViewURL          string             `json:"view_url,omitempty"`
	ThumbnailURL     string             `json:"thumbnail_url,omitempty"`
}

type albumRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsOwner bool   `json:"is_owner"`
}

func (api *API) itemResponse(principal auth.Principal, item Item, withTickets bool, now time.Time) (itemResponse, error) {
	response := itemResponse{
		ID: item.ID, OriginalFilename: item.OriginalFilename, MediaType: item.MediaType,
		ByteSize: item.ByteSize, ContentSHA256: hex.EncodeToString(item.ContentSHA256[:]),
		CreatedAt: item.CreatedAt, TakenAt: item.TakenAt, CapturedAt: item.CapturedAt,
		Width: item.Width, Height: item.Height, DurationMS: item.DurationMS,
		Favorite: item.Favorite, Hidden: item.Hidden, TrashedAt: item.TrashedAt,
		Caption: item.Caption, Subtype: item.Subtype, LivePhotoID: item.LivePhotoID,
		Latitude: item.Latitude, Longitude: item.Longitude, PlaceCell: item.PlaceCell, PlaceName: item.PlaceName,
		OwnerID: item.OwnerID, Mine: item.OwnerID == principal.UserID,
		AddedByName: item.AddedByName, LikeCount: item.LikeCount, Liked: item.LikedByMe, CommentCount: item.CommentCount,
		OriginalURL: "/v1/assets/" + item.ID + "/original",
	}
	if !response.Mine {
		response.OwnerName = item.OwnerName
	}
	if item.TrashedAt != nil {
		purge := item.TrashedAt.Add(TrashRetention)
		response.PurgeAt = &purge
	}
	if item.LiveVideo != nil {
		response.LiveVideo = &liveVideoResponse{
			ID: item.LiveVideo.ID, OriginalFilename: item.LiveVideo.OriginalFilename, MediaType: item.LiveVideo.MediaType,
			ByteSize: item.LiveVideo.ByteSize, DurationMS: item.LiveVideo.DurationMS,
			OriginalURL: "/v1/assets/" + item.LiveVideo.ID + "/original",
		}
	}
	if withTickets {
		ticket, err := api.viewTickets.IssueView(principal, item.ID, auth.ViewOriginal, now, viewTicketTTL)
		if err != nil {
			return itemResponse{}, err
		}
		response.ViewURL = response.OriginalURL + "?ticket=" + url.QueryEscape(ticket)
		if response.LiveVideo != nil {
			ticket, err := api.viewTickets.IssueView(principal, item.LiveVideo.ID, auth.ViewOriginal, now, viewTicketTTL)
			if err != nil {
				return itemResponse{}, err
			}
			response.LiveVideo.ViewURL = response.LiveVideo.OriginalURL + "?ticket=" + url.QueryEscape(ticket)
		}
		if api.hasThumbnail(item.OwnerID, item.ID) {
			if response.ThumbnailURL, err = api.thumbnailURL(principal, item.ID, now); err != nil {
				return itemResponse{}, err
			}
		}
	}
	return response, nil
}

type listResponse struct {
	Assets     []itemResponse `json:"assets"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

func (api *API) list(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	values := r.URL.Query()
	limit, err := pageSize(values.Get("limit"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 through 100")
		return
	}
	cursor, err := decodeCursor(values.Get("cursor"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
		return
	}
	query := ListQuery{
		ViewerID: principal.UserID, View: values.Get("view"), AlbumID: values.Get("album"),
		PlaceCell: values.Get("place"), Sort: values.Get("sort"), Media: values.Get("media"),
		Search: values.Get("q"), Cursor: cursor, Limit: limit,
	}
	if query.AlbumID != "" {
		if !safeID.MatchString(query.AlbumID) {
			writeProblem(w, http.StatusNotFound, "not_found", "album not found")
			return
		}
		query.View = "album"
	}
	if query.PlaceCell != "" {
		query.View = "place"
	}
	if raw := values.Get("ids"); raw != "" {
		query.View = "ids"
		for _, id := range strings.Split(raw, ",") {
			if !safeID.MatchString(id) {
				writeProblem(w, http.StatusBadRequest, "invalid_ids", "ids must be a comma-separated list of asset IDs")
				return
			}
			query.IDs = append(query.IDs, id)
		}
		if len(query.IDs) > 200 {
			writeProblem(w, http.StatusBadRequest, "invalid_ids", "at most 200 ids")
			return
		}
		query.Limit = len(query.IDs)
	}
	if query.View == "on_this_day" {
		query.Month, _ = strconv.Atoi(values.Get("month"))
		query.Day, _ = strconv.Atoi(values.Get("day"))
		query.TZOffset, _ = strconv.Atoi(values.Get("tzoffset"))
	}
	items, next, err := api.store.List(r.Context(), query)
	if err != nil {
		api.writeStoreError(w, err, "could not list assets")
		return
	}
	withTickets := values.Get("tickets") == "1" && api.viewTickets != nil
	now := time.Now()
	response := listResponse{Assets: make([]itemResponse, 0, len(items))}
	for _, item := range items {
		entry, err := api.itemResponse(principal, item, withTickets, now)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "view_ticket_failed", "could not list assets")
			return
		}
		response.Assets = append(response.Assets, entry)
	}
	if len(next) > 0 {
		response.NextCursor, err = encodeCursor(next)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "cursor_encode_failed", "could not list assets")
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) detail(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) {
	item, err := api.store.Accessible(r.Context(), principal.UserID, assetID)
	if err != nil {
		api.writeStoreError(w, err, "could not load asset")
		return
	}
	response, err := api.itemResponse(principal, item, r.URL.Query().Get("tickets") == "1" && api.viewTickets != nil, time.Now())
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "view_ticket_failed", "could not load asset")
		return
	}
	response.Details = item.Details
	if albums, err := api.store.AssetAlbums(r.Context(), principal.UserID, assetID); err == nil {
		for _, album := range albums {
			response.Albums = append(response.Albums, albumRef{ID: album.ID, Name: album.Name, IsOwner: album.IsOwner})
		}
	} else if !errors.Is(err, ErrUnsupported) {
		writeProblem(w, http.StatusInternalServerError, "asset_lookup_failed", "could not load asset")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *API) patchAsset(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) {
	var request struct {
		Caption  *string `json:"caption"`
		Favorite *bool   `json:"favorite"`
		Hidden   *bool   `json:"hidden"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	ctx := r.Context()
	if request.Caption != nil {
		if len([]rune(*request.Caption)) > 2000 {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_caption", "caption is at most 2000 characters")
			return
		}
		if err := api.store.SetCaption(ctx, principal.UserID, assetID, strings.TrimSpace(*request.Caption)); err != nil {
			api.writeStoreError(w, err, "could not update asset")
			return
		}
	}
	for _, change := range []struct {
		value   *bool
		yes, no string
	}{{request.Favorite, ActionFavorite, ActionUnfavorite}, {request.Hidden, ActionHide, ActionUnhide}} {
		if change.value == nil {
			continue
		}
		action := change.no
		if *change.value {
			action = change.yes
		}
		if _, err := api.store.UpdateAssets(ctx, principal.UserID, []string{assetID}, action); err != nil {
			api.writeStoreError(w, err, "could not update asset")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *API) batch(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	var request struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.IDs) == 0 || len(request.IDs) > 1000 || !allSafe(request.IDs) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_ids", "send 1 to 1000 asset IDs")
		return
	}
	if request.Action == "purge" {
		purged, err := api.purgeAll(r.Context(), principal.UserID, request.IDs)
		if err != nil {
			api.writeStoreError(w, err, "could not delete items")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"changed": purged})
		return
	}
	changed, err := api.store.UpdateAssets(r.Context(), principal.UserID, request.IDs, request.Action)
	if err != nil {
		api.writeStoreError(w, err, "could not update items")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"changed": changed})
}

func (api *API) purgeAll(ctx context.Context, ownerID string, ids []string) (int, error) {
	if api.purger == nil {
		return 0, ErrUnsupported
	}
	purged := 0
	for _, id := range ids {
		err := api.purger.Purge(ctx, ownerID, id)
		if errors.Is(err, ErrNotFound) || errors.Is(err, upload.ErrNotFound) {
			continue
		}
		if err != nil {
			return purged, err
		}
		purged++
	}
	return purged, nil
}

func (api *API) emptyTrash(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ids, err := api.store.TrashedIDs(r.Context(), principal.UserID)
	if err != nil {
		api.writeStoreError(w, err, "could not empty Recently Deleted")
		return
	}
	purged, err := api.purgeAll(r.Context(), principal.UserID, ids)
	if err != nil {
		api.writeStoreError(w, err, "could not empty Recently Deleted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"changed": purged})
}

func (api *API) restoreAll(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ids, err := api.store.TrashedIDs(r.Context(), principal.UserID)
	if err != nil {
		api.writeStoreError(w, err, "could not restore items")
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, map[string]int{"changed": 0})
		return
	}
	changed := 0
	for start := 0; start < len(ids); start += 1000 {
		end := min(start+1000, len(ids))
		count, err := api.store.UpdateAssets(r.Context(), principal.UserID, ids[start:end], ActionRestore)
		if err != nil {
			api.writeStoreError(w, err, "could not restore items")
			return
		}
		changed += count
	}
	writeJSON(w, http.StatusOK, map[string]int{"changed": changed})
}

func (api *API) summary(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	counts, covers, err := api.store.Counts(r.Context(), principal.UserID)
	if err != nil {
		api.writeStoreError(w, err, "could not load the library summary")
		return
	}
	usage, err := api.store.Usage(r.Context(), principal.UserID)
	if err != nil {
		api.writeStoreError(w, err, "could not load the library summary")
		return
	}
	now := time.Now()
	coverURLs := map[string]string{}
	if api.viewTickets != nil {
		for name, id := range covers {
			if api.hasThumbnail(principal.UserID, id) {
				if link, err := api.thumbnailURL(principal, id, now); err == nil {
					coverURLs[name] = link
				}
			}
		}
	}
	var free *int64
	if available, err := upload.AvailableBytes(api.mediaRoot)(); err == nil {
		free = &available
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"counts": counts,
		"covers": coverURLs,
		"usage": map[string]any{
			"photo_count": usage.PhotoCount, "photo_bytes": usage.PhotoBytes,
			"video_count": usage.VideoCount, "video_bytes": usage.VideoBytes,
			"trash_count": usage.TrashCount, "trash_bytes": usage.TrashBytes,
			"hidden_count": usage.HiddenCount, "quota_bytes": usage.QuotaBytes,
			"disk_free_bytes": free,
		},
		"trash_retention_days": int(TrashRetention / (24 * time.Hour)),
		"sorts":                SortNames,
	})
}

// ------------------------------------------------------------------ originals

func (api *API) original(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) {
	asset, file, ok := api.openOriginal(w, r, principal, assetID)
	if !ok {
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", asset.MediaType)
	w.Header().Set("Content-Disposition", "inline; filename=\"original\"")
	w.Header().Set("ETag", `"sha256-`+hex.EncodeToString(asset.ContentSHA256[:])+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	modified := asset.CreatedAt
	http.ServeContent(api.progressDeadlineWriter(w, time.Now()), r, "original", modified, file)
}

// openOriginal finds an item the viewer may open and opens its bytes,
// writing the error response itself when it cannot.
func (api *API) openOriginal(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) (Item, *os.File, bool) {
	asset, err := api.store.Accessible(r.Context(), principal.UserID, assetID)
	if err != nil {
		api.writeStoreError(w, err, "could not load asset")
		return Item{}, nil, false
	}
	path, err := api.originalPath(asset.StorageKey)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "asset_storage_key_invalid", "asset storage is unavailable")
		return Item{}, nil, false
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeProblem(w, http.StatusGone, "asset_bytes_missing", "asset bytes are missing")
		return Item{}, nil, false
	}
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "asset_read_failed", "could not read asset")
		return Item{}, nil, false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		writeProblem(w, http.StatusInternalServerError, "asset_read_failed", "could not read asset")
		return Item{}, nil, false
	}
	if info.Size() != asset.ByteSize {
		file.Close()
		writeProblem(w, http.StatusConflict, "asset_size_mismatch", "asset failed integrity precondition")
		return Item{}, nil, false
	}
	return asset, file, true
}

// progressDeadlineWriter extends the connection write deadline before every
// write. Only the ResponseWriter methods are exposed, so io.Copy feeds it in
// bounded buffers and each buffer renews the idle deadline.
type progressDeadlineWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	idle       time.Duration
	ceiling    time.Time
}

func (api *API) progressDeadlineWriter(w http.ResponseWriter, start time.Time) *progressDeadlineWriter {
	writer := &progressDeadlineWriter{
		ResponseWriter: w,
		controller:     http.NewResponseController(w),
		idle:           api.writeIdleTimeout,
		ceiling:        start.Add(api.writeMaxDuration),
	}
	writer.extend()
	return writer
}

func (w *progressDeadlineWriter) extend() {
	deadline := time.Now().Add(w.idle)
	if deadline.After(w.ceiling) {
		deadline = w.ceiling
	}
	// Unsupported writers (for example test recorders) keep the server default.
	_ = w.controller.SetWriteDeadline(deadline)
}

func (w *progressDeadlineWriter) Write(p []byte) (int, error) {
	w.extend()
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying connection.
func (w *progressDeadlineWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
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

// ------------------------------------------------------------------ helpers

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

func encodeCursor(values []string) (string, error) {
	payload, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(payload) > 1024 {
		return nil, errors.New("invalid cursor")
	}
	var values []string
	if err := json.Unmarshal(payload, &values); err != nil || len(values) == 0 || len(values) > 8 {
		return nil, errors.New("invalid cursor")
	}
	for _, value := range values {
		if value == "" || len(value) > 300 {
			return nil, errors.New("invalid cursor")
		}
	}
	return values, nil
}

func allSafe(ids []string) bool {
	for _, id := range ids {
		if !safeID.MatchString(id) {
			return false
		}
	}
	return true
}

// decodeJSON reads one small JSON object, writing the error response itself.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, http.StatusBadRequest, "invalid_json", "request must contain one JSON object")
		return false
	}
	return true
}

// writeStoreError maps store errors to problem responses.
func (api *API) writeStoreError(w http.ResponseWriter, err error, detail string) {
	var pgError *pgconn.PgError
	switch {
	case errors.Is(err, ErrNotFound) || errors.Is(err, upload.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.As(err, &pgError) && pgError.Code == "22P02":
		// A malformed ID never names anything.
		writeProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrForbidden):
		writeProblem(w, http.StatusForbidden, "forbidden", "only the album's owner can do that")
	case errors.Is(err, ErrInvalid):
		writeProblem(w, http.StatusBadRequest, "invalid_request", "the request is not valid")
	case errors.Is(err, ErrUnsupported):
		writeProblem(w, http.StatusNotImplemented, "unsupported", "this server does not support that yet")
	default:
		writeProblem(w, http.StatusInternalServerError, "library_failed", detail)
	}
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
