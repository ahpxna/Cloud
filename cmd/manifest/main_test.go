package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
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
