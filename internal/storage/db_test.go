package storage

import (
	"bytes"
	"database/sql"
	"testing"
)

func TestEncryptedFileMetadataLifecycleFollowsEntry(t *testing.T) {
	db, err := NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	entry := &EntryRecord{
		VaultID:     "vault",
		PathKey:     "path-key",
		ParentKey:   "parent-key",
		NameKey:     "name-key",
		ContentHash: "protected-hash",
		Size:        123,
		ModTime:     456,
	}
	if err := db.UpsertEntry(entry); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	payload := []byte("encrypted-payload")
	if err := db.UpsertFileMetadata(entry.VaultID, entry.PathKey, payload); err != nil {
		t.Fatalf("UpsertFileMetadata: %v", err)
	}
	got, err := db.GetFileMetadata(entry.VaultID, entry.PathKey)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("GetFileMetadata = %q, %v", got, err)
	}

	if err := db.DeleteEntry(entry.VaultID, entry.PathKey); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}
	if _, err := db.GetFileMetadata(entry.VaultID, entry.PathKey); err != sql.ErrNoRows {
		t.Fatalf("metadata after entry deletion error = %v, want sql.ErrNoRows", err)
	}
}

func TestClearEntriesAlsoClearsEncryptedFileMetadata(t *testing.T) {
	db, err := NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	entry := &EntryRecord{
		VaultID:   "vault",
		PathKey:   "path-key",
		ParentKey: "parent-key",
		NameKey:   "name-key",
	}
	if err := db.UpsertEntry(entry); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	if err := db.UpsertFileMetadata(entry.VaultID, entry.PathKey, []byte("encrypted-payload")); err != nil {
		t.Fatalf("UpsertFileMetadata: %v", err)
	}

	if err := db.ClearEntries(entry.VaultID); err != nil {
		t.Fatalf("ClearEntries: %v", err)
	}
	if _, err := db.GetEntry(entry.VaultID, entry.PathKey); err != sql.ErrNoRows {
		t.Fatalf("entry after clear error = %v, want sql.ErrNoRows", err)
	}
	if _, err := db.GetFileMetadata(entry.VaultID, entry.PathKey); err != sql.ErrNoRows {
		t.Fatalf("metadata after clear error = %v, want sql.ErrNoRows", err)
	}
}

func TestUpsertEntryWithFileMetadataIsAtomic(t *testing.T) {
	db, err := NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	entry := &EntryRecord{VaultID: "vault", PathKey: "path-key", ParentKey: "parent-key", NameKey: "name-key"}
	payload := []byte("encrypted-payload")
	if err := db.UpsertEntryWithFileMetadata(entry, payload); err != nil {
		t.Fatalf("UpsertEntryWithFileMetadata: %v", err)
	}
	if _, err := db.GetEntry(entry.VaultID, entry.PathKey); err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	stored, err := db.GetFileMetadata(entry.VaultID, entry.PathKey)
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("GetFileMetadata = %q, %v", stored, err)
	}
}
