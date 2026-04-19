package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// HashVirtualFile returns the SHA-256 hash of a decrypted vault file.
func (v *Vault) HashVirtualFile(virtualPath string) (string, error) {
	encPath, err := v.ResolveExistingFilePath(virtualPath)
	if err != nil {
		return "", err
	}

	file, err := os.Open(encPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Rebuild-index is a pure sequential scan, so tell the kernel not to
	// retain these encrypted pages in cache longer than necessary.
	unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_SEQUENTIAL|unix.FADV_NOREUSE)

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
		n, readErr := reader.Read(*bufp)
		if n > 0 {
			if _, err := hasher.Write((*bufp)[:n]); err != nil {
				return "", fmt.Errorf("hash decrypted content: %w", err)
			}

			readEnd, seekErr := file.Seek(0, io.SeekCurrent)
			if seekErr == nil && readEnd-droppedUntil >= 8*1024*1024 {
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

	DropFileCache(file)

	return hex.EncodeToString(hasher.Sum(nil)), nil
}
