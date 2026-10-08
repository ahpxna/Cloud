package library

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"family-photo-cloud/internal/auth"
)

// Thumbnails are derived, replaceable previews. iPhone originals are mostly
// HEIC, which Go cannot decode, so the owner's browser renders a preview and
// uploads it once; the server never trusts those bytes as-is but decodes and
// re-encodes them within strict bounds. Originals are never touched.
const (
	thumbnailMaxUploadBytes = 512 << 10
	thumbnailMaxSide        = 640
	thumbnailQuality        = 80
	thumbnailTicketTTL      = 2 * time.Hour
	thumbnailCacheControl   = "private, max-age=3600"
)

var thumbnailSafeID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func (api *API) thumbnailPath(ownerID, assetID string) (string, error) {
	if !thumbnailSafeID.MatchString(ownerID) || !thumbnailSafeID.MatchString(assetID) {
		return "", errors.New("unsafe thumbnail identifier")
	}
	return filepath.Join(api.mediaRoot, "thumbnails", ownerID, assetID+".jpg"), nil
}

func (api *API) hasThumbnail(ownerID, assetID string) bool {
	path, err := api.thumbnailPath(ownerID, assetID)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// thumbnailURL returns a view URL whose ticket is identical for a whole hour,
// so the browser can cache the image across library reloads.
func (api *API) thumbnailURL(principal auth.Principal, assetID string, now time.Time) (string, error) {
	ticket, err := api.viewTickets.IssueView(principal, assetID, auth.ViewThumbnail, now.Truncate(time.Hour), thumbnailTicketTTL)
	if err != nil {
		return "", err
	}
	return "/v1/assets/" + assetID + "/thumbnail?ticket=" + url.QueryEscape(ticket), nil
}

func (api *API) serveThumbnail(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) {
	asset, err := api.store.Accessible(r.Context(), principal.UserID, assetID)
	if err != nil {
		api.writeStoreError(w, err, "could not load asset")
		return
	}
	path, err := api.thumbnailPath(asset.OwnerID, asset.ID)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "not_found", "thumbnail not found")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "thumbnail_missing", "thumbnail not generated yet")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeProblem(w, http.StatusNotFound, "thumbnail_missing", "thumbnail not generated yet")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", thumbnailCacheControl)
	http.ServeContent(w, r, "thumbnail.jpg", info.ModTime(), file)
}

func (api *API) storeThumbnail(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "image/jpeg" {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "thumbnail must be image/jpeg")
		return
	}
	asset, err := api.store.Accessible(r.Context(), principal.UserID, assetID)
	if err != nil {
		api.writeStoreError(w, err, "could not load asset")
		return
	}
	// Only the owner's devices write previews of their photos.
	if asset.OwnerID != principal.UserID {
		writeProblem(w, http.StatusForbidden, "forbidden", "only the owner can set a thumbnail")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, thumbnailMaxUploadBytes))
	if err != nil {
		writeProblem(w, http.StatusRequestEntityTooLarge, "thumbnail_too_large", "thumbnail must be at most 512 KiB")
		return
	}
	// Check dimensions before decoding pixels so a crafted header cannot
	// trigger a huge allocation.
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || format != "jpeg" || config.Width < 1 || config.Height < 1 ||
		config.Width > thumbnailMaxSide || config.Height > thumbnailMaxSide {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_thumbnail", "thumbnail must be a JPEG of at most 640x640")
		return
	}
	decoded, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_thumbnail", "thumbnail could not be decoded")
		return
	}
	path, err := api.thumbnailPath(asset.OwnerID, asset.ID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "thumbnail_store_failed", "could not store thumbnail")
		return
	}
	if err := writeThumbnail(path, decoded); err != nil {
		writeProblem(w, http.StatusInternalServerError, "thumbnail_store_failed", "could not store thumbnail")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeThumbnail re-encodes the decoded pixels (dropping any metadata the
// client sent) and replaces the file atomically.
func writeThumbnail(path string, decoded image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".thumb-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := jpeg.Encode(temporary, decoded, &jpeg.Options{Quality: thumbnailQuality}); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
