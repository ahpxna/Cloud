package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"family-photo-cloud/internal/media"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore implements Store. Every query is scoped to the viewer: their
// own items, or items in albums they own or that were shared with them.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

var placeCellPattern = regexp.MustCompile(`^-?[0-9]{1,3}\.[0-9]{2},-?[0-9]{1,3}\.[0-9]{2}$`)

// displayName falls back to the part of the email before "@".
const displayNameSQL = `COALESCE(NULLIF(%[1]s.display_name, ''), split_part(%[1]s.email, '@', 1))`

func nameOf(alias string) string { return fmt.Sprintf(displayNameSQL, alias) }

// pairedVideo is true for the motion half of a Live Photo whose still is in
// the same state, so lists show one item per Live Photo.
func pairedVideo(alias string) string {
	return `(` + alias + `.media_type LIKE 'video/%' AND ` + alias + `.live_photo_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM assets still
        WHERE still.owner_id = ` + alias + `.owner_id AND still.live_photo_id = ` + alias + `.live_photo_id
          AND still.media_type NOT LIKE 'video/%' AND still.deleted_at IS NULL
          AND (still.trashed_at IS NULL) = (` + alias + `.trashed_at IS NULL) AND still.hidden = ` + alias + `.hidden))`
}

// visible is the normal library state: not binned, hidden or purged.
func visible(alias string) string {
	return `(` + alias + `.deleted_at IS NULL AND ` + alias + `.trashed_at IS NULL AND NOT ` + alias + `.hidden)`
}

// canSeeAlbum is true when the viewer owns the album or it was shared with them.
func canSeeAlbum(albumAlias, viewerArg string) string {
	return `(` + albumAlias + `.owner_id = ` + viewerArg + ` OR EXISTS (
        SELECT 1 FROM album_members member WHERE member.album_id = ` + albumAlias + `.id AND member.user_id = ` + viewerArg + `))`
}

const itemColumns = `
    a.id::text, a.owner_id::text, ` + `COALESCE(NULLIF(owner.display_name, ''), split_part(owner.email, '@', 1))` + `,
    a.storage_key, a.original_filename, a.media_type, a.byte_size, a.content_sha256,
    a.created_at, a.taken_at, a.captured_at, COALESCE(a.width, 0), COALESCE(a.height, 0),
    COALESCE(a.duration_ms, 0), a.favorite, a.hidden, a.trashed_at, COALESCE(a.caption, ''),
    COALESCE(a.live_photo_id, ''), COALESCE(a.subtype, ''), a.latitude, a.longitude,
    COALESCE(a.place_cell, ''), COALESCE(label.name, ''), a.metadata,
    live.id::text, live.original_filename, live.media_type, live.byte_size, live.duration_ms`

const itemJoins = `
    JOIN users AS owner ON owner.id = a.owner_id
    LEFT JOIN place_labels AS label ON label.owner_id = a.owner_id AND label.cell = a.place_cell
    LEFT JOIN LATERAL (
        SELECT video.id, video.original_filename, video.media_type, video.byte_size, video.duration_ms
        FROM assets AS video
        WHERE a.live_photo_id IS NOT NULL AND a.media_type NOT LIKE 'video/%'
          AND video.owner_id = a.owner_id AND video.live_photo_id = a.live_photo_id
          AND video.media_type LIKE 'video/%' AND video.deleted_at IS NULL
        ORDER BY video.created_at
        LIMIT 1
    ) AS live ON true`

func scanItem(row pgx.Row, extra ...any) (Item, error) {
	var item Item
	var hash []byte
	var details map[string]any
	var liveID, liveName, liveType *string
	var liveSize, liveDuration *int64
	targets := []any{
		&item.ID, &item.OwnerID, &item.OwnerName, &item.StorageKey, &item.OriginalFilename,
		&item.MediaType, &item.ByteSize, &hash, &item.CreatedAt, &item.TakenAt, &item.CapturedAt,
		&item.Width, &item.Height, &item.DurationMS, &item.Favorite, &item.Hidden, &item.TrashedAt,
		&item.Caption, &item.LivePhotoID, &item.Subtype, &item.Latitude, &item.Longitude,
		&item.PlaceCell, &item.PlaceName, &details,
		&liveID, &liveName, &liveType, &liveSize, &liveDuration,
	}
	if err := row.Scan(append(targets, extra...)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Item{}, ErrNotFound
		}
		return Item{}, err
	}
	if len(hash) == 32 {
		copy(item.ContentSHA256[:], hash)
	}
	item.Details = details
	if liveID != nil {
		item.LiveVideo = &Item{ID: *liveID, OwnerID: item.OwnerID, OriginalFilename: deref(liveName), MediaType: deref(liveType)}
		if liveSize != nil {
			item.LiveVideo.ByteSize = *liveSize
		}
		if liveDuration != nil {
			item.LiveVideo.DurationMS = *liveDuration
		}
	}
	return item, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type sortSpec struct {
	exprs []string
	types []string
	desc  bool
}

var sortSpecs = map[string]sortSpec{
	"taken_desc":      {[]string{"a.taken_at", "a.id"}, []string{"timestamptz", "uuid"}, true},
	"taken_asc":       {[]string{"a.taken_at", "a.id"}, []string{"timestamptz", "uuid"}, false},
	"added_desc":      {[]string{"a.created_at", "a.id"}, []string{"timestamptz", "uuid"}, true},
	"size_desc":       {[]string{"a.byte_size", "a.id"}, []string{"bigint", "uuid"}, true},
	"name_asc":        {[]string{"lower(a.original_filename)", "a.id"}, []string{"text", "uuid"}, false},
	"favorites_first": {[]string{"a.favorite", "a.taken_at", "a.id"}, []string{"boolean", "timestamptz", "uuid"}, true},
	"trashed_desc":    {[]string{"a.trashed_at", "a.id"}, []string{"timestamptz", "uuid"}, true},
	"album_added":     {[]string{"album_item.added_at", "a.id"}, []string{"timestamptz", "uuid"}, false},
}

// SortNames are the sorts a person can choose for the library.
var SortNames = []string{"taken_desc", "taken_asc", "added_desc", "size_desc", "name_asc", "favorites_first"}

type queryArgs []any

func (q *queryArgs) add(value any) string {
	*q = append(*q, value)
	return "$" + strconv.Itoa(len(*q))
}

func (s *PostgresStore) List(ctx context.Context, query ListQuery) ([]Item, []string, error) {
	if query.Limit <= 0 || query.Limit > 500 {
		return nil, nil, ErrInvalid
	}
	var args queryArgs
	viewer := args.add(query.ViewerID)
	where := []string{}
	joins := ""
	extraColumns := `, 0, false, 0, ''`
	sortName := query.Sort
	if sortName == "" {
		sortName = "taken_desc"
	}
	if _, ok := sortSpecs[sortName]; !ok || sortName == "trashed_desc" || sortName == "album_added" {
		return nil, nil, ErrInvalid
	}
	owned := "a.owner_id = " + viewer
	switch query.View {
	case "", "library":
		where = append(where, owned, visible("a"), "NOT "+pairedVideo("a"))
	case "photos":
		where = append(where, owned, visible("a"), "a.media_type LIKE 'image/%'")
	case "videos":
		where = append(where, owned, visible("a"), "a.media_type LIKE 'video/%'", "NOT "+pairedVideo("a"))
	case "live":
		where = append(where, owned, visible("a"), "live.id IS NOT NULL")
	case "selfies", "screenshots", "panoramas":
		subtype := map[string]string{"selfies": "selfie", "screenshots": "screenshot", "panoramas": "panorama"}[query.View]
		where = append(where, owned, visible("a"), "a.subtype = "+args.add(subtype), "NOT "+pairedVideo("a"))
	case "favorites":
		where = append(where, owned, visible("a"), "a.favorite", "NOT "+pairedVideo("a"))
	case "recent":
		where = append(where, owned, visible("a"), "NOT "+pairedVideo("a"))
		if query.Sort == "" {
			sortName = "added_desc"
		}
	case "hidden":
		where = append(where, owned, "a.deleted_at IS NULL", "a.trashed_at IS NULL", "a.hidden", "NOT "+pairedVideo("a"))
	case "trash":
		where = append(where, owned, "a.deleted_at IS NULL", "a.trashed_at IS NOT NULL", "NOT "+pairedVideo("a"))
		sortName = "trashed_desc"
	case "place":
		if !placeCellPattern.MatchString(query.PlaceCell) {
			return nil, nil, ErrInvalid
		}
		where = append(where, owned, visible("a"), "a.place_cell = "+args.add(query.PlaceCell), "NOT "+pairedVideo("a"))
	case "on_this_day":
		if query.Month < 1 || query.Month > 12 || query.Day < 1 || query.Day > 31 || query.TZOffset < -14*60 || query.TZOffset > 14*60 {
			return nil, nil, ErrInvalid
		}
		shifted := "(a.taken_at AT TIME ZONE 'UTC' + make_interval(mins => " + args.add(query.TZOffset) + "))"
		where = append(where, owned, visible("a"), "NOT "+pairedVideo("a"),
			"extract(month FROM "+shifted+") = "+args.add(query.Month),
			"extract(day FROM "+shifted+") = "+args.add(query.Day),
			"extract(year FROM "+shifted+") < extract(year FROM now() AT TIME ZONE 'UTC' + make_interval(mins => "+args.add(query.TZOffset)+"))")
	case "album":
		if query.AlbumID == "" {
			return nil, nil, ErrInvalid
		}
		album, err := s.Album(ctx, query.ViewerID, query.AlbumID)
		if err != nil {
			return nil, nil, err
		}
		albumArg := args.add(album.ID)
		joins = ` JOIN album_assets AS album_item ON album_item.asset_id = a.id AND album_item.album_id = ` + albumArg + `
            JOIN users AS adder ON adder.id = album_item.added_by`
		extraColumns = `,
            (SELECT count(*) FROM album_likes AS liked WHERE liked.album_id = album_item.album_id AND liked.asset_id = a.id),
            EXISTS (SELECT 1 FROM album_likes AS liked WHERE liked.album_id = album_item.album_id AND liked.asset_id = a.id AND liked.user_id = ` + viewer + `),
            (SELECT count(*) FROM album_comments AS note WHERE note.album_id = album_item.album_id AND note.asset_id = a.id),
            ` + nameOf("adder")
		where = append(where, visible("a"), `NOT (`+pairedVideo("a")+` AND EXISTS (
            SELECT 1 FROM album_assets AS pair JOIN assets AS still ON still.id = pair.asset_id
            WHERE pair.album_id = album_item.album_id AND still.live_photo_id = a.live_photo_id
              AND still.media_type NOT LIKE 'video/%'))`)
		if query.Sort == "" {
			sortName = map[string]string{"newest_first": "taken_desc", "oldest_first": "taken_asc", "added": "album_added"}[album.SortOrder]
		}
	case "ids":
		if len(query.IDs) == 0 || len(query.IDs) > 500 {
			return nil, nil, ErrInvalid
		}
		where = append(where, "a.id = ANY("+args.add(query.IDs)+"::uuid[])", "a.deleted_at IS NULL", `(a.owner_id = `+viewer+` OR (`+visible("a")+` AND EXISTS (
            SELECT 1 FROM album_assets AS shared JOIN albums AS shared_album ON shared_album.id = shared.album_id
            WHERE shared.asset_id = a.id AND `+canSeeAlbum("shared_album", viewer)+`)))`)
	default:
		return nil, nil, ErrInvalid
	}
	switch query.Media {
	case "":
	case "photo":
		where = append(where, "a.media_type LIKE 'image/%'")
	case "video":
		where = append(where, "a.media_type LIKE 'video/%'")
	default:
		return nil, nil, ErrInvalid
	}
	if search := strings.TrimSpace(query.Search); search != "" {
		if len(search) > 100 {
			return nil, nil, ErrInvalid
		}
		pattern := args.add("%" + escapeLike(search) + "%")
		where = append(where, `(a.original_filename ILIKE `+pattern+` OR a.caption ILIKE `+pattern+`
            OR label.name ILIKE `+pattern+` OR a.metadata->>'model' ILIKE `+pattern+`
            OR to_char(a.taken_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') LIKE `+pattern+`)`)
	}
	spec := sortSpecs[sortName]
	if len(query.Cursor) > 0 {
		if len(query.Cursor) != len(spec.exprs)+1 || query.Cursor[0] != sortName {
			return nil, nil, ErrInvalid
		}
		placeholders := make([]string, len(spec.exprs))
		for index, value := range query.Cursor[1:] {
			placeholders[index] = args.add(value) + "::" + spec.types[index]
		}
		operator := ">"
		if spec.desc {
			operator = "<"
		}
		where = append(where, "("+strings.Join(spec.exprs, ", ")+") "+operator+" ("+strings.Join(placeholders, ", ")+")")
	}
	direction := " ASC"
	if spec.desc {
		direction = " DESC"
	}
	orders := make([]string, len(spec.exprs))
	keys := make([]string, len(spec.exprs))
	for index, expr := range spec.exprs {
		orders[index] = expr + direction
		keys[index] = "(" + expr + ")::text"
	}
	limit := args.add(query.Limit + 1)
	sql := `SELECT ` + itemColumns + extraColumns + `, ` + strings.Join(keys, ", ") + `
        FROM assets AS a ` + itemJoins + joins + `
        WHERE ` + strings.Join(where, " AND ") + `
        ORDER BY ` + strings.Join(orders, ", ") + `
        LIMIT ` + limit
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []Item
	var lastKeys []string
	for rows.Next() {
		keyValues := make([]string, len(spec.exprs))
		var likeCount, commentCount int
		var liked bool
		var addedBy string
		extras := []any{&likeCount, &liked, &commentCount, &addedBy}
		for index := range keyValues {
			extras = append(extras, &keyValues[index])
		}
		item, err := scanItem(rows, extras...)
		if err != nil {
			return nil, nil, err
		}
		item.LikeCount, item.LikedByMe, item.CommentCount, item.AddedByName = likeCount, liked, commentCount, addedBy
		if len(items) < query.Limit {
			items = append(items, item)
			lastKeys = keyValues
		} else {
			return items, append([]string{sortName}, lastKeys...), rows.Err()
		}
	}
	return items, nil, rows.Err()
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func (s *PostgresStore) Accessible(ctx context.Context, viewerID, assetID string) (Item, error) {
	return scanItem(s.pool.QueryRow(ctx, `SELECT `+itemColumns+` FROM assets AS a `+itemJoins+`
        WHERE a.id = $2::uuid AND a.deleted_at IS NULL AND (
            a.owner_id = $1::uuid OR (`+visible("a")+` AND EXISTS (
                SELECT 1 FROM album_assets AS shared JOIN albums AS shared_album ON shared_album.id = shared.album_id
                WHERE shared.asset_id = a.id AND `+canSeeAlbum("shared_album", "$1::uuid")+`)))`, viewerID, assetID))
}

func (s *PostgresStore) AssetAlbums(ctx context.Context, viewerID, assetID string) ([]Album, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT al.id::text, al.name, al.owner_id = $1::uuid
        FROM album_assets AS item JOIN albums AS al ON al.id = item.album_id
        WHERE item.asset_id = $2::uuid AND `+canSeeAlbum("al", "$1::uuid")+`
        ORDER BY al.name`, viewerID, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var albums []Album
	for rows.Next() {
		var album Album
		if err := rows.Scan(&album.ID, &album.Name, &album.IsOwner); err != nil {
			return nil, err
		}
		albums = append(albums, album)
	}
	return albums, rows.Err()
}

// targetsCTE expands requested IDs with the other half of each Live Photo.
const targetsCTE = `
    requested AS (
        SELECT id, live_photo_id FROM assets
        WHERE owner_id = $1::uuid AND id = ANY($2::uuid[]) AND deleted_at IS NULL
    ), targets AS (
        SELECT id FROM requested
        UNION
        SELECT pair.id FROM assets AS pair JOIN requested ON pair.live_photo_id = requested.live_photo_id
        WHERE pair.owner_id = $1::uuid AND pair.deleted_at IS NULL AND requested.live_photo_id IS NOT NULL
    )`

func (s *PostgresStore) UpdateAssets(ctx context.Context, ownerID string, ids []string, action string) (int, error) {
	if len(ids) == 0 || len(ids) > 1000 {
		return 0, ErrInvalid
	}
	var change, event string
	switch action {
	case ActionFavorite:
		change, event = "favorite = true WHERE NOT favorite", "favorited"
	case ActionUnfavorite:
		change, event = "favorite = false WHERE favorite", "unfavorited"
	case ActionHide:
		change, event = "hidden = true WHERE NOT hidden", "hidden"
	case ActionUnhide:
		change, event = "hidden = false WHERE hidden", "unhidden"
	case ActionTrash:
		change, event = "trashed_at = now() WHERE trashed_at IS NULL", "trashed"
	case ActionRestore:
		change, event = "trashed_at = NULL WHERE trashed_at IS NOT NULL", "restored"
	default:
		return 0, ErrInvalid
	}
	command, err := s.pool.Exec(ctx, `WITH `+targetsCTE+`, changed AS (
            UPDATE assets SET `+change+` AND id IN (SELECT id FROM targets)
            RETURNING id, owner_id
        )
        INSERT INTO asset_events (asset_id, owner_id, event_type, actor)
        SELECT id, owner_id, '`+event+`', 'owner' FROM changed`, ownerID, ids)
	if err != nil {
		return 0, err
	}
	return int(command.RowsAffected()), nil
}

func (s *PostgresStore) SetCaption(ctx context.Context, ownerID, assetID, caption string) error {
	command, err := s.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE assets SET caption = NULLIF($3, '')
            WHERE id = $2::uuid AND owner_id = $1::uuid AND deleted_at IS NULL
            RETURNING id, owner_id
        )
        INSERT INTO asset_events (asset_id, owner_id, event_type, actor)
        SELECT id, owner_id, 'caption_changed', 'owner' FROM changed`, ownerID, assetID, caption)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) TrashedIDs(ctx context.Context, ownerID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT id::text FROM assets
        WHERE owner_id = $1::uuid AND deleted_at IS NULL AND trashed_at IS NOT NULL
        ORDER BY trashed_at LIMIT 10000`, ownerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *PostgresStore) Counts(ctx context.Context, ownerID string) (Counts, map[string]string, error) {
	names := []string{"library", "photos", "videos", "live", "selfies", "screenshots", "panoramas", "favorites", "hidden", "trash", "places"}
	filters := map[string]string{
		"library":     "shown",
		"photos":      "shown AND a.media_type LIKE 'image/%'",
		"videos":      "shown AND a.media_type LIKE 'video/%'",
		"live":        "shown AND live_still",
		"selfies":     "shown AND a.subtype = 'selfie'",
		"screenshots": "shown AND a.subtype = 'screenshot'",
		"panoramas":   "shown AND a.subtype = 'panorama'",
		"favorites":   "shown AND a.favorite",
		"hidden":      "a.trashed_at IS NULL AND a.hidden AND NOT paired",
		"trash":       "a.trashed_at IS NOT NULL AND NOT paired",
		"places":      "shown AND a.place_cell IS NOT NULL",
	}
	var columns []string
	for _, name := range names {
		columns = append(columns,
			"count(*) FILTER (WHERE "+filters[name]+")",
			"COALESCE(((array_agg(a.id::text ORDER BY a.taken_at DESC) FILTER (WHERE "+filters[name]+"))[1]), '')")
	}
	row := s.pool.QueryRow(ctx, `
        SELECT `+strings.Join(columns, ", ")+`
        FROM (
            SELECT a.*, (`+visible("a")+` AND NOT `+pairedVideo("a")+`) AS shown,
                   `+pairedVideo("a")+` AS paired,
                   (a.live_photo_id IS NOT NULL AND a.media_type NOT LIKE 'video/%' AND EXISTS (
                       SELECT 1 FROM assets AS video WHERE video.owner_id = a.owner_id
                         AND video.live_photo_id = a.live_photo_id AND video.media_type LIKE 'video/%'
                         AND video.deleted_at IS NULL)) AS live_still
            FROM assets AS a WHERE a.owner_id = $1::uuid AND a.deleted_at IS NULL
        ) AS a`, ownerID)
	targets := make([]any, 0, len(names)*2)
	counts := make([]int64, len(names))
	covers := make([]string, len(names))
	for index := range names {
		targets = append(targets, &counts[index], &covers[index])
	}
	if err := row.Scan(targets...); err != nil {
		return nil, nil, err
	}
	result := Counts{}
	coverMap := map[string]string{}
	for index, name := range names {
		result[name] = counts[index]
		if covers[index] != "" {
			coverMap[name] = covers[index]
		}
	}
	return result, coverMap, nil
}

func (s *PostgresStore) Usage(ctx context.Context, ownerID string) (Usage, error) {
	var usage Usage
	err := s.pool.QueryRow(ctx, `
        SELECT
            count(*) FILTER (WHERE trashed_at IS NULL AND media_type NOT LIKE 'video/%'),
            COALESCE(sum(byte_size) FILTER (WHERE trashed_at IS NULL AND media_type NOT LIKE 'video/%'), 0),
            count(*) FILTER (WHERE trashed_at IS NULL AND media_type LIKE 'video/%'),
            COALESCE(sum(byte_size) FILTER (WHERE trashed_at IS NULL AND media_type LIKE 'video/%'), 0),
            count(*) FILTER (WHERE trashed_at IS NOT NULL),
            COALESCE(sum(byte_size) FILTER (WHERE trashed_at IS NOT NULL), 0),
            count(*) FILTER (WHERE trashed_at IS NULL AND hidden),
            (SELECT quota_bytes FROM users WHERE id = $1::uuid)
        FROM assets WHERE owner_id = $1::uuid AND deleted_at IS NULL`, ownerID).Scan(
		&usage.PhotoCount, &usage.PhotoBytes, &usage.VideoCount, &usage.VideoBytes,
		&usage.TrashCount, &usage.TrashBytes, &usage.HiddenCount, &usage.QuotaBytes)
	return usage, err
}

// ------------------------------------------------------------------ albums

const albumColumns = `
    al.id::text, al.owner_id::text, ` + `COALESCE(NULLIF(album_owner.display_name, ''), split_part(album_owner.email, '@', 1))` + `,
    COALESCE(al.folder_id::text, ''), al.name, al.sort_order, al.members_can_add,
    al.owner_id = $1::uuid, EXISTS (SELECT 1 FROM album_members AS any_member WHERE any_member.album_id = al.id),
    al.updated_at, COALESCE(stats.item_count, 0), stats.latest_added,
    COALESCE(cover.id::text, ''), COALESCE(cover.owner_id::text, '')`

func albumFrom() string {
	shownInAlbum := `album_item.album_id = al.id AND ` + visible("item") + ` AND NOT (` + pairedVideo("item") + ` AND EXISTS (
        SELECT 1 FROM album_assets AS pair JOIN assets AS still ON still.id = pair.asset_id
        WHERE pair.album_id = al.id AND still.live_photo_id = item.live_photo_id AND still.media_type NOT LIKE 'video/%'))`
	return `
    FROM albums AS al
    JOIN users AS album_owner ON album_owner.id = al.owner_id
    LEFT JOIN LATERAL (
        SELECT count(*) AS item_count, max(album_item.added_at) AS latest_added
        FROM album_assets AS album_item JOIN assets AS item ON item.id = album_item.asset_id
        WHERE ` + shownInAlbum + `
    ) AS stats ON true
    LEFT JOIN LATERAL (
        SELECT item.id, item.owner_id
        FROM album_assets AS album_item JOIN assets AS item ON item.id = album_item.asset_id
        WHERE ` + shownInAlbum + `
        ORDER BY item.id = al.cover_asset_id DESC, item.taken_at DESC
        LIMIT 1
    ) AS cover ON true`
}

func scanAlbum(row pgx.Row) (Album, error) {
	var album Album
	err := row.Scan(&album.ID, &album.OwnerID, &album.OwnerName, &album.FolderID, &album.Name,
		&album.SortOrder, &album.MembersCanAdd, &album.IsOwner, &album.Shared, &album.UpdatedAt,
		&album.Count, &album.LatestAddedAt, &album.CoverAssetID, &album.CoverOwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Album{}, ErrNotFound
	}
	return album, err
}

func (s *PostgresStore) Albums(ctx context.Context, viewerID string) ([]Album, []Folder, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+albumColumns+albumFrom()+`
        WHERE `+canSeeAlbum("al", "$1::uuid")+`
        ORDER BY lower(al.name), al.id`, viewerID)
	if err != nil {
		return nil, nil, err
	}
	var albums []Album
	for rows.Next() {
		album, err := scanAlbum(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		albums = append(albums, album)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	folderRows, err := s.pool.Query(ctx, `
        SELECT id::text, COALESCE(parent_id::text, ''), name, updated_at
        FROM album_folders WHERE owner_id = $1::uuid ORDER BY lower(name), id`, viewerID)
	if err != nil {
		return nil, nil, err
	}
	defer folderRows.Close()
	var folders []Folder
	for folderRows.Next() {
		var folder Folder
		if err := folderRows.Scan(&folder.ID, &folder.ParentID, &folder.Name, &folder.UpdatedAt); err != nil {
			return nil, nil, err
		}
		folders = append(folders, folder)
	}
	return albums, folders, folderRows.Err()
}

func (s *PostgresStore) Album(ctx context.Context, viewerID, albumID string) (Album, error) {
	album, err := scanAlbum(s.pool.QueryRow(ctx, `SELECT `+albumColumns+albumFrom()+`
        WHERE al.id = $2::uuid AND `+canSeeAlbum("al", "$1::uuid"), viewerID, albumID))
	if err != nil {
		return Album{}, err
	}
	rows, err := s.pool.Query(ctx, `
        SELECT person.id::text, `+nameOf("person")+`, person.email, person.id = $2::uuid
        FROM album_members AS member JOIN users AS person ON person.id = member.user_id
        WHERE member.album_id = $1::uuid
        ORDER BY member.added_at`, album.ID, viewerID)
	if err != nil {
		return Album{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var person Person
		if err := rows.Scan(&person.ID, &person.DisplayName, &person.Email, &person.IsMe); err != nil {
			return Album{}, err
		}
		album.Members = append(album.Members, person)
	}
	return album, rows.Err()
}

func validName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	return name, name != "" && len([]rune(name)) <= 100
}

func (s *PostgresStore) CreateAlbum(ctx context.Context, ownerID, name, folderID string) (Album, error) {
	name, ok := validName(name)
	if !ok {
		return Album{}, ErrInvalid
	}
	var id string
	err := s.pool.QueryRow(ctx, `
        INSERT INTO albums (owner_id, name, folder_id)
        SELECT $1::uuid, $2, NULLIF($3, '')::uuid
        WHERE $3 = '' OR EXISTS (SELECT 1 FROM album_folders WHERE id = NULLIF($3, '')::uuid AND owner_id = $1::uuid)
        RETURNING id::text`, ownerID, name, folderID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Album{}, ErrInvalid
	}
	if err != nil {
		return Album{}, err
	}
	return s.Album(ctx, ownerID, id)
}

func (s *PostgresStore) UpdateAlbum(ctx context.Context, ownerID, albumID string, patch AlbumPatch) error {
	var args queryArgs
	owner := args.add(ownerID)
	album := args.add(albumID)
	sets := []string{"updated_at = now()"}
	if patch.Name != nil {
		name, ok := validName(*patch.Name)
		if !ok {
			return ErrInvalid
		}
		sets = append(sets, "name = "+args.add(name))
	}
	if patch.FolderID != nil {
		if *patch.FolderID == "" {
			sets = append(sets, "folder_id = NULL")
		} else {
			var exists bool
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM album_folders WHERE id = $1::uuid AND owner_id = $2::uuid)`, *patch.FolderID, ownerID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrInvalid
			}
			sets = append(sets, "folder_id = "+args.add(*patch.FolderID)+"::uuid")
		}
	}
	if patch.CoverAssetID != nil {
		if *patch.CoverAssetID == "" {
			sets = append(sets, "cover_asset_id = NULL")
		} else {
			var exists bool
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM album_assets WHERE album_id = $1::uuid AND asset_id = $2::uuid)`, albumID, *patch.CoverAssetID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrInvalid
			}
			sets = append(sets, "cover_asset_id = "+args.add(*patch.CoverAssetID)+"::uuid")
		}
	}
	if patch.SortOrder != nil {
		switch *patch.SortOrder {
		case "newest_first", "oldest_first", "added":
			sets = append(sets, "sort_order = "+args.add(*patch.SortOrder))
		default:
			return ErrInvalid
		}
	}
	if patch.MembersCanAdd != nil {
		sets = append(sets, "members_can_add = "+args.add(*patch.MembersCanAdd))
	}
	command, err := s.pool.Exec(ctx, `UPDATE albums SET `+strings.Join(sets, ", ")+`
        WHERE id = `+album+`::uuid AND owner_id = `+owner+`::uuid`, args...)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return s.ownerOnly(ctx, ownerID, albumID)
	}
	return nil
}

// ownerOnly explains why an owner-only change matched nothing.
func (s *PostgresStore) ownerOnly(ctx context.Context, viewerID, albumID string) error {
	if _, err := s.Album(ctx, viewerID, albumID); err != nil {
		return err
	}
	return ErrForbidden
}

func (s *PostgresStore) DeleteAlbum(ctx context.Context, ownerID, albumID string) error {
	command, err := s.pool.Exec(ctx, `DELETE FROM albums WHERE id = $2::uuid AND owner_id = $1::uuid`, ownerID, albumID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return s.ownerOnly(ctx, ownerID, albumID)
	}
	return nil
}

func (s *PostgresStore) AddToAlbum(ctx context.Context, viewerID, albumID string, assetIDs []string) (int, error) {
	if len(assetIDs) == 0 || len(assetIDs) > 1000 {
		return 0, ErrInvalid
	}
	album, err := s.Album(ctx, viewerID, albumID)
	if err != nil {
		return 0, err
	}
	if !album.IsOwner && !album.MembersCanAdd {
		return 0, ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `WITH `+targetsCTE+`
        INSERT INTO album_assets (album_id, asset_id, added_by)
        SELECT $3::uuid, targets.id, $1::uuid FROM targets
        JOIN assets ON assets.id = targets.id AND assets.trashed_at IS NULL
        ON CONFLICT DO NOTHING`, viewerID, assetIDs, album.ID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE albums SET updated_at = now() WHERE id = $1::uuid`, album.ID); err != nil {
		return 0, err
	}
	return int(command.RowsAffected()), tx.Commit(ctx)
}

func (s *PostgresStore) RemoveFromAlbum(ctx context.Context, viewerID, albumID string, assetIDs []string) (int, error) {
	if len(assetIDs) == 0 || len(assetIDs) > 1000 {
		return 0, ErrInvalid
	}
	album, err := s.Album(ctx, viewerID, albumID)
	if err != nil {
		return 0, err
	}
	// The owner may remove anything; members only what they added. The other
	// half of a Live Photo goes with it.
	command, err := s.pool.Exec(ctx, `
        WITH requested AS (
            SELECT assets.id, assets.owner_id, assets.live_photo_id FROM album_assets
            JOIN assets ON assets.id = album_assets.asset_id
            WHERE album_assets.album_id = $1::uuid AND album_assets.asset_id = ANY($2::uuid[])
        ), targets AS (
            SELECT id FROM requested
            UNION
            SELECT pair.id FROM assets AS pair JOIN requested
              ON pair.owner_id = requested.owner_id AND pair.live_photo_id = requested.live_photo_id
            WHERE requested.live_photo_id IS NOT NULL
        )
        DELETE FROM album_assets
        WHERE album_id = $1::uuid AND asset_id IN (SELECT id FROM targets)
          AND ($3 OR added_by = $4::uuid)`, album.ID, assetIDs, album.IsOwner, viewerID)
	if err != nil {
		return 0, err
	}
	_, err = s.pool.Exec(ctx, `
        UPDATE albums SET updated_at = now(),
            cover_asset_id = CASE WHEN cover_asset_id = ANY($2::uuid[]) THEN NULL ELSE cover_asset_id END
        WHERE id = $1::uuid`, album.ID, assetIDs)
	return int(command.RowsAffected()), err
}

func (s *PostgresStore) SetMembers(ctx context.Context, ownerID, albumID string, userIDs []string) error {
	if len(userIDs) > 50 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var isOwner bool
	err = tx.QueryRow(ctx, `SELECT owner_id = $1::uuid FROM albums WHERE id = $2::uuid FOR UPDATE`, ownerID, albumID).Scan(&isOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !isOwner {
		return s.ownerOnly(ctx, ownerID, albumID)
	}
	var valid int
	if err := tx.QueryRow(ctx, `
        SELECT count(*) FROM users
        WHERE id = ANY($1::uuid[]) AND id <> $2::uuid AND state = 'active' AND deleted_at IS NULL`,
		userIDs, ownerID).Scan(&valid); err != nil {
		return err
	}
	if valid != len(uniqueStrings(userIDs)) {
		return ErrInvalid
	}
	// People removed from the album also take their own photos with them.
	if _, err := tx.Exec(ctx, `
        DELETE FROM album_assets WHERE album_id = $1::uuid AND added_by <> $3::uuid
          AND added_by IN (SELECT user_id FROM album_members WHERE album_id = $1::uuid AND user_id <> ALL($2::uuid[]))`,
		albumID, userIDs, ownerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM album_members WHERE album_id = $1::uuid AND user_id <> ALL($2::uuid[])`, albumID, userIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO album_members (album_id, user_id)
        SELECT $1::uuid, unnest($2::uuid[]) ON CONFLICT DO NOTHING`, albumID, userIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE albums SET updated_at = now() WHERE id = $1::uuid`, albumID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func (s *PostgresStore) LeaveAlbum(ctx context.Context, userID, albumID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `DELETE FROM album_members WHERE album_id = $1::uuid AND user_id = $2::uuid`, albumID, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM album_assets WHERE album_id = $1::uuid AND added_by = $2::uuid`, albumID, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// inAlbum checks the viewer can see the album and the item is shown in it.
func (s *PostgresStore) inAlbum(ctx context.Context, viewerID, albumID, assetID string) (Album, error) {
	album, err := s.Album(ctx, viewerID, albumID)
	if err != nil {
		return Album{}, err
	}
	var shown bool
	if err := s.pool.QueryRow(ctx, `
        SELECT EXISTS (
            SELECT 1 FROM album_assets JOIN assets AS a ON a.id = album_assets.asset_id
            WHERE album_assets.album_id = $1::uuid AND album_assets.asset_id = $2::uuid AND `+visible("a")+`)`,
		album.ID, assetID).Scan(&shown); err != nil {
		return Album{}, err
	}
	if !shown {
		return Album{}, ErrNotFound
	}
	return album, nil
}

func (s *PostgresStore) SetLike(ctx context.Context, viewerID, albumID, assetID string, liked bool) error {
	album, err := s.inAlbum(ctx, viewerID, albumID, assetID)
	if err != nil {
		return err
	}
	if liked {
		_, err = s.pool.Exec(ctx, `INSERT INTO album_likes (album_id, asset_id, user_id) VALUES ($1::uuid, $2::uuid, $3::uuid) ON CONFLICT DO NOTHING`, album.ID, assetID, viewerID)
	} else {
		_, err = s.pool.Exec(ctx, `DELETE FROM album_likes WHERE album_id = $1::uuid AND asset_id = $2::uuid AND user_id = $3::uuid`, album.ID, assetID, viewerID)
	}
	return err
}

func (s *PostgresStore) Comments(ctx context.Context, viewerID, albumID, assetID string) ([]Comment, error) {
	album, err := s.inAlbum(ctx, viewerID, albumID, assetID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
        SELECT note.id::text, note.user_id::text, `+nameOf("author")+`, note.body, note.created_at
        FROM album_comments AS note JOIN users AS author ON author.id = note.user_id
        WHERE note.album_id = $1::uuid AND note.asset_id = $2::uuid
        ORDER BY note.created_at, note.id LIMIT 500`, album.ID, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []Comment
	for rows.Next() {
		comment := Comment{AlbumID: album.ID, AssetID: assetID}
		if err := rows.Scan(&comment.ID, &comment.AuthorID, &comment.AuthorName, &comment.Body, &comment.CreatedAt); err != nil {
			return nil, err
		}
		comment.Mine = comment.AuthorID == viewerID
		comments = append(comments, comment)
	}
	return comments, rows.Err()
}

func (s *PostgresStore) AddComment(ctx context.Context, viewerID, albumID, assetID, body string) (Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" || len([]rune(body)) > 1000 {
		return Comment{}, ErrInvalid
	}
	album, err := s.inAlbum(ctx, viewerID, albumID, assetID)
	if err != nil {
		return Comment{}, err
	}
	comment := Comment{AlbumID: album.ID, AssetID: assetID, AuthorID: viewerID, Body: body, Mine: true}
	err = s.pool.QueryRow(ctx, `
        WITH inserted AS (
            INSERT INTO album_comments (album_id, asset_id, user_id, body)
            VALUES ($1::uuid, $2::uuid, $3::uuid, $4)
            RETURNING id, created_at
        )
        SELECT inserted.id::text, inserted.created_at, `+nameOf("author")+`
        FROM inserted, users AS author WHERE author.id = $3::uuid`,
		album.ID, assetID, viewerID, body).Scan(&comment.ID, &comment.CreatedAt, &comment.AuthorName)
	return comment, err
}

func (s *PostgresStore) DeleteComment(ctx context.Context, viewerID, albumID, commentID string) error {
	command, err := s.pool.Exec(ctx, `
        DELETE FROM album_comments AS note
        USING albums AS al
        WHERE note.id = $3::uuid AND note.album_id = $2::uuid AND al.id = note.album_id
          AND (note.user_id = $1::uuid OR al.owner_id = $1::uuid)`, viewerID, albumID, commentID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Activity(ctx context.Context, viewerID string, limit int) ([]Activity, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
        SELECT * FROM (
            SELECT 'added' AS kind, al.id::text, al.name, `+nameOf("actor")+`,
                   (array_agg(a.id::text ORDER BY item.added_at DESC))[1:4], count(*)::int, '' AS body,
                   max(item.added_at) AS at
            FROM album_assets AS item
            JOIN albums AS al ON al.id = item.album_id
            JOIN assets AS a ON a.id = item.asset_id
            JOIN users AS actor ON actor.id = item.added_by
            WHERE `+canSeeAlbum("al", "$1::uuid")+` AND item.added_by <> $1::uuid AND `+visible("a")+`
              AND NOT `+pairedVideo("a")+` AND item.added_at > now() - interval '90 days'
            GROUP BY al.id, al.name, actor.id, actor.display_name, actor.email, date_trunc('hour', item.added_at)
            UNION ALL
            SELECT 'comment', al.id::text, al.name, `+nameOf("actor")+`, ARRAY[note.asset_id::text], 1, note.body, note.created_at
            FROM album_comments AS note
            JOIN albums AS al ON al.id = note.album_id
            JOIN users AS actor ON actor.id = note.user_id
            JOIN assets AS a ON a.id = note.asset_id
            WHERE `+canSeeAlbum("al", "$1::uuid")+` AND note.user_id <> $1::uuid AND `+visible("a")+`
              AND note.created_at > now() - interval '90 days'
        ) AS feed
        ORDER BY at DESC
        LIMIT $2`, viewerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var feed []Activity
	for rows.Next() {
		var entry Activity
		if err := rows.Scan(&entry.Kind, &entry.AlbumID, &entry.AlbumName, &entry.ActorName, &entry.AssetIDs, &entry.Count, &entry.Body, &entry.At); err != nil {
			return nil, err
		}
		feed = append(feed, entry)
	}
	return feed, rows.Err()
}

// ----------------------------------------------------------------- folders

const maxFolderDepth = 5

func (s *PostgresStore) folderDepth(ctx context.Context, tx pgx.Tx, ownerID, folderID string) (int, error) {
	var depth int
	err := tx.QueryRow(ctx, `
        WITH RECURSIVE chain AS (
            SELECT id, parent_id, 1 AS depth FROM album_folders WHERE id = $1::uuid AND owner_id = $2::uuid
            UNION ALL
            SELECT parent.id, parent.parent_id, chain.depth + 1
            FROM album_folders AS parent JOIN chain ON parent.id = chain.parent_id
            WHERE chain.depth < 50
        )
        SELECT COALESCE(max(depth), 0) FROM chain`, folderID, ownerID).Scan(&depth)
	return depth, err
}

func (s *PostgresStore) CreateFolder(ctx context.Context, ownerID, name, parentID string) (Folder, error) {
	name, ok := validName(name)
	if !ok {
		return Folder{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Folder{}, err
	}
	defer tx.Rollback(ctx)
	if parentID != "" {
		depth, err := s.folderDepth(ctx, tx, ownerID, parentID)
		if err != nil {
			return Folder{}, err
		}
		if depth == 0 || depth >= maxFolderDepth {
			return Folder{}, ErrInvalid
		}
	}
	folder := Folder{ParentID: parentID, Name: name}
	if err := tx.QueryRow(ctx, `
        INSERT INTO album_folders (owner_id, parent_id, name) VALUES ($1::uuid, NULLIF($2, '')::uuid, $3)
        RETURNING id::text, updated_at`, ownerID, parentID, name).Scan(&folder.ID, &folder.UpdatedAt); err != nil {
		return Folder{}, err
	}
	return folder, tx.Commit(ctx)
}

func (s *PostgresStore) UpdateFolder(ctx context.Context, ownerID, folderID string, patch FolderPatch) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM album_folders WHERE id = $1::uuid AND owner_id = $2::uuid)`, folderID, ownerID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if patch.Name != nil {
		name, ok := validName(*patch.Name)
		if !ok {
			return ErrInvalid
		}
		if _, err := tx.Exec(ctx, `UPDATE album_folders SET name = $3, updated_at = now() WHERE id = $1::uuid AND owner_id = $2::uuid`, folderID, ownerID, name); err != nil {
			return err
		}
	}
	if patch.ParentID != nil {
		parent := *patch.ParentID
		if parent != "" {
			// The new parent must not be the folder or anything inside it.
			var cycle bool
			if err := tx.QueryRow(ctx, `
                WITH RECURSIVE inside AS (
                    SELECT id FROM album_folders WHERE id = $1::uuid
                    UNION
                    SELECT child.id FROM album_folders AS child JOIN inside ON child.parent_id = inside.id
                )
                SELECT $2::uuid IN (SELECT id FROM inside)`, folderID, parent).Scan(&cycle); err != nil {
				return err
			}
			depth, err := s.folderDepth(ctx, tx, ownerID, parent)
			if err != nil {
				return err
			}
			if cycle || depth == 0 || depth >= maxFolderDepth {
				return ErrInvalid
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE album_folders SET parent_id = NULLIF($3, '')::uuid, updated_at = now() WHERE id = $1::uuid AND owner_id = $2::uuid`, folderID, ownerID, parent); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DeleteFolder removes a folder and moves its albums and folders up a level;
// no album or photo is deleted.
func (s *PostgresStore) DeleteFolder(ctx context.Context, ownerID, folderID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var parent *string
	err = tx.QueryRow(ctx, `SELECT parent_id::text FROM album_folders WHERE id = $1::uuid AND owner_id = $2::uuid FOR UPDATE`, folderID, ownerID).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE albums SET folder_id = $3::uuid WHERE folder_id = $1::uuid AND owner_id = $2::uuid`, folderID, ownerID, parent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE album_folders SET parent_id = $3::uuid WHERE parent_id = $1::uuid AND owner_id = $2::uuid`, folderID, ownerID, parent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM album_folders WHERE id = $1::uuid AND owner_id = $2::uuid`, folderID, ownerID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ------------------------------------------------------------------ places

func (s *PostgresStore) Places(ctx context.Context, ownerID string) ([]Place, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT a.place_cell, COALESCE(label.name, ''), avg(a.latitude), avg(a.longitude), count(*)::int,
               (array_agg(a.id::text ORDER BY a.taken_at DESC))[1], max(a.taken_at)
        FROM assets AS a
        LEFT JOIN place_labels AS label ON label.owner_id = a.owner_id AND label.cell = a.place_cell
        WHERE a.owner_id = $1::uuid AND a.place_cell IS NOT NULL AND `+visible("a")+` AND NOT `+pairedVideo("a")+`
        GROUP BY a.place_cell, label.name
        ORDER BY count(*) DESC, max(a.taken_at) DESC
        LIMIT 500`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var places []Place
	for rows.Next() {
		var place Place
		if err := rows.Scan(&place.Cell, &place.Name, &place.Latitude, &place.Longitude, &place.Count, &place.CoverAssetID, &place.LatestTaken); err != nil {
			return nil, err
		}
		places = append(places, place)
	}
	return places, rows.Err()
}

func (s *PostgresStore) LabelPlace(ctx context.Context, ownerID, cell, name string) error {
	if !placeCellPattern.MatchString(cell) {
		return ErrInvalid
	}
	name = strings.TrimSpace(name)
	if name == "" {
		_, err := s.pool.Exec(ctx, `DELETE FROM place_labels WHERE owner_id = $1::uuid AND cell = $2`, ownerID, cell)
		return err
	}
	if len([]rune(name)) > 100 {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `
        INSERT INTO place_labels (owner_id, cell, name) VALUES ($1::uuid, $2, $3)
        ON CONFLICT (owner_id, cell) DO UPDATE SET name = EXCLUDED.name, updated_at = now()`, ownerID, cell, name)
	return err
}

// ------------------------------------------------------------------ people

func (s *PostgresStore) People(ctx context.Context, viewerID string) ([]Person, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT id::text, `+nameOf("users")+`, email, id = $1::uuid
        FROM users WHERE state = 'active' AND deleted_at IS NULL
        ORDER BY id = $1::uuid DESC, lower(`+nameOf("users")+`)`, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var people []Person
	for rows.Next() {
		var person Person
		if err := rows.Scan(&person.ID, &person.DisplayName, &person.Email, &person.IsMe); err != nil {
			return nil, err
		}
		people = append(people, person)
	}
	return people, rows.Err()
}

func (s *PostgresStore) SetDisplayName(ctx context.Context, userID, name string) error {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 60 {
		return ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET display_name = NULLIF($2, '') WHERE id = $1::uuid`, userID, name)
	return err
}

// ---------------------------------------------------------------- metadata

func (s *PostgresStore) PendingMetadata(ctx context.Context, version int, limit int) ([]PendingMetadata, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT id::text, owner_id::text, storage_key, media_type FROM assets
        WHERE deleted_at IS NULL AND metadata_version < $1
        ORDER BY created_at, id LIMIT $2`, version, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []PendingMetadata
	for rows.Next() {
		var item PendingMetadata
		if err := rows.Scan(&item.ID, &item.OwnerID, &item.StorageKey, &item.MediaType); err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}

func nullablePositive[T int | int64](value T) *T {
	if value <= 0 {
		return nil
	}
	return &value
}

func (s *PostgresStore) SaveMetadata(ctx context.Context, assetID string, version int, meta media.Metadata) error {
	details, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
        UPDATE assets SET captured_at = $2, width = $3, height = $4, duration_ms = $5,
            live_photo_id = NULLIF($6, ''), subtype = NULLIF($7, ''), latitude = $8, longitude = $9,
            metadata = metadata || $10::jsonb, metadata_version = $11
        WHERE id = $1::uuid AND deleted_at IS NULL`,
		assetID, meta.CapturedAt, nullablePositive(meta.Width), nullablePositive(meta.Height),
		nullablePositive(meta.DurationMS), meta.LivePhotoID, meta.Subtype, meta.Latitude, meta.Longitude,
		string(details), version)
	return err
}

func (s *PostgresStore) MatchAsset(ctx context.Context, ownerID, filename, mediaType string, capturedAt *time.Time) string {
	var id string
	err := s.pool.QueryRow(ctx, `
        SELECT id::text FROM assets
        WHERE owner_id = $1::uuid AND deleted_at IS NULL
          AND split_part(media_type, '/', 1) = split_part($3, '/', 1)
          AND CASE WHEN $4::timestamptz IS NOT NULL
                   THEN captured_at BETWEEN $4::timestamptz - interval '1 second' AND $4::timestamptz + interval '1 second'
                   ELSE lower(original_filename) = lower($2) END
        ORDER BY created_at DESC LIMIT 1`, ownerID, filename, mediaType, capturedAt).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

var _ Store = (*PostgresStore)(nil)
