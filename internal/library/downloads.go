package library

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/media"
)

// A download selection is kept in memory for a short while: the browser
// first POSTs the chosen IDs, then navigates to a ticketed GET so the ZIP
// streams straight to disk (Safari saves it to Files).
const (
	downloadSetTTL      = 30 * time.Minute
	maxDownloadItems    = 1000
	maxPendingDownloads = 64
)

type downloadSet struct {
	ownerID   string
	sessionID string
	items     []Item
	expires   time.Time
}

type downloadSets struct {
	mu   sync.Mutex
	sets map[string]downloadSet
}

func newDownloadSets() *downloadSets {
	return &downloadSets{sets: make(map[string]downloadSet)}
}

func (d *downloadSets) put(set downloadSet) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for key, existing := range d.sets {
		if now.After(existing.expires) {
			delete(d.sets, key)
		}
	}
	if len(d.sets) >= maxPendingDownloads {
		return "", errors.New("too many pending downloads")
	}
	d.sets[id] = set
	return id, nil
}

func (d *downloadSets) get(id string) (downloadSet, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	set, ok := d.sets[id]
	if !ok || time.Now().After(set.expires) {
		delete(d.sets, id)
		return downloadSet{}, false
	}
	return set, true
}

func (api *API) createDownload(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if api.viewTickets == nil {
		writeProblem(w, http.StatusNotImplemented, "unsupported", "downloads are unavailable")
		return
	}
	var request struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	ids := uniqueStrings(request.IDs)
	if len(ids) == 0 || len(ids) > maxDownloadItems || !allSafe(ids) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_ids", "send 1 to 1000 asset IDs")
		return
	}
	var items []Item
	var total int64
	seen := map[string]bool{}
	for start := 0; start < len(ids); start += 200 {
		batch := ids[start:min(start+200, len(ids))]
		found, _, err := api.store.List(r.Context(), ListQuery{ViewerID: principal.UserID, View: "ids", IDs: batch, Limit: len(batch)})
		if err != nil {
			api.writeStoreError(w, err, "could not prepare the download")
			return
		}
		for _, item := range found {
			// A Live Photo downloads as both its still and its video.
			for _, part := range []*Item{&item, item.LiveVideo} {
				if part == nil || seen[part.ID] {
					continue
				}
				if part != &item {
					video, err := api.store.Accessible(r.Context(), principal.UserID, part.ID)
					if err != nil {
						continue
					}
					part = &video
				}
				seen[part.ID] = true
				items = append(items, *part)
				total += part.ByteSize
			}
		}
	}
	if len(items) == 0 {
		writeProblem(w, http.StatusNotFound, "not_found", "none of these items can be downloaded")
		return
	}
	id, err := api.downloads.put(downloadSet{ownerID: principal.UserID, sessionID: principal.SessionID, items: items, expires: time.Now().Add(downloadSetTTL)})
	if err != nil {
		w.Header().Set("Retry-After", "60")
		writeProblem(w, http.StatusTooManyRequests, "too_many_downloads", "too many downloads are being prepared")
		return
	}
	ticket, err := api.viewTickets.IssueView(principal, id, auth.ViewDownload, time.Now(), downloadSetTTL)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "view_ticket_failed", "could not prepare the download")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"url":         "/v1/assets/download/" + id + "?ticket=" + url.QueryEscape(ticket),
		"count":       len(items),
		"total_bytes": total,
	})
}

// serveDownload streams the selection as an uncompressed ZIP: photos and
// videos are already compressed, and storing them keeps the originals
// byte-for-byte identical inside the archive.
func (api *API) serveDownload(w http.ResponseWriter, r *http.Request, principal auth.Principal, id string) {
	set, ok := api.downloads.get(id)
	if !ok || set.ownerID != principal.UserID || set.sessionID != principal.SessionID {
		writeProblem(w, http.StatusNotFound, "not_found", "download expired; select the items again")
		return
	}
	name := "Anh-gia-dinh-" + time.Now().Format("2006-01-02-1504") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	writer := api.progressDeadlineWriter(w, time.Now())
	archive := zip.NewWriter(writer)
	used := map[string]int{}
	for _, item := range set.items {
		path, err := api.originalPath(item.StorageKey)
		if err != nil {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		header := &zip.FileHeader{Name: uniqueArchiveName(item.OriginalFilename, used), Method: zip.Store, Modified: item.TakenAt}
		header.SetMode(0o644)
		entry, err := archive.CreateHeader(header)
		if err == nil {
			_, err = io.Copy(entry, file)
		}
		file.Close()
		if err != nil {
			return // the client went away; the ZIP is incomplete either way
		}
	}
	_ = archive.Close()
}

func uniqueArchiveName(name string, used map[string]int) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "photo"
	}
	key := strings.ToLower(name)
	used[key]++
	if used[key] == 1 {
		return name
	}
	extension := filepath.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, extension), used[key], extension)
}

// clip streams part of a video, cut losslessly at the requested times.
func (api *API) clip(w http.ResponseWriter, r *http.Request, principal auth.Principal, assetID string) {
	start, errStart := strconv.ParseFloat(r.URL.Query().Get("start"), 64)
	end, errEnd := strconv.ParseFloat(r.URL.Query().Get("end"), 64)
	if errStart != nil || errEnd != nil || math.IsNaN(start) || math.IsNaN(end) || start < 0 || end <= start || end-start < 0.1 {
		writeProblem(w, http.StatusBadRequest, "invalid_range", "start and end must be seconds with end after start")
		return
	}
	asset, file, ok := api.openOriginal(w, r, principal, assetID)
	if !ok {
		return
	}
	defer file.Close()
	if !strings.HasPrefix(asset.MediaType, "video/") {
		writeProblem(w, http.StatusUnprocessableEntity, "not_a_video", "only videos can be cut")
		return
	}
	plan, err := media.PlanClip(file, asset.ByteSize, start, end)
	if errors.Is(err, media.ErrClipUnsupported) {
		writeProblem(w, http.StatusUnprocessableEntity, "clip_unsupported", "this video's format cannot be cut; download the whole video instead")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_range", "the chosen part is outside the video")
		return
	}
	base := strings.TrimSuffix(asset.OriginalFilename, filepath.Ext(asset.OriginalFilename))
	name := fmt.Sprintf("%s (%s-%s)%s", base, clipStamp(start), clipStamp(start+plan.Duration()), filepath.Ext(asset.OriginalFilename))
	w.Header().Set("Content-Type", asset.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(plan.Size(), 10))
	w.Header().Set("Content-Disposition", "attachment; filename=\"clip"+filepath.Ext(asset.OriginalFilename)+"\"; filename*=UTF-8''"+url.PathEscape(name))
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = plan.WriteTo(api.progressDeadlineWriter(w, time.Now()))
}

func clipStamp(seconds float64) string {
	whole := int(seconds)
	tenths := int((seconds - float64(whole)) * 10)
	return fmt.Sprintf("%dm%02d.%ds", whole/60, whole%60, tenths)
}
