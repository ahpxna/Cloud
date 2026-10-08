//go:build integration

package library_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/database"
	"family-photo-cloud/internal/library"
	"family-photo-cloud/internal/media"
	"family-photo-cloud/internal/upload"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type family struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	repo      *upload.PostgresRepository
	processor *upload.Processor
	store     *library.PostgresStore
	api       *library.API
	mediaRoot string
	sessions  map[string]string
	counter   int
}

func startFamily(t *testing.T) *family {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	root := t.TempDir()
	configuration := embeddedpostgres.DefaultConfig().Port(port).Database("photo_cloud").Username("photo_cloud").
		Password("test-only-password").RuntimePath(filepath.Join(root, "runtime")).DataPath(filepath.Join(root, "data")).
		CachePath(filepath.Join(root, "cache")).Logger(io.Discard)
	postgres := embeddedpostgres.NewDatabase(configuration)
	if err := postgres.Start(); err != nil {
		t.Fatalf("start embedded PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = postgres.Stop() })
	ctx := context.Background()
	_, file, _, _ := runtime.Caller(0)
	migrations, err := database.LoadMigrations(filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pgx.Connect(ctx, configuration.GetConnectionURL())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Apply(ctx, connection, migrations, 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	connection.Close(ctx)
	pool, err := pgxpool.New(ctx, configuration.GetConnectionURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo := upload.NewPostgresRepository(pool)
	mediaRoot := filepath.Join(root, "media")
	processor, err := upload.NewProcessor(repo, mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := library.NewPostgresStore(pool)
	api, err := library.NewAPI(repo, mediaRoot)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewAccessTokenManager([]byte(strings.Repeat("k", 32)), auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		t.Fatal(err)
	}
	api.EnableViewTickets(tokens)
	f := &family{t: t, ctx: ctx, pool: pool, repo: repo, processor: processor, store: store, api: api, mediaRoot: mediaRoot, sessions: map[string]string{}}
	api.UseStore(store, purger{f})
	return f
}

type purger struct{ f *family }

func (p purger) Purge(ctx context.Context, ownerID, assetID string) error {
	return p.f.processor.Purge(ctx, p.f.repo, ownerID, assetID, nil, "owner")
}

func (f *family) user(email string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO users (email, password_hash, state) VALUES ($1, 'x', 'active') RETURNING id::text`, email).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	f.sessions[id] = fmt.Sprintf("90000000-0000-4000-8000-%012d", len(f.sessions)+1)
	return id
}

// upload sends bytes through the real verified-commit pipeline.
func (f *family) upload(owner, name, mediaType string, content []byte) string {
	f.t.Helper()
	f.counter++
	hash := sha256.Sum256(content)
	session, _, err := f.repo.CreateSession(f.ctx, upload.CreateSessionInput{
		OwnerID: owner, ClientAssetID: fmt.Sprintf("client-%d", f.counter), OriginalFilename: name,
		MediaType: mediaType, ExpectedSize: int64(len(content)), ClientSHA256: hash,
		ExpiresAt: time.Now().Add(time.Hour), AvailableBytes: 1 << 40, MinimumFreeBytes: 1,
	})
	if err != nil {
		f.t.Fatalf("create session: %v", err)
	}
	return f.finish(session, content)
}

func (f *family) finish(session upload.Session, content []byte) string {
	f.t.Helper()
	if err := f.repo.ClaimTusCreation(f.ctx, session.ID, session.OwnerID, int64(len(content))); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.processor.StagingDirectory(), session.ID), content, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := f.repo.MarkReceived(f.ctx, session.ID, int64(len(content))); err != nil {
		f.t.Fatal(err)
	}
	if err := f.processor.Process(f.ctx, session.ID); err != nil {
		f.t.Fatalf("process: %v", err)
	}
	loaded, err := f.repo.SessionByID(f.ctx, session.ID)
	if err != nil || loaded.AssetID == "" {
		f.t.Fatalf("session after commit = %+v %v", loaded, err)
	}
	return loaded.AssetID
}

func (f *family) meta(assetID string, meta media.Metadata) {
	f.t.Helper()
	if err := f.store.SaveMetadata(f.ctx, assetID, library.MetadataVersion, meta); err != nil {
		f.t.Fatal(err)
	}
}

func at(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return &parsed
}

func float(value float64) *float64 { return &value }

func (f *family) call(user, method, target string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{UserID: user, SessionID: f.sessions[user]}))
	recorder := httptest.NewRecorder()
	f.api.ServeHTTP(recorder, request)
	return recorder
}

func (f *family) json(user, method, target string, body any, want int) map[string]any {
	f.t.Helper()
	response := f.call(user, method, target, body)
	if response.Code != want {
		f.t.Fatalf("%s %s = %d, want %d: %s", method, target, response.Code, want, response.Body)
	}
	decoded := map[string]any{}
	if response.Body.Len() > 0 {
		_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	}
	return decoded
}

func (f *family) ids(user, target string) []string {
	f.t.Helper()
	page := f.json(user, http.MethodGet, target, nil, http.StatusOK)
	var ids []string
	for _, raw := range page["assets"].([]any) {
		ids = append(ids, raw.(map[string]any)["id"].(string))
	}
	return ids
}

func equal(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func TestLibraryCurationTimelineAndRecentlyDeleted(t *testing.T) {
	f := startFamily(t)
	mom := f.user("mom@example.com")
	dad := f.user("dad@example.com")

	beach := f.upload(mom, "IMG_0001.JPG", "image/jpeg", []byte("beach photo, the largest file of all"))
	f.meta(beach, media.Metadata{CapturedAt: at("2024-07-01T10:00:00Z"), Width: 4032, Height: 3024, Latitude: float(10.7769), Longitude: float(106.7009), Make: "Apple", Model: "iPhone 14 Pro"})
	birthday := f.upload(mom, "IMG_0002.HEIC", "image/heic", []byte("birthday still"))
	f.meta(birthday, media.Metadata{CapturedAt: at("2025-03-05T08:00:00Z"), LivePhotoID: "LIVE-1"})
	birthdayMotion := f.upload(mom, "IMG_0002.MOV", "video/quicktime", []byte("birthday motion"))
	f.meta(birthdayMotion, media.Metadata{CapturedAt: at("2025-03-05T08:00:00Z"), LivePhotoID: "LIVE-1", DurationMS: 2500})
	video := f.upload(mom, "IMG_0003.MOV", "video/quicktime", []byte("a standalone video"))
	f.meta(video, media.Metadata{CapturedAt: at("2023-12-24T20:00:00Z"), DurationMS: 15000})
	screenshot := f.upload(mom, "IMG_0004.PNG", "image/png", []byte("shot"))
	f.meta(screenshot, media.Metadata{Subtype: "screenshot"}) // no capture date: uploaded time
	dadPhoto := f.upload(dad, "IMG_9000.JPG", "image/jpeg", []byte("dad's private photo"))

	// The timeline is ordered by date taken; the Live Photo is one item.
	equal(t, "library", f.ids(mom, "/v1/assets"), []string{screenshot, birthday, beach, video})
	page := f.json(mom, http.MethodGet, "/v1/assets?tickets=1", nil, http.StatusOK)
	live := page["assets"].([]any)[1].(map[string]any)["live_video"].(map[string]any)
	if live["id"] != birthdayMotion || !strings.Contains(live["view_url"].(string), "ticket=") {
		t.Fatalf("live video = %v", live)
	}
	equal(t, "oldest first", f.ids(mom, "/v1/assets?sort=taken_asc"), []string{video, beach, birthday, screenshot})
	equal(t, "largest first", f.ids(mom, "/v1/assets?sort=size_desc")[:1], []string{beach})
	equal(t, "uploaded order", f.ids(mom, "/v1/assets?sort=added_desc"), []string{screenshot, video, birthday, beach})
	equal(t, "name order", f.ids(mom, "/v1/assets?sort=name_asc"), []string{beach, birthday, video, screenshot})
	equal(t, "videos", f.ids(mom, "/v1/assets?view=videos"), []string{video})
	equal(t, "live", f.ids(mom, "/v1/assets?view=live"), []string{birthday})
	equal(t, "screenshots", f.ids(mom, "/v1/assets?view=screenshots"), []string{screenshot})
	equal(t, "search", f.ids(mom, "/v1/assets?q=iphone%2014"), []string{beach})
	equal(t, "search by date", f.ids(mom, "/v1/assets?q=2023-12"), []string{video})
	equal(t, "on this day", f.ids(mom, "/v1/assets?view=on_this_day&month=7&day=1&tzoffset=420"), []string{beach})
	if got := f.ids(dad, "/v1/assets"); len(got) != 1 || got[0] != dadPhoto {
		t.Fatalf("dad sees %v", got)
	}

	// Cursor pagination walks every item exactly once.
	var walked []string
	cursor := ""
	for range 10 {
		target := "/v1/assets?limit=1"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		page := f.json(mom, http.MethodGet, target, nil, http.StatusOK)
		for _, raw := range page["assets"].([]any) {
			walked = append(walked, raw.(map[string]any)["id"].(string))
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	equal(t, "paged", walked, []string{screenshot, birthday, beach, video})
	if response := f.call(mom, http.MethodGet, "/v1/assets?sort=size_desc&cursor="+cursor, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("cursor reused with another sort = %d", response.Code)
	}

	// Favourite and hide the Live Photo: both halves change together.
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{birthday}, "action": "favorite"}, http.StatusOK)
	equal(t, "favorites", f.ids(mom, "/v1/assets?view=favorites"), []string{birthday})
	equal(t, "favorites first", f.ids(mom, "/v1/assets?sort=favorites_first")[:1], []string{birthday})
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{birthday}, "action": "hide"}, http.StatusOK)
	equal(t, "hidden", f.ids(mom, "/v1/assets?view=hidden"), []string{birthday})
	equal(t, "library without hidden", f.ids(mom, "/v1/assets"), []string{screenshot, beach, video})
	f.json(mom, http.MethodPatch, "/v1/assets/"+birthday, map[string]any{"hidden": false, "caption": "Sinh nhật bé"}, http.StatusNoContent)
	equal(t, "search caption", f.ids(mom, "/v1/assets?q=sinh%20nh%E1%BA%ADt"), []string{birthday})

	// Somebody else cannot change mom's photos.
	if changed := f.json(dad, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{beach}, "action": "trash"}, http.StatusOK)["changed"]; changed != float64(0) {
		t.Fatalf("dad trashed mom's photo: %v", changed)
	}

	// Recently Deleted keeps items for 30 days and lists purge dates.
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{beach, birthday}, "action": "trash"}, http.StatusOK)
	trash := f.json(mom, http.MethodGet, "/v1/assets?view=trash", nil, http.StatusOK)["assets"].([]any)
	if len(trash) != 2 || trash[0].(map[string]any)["purge_at"] == nil {
		t.Fatalf("trash = %v", trash)
	}
	summary := f.json(mom, http.MethodGet, "/v1/library/summary", nil, http.StatusOK)
	counts := summary["counts"].(map[string]any)
	if counts["trash"] != float64(2) || counts["library"] != float64(2) || counts["favorites"] != float64(0) {
		t.Fatalf("counts = %v", counts)
	}
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{birthday}, "action": "restore"}, http.StatusOK)
	equal(t, "restored", f.ids(mom, "/v1/assets"), []string{screenshot, birthday, video})

	// Permanent deletion removes the bytes and the preview.
	beachItem, err := f.store.Accessible(f.ctx, mom, beach)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(f.mediaRoot, filepath.FromSlash(beachItem.StorageKey))
	thumbnail := filepath.Join(f.mediaRoot, "thumbnails", mom, beach+".jpg")
	_ = os.MkdirAll(filepath.Dir(thumbnail), 0o700)
	_ = os.WriteFile(thumbnail, []byte("preview"), 0o600)
	if purged := f.json(mom, http.MethodPost, "/v1/assets/trash/empty", map[string]any{}, http.StatusOK)["changed"]; purged != float64(1) {
		t.Fatalf("purged = %v", purged)
	}
	for _, path := range []string{original, thumbnail} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", path, err)
		}
	}
	if response := f.call(mom, http.MethodGet, "/v1/assets/"+beach+"/original", nil); response.Code != http.StatusNotFound {
		t.Fatalf("purged original = %d", response.Code)
	}
	var events int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM asset_events WHERE asset_id = $1 AND event_type = 'purged'`, beach).Scan(&events)
	if events != 1 {
		t.Fatalf("purge events = %d", events)
	}
	// The same photo can be uploaded again after it was purged.
	again := f.upload(mom, "IMG_0001.JPG", "image/jpeg", []byte("beach photo, the largest file of all"))
	if again == beach {
		t.Fatal("purged asset was resurrected instead of re-created")
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("re-uploaded original missing: %v", err)
	}
	// Uploading a binned photo again brings it back.
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{video}, "action": "trash"}, http.StatusOK)
	resent := f.upload(mom, "copy-of-video.MOV", "video/quicktime", []byte("a standalone video"))
	if resent != video {
		t.Fatalf("duplicate upload created %s instead of restoring %s", resent, video)
	}
	if got := f.ids(mom, "/v1/assets?view=trash"); len(got) != 0 {
		t.Fatalf("trash after re-upload = %v", got)
	}

	// Retention purges only items binned more than 30 days ago.
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{screenshot}, "action": "trash"}, http.StatusOK)
	if _, err := f.pool.Exec(f.ctx, `UPDATE assets SET trashed_at = now() - interval '31 days' WHERE id = $1`, screenshot); err != nil {
		t.Fatal(err)
	}
	purged, err := f.processor.PurgeExpiredTrash(f.ctx, f.repo, time.Now().Add(-library.TrashRetention), 10)
	if err != nil || purged != 1 {
		t.Fatalf("retention purged %d: %v", purged, err)
	}

	usage := f.json(mom, http.MethodGet, "/v1/library/summary", nil, http.StatusOK)["usage"].(map[string]any)
	if usage["video_count"] != float64(2) || usage["photo_count"] != float64(2) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestAlbumsFoldersSharingAndDownloads(t *testing.T) {
	f := startFamily(t)
	mom := f.user("mom@example.com")
	dad := f.user("dad@example.com")
	kid := f.user("kid@example.com")
	if err := f.store.SetDisplayName(f.ctx, mom, "Mẹ"); err != nil {
		t.Fatal(err)
	}
	first := f.upload(mom, "IMG_0001.JPG", "image/jpeg", []byte("first"))
	f.meta(first, media.Metadata{CapturedAt: at("2026-01-01T00:00:00Z"), Latitude: float(21.0285), Longitude: float(105.8542)})
	second := f.upload(mom, "IMG_0002.HEIC", "image/heic", []byte("second"))
	f.meta(second, media.Metadata{CapturedAt: at("2026-02-01T00:00:00Z"), LivePhotoID: "LIVE-2", Latitude: float(21.0290), Longitude: float(105.8550)})
	secondMotion := f.upload(mom, "IMG_0002.MOV", "video/quicktime", []byte("second motion"))
	f.meta(secondMotion, media.Metadata{LivePhotoID: "LIVE-2"})
	private := f.upload(mom, "IMG_0003.JPG", "image/jpeg", []byte("not shared"))
	dadPhoto := f.upload(dad, "IMG_5000.JPG", "image/jpeg", []byte("dad's contribution"))

	// Folders nest and cannot contain themselves.
	trips := f.json(mom, http.MethodPost, "/v1/album-folders", map[string]any{"name": "Du lịch"}, http.StatusCreated)["id"].(string)
	asia := f.json(mom, http.MethodPost, "/v1/album-folders", map[string]any{"name": "Châu Á", "parent_id": trips}, http.StatusCreated)["id"].(string)
	f.json(mom, http.MethodPatch, "/v1/album-folders/"+trips, map[string]any{"parent_id": asia}, http.StatusBadRequest)

	album := f.json(mom, http.MethodPost, "/v1/albums", map[string]any{"name": "Tết 2026", "folder_id": asia, "asset_ids": []string{first, second}}, http.StatusCreated)
	albumID := album["id"].(string)
	if album["count"] != float64(2) || album["folder_id"] != asia {
		t.Fatalf("album = %v", album)
	}
	equal(t, "album newest first", f.ids(mom, "/v1/assets?album="+albumID), []string{second, first})
	f.json(mom, http.MethodPatch, "/v1/albums/"+albumID, map[string]any{"sort_order": "oldest_first", "cover_asset_id": first}, http.StatusNoContent)
	equal(t, "album oldest first", f.ids(mom, "/v1/assets?album="+albumID), []string{first, second})
	if cover := f.json(mom, http.MethodGet, "/v1/albums/"+albumID, nil, http.StatusOK)["cover_asset_id"]; cover != first {
		t.Fatalf("cover = %v", cover)
	}

	// Deleting a folder moves its albums and folders up; nothing is lost.
	f.json(mom, http.MethodDelete, "/v1/album-folders/"+asia, nil, http.StatusNoContent)
	listing := f.json(mom, http.MethodGet, "/v1/albums", nil, http.StatusOK)
	if got := listing["albums"].([]any)[0].(map[string]any)["folder_id"]; got != trips {
		t.Fatalf("album folder after delete = %v", got)
	}

	// Sharing: dad can see and add, kid cannot see anything.
	f.json(mom, http.MethodPut, "/v1/albums/"+albumID+"/members", map[string]any{"user_ids": []string{dad}}, http.StatusNoContent)
	dadAlbums := f.json(dad, http.MethodGet, "/v1/albums", nil, http.StatusOK)["albums"].([]any)
	if len(dadAlbums) != 1 || dadAlbums[0].(map[string]any)["owner_name"] != "Mẹ" || dadAlbums[0].(map[string]any)["is_owner"] != false {
		t.Fatalf("dad's albums = %v", dadAlbums)
	}
	equal(t, "dad sees the shared album", f.ids(dad, "/v1/assets?album="+albumID), []string{first, second})
	if response := f.call(dad, http.MethodGet, "/v1/assets/"+first+"/original", nil); response.Code != http.StatusOK || response.Body.String() != "first" {
		t.Fatalf("dad opening a shared original = %d", response.Code)
	}
	if response := f.call(dad, http.MethodGet, "/v1/assets/"+private+"/original", nil); response.Code != http.StatusNotFound {
		t.Fatalf("dad opening an unshared original = %d", response.Code)
	}
	if response := f.call(kid, http.MethodGet, "/v1/assets?album="+albumID, nil); response.Code != http.StatusNotFound {
		t.Fatalf("kid listing the album = %d", response.Code)
	}
	if response := f.call(kid, http.MethodGet, "/v1/assets/"+first+"/original", nil); response.Code != http.StatusNotFound {
		t.Fatalf("kid opening a shared original = %d", response.Code)
	}
	f.json(dad, http.MethodPatch, "/v1/albums/"+albumID, map[string]any{"name": "taken over"}, http.StatusForbidden)
	f.json(dad, http.MethodDelete, "/v1/albums/"+albumID, nil, http.StatusForbidden)
	// Dad cannot add mom's private photo, only his own.
	if changed := f.json(dad, http.MethodPost, "/v1/albums/"+albumID+"/assets", map[string]any{"ids": []string{private}}, http.StatusOK)["changed"]; changed != float64(0) {
		t.Fatalf("dad added mom's photo: %v", changed)
	}
	f.json(dad, http.MethodPost, "/v1/albums/"+albumID+"/assets", map[string]any{"ids": []string{dadPhoto}}, http.StatusOK)
	items := f.json(mom, http.MethodGet, "/v1/assets?album="+albumID, nil, http.StatusOK)["assets"].([]any)
	if len(items) != 3 || items[2].(map[string]any)["owner_id"] != dad || items[2].(map[string]any)["mine"] != false {
		t.Fatalf("mom's view of the album = %v", items)
	}

	// Likes, comments and the activity feed.
	f.json(dad, http.MethodPost, "/v1/albums/"+albumID+"/likes", map[string]any{"asset_id": first, "liked": true}, http.StatusNoContent)
	comment := f.json(dad, http.MethodPost, "/v1/albums/"+albumID+"/comments", map[string]any{"asset_id": first, "body": "Đẹp quá!"}, http.StatusCreated)
	momItems := f.json(mom, http.MethodGet, "/v1/assets?album="+albumID, nil, http.StatusOK)["assets"].([]any)
	if momItems[0].(map[string]any)["like_count"] != float64(1) || momItems[0].(map[string]any)["comment_count"] != float64(1) {
		t.Fatalf("social counts = %v", momItems[0])
	}
	feed := f.json(mom, http.MethodGet, "/v1/activity", nil, http.StatusOK)["activity"].([]any)
	kinds := []string{}
	for _, entry := range feed {
		kinds = append(kinds, entry.(map[string]any)["kind"].(string))
	}
	sort.Strings(kinds)
	equal(t, "activity", kinds, []string{"added", "comment"})
	f.json(kid, http.MethodPost, "/v1/albums/"+albumID+"/comments", map[string]any{"asset_id": first, "body": "hi"}, http.StatusNotFound)
	// The owner may delete any comment in their album.
	f.json(mom, http.MethodDelete, "/v1/albums/"+albumID+"/comments/"+comment["id"].(string), nil, http.StatusNoContent)

	// The owner can stop members adding photos.
	f.json(mom, http.MethodPatch, "/v1/albums/"+albumID, map[string]any{"members_can_add": false}, http.StatusNoContent)
	f.json(dad, http.MethodPost, "/v1/albums/"+albumID+"/assets", map[string]any{"ids": []string{dadPhoto}}, http.StatusForbidden)

	// Downloads: a ZIP with the original bytes, Live Photo video included.
	download := f.json(mom, http.MethodPost, "/v1/assets/download", map[string]any{"ids": []string{first, second}}, http.StatusCreated)
	if download["count"] != float64(3) {
		t.Fatalf("download = %v", download)
	}
	request := httptest.NewRequest(http.MethodGet, download["url"].(string), nil)
	setID := strings.TrimPrefix(strings.SplitN(download["url"].(string), "?", 2)[0], "/v1/assets/download/")
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{UserID: mom, SessionID: f.sessions[mom]}))
	recorder := httptest.NewRecorder()
	f.api.ServeHTTP(recorder, request)
	archive, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	if err != nil {
		t.Fatalf("zip: %v (%d %s)", err, recorder.Code, setID)
	}
	var names []string
	for _, file := range archive.File {
		names = append(names, file.Name)
		if file.Method != zip.Store {
			t.Fatalf("%s was compressed", file.Name)
		}
	}
	sort.Strings(names)
	equal(t, "zip entries", names, []string{"IMG_0001.JPG", "IMG_0002.HEIC", "IMG_0002.MOV"})
	// Another person cannot use mom's download.
	other := httptest.NewRequest(http.MethodGet, download["url"].(string), nil)
	other = other.WithContext(auth.WithPrincipal(other.Context(), auth.Principal{UserID: dad, SessionID: f.sessions[dad]}))
	otherRecorder := httptest.NewRecorder()
	f.api.ServeHTTP(otherRecorder, other)
	if otherRecorder.Code != http.StatusNotFound {
		t.Fatalf("dad using mom's download = %d", otherRecorder.Code)
	}

	// Removing dad from the album removes his photos from it too.
	f.json(mom, http.MethodPut, "/v1/albums/"+albumID+"/members", map[string]any{"user_ids": []string{}}, http.StatusNoContent)
	equal(t, "after unsharing", f.ids(mom, "/v1/assets?album="+albumID), []string{first, second})
	if response := f.call(dad, http.MethodGet, "/v1/assets/"+first+"/original", nil); response.Code != http.StatusNotFound {
		t.Fatalf("dad after unsharing = %d", response.Code)
	}

	// Places group nearby photos; people can name them.
	places := f.json(mom, http.MethodGet, "/v1/places", nil, http.StatusOK)["places"].([]any)
	if len(places) != 1 || places[0].(map[string]any)["count"] != float64(2) {
		t.Fatalf("places = %v", places)
	}
	cell := places[0].(map[string]any)["cell"].(string)
	f.json(mom, http.MethodPut, "/v1/places/"+cell, map[string]any{"name": "Nhà bà nội"}, http.StatusNoContent)
	equal(t, "place view", f.ids(mom, "/v1/assets?place="+cell), []string{second, first})
	equal(t, "search place name", f.ids(mom, "/v1/assets?q=b%C3%A0%20n%E1%BB%99i"), []string{second, first})

	// Deleting an album keeps its photos; purging a photo leaves its albums.
	other2 := f.json(mom, http.MethodPost, "/v1/albums", map[string]any{"name": "Khác", "asset_ids": []string{private}}, http.StatusCreated)["id"].(string)
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{private}, "action": "trash"}, http.StatusOK)
	if count := f.json(mom, http.MethodGet, "/v1/albums/"+other2, nil, http.StatusOK)["count"]; count != float64(0) {
		t.Fatalf("binned photo still counted in album: %v", count)
	}
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{private}, "action": "purge"}, http.StatusOK)
	var memberships int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM album_assets WHERE asset_id = $1`, private).Scan(&memberships)
	if memberships != 0 {
		t.Fatalf("purged photo still in %d albums", memberships)
	}
	f.json(mom, http.MethodDelete, "/v1/albums/"+albumID, nil, http.StatusNoContent)
	if got := f.ids(mom, "/v1/assets"); len(got) != 2 {
		t.Fatalf("library after deleting the album = %v", got)
	}

	// People directory and the duplicate hint for stuck uploads.
	people := f.json(kid, http.MethodGet, "/v1/people", nil, http.StatusOK)["people"].([]any)
	if len(people) != 3 || people[0].(map[string]any)["me"] != true {
		t.Fatalf("people = %v", people)
	}
	if match := f.store.MatchAsset(f.ctx, mom, "whatever.HEIC", "image/heic", at("2026-02-01T00:00:00Z")); match != second {
		t.Fatalf("match = %q", match)
	}
}

func TestMetadataPendingAndPurgeSweep(t *testing.T) {
	f := startFamily(t)
	mom := f.user("mom@example.com")
	photo := f.upload(mom, "IMG_0001.JPG", "image/jpeg", []byte("not really a JPEG"))
	pending, err := f.store.PendingMetadata(f.ctx, library.MetadataVersion, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != photo {
		t.Fatalf("pending = %v %v", pending, err)
	}
	maintenance := &library.Maintenance{Store: f.store, MediaRoot: f.mediaRoot, Logger: testLogger()}
	if done, err := maintenance.ProcessPending(f.ctx, 10); err != nil || done != 1 {
		t.Fatalf("processed %d: %v", done, err)
	}
	if pending, _ := f.store.PendingMetadata(f.ctx, library.MetadataVersion, 10); len(pending) != 0 {
		t.Fatalf("still pending: %v", pending)
	}

	// A purge interrupted after its commit leaves bytes in .purging/; the
	// sweep deletes them. One whose commit never happened is put back.
	item, _ := f.store.Accessible(f.ctx, mom, photo)
	original := filepath.Join(f.mediaRoot, filepath.FromSlash(item.StorageKey))
	holding := filepath.Join(f.mediaRoot, ".purging", photo)
	if err := os.Rename(original, holding); err != nil {
		t.Fatal(err)
	}
	if err := f.processor.SweepPurging(f.ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("live asset bytes not restored: %v", err)
	}
	f.json(mom, http.MethodPost, "/v1/assets/batch", map[string]any{"ids": []string{photo}, "action": "trash"}, http.StatusOK)
	if _, err := f.pool.Exec(f.ctx, `UPDATE assets SET deleted_at = now() WHERE id = $1`, photo); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, holding); err != nil {
		t.Fatal(err)
	}
	if err := f.processor.SweepPurging(f.ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(holding); !os.IsNotExist(err) {
		t.Fatalf("purged bytes left behind: %v", err)
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
