// Package fileindex owns the durable relationship between an encrypted file,
// its searchable index row and its encrypted derived metadata.
package fileindex

import (
	"errors"

	"cryp/internal/crypto"
	"cryp/internal/filemeta"
	"cryp/internal/storage"
)

type EntryInput struct {
	VaultID         string
	VirtualPath     string
	IsDir           bool
	ChildrenIndexed bool
	Size            int64
	ModTime         int64
	ProtectedHash   string
}

type FileInput struct {
	VaultID       string
	VirtualPath   string
	Size          int64
	ModTime       int64
	ProtectedHash string
	Media         *filemeta.Record
}

func BuildEntryRecord(macKey []byte, input EntryInput) (*storage.EntryRecord, error) {
	normalized := crypto.NormalizeVirtualPath(input.VirtualPath)
	pathKey, err := crypto.EncryptIndexPath(macKey, input.VaultID, normalized)
	if err != nil {
		return nil, err
	}
	parent := crypto.ParentVirtualPath(normalized)
	parentKey := ""
	if parent != "" {
		parentKey, err = crypto.EncryptIndexPath(macKey, input.VaultID, parent)
		if err != nil {
			return nil, err
		}
	}
	nameKey, err := crypto.EncryptEntryNameKey(macKey, input.VaultID, parentKey, crypto.BaseVirtualName(normalized))
	if err != nil {
		return nil, err
	}
	if input.IsDir {
		input.ProtectedHash = ""
		input.Size = 0
	}
	return &storage.EntryRecord{
		VaultID:         input.VaultID,
		PathKey:         pathKey,
		ParentKey:       parentKey,
		NameKey:         nameKey,
		IsDir:           input.IsDir,
		ChildrenIndexed: input.ChildrenIndexed,
		ContentHash:     input.ProtectedHash,
		Size:            input.Size,
		ModTime:         input.ModTime,
	}, nil
}

// StoreFile atomically publishes a file index row with either encrypted media
// metadata or an explicit absence of metadata. A replacement can therefore
// never inherit the previous file's derived data.
func StoreFile(db *storage.DB, keys *crypto.VaultKeys, input FileInput) (*storage.EntryRecord, error) {
	if db == nil || keys == nil {
		return nil, errors.New("file index storage is unavailable")
	}
	entry, err := BuildEntryRecord(keys.MACKey, EntryInput{VaultID: input.VaultID, VirtualPath: input.VirtualPath, Size: input.Size, ModTime: input.ModTime, ProtectedHash: input.ProtectedHash})
	if err != nil {
		return nil, err
	}
	if input.Media == nil {
		if err := db.UpsertEntryWithoutFileMetadata(entry); err != nil {
			return nil, err
		}
		return entry, nil
	}

	record := *input.Media
	record.Binding = filemeta.Binding{ContentHash: input.ProtectedHash, Size: input.Size, ModTime: input.ModTime}
	payload, err := filemeta.Seal(keys.MasterKey, input.VaultID, entry.PathKey, record)
	if err != nil {
		return nil, err
	}
	if err := db.UpsertEntryWithFileMetadata(entry, payload); err != nil {
		return nil, err
	}
	return entry, nil
}
