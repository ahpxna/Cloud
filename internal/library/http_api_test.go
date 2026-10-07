package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/upload"
)

const (
	ownerA = "10000000-0000-4000-8000-000000000001"
	ownerB = "20000000-0000-4000-8000-000000000002"
)

type fakeAssets struct {
	assets []upload.Asset
}

func (f *fakeAssets) ListAssets(_ context.Context, owner string, before *upload.AssetCursor, limit int) ([]upload.Asset, error) {
	owned := make([]upload.Asset, 0)
	for _, asset := range f.assets {
		if asset.OwnerID != owner {
			continue
		}
		if before != nil && !(asset.CreatedAt.Before(before.CreatedAt) ||
			(asset.CreatedAt.Equal(before.CreatedAt) && asset.ID < before.ID)) {
			continue
		}
		owned = append(owned, asset)
	}
	sort.Slice(owned, func(i, j int) bool {
		if owned[i].CreatedAt.Equal(owned[j].CreatedAt) {
			return owned[i].ID > owned[j].ID
		}
		return owned[i].CreatedAt.After(owned[j].CreatedAt)
	})
	if len(owned) > limit {
		owned = owned[:limit]
	}
	return owned, nil
}

func (f *fakeAssets) AssetByID(_ context.Context, owner, id string) (upload.Asset, error) {
	for _, asset := range f.assets {
		if asset.ID == id && asset.OwnerID == owner {
			return asset, nil
		}
	}
	return upload.Asset{}, upload.ErrNotFound
}

type libraryFixture struct {
	t      *testing.T
	api    *API
	root   string
	assets *fakeAssets
}

func newLibraryFixture(t *testing.T) *libraryFixture {
	t.Helper()
	root := t.TempDir()
	assets := &fakeAssets{}
	api, err := NewAPI(assets, root)
	if err != nil {
		t.Fatal(err)
	}
	return &libraryFixture{t: t, api: api, root: root, assets: assets}
}

func (f *libraryFixture) addAsset(id, owner string, content []byte, created time.Time) upload.Asset {
	f.t.Helper()
	hash := sha256.Sum256(content)
	key := filepath.ToSlash(filepath.Join("originals", owner, hex.EncodeToString(hash[:1]), hex.EncodeToString(hash[:])))
	path := filepath.Join(f.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		f.t.Fatal(err)
	}
	asset := upload.Asset{
		ID: id, OwnerID: owner, StorageKey: key, OriginalFilename: id + ".jpg",
		MediaType: "image/jpeg", ByteSize: int64(len(content)), ContentSHA256: hash, CreatedAt: created,
	}
	f.assets.assets = append(f.assets.assets, asset)
	return asset
}

func (f *libraryFixture) serve(method, target, owner string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	request := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if owner != "" {
		request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{
			UserID: owner, SessionID: "90000000-0000-4000-8000-000000000009",
		}))
	}
	recorder := httptest.NewRecorder()
	f.api.ServeHTTP(recorder, request)
	return recorder
}

func TestListIsOwnerScopedAndPaginates(t *testing.T) {
	fixture := newLibraryFixture(t)
	base := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	for index, id := range []string{"a1", "a2", "a3"} {
		fixture.addAsset(id, ownerA, []byte("photo-"+id), base.Add(time.Duration(index)*time.Minute))
	}
	fixture.addAsset("b1", ownerB, []byte("other family member"), base.Add(time.Hour))

	type page struct {
		Assets []struct {
			ID          string `json:"id"`
			OriginalURL string `json:"original_url"`
		} `json:"assets"`
		NextCursor string `json:"next_cursor"`
	}
	var seen []string
	cursor := ""
	for range 3 {
		target := "/v1/assets?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		response := fixture.serve(http.MethodGet, target, ownerA, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("list status = %d: %s", response.Code, response.Body)
		}
		var decoded page
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		for _, asset := range decoded.Assets {
			seen = append(seen, asset.ID)
			if asset.OriginalURL != "/v1/assets/"+asset.ID+"/original" {
				t.Fatalf("unexpected original URL %q", asset.OriginalURL)
			}
		}
		cursor = decoded.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 || seen[0] != "a3" || seen[1] != "a2" || seen[2] != "a1" {
		t.Fatalf("paginated IDs = %v, want newest-first a3,a2,a1 without other owners", seen)
	}

	for _, target := range []string{"/v1/assets?limit=0", "/v1/assets?limit=101", "/v1/assets?cursor=not-base64!"} {
		if response := fixture.serve(http.MethodGet, target, ownerA, nil); response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", target, response.Code)
		}
	}
	if response := fixture.serve(http.MethodGet, "/v1/assets", "", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d", response.Code)
	}
	if response := fixture.serve(http.MethodPost, "/v1/assets", ownerA, nil); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", response.Code)
	}
}

func TestOriginalEnforcesOwnershipIntegrityAndRanges(t *testing.T) {
	fixture := newLibraryFixture(t)
	content := []byte("verified original bytes")
	asset := fixture.addAsset("a1", ownerA, content, time.Now())

	response := fixture.serve(http.MethodGet, "/v1/assets/a1/original", ownerA, nil)
	if response.Code != http.StatusOK || response.Body.String() != string(content) {
		t.Fatalf("owner download = %d %q", response.Code, response.Body)
	}
	wantTag := `"sha256-` + hex.EncodeToString(asset.ContentSHA256[:]) + `"`
	if response.Header().Get("ETag") != wantTag || response.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("headers = %v", response.Header())
	}

	ranged := fixture.serve(http.MethodGet, "/v1/assets/a1/original", ownerA, map[string]string{
		"Range": "bytes=9-16", "If-Range": wantTag,
	})
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != string(content[9:17]) {
		t.Fatalf("range = %d %q", ranged.Code, ranged.Body)
	}
	staleRange := fixture.serve(http.MethodGet, "/v1/assets/a1/original", ownerA, map[string]string{
		"Range": "bytes=9-16", "If-Range": `"sha256-other"`,
	})
	if staleRange.Code != http.StatusOK || staleRange.Body.String() != string(content) {
		t.Fatalf("mismatched If-Range must return the full original, got %d", staleRange.Code)
	}

	if response := fixture.serve(http.MethodGet, "/v1/assets/a1/original", ownerB, nil); response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner download status = %d, want 404", response.Code)
	}
	for _, target := range []string{"/v1/assets//original", "/v1/assets/a/b/original"} {
		if response := fixture.serve(http.MethodGet, target, ownerA, nil); response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, response.Code)
		}
	}

	fixture.assets.assets[0].ByteSize++
	if response := fixture.serve(http.MethodGet, "/v1/assets/a1/original", ownerA, nil); response.Code != http.StatusConflict {
		t.Fatalf("size mismatch status = %d, want 409", response.Code)
	}
	fixture.assets.assets[0].ByteSize--

	for _, key := range []string{"../outside", "/etc/passwd", "originals/../../outside", ""} {
		fixture.assets.assets[0].StorageKey = key
		if response := fixture.serve(http.MethodGet, "/v1/assets/a1/original", ownerA, nil); response.Code != http.StatusInternalServerError {
			t.Fatalf("storage key %q status = %d, want 500", key, response.Code)
		}
	}
	fixture.assets.assets[0].StorageKey = "originals/missing"
	if response := fixture.serve(http.MethodGet, "/v1/assets/a1/original", ownerA, nil); response.Code != http.StatusGone {
		t.Fatalf("missing bytes status = %d, want 410", response.Code)
	}
}

// A slow phone downloading a large video must not be cut off by the server's
// absolute write timeout while bytes are still flowing.
func TestOriginalDownloadOutlivesServerWriteTimeoutWhileProgressing(t *testing.T) {
	fixture := newLibraryFixture(t)
	content := make([]byte, 16<<20)
	for index := range content {
		content[index] = byte(index)
	}
	fixture.addAsset("video", ownerA, content, time.Now())
	fixture.api.writeIdleTimeout = 500 * time.Millisecond

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: ownerA, SessionID: "s"}))
		fixture.api.ServeHTTP(w, r)
	}))
	server.Config.WriteTimeout = 300 * time.Millisecond
	server.Start()
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/v1/assets/video/original")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	received := 0
	buffer := make([]byte, 256<<10)
	for {
		count, err := response.Body.Read(buffer)
		received += count
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("download failed after %d of %d bytes: %v", received, len(content), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if received != len(content) {
		t.Fatalf("received %d of %d bytes", received, len(content))
	}
}
