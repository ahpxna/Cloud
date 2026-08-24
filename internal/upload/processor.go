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

// TusResourceState is derived only from tusd's durable payload and sidecar.
// It is intentionally independent of the database state so expiry/restart can
// reconcile the final PATCH crash window before deleting any bytes.
type TusResourceState int

const (
	TusAbsent TusResourceState = iota
	TusIncomplete
	TusComplete
	TusInconsistent
)

type TusResourceInspection struct {
	State  TusResourceState
	Offset int64
	Size   int64
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

func (p *Processor) InspectTusResource(id string, expectedSize int64) (TusResourceInspection, error) {
	if !safeID.MatchString(id) || expectedSize < 0 {
		return TusResourceInspection{State: TusInconsistent}, ErrUploadResourceInconsistent
	}
	payloadPath := filepath.Join(p.StagingDirectory(), id)
	infoPath := payloadPath + ".info"
	payloadStat, payloadErr := os.Stat(payloadPath)
	infoStat, infoErr := os.Stat(infoPath)
	payloadMissing := errors.Is(payloadErr, os.ErrNotExist)
	infoMissing := errors.Is(infoErr, os.ErrNotExist)
	if payloadErr != nil && !payloadMissing {
		return TusResourceInspection{}, fmt.Errorf("stat tus payload: %w", payloadErr)
	}
	if infoErr != nil && !infoMissing {
		return TusResourceInspection{}, fmt.Errorf("stat tus sidecar: %w", infoErr)
	}
	if payloadMissing && infoMissing {
		return TusResourceInspection{State: TusAbsent}, nil
	}
	if payloadMissing || infoMissing || !payloadStat.Mode().IsRegular() || !infoStat.Mode().IsRegular() {
		return TusResourceInspection{State: TusInconsistent}, nil
	}

	contents, err := os.ReadFile(infoPath)
	if err != nil {
		return TusResourceInspection{}, fmt.Errorf("read tus sidecar: %w", err)
	}
	var info tusFileInfo
	if err := json.Unmarshal(contents, &info); err != nil {
		return TusResourceInspection{State: TusInconsistent}, nil
	}
	inspection := TusResourceInspection{Offset: info.Offset, Size: info.Size}
	if info.ID != id || info.Size != expectedSize || info.Size < 0 || info.Offset < 0 || info.Offset > info.Size {
		inspection.State = TusInconsistent
		return inspection, nil
	}
	if payloadStat.Size() != info.Offset {
		inspection.State = TusInconsistent
		return inspection, nil
	}
	if info.Offset == info.Size {
		inspection.State = TusComplete
		return inspection, nil
	}
	inspection.State = TusIncomplete
	return inspection, nil
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
			p.quarantineBadSidecar(entry.Name())
			continue
		}
		var info tusFileInfo
		if err := json.Unmarshal(contents, &info); err != nil {
			p.quarantineBadSidecar(entry.Name())
			continue
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

func (p *Processor) quarantineBadSidecar(name string) {
	source := filepath.Join(p.StagingDirectory(), name)
	destination := filepath.Join(p.mediaRoot, ".quarantine", name+".bad-info")
	if _, err := os.Stat(destination); err == nil {
		return
	}
	if err := os.Rename(source, destination); err == nil {
		_ = syncDirectory(filepath.Dir(destination))
	}
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

	observedHash, observedSize, err := hashAndSync(ctx, sourcePath)
	if err != nil {
		return p.fail(ctx, session.ID, "content_read_failed", err)
	}
	if observedSize != session.ExpectedSize || !bytes.Equal(observedHash[:], session.ClientSHA256[:]) {
		if session.State != StateQuarantining {
			if err := p.repository.MarkQuarantineIntent(ctx, session.ID, observedHash, "sha256_or_size_mismatch"); err != nil {
				return err
			}
		}
		quarantinePath := filepath.Join(
			p.mediaRoot, ".quarantine", session.ID+"."+hex.EncodeToString(observedHash[:8])+".bad",
		)
		if err := quarantineWithoutReplace(ctx, sourcePath, quarantinePath, observedHash, observedSize); err != nil {
			// Keep state=quarantining and the verifier fence. Reconciliation can
			// safely retry the idempotent filesystem mutation.
			return fmt.Errorf("quarantine_move_failed: %w", err)
		}
		if err := syncDirectory(filepath.Dir(quarantinePath)); err != nil {
			return fmt.Errorf("quarantine_sync_failed: %w", err)
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
		if session.State == StateQuarantining {
			return fmt.Errorf("quarantine_recovery_missing: %w", err)
		}
		return p.fail(ctx, session.ID, "content_missing", err)
	}
	hash, size, err := hashAndSync(ctx, matches[0])
	if err != nil {
		return fmt.Errorf("quarantine_read_failed: %w", err)
	}
	if size == session.ExpectedSize && bytes.Equal(hash[:], session.ClientSHA256[:]) {
		return fmt.Errorf("unexpected_valid_quarantine: %w", ErrInvalidState)
	}
	if session.State != StateQuarantining {
		if err := p.repository.MarkQuarantineIntent(ctx, session.ID, hash, "sha256_or_size_mismatch"); err != nil {
			return err
		}
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

// Expire deletes only a durably incomplete TUS resource. A complete final
// PATCH wins over stale database expiry state; inconsistent metadata is
// preserved for operator/reconciliation inspection rather than destroyed.
func (p *Processor) Expire(ctx context.Context, session Session) error {
	inspection, err := p.InspectTusResource(session.ID, session.ExpectedSize)
	if err != nil {
		return err
	}
	switch inspection.State {
	case TusComplete:
		if err := p.repository.MarkReceived(ctx, session.ID, inspection.Offset); err != nil {
			if errors.Is(err, ErrInvalidState) {
				current, loadErr := p.repository.SessionByID(ctx, session.ID)
				if loadErr == nil && current.State != StateCreated && current.State != StateUploading && current.State != StateFailed {
					return nil
				}
			}
			return err
		}
		return nil
	case TusInconsistent:
		return ErrUploadResourceInconsistent
	case TusAbsent, TusIncomplete:
		if session.ReceivedSize >= session.ExpectedSize {
			return ErrUploadResourceInconsistent
		}
		// Safe to remove below.
	default:
		return ErrUploadResourceInconsistent
	}
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
// TUS resume context. The durable TUS resource is inspected before the database
// is reset, so a complete final PATCH can never be deleted by a stale client.
func (p *Processor) ResetForRetry(ctx context.Context, id, ownerID string) (Session, error) {
	session, err := p.repository.SessionByID(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if session.OwnerID != ownerID {
		return Session{}, ErrOwnerMismatch
	}
	if session.State != StateCreated && session.State != StateUploading {
		return Session{}, ErrInvalidState
	}
	inspection, err := p.InspectTusResource(session.ID, session.ExpectedSize)
	if err != nil {
		return Session{}, err
	}
	switch inspection.State {
	case TusComplete:
		if session.State != StateUploading {
			return Session{}, ErrUploadResourceInconsistent
		}
		if err := p.repository.MarkReceived(ctx, session.ID, inspection.Offset); err != nil {
			return Session{}, err
		}
		return p.repository.SessionByID(ctx, session.ID)
	case TusInconsistent:
		return Session{}, ErrUploadResourceInconsistent
	case TusAbsent, TusIncomplete:
		// A database offset claiming the complete object is authoritative enough
		// to prevent destructive reset, even when the TUS sidecar/payload no
		// longer agrees. Preserve the evidence for reconciliation instead.
		if session.ReceivedSize >= session.ExpectedSize {
			return Session{}, ErrUploadResourceInconsistent
		}
		// Safe to reset below.
	default:
		return Session{}, ErrUploadResourceInconsistent
	}

	reset, err := p.repository.ResetForRetry(ctx, id, ownerID)
	if err != nil {
		return Session{}, err
	}
	for _, path := range []string{
		filepath.Join(p.StagingDirectory(), reset.ID),
		filepath.Join(p.StagingDirectory(), reset.ID+".info"),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Session{}, fmt.Errorf("remove reset staging data: %w", err)
		}
	}
	if err := syncDirectory(p.StagingDirectory()); err != nil {
		return Session{}, fmt.Errorf("sync reset staging cleanup: %w", err)
	}
	return reset, nil
}

func hashAndSync(ctx context.Context, path string) ([32]byte, int64, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, size, err
		}
		read, err := file.Read(buffer)
		if read > 0 {
			if _, writeErr := hasher.Write(buffer[:read]); writeErr != nil {
				return [32]byte{}, size, writeErr
			}
			size += int64(read)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return [32]byte{}, size, err
		}
	}
	if err := file.Sync(); err != nil {
		return [32]byte{}, size, err
	}
	var result [32]byte
	copy(result[:], hasher.Sum(nil))
	return result, size, nil
}

func quarantineWithoutReplace(ctx context.Context, source, destination string, expectedHash [32]byte, expectedSize int64) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	sourceStat, sourceErr := os.Stat(source)
	destinationStat, destinationErr := os.Stat(destination)
	sourceExists := sourceErr == nil
	destinationExists := destinationErr == nil
	if sourceErr != nil && !errors.Is(sourceErr, os.ErrNotExist) {
		return sourceErr
	}
	if destinationErr != nil && !errors.Is(destinationErr, os.ErrNotExist) {
		return destinationErr
	}
	if !sourceExists && !destinationExists {
		return errors.New("quarantine source and destination are both absent")
	}
	verifyDestination := func() error {
		hash, size, err := hashAndSync(ctx, destination)
		if err != nil {
			return err
		}
		if size != expectedSize || !bytes.Equal(hash[:], expectedHash[:]) {
			return errors.New("existing quarantine destination failed integrity verification")
		}
		return nil
	}
	if destinationExists {
		if !destinationStat.Mode().IsRegular() {
			return errors.New("quarantine destination is not a regular file")
		}
		if err := verifyDestination(); err != nil {
			return err
		}
		if sourceExists {
			if !sourceStat.Mode().IsRegular() {
				return errors.New("quarantine source is not a regular file")
			}
			hash, size, err := hashAndSync(ctx, source)
			if err != nil {
				return err
			}
			if size != expectedSize || !bytes.Equal(hash[:], expectedHash[:]) {
				return errors.New("quarantine source changed after intent")
			}
			if err := os.Remove(source); err != nil {
				return err
			}
			return syncDirectory(filepath.Dir(source))
		}
		return nil
	}
	if !sourceStat.Mode().IsRegular() {
		return errors.New("quarantine source is not a regular file")
	}
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	return verifyDestination()
}

func commitWithoutReplace(source, destination string, expectedHash [32]byte, expectedSize int64) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.Link(source, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create no-replace durable link: %w", err)
		}
		hash, size, verifyErr := hashAndSync(context.Background(), destination)
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
