// Command manifest signs, verifies, and reconciles immutable point-in-time
// inventories of verified originals. The private signing key is needed only in
// sign mode; verification/reconciliation use an independent public trust key.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"family-photo-cloud/internal/integrity"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	mode := flags.String("mode", "sign", "operation: sign, verify, or reconcile")
	outputPath := flags.String("output", "", "filesystem path for signed JSON output")
	inputPath := flags.String("input", "", "signed manifest JSON path for verify/reconcile")
	objectKey := flags.String("object-key", "", "stable storage key recorded in PostgreSQL")
	expectedVersion := flags.Int("expected-version", 0, "optional manifest version assertion")
	expectedAssetCount := flags.Int64("expected-asset-count", -1, "optional asset count assertion")
	expectedPayloadHash := flags.String("expected-payload-sha256", "", "optional canonical payload SHA-256 hex assertion")
	expectedKeyID := flags.String("expected-key-id", "", "optional signing key ID assertion")
	expectedSignatureHex := flags.String("expected-signature-hex", "", "optional Ed25519 signature hex assertion")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}

	switch *mode {
	case "sign":
		return signManifest(*outputPath, *objectKey)
	case "verify":
		return verifyManifestFile(*inputPath, manifestExpectations{
			Version:      *expectedVersion,
			AssetCount:   *expectedAssetCount,
			PayloadHash:  *expectedPayloadHash,
			SigningKeyID: *expectedKeyID,
			SignatureHex: *expectedSignatureHex,
		})
	case "reconcile":
		return reconcileManifestFile(*inputPath, *objectKey)
	default:
		return fmt.Errorf("unknown mode %q; expected sign, verify, or reconcile", *mode)
	}
}

func signManifest(outputPath, objectKey string) error {
	if outputPath == "" || objectKey == "" {
		return errors.New("sign mode requires -output /secure/path/manifest.json and -object-key manifests/manifest.json")
	}
	if err := validateManifestObjectKey(objectKey); err != nil {
		return err
	}
	privateKey, err := privateKeyFromEnvironment()
	if err != nil {
		return err
	}
	keyID := os.Getenv("MANIFEST_SIGNING_KEY_ID")
	if keyID == "" {
		return errors.New("MANIFEST_SIGNING_KEY_ID is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := manifestDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	var alreadyRecorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM signed_manifests WHERE object_key = $1)`, objectKey).Scan(&alreadyRecorded); err != nil {
		return fmt.Errorf("check manifest object key: %w", err)
	}
	if alreadyRecorded {
		return fmt.Errorf("manifest object key %q is already recorded; manifests are immutable", objectKey)
	}

	records, err := loadRecords(ctx, pool)
	if err != nil {
		return err
	}
	manifest, err := integrity.NewManifest(time.Now().UTC(), keyID, records)
	if err != nil {
		return err
	}
	if err := manifest.Sign(privateKey); err != nil {
		return err
	}
	payload, err := manifest.CanonicalPayload()
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := writeNoReplaceAtomically(outputPath, encoded); err != nil {
		return err
	}
	signature, err := base64.RawStdEncoding.Strict().DecodeString(manifest.Signature)
	if err != nil {
		return fmt.Errorf("decode generated signature: %w", err)
	}
	payloadHash := sha256.Sum256(payload)
	_, err = pool.Exec(ctx, `
        INSERT INTO signed_manifests (
            manifest_version, object_key, asset_count, payload_sha256,
            signing_key_id, signature
        ) VALUES ($1, $2, $3, $4, $5, $6)`,
		manifest.ManifestVersion, objectKey, len(manifest.Records), payloadHash[:],
		manifest.SigningKeyID, signature,
	)
	if err != nil {
		return fmt.Errorf("record signed manifest after writing %s: %w", outputPath, err)
	}
	fmt.Printf("wrote signed manifest %s (%d verified assets, key %s)\n", outputPath, len(records), keyID)
	return nil
}

type manifestExpectations struct {
	Version      int
	AssetCount   int64
	PayloadHash  string
	SigningKeyID string
	SignatureHex string
}

type verifiedManifest struct {
	Manifest    integrity.Manifest
	PayloadHash [32]byte
	Signature   []byte
}

func verifyManifestFile(inputPath string, expected manifestExpectations) error {
	verified, err := loadAndVerifyManifest(inputPath)
	if err != nil {
		return err
	}
	if err := assertManifestExpectations(verified, expected); err != nil {
		return err
	}
	fmt.Printf(
		"verified signed manifest %s (%d assets, key %s, payload_sha256=%x)\n",
		inputPath, len(verified.Manifest.Records), verified.Manifest.SigningKeyID, verified.PayloadHash,
	)
	return nil
}

func reconcileManifestFile(inputPath, objectKey string) error {
	if inputPath == "" || objectKey == "" {
		return errors.New("reconcile mode requires -input /path/manifest.json and -object-key manifests/manifest.json")
	}
	if err := validateManifestObjectKey(objectKey); err != nil {
		return err
	}
	verified, err := loadAndVerifyManifest(inputPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := manifestDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	var version int
	var count int64
	var payloadHash, signature []byte
	var keyID string
	err = pool.QueryRow(ctx, `
        SELECT manifest_version, asset_count, payload_sha256, signing_key_id, signature
        FROM signed_manifests WHERE object_key = $1`, objectKey,
	).Scan(&version, &count, &payloadHash, &keyID, &signature)
	if err == nil {
		expected := manifestExpectations{
			Version:      version,
			AssetCount:   count,
			PayloadHash:  hex.EncodeToString(payloadHash),
			SigningKeyID: keyID,
			SignatureHex: hex.EncodeToString(signature),
		}
		if err := assertManifestExpectations(verified, expected); err != nil {
			return fmt.Errorf("database linkage for %q disagrees with signed file: %w", objectKey, err)
		}
		fmt.Printf("manifest %s is already reconciled as %s\n", inputPath, objectKey)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read manifest linkage: %w", err)
	}
	_, err = pool.Exec(ctx, `
        INSERT INTO signed_manifests (
            manifest_version, object_key, asset_count, payload_sha256,
            signing_key_id, signature
        ) VALUES ($1, $2, $3, $4, $5, $6)`,
		verified.Manifest.ManifestVersion, objectKey, len(verified.Manifest.Records), verified.PayloadHash[:],
		verified.Manifest.SigningKeyID, verified.Signature,
	)
	if err != nil {
		return fmt.Errorf("reconcile signed manifest %q: %w", objectKey, err)
	}
	fmt.Printf("reconciled signed manifest %s as %s\n", inputPath, objectKey)
	return nil
}

func loadAndVerifyManifest(inputPath string) (verifiedManifest, error) {
	if inputPath == "" {
		return verifiedManifest{}, errors.New("verify/reconcile mode requires -input /path/manifest.json")
	}
	encoded, err := os.ReadFile(inputPath)
	if err != nil {
		return verifiedManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifest integrity.Manifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return verifiedManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return verifiedManifest{}, errors.New("manifest contains trailing JSON values")
	}
	publicKey, err := publicKeyFromEnvironment()
	if err != nil {
		return verifiedManifest{}, err
	}
	if err := manifest.Verify(publicKey); err != nil {
		return verifiedManifest{}, err
	}
	payload, err := manifest.CanonicalPayload()
	if err != nil {
		return verifiedManifest{}, err
	}
	signature, err := base64.RawStdEncoding.Strict().DecodeString(manifest.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return verifiedManifest{}, errors.New("manifest signature is invalid")
	}
	return verifiedManifest{Manifest: manifest, PayloadHash: sha256.Sum256(payload), Signature: signature}, nil
}

func assertManifestExpectations(verified verifiedManifest, expected manifestExpectations) error {
	if expected.Version > 0 && verified.Manifest.ManifestVersion != expected.Version {
		return fmt.Errorf("manifest version=%d want=%d", verified.Manifest.ManifestVersion, expected.Version)
	}
	if expected.AssetCount >= 0 && int64(len(verified.Manifest.Records)) != expected.AssetCount {
		return fmt.Errorf("asset count=%d want=%d", len(verified.Manifest.Records), expected.AssetCount)
	}
	if expected.SigningKeyID != "" && verified.Manifest.SigningKeyID != expected.SigningKeyID {
		return fmt.Errorf("signing key ID=%q want=%q", verified.Manifest.SigningKeyID, expected.SigningKeyID)
	}
	if expected.PayloadHash != "" {
		decoded, err := hex.DecodeString(strings.TrimSpace(expected.PayloadHash))
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("expected payload SHA-256 must be 64 hex characters")
		}
		if !bytes.Equal(decoded, verified.PayloadHash[:]) {
			return fmt.Errorf("payload SHA-256=%x want=%s", verified.PayloadHash, expected.PayloadHash)
		}
	}
	if expected.SignatureHex != "" {
		decoded, err := hex.DecodeString(strings.TrimSpace(expected.SignatureHex))
		if err != nil || len(decoded) != ed25519.SignatureSize {
			return errors.New("expected signature must be 128 hex characters")
		}
		if !bytes.Equal(decoded, verified.Signature) {
			return errors.New("manifest signature bytes do not match database linkage")
		}
	}
	return nil
}

func validateManifestObjectKey(objectKey string) error {
	if !strings.HasPrefix(objectKey, "manifests/") {
		return errors.New("manifest object key must begin with manifests/")
	}
	relative := strings.TrimPrefix(objectKey, "manifests/")
	if relative == "" || relative == "." || relative == ".." || strings.Contains(relative, "/") || filepath.Base(relative) != relative {
		return errors.New("manifest object key must identify one file directly under manifests/")
	}
	return nil
}

func manifestDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, fmt.Errorf("configure PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	return pool, nil
}

func loadRecords(ctx context.Context, pool *pgxpool.Pool) ([]integrity.AssetRecord, error) {
	rows, err := pool.Query(ctx, `
        SELECT asset.id::text, asset.owner_id::text, asset.storage_key,
               asset.byte_size, encode(asset.content_sha256, 'hex'),
               COALESCE(upload.verified_at, asset.created_at)
        FROM assets AS asset
        JOIN upload_sessions AS upload ON upload.id = asset.upload_session_id
        WHERE asset.deleted_at IS NULL AND upload.state = 'available'
        ORDER BY asset.owner_id, asset.id`)
	if err != nil {
		return nil, fmt.Errorf("read asset inventory: %w", err)
	}
	defer rows.Close()
	records := make([]integrity.AssetRecord, 0)
	for rows.Next() {
		var record integrity.AssetRecord
		if err := rows.Scan(
			&record.AssetID, &record.OwnerID, &record.StorageKey,
			&record.ByteSize, &record.ContentSHA256, &record.VerifiedAt,
		); err != nil {
			return nil, fmt.Errorf("scan asset inventory: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func privateKeyFromEnvironment() (ed25519.PrivateKey, error) {
	keyPath := os.Getenv("MANIFEST_ED25519_PRIVATE_KEY_FILE")
	raw := os.Getenv("MANIFEST_ED25519_PRIVATE_KEY_BASE64")
	if keyPath != "" && raw != "" {
		return nil, errors.New("set only one of MANIFEST_ED25519_PRIVATE_KEY_FILE or MANIFEST_ED25519_PRIVATE_KEY_BASE64")
	}
	if keyPath != "" {
		encoded, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read manifest signing key: %w", err)
		}
		block, _ := pem.Decode(encoded)
		if block == nil {
			return nil, errors.New("manifest signing key is not PEM")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse manifest signing key: %w", err)
		}
		privateKey, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("manifest signing key is not Ed25519")
		}
		return privateKey, nil
	}
	if raw == "" {
		return nil, errors.New("MANIFEST_ED25519_PRIVATE_KEY_FILE or MANIFEST_ED25519_PRIVATE_KEY_BASE64 is required")
	}
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(raw)
	if err != nil {
		return nil, errors.New("MANIFEST_ED25519_PRIVATE_KEY_BASE64 must be unpadded standard base64")
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, errors.New("MANIFEST_ED25519_PRIVATE_KEY_BASE64 does not contain an Ed25519 private key")
	}
	return ed25519.PrivateKey(decoded), nil
}

func publicKeyFromEnvironment() (ed25519.PublicKey, error) {
	keyPath := os.Getenv("MANIFEST_ED25519_PUBLIC_KEY_FILE")
	raw := os.Getenv("MANIFEST_ED25519_PUBLIC_KEY_BASE64")
	if keyPath != "" && raw != "" {
		return nil, errors.New("set only one of MANIFEST_ED25519_PUBLIC_KEY_FILE or MANIFEST_ED25519_PUBLIC_KEY_BASE64")
	}
	if keyPath != "" {
		encoded, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read manifest verification key: %w", err)
		}
		block, _ := pem.Decode(encoded)
		if block == nil {
			return nil, errors.New("manifest verification key is not PEM")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse manifest verification key: %w", err)
		}
		publicKey, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("manifest verification key is not Ed25519")
		}
		return publicKey, nil
	}
	if raw == "" {
		return nil, errors.New("MANIFEST_ED25519_PUBLIC_KEY_FILE or MANIFEST_ED25519_PUBLIC_KEY_BASE64 is required")
	}
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(raw)
	if err != nil {
		return nil, errors.New("MANIFEST_ED25519_PUBLIC_KEY_BASE64 must be unpadded standard base64")
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("MANIFEST_ED25519_PUBLIC_KEY_BASE64 does not contain an Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

func writeNoReplaceAtomically(outputPath string, content []byte) error {
	directory := filepath.Dir(outputPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".manifest-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Link is an atomic create-without-replace operation as long as the
	// temporary file lives in the same directory. Rename would silently replace
	// a historical manifest on POSIX systems.
	if err := os.Link(temporaryPath, outputPath); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	return directoryHandle.Sync()
}
