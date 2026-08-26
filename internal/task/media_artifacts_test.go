package task

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"cryp/internal/crypto"
	"cryp/internal/filemeta"
	"cryp/internal/storage"
)

type artifactThumbRecorder struct {
	deleted []string
	queued  []string
}

func (r *artifactThumbRecorder) Enqueue(_, _ string, _ *crypto.VaultKeys, virtualPath string) {
	r.queued = append(r.queued, virtualPath)
}

func (r *artifactThumbRecorder) EnqueueContext(_ context.Context, _, _ string, _ *crypto.VaultKeys, virtualPath string) error {
	r.queued = append(r.queued, virtualPath)
	return nil
}

func (r *artifactThumbRecorder) DeleteThumbnail(_, virtualPath string) {
	r.deleted = append(r.deleted, virtualPath)
}

func TestRebuildEntryIndexDerivesEncryptedMetadataAndThumbnail(t *testing.T) {
	db, err := storage.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	vaultPath := t.TempDir()
	_, keys, err := crypto.InitVault(vaultPath, []byte("test-password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	defer keys.Zero()
	vault := &crypto.Vault{ID: "vault", Path: vaultPath, Keys: keys}
	sourcePath := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(sourcePath, []byte("media-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := crypto.EncryptSingleFile(vault, keys, sourcePath, "/video.mp4"); err != nil {
		t.Fatalf("EncryptSingleFile: %v", err)
	}

	manager := NewManager(db)
	manager.SetMediaDeriver(func(context.Context, string, string, string, *crypto.VaultKeys) (*filemeta.Record, error) {
		return &filemeta.Record{Media: &filemeta.Media{DurationSeconds: 42, Width: 1920, Height: 1080}}, nil
	})
	thumbs := &artifactThumbRecorder{}
	manager.SetThumbEnqueuer(thumbs)
	taskRecord := &storage.TaskRecord{ID: "rebuild", VaultID: vault.ID}
	if err := manager.rebuildEntryIndex(context.Background(), vault, taskRecord, "/"); err != nil {
		t.Fatalf("rebuildEntryIndex: %v", err)
	}

	pathKey, err := crypto.EncryptIndexPath(keys.MACKey, vault.ID, "/video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := db.GetEntry(vault.ID, pathKey)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	payload, err := db.GetFileMetadata(vault.ID, pathKey)
	if err != nil {
		t.Fatalf("GetFileMetadata: %v", err)
	}
	record, err := filemeta.Open(keys.MasterKey, vault.ID, pathKey, payload)
	if err != nil {
		t.Fatalf("Open metadata: %v", err)
	}
	if record.Duration() != 42 || record.Binding.ContentHash != entry.ContentHash || record.Binding.Size != entry.Size || record.Binding.ModTime != entry.ModTime {
		t.Fatalf("metadata not bound to rebuilt entry: record=%+v entry=%+v", record, entry)
	}
	if len(thumbs.deleted) != 1 || thumbs.deleted[0] != "/video.mp4" || len(thumbs.queued) != 1 || thumbs.queued[0] != "/video.mp4" {
		t.Fatalf("thumbnail lifecycle = deleted %v queued %v", thumbs.deleted, thumbs.queued)
	}
}
