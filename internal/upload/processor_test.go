package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

func TestProcessorDeduplicatesDifferentExtensionsToOneCanonicalObject(t *testing.T) {
	t.Parallel()
	content := []byte("same immutable bytes with different client filenames")
	hash := sha256.Sum256(content)
	repository := NewMemoryRepository()
	processor, first := prepareReceivedSession(t, repository, content, hash, "photo.jpg")
	if err := processor.Process(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	second, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID:          testOwnerID,
		ClientAssetID:    "same-bytes-different-extension",
		OriginalFilename: "photo.png",
		MediaType:        "image/png",
		ExpectedSize:     int64(len(content)),
		ClientSHA256:     hash,
		ExpiresAt:        time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(context.Background(), second.ID, second.OwnerID, second.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), second.ID), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkReceived(context.Background(), second.ID, second.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := processor.Process(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	first, err = repository.SessionByID(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = repository.SessionByID(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.FinalStorageKey != second.FinalStorageKey || first.AssetID == "" || first.AssetID != second.AssetID {
		t.Fatalf("duplicate sessions did not share one canonical asset: first=%#v second=%#v", first, second)
	}
	if filepath.Ext(first.FinalStorageKey) != "" {
		t.Fatalf("content-addressed storage key unexpectedly has extension: %q", first.FinalStorageKey)
	}
	assets, err := repository.ListAssets(context.Background(), testOwnerID, nil, 10)
	if err != nil || len(assets) != 1 {
		t.Fatalf("dedup assets=%#v err=%v", assets, err)
	}
}

func TestProcessorExpiresIncompleteSessionAndPermitsSameClientRetry(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	processor, err := NewProcessor(repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("future upload"))
	session, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID: testOwnerID, ClientAssetID: "retry-after-expiry", OriginalFilename: "retry.jpg",
		MediaType: "image/jpeg", ExpectedSize: 12, ClientSHA256: hash, ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(context.Background(), session.ID, session.OwnerID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	partial := []byte("partial")
	if err := repository.RecordProgress(context.Background(), session.ID, int64(len(partial))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID), partial, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTusSidecar(t, processor, session.ID, session.ExpectedSize, int64(len(partial)))
	expired, err := repository.ExpiredSessions(context.Background(), time.Now(), 10)
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired sessions=%#v err=%v", expired, err)
	}
	if err := processor.Expire(context.Background(), expired[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(processor.StagingDirectory(), session.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired staging data remained: %v", err)
	}
	retried, created, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID: testOwnerID, ClientAssetID: "retry-after-expiry", OriginalFilename: "retry.jpg",
		MediaType: "image/jpeg", ExpectedSize: 12, ClientSHA256: hash, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil || created || retried.ID != session.ID || retried.State != StateCreated {
		t.Fatalf("expired client retry=%#v created=%v err=%v", retried, created, err)
	}
}

func TestCompletedTusUploadsFindsDurableFinalByteAfterEventLoss(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	processor, err := NewProcessor(repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("final PATCH persisted before gateway notification")
	hash := sha256.Sum256(content)
	session, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID: testOwnerID, ClientAssetID: "crash-after-final-patch", OriginalFilename: "final.mov",
		MediaType: "video/quicktime", ExpectedSize: int64(len(content)), ClientSHA256: hash,
		ExpiresAt: time.Now().Add(time.Hour),
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
	sidecar := []byte(`{"ID":"` + session.ID + `","Size":` + fmt.Sprint(len(content)) + `,"Offset":` + fmt.Sprint(len(content)) + `}`)
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID+".info"), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), "corrupt.info"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	completed, err := processor.CompletedTusUploads()
	if err != nil || len(completed) != 1 || completed[0].ID != session.ID || completed[0].Offset != int64(len(content)) {
		t.Fatalf("durable completed uploads=%#v err=%v", completed, err)
	}
	if _, err := os.Stat(filepath.Join(processor.mediaRoot, ".quarantine", "corrupt.info.bad-info")); err != nil {
		t.Fatalf("corrupt sidecar was not isolated: %v", err)
	}
	if err := repository.MarkReceived(context.Background(), completed[0].ID, completed[0].Offset); err != nil {
		t.Fatal(err)
	}
	if err := processor.Process(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	got, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil || got.State != StateAvailable {
		t.Fatalf("completed upload was not recovered: %#v err=%v", got, err)
	}
}

func TestResetForRetryRemovesOnlyIncompleteTusResource(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	processor, err := NewProcessor(repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("resettable bytes"))
	session, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID: testOwnerID, ClientAssetID: "lost-tus-context", OriginalFilename: "retry.mov",
		MediaType: "video/quicktime", ExpectedSize: 100, ClientSHA256: hash, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(context.Background(), session.ID, session.OwnerID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := repository.RecordProgress(context.Background(), session.ID, 25); err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, 25)
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID), partial, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTusSidecar(t, processor, session.ID, session.ExpectedSize, 25)
	if _, err := processor.ResetForRetry(context.Background(), session.ID, testOwnerID); err != nil {
		t.Fatal(err)
	}
	got, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil || got.State != StateCreated || got.ReceivedSize != 0 {
		t.Fatalf("reset session=%#v err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(processor.StagingDirectory(), session.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset staging data remained: %v", err)
	}
}

func TestVerificationFenceRejectsStaleWorkerTransition(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	content := []byte("fenced verification")
	processor, session := prepareReceivedSession(t, repository, content, sha256.Sum256(content), "fence.mov")
	claimed, err := repository.ClaimVerification(context.Background(), "worker-a", time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	stale := WithVerificationFence(context.Background(), claimed[0].VerificationClaim)
	repository.mu.Lock()
	current := repository.sessions[session.ID]
	current.VerificationClaim = "newer-claim"
	repository.sessions[session.ID] = current
	repository.mu.Unlock()
	if err := processor.Process(stale, session.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale verifier transition err=%v, want invalid state", err)
	}
}

func TestExpireReconcilesDurablyCompleteTusResource(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	processor, err := NewProcessor(repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("complete bytes persisted by the final PATCH")
	hash := sha256.Sum256(content)
	session, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID: testOwnerID, ClientAssetID: "expiry-final-patch", OriginalFilename: "final.mov",
		MediaType: "video/quicktime", ExpectedSize: int64(len(content)), ClientSHA256: hash,
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(context.Background(), session.ID, session.OwnerID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := repository.RecordProgress(context.Background(), session.ID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID), content, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTusSidecar(t, processor, session.ID, session.ExpectedSize, session.ExpectedSize)

	stale, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Expire(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	got, err := repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReceived {
		t.Fatalf("state=%s want received", got.State)
	}
	if _, err := os.Stat(filepath.Join(processor.StagingDirectory(), session.ID)); err != nil {
		t.Fatalf("complete payload was deleted: %v", err)
	}
}

func TestResetForRetryReconcilesDurablyCompleteTusResource(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	processor, err := NewProcessor(repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("complete retry bytes")
	hash := sha256.Sum256(content)
	session, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID: testOwnerID, ClientAssetID: "restart-final-patch", OriginalFilename: "final.jpg",
		MediaType: "image/jpeg", ExpectedSize: int64(len(content)), ClientSHA256: hash,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(context.Background(), session.ID, session.OwnerID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := repository.RecordProgress(context.Background(), session.ID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), session.ID), content, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTusSidecar(t, processor, session.ID, session.ExpectedSize, session.ExpectedSize)

	got, err := processor.ResetForRetry(context.Background(), session.ID, testOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReceived {
		t.Fatalf("state=%s want received", got.State)
	}
	if _, err := os.Stat(filepath.Join(processor.StagingDirectory(), session.ID)); err != nil {
		t.Fatalf("complete payload was deleted: %v", err)
	}
}

func TestExpirePreservesInconsistentTusResource(t *testing.T) {
	t.Parallel()
	repository := NewMemoryRepository()
	processor, err := NewProcessor(repository, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("expected"))
	session, _, err := repository.CreateSession(context.Background(), CreateSessionInput{
		OwnerID: testOwnerID, ClientAssetID: "bad-sidecar", OriginalFilename: "bad.jpg",
		MediaType: "image/jpeg", ExpectedSize: 8, ClientSHA256: hash, ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ClaimTusCreation(context.Background(), session.ID, session.OwnerID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(processor.StagingDirectory(), session.ID)
	if err := os.WriteFile(payload, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payload+".info", []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = processor.Expire(context.Background(), session)
	if !errors.Is(err, ErrUploadResourceInconsistent) {
		t.Fatalf("err=%v want inconsistent", err)
	}
	if _, statErr := os.Stat(payload); statErr != nil {
		t.Fatalf("inconsistent payload was deleted: %v", statErr)
	}
}

func writeTusSidecar(t *testing.T, processor *Processor, id string, size, offset int64) {
	t.Helper()
	sidecar := []byte(fmt.Sprintf(`{"ID":%q,"Size":%d,"Offset":%d}`, id, size, offset))
	if err := os.WriteFile(filepath.Join(processor.StagingDirectory(), id+".info"), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateSessionThrottleCountsOnlyNewOrRestartedIdentities(t *testing.T) {
	repository := NewMemoryRepository()
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	makeInput := func(clientID string) CreateSessionInput {
		return CreateSessionInput{
			OwnerID:             testOwnerID,
			ClientAssetID:       clientID,
			OriginalFilename:    clientID + ".jpg",
			MediaType:           "image/jpeg",
			ExpectedSize:        10,
			ClientSHA256:        sha256.Sum256([]byte(clientID)),
			ExpiresAt:           now.Add(time.Hour),
			Now:                 now,
			AvailableBytes:      1 << 30,
			MinimumFreeBytes:    0,
			MaxActiveSessions:   100,
			CreateWindow:        time.Minute,
			MaxCreatesPerWindow: 2,
		}
	}

	first, created, err := repository.CreateSession(context.Background(), makeInput("first"))
	if err != nil || !created {
		t.Fatalf("first creation: created=%v err=%v", created, err)
	}
	if retried, created, err := repository.CreateSession(context.Background(), makeInput("first")); err != nil || created || retried.ID != first.ID {
		t.Fatalf("idempotent retry consumed admission: created=%v session=%s err=%v", created, retried.ID, err)
	}
	if _, created, err := repository.CreateSession(context.Background(), makeInput("second")); err != nil || !created {
		t.Fatalf("second creation: created=%v err=%v", created, err)
	}
	if _, _, err := repository.CreateSession(context.Background(), makeInput("third")); !errors.Is(err, ErrCreateRateLimit) {
		t.Fatalf("third creation err=%v, want create rate limit", err)
	}
	if err := repository.MarkExpired(context.Background(), first.ID); err != nil {
		t.Fatalf("expire first session: %v", err)
	}
	if _, _, err := repository.CreateSession(context.Background(), makeInput("first")); !errors.Is(err, ErrCreateRateLimit) {
		t.Fatalf("expired identity restart err=%v, want create rate limit", err)
	}

	afterWindow := makeInput("third")
	afterWindow.Now = now.Add(time.Minute)
	if _, created, err := repository.CreateSession(context.Background(), afterWindow); err != nil || !created {
		t.Fatalf("creation after window: created=%v err=%v", created, err)
	}

	activeRepository := NewMemoryRepository()
	expiredInput := makeInput("expired")
	expiredInput.MaxCreatesPerWindow = 0
	expiredInput.MaxActiveSessions = 10
	expired, _, err := activeRepository.CreateSession(context.Background(), expiredInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := activeRepository.MarkExpired(context.Background(), expired.ID); err != nil {
		t.Fatal(err)
	}
	blockerInput := makeInput("blocker")
	blockerInput.MaxCreatesPerWindow = 0
	blockerInput.MaxActiveSessions = 10
	if _, _, err := activeRepository.CreateSession(context.Background(), blockerInput); err != nil {
		t.Fatal(err)
	}
	restartInput := makeInput("expired")
	restartInput.MaxCreatesPerWindow = 0
	restartInput.MaxActiveSessions = 1
	if _, _, err := activeRepository.CreateSession(context.Background(), restartInput); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("expired identity restart active-cap err=%v, want session limit", err)
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
	writeTusSidecar(t, processor, session.ID, session.ExpectedSize, session.ExpectedSize)
	if err := repository.MarkReceived(context.Background(), session.ID, session.ExpectedSize); err != nil {
		t.Fatal(err)
	}
	session, err = repository.SessionByID(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	return processor, session
}
