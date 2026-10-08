package library

import (
	"context"
	"errors"
	"time"

	"family-photo-cloud/internal/media"
	"family-photo-cloud/internal/upload"
)

// TrashRetention is how long Recently Deleted keeps an item.
const TrashRetention = 30 * 24 * time.Hour

var (
	ErrNotFound    = errors.New("not found")
	ErrForbidden   = errors.New("not allowed")
	ErrInvalid     = errors.New("invalid request")
	ErrUnsupported = errors.New("library feature unavailable")
)

// Item is one photo or video as a viewer sees it.
type Item struct {
	ID               string
	OwnerID          string
	OwnerName        string
	StorageKey       string
	OriginalFilename string
	MediaType        string
	ByteSize         int64
	ContentSHA256    [32]byte
	CreatedAt        time.Time
	TakenAt          time.Time
	CapturedAt       *time.Time
	Width            int
	Height           int
	DurationMS       int64
	Favorite         bool
	Hidden           bool
	TrashedAt        *time.Time
	Caption          string
	LivePhotoID      string
	Subtype          string
	Latitude         *float64
	Longitude        *float64
	PlaceCell        string
	PlaceName        string
	Details          map[string]any
	LiveVideo        *Item
	AddedByName      string
	LikeCount        int
	LikedByMe        bool
	CommentCount     int
}

// ListQuery selects what a list shows. Views: library, photos, videos, live,
// selfies, screenshots, panoramas, favorites, recent, hidden, trash, album,
// place, on_this_day, ids.
type ListQuery struct {
	ViewerID  string
	View      string
	AlbumID   string
	PlaceCell string
	IDs       []string
	Sort      string
	Media     string
	Search    string
	Month     int
	Day       int
	TZOffset  int // minutes east of UTC, for "on this day"
	Cursor    []string
	Limit     int
}

// Album is an album as a viewer sees it.
type Album struct {
	ID            string
	OwnerID       string
	OwnerName     string
	FolderID      string
	Name          string
	CoverAssetID  string
	CoverOwnerID  string
	SortOrder     string
	MembersCanAdd bool
	IsOwner       bool
	Shared        bool
	Count         int
	UpdatedAt     time.Time
	LatestAddedAt *time.Time
	Members       []Person
}

type Folder struct {
	ID        string
	ParentID  string
	Name      string
	UpdatedAt time.Time
}

type AlbumPatch struct {
	Name          *string
	FolderID      *string // "" moves to the top level
	CoverAssetID  *string // "" clears the chosen cover
	SortOrder     *string
	MembersCanAdd *bool
}

type FolderPatch struct {
	Name     *string
	ParentID *string // "" moves to the top level
}

type Person struct {
	ID          string
	DisplayName string
	Email       string
	IsMe        bool
}

type Comment struct {
	ID         string
	AlbumID    string
	AssetID    string
	AuthorID   string
	AuthorName string
	Body       string
	CreatedAt  time.Time
	Mine       bool
}

// Activity is one entry of the shared-album feed.
type Activity struct {
	Kind      string // "added" or "comment"
	AlbumID   string
	AlbumName string
	ActorName string
	AssetIDs  []string
	Count     int
	Body      string
	At        time.Time
}

type Place struct {
	Cell         string
	Name         string
	Latitude     float64
	Longitude    float64
	Count        int
	CoverAssetID string
	LatestTaken  time.Time
}

type Usage struct {
	PhotoCount  int64
	PhotoBytes  int64
	VideoCount  int64
	VideoBytes  int64
	TrashCount  int64
	TrashBytes  int64
	HiddenCount int64
	QuotaBytes  *int64
}

// Counts are item counts for the smart albums.
type Counts map[string]int64

// PendingMetadata is an asset whose capture metadata still has to be read.
type PendingMetadata struct {
	ID         string
	OwnerID    string
	StorageKey string
	MediaType  string
}

// Curation actions for UpdateAssets.
const (
	ActionFavorite   = "favorite"
	ActionUnfavorite = "unfavorite"
	ActionHide       = "hide"
	ActionUnhide     = "unhide"
	ActionTrash      = "trash"
	ActionRestore    = "restore"
)

// Store is the library's database.
type Store interface {
	List(ctx context.Context, query ListQuery) ([]Item, []string, error)
	// Accessible returns an item the viewer may open: their own (including
	// hidden and binned ones) or one in an album shared with them.
	Accessible(ctx context.Context, viewerID, assetID string) (Item, error)
	AssetAlbums(ctx context.Context, viewerID, assetID string) ([]Album, error)
	UpdateAssets(ctx context.Context, ownerID string, ids []string, action string) (int, error)
	SetCaption(ctx context.Context, ownerID, assetID, caption string) error
	TrashedIDs(ctx context.Context, ownerID string) ([]string, error)
	Counts(ctx context.Context, ownerID string) (Counts, map[string]string, error)
	Usage(ctx context.Context, ownerID string) (Usage, error)

	Albums(ctx context.Context, viewerID string) ([]Album, []Folder, error)
	Album(ctx context.Context, viewerID, albumID string) (Album, error)
	CreateAlbum(ctx context.Context, ownerID, name, folderID string) (Album, error)
	UpdateAlbum(ctx context.Context, ownerID, albumID string, patch AlbumPatch) error
	DeleteAlbum(ctx context.Context, ownerID, albumID string) error
	AddToAlbum(ctx context.Context, viewerID, albumID string, assetIDs []string) (int, error)
	RemoveFromAlbum(ctx context.Context, viewerID, albumID string, assetIDs []string) (int, error)
	SetMembers(ctx context.Context, ownerID, albumID string, userIDs []string) error
	LeaveAlbum(ctx context.Context, userID, albumID string) error
	SetLike(ctx context.Context, viewerID, albumID, assetID string, liked bool) error
	Comments(ctx context.Context, viewerID, albumID, assetID string) ([]Comment, error)
	AddComment(ctx context.Context, viewerID, albumID, assetID, body string) (Comment, error)
	DeleteComment(ctx context.Context, viewerID, albumID, commentID string) error
	Activity(ctx context.Context, viewerID string, limit int) ([]Activity, error)

	CreateFolder(ctx context.Context, ownerID, name, parentID string) (Folder, error)
	UpdateFolder(ctx context.Context, ownerID, folderID string, patch FolderPatch) error
	DeleteFolder(ctx context.Context, ownerID, folderID string) error

	Places(ctx context.Context, ownerID string) ([]Place, error)
	LabelPlace(ctx context.Context, ownerID, cell, name string) error

	People(ctx context.Context, viewerID string) ([]Person, error)
	SetDisplayName(ctx context.Context, userID, name string) error

	PendingMetadata(ctx context.Context, version int, limit int) ([]PendingMetadata, error)
	SaveMetadata(ctx context.Context, assetID string, version int, meta media.Metadata) error
	MatchAsset(ctx context.Context, ownerID, filename, mediaType string, capturedAt *time.Time) string
}

// legacyStore serves the original read-only asset list over an
// upload.AssetRepository (used by tests without PostgreSQL).
type legacyStore struct {
	Store
	assets upload.AssetRepository
}

func (s legacyStore) List(ctx context.Context, query ListQuery) ([]Item, []string, error) {
	if (query.View != "" && query.View != "library") || (query.Sort != "" && query.Sort != "added_desc") ||
		query.Search != "" || query.Media != "" || len(query.IDs) > 0 {
		return nil, nil, ErrUnsupported
	}
	var cursor *upload.AssetCursor
	if len(query.Cursor) == 2 {
		createdAt, err := time.Parse(time.RFC3339Nano, query.Cursor[0])
		if err != nil {
			return nil, nil, ErrInvalid
		}
		cursor = &upload.AssetCursor{CreatedAt: createdAt, ID: query.Cursor[1]}
	}
	assets, err := s.assets.ListAssets(ctx, query.ViewerID, cursor, query.Limit+1)
	if err != nil {
		return nil, nil, err
	}
	items := make([]Item, 0, len(assets))
	for _, asset := range assets {
		items = append(items, itemFromAsset(asset))
	}
	var next []string
	if len(items) > query.Limit {
		last := items[query.Limit-1]
		next = []string{last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID}
		items = items[:query.Limit]
	}
	return items, next, nil
}

func (s legacyStore) Accessible(ctx context.Context, viewerID, assetID string) (Item, error) {
	asset, err := s.assets.AssetByID(ctx, viewerID, assetID)
	if errors.Is(err, upload.ErrNotFound) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	return itemFromAsset(asset), nil
}

func itemFromAsset(asset upload.Asset) Item {
	return Item{
		ID: asset.ID, OwnerID: asset.OwnerID, StorageKey: asset.StorageKey,
		OriginalFilename: asset.OriginalFilename, MediaType: asset.MediaType,
		ByteSize: asset.ByteSize, ContentSHA256: asset.ContentSHA256,
		CreatedAt: asset.CreatedAt, TakenAt: asset.CreatedAt,
	}
}
