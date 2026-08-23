package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	cacheDropInterval = 8 * 1024 * 1024 // 8MB - keep low to limit page cache in containers
	copyBufSize       = 32 * 1024       // 32KB
)

// EncryptResult describes an encrypted file written into the vault.
type EncryptResult struct {
	PlaintextSize int64
	ContentHash   string
	ModTime       int64
}

// CopyWithCacheDrop copies src to dst through encWriter, periodically
// dropping page cache on both files to limit memory usage during large file encryption.
func CopyWithCacheDrop(encWriter io.Writer, src *os.File, dst *os.File) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	var sinceLastDrop int64
	var srcDropped int64
	var dstDropped int64

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := encWriter.Write(buf[:n])
			total += int64(nw)
			sinceLastDrop += int64(nw)
			if writeErr != nil {
				return total, writeErr
			}
			if sinceLastDrop >= cacheDropInterval {
				unix.Fadvise(int(src.Fd()), srcDropped, sinceLastDrop, unix.FADV_DONTNEED)
				srcDropped += sinceLastDrop
				dst.Sync()
				pos, _ := dst.Seek(0, io.SeekCurrent)
				if pos > dstDropped {
					unix.Fadvise(int(dst.Fd()), dstDropped, pos-dstDropped, unix.FADV_DONTNEED)
					dstDropped = pos
				}
				sinceLastDrop = 0
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// StreamCopyWithOutputCacheDrop copies from an io.Reader (e.g. multipart body)
// through encWriter to disk, periodically dropping page cache on the output file.
// Unlike CopyWithCacheDrop, this does NOT fadvise the source since network
// streams have no page cache.
func StreamCopyWithOutputCacheDrop(encWriter io.Writer, src io.Reader, dst *os.File) (int64, error) {
	buf := make([]byte, copyBufSize)
	var total int64
	var sinceLastDrop int64
	var dstDropped int64

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := encWriter.Write(buf[:n])
			total += int64(nw)
			sinceLastDrop += int64(nw)
			if writeErr != nil {
				return total, writeErr
			}
			if sinceLastDrop >= cacheDropInterval {
				dst.Sync()
				pos, _ := dst.Seek(0, io.SeekCurrent)
				if pos > dstDropped {
					unix.Fadvise(int(dst.Fd()), dstDropped, pos-dstDropped, unix.FADV_DONTNEED)
					dstDropped = pos
				}
				sinceLastDrop = 0
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// DropFileCache syncs and drops all page cache for the file.
func DropFileCache(f *os.File) {
	if err := f.Sync(); err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		return
	}
	unix.Fadvise(int(f.Fd()), 0, info.Size(), unix.FADV_DONTNEED)
}

// EncryptSingleFile encrypts a file from srcPath into the vault at virtualPath,
// computing the plaintext SHA-256 hash while streaming.
func EncryptSingleFile(vault *Vault, keys *VaultKeys, srcPath, virtualPath string) (EncryptResult, error) {
	var result EncryptResult
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return result, err
	}
	defer srcFile.Close()

	if info, err := srcFile.Stat(); err == nil {
		result.ModTime = info.ModTime().Unix()
	}

	encPath, err := vault.GetEncryptedFilePath(virtualPath)
	if err != nil {
		return result, err
	}

	if err := os.MkdirAll(filepath.Dir(encPath), 0700); err != nil {
		return result, err
	}

	outFile, err := os.CreateTemp(filepath.Dir(encPath), ".cryp-encrypt-*")
	if err != nil {
		return result, err
	}
	tmpPath := outFile.Name()
	defer func() {
		_ = outFile.Close()
		_ = os.Remove(tmpPath)
	}()

	unix.Fadvise(int(srcFile.Fd()), 0, 0, unix.FADV_SEQUENTIAL|unix.FADV_NOREUSE)

	contentKey := make([]byte, MasterKeySize)
	if _, err := rand.Read(contentKey); err != nil {
		os.Remove(tmpPath)
		return result, err
	}

	header, err := WriteFileHeader(outFile, keys.MasterKey, contentKey)
	if err != nil {
		os.Remove(tmpPath)
		return result, err
	}

	writer, err := NewEncryptingWriter(outFile, header.ContentKey, header.Nonce)
	if err != nil {
		os.Remove(tmpPath)
		return result, err
	}

	hasher := sha256.New()
	written, err := CopyWithCacheDrop(io.MultiWriter(writer, hasher), srcFile, outFile)
	if err != nil {
		os.Remove(tmpPath)
		return result, err
	}
	result.PlaintextSize = written
	result.ContentHash = hex.EncodeToString(hasher.Sum(nil))

	if err := writer.Close(); err != nil {
		os.Remove(tmpPath)
		return result, err
	}

	// Fsync to ensure encrypted data is durable on disk before caller
	// may delete the source file.
	if err := outFile.Sync(); err != nil {
		os.Remove(tmpPath)
		return result, fmt.Errorf("fsync encrypted file: %w", err)
	}

	DropFileCache(srcFile)
	DropFileCache(outFile)
	if err := outFile.Close(); err != nil {
		os.Remove(tmpPath)
		return result, fmt.Errorf("close encrypted file: %w", err)
	}
	if err := os.Rename(tmpPath, encPath); err != nil {
		os.Remove(tmpPath)
		return result, fmt.Errorf("replace encrypted file: %w", err)
	}
	return result, nil
}

// ReleaseMemoryAfterLargeFile is a no-op kept for API compatibility.
// With GOMEMLIMIT set, Go's GC automatically manages memory boundaries.
// Manual runtime.GC() causes unnecessary Stop-The-World pauses.
func ReleaseMemoryAfterLargeFile() {
	// Intentionally empty — rely on Go runtime + GOMEMLIMIT
}

// DropPathCache opens a file by path and drops its page cache.
// Useful after the original fd is closed but cache persists.
func DropPathCache(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}
	unix.Fadvise(int(f.Fd()), 0, info.Size(), unix.FADV_DONTNEED)
	f.Close()
}
