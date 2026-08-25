package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"family-photo-cloud/internal/integrity"
)

func TestPrivateKeyLoadsFromPKCS8PEM(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.key")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANIFEST_ED25519_PRIVATE_KEY_FILE", path)
	t.Setenv("MANIFEST_ED25519_PRIVATE_KEY_BASE64", "")
	loaded, err := privateKeyFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Public().(ed25519.PublicKey).Equal(publicKey) {
		t.Fatal("loaded key differs from source key")
	}
}

func TestWriteNoReplaceAtomicallyLeavesOnlyFinalFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "manifest.json")
	if err := writeNoReplaceAtomically(path, []byte("signed inventory")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "signed inventory" {
		t.Fatalf("content = %q", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := writeNoReplaceAtomically(path, []byte("new inventory")); err == nil {
		t.Fatal("existing manifest was overwritten")
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "signed inventory" {
		t.Fatalf("existing content changed to %q", content)
	}
}

func TestPublicKeyLoadsAndManifestVerificationChecksDatabaseEvidence(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "manifest.pub")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANIFEST_ED25519_PUBLIC_KEY_FILE", keyPath)
	t.Setenv("MANIFEST_ED25519_PUBLIC_KEY_BASE64", "")

	record := integrity.AssetRecord{
		AssetID:       "asset-1",
		OwnerID:       "owner-1",
		StorageKey:    "originals/owner-1/asset-1",
		ByteSize:      4,
		ContentSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		VerifiedAt:    time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
	}
	manifest, err := integrity.NewManifest(time.Date(2026, 8, 25, 12, 1, 0, 0, time.UTC), "test-key", []integrity.AssetRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	verified, err := loadAndVerifyManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertManifestExpectations(verified, manifestExpectations{
		Version:      manifest.ManifestVersion,
		AssetCount:   1,
		PayloadHash:  hex.EncodeToString(verified.PayloadHash[:]),
		SigningKeyID: "test-key",
		SignatureHex: hex.EncodeToString(verified.Signature),
	}); err != nil {
		t.Fatal(err)
	}
	if err := assertManifestExpectations(verified, manifestExpectations{PayloadHash: strings.Repeat("00", 32), AssetCount: -1}); err == nil {
		t.Fatal("mismatched database payload hash was accepted")
	}
}
