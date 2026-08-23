package crypto

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// ErrUnsafeSource is returned when the path opened for import is no longer the
// same regular file that was validated by the caller.  Re-checking the opened
// descriptor's identity closes the most useful part of the Lstat/EvalSymlinks
// TOCTOU window without tying the crypto package to a particular path policy.
var ErrUnsafeSource = errors.New("source path changed or is not a regular file")

// EncryptResult describes an encrypted file written into the vault.
type EncryptResult struct {
	PlaintextSize int64
	ContentHash   string
	ModTime       int64
}

// CopyWithCacheDrop copies src to dst through encWriter, periodically
// dropping page cache on both files to limit memory usage during large file encryption.
func CopyWithCacheDrop(encWriter io.Writer, src *os.File, dst *os.File) (int64, error) {
	return CopyWithCacheDropContext(context.Background(), encWriter, src, dst)
}

// CopyWithCacheDropContext is the cancellable form used by background tasks.
// Cancellation is checked between bounded (32 KiB) reads, so a large import
// can be stopped without waiting for the entire file to be encrypted.
func CopyWithCacheDropContext(ctx context.Context, encWriter io.Writer, src *os.File, dst *os.File) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	buf := make([]byte, copyBufSize)
	var total int64
	var sinceLastDrop int64
	var srcDropped int64
	var dstDropped int64

	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
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
	return EncryptSingleFileContext(context.Background(), vault, keys, srcPath, virtualPath)
}

// EncryptSingleFileContext encrypts one file while honoring cancellation.
// The destination remains an atomic replacement: cancellation or any other
// error removes only the private temporary file.
func EncryptSingleFileContext(ctx context.Context, vault *Vault, keys *VaultKeys, srcPath, virtualPath string) (EncryptResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return EncryptResult{}, err
	}
	var result EncryptResult
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return result, err
	}
	defer srcFile.Close()
	pathInfo, err := os.Lstat(srcPath)
	if err != nil {
		return result, fmt.Errorf("stat source after open: %w", err)
	}
	fileInfo, err := srcFile.Stat()
	if err != nil {
		return result, fmt.Errorf("stat opened source: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) {
		return result, fmt.Errorf("%w: %s", ErrUnsafeSource, srcPath)
	}

	result.ModTime = fileInfo.ModTime().Unix()

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
	written, err := CopyWithCacheDropContext(ctx, io.MultiWriter(writer, hasher), srcFile, outFile)
	if err != nil {
		os.Remove(tmpPath)
		return result, err
	}
	if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
		os.Remove(tmpPath)
		return result, err
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
