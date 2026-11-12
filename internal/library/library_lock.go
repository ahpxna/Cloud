package library

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var ErrLibraryLocked = errors.New("library locked")

// lockedStore is scoped to one authenticated request, never shared across users.
type lockedStore struct{ Store }

func (s lockedStore) check(ctx context.Context, user string, ids []string) error {
	for _, id := range ids {
		item, err := s.Accessible(ctx, user, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && item.LivePhotoID != "" {
			if checker, ok := s.Store.(interface {
				ProtectedTargets(context.Context, string, []string) (bool, error)
			}); ok {
				protected, err := checker.ProtectedTargets(ctx, user, ids)
				if err != nil {
					return err
				}
				if protected {
					return ErrLibraryLocked
				}
				break
			}
		}
	}
	return nil
}

func (s lockedStore) Accessible(ctx context.Context, user, id string) (Item, error) {
	item, err := s.Store.Accessible(ctx, user, id)
	if err == nil && (item.Hidden || item.TrashedAt != nil) {
		return Item{}, ErrLibraryLocked
	}
	return item, err
}
func (s lockedStore) List(ctx context.Context, q ListQuery) ([]Item, []string, error) {
	if q.View == "hidden" || q.View == "trash" {
		return nil, nil, ErrLibraryLocked
	}
	if q.View == "ids" {
		if err := s.check(ctx, q.ViewerID, q.IDs); err != nil {
			return nil, nil, err
		}
	}
	items, cursor, err := s.Store.List(ctx, q)
	if err == nil {
		for _, item := range items {
			if item.Hidden || item.TrashedAt != nil {
				return nil, nil, ErrLibraryLocked
			}
		}
	}
	return items, cursor, err
}
func (s lockedStore) UpdateAssets(ctx context.Context, user string, ids []string, action string) (int, error) {
	if err := s.check(ctx, user, ids); err != nil {
		return 0, err
	}
	return s.Store.UpdateAssets(ctx, user, ids, action)
}
func (s lockedStore) SetCaption(ctx context.Context, user, id, caption string) error {
	if err := s.check(ctx, user, []string{id}); err != nil {
		return err
	}
	return s.Store.SetCaption(ctx, user, id, caption)
}
func (s lockedStore) TrashedIDs(context.Context, string) ([]string, error) {
	return nil, ErrLibraryLocked
}
func (s lockedStore) AssetAlbums(ctx context.Context, user, id string) ([]Album, error) {
	if _, err := s.Accessible(ctx, user, id); err != nil {
		return nil, err
	}
	return s.Store.AssetAlbums(ctx, user, id)
}
func (s lockedStore) AddToAlbum(ctx context.Context, user, album string, ids []string) (int, error) {
	if err := s.check(ctx, user, ids); err != nil {
		return 0, err
	}
	return s.Store.AddToAlbum(ctx, user, album, ids)
}
func (s lockedStore) RemoveFromAlbum(ctx context.Context, user, album string, ids []string) (int, error) {
	if err := s.check(ctx, user, ids); err != nil {
		return 0, err
	}
	return s.Store.RemoveFromAlbum(ctx, user, album, ids)
}
func (s lockedStore) UpdateAlbum(ctx context.Context, user, album string, patch AlbumPatch) error {
	if patch.CoverAssetID != nil && *patch.CoverAssetID != "" {
		if err := s.check(ctx, user, []string{*patch.CoverAssetID}); err != nil {
			return err
		}
	}
	return s.Store.UpdateAlbum(ctx, user, album, patch)
}
func (s lockedStore) SetLike(ctx context.Context, user, album, id string, liked bool) error {
	if err := s.check(ctx, user, []string{id}); err != nil {
		return err
	}
	return s.Store.SetLike(ctx, user, album, id, liked)
}
func (s lockedStore) Comments(ctx context.Context, user, album, id string) ([]Comment, error) {
	if err := s.check(ctx, user, []string{id}); err != nil {
		return nil, err
	}
	return s.Store.Comments(ctx, user, album, id)
}
func (s lockedStore) AddComment(ctx context.Context, user, album, id, body string) (Comment, error) {
	if err := s.check(ctx, user, []string{id}); err != nil {
		return Comment{}, err
	}
	return s.Store.AddComment(ctx, user, album, id, body)
}

func (s lockedStore) DeleteComment(ctx context.Context, user, album, comment string) error {
	if lookup, ok := s.Store.(interface {
		CommentAsset(context.Context, string, string, string) (string, error)
	}); ok {
		id, err := lookup.CommentAsset(ctx, user, album, comment)
		if err != nil {
			return err
		}
		if err := s.check(ctx, user, []string{id}); err != nil {
			return err
		}
	}
	return s.Store.DeleteComment(ctx, user, album, comment)
}

func (s *PostgresStore) CommentAsset(ctx context.Context, user, album, comment string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT note.asset_id::text FROM album_comments note
 JOIN albums al ON al.id=note.album_id WHERE note.id=$3::uuid AND note.album_id=$2::uuid
 AND `+canSeeAlbum("al", "$1::uuid"), user, album, comment).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// ProtectedTargets includes the paired half of a Live Photo, just as mutations do.
func (s *PostgresStore) ProtectedTargets(ctx context.Context, user string, ids []string) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `WITH `+targetsCTE+` SELECT EXISTS(SELECT 1 FROM targets JOIN assets a ON a.id=targets.id WHERE a.hidden OR a.trashed_at IS NOT NULL)`, user, ids).Scan(&found)
	return found, err
}
