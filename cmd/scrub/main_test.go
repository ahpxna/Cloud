package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
)

func TestSafePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"", "../escape", "a/../../escape", "/absolute"} {
		if _, err := safePath(root, key); err == nil {
			t.Fatalf("safePath(%q) accepted unsafe key", key)
		}
	}
	got, err := safePath(root, "originals/ab/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "originals", "ab", "file.bin")
	if got != want {
		t.Fatalf("safePath=%q want %q", got, want)
	}
}

func TestHashFile(t *testing.T) {
	payload := bytes.Repeat([]byte("integrity"), 1000)
	got, err := hashFile(context.Background(), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	if got != want {
		t.Fatalf("hash mismatch: got %x want %x", got, want)
	}
}
