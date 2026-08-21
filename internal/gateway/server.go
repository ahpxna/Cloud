package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"family-photo-cloud/internal/account"
	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/library"
	"family-photo-cloud/internal/upload"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

const tusBasePath = "/v1/uploads/"

type Config struct {
	Repository           upload.Repository
	Accounts             account.Repository
	Tokens               *auth.AccessTokenManager
	MediaRoot            string
	MaxUploadBytes       int64
	ChunkBytes           int64
	VerificationJobs     int
	MaxConcurrentPatches int
	MaxPatchesPerUser    int
	Logger               *slog.Logger
}

type Server struct {
	handler http.Handler
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func New(config Config) (*Server, error) {
	if config.Repository == nil || config.Tokens == nil {
		return nil, errors.New("repository and token manager are required")
	}
	if config.MaxUploadBytes <= 0 || config.ChunkBytes <= 0 {
		return nil, errors.New("positive upload and chunk limits are required")
	}
	if config.VerificationJobs <= 0 {
		config.VerificationJobs = 2
	}
	if config.MaxConcurrentPatches <= 0 {
		config.MaxConcurrentPatches = 6
	}
	if config.MaxPatchesPerUser <= 0 {
		config.MaxPatchesPerUser = 2
	}
	if config.MaxPatchesPerUser > config.MaxConcurrentPatches {
		return nil, errors.New("per-user PATCH limit cannot exceed global limit")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}

	processor, err := upload.NewProcessor(config.Repository, config.MediaRoot)
	if err != nil {
		return nil, err
	}
	store := filestore.New(processor.StagingDirectory())
	store.DirModePerm = 0o700
	store.FileModePerm = 0o600
	locker := filelocker.New(processor.StagingDirectory())
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)
	locker.UseIn(composer)

	tusHandler, err := tusd.NewHandler(tusd.Config{
		StoreComposer:          composer,
		BasePath:               tusBasePath,
		MaxSize:                config.MaxUploadBytes,
		DisableDownload:        true,
		DisableTermination:     true,
		DisableConcatenation:   true,
		Cors:                   &tusd.CorsConfig{Disable: true},
		NotifyCompleteUploads:  true,
		NotifyUploadProgress:   true,
		UploadProgressInterval: time.Second,
		PreUploadCreateCallback: func(event tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
			principal, ok := auth.PrincipalFrom(event.Context)
			if !ok {
				return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError(
					"ERR_PRODUCT_UNAUTHORIZED", "access token required", http.StatusUnauthorized,
				)
			}
			sessionID := event.Upload.MetaData["session_id"]
			if sessionID == "" || strings.Contains(sessionID, "/") {
				return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError(
					"ERR_PRODUCT_SESSION", "valid upload session metadata required", http.StatusForbidden,
				)
			}
			if principal.UploadID != "" && principal.UploadID != sessionID {
				return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError(
					"ERR_PRODUCT_SESSION", "upload capability does not match session", http.StatusForbidden,
				)
			}
			session, err := config.Repository.SessionByID(event.Context, sessionID)
			if err != nil || subtle.ConstantTimeCompare([]byte(session.OwnerID), []byte(principal.UserID)) != 1 ||
				time.Now().After(session.ExpiresAt) {
				return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError(
					"ERR_PRODUCT_SESSION", "upload session is unavailable", http.StatusForbidden,
				)
			}
			if err := config.Repository.ClaimTusCreation(event.Context, sessionID, principal.UserID, event.Upload.Size); err != nil {
				return tusd.HTTPResponse{}, tusd.FileInfoChanges{}, tusd.NewError(
					"ERR_PRODUCT_SESSION_STATE", "upload session cannot be claimed", http.StatusConflict,
				)
			}
			return tusd.HTTPResponse{}, tusd.FileInfoChanges{
				ID: sessionID,
				MetaData: tusd.MetaData{
					"session_id": sessionID,
				},
			}, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create tus handler: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{cancel: cancel}
	queue := make(chan string, config.VerificationJobs*4)

	server.wg.Add(3)
	go func() {
		defer server.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-tusHandler.UploadProgress:
				if err := config.Repository.RecordProgress(ctx, event.Upload.ID, event.Upload.Offset); err != nil &&
					!errors.Is(err, upload.ErrInvalidState) {
					config.Logger.Error("record upload progress", "upload_id", event.Upload.ID, "error", err)
				}
			}
		}
	}()
	go func() {
		defer server.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-tusHandler.CompleteUploads:
				if err := config.Repository.MarkReceived(ctx, event.Upload.ID, event.Upload.Offset); err != nil &&
					!errors.Is(err, upload.ErrInvalidState) {
					config.Logger.Error("mark upload received", "upload_id", event.Upload.ID, "error", err)
					continue
				}
				select {
				case queue <- event.Upload.ID:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() {
		defer server.wg.Done()
		pending, err := config.Repository.PendingVerification(ctx, 1000)
		if err != nil {
			config.Logger.Error("load verification recovery queue", "error", err)
			return
		}
		for _, session := range pending {
			select {
			case queue <- session.ID:
			case <-ctx.Done():
				return
			}
		}
	}()
	for range config.VerificationJobs {
		server.wg.Add(1)
		go func() {
			defer server.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-queue:
					if err := processor.Process(ctx, id); err != nil && !errors.Is(err, upload.ErrChecksumMismatch) {
						config.Logger.Error("verify and commit upload", "upload_id", id, "error", err)
					}
				}
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	uploadAPI := upload.NewAPI(config.Repository, config.MaxUploadBytes, config.ChunkBytes, config.Tokens)
	if config.Accounts != nil {
		accountAPI := account.NewAPI(config.Accounts, config.Tokens, config.Logger)
		mux.Handle("/v1/auth/", accountAPI)
	}
	assetRepository, ok := config.Repository.(upload.AssetRepository)
	if !ok {
		return nil, errors.New("repository must implement asset reads")
	}
	libraryAPI, err := library.NewAPI(assetRepository, config.MediaRoot)
	if err != nil {
		return nil, err
	}
	mux.Handle("/v1/assets", authenticate(config.Tokens, libraryAPI))
	mux.Handle("/v1/assets/", authenticate(config.Tokens, libraryAPI))
	mux.Handle("/v1/upload-sessions", authenticate(config.Tokens, uploadAPI))
	mux.Handle("/v1/upload-sessions/", authenticate(config.Tokens, uploadAPI))

	strippedTus := http.StripPrefix(strings.TrimSuffix(tusBasePath, "/"), tusHandler)
	limiter := newPatchLimiter(config.MaxConcurrentPatches, config.MaxPatchesPerUser)
	protectedTus := authenticateTus(config.Tokens, config.Repository, limiter, config.ChunkBytes, strippedTus)
	mux.Handle(strings.TrimSuffix(tusBasePath, "/"), protectedTus)
	mux.Handle(tusBasePath, protectedTus)
	server.handler = securityHeaders(mux)
	return server, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

func authenticate(tokens *auth.AccessTokenManager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := authenticateHeader(tokens, r.Header.Get("Authorization"))
		if !ok {
			writeAuthError(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	})
}

func authenticateTus(
	tokens *auth.AccessTokenManager,
	repository upload.Repository,
	limiter *patchLimiter,
	maxPatchBytes int64,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		principal, ok := authenticateTusHeader(tokens, r.Header.Get("Authorization"))
		if !ok {
			writeAuthError(w)
			return
		}

		resourceID := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(tusBasePath, "/"))
		resourceID = strings.Trim(resourceID, "/")
		if principal.UploadID != "" && resourceID != "" && principal.UploadID != resourceID {
			http.NotFound(w, r)
			return
		}
		if resourceID != "" {
			if strings.Contains(resourceID, "/") {
				http.NotFound(w, r)
				return
			}
			session, err := repository.SessionByID(r.Context(), resourceID)
			if err != nil || subtle.ConstantTimeCompare([]byte(session.OwnerID), []byte(principal.UserID)) != 1 {
				http.NotFound(w, r)
				return
			}
		}
		if r.Method == http.MethodPatch {
			if r.ContentLength < 0 {
				http.Error(w, "PATCH Content-Length is required", http.StatusLengthRequired)
				return
			}
			if r.ContentLength > maxPatchBytes {
				http.Error(w, "upload chunk exceeds configured limit", http.StatusRequestEntityTooLarge)
				return
			}
			if !limiter.Acquire(principal.UserID) {
				w.Header().Set("Retry-After", "2")
				http.Error(w, "too many concurrent upload chunks", http.StatusTooManyRequests)
				return
			}
			defer limiter.Release(principal.UserID)
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	})
}

type patchLimiter struct {
	global  chan struct{}
	perUser int
	mu      sync.Mutex
	users   map[string]int
}

func newPatchLimiter(global, perUser int) *patchLimiter {
	return &patchLimiter{global: make(chan struct{}, global), perUser: perUser, users: make(map[string]int)}
}

func (limiter *patchLimiter) Acquire(userID string) bool {
	limiter.mu.Lock()
	if limiter.users[userID] >= limiter.perUser {
		limiter.mu.Unlock()
		return false
	}
	limiter.users[userID]++
	limiter.mu.Unlock()

	select {
	case limiter.global <- struct{}{}:
		return true
	default:
		limiter.ReleaseUser(userID)
		return false
	}
}

func (limiter *patchLimiter) Release(userID string) {
	<-limiter.global
	limiter.ReleaseUser(userID)
}

func (limiter *patchLimiter) ReleaseUser(userID string) {
	limiter.mu.Lock()
	limiter.users[userID]--
	if limiter.users[userID] == 0 {
		delete(limiter.users, userID)
	}
	limiter.mu.Unlock()
}

func authenticateHeader(tokens *auth.AccessTokenManager, header string) (auth.Principal, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return auth.Principal{}, false
	}
	principal, err := tokens.Verify(parts[1])
	return principal, err == nil
}

func authenticateTusHeader(tokens *auth.AccessTokenManager, header string) (auth.Principal, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return auth.Principal{}, false
	}
	if principal, err := tokens.VerifyUpload(parts[1]); err == nil {
		return principal, true
	}
	principal, err := tokens.Verify(parts[1])
	return principal, err == nil
}

func writeAuthError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="family-photo-cloud"`)
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("{\"status\":401,\"code\":\"unauthorized\",\"detail\":\"valid access token required\"}\n"))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
