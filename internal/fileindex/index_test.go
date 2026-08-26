package fileindex

import (
	"database/sql"
	"testing"

	"cryp/internal/crypto"
	"cryp/internal/filemeta"
	"cryp/internal/storage"
)

func TestStoreFilePublishesBoundMetadataAtomically(t *testing.T) {
	db, err := storage.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	_, keys, err := crypto.InitVault(t.TempDir(), []byte("password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	defer keys.Zero()

	record := &filemeta.Record{Media: &filemeta.Media{DurationSeconds: 12.5, Width: 1280, Height: 720}}
	entry, err := StoreFile(db, keys, FileInput{VaultID: "vault", VirtualPath: "/clip.mp4", Size: 99, ModTime: 123, ProtectedHash: "hash-1", Media: record})
	if err != nil {
		t.Fatalf("StoreFile: %v", err)
	}
	payload, err := db.GetFileMetadata("vault", entry.PathKey)
	if err != nil {
		t.Fatalf("GetFileMetadata: %v", err)
	}
	decoded, err := filemeta.Open(keys.MasterKey, "vault", entry.PathKey, payload)
	if err != nil {
		t.Fatalf("Open metadata: %v", err)
	}
	if decoded.Duration() != 12.5 || decoded.Binding.ContentHash != "hash-1" || decoded.Binding.Size != 99 || decoded.Binding.ModTime != 123 {
		t.Fatalf("unexpected metadata: %+v", decoded)
	}
}

func TestStoreFileNonMediaReplacementRemovesOldMetadata(t *testing.T) {
	db, err := storage.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	_, keys, err := crypto.InitVault(t.TempDir(), []byte("password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	defer keys.Zero()

	entry, err := StoreFile(db, keys, FileInput{VaultID: "vault", VirtualPath: "/item.mp4", Size: 10, ModTime: 1, ProtectedHash: "old", Media: &filemeta.Record{Media: &filemeta.Media{DurationSeconds: 1}}})
	if err != nil {
		t.Fatalf("initial StoreFile: %v", err)
	}
	if _, err := StoreFile(db, keys, FileInput{VaultID: "vault", VirtualPath: "/item.mp4", Size: 20, ModTime: 2, ProtectedHash: "new"}); err != nil {
		t.Fatalf("replacement StoreFile: %v", err)
	}
	if _, err := db.GetFileMetadata("vault", entry.PathKey); err != sql.ErrNoRows {
		t.Fatalf("old metadata error = %v, want sql.ErrNoRows", err)
	}
}
