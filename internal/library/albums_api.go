package library

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"family-photo-cloud/internal/auth"
)

type albumResponse struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	FolderID      string           `json:"folder_id,omitempty"`
	OwnerID       string           `json:"owner_id"`
	OwnerName     string           `json:"owner_name"`
	IsOwner       bool             `json:"is_owner"`
	Shared        bool             `json:"shared"`
	MembersCanAdd bool             `json:"members_can_add"`
	CanAdd        bool             `json:"can_add"`
	SortOrder     string           `json:"sort_order"`
	Count         int              `json:"count"`
	UpdatedAt     time.Time        `json:"updated_at"`
	LatestAddedAt *time.Time       `json:"latest_added_at,omitempty"`
	CoverAssetID  string           `json:"cover_asset_id,omitempty"`
	CoverURL      string           `json:"cover_url,omitempty"`
	Members       []personResponse `json:"members,omitempty"`
}

type folderResponse struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

type personResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Me          bool   `json:"me,omitempty"`
}

func (api *API) albumResponse(principal auth.Principal, album Album, now time.Time) albumResponse {
	response := albumResponse{
		ID: album.ID, Name: album.Name, FolderID: album.FolderID, OwnerID: album.OwnerID, OwnerName: album.OwnerName,
		IsOwner: album.IsOwner, Shared: album.Shared, MembersCanAdd: album.MembersCanAdd,
		CanAdd: album.IsOwner || album.MembersCanAdd, SortOrder: album.SortOrder, Count: album.Count,
		UpdatedAt: album.UpdatedAt, LatestAddedAt: album.LatestAddedAt, CoverAssetID: album.CoverAssetID,
	}
	if album.CoverAssetID != "" && api.viewTickets != nil && api.hasThumbnail(album.CoverOwnerID, album.CoverAssetID) {
		response.CoverURL, _ = api.thumbnailURL(principal, album.CoverAssetID, now)
	}
	for _, member := range album.Members {
		response.Members = append(response.Members, personResponse{ID: member.ID, DisplayName: member.DisplayName, Email: member.Email, Me: member.IsMe})
	}
	return response
}

func (api *API) serveAlbums(w http.ResponseWriter, r *http.Request, principal auth.Principal, path string) {
	ctx := r.Context()
	parts := strings.Split(path, "/")
	if path == "" {
		switch r.Method {
		case http.MethodGet:
			albums, folders, err := api.store.Albums(ctx, principal.UserID)
			if err != nil {
				api.writeStoreError(w, err, "could not list albums")
				return
			}
			now := time.Now()
			response := map[string]any{"albums": []albumResponse{}, "folders": []folderResponse{}}
			albumList := make([]albumResponse, 0, len(albums))
			for _, album := range albums {
				albumList = append(albumList, api.albumResponse(principal, album, now))
			}
			folderList := make([]folderResponse, 0, len(folders))
			for _, folder := range folders {
				folderList = append(folderList, folderResponse{ID: folder.ID, ParentID: folder.ParentID, Name: folder.Name, UpdatedAt: folder.UpdatedAt})
			}
			response["albums"], response["folders"] = albumList, folderList
			writeJSON(w, http.StatusOK, response)
		case http.MethodPost:
			var request struct {
				Name      string   `json:"name"`
				FolderID  string   `json:"folder_id"`
				AssetIDs  []string `json:"asset_ids"`
				MemberIDs []string `json:"member_ids"`
			}
			if !decodeJSON(w, r, &request) {
				return
			}
			if (request.FolderID != "" && !safeID.MatchString(request.FolderID)) || !allSafe(request.AssetIDs) || !allSafe(request.MemberIDs) {
				writeProblem(w, http.StatusBadRequest, "invalid_request", "invalid IDs")
				return
			}
			album, err := api.store.CreateAlbum(ctx, principal.UserID, request.Name, request.FolderID)
			if err != nil {
				api.writeStoreError(w, err, "could not create the album")
				return
			}
			if len(request.AssetIDs) > 0 {
				if _, err := api.store.AddToAlbum(ctx, principal.UserID, album.ID, request.AssetIDs); err != nil {
					api.writeStoreError(w, err, "could not add photos to the album")
					return
				}
			}
			if len(request.MemberIDs) > 0 {
				if err := api.store.SetMembers(ctx, principal.UserID, album.ID, request.MemberIDs); err != nil {
					api.writeStoreError(w, err, "could not share the album")
					return
				}
			}
			album, err = api.store.Album(ctx, principal.UserID, album.ID)
			if err != nil {
				api.writeStoreError(w, err, "could not load the album")
				return
			}
			writeJSON(w, http.StatusCreated, api.albumResponse(principal, album, time.Now()))
		default:
			w.Header().Set("Allow", "GET, POST")
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return
	}
	albumID := parts[0]
	if !safeID.MatchString(albumID) || len(parts) > 3 {
		writeProblem(w, http.StatusNotFound, "not_found", "album not found")
		return
	}
	route := strings.Join(parts[1:], "/")
	switch {
	case route == "" && r.Method == http.MethodGet:
		album, err := api.store.Album(ctx, principal.UserID, albumID)
		if err != nil {
			api.writeStoreError(w, err, "could not load the album")
			return
		}
		writeJSON(w, http.StatusOK, api.albumResponse(principal, album, time.Now()))
	case route == "" && r.Method == http.MethodPatch:
		var request struct {
			Name          *string `json:"name"`
			FolderID      *string `json:"folder_id"`
			CoverAssetID  *string `json:"cover_asset_id"`
			SortOrder     *string `json:"sort_order"`
			MembersCanAdd *bool   `json:"members_can_add"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		for _, id := range []*string{request.FolderID, request.CoverAssetID} {
			if id != nil && *id != "" && !safeID.MatchString(*id) {
				writeProblem(w, http.StatusBadRequest, "invalid_request", "invalid IDs")
				return
			}
		}
		patch := AlbumPatch{Name: request.Name, FolderID: request.FolderID, CoverAssetID: request.CoverAssetID, SortOrder: request.SortOrder, MembersCanAdd: request.MembersCanAdd}
		if err := api.store.UpdateAlbum(ctx, principal.UserID, albumID, patch); err != nil {
			api.writeStoreError(w, err, "could not update the album")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case route == "" && r.Method == http.MethodDelete:
		if err := api.store.DeleteAlbum(ctx, principal.UserID, albumID); err != nil {
			api.writeStoreError(w, err, "could not delete the album")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case (route == "assets" || route == "assets/remove") && r.Method == http.MethodPost:
		var request struct {
			IDs []string `json:"ids"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if len(request.IDs) == 0 || len(request.IDs) > 1000 || !allSafe(request.IDs) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_ids", "send 1 to 1000 asset IDs")
			return
		}
		var changed int
		var err error
		if route == "assets" {
			changed, err = api.store.AddToAlbum(ctx, principal.UserID, albumID, request.IDs)
		} else {
			changed, err = api.store.RemoveFromAlbum(ctx, principal.UserID, albumID, request.IDs)
		}
		if err != nil {
			api.writeStoreError(w, err, "could not change the album")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"changed": changed})
	case route == "members" && r.Method == http.MethodPut:
		var request struct {
			UserIDs []string `json:"user_ids"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.UserIDs == nil || !allSafe(request.UserIDs) {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid_ids", "user_ids must list family members")
			return
		}
		if err := api.store.SetMembers(ctx, principal.UserID, albumID, request.UserIDs); err != nil {
			api.writeStoreError(w, err, "could not share the album")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case route == "leave" && r.Method == http.MethodPost:
		if err := api.store.LeaveAlbum(ctx, principal.UserID, albumID); err != nil {
			api.writeStoreError(w, err, "could not leave the album")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case route == "likes" && r.Method == http.MethodPost:
		var request struct {
			AssetID string `json:"asset_id"`
			Liked   bool   `json:"liked"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if !safeID.MatchString(request.AssetID) {
			writeProblem(w, http.StatusBadRequest, "invalid_request", "asset_id is required")
			return
		}
		if err := api.store.SetLike(ctx, principal.UserID, albumID, request.AssetID, request.Liked); err != nil {
			api.writeStoreError(w, err, "could not save the like")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case route == "comments" && r.Method == http.MethodGet:
		assetID := r.URL.Query().Get("asset_id")
		if !safeID.MatchString(assetID) {
			writeProblem(w, http.StatusBadRequest, "invalid_request", "asset_id is required")
			return
		}
		comments, err := api.store.Comments(ctx, principal.UserID, albumID, assetID)
		if err != nil {
			api.writeStoreError(w, err, "could not load comments")
			return
		}
		list := make([]map[string]any, 0, len(comments))
		for _, comment := range comments {
			list = append(list, commentJSON(comment))
		}
		writeJSON(w, http.StatusOK, map[string]any{"comments": list})
	case route == "comments" && r.Method == http.MethodPost:
		var request struct {
			AssetID string `json:"asset_id"`
			Body    string `json:"body"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if !safeID.MatchString(request.AssetID) {
			writeProblem(w, http.StatusBadRequest, "invalid_request", "asset_id is required")
			return
		}
		comment, err := api.store.AddComment(ctx, principal.UserID, albumID, request.AssetID, request.Body)
		if err != nil {
			api.writeStoreError(w, err, "could not save the comment")
			return
		}
		writeJSON(w, http.StatusCreated, commentJSON(comment))
	case len(parts) == 3 && parts[1] == "comments" && r.Method == http.MethodDelete:
		if !safeID.MatchString(parts[2]) {
			writeProblem(w, http.StatusNotFound, "not_found", "comment not found")
			return
		}
		if err := api.store.DeleteComment(ctx, principal.UserID, albumID, parts[2]); err != nil {
			api.writeStoreError(w, err, "could not delete the comment")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeProblem(w, http.StatusNotFound, "not_found", "not found")
	}
}

func commentJSON(comment Comment) map[string]any {
	return map[string]any{
		"id": comment.ID, "asset_id": comment.AssetID, "author_name": comment.AuthorName,
		"body": comment.Body, "created_at": comment.CreatedAt, "mine": comment.Mine,
	}
}

func (api *API) serveFolders(w http.ResponseWriter, r *http.Request, principal auth.Principal, folderID string) {
	ctx := r.Context()
	if folderID == "" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		var request struct {
			Name     string `json:"name"`
			ParentID string `json:"parent_id"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.ParentID != "" && !safeID.MatchString(request.ParentID) {
			writeProblem(w, http.StatusBadRequest, "invalid_request", "invalid parent_id")
			return
		}
		folder, err := api.store.CreateFolder(ctx, principal.UserID, request.Name, request.ParentID)
		if err != nil {
			api.writeStoreError(w, err, "could not create the folder")
			return
		}
		writeJSON(w, http.StatusCreated, folderResponse{ID: folder.ID, ParentID: folder.ParentID, Name: folder.Name, UpdatedAt: folder.UpdatedAt})
		return
	}
	if !safeID.MatchString(folderID) {
		writeProblem(w, http.StatusNotFound, "not_found", "folder not found")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var request struct {
			Name     *string `json:"name"`
			ParentID *string `json:"parent_id"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.ParentID != nil && *request.ParentID != "" && !safeID.MatchString(*request.ParentID) {
			writeProblem(w, http.StatusBadRequest, "invalid_request", "invalid parent_id")
			return
		}
		if err := api.store.UpdateFolder(ctx, principal.UserID, folderID, FolderPatch{Name: request.Name, ParentID: request.ParentID}); err != nil {
			api.writeStoreError(w, err, "could not update the folder")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := api.store.DeleteFolder(ctx, principal.UserID, folderID); err != nil {
			api.writeStoreError(w, err, "could not delete the folder")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "PATCH, DELETE")
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (api *API) servePlaces(w http.ResponseWriter, r *http.Request, principal auth.Principal, cell string) {
	ctx := r.Context()
	if cell == "" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		places, err := api.store.Places(ctx, principal.UserID)
		if err != nil {
			api.writeStoreError(w, err, "could not list places")
			return
		}
		now := time.Now()
		list := make([]map[string]any, 0, len(places))
		for _, place := range places {
			entry := map[string]any{
				"cell": place.Cell, "name": place.Name, "latitude": place.Latitude, "longitude": place.Longitude,
				"count": place.Count, "latest_taken_at": place.LatestTaken,
			}
			if api.viewTickets != nil && api.hasThumbnail(principal.UserID, place.CoverAssetID) {
				if link, err := api.thumbnailURL(principal, place.CoverAssetID, now); err == nil {
					entry["cover_url"] = link
				}
			}
			list = append(list, entry)
		}
		writeJSON(w, http.StatusOK, map[string]any{"places": list})
		return
	}
	cell, err := url.PathUnescape(cell)
	if err != nil || !placeCellPattern.MatchString(cell) {
		writeProblem(w, http.StatusNotFound, "not_found", "place not found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var request struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if err := api.store.LabelPlace(ctx, principal.UserID, cell, request.Name); err != nil {
			api.writeStoreError(w, err, "could not name the place")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := api.store.LabelPlace(ctx, principal.UserID, cell, ""); err != nil {
			api.writeStoreError(w, err, "could not rename the place")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (api *API) servePeople(w http.ResponseWriter, r *http.Request, principal auth.Principal, path string) {
	ctx := r.Context()
	switch {
	case path == "/v1/people" && r.Method == http.MethodGet:
		people, err := api.store.People(ctx, principal.UserID)
		if err != nil {
			api.writeStoreError(w, err, "could not list family members")
			return
		}
		list := make([]personResponse, 0, len(people))
		for _, person := range people {
			list = append(list, personResponse{ID: person.ID, DisplayName: person.DisplayName, Email: person.Email, Me: person.IsMe})
		}
		writeJSON(w, http.StatusOK, map[string]any{"people": list})
	case path == "/v1/people/me" && r.Method == http.MethodPut:
		var request struct {
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if err := api.store.SetDisplayName(ctx, principal.UserID, request.DisplayName); err != nil {
			api.writeStoreError(w, err, "could not save the name")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (api *API) activity(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	feed, err := api.store.Activity(r.Context(), principal.UserID, 60)
	if err != nil {
		api.writeStoreError(w, err, "could not load activity")
		return
	}
	now := time.Now()
	list := make([]map[string]any, 0, len(feed))
	for _, entry := range feed {
		var thumbnails []string
		if api.viewTickets != nil {
			for _, id := range entry.AssetIDs {
				if link, err := api.thumbnailURL(principal, id, now); err == nil {
					thumbnails = append(thumbnails, link)
				}
			}
		}
		list = append(list, map[string]any{
			"kind": entry.Kind, "album_id": entry.AlbumID, "album_name": entry.AlbumName,
			"actor_name": entry.ActorName, "asset_ids": entry.AssetIDs, "count": entry.Count,
			"body": entry.Body, "at": entry.At, "thumbnail_urls": thumbnails,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity": list})
}
