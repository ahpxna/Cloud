package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"family-photo-cloud/internal/account"
	"family-photo-cloud/internal/auth"
	"family-photo-cloud/internal/gateway"
	"family-photo-cloud/internal/upload"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("upload gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	key, err := decodeSecret(os.Getenv("ACCESS_TOKEN_HMAC_KEY_BASE64"))
	if err != nil {
		return err
	}
	tokens, err := auth.NewAccessTokenManager(key, auth.DefaultIssuer, auth.DefaultAudience)
	if err != nil {
		return err
	}

	startupContext, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()
	databaseURL := os.Getenv("DATABASE_URL")
	pool, err := pgxpool.New(startupContext, databaseURL)
	if err != nil {
		return fmt.Errorf("configure PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(startupContext); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}

	maxUploadBytes, err := envInt64("TUS_MAX_UPLOAD_BYTES", 20<<30)
	if err != nil {
		return err
	}
	chunkBytes, err := envInt64("TUS_MVP_CHUNK_BYTES", 32<<20)
	if err != nil {
		return err
	}
	verificationJobs, err := envInt("VERIFICATION_WORKERS", 2)
	if err != nil {
		return err
	}
	maxConcurrentPatches, err := envInt("MAX_CONCURRENT_PATCHES", 6)
	if err != nil {
		return err
	}
	maxPatchesPerUser, err := envInt("MAX_PATCHES_PER_USER", 2)
	if err != nil {
		return err
	}
	repository := upload.NewPostgresRepository(pool)
	application, err := gateway.New(gateway.Config{
		Repository:           repository,
		Accounts:             account.NewPostgresRepository(pool),
		Tokens:               tokens,
		MediaRoot:            envString("PHOTO_MEDIA_ROOT", "/srv/media"),
		MaxUploadBytes:       maxUploadBytes,
		ChunkBytes:           chunkBytes,
		VerificationJobs:     verificationJobs,
		MaxConcurrentPatches: maxConcurrentPatches,
		MaxPatchesPerUser:    maxPatchesPerUser,
		Logger:               logger,
	})
	if err != nil {
		return err
	}
	defer application.Close()

	server := &http.Server{
		Addr:              envString("HTTP_ADDR", "127.0.0.1:8080"),
		Handler:           application,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       envDuration("HTTP_READ_TIMEOUT", 20*time.Minute),
		WriteTimeout:      envDuration("HTTP_WRITE_TIMEOUT", 30*time.Minute),
		IdleTimeout:       envDuration("HTTP_IDLE_TIMEOUT", 2*time.Minute),
		MaxHeaderBytes:    64 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("upload gateway listening", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	signals, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	select {
	case <-signals.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func decodeSecret(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("ACCESS_TOKEN_HMAC_KEY_BASE64 is required")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("ACCESS_TOKEN_HMAC_KEY_BASE64 must be standard base64")
	}
	if len(key) < 32 {
		return nil, errors.New("decoded access-token key must contain at least 32 bytes")
	}
	return key, nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value, err := envInt64(name, int64(fallback))
	return int(value), err
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
