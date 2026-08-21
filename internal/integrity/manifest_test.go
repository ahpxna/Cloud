package integrity

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestManifestSignatureDetectsAnyInventoryChange(t *testing.T) {
	t.Parallel()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewManifest(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), "offline-ed25519-2026-01", []AssetRecord{
		assetRecord("asset-b", "owner-a", "originals/owner-a/bb/hash-b.jpg"),
		assetRecord("asset-a", "owner-a", "originals/owner-a/aa/hash-a.jpg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Sign(privateKey); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(publicKey); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	manifest.Records[0].StorageKey = "originals/owner-a/tampered.jpg"
	if err := manifest.Verify(publicKey); err == nil {
		t.Fatal("tampered manifest verified")
	}
}

func TestCanonicalPayloadIsIndependentOfInputOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	first, err := NewManifest(now, "key-1", []AssetRecord{
		assetRecord("asset-b", "owner-z", "originals/owner-z/b.jpg"),
		assetRecord("asset-a", "owner-a", "originals/owner-a/a.jpg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewManifest(now, "key-1", []AssetRecord{
		assetRecord("asset-a", "owner-a", "originals/owner-a/a.jpg"),
		assetRecord("asset-b", "owner-z", "originals/owner-z/b.jpg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstPayload, err := first.CanonicalPayload()
	if err != nil {
		t.Fatal(err)
	}
	secondPayload, err := second.CanonicalPayload()
	if err != nil {
		t.Fatal(err)
	}
	if string(firstPayload) != string(secondPayload) {
		t.Fatalf("payload differs for equivalent inventory\nfirst=%q\nsecond=%q", firstPayload, secondPayload)
	}
}

func TestManifestRejectsDuplicateAndMalformedRecords(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	_, err := NewManifest(now, "key", []AssetRecord{
		assetRecord("asset-a", "owner", "originals/a"),
		assetRecord("asset-a", "owner", "originals/b"),
	})
	if err == nil {
		t.Fatal("duplicate asset IDs were accepted")
	}
	manifest, err := NewManifest(now, "key", []AssetRecord{assetRecord("asset-a", "owner", "originals/a")})
	if err != nil {
		t.Fatal(err)
	}
	manifest.Records[0].ContentSHA256 = "not-a-hash"
	if _, err := manifest.CanonicalPayload(); err == nil {
		t.Fatal("malformed hash was accepted")
	}
}

func assetRecord(id, owner, key string) AssetRecord {
	return AssetRecord{
		AssetID:       id,
		OwnerID:       owner,
		StorageKey:    key,
		ByteSize:      42,
		ContentSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		VerifiedAt:    time.Date(2026, 8, 19, 10, 11, 12, 123000000, time.UTC),
	}
}
