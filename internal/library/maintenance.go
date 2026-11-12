package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"family-photo-cloud/internal/media"
)

// MetadataVersion is bumped when extraction learns something new, so older
// assets are read again in the background.
const MetadataVersion = 1

// serverThumbnailSide is the longer side of previews the server renders.
const serverThumbnailSide = 400

// Maintenance does the library's background work for newly uploaded items:
// it reads capture metadata (date taken, size, camera, place, Live Photo
// pairing) and renders previews of JPEG and PNG photos.
type Maintenance struct {
	Store     Store
	MediaRoot string
	Location  *time.Location
	Logger    *slog.Logger
}

// ProcessPending handles up to limit items and reports how many it did.
func (m *Maintenance) ProcessPending(ctx context.Context, limit int) (int, error) {
	pending, err := m.Store.PendingMetadata(ctx, MetadataVersion, limit)
	if err != nil {
		return 0, err
	}
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		meta := m.read(item)
		if err := m.Store.SaveMetadata(ctx, item.ID, MetadataVersion, meta); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

func (m *Maintenance) read(item PendingMetadata) media.Metadata {
	path, err := m.path(item.StorageKey)
	if err != nil {
		return media.Metadata{}
	}
	file, err := os.Open(path)
	if err != nil {
		// Missing bytes are the integrity scrub's to report; record the
		// attempt so this item is not retried forever.
		m.Logger.Warn("read metadata", "asset_id", item.ID, "error", err)
		return media.Metadata{}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return media.Metadata{}
	}
	meta, err := media.Extract(file, info.Size(), m.Location)
	if err != nil && !errors.Is(err, media.ErrUnsupported) {
		m.Logger.Info("partial metadata", "asset_id", item.ID, "error", err)
	}
	if strings.HasPrefix(item.MediaType, "image/") {
		m.renderThumbnail(item, file, meta.Orientation)
	}
	return meta
}

func (m *Maintenance) renderThumbnail(item PendingMetadata, file *os.File, orientation int) {
	target := filepath.Join(m.MediaRoot, "thumbnails", item.OwnerID, item.ID+".jpg")
	if !thumbnailSafeID.MatchString(item.OwnerID) || !thumbnailSafeID.MatchString(item.ID) {
		return
	}
	if _, err := os.Stat(target); err == nil {
		return
	}
	if _, err := file.Seek(0, 0); err != nil {
		return
	}
	preview, err := media.Thumbnail(file, serverThumbnailSide, orientation)
	if err != nil {
		return // HEIC and other formats get their preview from the browser
	}
	if err := writeThumbnail(target, preview); err != nil {
		m.Logger.Warn("write thumbnail", "asset_id", item.ID, "error", err)
	}
}

func (m *Maintenance) path(storageKey string) (string, error) {
	api := API{mediaRoot: m.MediaRoot}
	return api.originalPath(storageKey)
}
