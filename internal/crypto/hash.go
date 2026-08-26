package crypto

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// ProtectContentHash converts a plaintext content SHA-256 hex string into a
// keyed digest before it is stored in SQLite metadata. Equal files still group
// together inside the same vault, but the database no longer exposes a raw
// hash that can be compared against known files without the vault key.
func ProtectContentHash(macKey []byte, vaultID, plaintextHash string) string {
	mac := hmac.New(sha256.New, macKey)
	mac.Write([]byte("cryp:file-content-hash:v1:"))
	mac.Write([]byte(vaultID))
	mac.Write([]byte{0})
	mac.Write([]byte(plaintextHash))
	return hex.EncodeToString(mac.Sum(nil))
}

// HashVirtualFile returns the SHA-256 hash of a decrypted vault file.
func (v *Vault) HashVirtualFile(virtualPath string) (string, error) {
	return v.HashVirtualFileContext(context.Background(), virtualPath, true)
}

// HashVirtualFileContext hashes a decrypted file with cancellation support.
// dropCache controls the page-cache hints: rebuilds can keep pages available
// for their immediate metadata/thumbnail passes on bounded files, while large
// files and one-off scans can opt into aggressive cache dropping.
func (v *Vault) HashVirtualFileContext(ctx context.Context, virtualPath string, dropCache bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	encPath, err := v.ResolveExistingFilePath(virtualPath)
	if err != nil {
		return "", err
	}

	file, err := os.Open(encPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	if dropCache {
		unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_SEQUENTIAL|unix.FADV_NOREUSE)
	}

	header, err := ReadFileHeader(file, v.Keys.MasterKey)
	if err != nil {
		return "", fmt.Errorf("read file header: %w", err)
	}

	reader, err := NewDecryptingReader(file, header.ContentKey, header.Nonce)
	if err != nil {
		return "", fmt.Errorf("create decrypting reader: %w", err)
	}
	defer reader.Release()

	hasher := sha256.New()
	bufp := CopyBufPool.Get().(*[]byte)
	defer CopyBufPool.Put(bufp)

	var droppedUntil int64
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		n, readErr := reader.Read(*bufp)
		if n > 0 {
			if _, err := hasher.Write((*bufp)[:n]); err != nil {
				return "", fmt.Errorf("hash decrypted content: %w", err)
			}

			readEnd, seekErr := file.Seek(0, io.SeekCurrent)
			if dropCache && seekErr == nil && readEnd-droppedUntil >= 8*1024*1024 {
				unix.Fadvise(int(file.Fd()), droppedUntil, readEnd-droppedUntil, unix.FADV_DONTNEED)
				droppedUntil = readEnd
			}
		}

		if readErr == nil {
			continue
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("hash decrypted content: %w", readErr)
		}
	}

	if dropCache {
		DropFileCache(file)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}
