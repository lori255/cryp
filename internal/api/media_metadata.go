package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/fileindex"
	"cryp/internal/filemeta"
	"cryp/internal/thumbnail"
)

const mediaMetadataProbeTimeout = 5 * time.Second

// deriveMediaMetadata is the single server-side metadata entrypoint used by
// uploads, imports and explicit index rebuilds. It probes the encrypted file
// through the authenticated Range-capable content endpoint; no plaintext file
// is materialized on disk.
func (s *Server) deriveMediaMetadata(ctx context.Context, vaultID, vaultPath, virtualPath string, keys *crypto.VaultKeys) (*filemeta.Record, error) {
	if !filemeta.IsMediaPath(virtualPath) {
		return nil, nil
	}
	if s == nil || keys == nil || s.sessions == nil {
		return nil, errors.New("media metadata service unavailable")
	}
	keysCopy := keys.Clone()
	if keysCopy == nil {
		return nil, errors.New("media metadata keys unavailable")
	}
	sessionID, err := s.sessions.Create(vaultID, vaultPath, keysCopy)
	keysCopy.Zero()
	if err != nil {
		return nil, fmt.Errorf("create metadata session: %w", err)
	}
	defer s.sessions.Delete(sessionID)

	contentURL := s.hlsContentURL(vaultID, virtualPath)
	record, err := filemeta.Probe(ctx, contentURL, fmt.Sprintf("Cookie: session_id=%s\r\n", sessionID), mediaMetadataProbeTimeout)
	if err != nil {
		return nil, err
	}
	if !validMediaDuration(record.Duration()) {
		return nil, errors.New("media duration unavailable")
	}
	return &record, nil
}

// indexCommittedFile publishes one encrypted file and all metadata needed by
// read paths. The entry and encrypted metadata share a database transaction;
// thumbnail generation is queued only after that transaction succeeds.
func (s *Server) indexCommittedFile(ctx context.Context, vaultID, vaultPath, virtualPath string, keys *crypto.VaultKeys, size, modTime int64, protectedHash string) (bool, error) {
	if s == nil || s.db == nil || keys == nil {
		return false, errors.New("file index service unavailable")
	}
	metadataReady := true
	var record *filemeta.Record
	if filemeta.IsMediaPath(virtualPath) {
		var probeErr error
		record, probeErr = s.deriveMediaMetadata(ctx, vaultID, vaultPath, virtualPath, keys)
		if probeErr != nil {
			metadataReady = false
			record = &filemeta.Record{}
			log.Printf("metadata: probe %s: %v", virtualPath, probeErr)
		}
	}
	if _, err := fileindex.StoreFile(s.db, keys, fileindex.FileInput{VaultID: vaultID, VirtualPath: virtualPath, Size: size, ModTime: modTime, ProtectedHash: protectedHash, Media: record}); err != nil {
		return false, err
	}

	if s.thumbs != nil && size > 0 && (thumbnail.IsVideo(virtualPath) || thumbnail.IsHEIF(virtualPath)) {
		s.thumbs.DeleteThumbnail(vaultID, virtualPath)
		s.thumbs.Enqueue(vaultID, vaultPath, keys, virtualPath)
	}
	return metadataReady, nil
}

// storedMediaDuration is intentionally read-only. Missing or stale metadata is
// repaired only by an explicit index rebuild; playback never probes and writes
// compatibility data behind the user's back.
func (s *Server) storedMediaDuration(keys *crypto.VaultKeys, key hlsKey) (float64, error) {
	if s == nil || s.db == nil || keys == nil {
		return 0, errHLSMetadataMissing
	}
	pathKey, err := crypto.EncryptIndexPath(keys.MACKey, key.vaultID, key.virtualPath)
	if err != nil {
		return 0, errHLSMetadataMissing
	}
	entry, err := s.db.GetEntry(key.vaultID, pathKey)
	if err != nil || entry.IsDir {
		return 0, errHLSMetadataMissing
	}
	payload, err := s.db.GetFileMetadata(key.vaultID, pathKey)
	if err != nil {
		return 0, errHLSMetadataMissing
	}
	record, err := filemeta.Open(keys.MasterKey, key.vaultID, pathKey, payload)
	if err != nil || record.Binding.ContentHash != entry.ContentHash ||
		record.Binding.Size != entry.Size || record.Binding.ModTime != entry.ModTime {
		return 0, errHLSMetadataMissing
	}
	duration := record.Duration()
	if !validMediaDuration(duration) {
		return 0, errHLSMetadataUnavailable
	}
	return duration, nil
}
