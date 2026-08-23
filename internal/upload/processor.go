package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

type Processor struct {
	repository Repository
	mediaRoot  string
}

// CompletedTusUpload is reconstructed from tusd's durable FileInfo sidecar.
// It closes the crash window between tusd acknowledging the final PATCH and
// the gateway consuming its in-memory CompleteUploads notification.
type CompletedTusUpload struct {
	ID     string
	Offset int64
}

type tusFileInfo struct {
	ID     string `json:"ID"`
	Size   int64  `json:"Size"`
	Offset int64  `json:"Offset"`
}

func NewProcessor(repository Repository, mediaRoot string) (*Processor, error) {
	absolute, err := filepath.Abs(mediaRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve media root: %w", err)
	}
	for _, directory := range []string{
		filepath.Join(absolute, ".staging", "tus"),
		filepath.Join(absolute, ".quarantine"),
		filepath.Join(absolute, "originals"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create media directory: %w", err)
		}
	}
	return &Processor{repository: repository, mediaRoot: absolute}, nil
}

func (p *Processor) StagingDirectory() string {
	return filepath.Join(p.mediaRoot, ".staging", "tus")
}

func (p *Processor) CompletedTusUploads() ([]CompletedTusUpload, error) {
	entries, err := os.ReadDir(p.StagingDirectory())
	if err != nil {
		return nil, fmt.Errorf("read tus staging directory: %w", err)
	}
	completed := make([]CompletedTusUpload, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".info") {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(p.StagingDirectory(), entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read tus sidecar %q: %w", entry.Name(), err)
		}
		var info tusFileInfo
		if err := json.Unmarshal(contents, &info); err != nil {
			return nil, fmt.Errorf("decode tus sidecar %q: %w", entry.Name(), err)
		}
		if !safeID.MatchString(info.ID) || info.ID+".info" != entry.Name() || info.Size < 0 || info.Offset != info.Size {
			continue
		}
		stat, err := os.Stat(filepath.Join(p.StagingDirectory(), info.ID))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat completed tus object %q: %w", info.ID, err)
		}
		if stat.Size() != info.Size {
			continue
		}
		completed = append(completed, CompletedTusUpload{ID: info.ID, Offset: info.Offset})
	}
	return completed, nil
}

func (p *Processor) Process(ctx context.Context, id string) error {
	session, err := p.repository.BeginVerification(ctx, id)
	if err != nil {
		return err
	}
	if session.State == StateAvailable {
		return nil
	}
	if !safeID.MatchString(session.ID) || !safeID.MatchString(session.OwnerID) {
		return p.fail(ctx, session.ID, "unsafe_storage_identifier", errors.New("unsafe storage identifier"))
	}

	storageKey := finalStorageKey(session)
	stagePath := filepath.Join(p.StagingDirectory(), session.ID)
	finalPath := filepath.Join(p.mediaRoot, filepath.FromSlash(storageKey))
	sourcePath := stagePath
	if _, statErr := os.Stat(stagePath); errors.Is(statErr, os.ErrNotExist) {
		if _, finalErr := os.Stat(finalPath); finalErr == nil {
			sourcePath = finalPath
		} else if errors.Is(finalErr, os.ErrNotExist) {
			return p.recoverQuarantine(ctx, session)
		} else {
			return p.fail(ctx, session.ID, "destination_stat_failed", finalErr)
		}
	} else if statErr != nil {
		return p.fail(ctx, session.ID, "staging_stat_failed", statErr)
	}

	observedHash, observedSize, err := hashAndSync(sourcePath)
	if err != nil {
		return p.fail(ctx, session.ID, "content_read_failed", err)
	}
	if observedSize != session.ExpectedSize || !bytes.Equal(observedHash[:], session.ClientSHA256[:]) {
		quarantinePath := filepath.Join(
			p.mediaRoot, ".quarantine", session.ID+"."+hex.EncodeToString(observedHash[:8])+".bad",
		)
		if err := moveWithoutReplace(sourcePath, quarantinePath); err != nil {
			return p.fail(ctx, session.ID, "quarantine_move_failed", err)
		}
		if err := syncDirectory(filepath.Dir(quarantinePath)); err != nil {
			return p.fail(ctx, session.ID, "quarantine_sync_failed", err)
		}
		if err := p.repository.MarkQuarantined(ctx, session.ID, observedHash, "sha256_or_size_mismatch"); err != nil {
			return err
		}
		return ErrChecksumMismatch
	}

	if session.State == StateVerifying {
		if err := p.repository.MarkVerified(ctx, session.ID, observedHash); err != nil {
			return err
		}
	}
	if err := p.repository.MarkCommitting(ctx, session.ID, storageKey); err != nil {
		return err
	}

	if sourcePath != finalPath {
		if err := commitWithoutReplace(sourcePath, finalPath, observedHash, observedSize); err != nil {
			// Stay COMMITTING. Startup reconciliation can safely retry because
			// commitWithoutReplace never replaces an existing destination.
			return fmt.Errorf("durable_commit_move_failed: %w", err)
		}
	}
	if err := syncDirectory(filepath.Dir(finalPath)); err != nil {
		return fmt.Errorf("durable_commit_sync_failed: %w", err)
	}
	if err := p.repository.MarkAvailable(ctx, session.ID, storageKey, observedHash); err != nil {
		return err
	}

	// The TUS sidecar is no longer authoritative after the database commit.
	// Removing it only after MarkAvailable preserves crash recovery.
	infoPath := filepath.Join(p.StagingDirectory(), session.ID+".info")
	if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove completed TUS sidecar: %w", err)
	}
	_ = syncDirectory(p.StagingDirectory())
	return nil
}

func (p *Processor) recoverQuarantine(ctx context.Context, session Session) error {
	matches, err := filepath.Glob(filepath.Join(p.mediaRoot, ".quarantine", session.ID+".*.bad"))
	if err != nil || len(matches) != 1 {
		if err == nil {
			err = errors.New("staging, destination, and unique quarantine object are missing")
		}
		return p.fail(ctx, session.ID, "content_missing", err)
	}
	hash, size, err := hashAndSync(matches[0])
	if err != nil {
		return p.fail(ctx, session.ID, "quarantine_read_failed", err)
	}
	if size == session.ExpectedSize && bytes.Equal(hash[:], session.ClientSHA256[:]) {
		return p.fail(ctx, session.ID, "unexpected_valid_quarantine", errors.New("quarantine object matches expected content"))
	}
	if err := p.repository.MarkQuarantined(ctx, session.ID, hash, "sha256_or_size_mismatch"); err != nil {
		return err
	}
	return ErrChecksumMismatch
}

func (p *Processor) fail(ctx context.Context, id, code string, cause error) error {
	if err := p.repository.MarkFailed(ctx, id, code); err != nil {
		return fmt.Errorf("%s: %v (record failure: %w)", code, cause, err)
	}
	return fmt.Errorf("%s: %w", code, cause)
}

func finalStorageKey(session Session) string {
	hash := hex.EncodeToString(session.ClientSHA256[:])
	// Physical object identity is content-addressed and intentionally does not
	// contain a client-supplied extension. Database uniqueness is per owner and
	// digest, so extension-based paths would otherwise create orphan duplicates.
	return filepath.ToSlash(filepath.Join("originals", session.OwnerID, hash[:2], hash))
}

func safeExtension(filename string) string {
	extension := strings.ToLower(filepath.Ext(filename))
	if len(extension) < 2 || len(extension) > 11 {
		return ""
	}
	for _, character := range extension[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return ""
		}
	}
	return extension
}

// Expire removes only incomplete staging data, then records the terminal
// state. If a process dies in either half, the periodic sweeper retries safely.
func (p *Processor) Expire(ctx context.Context, session Session) error {
	for _, path := range []string{
		filepath.Join(p.StagingDirectory(), session.ID),
		filepath.Join(p.StagingDirectory(), session.ID+".info"),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove expired staging data: %w", err)
		}
	}
	if err := syncDirectory(p.StagingDirectory()); err != nil {
		return fmt.Errorf("sync expired staging cleanup: %w", err)
	}
	return p.repository.MarkExpired(ctx, session.ID)
}

// ResetForRetry is used only when an authenticated device has lost its local
// TUSKit resume context. State is first moved away from `uploading`, causing
// the gateway to reject any stale PATCH, then the old server resource is
// removed so a deterministic TUS POST can create it again safely.
func (p *Processor) ResetForRetry(ctx context.Context, id, ownerID string) (Session, error) {
	session, err := p.repository.ResetForRetry(ctx, id, ownerID)
	if err != nil {
		return Session{}, err
	}
	for _, path := range []string{
		filepath.Join(p.StagingDirectory(), session.ID),
		filepath.Join(p.StagingDirectory(), session.ID+".info"),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Session{}, fmt.Errorf("remove reset staging data: %w", err)
		}
	}
	if err := syncDirectory(p.StagingDirectory()); err != nil {
		return Session{}, fmt.Errorf("sync reset staging cleanup: %w", err)
	}
	return session, nil
}

func hashAndSync(path string) ([32]byte, int64, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return [32]byte{}, size, err
	}
	if err := file.Sync(); err != nil {
		return [32]byte{}, size, err
	}
	var result [32]byte
	copy(result[:], hasher.Sum(nil))
	return result, size, nil
}

func moveWithoutReplace(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		return errors.New("quarantine destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, destination)
}

func commitWithoutReplace(source, destination string, expectedHash [32]byte, expectedSize int64) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.Link(source, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create no-replace durable link: %w", err)
		}
		hash, size, verifyErr := hashAndSync(destination)
		if verifyErr != nil {
			return fmt.Errorf("verify existing destination: %w", verifyErr)
		}
		if size != expectedSize || !bytes.Equal(hash[:], expectedHash[:]) {
			return errors.New("existing destination failed integrity verification")
		}
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(source))
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
