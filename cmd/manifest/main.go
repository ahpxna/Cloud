// Command manifest signs a point-in-time inventory of verified originals.
// It is intentionally a separately invoked administrative tool: the signing
// key must not be present in the long-running upload gateway container.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"family-photo-cloud/internal/integrity"

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
	outputPath := flags.String("output", "", "filesystem path for signed JSON output")
	objectKey := flags.String("object-key", "", "stable storage key recorded in PostgreSQL")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *outputPath == "" || *objectKey == "" {
		return errors.New("usage: manifest -output /secure/path/manifest.json -object-key manifests/manifest.json")
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
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("configure PostgreSQL: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	var alreadyRecorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM signed_manifests WHERE object_key = $1)`, *objectKey).Scan(&alreadyRecorded); err != nil {
		return fmt.Errorf("check manifest object key: %w", err)
	}
	if alreadyRecorded {
		return fmt.Errorf("manifest object key %q is already recorded; manifests are immutable", *objectKey)
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
	if err := writeNoReplaceAtomically(*outputPath, encoded); err != nil {
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
		manifest.ManifestVersion, *objectKey, len(manifest.Records), payloadHash[:],
		manifest.SigningKeyID, signature,
	)
	if err != nil {
		return fmt.Errorf("record signed manifest after writing %s: %w", *outputPath, err)
	}
	fmt.Printf("wrote signed manifest %s (%d verified assets, key %s)\n", *outputPath, len(records), keyID)
	return nil
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
