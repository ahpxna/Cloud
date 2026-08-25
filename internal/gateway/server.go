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
	Repository                upload.Repository
	Accounts                  account.Repository
	Tokens                    *auth.AccessTokenManager
	MediaRoot                 string
	MaxUploadBytes            int64
	ChunkBytes                int64
	VerificationJobs          int
	MaxConcurrentPatches      int
	MaxPatchesPerUser         int
	MinimumFreeBytes          int64
	ReconcileInterval         time.Duration
	VerificationLease         time.Duration
	MaxActiveUploadSessions   int
	UploadSessionCreateWindow time.Duration
	MaxUploadCreatesPerWindow int
	LoginThrottleHMACKey      []byte
	GlobalLoginRatePerSecond  float64
	GlobalLoginBurst          int
	MFAEncryptionKey          []byte
	Logger                    *slog.Logger
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
	if config.MinimumFreeBytes < 0 {
		return nil, errors.New("minimum free bytes cannot be negative")
	}
	if config.ReconcileInterval <= 0 {
		config.ReconcileInterval = time.Minute
	}
	if config.VerificationLease <= 0 {
		config.VerificationLease = 10 * time.Minute
	}
	if config.MaxActiveUploadSessions <= 0 {
		config.MaxActiveUploadSessions = 200
	}
	if config.UploadSessionCreateWindow <= 0 {
		config.UploadSessionCreateWindow = time.Minute
	}
	if config.MaxUploadCreatesPerWindow <= 0 {
		config.MaxUploadCreatesPerWindow = 30
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
				time.Now().After(session.ExpiresAt) || session.State == upload.StateExpired {
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
	// Workers claim one job only when they are about to process it. Never lease
	// a backlog into RAM: queued work must remain claimable in PostgreSQL.
	wakeWorkers := make(chan struct{}, config.VerificationJobs)
	workerID, err := upload.NewID()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create verifier identity: %w", err)
	}
	workerID = "gateway-" + workerID
	resourceLocks := newResourceLocks()
	wake := func() {
		for range config.VerificationJobs {
			select {
			case wakeWorkers <- struct{}{}:
			default:
				return
			}
		}
	}

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
				wake()
			}
		}
	}()
	recoverCompletedTus := func() {
		completed, err := processor.CompletedTusUploads()
		if err != nil {
			config.Logger.Error("scan completed tus uploads", "error", err)
			return
		}
		for _, durable := range completed {
			session, err := config.Repository.SessionByID(ctx, durable.ID)
			if err != nil {
				config.Logger.Error("load completed tus upload", "upload_id", durable.ID, "error", err)
				continue
			}
			if session.State != upload.StateUploading {
				continue
			}
			if err := config.Repository.MarkReceived(ctx, durable.ID, durable.Offset); err != nil && !errors.Is(err, upload.ErrInvalidState) {
				config.Logger.Error("recover completed tus upload", "upload_id", durable.ID, "error", err)
			}
		}
	}
	expireStale := func() {
		for {
			expired, err := config.Repository.ExpiredSessions(ctx, time.Now().UTC(), 100)
			if err != nil {
				config.Logger.Error("load expired upload sessions", "error", err)
				return
			}
			for _, session := range expired {
				unlock := resourceLocks.lock(session.ID)
				err := processor.Expire(ctx, session)
				unlock()
				if err != nil && !errors.Is(err, upload.ErrInvalidState) {
					config.Logger.Error("expire stale upload", "upload_id", session.ID, "error", err)
				}
			}
			if len(expired) < 100 {
				return
			}
		}
	}
	go func() {
		defer server.wg.Done()
		recoverCompletedTus()
		wake()
		expireStale()
		ticker := time.NewTicker(config.ReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				recoverCompletedTus()
				wake()
				expireStale()
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
				case <-wakeWorkers:
					for {
						claimed, err := config.Repository.ClaimVerification(ctx, workerID, config.VerificationLease, 1)
						if err != nil {
							config.Logger.Error("claim verification job", "error", err)
							break
						}
						if len(claimed) == 0 {
							break
						}
						if err := processWithLease(ctx, config.Repository, processor, claimed[0], workerID, config.VerificationLease, config.Logger); err != nil && !errors.Is(err, upload.ErrChecksumMismatch) {
							config.Logger.Error("verify and commit upload", "upload_id", claimed[0].ID, "error", err)
						}
					}
				}
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	readyHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		available, err := upload.AvailableBytes(config.MediaRoot)()
		if err != nil || available < config.MinimumFreeBytes || upload.CheckWritable(config.MediaRoot) != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("{\"status\":\"not_ready\"}\n"))
			return
		}
		readyContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := config.Repository.PendingVerification(readyContext, 1); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("{\"status\":\"not_ready\"}\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ready\"}\n"))
	})
	mux.Handle("GET /readyz", readyHandler)
	// Compatibility alias for local probes; readiness is deliberately stricter
	// than liveness and is the endpoint used by Compose.
	mux.Handle("GET /healthz", readyHandler)
	uploadAPI := upload.NewAPI(config.Repository, config.MaxUploadBytes, config.ChunkBytes, config.Tokens, upload.AvailableBytes(config.MediaRoot), config.MinimumFreeBytes, config.MaxActiveUploadSessions, config.UploadSessionCreateWindow, config.MaxUploadCreatesPerWindow, func(requestContext context.Context, id, ownerID string) (upload.Session, error) {
		unlock := resourceLocks.lock(id)
		defer unlock()
		return processor.ResetForRetry(requestContext, id, ownerID)
	})
	if config.Accounts != nil {
		accountAPI, err := account.NewSecureAPI(config.Accounts, config.Tokens, config.Logger, account.SecurityConfig{
			LoginThrottleHMACKey: config.LoginThrottleHMACKey,
			GlobalLoginRate:      config.GlobalLoginRatePerSecond,
			GlobalLoginBurst:     config.GlobalLoginBurst,
			MFAEncryptionKey:     config.MFAEncryptionKey,
		})
		if err != nil {
			return nil, fmt.Errorf("configure account security: %w", err)
		}
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
	mux.Handle("/v1/assets", authenticate(config.Tokens, config.Accounts, libraryAPI))
	mux.Handle("/v1/assets/", authenticate(config.Tokens, config.Accounts, libraryAPI))
	mux.Handle("/v1/upload-sessions", authenticate(config.Tokens, config.Accounts, uploadAPI))
	mux.Handle("/v1/upload-sessions/", authenticate(config.Tokens, config.Accounts, uploadAPI))

	strippedTus := http.StripPrefix(strings.TrimSuffix(tusBasePath, "/"), tusHandler)
	limiter := newPatchLimiter(config.MaxConcurrentPatches, config.MaxPatchesPerUser)
	protectedTus := lockPatches(resourceLocks, authenticateTus(config.Tokens, config.Accounts, config.Repository, limiter, config.ChunkBytes, strippedTus))
	mux.Handle(strings.TrimSuffix(tusBasePath, "/"), protectedTus)
	mux.Handle(tusBasePath, protectedTus)
	server.handler = securityHeaders(mux)
	return server, nil
}

type resourceLocks struct {
	mu      sync.Mutex
	entries map[string]*resourceLock
}
type resourceLock struct {
	mu   sync.Mutex
	refs int
}

func newResourceLocks() *resourceLocks {
	return &resourceLocks{entries: make(map[string]*resourceLock)}
}
func (locks *resourceLocks) lock(id string) func() {
	locks.mu.Lock()
	entry := locks.entries[id]
	if entry == nil {
		entry = &resourceLock{}
		locks.entries[id] = entry
	}
	entry.refs++
	locks.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.entries, id)
		}
		locks.mu.Unlock()
	}
}

func lockPatches(locks *resourceLocks, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			next.ServeHTTP(w, r)
			return
		}
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(tusBasePath, "/")), "/")
		if id == "" || strings.Contains(id, "/") {
			next.ServeHTTP(w, r)
			return
		}
		unlock := locks.lock(id)
		defer unlock()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

func processWithLease(
	ctx context.Context,
	repository upload.Repository,
	processor *upload.Processor,
	session upload.Session, workerID string,
	lease time.Duration,
	logger *slog.Logger,
) error {
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	interval := lease / 3
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-processContext.Done():
				return
			case <-ticker.C:
				if err := repository.RenewVerificationLease(processContext, session.ID, workerID, session.VerificationClaim, lease); err != nil {
					logger.Error("renew verification lease", "upload_id", session.ID, "error", err)
					// Continuing after ownership is lost risks two workers committing
					// the same staging object. The durable state remains reclaimable.
					cancel()
					return
				}
			}
		}
	}()
	err := processor.Process(upload.WithVerificationFence(processContext, session.VerificationClaim), session.ID)
	close(done)
	heartbeat.Wait()
	return err
}

func authenticate(tokens *auth.AccessTokenManager, accounts account.Repository, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := authenticateHeader(tokens, r.Header.Get("Authorization"))
		if !ok {
			writeAuthError(w)
			return
		}
		if accounts != nil {
			active, err := accounts.SessionActive(r.Context(), principal.UserID, principal.SessionID)
			if err != nil || !active {
				writeAuthError(w)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	})
}

func authenticateTus(
	tokens *auth.AccessTokenManager,
	accounts account.Repository,
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
		if accounts != nil {
			active, err := accounts.SessionActive(r.Context(), principal.UserID, principal.SessionID)
			if err != nil || !active {
				writeAuthError(w)
				return
			}
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
			if time.Now().After(session.ExpiresAt) || session.State == upload.StateExpired {
				http.Error(w, "upload session expired", http.StatusGone)
				return
			}
			if r.Method == http.MethodPatch && session.State != upload.StateUploading {
				http.Error(w, "upload session is not accepting chunks", http.StatusConflict)
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
	principal, err := tokens.VerifyUpload(parts[1])
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
