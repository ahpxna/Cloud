package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testOwnerID = "10000000-0000-4000-8000-000000000001"

func TestProcessorVerifiesAndCommitsOriginal(t *testing.T) {
	t.Parallel()
	content := []byte("original photo bytes that must remain unchanged")
	repository := NewMemoryRepository()
	processor, session := prepareReceivedSession(t, repository, content, sha256.Sum256(content), "IMG_0001.HEIC")

	if err := processor.Process(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	got, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateAvailable || got.ServerSHA256 == nil || *got.ServerSHA256 != got.ClientSHA256 {
		t.Fatalf("unexpected committed session: %#v", got)
	}
	finalPath := filepath.Join(processor.mediaRoot, filepath.FromSlash(got.FinalStorageKey))
	committed, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(content) {
		t.Fatal("committed bytes changed")
	}
	if _, err := os.Stat(filepath.Join(processor.StagingDirectory(), session.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging data was not removed: %v", err)
	}
}

func TestProcessorQuarantinesChecksumMismatch(t *testing.T) {
	t.Parallel()
	content := []byte("damaged bytes")
	expected := sha256.Sum256([]byte("expected original bytes"))
	repository := NewMemoryRepository()
	processor, session := prepareReceivedSession(t, repository, content, expected, "IMG_0002.JPG")

	err := processor.Process(context.Background(), session.ID)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("got %v, want checksum mismatch", err)
	}
	got, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateQuarantined {
		t.Fatalf("state = %s, want quarantined", got.State)
	}
	matches, err := filepath.Glob(filepath.Join(processor.mediaRoot, ".quarantine", session.ID+".*.bad"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("quarantine matches = %v", matches)
	}
}

func TestProcessorRecoversWhenVerifiedDestinationAlreadyExists(t *testing.T) {
	t.Parallel()
	content := []byte("content from a commit interrupted before database update")
	hash := sha256.Sum256(content)
	repository := NewMemoryRepository()
	processor, session := prepareReceivedSession(t, repository, content, hash, "video.MOV")
	storageKey := finalStorageKey(session)
	destination := filepath.Join(processor.mediaRoot, filepath.FromSlash(storageKey))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, content, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := processor.Process(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	got, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateAvailable || got.FinalStorageKey != storageKey {
		t.Fatalf("recovery did not finish: %#v", got)
	}
}

func TestProcessorRecoversQuarantineAfterDatabaseInterruption(t *testing.T) {
	t.Parallel()
	content := []byte("bytes that do not match the client digest")
	expected := sha256.Sum256([]byte("different expected bytes"))
	repository := NewMemoryRepository()
	processor, session := prepareReceivedSession(t, repository, content, expected, "damaged.jpg")
	observed := sha256.Sum256(content)
	quarantinePath := filepath.Join(
		processor.mediaRoot, ".quarantine", session.ID+"."+hex.EncodeToString(observed[:8])+".bad",
	)
	if err := os.Rename(filepath.Join(processor.StagingDirectory(), session.ID), quarantinePath); err != nil {
		t.Fatal(err)
	}

	err := processor.Process(context.Background(), session.ID)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("got %v, want recovered checksum mismatch", err)
	}
	got, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateQuarantined || got.ServerSHA256 == nil || *got.ServerSHA256 != observed {
		t.Fatalf("quarantine recovery did not persist observation: %#v", got)
	}
}

func prepareReceivedSession(
	t *testing.T,
	repository *MemoryRepository,
	content []byte,
	expectedHash [32]byte,
	filename string,
) (*Processor, Session) {
	t.Helper()
	processor, err := NewProcessor(repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID:          testOwnerID,
		ClientAssetID:    filename,
		OriginalFilename: filename,
		MediaType:        "image/jpeg",
		ExpectedSize:     int64(len(content)),
		ClientSHA256:     expectedHash,
		ExpiresAt:        time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(context.Background(), session.ID, session.OwnerID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID+".info"), []byte("test sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkReceived(context.Background(), session.ID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	session, err = repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	return processor, session
}
