// Package integrity defines the signed inventory format used to detect
// unrecorded deletion or modification of committed originals. It deliberately
// has no database dependency so both Go services and an iOS verifier can
// implement the same compact canonical payload.
package integrity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"
)

const ManifestVersion = 1

type AssetRecord struct {
	AssetID       string    `json:"asset_id"`
	OwnerID       string    `json:"owner_id"`
	StorageKey    string    `json:"storage_key"`
	ByteSize      int64     `json:"byte_size"`
	ContentSHA256 string    `json:"content_sha256"`
	VerifiedAt    time.Time `json:"verified_at"`
}

// Manifest's JSON is transport/storage only. The Ed25519 signature is over
// CanonicalPayload, not the JSON bytes, so independent Swift and Go encoders
// cannot alter signature semantics.
type Manifest struct {
	ManifestVersion int           `json:"manifest_version"`
	GeneratedAt     time.Time     `json:"generated_at"`
	SigningKeyID    string        `json:"signing_key_id"`
	Records         []AssetRecord `json:"records"`
	Signature       string        `json:"signature"`
}

func NewManifest(generatedAt time.Time, signingKeyID string, records []AssetRecord) (Manifest, error) {
	manifest := Manifest{
		ManifestVersion: ManifestVersion,
		GeneratedAt:     generatedAt.UTC(),
		SigningKeyID:    signingKeyID,
		Records:         append([]AssetRecord(nil), records...),
	}
	sortRecords(manifest.Records)
	return manifest, manifest.Validate()
}

func (m Manifest) Validate() error {
	if m.ManifestVersion != ManifestVersion {
		return fmt.Errorf("unsupported manifest version %d", m.ManifestVersion)
	}
	if m.GeneratedAt.IsZero() || m.SigningKeyID == "" || len(m.SigningKeyID) > 255 || !utf8.ValidString(m.SigningKeyID) {
		return errors.New("generated_at and a valid signing_key_id are required")
	}
	seen := make(map[string]struct{}, len(m.Records))
	for _, record := range m.Records {
		if err := validateRecord(record); err != nil {
			return err
		}
		if _, exists := seen[record.AssetID]; exists {
			return fmt.Errorf("duplicate asset ID %q", record.AssetID)
		}
		seen[record.AssetID] = struct{}{}
	}
	return nil
}

func (m Manifest) CanonicalPayload() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	records := append([]AssetRecord(nil), m.Records...)
	sortRecords(records)

	var payload bytes.Buffer
	payload.WriteString("family-photo-cloud.asset-manifest/1\n")
	writeField(&payload, "manifest_version", strconv.Itoa(m.ManifestVersion))
	writeField(&payload, "generated_at", m.GeneratedAt.UTC().Format(time.RFC3339Nano))
	writeField(&payload, "signing_key_id", m.SigningKeyID)
	writeField(&payload, "record_count", strconv.Itoa(len(records)))
	for _, record := range records {
		payload.WriteString("record\n")
		writeField(&payload, "asset_id", record.AssetID)
		writeField(&payload, "owner_id", record.OwnerID)
		writeField(&payload, "storage_key", record.StorageKey)
		writeField(&payload, "byte_size", strconv.FormatInt(record.ByteSize, 10))
		writeField(&payload, "content_sha256", record.ContentSHA256)
		writeField(&payload, "verified_at", record.VerifiedAt.UTC().Format(time.RFC3339Nano))
	}
	return payload.Bytes(), nil
}

func (m *Manifest) Sign(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid Ed25519 private key")
	}
	payload, err := m.CanonicalPayload()
	if err != nil {
		return err
	}
	sortRecords(m.Records)
	m.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return nil
}

func (m Manifest) Verify(publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid Ed25519 public key")
	}
	if m.Signature == "" {
		return errors.New("manifest signature is missing")
	}
	signature, err := base64.RawStdEncoding.Strict().DecodeString(m.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("manifest signature is invalid")
	}
	payload, err := m.CanonicalPayload()
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return errors.New("manifest signature verification failed")
	}
	return nil
}

func writeField(payload *bytes.Buffer, name, value string) {
	payload.WriteString(name)
	payload.WriteByte(' ')
	payload.WriteString(strconv.Itoa(len(value)))
	payload.WriteByte(':')
	payload.WriteString(value)
	payload.WriteByte('\n')
}

func sortRecords(records []AssetRecord) {
	sort.Slice(records, func(left, right int) bool {
		if records[left].OwnerID == records[right].OwnerID {
			return records[left].AssetID < records[right].AssetID
		}
		return records[left].OwnerID < records[right].OwnerID
	})
}

func validateRecord(record AssetRecord) error {
	if record.AssetID == "" || record.OwnerID == "" || record.StorageKey == "" ||
		!utf8.ValidString(record.AssetID) || !utf8.ValidString(record.OwnerID) || !utf8.ValidString(record.StorageKey) ||
		record.ByteSize < 0 || record.VerifiedAt.IsZero() {
		return errors.New("asset record has invalid required fields")
	}
	hash, err := hex.DecodeString(record.ContentSHA256)
	if err != nil || len(hash) != 32 {
		return errors.New("asset record has invalid SHA-256")
	}
	return nil
}
